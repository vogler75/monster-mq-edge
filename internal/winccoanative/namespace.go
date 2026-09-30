// Package winccoanative implements the native WinCC OA MQTT namespace,
// subscription interests and typed writes (spec-winccoa-native.md section 4).
package winccoanative

import (
	"errors"
	"fmt"
	"strings"
)

const (
	Root        = "winccoa"
	SegTags     = "tags"
	SegTypes    = "types"
	SegCNS      = "cns"
	SegSet      = "set"
	DefaultAttr = "_online.._value"
	WriteAttr   = "_original.._value"
)

// ReadAttrs is the read attribute allowlist.
var ReadAttrs = map[string]bool{
	"_online.._value":   true,
	"_online.._stime":   true,
	"_online.._status":  true,
	"_online.._invalid": true,
}

type Kind int

const (
	KindOther  Kind = iota // not in the native namespace
	KindNative             // winccoa/<system>/...
	KindStatus             // winccoa/<system>: broker status (retained JSON)
	KindCNS                // winccoa/<system>/cns/... (reserved, disabled)
)

var (
	ErrMalformed    = errors.New("malformed native topic")
	ErrNoncanonical = errors.New("noncanonical native topic encoding")
	ErrAttribute    = errors.New("attribute not allowed")
)

// Target is a parsed native topic.
type Target struct {
	System   string // WinCC OA system name (the first level)
	TypeName string // non-empty for the types/ form
	DP       string
	Elements []string
	Attr     string // explicit attribute or DefaultAttr
	Explicit bool   // attribute segment was present
	Command  bool   // terminal set
}

// Classify reports which reserved branch a topic name or filter belongs to.
// Shared-subscription prefixes must be stripped by the caller.
func Classify(topic string) Kind {
	if topic != Root && !strings.HasPrefix(topic, Root+"/") {
		return KindOther
	}
	rest := strings.TrimPrefix(topic, Root)
	rest = strings.TrimPrefix(rest, "/")
	system, after, more := strings.Cut(rest, "/")
	switch {
	case system == "" || system == "+" || system == "#":
		// "winccoa", "winccoa/#", "winccoa/+/...": ordinary MQTT filters.
		return KindOther
	case !more:
		return KindStatus
	case after == SegCNS || strings.HasPrefix(after, SegCNS+"/"):
		return KindCNS
	}
	return KindNative
}

// HasWildcard reports whether a filter contains an MQTT wildcard.
func HasWildcard(filter string) bool { return strings.ContainsAny(filter, "+#") }

// SplitShared returns the underlying filter of a $share/<group>/<filter>.
func SplitShared(filter string) (string, bool) {
	if !strings.HasPrefix(filter, "$share/") {
		return filter, false
	}
	rest := strings.TrimPrefix(filter, "$share/")
	_, f, ok := strings.Cut(rest, "/")
	if !ok {
		return "", true
	}
	return f, true
}

// Parse parses an exact native topic (no wildcards).
func Parse(topic string) (Target, error) {
	var t Target
	if Classify(topic) != KindNative {
		return t, fmt.Errorf("%w: not a native topic", ErrMalformed)
	}
	if HasWildcard(topic) {
		return t, fmt.Errorf("%w: wildcard", ErrMalformed)
	}
	segs := strings.Split(topic, "/")[1:]
	sys, err := decodeSegment(segs[0], false)
	if err != nil {
		return t, err
	}
	if sys == "" {
		return t, fmt.Errorf("%w: missing system", ErrMalformed)
	}
	t.System = sys
	i := 1
	if i >= len(segs) {
		return t, fmt.Errorf("%w: missing tags/types", ErrMalformed)
	}
	switch segs[i] {
	case SegTags:
		i++
	case SegTypes:
		i++
		if i >= len(segs) {
			return t, fmt.Errorf("%w: missing type name", ErrMalformed)
		}
		tn, err := decodeSegment(segs[i], false)
		if err != nil {
			return t, err
		}
		t.TypeName = tn
		i++
	default:
		return t, fmt.Errorf("%w: expected tags or types, got %q", ErrMalformed, segs[i])
	}
	rest := segs[i:]
	if len(rest) == 0 {
		return t, fmt.Errorf("%w: missing datapoint", ErrMalformed)
	}

	last := rest[len(rest)-1]
	switch {
	case last == SegSet:
		t.Command = true
		rest = rest[:len(rest)-1]
	case strings.HasPrefix(last, "_") && len(rest) > 1:
		if !ReadAttrs[last] {
			return t, fmt.Errorf("%w: %q", ErrAttribute, last)
		}
		t.Attr = last
		t.Explicit = true
		rest = rest[:len(rest)-1]
	}
	if len(rest) == 0 {
		return t, fmt.Errorf("%w: missing datapoint", ErrMalformed)
	}
	for j, s := range rest {
		name, err := decodeSegment(s, j == len(rest)-1)
		if err != nil {
			return t, err
		}
		if name == "" {
			return t, fmt.Errorf("%w: empty segment", ErrMalformed)
		}
		if j == 0 {
			t.DP = name
		} else {
			t.Elements = append(t.Elements, name)
		}
	}
	if t.Attr == "" {
		t.Attr = DefaultAttr
	}
	return t, nil
}

// DPE returns the DPE access name including the system prefix. A DP-only
// target yields the root form "Sys:DP." (the caller must verify that the
// root is a value element); element paths never get a trailing dot.
func (t Target) DPE(system string) string {
	name := t.DP
	if len(t.Elements) > 0 {
		name += "." + strings.Join(t.Elements, ".")
	}
	if !strings.Contains(name, ".") {
		name += "."
	}
	return system + ":" + name
}

// DPName is the datapoint lifecycle name (no trailing dot).
func (t Target) DPName(system string) string { return system + ":" + t.DP }

func (t Target) ReadAddress(system string) string  { return t.DPE(system) + ":" + t.Attr }
func (t Target) WriteAddress(system string) string { return t.DPE(system) + ":" + WriteAttr }

// IsRoot reports whether the target addresses the DP root element.
func (t Target) IsRoot() bool { return len(t.Elements) == 0 }

// Topic renders the canonical topic for the target in the given form.
func (t Target) Topic() string {
	segs := []string{Root, encodeSegment(t.System, false)}
	if t.TypeName != "" {
		segs = append(segs, SegTypes, encodeSegment(t.TypeName, false))
	} else {
		segs = append(segs, SegTags)
	}
	path := append([]string{t.DP}, t.Elements...)
	for j, s := range path {
		segs = append(segs, encodeSegment(s, j == len(path)-1))
	}
	if t.Command {
		segs = append(segs, SegSet)
	} else if t.Explicit {
		segs = append(segs, t.Attr)
	}
	return strings.Join(segs, "/")
}

// Key identifies the OA read target independent of the topic alias form.
func (t Target) Key(system string) string { return t.ReadAddress(system) }

func needsEscape(c byte) bool {
	return c == '%' || c == '/' || c == '+' || c == '#' || c < 0x20
}

func isReservedToken(s string, last bool) bool {
	if !last {
		return false
	}
	return s == SegSet || strings.HasPrefix(s, "_")
}

func encodeSegment(s string, last bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if needsEscape(c) || (i == 0 && isReservedToken(s, last)) {
			fmt.Fprintf(&b, "%%%02X", c)
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// EncodeName encodes one DP/element/system name as a topic segment.
func EncodeName(s string, last bool) string { return encodeSegment(s, last) }

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// decodeSegment decodes a percent-encoded segment and enforces the canonical
// form: only required escapes (plus the first character of a reserved-token
// spelling in the last path segment) are allowed, uppercase hex only.
func decodeSegment(s string, last bool) (string, error) {
	var out []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '%' {
			out = append(out, c)
			continue
		}
		if i+2 >= len(s) {
			return "", fmt.Errorf("%w: truncated escape", ErrNoncanonical)
		}
		hi, ok1 := unhex(s[i+1])
		lo, ok2 := unhex(s[i+2])
		if !ok1 || !ok2 {
			return "", fmt.Errorf("%w: bad escape in %q", ErrNoncanonical, s)
		}
		d := hi<<4 | lo
		if !needsEscape(d) && !(i == 0 && last) {
			return "", fmt.Errorf("%w: unnecessary escape in %q", ErrNoncanonical, s)
		}
		out = append(out, d)
		i += 2
	}
	name := string(out)
	if strings.HasPrefix(s, "%") && last && len(out) > 0 && !needsEscape(out[0]) && !isReservedToken(name, true) {
		return "", fmt.Errorf("%w: unnecessary escape in %q", ErrNoncanonical, s)
	}
	for i := 0; i < len(name); i++ {
		if name[i] == '.' || name[i] == ':' || name[i] < 0x20 {
			return "", fmt.Errorf("%w: illegal character in name %q", ErrMalformed, name)
		}
	}
	return name, nil
}
