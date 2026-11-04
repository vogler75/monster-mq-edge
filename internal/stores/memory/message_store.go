// Package memory provides an in-memory MessageStore for last-value caching.
// Nothing is persisted — useful for fast volatile current-state lookups.
package memory

import (
	"bytes"
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"monstermq.io/edge/internal/stores"
)

const numShards = 64

type messageShard struct {
	mu   sync.RWMutex
	data map[string]stores.BrokerMessage
}

type MessageStore struct {
	name   string
	shards [numShards]messageShard
}

func NewMessageStore(name string) *MessageStore {
	s := &MessageStore{name: name}
	for i := range s.shards {
		s.shards[i].data = make(map[string]stores.BrokerMessage)
	}
	return s
}

func shardFor(topic string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(topic); i++ {
		h ^= uint32(topic[i])
		h *= 16777619
	}
	return h & (numShards - 1)
}

func (s *MessageStore) Name() string                  { return s.name }
func (s *MessageStore) Type() stores.MessageStoreType { return stores.MessageStoreMemory }
func (s *MessageStore) Close() error                  { return nil }
func (s *MessageStore) EnsureTable(_ context.Context) error { return nil }

func (s *MessageStore) Get(_ context.Context, topic string) (*stores.BrokerMessage, error) {
	sh := &s.shards[shardFor(topic)]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	if m, ok := sh.data[topic]; ok {
		copy := m
		return &copy, nil
	}
	return nil, nil
}

// PutOne inserts or updates a single message. The store keeps the message long
// after the publish, while the caller's topic, payload and MQTT 5 properties may
// point into a connection read buffer shared with other packets. They are
// detached here so one stored value doesn't keep that buffer alive.
func (s *MessageStore) PutOne(m stores.BrokerMessage) {
	sh := &s.shards[shardFor(m.TopicName)]
	m.Payload = bytes.Clone(m.Payload)
	m.ContentType = strings.Clone(m.ContentType)
	m.ResponseTopic = strings.Clone(m.ResponseTopic)
	if len(m.UserProperties) > 0 {
		props := make([]stores.UserProperty, len(m.UserProperties))
		for i, p := range m.UserProperties {
			props[i] = stores.UserProperty{Key: strings.Clone(p.Key), Value: strings.Clone(p.Value)}
		}
		m.UserProperties = props
	}
	sh.mu.Lock()
	// Go replaces a string map key on every assignment, so the key must be
	// detached as well: reuse the stored key, or clone a new one.
	if existing, ok := sh.data[m.TopicName]; ok {
		m.TopicName = existing.TopicName
	} else {
		m.TopicName = strings.Clone(m.TopicName)
	}
	sh.data[m.TopicName] = m
	sh.mu.Unlock()
}

// DelOne deletes a single topic without slice or context allocations.
func (s *MessageStore) DelOne(topic string) {
	sh := &s.shards[shardFor(topic)]
	sh.mu.Lock()
	delete(sh.data, topic)
	sh.mu.Unlock()
}

func (s *MessageStore) AddAll(_ context.Context, msgs []stores.BrokerMessage) error {
	for _, m := range msgs {
		s.PutOne(m)
	}
	return nil
}

func (s *MessageStore) DelAll(_ context.Context, topics []string) error {
	for _, t := range topics {
		s.DelOne(t)
	}
	return nil
}

func (s *MessageStore) FindMatchingMessages(_ context.Context, pattern string, yield func(stores.BrokerMessage) bool) error {
	var keys []string
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.RLock()
		for k := range sh.data {
			if matchTopic(pattern, k) {
				keys = append(keys, k)
			}
		}
		sh.mu.RUnlock()
	}
	sort.Strings(keys)
	for _, k := range keys {
		sh := &s.shards[shardFor(k)]
		sh.mu.RLock()
		m, ok := sh.data[k]
		sh.mu.RUnlock()
		if !ok {
			continue
		}
		if m.MessageExpiryInterval != nil && *m.MessageExpiryInterval > 0 && !m.Time.IsZero() {
			if (time.Now().UnixMilli()-m.Time.UnixMilli())/1000 >= int64(*m.MessageExpiryInterval) {
				continue
			}
		}
		copy := m
		if !yield(copy) {
			return nil
		}
	}
	return nil
}

func (s *MessageStore) FindMatchingTopics(_ context.Context, pattern string, yield func(string) bool) error {
	var keys []string
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.RLock()
		for k := range sh.data {
			if matchTopic(pattern, k) {
				keys = append(keys, k)
			}
		}
		sh.mu.RUnlock()
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !yield(k) {
			return nil
		}
	}
	return nil
}

func (s *MessageStore) PurgeOlderThan(_ context.Context, t time.Time) (stores.PurgeResult, error) {
	var n int64
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		for k, m := range sh.data {
			if m.Time.Before(t) {
				delete(sh.data, k)
				n++
			}
		}
		sh.mu.Unlock()
	}
	return stores.PurgeResult{DeletedRows: n}, nil
}

func (s *MessageStore) PurgeExpired(_ context.Context) (stores.PurgeResult, error) {
	now := time.Now().UnixMilli()
	var n int64
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		for k, m := range sh.data {
			if m.MessageExpiryInterval != nil && *m.MessageExpiryInterval > 0 && !m.Time.IsZero() {
				if (now-m.Time.UnixMilli())/1000 >= int64(*m.MessageExpiryInterval) {
					delete(sh.data, k)
					n++
				}
			}
		}
		sh.mu.Unlock()
	}
	return stores.PurgeResult{DeletedRows: n}, nil
}

func matchTopic(pattern, topic string) bool {
	// MQTT 4.7.2: a filter starting with a wildcard must not match $-topics ($SYS/...).
	if strings.HasPrefix(topic, "$") && (strings.HasPrefix(pattern, "#") || strings.HasPrefix(pattern, "+")) {
		return false
	}
	if pattern == "#" {
		return true
	}
	for {
		var pSeg string
		pSlash := strings.IndexByte(pattern, '/')
		if pSlash >= 0 {
			pSeg = pattern[:pSlash]
		} else {
			pSeg = pattern
		}
		if pSeg == "#" {
			return true
		}

		var tSeg string
		tSlash := strings.IndexByte(topic, '/')
		if tSlash >= 0 {
			tSeg = topic[:tSlash]
		} else {
			tSeg = topic
		}

		if pSeg != "+" && pSeg != tSeg {
			return false
		}
		if pSlash < 0 || tSlash < 0 {
			return pSlash < 0 && tSlash < 0
		}
		pattern = pattern[pSlash+1:]
		topic = topic[tSlash+1:]
	}
}

var _ stores.MessageStore = (*MessageStore)(nil)
