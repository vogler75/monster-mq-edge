package peerlink

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
	"unsafe"

	"monstermq.io/edge/internal/peerlink/wire"
)

// interestState is the lifecycle state of a peer's interest on this source
// (plan-peerlink-interest-routing 7.1).
type interestState uint8

const (
	interestUnknown interestState = iota
	interestLive
	interestDisconnected
)

func (s interestState) String() string {
	switch s {
	case interestLive:
		return "LIVE"
	case interestDisconnected:
		return "DISCONNECTED"
	}
	return "UNKNOWN"
}

var (
	errInterestNoSnapshot = errors.New("INTEREST_SNAPSHOT continuation without an open snapshot")
	errInterestGeneration = errors.New("INTEREST_SNAPSHOT chunk with a different generation")
)

type interestEntry struct {
	class     uint8
	expirySec uint32
}

// peerInterest is the interest of one consumer (6.1). All fields are guarded by interestTable.mu.
type peerInterest struct {
	idx    int
	nodeID string
	bit    uint64
	// enabled: interest routing applies to this peer (Interest.Enabled and the peer not OFF).
	enabled bool

	state     interestState
	connected bool
	// dense: the last session did not agree CapInterest, so the peer is served everything.
	dense     bool
	overLimit bool
	inTrie    bool

	instance       uint64
	instanceSeen   bool
	disconnectedAt time.Time

	entries    map[string]interestEntry
	persistent int

	lastGen        uint32
	haveGen        bool
	snapOpen       bool
	snapGen        uint32
	snapMarks      map[string]interestEntry
	snapshotGen    uint32
	lastSnapshotAt time.Time

	sweeping  bool
	sweepFrom uint64
	sweepSeq  uint64
}

// interestTable is the remote interest table of a source: per-peer entries and the union trie the
// capture path matches against (6.1, 6.2). Capture takes the read lock; interest frames, connects,
// disconnects and expiry take the write lock.
type interestTable struct {
	mu         sync.RWMutex
	trie       maskNode
	always     uint64
	full       uint64 // every consumer bit
	peers      []*peerInterest
	unknownAll bool
	maxFilters int
	maxBytes   int
	logger     *slog.Logger

	skipped          stripedCounter
	matched          stripedCounter
	sparseBatches    atomic.Uint64
	volatileDropped  atomic.Uint64
	persistentExpiry atomic.Uint64
	backlogDiscarded atomic.Uint64
	rejected         atomic.Uint64
	overLimit        atomic.Uint64
	deltasReceived   atomic.Uint64
}

// interestPeerConfig is the per-consumer input of newInterestTable, indexed by log consumer index.
type interestPeerConfig struct {
	nodeID  string
	enabled bool
}

func newInterestTable(peers []interestPeerConfig, unknownAll bool, maxFilters, maxBytes int, logger *slog.Logger) *interestTable {
	t := &interestTable{unknownAll: unknownAll, maxFilters: maxFilters, maxBytes: maxBytes, logger: logger}
	for i, pc := range peers {
		p := &peerInterest{idx: i, nodeID: pc.nodeID, bit: 1 << uint(i), enabled: pc.enabled, entries: map[string]interestEntry{}}
		t.peers = append(t.peers, p)
		t.full |= p.bit
		t.applyModeLocked(p)
	}
	return t
}

// match returns the consumers that need a non-retained publish on topic.
func (t *interestTable) match(topic string) uint64 {
	t.mu.RLock()
	m := t.always
	if m != t.full && (t.trie.children != nil || t.trie.plus != nil || t.trie.hashMask != 0) {
		m |= t.trie.match(topic, t.full&^m)
	}
	t.mu.RUnlock()
	return m
}

func (t *interestTable) peer(idx int) *peerInterest {
	if idx < 0 || idx >= len(t.peers) {
		return nil
	}
	return t.peers[idx]
}

// filteredLocked reports whether p is served by its entries; otherwise its bit is fixed by alwaysLocked.
func (t *interestTable) filteredLocked(p *peerInterest) bool {
	return p.enabled && !p.dense && !p.overLimit && p.state != interestUnknown
}

func (t *interestTable) alwaysLocked(p *peerInterest) bool {
	if !p.enabled || p.dense || p.overLimit {
		return true
	}
	return p.state == interestUnknown && t.unknownAll
}

// applyModeLocked brings the trie and the always mask in line with p's mode.
func (t *interestTable) applyModeLocked(p *peerInterest) {
	want := t.filteredLocked(p)
	if want != p.inTrie {
		for f := range p.entries {
			t.trie.set(f, p.bit, want)
		}
		p.inTrie = want
	}
	if t.alwaysLocked(p) {
		t.always |= p.bit
	} else {
		t.always &^= p.bit
	}
}

func (t *interestTable) putLocked(p *peerInterest, f string, e interestEntry) {
	old, had := p.entries[f]
	if had && old.class == wire.InterestPer {
		p.persistent--
	}
	p.entries[f] = e
	if e.class == wire.InterestPer {
		p.persistent++
	}
	if !had && p.inTrie {
		t.trie.set(f, p.bit, true)
	}
}

func (t *interestTable) deleteLocked(p *peerInterest, f string) bool {
	old, had := p.entries[f]
	if !had {
		return false
	}
	if old.class == wire.InterestPer {
		p.persistent--
	}
	delete(p.entries, f)
	if p.inTrie {
		t.trie.set(f, p.bit, false)
	}
	return true
}

func (t *interestTable) clearEntriesLocked(p *peerInterest) {
	if p.inTrie {
		for f := range p.entries {
			t.trie.set(f, p.bit, false)
		}
	}
	clear(p.entries)
	p.persistent = 0
}

// validEntry checks an entry against 5.6; snapshots allow only VOL and PER.
func (t *interestTable) validEntry(e *wire.InterestEntry, delta bool) bool {
	switch e.Class {
	case wire.InterestVol, wire.InterestPer:
	case wire.InterestNone:
		if !delta {
			return false
		}
	default:
		return false
	}
	return validInterestFilter(e.Filter, t.maxBytes)
}

// validInterestFilter is the filter check of 4.4 and 5.6, shared by tracker and table.
func validInterestFilter(f string, maxBytes int) bool {
	n := len(f)
	if n == 0 || n > maxBytes || !utf8.ValidString(f) || strings.IndexByte(f, 0) >= 0 {
		return false
	}
	return validFilter(f) == nil
}

// connect is called once a session of consumer idx passed HELLO_OK. capable is the final CapInterest
// agreement (5.1); instance is the consumer's HELLO.InstanceID (5.2).
func (t *interestTable) connect(idx int, instance uint64, capable bool) {
	t.mu.Lock()
	p := t.peer(idx)
	if p == nil || !p.enabled {
		t.mu.Unlock()
		return
	}
	prev, prevDense := p.state, p.dense
	p.connected = true
	p.snapOpen = false
	p.haveGen = false
	p.sweeping = false
	var dropped int
	restarted := p.instanceSeen && p.instance != instance
	p.instance, p.instanceSeen = instance, true
	if !capable {
		t.clearEntriesLocked(p)
		p.dense = true
		p.state = interestLive
	} else {
		p.dense = false
		if prevDense {
			p.state = interestUnknown
		}
		if restarted {
			for f, e := range p.entries {
				if e.class == wire.InterestVol && t.deleteLocked(p, f) {
					dropped++
				}
			}
		}
	}
	t.applyModeLocked(p)
	n, state := len(p.entries), p.state
	t.mu.Unlock()
	if dropped > 0 {
		t.volatileDropped.Add(uint64(dropped))
	}
	if !capable {
		if !prevDense || prev != interestLive {
			t.logger.Info("peerlink: interest not agreed with the consumer; serving all records", "peer", p.nodeID)
		}
		return
	}
	if restarted {
		t.logger.Info("peerlink: consumer restarted; volatile interest dropped", "peer", p.nodeID,
			"volatileDropped", dropped, "filters", n, "state", state.String())
	}
}

// disconnect is called when the active session of consumer idx ends.
func (t *interestTable) disconnect(idx int, now time.Time) {
	t.mu.Lock()
	p := t.peer(idx)
	if p == nil || !p.enabled || !p.connected {
		t.mu.Unlock()
		return
	}
	p.connected = false
	p.snapOpen = false
	p.snapMarks = nil
	changed := p.state == interestLive
	if changed {
		p.state = interestDisconnected
		p.disconnectedAt = now
	}
	t.applyModeLocked(p)
	n, dense := len(p.entries), p.dense
	t.mu.Unlock()
	if changed {
		t.logger.Warn("peerlink: peer interest DISCONNECTED", "peer", p.nodeID, "filters", n, "dense", dense)
	}
}

// applySnapshot applies one INTEREST_SNAPSHOT frame (5.3). It returns an error for a protocol error.
func (t *interestTable) applySnapshot(idx int, s *wire.InterestSnapshot, now time.Time) error {
	var rejected int
	t.mu.Lock()
	p := t.peer(idx)
	if p == nil || !p.enabled {
		t.mu.Unlock()
		return nil
	}
	switch {
	case s.Flags&wire.InterestFlagFirst != 0:
		p.snapOpen = true
		p.snapGen = s.Generation
		if p.snapMarks == nil {
			p.snapMarks = make(map[string]interestEntry, len(s.Entries))
		} else {
			clear(p.snapMarks)
		}
	case !p.snapOpen:
		t.mu.Unlock()
		return errInterestNoSnapshot
	case s.Generation != p.snapGen:
		t.mu.Unlock()
		return errInterestGeneration
	}
	for i := range s.Entries {
		e := &s.Entries[i]
		if !t.validEntry(e, false) {
			rejected++
			continue
		}
		p.snapMarks[e.Filter] = interestEntry{class: e.Class, expirySec: expiryOf(e)}
	}
	if s.Flags&wire.InterestFlagLast == 0 {
		t.mu.Unlock()
		t.countRejected(p.nodeID, rejected)
		return nil
	}
	// Mark and sweep: the marked set replaces the old one at once.
	prev := p.state
	wasOver := p.overLimit
	t.clearEntriesLocked(p)
	marks := p.snapMarks
	p.snapMarks, p.snapOpen = nil, false
	p.overLimit = len(marks) > t.maxFilters
	if !p.overLimit {
		p.entries, marks = marks, p.entries
		for _, e := range p.entries {
			if e.class == wire.InterestPer {
				p.persistent++
			}
		}
		if p.inTrie {
			for f := range p.entries {
				t.trie.set(f, p.bit, true)
			}
		}
	}
	_ = marks
	p.lastGen, p.haveGen = s.Generation, true
	p.snapshotGen = s.Generation
	p.lastSnapshotAt = now
	p.state = interestLive
	p.sweeping = false
	t.applyModeLocked(p)
	n, per, over := len(p.entries), p.persistent, p.overLimit
	t.mu.Unlock()
	t.countRejected(p.nodeID, rejected)
	t.overLimitChanged(p.nodeID, wasOver, over, len(s.Entries))
	if prev != interestLive {
		t.logger.Info("peerlink: peer interest LIVE", "peer", p.nodeID, "filters", n, "filtersPersistent", per,
			"generation", s.Generation)
	}
	return nil
}

// applyDelta applies one INTEREST_DELTA frame (5.4).
func (t *interestTable) applyDelta(idx int, d *wire.InterestDelta) {
	t.deltasReceived.Add(1)
	var rejected int
	t.mu.Lock()
	p := t.peer(idx)
	if p == nil || !p.enabled || (p.haveGen && d.Generation <= p.lastGen) {
		t.mu.Unlock()
		return
	}
	p.lastGen, p.haveGen = d.Generation, true
	if p.overLimit {
		t.mu.Unlock()
		return
	}
	for i := range d.Entries {
		e := &d.Entries[i]
		if !t.validEntry(e, true) {
			rejected++
			continue
		}
		if e.Class == wire.InterestNone {
			t.deleteLocked(p, e.Filter)
		} else {
			t.putLocked(p, e.Filter, interestEntry{class: e.Class, expirySec: expiryOf(e)})
		}
	}
	over := len(p.entries) > t.maxFilters
	n := len(p.entries)
	if over {
		t.clearEntriesLocked(p)
		p.overLimit = true
		t.applyModeLocked(p)
	}
	t.mu.Unlock()
	t.countRejected(p.nodeID, rejected)
	t.overLimitChanged(p.nodeID, false, over, n)
}

func expiryOf(e *wire.InterestEntry) uint32 {
	if e.Class == wire.InterestPer {
		return e.ExpirySec
	}
	return 0
}

func (t *interestTable) countRejected(peer string, n int) {
	if n == 0 {
		return
	}
	t.rejected.Add(uint64(n))
	t.logger.Warn("peerlink: invalid interest entries ignored", "peer", peer, "rejected", n)
}

func (t *interestTable) overLimitChanged(peer string, was, now bool, filters int) {
	switch {
	case now && !was:
		t.overLimit.Add(1)
		t.logger.Warn("peerlink: peer interest exceeds MaxFiltersPerPeer; serving all records", "peer", peer,
			"filters", filters, "maxFiltersPerPeer", t.maxFilters)
	case was && !now:
		t.logger.Warn("peerlink: peer interest within MaxFiltersPerPeer again; filtering resumed", "peer", peer,
			"filters", filters)
	}
}

// expire drops the persistent entries of disconnected peers whose announced expiry ran out (7.3) and
// starts the backlog sweep for those peers (6.5).
func (t *interestTable) expire(now time.Time) {
	type ev struct {
		peer    string
		expired int
		left    int
	}
	var evs []ev
	t.mu.Lock()
	for _, p := range t.peers {
		if p.state != interestDisconnected || p.connected || p.persistent == 0 {
			continue
		}
		down := now.Sub(p.disconnectedAt)
		n := 0
		for f, e := range p.entries {
			if e.class != wire.InterestPer || e.expirySec == wire.InterestExpiryNever {
				continue
			}
			if down > time.Duration(e.expirySec)*time.Second && t.deleteLocked(p, f) {
				n++
			}
		}
		if n > 0 {
			p.sweeping, p.sweepFrom = true, 0
			p.sweepSeq++
			evs = append(evs, ev{p.nodeID, n, len(p.entries)})
		}
	}
	t.mu.Unlock()
	for _, e := range evs {
		t.persistentExpiry.Add(uint64(e.expired))
		t.logger.Info("peerlink: persistent interest expired", "peer", e.peer, "expired", e.expired, "filters", e.left)
	}
}

// sweep runs one bounded step of the persistent-expiry backlog sweep per peer (6.5): bits of
// non-retained, non-snapshot records the peer no longer matches are cleared.
func (t *interestTable) sweep(lg *Log) {
	if lg == nil || !lg.Masked() {
		return
	}
	type done struct {
		p    *peerInterest
		seq  uint64
		next uint64
		end  bool
	}
	var res []done
	t.mu.RLock()
	for _, p := range t.peers {
		if !p.sweeping {
			continue
		}
		bit := p.bit
		keep := t.always & bit
		next, cleared := lg.ClearConsumerBits(p.idx, p.sweepFrom, func(frame []byte) bool {
			if keep != 0 {
				return false
			}
			flags, topic, ok := wire.PeekTopic(frame)
			if !ok || len(topic) == 0 || flags&(wire.FlagRetain|wire.FlagSnapshot|wire.FlagSkipped) != 0 {
				return false
			}
			return t.trie.match(unsafe.String(&topic[0], len(topic)), bit)&bit == 0
		})
		t.backlogDiscarded.Add(cleared)
		res = append(res, done{p, p.sweepSeq, next, next >= lg.LEO()})
	}
	t.mu.RUnlock()
	if len(res) == 0 {
		return
	}
	t.mu.Lock()
	for _, r := range res {
		if r.p.sweepSeq != r.seq || !r.p.sweeping {
			continue
		}
		if r.end {
			r.p.sweeping = false
		} else {
			r.p.sweepFrom = r.next
		}
	}
	t.mu.Unlock()
}

// InterestStatus is the interest object of a consumer in the status (13).
type InterestStatus struct {
	State              string `json:"state"`
	Mode               string `json:"mode"`
	Filters            int    `json:"filters"`
	FiltersPersistent  int    `json:"filtersPersistent"`
	SnapshotGeneration uint32 `json:"snapshotGeneration"`
	LastSnapshotAt     string `json:"lastSnapshotAt,omitempty"`
	InstanceID         string `json:"instanceId,omitempty"`
}

func (t *interestTable) status(idx int) *InterestStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()
	p := t.peer(idx)
	if p == nil {
		return nil
	}
	s := &InterestStatus{
		State:              p.state.String(),
		Filters:            len(p.entries),
		FiltersPersistent:  p.persistent,
		SnapshotGeneration: p.snapshotGen,
	}
	switch {
	case t.filteredLocked(p):
		s.Mode = "FILTERED"
	case t.alwaysLocked(p):
		s.Mode = "ALL"
	default:
		s.Mode = "NONE"
	}
	if !p.enabled {
		s.State = "OFF"
	}
	if !p.lastSnapshotAt.IsZero() {
		s.LastSnapshotAt = p.lastSnapshotAt.UTC().Format(time.RFC3339Nano)
	}
	if p.instanceSeen {
		s.InstanceID = fmt.Sprintf("%016x", p.instance)
	}
	return s
}
