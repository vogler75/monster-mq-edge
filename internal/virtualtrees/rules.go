package virtualtrees

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const maxTopicLength = 65535

// Match is MQTT filter matching; filters starting with a wildcard never match
// '$' topics and "a/#" matches "a".
func Match(filter, topic string) bool {
	if strings.HasPrefix(topic, "$") && (strings.HasPrefix(filter, "+") || strings.HasPrefix(filter, "#")) {
		return false
	}
	for {
		fi := strings.IndexByte(filter, '/')
		ti := strings.IndexByte(topic, '/')
		var fp, tp string
		if fi < 0 {
			fp = filter
		} else {
			fp = filter[:fi]
		}
		if fp == "#" {
			return fi < 0
		}
		if ti < 0 {
			tp = topic
		} else {
			tp = topic[:ti]
		}
		if fp != "+" && fp != tp {
			return false
		}
		if fi < 0 || ti < 0 {
			if fi < 0 && ti < 0 {
				return true
			}
			// filter "a/#" matches topic "a"
			return ti < 0 && filter[fi+1:] == "#"
		}
		filter = filter[fi+1:]
		topic = topic[ti+1:]
	}
}

// Under reports whether topic equals prefix or lies below it (whole levels only).
func Under(topic, prefix string) bool {
	return topic == prefix || (len(topic) > len(prefix) && strings.HasPrefix(topic, prefix) && topic[len(prefix)] == '/')
}

// Intersects reports whether the filter matches the prefix itself or any topic below it.
func Intersects(filter, prefix string) bool {
	f := strings.Split(filter, "/")
	p := strings.Split(prefix, "/")
	for i, part := range f {
		if part == "#" {
			return true
		}
		if i >= len(p) {
			return true
		}
		if part != "+" && part != p[i] {
			return false
		}
	}
	return len(f) >= len(p)
}

func validName(s string) bool {
	return s != "" && len(s) <= maxTopicLength && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

func validSegment(s string) bool {
	return validName(s) && !strings.ContainsAny(s, "/+#")
}

func hasWildcard(s string) bool { return strings.ContainsAny(s, "+#") }

// Validate checks candidate against the other configured trees (others must not
// contain the candidate). Only issues concerning the candidate are reported.
func Validate(t *Tree, others []*Tree) []Issue {
	var issues []Issue
	add := func(code, node, path, msg string) {
		issues = append(issues, Issue{Code: code, NodeID: node, Path: path, Message: msg})
	}
	if t.ID == "" {
		add(CodeInvalidTopic, "", "", "Tree id must not be empty")
	}
	if t.SchemaVersion != SchemaVersion {
		add(CodeSchemaVersion, "", t.RootPath, fmt.Sprintf("Unsupported schemaVersion %d", t.SchemaVersion))
	}
	switch {
	case !validName(t.RootPath) || hasWildcard(t.RootPath):
		add(CodeInvalidTopic, "", t.RootPath, fmt.Sprintf("Invalid root path '%s'", t.RootPath))
	case strings.HasPrefix(t.RootPath, "$"):
		add(CodeReservedTopic, "", t.RootPath, "Root path must not start with '$'")
	}
	ids := map[string]bool{}
	for _, n := range t.Nodes {
		if ids[n.ID] {
			add(CodeDuplicateID, n.ID, "", fmt.Sprintf("Duplicate node id '%s'", n.ID))
		}
		ids[n.ID] = true
	}
	roots := make([]string, 0, len(others)+1)
	for _, o := range others {
		roots = append(roots, o.RootPath)
	}
	roots = append(roots, t.RootPath)
	paths := t.Paths()
	seen := map[string]string{}
	for _, n := range t.Nodes {
		if n.ID == "" {
			add(CodeInvalidSegment, n.ID, "", "Node id must not be empty")
		}
		if !validSegment(n.Segment) {
			add(CodeInvalidSegment, n.ID, "", fmt.Sprintf("Invalid segment '%s'", n.Segment))
		}
		if n.ParentID != nil && !ids[*n.ParentID] {
			add(CodeInvalidParent, n.ID, "", fmt.Sprintf("Unknown parent '%s'", *n.ParentID))
			continue
		}
		p, ok := paths[n.ID]
		if !ok {
			add(CodeCycle, n.ID, "", "Node is part of a cycle")
			continue
		}
		if len(p) > maxTopicLength {
			add(CodeInvalidTopic, n.ID, "", "Derived path is too long")
		}
		if other, dup := seen[p]; dup && other != n.ID {
			add(CodeDuplicatePath, n.ID, p, fmt.Sprintf("Path '%s' is already used by node '%s'", p, other))
		}
		seen[p] = n.ID
		b := n.Binding
		if b == nil {
			continue
		}
		switch {
		case (b.Kind != Exact && b.Kind != Subtree) || (b.Access != ReadOnly && b.Access != ReadWrite):
			add(CodeInvalidBinding, n.ID, p, fmt.Sprintf("Invalid binding kind '%s' or access '%s'", b.Kind, b.Access))
		case hasWildcard(b.Target):
			add(CodeUnsupportedWild, n.ID, p, fmt.Sprintf("Binding target '%s' must not contain wildcards", b.Target))
		case !validName(b.Target):
			add(CodeInvalidTopic, n.ID, p, fmt.Sprintf("Invalid binding target '%s'", b.Target))
		}
		for _, root := range roots {
			if Under(b.Target, root) {
				add(CodeTargetInRoot, n.ID, p, fmt.Sprintf("Binding target '%s' lies in virtual root '%s'", b.Target, root))
			}
		}
	}
	for _, o := range others {
		if Under(o.RootPath, t.RootPath) || Under(t.RootPath, o.RootPath) {
			add(CodeRootConflict, "", t.RootPath, fmt.Sprintf("Root '%s' overlaps root '%s' of tree '%s'", t.RootPath, o.RootPath, o.ID))
		}
		for _, n := range o.Nodes {
			if n.Binding != nil && Under(n.Binding.Target, t.RootPath) {
				add(CodeTargetInRoot, "", t.RootPath, fmt.Sprintf("Root '%s' contains target '%s' of tree '%s'", t.RootPath, n.Binding.Target, o.ID))
			}
		}
	}
	return issues
}
