package oastore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"monstermq.io/edge/internal/oahost"
	"monstermq.io/edge/internal/stores"
)

// RetainedType is the datapoint type of retained messages: one datapoint per
// retained topic, named MMQRetained_k<hash of the topic>.
const RetainedType = "MMQRetained"

// Elements of MMQRetained.
var retainedElements = []string{"value", "topic", "user", "qos", "expiry", "updated"}

func retainedElementKinds() []uint32 {
	return []uint32{
		uint32(oahost.KindBytes),  // value: payload (blob)
		uint32(oahost.KindString), // topic: MQTT topic (the DP name is a hash)
		uint32(oahost.KindString), // user: MQTT user that set the value
		uint32(oahost.KindUint),   // qos
		uint32(oahost.KindUint),   // expiry: message expiry interval in s, 0 = none
		uint32(oahost.KindTime),   // updated
	}
}

// valueBatch bounds payloads per dpGet at startup; batches whose answer
// would exceed the ABI message limit are split further.
const valueBatch = 32

// RetainedStore implements stores.MessageStore on MMQRetained datapoints.
// All retained messages are loaded once at startup and kept in memory;
// writes go through to WinCC OA and are confirmed before they count.
type RetainedStore struct {
	api     oahost.API
	timeout time.Duration
	logger  *slog.Logger

	memOnly string // topic root kept in memory only (the native namespace)

	wmu    sync.Mutex // serializes writes (create/set/delete of one topic)
	mu     sync.RWMutex
	loaded bool
	data   map[string]*retainedEntry // topic -> entry
}

type retainedEntry struct {
	dp  string // empty for topics kept in memory only
	msg stores.BrokerMessage
}

// KeepInMemory makes retained topics at and below root memory-only: they
// get no datapoint. Used for the broker's own native namespace (status
// topics), which the broker republishes at startup.
func (s *RetainedStore) KeepInMemory(root string) { s.memOnly = root }

func (s *RetainedStore) inMemoryOnly(topic string) bool {
	return s.memOnly != "" && (topic == s.memOnly || strings.HasPrefix(topic, s.memOnly+"/"))
}

var _ stores.MessageStore = (*RetainedStore)(nil)

func newRetainedStore(api oahost.API, timeout time.Duration, logger *slog.Logger) *RetainedStore {
	return &RetainedStore{api: api, timeout: timeout, logger: logger, data: map[string]*retainedEntry{}}
}

func retainedDP(topic string) string { return DPName(RetainedType, "retained", topic) }

func (s *RetainedStore) Name() string                          { return "retainedmessages" }
func (s *RetainedStore) Type() stores.MessageStoreType         { return stores.MessageStoreWinCCOA }
func (s *RetainedStore) Close() error                          { return nil }
func (s *RetainedStore) EnsureTable(ctx context.Context) error { return s.Load(ctx) }

func (s *RetainedStore) ctx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, s.timeout)
}

// Load reads every MMQRetained datapoint once. Datapoints whose stored topic
// does not match their name, or that were created but never written, are
// skipped and left untouched.
func (s *RetainedStore) Load(parent context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, 10*s.timeout)
	defer cancel()
	names, err := s.api.DpNames(ctx, RetainedType+"_*", RetainedType, s.timeout)
	if err != nil {
		return fmt.Errorf("oastore: enumerate %s: %w", RetainedType, err)
	}
	meta := retainedElements[1:]
	for i := 0; i < len(names); i += enumBatch {
		batch := names[i:min(i+enumBatch, len(names))]
		addrs := make([]string, 0, len(batch)*len(meta))
		for _, n := range batch {
			for _, el := range meta {
				addrs = append(addrs, n+"."+el+":_online.._value")
			}
		}
		vals, err := s.api.DpGet(ctx, addrs, s.timeout)
		if err != nil {
			return fmt.Errorf("oastore: read %s: %w", RetainedType, err)
		}
		var valid []string
		byDP := map[string]*retainedEntry{}
		for j, n := range batch {
			dp := stripSystem(n)
			v := vals[j*len(meta) : (j+1)*len(meta)]
			topic := v[0].Str
			if topic == "" {
				continue // created but never committed
			}
			if retainedDP(topic) != dp {
				s.logger.Warn("oastore: retained datapoint name does not match its topic; ignored", "dp", dp, "topic", topic)
				continue
			}
			if s.inMemoryOnly(topic) {
				s.logger.Warn("oastore: retained datapoint of a memory-only topic ignored", "dp", dp, "topic", topic)
				continue
			}
			msg := stores.BrokerMessage{TopicName: topic, IsRetain: true, Username: v[1].Str, QoS: byte(v[2].Uint), Time: v[4].Time}
			if v[3].Uint > 0 {
				e := uint32(v[3].Uint)
				msg.MessageExpiryInterval = &e
			}
			e := &retainedEntry{dp: dp, msg: msg}
			s.data[topic] = e
			byDP[dp] = e
			valid = append(valid, n)
		}
		if err := s.loadPayloads(ctx, valid, byDP); err != nil {
			return err
		}
	}
	s.loaded = true
	return nil
}

// loadPayloads reads the value element of dps into their entries (caller
// holds s.mu).
func (s *RetainedStore) loadPayloads(ctx context.Context, dps []string, byDP map[string]*retainedEntry) error {
	for i := 0; i < len(dps); i += valueBatch {
		if err := s.readPayloads(ctx, dps[i:min(i+valueBatch, len(dps))], byDP); err != nil {
			return err
		}
	}
	return nil
}

func (s *RetainedStore) readPayloads(ctx context.Context, dps []string, byDP map[string]*retainedEntry) error {
	addrs := make([]string, len(dps))
	for i, n := range dps {
		addrs[i] = n + ".value:_online.._value"
	}
	vals, err := s.api.DpGet(ctx, addrs, s.timeout)
	if errors.Is(err, oahost.ErrTooLarge) && len(dps) > 1 {
		half := len(dps) / 2
		if err := s.readPayloads(ctx, dps[:half], byDP); err != nil {
			return err
		}
		return s.readPayloads(ctx, dps[half:], byDP)
	}
	if err != nil {
		return fmt.Errorf("oastore: read %s values: %w", RetainedType, err)
	}
	for i, n := range dps {
		if e := byDP[stripSystem(n)]; e != nil {
			e.msg.Payload = vals[i].Bytes
		}
	}
	return nil
}

func (s *RetainedStore) ensureLoaded(ctx context.Context) error {
	s.mu.RLock()
	loaded := s.loaded
	s.mu.RUnlock()
	if loaded {
		return nil
	}
	return s.Load(ctx)
}

func (s *RetainedStore) Get(ctx context.Context, topic string) (*stores.BrokerMessage, error) {
	if err := s.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e, ok := s.data[topic]; ok {
		m := e.msg
		return &m, nil
	}
	return nil, nil
}

// AddAll writes each message to its datapoint (created on first use) with a
// confirmed dpSet. An empty payload removes the datapoint.
func (s *RetainedStore) AddAll(ctx context.Context, msgs []stores.BrokerMessage) error {
	if err := s.ensureLoaded(ctx); err != nil {
		return err
	}
	var errs []error
	for _, m := range msgs {
		var err error
		if len(m.Payload) == 0 {
			err = s.del(ctx, m.TopicName)
		} else {
			err = s.put(ctx, m)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *RetainedStore) put(parent context.Context, m stores.BrokerMessage) error {
	if m.Time.IsZero() {
		m.Time = time.Now().UTC()
	}
	if s.inMemoryOnly(m.TopicName) {
		m.IsRetain = true
		m.Payload = append([]byte(nil), m.Payload...)
		s.mu.Lock()
		s.data[m.TopicName] = &retainedEntry{msg: m}
		s.mu.Unlock()
		return nil
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.mu.RLock()
	_, exists := s.data[m.TopicName]
	s.mu.RUnlock()
	dp := retainedDP(m.TopicName)
	ctx, cancel := s.ctx(parent)
	defer cancel()
	if !exists {
		// Lifecycle calls take the DP name without a trailing dot.
		if err := s.api.DpCreate(ctx, dp, RetainedType, s.timeout); err != nil && !isExists(err) {
			return fmt.Errorf("%w: create %s for %s: %v", oahost.ErrPersist, dp, m.TopicName, err)
		}
	}
	var expiry uint64
	if m.MessageExpiryInterval != nil {
		expiry = uint64(*m.MessageExpiryInterval)
	}
	names := make([]string, len(retainedElements))
	for i, el := range retainedElements {
		names[i] = dp + "." + el + ":_original.._value"
	}
	values := []oahost.Value{
		{Kind: oahost.KindBytes, Bytes: m.Payload},
		{Kind: oahost.KindString, Str: m.TopicName},
		{Kind: oahost.KindString, Str: m.Username},
		{Kind: oahost.KindUint, Uint: uint64(m.QoS)},
		{Kind: oahost.KindUint, Uint: expiry},
		{Kind: oahost.KindTime, Time: m.Time},
	}
	if err := s.api.DpSet(ctx, names, values, s.timeout); err != nil {
		return fmt.Errorf("%w: write %s for %s: %v", oahost.ErrPersist, dp, m.TopicName, err)
	}
	m.IsRetain = true
	m.Payload = append([]byte(nil), m.Payload...)
	s.mu.Lock()
	s.data[m.TopicName] = &retainedEntry{dp: dp, msg: m}
	s.mu.Unlock()
	return nil
}

func (s *RetainedStore) del(parent context.Context, topic string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.mu.RLock()
	e, ok := s.data[topic]
	s.mu.RUnlock()
	if !ok {
		return nil
	}
	if e.dp != "" {
		ctx, cancel := s.ctx(parent)
		defer cancel()
		if err := s.api.DpDelete(ctx, e.dp, s.timeout); err != nil && !errors.Is(err, oahost.ErrNotFound) {
			return fmt.Errorf("%w: delete %s for %s: %v", oahost.ErrPersist, e.dp, topic, err)
		}
	}
	s.mu.Lock()
	delete(s.data, topic)
	s.mu.Unlock()
	return nil
}

// DelAll removes the datapoints of the given topics.
func (s *RetainedStore) DelAll(ctx context.Context, topics []string) error {
	if err := s.ensureLoaded(ctx); err != nil {
		return err
	}
	var errs []error
	for _, t := range topics {
		if err := s.del(ctx, t); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *RetainedStore) sortedTopics(pattern string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for t := range s.data {
		if matchTopic(pattern, t) {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

func (s *RetainedStore) FindMatchingMessages(ctx context.Context, pattern string, yield func(stores.BrokerMessage) bool) error {
	if err := s.ensureLoaded(ctx); err != nil {
		return err
	}
	for _, t := range s.sortedTopics(pattern) {
		s.mu.RLock()
		e, ok := s.data[t]
		var m stores.BrokerMessage
		if ok {
			m = e.msg
		}
		s.mu.RUnlock()
		if ok && !yield(m) {
			return nil
		}
	}
	return nil
}

func (s *RetainedStore) FindMatchingTopics(ctx context.Context, pattern string, yield func(string) bool) error {
	if err := s.ensureLoaded(ctx); err != nil {
		return err
	}
	for _, t := range s.sortedTopics(pattern) {
		if !yield(t) {
			return nil
		}
	}
	return nil
}

func (s *RetainedStore) purge(ctx context.Context, drop func(stores.BrokerMessage) bool) (stores.PurgeResult, error) {
	if err := s.ensureLoaded(ctx); err != nil {
		return stores.PurgeResult{}, err
	}
	s.mu.RLock()
	var topics []string
	for t, e := range s.data {
		if drop(e.msg) {
			topics = append(topics, t)
		}
	}
	s.mu.RUnlock()
	err := s.DelAll(ctx, topics)
	return stores.PurgeResult{DeletedRows: int64(len(topics))}, err
}

func (s *RetainedStore) PurgeOlderThan(ctx context.Context, t time.Time) (stores.PurgeResult, error) {
	return s.purge(ctx, func(m stores.BrokerMessage) bool { return m.Time.Before(t) })
}

func (s *RetainedStore) PurgeExpired(ctx context.Context) (stores.PurgeResult, error) {
	now := time.Now()
	return s.purge(ctx, func(m stores.BrokerMessage) bool {
		return m.MessageExpiryInterval != nil && *m.MessageExpiryInterval > 0 && !m.Time.IsZero() &&
			now.Sub(m.Time) >= time.Duration(*m.MessageExpiryInterval)*time.Second
	})
}

// matchTopic matches an MQTT filter against a topic name.
func matchTopic(filter, topic string) bool {
	ff := strings.Split(filter, "/")
	tt := strings.Split(topic, "/")
	if strings.HasPrefix(topic, "$") && (ff[0] == "#" || ff[0] == "+") {
		return false // [MQTT-4.7.2-1]
	}
	for i, f := range ff {
		if f == "#" {
			return true
		}
		if i >= len(tt) {
			return false
		}
		if f != "+" && f != tt[i] {
			return false
		}
	}
	return len(ff) == len(tt)
}
