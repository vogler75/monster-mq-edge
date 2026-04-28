package topic

import (
	"strings"
	"sync"
)

// ExactIndex is a hash-keyed index for wildcard-free topics.
// Lookup and update are O(1). Mirror of TopicIndexExact in the Kotlin broker.
//
// This index rejects topics containing '+' or '#' — route those to a
// WildcardIndex instead.
type ExactIndex[V any] struct {
	mu sync.RWMutex
	m  map[string]map[string]V
}

func NewExactIndex[V any]() *ExactIndex[V] {
	return &ExactIndex[V]{m: map[string]map[string]V{}}
}

// Add stores (key → value) under topic. Overwrites any existing value for
// the same (topic, key).
func (e *ExactIndex[V]) Add(topic, key string, value V) {
	if strings.ContainsAny(topic, "+#") {
		panic("ExactIndex: topic must not contain wildcards: " + topic)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	bucket, ok := e.m[topic]
	if !ok {
		bucket = map[string]V{}
		e.m[topic] = bucket
	}
	bucket[key] = value
}

// Remove deletes (topic, key). Returns true if anything was removed.
func (e *ExactIndex[V]) Remove(topic, key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	bucket, ok := e.m[topic]
	if !ok {
		return false
	}
	if _, had := bucket[key]; !had {
		return false
	}
	delete(bucket, key)
	if len(bucket) == 0 {
		delete(e.m, topic)
	}
	return true
}

// Find returns every (key, value) entry for the exact topic. The returned
// slice is freshly allocated; callers may mutate it.
func (e *ExactIndex[V]) Find(topic string) []Entry[V] {
	e.mu.RLock()
	defer e.mu.RUnlock()
	bucket, ok := e.m[topic]
	if !ok {
		return nil
	}
	out := make([]Entry[V], 0, len(bucket))
	for k, v := range bucket {
		out = append(out, Entry[V]{Key: k, Value: v})
	}
	return out
}

// Has reports whether topic has any subscribers.
func (e *ExactIndex[V]) Has(topic string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.m[topic]) > 0
}

// HasKey reports whether (topic, key) is present.
func (e *ExactIndex[V]) HasKey(topic, key string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	_, ok := e.m[topic][key]
	return ok
}

// AllTopics returns the set of topics with at least one entry.
func (e *ExactIndex[V]) AllTopics() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]string, 0, len(e.m))
	for t := range e.m {
		out = append(out, t)
	}
	return out
}

// RemoveKey removes every (topic, key) entry whose key matches across all
// topics, returning the affected topics.
func (e *ExactIndex[V]) RemoveKey(key string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var affected []string
	for topic, bucket := range e.m {
		if _, ok := bucket[key]; ok {
			delete(bucket, key)
			affected = append(affected, topic)
			if len(bucket) == 0 {
				delete(e.m, topic)
			}
		}
	}
	return affected
}

// TopicCount returns the number of distinct topics with at least one entry.
func (e *ExactIndex[V]) TopicCount() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.m)
}

// EntryCount returns the total number of (topic, key) entries.
func (e *ExactIndex[V]) EntryCount() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	n := 0
	for _, b := range e.m {
		n += len(b)
	}
	return n
}
