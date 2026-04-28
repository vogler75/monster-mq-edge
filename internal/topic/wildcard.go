package topic

import "strings"

// WildcardIndex stores topic patterns containing '+' or '#' wildcards in a
// per-level Tree. Lookup against a published topic is O(depth). Mirror of
// TopicIndexWildcard in the Kotlin broker.
type WildcardIndex[V any] struct {
	tree *Tree[V]
}

func NewWildcardIndex[V any]() *WildcardIndex[V] {
	return &WildcardIndex[V]{tree: NewTree[V]()}
}

// Add stores (key → value) under the wildcard pattern.
func (w *WildcardIndex[V]) Add(pattern, key string, value V) {
	if !strings.ContainsAny(pattern, "+#") {
		panic("WildcardIndex: pattern must contain '+' or '#': " + pattern)
	}
	w.tree.AddData(pattern, key, value)
}

// Remove deletes (pattern, key). Returns true on success.
func (w *WildcardIndex[V]) Remove(pattern, key string) bool {
	w.tree.DelData(pattern, key)
	return true
}

// FindMatching returns every entry whose stored pattern matches the published
// topic. MQTT 3.1.1 $SYS rule applies.
func (w *WildcardIndex[V]) FindMatching(topic string) []Entry[V] {
	return w.tree.FindData(topic)
}

// HasMatching is FindMatching reduced to a bool.
func (w *WildcardIndex[V]) HasMatching(topic string) bool {
	return w.tree.IsMatching(topic)
}

// RemoveKey purges every entry for the given key from the index. Returns the
// patterns from which entries were removed.
func (w *WildcardIndex[V]) RemoveKey(key string) []string {
	return w.tree.RemoveKey(key)
}

// PatternCount returns the total number of (pattern, key) entries.
func (w *WildcardIndex[V]) PatternCount() int {
	return w.tree.Size()
}
