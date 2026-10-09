package peerlink

import (
	"fmt"
	"strings"
)

// topicFilter is a precompiled set of MQTT topic filters (plan 7.2). Matching does not allocate:
// "#" needs no test, plain topics are one map lookup, "<prefix>/#" entries are prefix tests and only
// the remaining filters walk a level trie with an index-walking iterator.
type topicFilter struct {
	all      bool
	exact    map[string]struct{}
	prefixes []string
	trie     *filterNode
}

type filterNode struct {
	children map[string]*filterNode
	plus     *filterNode
	hash     bool // a "#" below this node: matches the node itself and everything under it
	end      bool
}

// compileFilters compiles list. An empty list matches nothing.
func compileFilters(list []string) (*topicFilter, error) {
	f := &topicFilter{}
	for _, raw := range list {
		if err := validFilter(raw); err != nil {
			return nil, err
		}
		switch {
		case raw == "#":
			f.all = true
		case !strings.ContainsAny(raw, "+#"):
			if f.exact == nil {
				f.exact = make(map[string]struct{})
			}
			f.exact[raw] = struct{}{}
		case strings.HasSuffix(raw, "/#") && !strings.ContainsAny(raw[:len(raw)-2], "+#"):
			f.prefixes = append(f.prefixes, raw[:len(raw)-2])
		default:
			if f.trie == nil {
				f.trie = &filterNode{}
			}
			f.trie.add(raw)
		}
	}
	return f, nil
}

func validFilter(s string) error {
	if s == "" {
		return fmt.Errorf("empty topic filter")
	}
	levels := strings.Split(s, "/")
	for i, l := range levels {
		if strings.Contains(l, "#") && (l != "#" || i != len(levels)-1) {
			return fmt.Errorf("invalid topic filter %q: '#' must be the last level on its own", s)
		}
		if strings.Contains(l, "+") && l != "+" {
			return fmt.Errorf("invalid topic filter %q: '+' must be a level on its own", s)
		}
	}
	return nil
}

func (n *filterNode) add(filter string) {
	cur := n
	for _, l := range strings.Split(filter, "/") {
		switch l {
		case "#":
			cur.hash = true
			return
		case "+":
			if cur.plus == nil {
				cur.plus = &filterNode{}
			}
			cur = cur.plus
		default:
			if cur.children == nil {
				cur.children = make(map[string]*filterNode)
			}
			next := cur.children[l]
			if next == nil {
				next = &filterNode{}
				cur.children[l] = next
			}
			cur = next
		}
	}
	cur.end = true
}

// match walks the levels of t starting at index i; i > len(t) means every level was consumed.
func (n *filterNode) match(t string, i int) bool {
	if n.hash {
		return true
	}
	if i > len(t) {
		return n.end
	}
	var level string
	next := len(t) + 1
	if j := strings.IndexByte(t[i:], '/'); j >= 0 {
		level, next = t[i:i+j], i+j+1
	} else {
		level = t[i:]
	}
	if c := n.children[level]; c != nil && c.match(t, next) {
		return true
	}
	return n.plus != nil && n.plus.match(t, next)
}

// match reports whether any filter of the set matches the topic name t.
func (f *topicFilter) match(t string) bool {
	if f == nil {
		return false
	}
	if f.all {
		return true
	}
	if _, ok := f.exact[t]; ok {
		return true
	}
	for _, p := range f.prefixes {
		if underRoot(t, p) {
			return true
		}
	}
	return f.trie != nil && f.trie.match(t, 0)
}

// underRoot reports t == root || t starts with root + "/". An empty root matches nothing.
func underRoot(t, root string) bool {
	if root == "" || len(t) < len(root) || t[:len(root)] != root {
		return false
	}
	return len(t) == len(root) || t[len(root)] == '/'
}

// includeExclude is an Include list minus an Exclude list; Exclude wins.
type includeExclude struct {
	include *topicFilter
	exclude *topicFilter
}

func newIncludeExclude(include, exclude []string) (includeExclude, error) {
	if len(include) == 0 {
		include = []string{"#"}
	}
	in, err := compileFilters(include)
	if err != nil {
		return includeExclude{}, err
	}
	ex, err := compileFilters(exclude)
	if err != nil {
		return includeExclude{}, err
	}
	return includeExclude{include: in, exclude: ex}, nil
}

func (f includeExclude) accept(t string) bool {
	return f.include.match(t) && !f.exclude.match(t)
}

// maskNode is a level of the interest union trie (plan-peerlink-interest-routing 6.1). endMask holds the
// consumers with a filter ending at this node, hashMask those with a "#" below it. A node and its kind
// identify exactly one filter, so a consumer's bit is set or cleared without counting.
type maskNode struct {
	children map[string]*maskNode
	plus     *maskNode
	endMask  uint64
	hashMask uint64
}

func (n *maskNode) empty() bool {
	return n.endMask == 0 && n.hashMask == 0 && n.plus == nil && len(n.children) == 0
}

// set sets (on) or clears bit in the node of filter, creating nodes on set and pruning empty ones on clear.
func (n *maskNode) set(filter string, bit uint64, on bool) {
	n.setLevels(filter, bit, on)
}

func (n *maskNode) setLevels(rest string, bit uint64, on bool) {
	level, tail, last := rest, "", true
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		level, tail, last = rest[:j], rest[j+1:], false
	}
	if level == "#" {
		if on {
			n.hashMask |= bit
		} else {
			n.hashMask &^= bit
		}
		return
	}
	var child *maskNode
	if level == "+" {
		child = n.plus
	} else {
		child = n.children[level]
	}
	if child == nil {
		if !on {
			return
		}
		child = &maskNode{}
		if level == "+" {
			n.plus = child
		} else {
			if n.children == nil {
				n.children = make(map[string]*maskNode)
			}
			n.children[level] = child
		}
	}
	if last {
		if on {
			child.endMask |= bit
		} else {
			child.endMask &^= bit
		}
	} else {
		child.setLevels(tail, bit, on)
	}
	if !on && child.empty() {
		if level == "+" {
			n.plus = nil
		} else {
			delete(n.children, level)
		}
	}
}

// match returns the union of the masks of all filters matching the topic name t. Topics starting with
// '$' are not matched by a leading wildcard (MQTT 4.7.2).
func (n *maskNode) match(t string, want uint64) uint64 {
	if len(t) > 0 && t[0] == '$' {
		var m uint64
		level, next := splitLevel(t, 0)
		if c := n.children[level]; c != nil {
			m = c.matchFrom(t, next, want)
		}
		return m
	}
	if n.hashMask&want == want {
		return n.hashMask
	}
	return n.matchFrom(t, 0, want) | n.hashMask
}

// matchFrom matches the levels of t from index i against the children of n; i > len(t) means every level
// was consumed by n itself. It stops early once every bit in want is set.
func (n *maskNode) matchFrom(t string, i int, want uint64) uint64 {
	if i > len(t) {
		return n.endMask
	}
	level, next := splitLevel(t, i)
	var m uint64
	if c := n.children[level]; c != nil {
		if m = c.hashMask; m&want != want {
			m |= c.matchFrom(t, next, want)
		}
	}
	if c := n.plus; c != nil && m&want != want {
		m |= c.hashMask
		if m&want != want {
			m |= c.matchFrom(t, next, want)
		}
	}
	return m
}

func splitLevel(t string, i int) (string, int) {
	if j := strings.IndexByte(t[i:], '/'); j >= 0 {
		return t[i : i+j], i + j + 1
	}
	return t[i:], len(t) + 1
}
