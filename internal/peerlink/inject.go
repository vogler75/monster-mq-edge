package peerlink

import (
	"context"
	"errors"
	"math"
	"time"

	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/peerlink/wire"
)

// Per-record drop reasons on the receiver (12.2, 20.1).
const (
	dropMalformed = iota
	dropSizeSource
	dropNamespace
	dropFiltered
	dropSize
	dropExpired
	dropStale
	dropWillSuperseded
	numDropReasons
)

var dropNames = [numDropReasons]string{"malformed", "size_source", "namespace", "filtered", "size", "expired", "stale", "will_superseded"}

// retainedDiverged reasons (12.2).
const (
	divSize = iota
	divSizeSource
	divExpired
	numDivReasons
)

var divNames = [numDivReasons]string{"size", "size_source", "expired"}

// MarkReplicasKey is the user property Receive.MarkReplicas adds to injected replicas (14.6).
const MarkReplicasKey = "mmq-peer-src"

// snapshotMode selects how snapshot records are applied (16.5).
type snapshotMode int

const (
	snapNone  snapshotMode = iota
	snapFill               // inject only topics without a retained value
	snapNewer              // also overwrite values older than the source's by more than 1 s
)

// internCache is a per-source two-generation string cache for topics, client ids and usernames
// (12.3): no string handed to the engine aliases a batch buffer, and steady traffic over a fixed
// topic set allocates no strings. A generation rotates at maxEntries entries or maxBytes of string
// data, and strings longer than internMaxLen are not cached, so one source pins at most about
// 2 x maxBytes however many distinct long topics its clients publish.
type internCache struct {
	cur, old   map[string]string
	curBytes   int
	maxEntries int
	maxBytes   int
}

const (
	internGeneration = 64 << 10
	internGenBytes   = 8 << 20
	internMaxLen     = 256
)

func newInternCache() *internCache {
	return &internCache{cur: make(map[string]string), maxEntries: internGeneration, maxBytes: internGenBytes}
}

func (c *internCache) get(b []byte) string {
	if len(b) > internMaxLen {
		return string(b)
	}
	if s, ok := c.cur[string(b)]; ok {
		return s
	}
	if s, ok := c.old[string(b)]; ok {
		c.put(s)
		return s
	}
	s := string(b)
	c.put(s)
	return s
}

func (c *internCache) put(s string) {
	if len(c.cur) >= c.maxEntries || c.curBytes+len(s) > c.maxBytes {
		c.old = c.cur
		c.cur = make(map[string]string, min(c.maxEntries, len(c.old))/4+1)
		c.curBytes = 0
	}
	c.cur[s] = s
	c.curBytes += len(s)
}

func underRootBytes(b []byte, root string) bool {
	if root == "" || len(b) < len(root) || string(b[:len(root)]) != root {
		return false
	}
	return len(b) == len(root) || b[len(root)] == '/'
}

// pacer is the catch-up token bucket of 12.6.
type pacer struct {
	factor   float64
	maxRate  float64
	rate     float64 // smoothed source append rate, records per second
	lastLeo  uint64
	lastMono uint64
	tokens   float64
	last     time.Time
}

const pacerFloor = 1000.0

func (pc *pacer) observe(leo, monoMs uint64) {
	if pc.lastMono != 0 && monoMs > pc.lastMono && leo >= pc.lastLeo {
		dt := float64(monoMs-pc.lastMono) / 1000
		if dt >= 0.05 {
			r := float64(leo-pc.lastLeo) / dt
			if pc.rate == 0 {
				pc.rate = r
			} else {
				pc.rate = pc.rate*0.7 + r*0.3
			}
			pc.lastLeo, pc.lastMono = leo, monoMs
		}
		return
	}
	pc.lastLeo, pc.lastMono = leo, monoMs
}

// limit returns the apply rate cap in records per second for the given lag, 0 = none.
func (pc *pacer) limit(lag uint64, threshold int) float64 {
	var r float64
	if pc.factor > 0 && lag > uint64(threshold) {
		r = max(pc.factor*pc.rate, pacerFloor)
	}
	if pc.maxRate > 0 && (r == 0 || pc.maxRate < r) {
		r = pc.maxRate
	}
	return r
}

// take consumes one token at rate r, sleeping when the bucket is empty. Sleeps are at least 2 ms so
// timer granularity does not throttle high rates; the tokens earned while sleeping are kept. It
// reports whether it slept.
func (pc *pacer) take(ctx context.Context, r float64) bool {
	now := time.Now()
	burst := max(r/10, 1)
	if pc.last.IsZero() {
		pc.tokens = burst
	} else {
		pc.tokens = min(burst, pc.tokens+now.Sub(pc.last).Seconds()*r)
	}
	pc.last = now
	slept := false
	if pc.tokens < 1 {
		wait := max(time.Duration((1-pc.tokens)/r*float64(time.Second)), 2*time.Millisecond)
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
		}
		after := time.Now()
		pc.tokens = min(burst, pc.tokens+after.Sub(now).Seconds()*r)
		pc.last = after
		slept = true
	}
	pc.tokens--
	return slept
}

// batchIn is a received BATCH handed from the reader to the injector.
type batchIn struct {
	b      wire.Batch
	recvAt time.Time
	poison bool
}

// applyCtx carries what the injector needs while applying one batch.
type applyCtx struct {
	ctx      context.Context
	srcRoot  string
	ownRoot  string
	epoch    uint64
	mode     snapshotMode
	present  map[string]int64 // local retained topics preloaded for a DB-mode snapshot, nil = ask the store
	rttHalf  int64
	skewMs   int64
	commitFn func(next uint64) // mid-batch commit after 100 ms of applying (9.8)
}

func (ac *applyCtx) has(p *puller, topic string) bool {
	if ac.present != nil {
		_, ok := ac.present[topic]
		return ok
	}
	return p.m.retained.Has(topic)
}

func (ac *applyCtx) created(p *puller, topic string) (int64, bool) {
	if ac.present != nil {
		c, ok := ac.present[topic]
		return c, ok
	}
	return p.m.retained.Created(topic)
}

// applyBatch applies every record of a batch in offset order (12.2-12.6) and returns the offset
// after the batch. Per-record failures are counted drops; a batch-structural fault drops the rest.
func (p *puller) applyBatch(ac *applyCtx, in *batchIn) uint64 {
	h := &in.b.Header
	snapshot := h.Flags&wire.BatchFlagSnapshot != 0
	next := h.BaseOffset + uint64(h.Count)
	if snapshot {
		next = 0
	}
	if in.poison {
		p.dropped[dropMalformed].Add(uint64(h.Count))
		p.m.logger.Error("peerlink: poison batch skipped after repeated CRC failures", "peer", p.nodeID,
			"baseOffset", h.BaseOffset, "count", h.Count)
		return next
	}
	if h.Count == 0 {
		return next
	}
	// Count is bounded by Batch.Decode (ErrBatchCountRange); size by what the region can really hold.
	fwds := make([]packets.Forward, min(uint64(h.Count), uint64(h.RecordsBytes)/wire.TombstoneLen+1))
	it := in.b.Iter()
	var v wire.RecordView
	lastCommit := time.Now()
	for i := uint32(0); ; i++ {
		ok, err := it.Next(&v)
		if !ok {
			if err != nil {
				rest := it.Remaining()
				p.dropped[dropMalformed].Add(uint64(rest))
				p.m.logger.Error("peerlink: batch-structural fault; rest of batch dropped", "peer", p.nodeID,
					"baseOffset", h.BaseOffset, "count", h.Count, "dropped", rest, "error", err)
			}
			break
		}
		off := h.BaseOffset + uint64(i)
		if err != nil {
			p.dropped[dropMalformed].Add(1)
			if ok, n := p.rate.allow("malformed", 10*time.Second); ok {
				p.m.logger.Warn("peerlink: malformed record dropped", "peer", p.nodeID, "offset", off, "error", err, "suppressed", n)
			}
			continue
		}
		if !snapshot && off < p.appliedNext.Load() {
			p.dupSkipped.Add(1)
			continue
		}
		p.unknownProps.Add(uint64(v.UnknownProps))
		var f *packets.Forward
		if int(i) < len(fwds) {
			f = &fwds[i]
		} else {
			f = new(packets.Forward)
		}
		if !snapshot {
			f.Offset = off
		}
		p.applyRecord(ac, in, &v, f, h)
		if !snapshot && i&63 == 63 && ac.commitFn != nil && time.Since(lastCommit) >= 100*time.Millisecond {
			ac.commitFn(off + 1)
			lastCommit = time.Now()
		}
		if ac.ctx.Err() != nil && !snapshot {
			// Stopped while pacing: the rest is re-fetched after the restart from the commit.
			return off + 1
		}
	}
	return next
}

// applyRecord runs the per-record validation, filters and policies of 12.2 and injects the packet.
func (p *puller) applyRecord(ac *applyCtx, in *batchIn, v *wire.RecordView, fwd *packets.Forward, h *wire.BatchHeader) {
	retain := v.Retain()
	if v.Skipped() {
		p.dropped[dropSizeSource].Add(1)
		if retain {
			p.diverge(divSizeSource, "")
		}
		return
	}
	if v.Topic[0] == '$' || underRootBytes(v.Topic, ac.ownRoot) || underRootBytes(v.Topic, ac.srcRoot) {
		p.dropped[dropNamespace].Add(1)
		return
	}
	topic := p.intern.get(v.Topic)
	if !p.filter.accept(topic) {
		p.dropped[dropFiltered].Add(1)
		return
	}
	if p.m.deps.MaxMessageSize > 0 && len(v.Payload) > p.m.deps.MaxMessageSize {
		p.dropped[dropSize].Add(1)
		if retain {
			p.diverge(divSize, topic)
		}
		return
	}
	snapshot := v.Snapshot() && ac.mode != snapNone
	if snapshot && ac.mode == snapFill && ac.has(p, topic) {
		p.snapSkipped.Add(1)
		return
	}

	now := time.Now()
	ageMs := int64(h.SourceMonoMs-v.CaptureMonoMs) + int64(now.Sub(in.recvAt)/time.Millisecond) + ac.rttHalf
	// The expiry of a retained delete only limits its delivery; the clear itself is unconditional
	// on the source, so an expired delete is applied silently like a stale value.
	expired := v.ExpirySec > 0 && ageMs >= int64(v.ExpirySec)*1000
	if expired && !(retain && len(v.Payload) == 0) {
		p.dropped[dropExpired].Add(1)
		if retain {
			p.diverge(divExpired, topic)
		}
		return
	}
	stale := expired || (!snapshot && p.maxAgeMs > 0 && ageMs > p.maxAgeMs)
	if stale && !retain {
		p.dropped[dropStale].Add(1)
		return
	}
	clientID := p.intern.get(v.ClientID)
	if v.Will() {
		hk := p.m.hook
		if p.m.clients.Connected(clientID) ||
			(hk.sessions != nil && hk.sessions.since(clientID, p.m.monoNs()-ageMs*int64(time.Millisecond))) {
			p.dropped[dropWillSuperseded].Add(1)
			if retain {
				// The source applied the will to its own retained store. Sending this node's current
				// value of the topic back lets the source converge to it (PL-29).
				if pk, ok := p.m.retained.Get(topic); ok && p.m.hook.recapture(pk, now.Unix()) {
					p.willResent.Add(1)
				}
			}
			return
		}
	}
	if snapshot && ac.mode == snapNewer {
		if created, ok := ac.created(p, topic); ok {
			srcSec := (v.PublishWallNs/int64(time.Millisecond) - ac.skewMs) / 1000
			if srcSec <= created+1 {
				p.snapSkipped.Add(1)
				return
			}
			p.snapNewer.Add(1)
		}
	}

	*fwd = packets.Forward{
		SourceNode: p.nodeID,
		ClientID:   clientID,
		TimeNs:     v.PublishWallNs,
		Epoch:      ac.epoch,
		Offset:     fwd.Offset,
		Dup:        v.Dup(),
		Will:       v.Will(),
		Snapshot:   v.Snapshot(),
	}
	if len(v.Username) > 0 {
		fwd.Username = p.intern.get(v.Username)
	}
	q := v.QoS()
	pk := packets.Packet{
		FixedHeader: packets.FixedHeader{Type: packets.Publish, Qos: q, Retain: retain},
		TopicName:   topic,
		Payload:     v.Payload,
		Origin:      clientID,
		Created:     max((now.UnixMilli()-ageMs)/1000, 1),
		Forward:     fwd,
	}
	if q > 0 {
		pk.PacketID = 1
	}
	v.Properties(&pk.Properties)
	if v.Snapshot() {
		snapshotCreated(&pk, v.PublishWallNs/int64(time.Millisecond)-ac.skewMs)
	}
	if p.markReplicas {
		pk.Properties.User = append(pk.Properties.User, packets.UserProperty{Key: MarkReplicasKey, Val: p.nodeID})
	}

	if !snapshot {
		var lag uint64
		if h.Leo > fwd.Offset+1 {
			lag = h.Leo - fwd.Offset - 1
		}
		if r := p.pace.limit(lag, p.fetchMaxRecords); r > 0 && p.pace.take(ac.ctx, r) {
			p.paced.Add(1)
		}
	}

	var err error
	if stale {
		err = p.m.srv.RetainOnly(p.inj, pk)
		if err == nil {
			p.retainOnly.Add(1)
			if hk := p.m.hook; hk.echo != nil {
				hk.echo.record(topic, v.Payload, true, p.m.monoNs())
			}
		}
	} else {
		err = p.m.srv.InjectPacket(p.inj, pk)
		if err == nil {
			p.injected.Add(1)
			if snapshot {
				p.snapFilled.Add(1)
			}
		}
	}
	if err != nil {
		p.rejected.Add(1)
		if ok, n := p.rate.allow("rejected", 10*time.Second); ok {
			p.m.logger.Warn("peerlink: replica rejected by the engine", "peer", p.nodeID, "topic", topic, "error", err, "suppressed", n)
		}
		return
	}
	p.appliedBytes.Add(uint64(len(v.Payload)))
	p.hist.observe(ageMs)
}

// snapshotCreated backdates a snapshot value to its original publish time. The source clamps the
// mono age of a retained value at its own start, so the mono-derived Created of a value older than
// the source process is too new; the skew-corrected wall time is not. The expiry interval grows by
// the same amount, so Created + interval, the absolute expiry, stays as the mono age gave it.
func snapshotCreated(pk *packets.Packet, wallMs int64) {
	c := max(wallMs/1000, 1)
	if wallMs <= 0 || c >= pk.Created {
		return
	}
	if mei := pk.Properties.MessageExpiryInterval; mei > 0 {
		pk.Properties.MessageExpiryInterval = uint32(min(int64(mei)+pk.Created-c, math.MaxUint32))
	}
	pk.Created = c
}

func (p *puller) diverge(reason int, topic string) {
	p.diverged[reason].Add(1)
	if ok, n := p.rate.allow("diverged:"+divNames[reason], 10*time.Second); ok {
		p.m.logger.Warn("peerlink: retained value not applied; retained state diverges from the source",
			"peer", p.nodeID, "reason", divNames[reason], "topic", topic, "suppressed", n)
	}
}

var errStopped = errors.New("peerlink: stopped")

func clampU16(v int) uint16 { return uint16(min(max(v, 0), math.MaxUint16)) }

func clampU32(v int) uint32 {
	if v <= 0 {
		return 0
	}
	if uint64(v) > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(v)
}
