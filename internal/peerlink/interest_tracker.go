package peerlink

import (
	"log/slog"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"monstermq.io/edge/internal/mqtt"
	"monstermq.io/edge/internal/peerlink/wire"
)

// Interest tracker limits (plan-peerlink-interest-routing 4.2).
const (
	// trackerFlushPending flushes at once when this many filters wait for a delta.
	trackerFlushPending = 1024
	// trackerFrameEntries caps the entries of one interest frame.
	trackerFrameEntries = 1024
	// trackerCursorFrames caps the delta frames queued for one puller; beyond it the puller gets a
	// fresh snapshot instead.
	trackerCursorFrames = 1024
)

// holdClass is the class one local holder gives a filter.
type holdClass struct {
	per bool
	exp uint32 // seconds, wire.InterestExpiryNever for no expiry; PER only
}

// trackEntry counts the local holders of one filter by class (4.2). perExp counts the persistent
// holders by expiry, so the maximum survives the holder of the maximum leaving.
type trackEntry struct {
	vol, per uint32
	perExp   map[uint32]uint32
}

type clientSubKey struct{ filter, group string }

type inlineSubKey struct {
	id     int
	filter string
}

// interestTracker is the consumer side of interest routing (4): one per node, shared by all pullers.
// It refcounts the local subscriptions per filter and announces only changes of the aggregated class.
type interestTracker struct {
	maxFilterBytes int
	sharedDeliver  bool
	flushEvery     time.Duration
	classOf        func(clientID string) holdClass
	logger         *slog.Logger

	mu        sync.Mutex
	entries   map[string]*trackEntry
	clients   map[string]map[clientSubKey]holdClass
	inline    map[inlineSubKey]struct{}
	provided  map[string]map[string]holdClass // provider source -> filter -> class
	restored  map[string]*restoredSession
	announced map[string]wire.InterestEntry
	pending   map[string]struct{}
	gen       uint32
	timer     *time.Timer
	cursors   map[*interestCursor]struct{}
	closed    bool

	rejected atomic.Uint64
}

func newInterestTracker(maxFilterBytes int, sharedDeliver bool, flushEvery time.Duration,
	classOf func(string) holdClass, logger *slog.Logger) *interestTracker {
	return &interestTracker{
		maxFilterBytes: maxFilterBytes,
		sharedDeliver:  sharedDeliver,
		flushEvery:     flushEvery,
		classOf:        classOf,
		logger:         logger,
		entries:        make(map[string]*trackEntry),
		clients:        make(map[string]map[clientSubKey]holdClass),
		inline:         make(map[inlineSubKey]struct{}),
		provided:       make(map[string]map[string]holdClass),
		restored:       make(map[string]*restoredSession),
		announced:      make(map[string]wire.InterestEntry),
		pending:        make(map[string]struct{}),
		cursors:        make(map[*interestCursor]struct{}),
	}
}

// restoredSession holds the subscriptions of an offline persistent session restored from storage
// that its client has not re-added yet; timer drops them when the session expires offline.
type restoredSession struct {
	subs  map[clientSubKey]struct{}
	timer *time.Timer
}

// sessionHoldClass is the class of a session (4.3): persistent if it survives a disconnect, with
// the session expiry capped by the broker maximum.
func sessionHoldClass(protocol byte, clean bool, sessionExpiry, maxExpiry uint32) holdClass {
	if protocol == 5 {
		if sessionExpiry == 0 {
			return holdClass{}
		}
		return holdClass{per: true, exp: min(sessionExpiry, maxExpiry)}
	}
	if clean {
		return holdClass{}
	}
	return holdClass{per: true, exp: maxExpiry}
}

// accept reports whether filter may be announced (4.4). $-filters are skipped silently; invalid
// filters are counted and logged when warn is set.
func (t *interestTracker) accept(filter string, warn bool) bool {
	if strings.HasPrefix(filter, "$") {
		return false
	}
	if validInterestFilter(filter, t.maxFilterBytes) {
		return true
	}
	if warn {
		t.rejected.Add(1)
		t.logger.Warn("peerlink: subscription filter not announced to sources (invalid for interest routing)",
			"filter", truncateFilter(filter))
	}
	return false
}

func truncateFilter(f string) string {
	if len(f) > 128 {
		return f[:128] + "..."
	}
	return f
}

// SubscriptionAdded implements mqtt.InterestObserver (E8).
func (t *interestTracker) SubscriptionAdded(client, filter, group string, inline bool, inlineID int) {
	if group != "" && !t.sharedDeliver {
		return
	}
	if inline {
		if !t.accept(filter, true) {
			return
		}
		t.mu.Lock()
		k := inlineSubKey{id: inlineID, filter: filter}
		if _, ok := t.inline[k]; !ok {
			t.inline[k] = struct{}{}
			t.addLocked(filter, holdClass{})
		}
		t.mu.Unlock()
		return
	}
	if strings.HasPrefix(client, InjectorPrefix) || !t.accept(filter, true) {
		return
	}
	hc := t.classOf(client)
	t.mu.Lock()
	subs := t.clients[client]
	if subs == nil {
		subs = make(map[clientSubKey]holdClass)
		t.clients[client] = subs
	}
	k := clientSubKey{filter: filter, group: group}
	if old, ok := subs[k]; ok {
		t.removeLocked(filter, old)
	}
	subs[k] = hc
	t.addLocked(filter, hc)
	if r := t.restored[client]; r != nil {
		delete(r.subs, k)
	}
	t.mu.Unlock()
}

// SubscriptionRemoved implements mqtt.InterestObserver (E8).
func (t *interestTracker) SubscriptionRemoved(client, filter, group string, inline bool, inlineID int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if inline {
		k := inlineSubKey{id: inlineID, filter: filter}
		if _, ok := t.inline[k]; ok {
			delete(t.inline, k)
			t.removeLocked(filter, holdClass{})
		}
		return
	}
	subs := t.clients[client]
	k := clientSubKey{filter: filter, group: group}
	hc, ok := subs[k]
	if !ok {
		return
	}
	delete(subs, k)
	if len(subs) == 0 {
		delete(t.clients, client)
	}
	if r := t.restored[client]; r != nil {
		delete(r.subs, k)
	}
	t.removeLocked(filter, hc)
}

// FiltersAdded implements pubsub.Observer: bus subscriptions are volatile interest.
func (t *interestTracker) FiltersAdded(filters []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, f := range filters {
		if t.accept(f, true) {
			t.addLocked(f, holdClass{})
		}
	}
}

// FiltersRemoved implements pubsub.Observer.
func (t *interestTracker) FiltersRemoved(filters []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, f := range filters {
		if t.accept(f, false) {
			t.removeLocked(f, holdClass{})
		}
	}
}

// SetProvided replaces the filters of a configuration provider (archive groups, the redundancy
// component provider): vol are announced volatile, per persistent without expiry.
func (t *interestTracker) SetProvided(source string, vol, per []string) {
	next := make(map[string]holdClass, len(vol)+len(per))
	for _, f := range vol {
		if t.accept(f, true) {
			next[f] = holdClass{}
		}
	}
	for _, f := range per {
		if t.accept(f, true) {
			next[f] = holdClass{per: true, exp: wire.InterestExpiryNever}
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for f, hc := range t.provided[source] {
		t.removeLocked(f, hc)
	}
	for f, hc := range next {
		t.addLocked(f, hc)
	}
	if len(next) == 0 {
		delete(t.provided, source)
	} else {
		t.provided[source] = next
	}
}

// restore counts the subscriptions of an offline persistent session from storage. The broker
// restores a stored session only when its client reconnects, yet the session holds interest while
// offline (4.3). Subscriptions the client does not re-add are dropped when its session is
// established; all are dropped after expireIn unless it is zero.
func (t *interestTracker) restore(client string, filters []string, hc holdClass, expireIn time.Duration) {
	keys := make([]clientSubKey, 0, len(filters))
	for _, f := range filters {
		k := clientSubKey{filter: f}
		if mqtt.IsSharedFilter(f) {
			k.group, k.filter = mqtt.SplitSharedFilter(f)
			if !t.sharedDeliver {
				continue
			}
		}
		if t.accept(k.filter, true) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.restored[client] != nil {
		return
	}
	subs := t.clients[client]
	if subs == nil {
		subs = make(map[clientSubKey]holdClass)
		t.clients[client] = subs
	}
	r := &restoredSession{subs: make(map[clientSubKey]struct{}, len(keys))}
	for _, k := range keys {
		if _, ok := subs[k]; ok {
			continue
		}
		subs[k] = hc
		r.subs[k] = struct{}{}
		t.addLocked(k.filter, hc)
	}
	t.restored[client] = r
	if expireIn > 0 {
		r.timer = time.AfterFunc(expireIn, func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			if t.restored[client] == r {
				t.dropRestoredLocked(client)
			}
		})
	}
}

// dropRestoredLocked removes the restored subscriptions of client that were not re-added.
func (t *interestTracker) dropRestoredLocked(client string) {
	r := t.restored[client]
	if r == nil {
		return
	}
	delete(t.restored, client)
	if r.timer != nil {
		r.timer.Stop()
	}
	subs := t.clients[client]
	for k := range r.subs {
		hc, ok := subs[k]
		if !ok {
			continue
		}
		delete(subs, k)
		t.removeLocked(k.filter, hc)
	}
	if len(subs) == 0 {
		delete(t.clients, client)
	}
}

// reclassify recomputes the class of a client's subscriptions, e.g. after its session was
// established: restored subscriptions are counted before the session is known. Stored
// subscriptions the session did not take over are dropped.
func (t *interestTracker) reclassify(client string) {
	hc := t.classOf(client)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dropRestoredLocked(client)
	for k, old := range t.clients[client] {
		if old == hc {
			continue
		}
		t.removeLocked(k.filter, old)
		t.addLocked(k.filter, hc)
		t.clients[client][k] = hc
	}
}

func (t *interestTracker) addLocked(filter string, hc holdClass) {
	e := t.entries[filter]
	if e == nil {
		e = &trackEntry{}
		t.entries[filter] = e
	}
	if hc.per {
		e.per++
		if e.perExp == nil {
			e.perExp = make(map[uint32]uint32)
		}
		e.perExp[hc.exp]++
	} else {
		e.vol++
	}
	t.markLocked(filter)
}

func (t *interestTracker) removeLocked(filter string, hc holdClass) {
	e := t.entries[filter]
	if e == nil {
		return
	}
	if hc.per {
		if e.per == 0 {
			return
		}
		e.per--
		if n := e.perExp[hc.exp]; n <= 1 {
			delete(e.perExp, hc.exp)
		} else {
			e.perExp[hc.exp] = n - 1
		}
	} else {
		if e.vol == 0 {
			return
		}
		e.vol--
	}
	if e.vol == 0 && e.per == 0 {
		delete(t.entries, filter)
	}
	t.markLocked(filter)
}

func (t *interestTracker) markLocked(filter string) {
	if t.closed {
		return
	}
	t.pending[filter] = struct{}{}
	if len(t.pending) >= trackerFlushPending {
		t.flushLocked()
		return
	}
	if t.timer == nil {
		t.timer = time.AfterFunc(t.flushEvery, t.flushTimer)
	}
}

func (t *interestTracker) flushTimer() {
	t.mu.Lock()
	t.timer = nil
	t.flushLocked()
	t.mu.Unlock()
}

// classLocked is the announced class of a filter (4.2).
func (t *interestTracker) classLocked(filter string) wire.InterestEntry {
	e := t.entries[filter]
	switch {
	case e == nil:
		return wire.InterestEntry{Class: wire.InterestNone, Filter: filter}
	case e.per == 0:
		return wire.InterestEntry{Class: wire.InterestVol, Filter: filter}
	}
	var maxExp uint32
	for exp := range e.perExp {
		maxExp = max(maxExp, exp)
	}
	return wire.InterestEntry{Class: wire.InterestPer, ExpirySec: maxExp, Filter: filter}
}

// interestChanged reports whether next must be announced over old: a class change, or a PER
// expiry that crosses "never" or moves by more than 10 % (4.2).
func interestChanged(old, next wire.InterestEntry) bool {
	if old.Class != next.Class {
		return true
	}
	if old.Class != wire.InterestPer || old.ExpirySec == next.ExpirySec {
		return false
	}
	if old.ExpirySec == wire.InterestExpiryNever || next.ExpirySec == wire.InterestExpiryNever || old.ExpirySec == 0 {
		return true
	}
	d := uint64(max(old.ExpirySec, next.ExpirySec) - min(old.ExpirySec, next.ExpirySec))
	return d*10 > uint64(old.ExpirySec)
}

// flushLocked turns the pending filters into INTEREST_DELTA frames for every cursor.
func (t *interestTracker) flushLocked() {
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	if len(t.pending) == 0 {
		return
	}
	var changes []wire.InterestEntry
	for f := range t.pending {
		next := t.classLocked(f)
		old, ok := t.announced[f]
		if !ok {
			old = wire.InterestEntry{Class: wire.InterestNone, Filter: f}
		}
		if !interestChanged(old, next) {
			continue
		}
		if next.Class == wire.InterestNone {
			delete(t.announced, f)
		} else {
			t.announced[f] = next
		}
		changes = append(changes, next)
	}
	clear(t.pending)
	if len(changes) == 0 {
		return
	}
	var frames []wire.Frame
	wrapped := false
	splitInterest(changes, wire.InterestDeltaHeaderLen, func(es []wire.InterestEntry) {
		if t.gen == math.MaxUint32 {
			wrapped = true
		}
		t.gen++
		frames = append(frames, &wire.InterestDelta{Generation: t.gen, Entries: es})
	})
	if wrapped {
		// Generations must not wrap; a FIRST snapshot resets every source to generation 0.
		t.gen = 0
		for c := range t.cursors {
			c.queue = nil
			c.resnap = true
			c.signal()
		}
		return
	}
	for c := range t.cursors {
		if c.resnap {
			continue
		}
		if len(c.queue)+len(frames) > trackerCursorFrames {
			c.queue = nil
			c.resnap = true
		} else {
			c.queue = append(c.queue, frames...)
		}
		c.signal()
	}
}

// splitInterest cuts es into chunks that fit one frame of at most MaxConsumerFrame.
func splitInterest(es []wire.InterestEntry, headerLen int, emit func([]wire.InterestEntry)) {
	budget := wire.MaxConsumerFrame - 1 - headerLen
	start, size := 0, 0
	for i := range es {
		n := es[i].Len()
		if i > start && (i-start >= trackerFrameEntries || size+n > budget) {
			emit(es[start:i:i])
			start, size = i, 0
		}
		size += n
	}
	if start < len(es) {
		emit(es[start:])
	}
}

// snapshotLocked returns the INTEREST_SNAPSHOT frames of the announced set at the current
// generation (5.3). An empty set is one FIRST|LAST frame without entries.
func (t *interestTracker) snapshotLocked() []wire.Frame {
	t.flushLocked()
	es := make([]wire.InterestEntry, 0, len(t.announced))
	for _, e := range t.announced {
		es = append(es, e)
	}
	var frames []*wire.InterestSnapshot
	splitInterest(es, wire.InterestSnapshotHeaderLen, func(part []wire.InterestEntry) {
		frames = append(frames, &wire.InterestSnapshot{Generation: t.gen, Entries: part})
	})
	if len(frames) == 0 {
		frames = append(frames, &wire.InterestSnapshot{Generation: t.gen})
	}
	frames[0].Flags |= wire.InterestFlagFirst
	frames[len(frames)-1].Flags |= wire.InterestFlagLast
	out := make([]wire.Frame, len(frames))
	for i, f := range frames {
		out[i] = f
	}
	return out
}

// attach registers a cursor for a puller session and returns the snapshot it must send first.
func (t *interestTracker) attach() (*interestCursor, []wire.Frame) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := &interestCursor{t: t, notify: make(chan struct{}, 1)}
	snap := t.snapshotLocked()
	t.cursors[c] = struct{}{}
	return c, snap
}

// close stops the flush timer; later changes are no longer announced.
func (t *interestTracker) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	for _, r := range t.restored {
		if r.timer != nil {
			r.timer.Stop()
		}
	}
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
}

// TrackerStatus is the node-wide consumer interest state in the status (13).
type TrackerStatus struct {
	Filters    int    `json:"filters"`
	Generation uint32 `json:"generation"`
	Rejected   uint64 `json:"rejected"`
}

func (t *interestTracker) status() TrackerStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return TrackerStatus{Filters: len(t.announced), Generation: t.gen, Rejected: t.rejected.Load()}
}

// interestCursor is a puller session's position in the tracker: the delta frames it has not sent.
type interestCursor struct {
	t      *interestTracker
	notify chan struct{}
	queue  []wire.Frame // guarded by t.mu
	resnap bool         // guarded by t.mu: the queue overflowed, send a fresh snapshot
}

func (c *interestCursor) signal() {
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

// take returns the frames to send now: queued deltas, or a fresh snapshot after an overflow.
func (c *interestCursor) take() []wire.Frame {
	t := c.t
	t.mu.Lock()
	defer t.mu.Unlock()
	if c.resnap {
		c.resnap = false
		snap := t.snapshotLocked()
		c.queue = nil
		return snap
	}
	q := c.queue
	c.queue = nil
	return q
}

// notifyChan is nil for a nil cursor, so a select on it never fires.
func (c *interestCursor) notifyChan() <-chan struct{} {
	if c == nil {
		return nil
	}
	return c.notify
}

func (c *interestCursor) detach() {
	t := c.t
	t.mu.Lock()
	delete(t.cursors, c)
	t.mu.Unlock()
}
