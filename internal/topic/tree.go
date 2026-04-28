// Package topic implements MQTT 3.1.1 topic-filter matching with a per-level
// trie plus a hash map for exact (wildcard-free) topics. The design mirrors
// the data/SubscriptionManager + TopicTree pair from the Kotlin monster-mq
// broker so the two implementations stay behaviorally aligned.
package topic

import (
	"strings"
	"sync"
)

// Entry is one (key, value) pair stored at a tree node. Key is typically a
// clientId; Value is whatever payload the index needs (QoS byte for MQTT
// subscriptions, an empty struct for set-style indexes).
type Entry[V any] struct {
	Key   string
	Value V
}

type node[V any] struct {
	children map[string]*node[V]
	dataset  map[string]V
}

func newNode[V any]() *node[V] {
	return &node[V]{children: map[string]*node[V]{}, dataset: map[string]V{}}
}

// Tree is a topic-level trie that stores topic patterns (which may contain
// '+' or '#' wildcards) and supports two complementary lookups:
//   - FindData / IsMatching: match a concrete published topic against the
//     stored patterns and return the data on every match.
//   - FindMatchingTopics: match a wildcard filter against the stored exact
//     topics — used for browsing / dump operations.
//
// The MQTT 3.1.1 rule that wildcards must not match topics whose first level
// starts with '$' (unless the filter itself starts with '$') is enforced by
// FindData and IsMatching.
//
// Concurrency: a single RWMutex guards the entire tree. Reads are concurrent.
type Tree[V any] struct {
	mu   sync.RWMutex
	root *node[V]
}

func NewTree[V any]() *Tree[V] {
	return &Tree[V]{root: newNode[V]()}
}

// Add inserts a topic with no associated data. Useful when the tree is being
// used purely as a set of topic names.
func (t *Tree[V]) Add(topic string) {
	t.add(topic, "", nil)
}

// AddData inserts a (key, value) entry under topic. Repeated inserts with the
// same (topic, key) overwrite the value.
func (t *Tree[V]) AddData(topic, key string, value V) {
	t.add(topic, key, &value)
}

func (t *Tree[V]) add(topic, key string, value *V) {
	levels := splitTopic(topic)
	if len(levels) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.root
	for _, lvl := range levels {
		child, ok := n.children[lvl]
		if !ok {
			child = newNode[V]()
			n.children[lvl] = child
		}
		n = child
	}
	if key != "" && value != nil {
		n.dataset[key] = *value
	}
}

// Del removes a topic with no key (mirror of Add).
func (t *Tree[V]) Del(topic string) {
	t.del(topic, "")
}

// DelData removes the (topic, key) entry. If both children and dataset become
// empty along the path, the empty chain is collapsed.
func (t *Tree[V]) DelData(topic, key string) {
	t.del(topic, key)
}

func (t *Tree[V]) del(topic, key string) {
	levels := splitTopic(topic)
	if len(levels) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var walk func(n *node[V], i int) bool
	walk = func(n *node[V], i int) bool {
		if i == len(levels) {
			if key != "" {
				delete(n.dataset, key)
			}
			return len(n.children) == 0 && len(n.dataset) == 0
		}
		child, ok := n.children[levels[i]]
		if !ok {
			return false
		}
		if walk(child, i+1) {
			delete(n.children, levels[i])
		}
		return len(n.children) == 0 && len(n.dataset) == 0
	}
	walk(t.root, 0)
}

// FindData returns the dataset of every stored pattern that matches the
// concrete published topic. MQTT 3.1.1 $SYS rule applies.
func (t *Tree[V]) FindData(topic string) []Entry[V] {
	levels := splitTopic(topic)
	if len(levels) == 0 {
		return nil
	}
	dollar := strings.HasPrefix(levels[0], "$")
	var out []Entry[V]
	t.mu.RLock()
	defer t.mu.RUnlock()

	// walk(n, current, rest, level, filterDollar) iterates n.children, treating
	// each child key as the next pattern level. level is 1-based so isFirstLevel
	// can distinguish $-prefixed filters at level 1.
	var walk func(n *node[V], current string, rest []string, level int, filterDollar bool)
	walk = func(n *node[V], current string, rest []string, level int, filterDollar bool) {
		isFirst := level == 1
		for childKey, child := range n.children {
			childDollar := filterDollar
			if isFirst {
				childDollar = strings.HasPrefix(childKey, "$")
			}
			switch childKey {
			case "#":
				if isFirst && dollar && !childDollar {
					continue
				}
				appendDataset(&out, child)
			case "+":
				if isFirst && dollar && !childDollar {
					continue
				}
				if len(rest) == 0 {
					appendDataset(&out, child)
				} else {
					walk(child, rest[0], rest[1:], level+1, childDollar)
				}
			case current:
				if len(rest) == 0 {
					appendDataset(&out, child)
					// Look for trailing '#' patterns: e.g. "a/#" matches "a".
					// (Mirror of the Kotlin TopicTree.findDataOfTopicName branch
					// that recurses with current="" and empty rest. Note: that
					// recursion also lets "a/+" match "a" via the '+' arm — a
					// known deviation from MQTT spec carried over for parity.)
					walk(child, "", nil, level+1, childDollar)
				} else {
					walk(child, rest[0], rest[1:], level+1, childDollar)
				}
			}
		}
	}
	walk(t.root, levels[0], levels[1:], 1, false)
	return out
}

func appendDataset[V any](out *[]Entry[V], n *node[V]) {
	for k, v := range n.dataset {
		*out = append(*out, Entry[V]{Key: k, Value: v})
	}
}

// IsMatching is FindData reduced to a bool — returns true as soon as any
// stored pattern matches the topic and has at least one dataset entry.
//
// Note: unlike FindData, IsMatching does NOT recurse for trailing '#' after
// the topic is fully consumed. This matches the Kotlin isTopicNameMatching
// behavior (so "a/#" matches "a" via FindData but not via IsMatching).
func (t *Tree[V]) IsMatching(topic string) bool {
	levels := splitTopic(topic)
	if len(levels) == 0 {
		return false
	}
	dollar := strings.HasPrefix(levels[0], "$")
	t.mu.RLock()
	defer t.mu.RUnlock()
	var walk func(n *node[V], current string, rest []string, level int, filterDollar bool) bool
	walk = func(n *node[V], current string, rest []string, level int, filterDollar bool) bool {
		isFirst := level == 1
		for childKey, child := range n.children {
			childDollar := filterDollar
			if isFirst {
				childDollar = strings.HasPrefix(childKey, "$")
			}
			switch childKey {
			case "#":
				if isFirst && dollar && !childDollar {
					continue
				}
				if len(child.dataset) > 0 {
					return true
				}
			case "+":
				if isFirst && dollar && !childDollar {
					continue
				}
				if len(rest) == 0 {
					if len(child.dataset) > 0 {
						return true
					}
				} else if walk(child, rest[0], rest[1:], level+1, childDollar) {
					return true
				}
			case current:
				if len(rest) == 0 {
					if len(child.dataset) > 0 {
						return true
					}
				} else if walk(child, rest[0], rest[1:], level+1, childDollar) {
					return true
				}
			}
		}
		return false
	}
	return walk(t.root, levels[0], levels[1:], 1, false)
}

// FindMatchingTopics walks the tree under filter (which may contain wildcards)
// and invokes cb with every stored exact topic that matches. cb returning
// false stops iteration.
//
// This is the inverse direction from FindData: filter is a wildcard, the
// stored topics are exact. Used for browsing / dumping the topic space.
func (t *Tree[V]) FindMatchingTopics(filter string, cb func(string) bool) {
	levels := splitTopic(filter)
	if len(levels) == 0 {
		return
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	var find func(n *node[V], current string, rest []string, prefix string) bool
	find = func(n *node[V], current string, rest []string, prefix string) bool {
		if len(n.children) == 0 && len(rest) == 0 {
			if prefix != "" {
				return cb(prefix)
			}
			return true
		}
		for childKey, child := range n.children {
			next := childKey
			if prefix != "" {
				next = prefix + "/" + childKey
			}
			switch {
			case current == "#":
				if !find(child, "#", nil, next) {
					return false
				}
			case current == "+" || current == childKey:
				if len(rest) > 0 {
					if !find(child, rest[0], rest[1:], next) {
						return false
					}
				} else {
					if !cb(next) {
						return false
					}
				}
			}
		}
		return true
	}
	find(t.root, levels[0], levels[1:], "")
}

// Size returns the total number of (key, value) entries stored anywhere in
// the tree.
func (t *Tree[V]) Size() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var count func(n *node[V]) int
	count = func(n *node[V]) int {
		c := len(n.dataset)
		for _, ch := range n.children {
			c += count(ch)
		}
		return c
	}
	return count(t.root)
}

// RemoveKey removes every (topic, key) entry whose key equals the given key,
// across the entire tree, and returns the topics from which entries were
// removed. Useful when a client disconnects.
func (t *Tree[V]) RemoveKey(key string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var affected []string
	var walk func(n *node[V], path []string)
	walk = func(n *node[V], path []string) {
		if _, ok := n.dataset[key]; ok {
			delete(n.dataset, key)
			affected = append(affected, strings.Join(path, "/"))
		}
		for childKey, child := range n.children {
			walk(child, append(path, childKey))
			if len(child.children) == 0 && len(child.dataset) == 0 {
				delete(n.children, childKey)
			}
		}
	}
	walk(t.root, nil)
	return affected
}

func splitTopic(topic string) []string {
	if topic == "" {
		return nil
	}
	return strings.Split(topic, "/")
}

// MatchFilter is the standalone topic-filter matcher (no tree). Implements
// MQTT 3.1.1 wildcard semantics including the $SYS rule for level 1. Use it
// when you have a single (filter, topic) pair to test and don't want to
// allocate a tree.
func MatchFilter(filter, topic string) bool {
	if filter == topic {
		return true
	}
	if !strings.ContainsAny(filter, "+#") {
		return false
	}
	fl := strings.Split(filter, "/")
	tl := strings.Split(topic, "/")
	if len(tl) > 0 && strings.HasPrefix(tl[0], "$") {
		if len(fl) == 0 || !strings.HasPrefix(fl[0], "$") {
			return false
		}
	}
	fi, ti := 0, 0
	for fi < len(fl) {
		switch fl[fi] {
		case "#":
			return fi == len(fl)-1
		case "+":
			if ti >= len(tl) {
				return false
			}
			fi++
			ti++
		default:
			if ti >= len(tl) || fl[fi] != tl[ti] {
				return false
			}
			fi++
			ti++
		}
	}
	return ti == len(tl)
}
