package winccoanative

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"monstermq.io/edge/internal/oahost"
)

// wildQuery is one dpQueryConnectSingle shared by every subscription with
// the same wildcard filter semantics (WildTarget.Key).
type wildQuery struct {
	key        string
	target     WildTarget
	ref        uint64
	registered bool
	inflight   bool
	answerDone bool // the (possibly chunked) initial answer is complete
	subs       map[subKey]bool
	pending    map[subKey]bool   // subscribed before the initial answer
	cache      map[string][]byte // topic -> last payload (current values)
}

// parseWild parses a wildcard filter and binds it to the local system.
func (s *Service) parseWild(filter string) (WildTarget, error) {
	w, err := ParseWildcard(filter)
	w.Remote = err == nil && w.System != s.localSystem
	return w, err
}

// validateWild decides a native wildcard filter.
func (s *Service) validateWild(f string) (Verdict, string) {
	w, err := s.parseWild(f)
	if err != nil {
		return Invalid, err.Error()
	}
	if w.IsRoot() && !s.opts.AllowRootWildcard {
		return Invalid, "root wildcard subscriptions are disabled (AllowRootWildcardSubscription)"
	}
	if !s.ready.Load() || !s.oaUp.Load() {
		return Unavailable, "WinCC OA not ready"
	}
	if w.Remote {
		// A lookup on the remote system reports an unknown or
		// disconnected system as unavailable before any name check.
		if _, err := s.lookup(w.System + ":" + "MMQ_system_probe."); err != nil {
			return Unavailable, err.Error()
		}
	}
	if w.TypeName != "" {
		if protectedTypes[w.TypeName] {
			return Denied, "datapoint type is not exposed"
		}
		ctx, cancel := context.WithTimeout(context.Background(), s.opts.ResolveTimeout)
		defer cancel()
		if err := s.api.TypeCheck(ctx, w.TypeName, nil, nil); err != nil {
			if errors.Is(err, oahost.ErrNotFound) {
				return Invalid, "datapoint type not found: " + w.TypeName
			}
			return Unavailable, err.Error()
		}
	}
	s.mu.Lock()
	n, exists := len(s.wild), s.wild[w.Key()] != nil
	s.mu.Unlock()
	if !exists && n >= s.opts.MaxWildcardQueries {
		return Unavailable, fmt.Sprintf("wildcard query limit %d reached", s.opts.MaxWildcardQueries)
	}
	return Accept, ""
}

// subscribedWild registers an accepted wildcard filter.
// dormant keeps a remote query unregistered until its system connects
// (wildOnSystem starts it); used for restored filters of an unavailable system.
func (s *Service) subscribedWild(clientID, filter string, existed, restored, dormant bool) {
	w, err := s.parseWild(filter)
	if err != nil {
		return
	}
	sk := subKey{clientID, filter}
	key := w.Key()
	s.mu.Lock()
	if old, ok := s.wildSubs[sk]; ok && old == key {
		q := s.wild[key]
		cache := copyCache(q)
		s.mu.Unlock()
		if !restored {
			s.deliverCache(clientID, filter, cache, true)
		}
		return
	}
	q := s.wild[key]
	if q == nil {
		q = &wildQuery{key: key, target: w, subs: map[subKey]bool{}, pending: map[subKey]bool{}, cache: map[string][]byte{}}
		s.wild[key] = q
	}
	q.subs[sk] = true
	s.wildSubs[sk] = key
	var cache map[string][]byte
	if q.registered {
		cache = copyCache(q)
	}
	if (!q.registered || !q.answerDone) && !restored {
		// Rows of the initial answer still to come go to this subscriber too.
		q.pending[sk] = true
	}
	start := !q.registered && !q.inflight && !(dormant && w.Remote)
	if start {
		q.inflight = true
	}
	s.mu.Unlock()
	if cache != nil && !restored {
		s.deliverCache(clientID, filter, cache, existed)
	}
	if start {
		go s.registerWild(q)
	}
}

func copyCache(q *wildQuery) map[string][]byte {
	if q == nil {
		return nil
	}
	out := make(map[string][]byte, len(q.cache))
	for k, v := range q.cache {
		out[k] = v
	}
	return out
}

func (s *Service) deliverCache(clientID, filter string, cache map[string][]byte, existed bool) {
	for topic, payload := range cache {
		if TopicMatches(filter, topic) {
			_ = s.broker.PublishCurrentValue(clientID, filter, topic, payload, existed)
		}
	}
}

func (s *Service) registerWild(q *wildQuery) {
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.ConnectTimeout)
	defer cancel()
	ref, err := s.api.QueryConnect(ctx, q.target.Query(), true, func(m oahost.Message) { s.onWildRows(q, m) }, s.opts.ConnectTimeout)
	s.mu.Lock()
	q.inflight = false
	alive := s.wild[q.key] == q && len(q.subs) > 0
	if err == nil && alive {
		q.ref = ref
		q.registered = true
	}
	s.mu.Unlock()
	switch {
	case err != nil:
		s.logger.Warn("native wildcard query registration failed", "query", q.target.Query(), "err", err)
	case !alive:
		// Everyone unsubscribed while the registration was in flight.
		go func() { _ = s.api.QueryDisconnect(context.Background(), ref) }()
	}
}

// onWildRows publishes query rows. The initial answer only feeds the cache
// and the subscribers that are waiting for their current values; hotlink
// rows are published to their topic unless an exact subscription on the
// same topic already publishes it or another query delivered the same
// change (same value and source time).
func (s *Service) onWildRows(q *wildQuery, m oahost.Message) {
	flags, _ := m.U32(oahost.TagFlags)
	answer := flags&oahost.FlagAnswer != 0
	more := flags&oahost.FlagMore != 0
	rows, err := oahost.QueryRows(m)
	if err != nil || len(rows) < 1 {
		return
	}
	now := time.Now()
	type out struct {
		topic   string
		payload []byte
	}
	var publish []out
	var initial []delivery
	for _, row := range rows[1:] {
		if len(row) < 2 || row[0].Kind != oahost.KindString {
			continue
		}
		typeName := q.target.TypeName
		if q.target.Types && typeName == "" {
			typeName = s.typeOf(row[0].Str)
			if typeName == "" {
				continue
			}
		}
		t, ok := q.target.RowTarget(row[0].Str, typeName)
		if !ok {
			continue
		}
		topic := t.Topic()
		payload := ValuePayload(row[1], now)
		sig := string(payload[strings.Index(string(payload), `"value":`):])
		if len(row) > 2 {
			sig += "|" + fmt.Sprint(row[2].JSON())
		}
		s.mu.Lock()
		if s.wild[q.key] != q {
			s.mu.Unlock()
			return
		}
		q.cache[topic] = payload
		if answer {
			for sk := range q.pending {
				if TopicMatches(sk.filter, topic) {
					initial = append(initial, delivery{sk.client, sk.filter, topic})
				}
			}
			s.lastSig[topic] = sig
		} else if s.exactTopics[topic] == 0 && s.lastSig[topic] != sig {
			s.lastSig[topic] = sig
			publish = append(publish, out{topic, payload})
		}
		s.mu.Unlock()
		for _, d := range initial {
			_ = s.broker.PublishCurrentValue(d.client, d.filter, d.topic, payload, false)
		}
		initial = initial[:0]
	}
	if answer && !more {
		s.mu.Lock()
		q.pending = map[subKey]bool{}
		q.answerDone = true
		s.mu.Unlock()
	}
	for _, p := range publish {
		if err := s.broker.Publish(p.topic, p.payload, false, 1); err != nil {
			s.logger.Warn("native wildcard publish failed", "topic", p.topic, "err", err)
			continue
		}
		s.published.Add(1)
	}
}

// typeOf returns the DPT of the datapoint of a row name ("Sys:DP.el").
func (s *Service) typeOf(row string) string {
	sys, rest, _ := strings.Cut(row, ":")
	dp, _, _ := strings.Cut(rest, ".")
	res, err := s.lookup(sys + ":" + dp + ".")
	if err != nil || !res.Exists {
		return ""
	}
	return res.TypeName
}

// unsubscribedWild removes one wildcard subscription; the last one
// disconnects its query.
func (s *Service) unsubscribedWild(sk subKey) {
	s.mu.Lock()
	key, ok := s.wildSubs[sk]
	if !ok {
		s.mu.Unlock()
		return
	}
	delete(s.wildSubs, sk)
	q := s.wild[key]
	var ref uint64
	if q != nil {
		delete(q.subs, sk)
		delete(q.pending, sk)
		if len(q.subs) == 0 {
			delete(s.wild, key)
			if q.registered {
				ref = q.ref
			}
		}
	}
	s.mu.Unlock()
	if ref != 0 {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), s.opts.ConnectTimeout)
			defer cancel()
			if err := s.api.QueryDisconnect(ctx, ref); err != nil {
				s.logger.Warn("native wildcard query disconnect failed", "ref", ref, "err", err)
			}
		}()
	}
}

// wildOnSystem disconnects the queries of a lost remote system and
// registers them again when it returns.
func (s *Service) wildOnSystem(system string, available bool) {
	var drop []uint64
	var restart []*wildQuery
	s.mu.Lock()
	for _, q := range s.wild {
		if !q.target.Remote || q.target.System != system {
			continue
		}
		if !available && q.registered {
			drop = append(drop, q.ref)
			q.registered = false
			q.answerDone = false
			q.cache = map[string][]byte{}
			for sk := range q.subs {
				q.pending[sk] = true
			}
		}
		if available && !q.registered && !q.inflight {
			q.inflight = true
			restart = append(restart, q)
		}
	}
	s.mu.Unlock()
	for _, ref := range drop {
		go func(ref uint64) { _ = s.api.QueryDisconnect(context.Background(), ref) }(ref)
	}
	for _, q := range restart {
		go s.registerWild(q)
	}
}

// stopWild disconnects every wildcard query.
func (s *Service) stopWild(ctx context.Context) {
	s.mu.Lock()
	var refs []uint64
	for _, q := range s.wild {
		if q.registered {
			refs = append(refs, q.ref)
		}
	}
	s.wild = map[string]*wildQuery{}
	s.wildSubs = map[subKey]string{}
	s.mu.Unlock()
	for _, ref := range refs {
		if err := s.api.QueryDisconnect(ctx, ref); err != nil {
			s.logger.Warn("native wildcard query disconnect on stop failed", "ref", ref, "err", err)
		}
	}
}

// TopicMatches reports whether an MQTT topic matches a filter.
func TopicMatches(filter, topic string) bool {
	fs := strings.Split(filter, "/")
	ts := strings.Split(topic, "/")
	for i, f := range fs {
		if f == "#" {
			return true
		}
		if i >= len(ts) {
			return false
		}
		if f != "+" && f != ts[i] {
			return false
		}
	}
	return len(fs) == len(ts)
}
