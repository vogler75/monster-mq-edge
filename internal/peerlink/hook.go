package peerlink

import (
	"bytes"
	"hash/fnv"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"monstermq.io/edge/internal/mqtt"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/peerlink/wire"
)

const (
	// ListenerID is the listener id of every injector client. No MQTT listener may use it.
	ListenerID = "peerlink"
	// InjectorPrefix prefixes the client id of the injector client of a source ("peerlink:<nodeId>").
	InjectorPrefix = "peerlink:"
)

// Hook captures local publishes into the log (plan 7) and serves the receiver-side engine events:
// reserved client ids (12.1), session times for will supersession (12.2 step 7) and the shared
// subscription skip for replicas (12.4). It never provides OnPublish.
type Hook struct {
	mqtt.HookBase

	m             *Manager
	srv           *mqtt.Server
	log           *Log // nil when no peer may pull from this node
	active        atomic.Bool
	draining      atomic.Bool
	captureWills  bool
	retainViaHook bool
	maxExpirySec  uint32
	filter        includeExclude
	sharedSkip    bool
	echo          *echoTable
	sessions      *sessionTimes
	// interest is the remote interest table; nil when no consumer uses interest routing.
	interest *interestTable

	filtered       stripedCounter
	echoSuppressed stripedCounter
	skipPeer       stripedCounter
	skipWill       stripedCounter
	sharedSkipped  stripedCounter
	refusedIDs     atomic.Uint64

	usernameStripped stripedCounter
}

func validUsername(b []byte) bool {
	return len(b) <= wire.MaxStringLen && utf8.Valid(b) && bytes.IndexByte(b, 0) < 0
}

func (h *Hook) ID() string { return "peerlink" }

func (h *Hook) Provides(b byte) bool {
	switch b {
	case mqtt.OnConnect, mqtt.OnPublished, mqtt.OnRetainMessage, mqtt.OnWillSent,
		mqtt.OnSessionEstablished, mqtt.OnSelectSubscribers:
		return true
	}
	return false
}

// OnConnect refuses network client ids that collide with the shared inline client or an injector
// client: they would confuse NoLocal and will supersession with replicas.
func (h *Hook) OnConnect(cl *mqtt.Client, pk packets.Packet) error {
	if cl.ID == mqtt.InlineClientId || strings.HasPrefix(cl.ID, InjectorPrefix) {
		h.refusedIDs.Add(1)
		if h.srv != nil {
			_ = h.srv.SendConnack(cl, packets.ErrClientIdentifierNotValid, false, nil)
		}
		return packets.ErrClientIdentifierNotValid
	}
	return nil
}

func (h *Hook) OnSessionEstablished(cl *mqtt.Client, pk packets.Packet) {
	if h.sessions != nil {
		h.sessions.record(cl.ID, h.m.monoNs())
	}
	if h.m.tracker != nil {
		h.m.tracker.reclassify(cl.ID)
	}
}

func (h *Hook) OnSelectSubscribers(subs *mqtt.Subscribers, pk packets.Packet) *mqtt.Subscribers {
	if pk.Forward != nil && h.sharedSkip && (len(subs.Shared) > 0 || len(subs.SharedSelected) > 0) {
		subs.Shared = nil
		subs.SharedSelected = nil
		h.sharedSkipped.Inc()
	}
	return subs
}

func isReplica(cl *mqtt.Client, pk *packets.Packet) bool {
	return cl.Net.Listener == ListenerID || pk.Forward != nil
}

func (h *Hook) OnRetainMessage(cl *mqtt.Client, pk packets.Packet, r int64) {
	if pk.Will || isReplica(cl, &pk) || !h.retainViaHook {
		return
	}
	h.capture(cl, &pk, false)
}

func (h *Hook) OnPublished(cl *mqtt.Client, pk packets.Packet) {
	if isReplica(cl, &pk) {
		h.skipPeer.Inc()
		if h.echo != nil && pk.Forward != nil && !pk.Forward.Will {
			h.echo.record(pk.TopicName, pk.Payload, pk.FixedHeader.Retain, h.m.monoNs())
		}
		return
	}
	if pk.FixedHeader.Retain && h.retainViaHook && !pk.Ignore {
		return
	}
	h.capture(cl, &pk, false)
}

func (h *Hook) OnWillSent(cl *mqtt.Client, pk packets.Packet) {
	if isReplica(cl, &pk) {
		return
	}
	if !h.captureWills || h.draining.Load() {
		h.skipWill.Inc()
		return
	}
	h.capture(cl, &pk, true)
}

// accept is the capture filter chain after the replica and Ignore tests: $ topics, the own WinCC OA
// namespace while native mode is active, then Include/Exclude.
func (h *Hook) accept(t string) bool {
	if t == "" || t[0] == '$' || h.m.inNamespace(t) {
		return false
	}
	return h.filter.accept(t)
}

func (h *Hook) capture(cl *mqtt.Client, pk *packets.Packet, will bool) {
	h.captureAs(cl.ID, cl.Properties.Username, cl.Net.Inline, pk, will)
}

// recapture appends a local retained value again, as published by its original client. It reports
// whether a record was appended.
func (h *Hook) recapture(pk packets.Packet, now int64) bool {
	if h.log == nil || !h.active.Load() || len(pk.Payload) == 0 {
		return false
	}
	if pk.Expiry > 0 {
		if pk.Expiry <= now {
			return false
		}
		pk.Properties.MessageExpiryInterval = uint32(min(pk.Expiry-now, math.MaxUint32))
	}
	pk.FixedHeader = packets.FixedHeader{Type: packets.Publish, Qos: pk.FixedHeader.Qos, Retain: true}
	before := h.log.LEO()
	h.captureAs(pk.Origin, nil, false, &pk, false)
	return h.log.LEO() != before
}

func (h *Hook) captureAs(clientID string, username []byte, inline bool, pk *packets.Packet, will bool) {
	if !h.active.Load() {
		if h.log != nil && h.log.Sealed() && !pk.Ignore && h.accept(pk.TopicName) {
			h.log.CountUncapturedAtShutdown()
		}
		return
	}
	if pk.Ignore {
		return
	}
	if !h.accept(pk.TopicName) {
		h.filtered.Inc()
		return
	}
	var rec wire.Record
	kind := LogKindClient
	if inline {
		rec.Flags |= wire.FlagInline
		kind = LogKindInline
	}
	if will {
		rec.Flags |= wire.FlagWill
		kind = LogKindWill
	}
	rec.SetPacket(pk, h.maxExpirySec)
	rec.ClientID = clientID
	rec.Username = username
	if !validUsername(rec.Username) {
		// The engine does not validate the CONNECT username (e.g. a Latin-1 legacy device); the
		// receiver would drop the whole record as malformed, so forward it without the username.
		rec.Username = nil
		h.usernameStripped.Inc()
	}
	if will {
		// Will expiry is dropped by ParseConnect; a delayed will's pk.Expiry holds its send time.
		rec.ExpirySec = 0
	}
	if inline && !rec.ValidContent() {
		h.log.CountCaptureInvalid()
		return
	}
	if h.echo != nil && !will && h.echo.match(pk.TopicName, pk.Payload, pk.FixedHeader.Retain, h.m.monoNs()) {
		h.echoSuppressed.Inc()
		return
	}
	mask := uint64(0)
	if h.interest != nil {
		if pk.FixedHeader.Retain {
			mask = h.log.AllConsumers()
		} else if mask = h.interest.match(pk.TopicName); mask == 0 {
			h.interest.skipped.Inc()
			return
		} else {
			h.interest.matched.Inc()
		}
	}
	size := wire.RecordSize(&rec)
	if size == 0 {
		h.log.CountCaptureInvalid()
		return
	}
	if !h.log.CheckRecordSize(size) {
		return
	}
	t := time.Now()
	rec.PublishWallNs = t.UnixNano()
	rec.CaptureMonoMs = h.log.MonoMs(t)
	buf := make([]byte, size)
	wire.EncodeRecord(buf, &rec)
	if h.interest != nil {
		h.log.AppendMask(buf, kind, mask)
		return
	}
	h.log.Append(buf, kind)
}

func maxExpiry(srv *mqtt.Server) uint32 {
	if srv == nil || srv.Options == nil || srv.Options.Capabilities == nil {
		return 0
	}
	v := srv.Options.Capabilities.MaximumMessageExpiryInterval
	if v <= 0 {
		return 0
	}
	if v > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(v)
}

const hookShards = 16

// sessionTimes maps a client id to the monotonic time its last session was established on this node
// (plan 12.2 step 7). Keys are 64-bit hashes, so long client ids cost nothing extra; entries older
// than 24 h are pruned and each shard is capped, so reconnects with ever new ids cannot grow it
// without bound. Under that pressure the oldest information goes first, which only means a will
// may not be recognised as superseded.
type sessionTimes struct {
	shards [hookShards]struct {
		mu        sync.Mutex
		m         map[uint64]int64
		lastPrune int64
	}
}

const (
	sessionTimesTTL      = int64(24 * time.Hour)
	sessionTimesShardMax = 16 << 10
)

func newSessionTimes() *sessionTimes {
	s := &sessionTimes{}
	for i := range s.shards {
		s.shards[i].m = make(map[uint64]int64)
	}
	return s
}

func hashID(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

func shardOf(s string) int { return int(hashID(s) % hookShards) }

func (s *sessionTimes) record(id string, now int64) {
	k := hashID(id)
	sh := &s.shards[k%hookShards]
	sh.mu.Lock()
	sh.m[k] = now
	if len(sh.m) > sessionTimesShardMax || (now-sh.lastPrune > int64(time.Minute) && len(sh.m) > 1024) {
		sh.lastPrune = now
		for key, v := range sh.m {
			if now-v > sessionTimesTTL {
				delete(sh.m, key)
			}
		}
		if len(sh.m) > sessionTimesShardMax {
			// Drop the older half; amortized over the next half shard of new ids.
			cut := oldestCut(sh.m, len(sh.m)/2)
			for key, v := range sh.m {
				if v < cut {
					delete(sh.m, key)
				}
			}
		}
	}
	sh.mu.Unlock()
}

// oldestCut returns a time below which about n entries of m lie.
func oldestCut(m map[uint64]int64, n int) int64 {
	times := make([]int64, 0, len(m))
	for _, v := range m {
		times = append(times, v)
	}
	slices.Sort(times)
	return times[min(n, len(times)-1)]
}

// since reports whether id established a session at or after the monotonic time at.
func (s *sessionTimes) since(id string, at int64) bool {
	k := hashID(id)
	sh := &s.shards[k%hookShards]
	sh.mu.Lock()
	v, ok := sh.m[k]
	sh.mu.Unlock()
	return ok && v >= at
}

// echoTable remembers replicas applied on this node, so a publish that echoes one within the window
// is not captured (plan 14.6, Capture.EchoSuppressMs).
type echoTable struct {
	window int64
	shards [hookShards]struct {
		mu        sync.Mutex
		m         map[string]echoEntry
		lastPrune int64
	}
}

type echoEntry struct {
	hash   uint64
	retain bool
	at     int64
}

func newEchoTable(windowMs int) *echoTable {
	e := &echoTable{window: int64(windowMs) * int64(time.Millisecond)}
	for i := range e.shards {
		e.shards[i].m = make(map[string]echoEntry)
	}
	return e
}

func payloadHash(b []byte) uint64 {
	h := fnv.New64a()
	h.Write(b)
	return h.Sum64()
}

func (e *echoTable) record(topic string, payload []byte, retain bool, now int64) {
	sh := &e.shards[shardOf(topic)]
	ent := echoEntry{hash: payloadHash(payload), retain: retain, at: now}
	sh.mu.Lock()
	sh.m[strings.Clone(topic)] = ent
	if now-sh.lastPrune > e.window && len(sh.m) > 256 {
		sh.lastPrune = now
		for k, v := range sh.m {
			if now-v.at > e.window {
				delete(sh.m, k)
			}
		}
	}
	sh.mu.Unlock()
}

func (e *echoTable) match(topic string, payload []byte, retain bool, now int64) bool {
	sh := &e.shards[shardOf(topic)]
	sh.mu.Lock()
	ent, ok := sh.m[topic]
	sh.mu.Unlock()
	if !ok || ent.retain != retain || now-ent.at > e.window {
		return false
	}
	return ent.hash == payloadHash(payload)
}
