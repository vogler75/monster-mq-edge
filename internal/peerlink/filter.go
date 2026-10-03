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
