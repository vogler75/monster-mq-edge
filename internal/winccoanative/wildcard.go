package winccoanative

import (
	"errors"
	"fmt"
	"strings"
)

// WildTarget is a native filter with MQTT wildcards. It is served by one
// dpQueryConnectSingle (spec-winccoa-native.md section 4.2):
//
//	winccoa/<sys>/tags/#                 -> '*.**'
//	winccoa/<sys>/tags/<dp>/#            -> '<dp>.**'
//	winccoa/<sys>/tags/<dp>/<el>/#       -> '{<dp>.<el>,<dp>.<el>.**}'
//	winccoa/<sys>/tags/+/<el>            -> '*.<el>'
//	winccoa/<sys>/types/<dpt>/#          -> '*.**' WHERE _DPT = "<dpt>"
//	winccoa/<sys>/types/<dpt>/+/<el>/#   -> '{*.<el>,*.<el>.**}' WHERE _DPT = "<dpt>"
//
// A system other than the local one adds REMOTE '<sys>' after FROM.
type WildTarget struct {
	Remote   bool     // System is not the local system (set by the service)
	System   string   // WinCC OA system name
	Types    bool     // types/ form: publish to .../types/<DPT>/...
	TypeName string   // fixed DPT; empty = any type
	Path     []string // DP then elements; "+" = one level
	Hash     bool     // trailing '#'

	names Names // names the filter was parsed with; used for row topics
}

var ErrWildcard = errors.New("unsupported wildcard filter")

// oaPatternChars cannot appear in a literal name inside a query pattern.
const oaPatternChars = "*?[]{},'\""

// ParseWildcard parses a native filter that contains '+' or '#'.
func (n Names) ParseWildcard(filter string) (WildTarget, error) {
	w := WildTarget{names: n}
	if n.Classify(filter) != KindNative || !HasWildcard(filter) {
		return w, fmt.Errorf("%w: not a native wildcard filter", ErrWildcard)
	}
	sys, segs, err := n.scope(filter)
	if err != nil {
		return w, fmt.Errorf("%w: bad system", ErrWildcard)
	}
	w.System = sys
	i := 0
	if i >= len(segs) {
		return w, fmt.Errorf("%w: missing %s/%s", ErrWildcard, n.Tags, n.Types)
	}
	switch segs[i] {
	case n.Tags:
		i++
	case n.Types:
		w.Types = true
		i++
		if i >= len(segs) {
			return w, fmt.Errorf("%w: missing type", ErrWildcard)
		}
		switch segs[i] {
		case "#":
			w.Hash = true
			if i != len(segs)-1 {
				return w, fmt.Errorf("%w: '#' must be last", ErrWildcard)
			}
			return w, nil
		case "+":
		default:
			tn, err := decodeSegment(segs[i], false)
			if err != nil || tn == "" || strings.ContainsAny(tn, oaPatternChars) {
				return w, fmt.Errorf("%w: bad type name", ErrWildcard)
			}
			w.TypeName = tn
		}
		i++
	default:
		return w, fmt.Errorf("%w: expected %s or %s after the system", ErrWildcard, n.Tags, n.Types)
	}
	rest := segs[i:]
	for j, s := range rest {
		switch s {
		case "#":
			if j != len(rest)-1 {
				return w, fmt.Errorf("%w: '#' must be last", ErrWildcard)
			}
			w.Hash = true
		case "+":
			w.Path = append(w.Path, "+")
		default:
			if s == SegSet && j == len(rest)-1 {
				return w, fmt.Errorf("%w: commands cannot be subscribed", ErrWildcard)
			}
			if strings.HasPrefix(s, "_") {
				// Attributes and internal datapoints are not reachable
				// through wildcards.
				return w, fmt.Errorf("%w: attribute or internal name %q", ErrWildcard, s)
			}
			name, err := decodeSegment(s, false)
			if err != nil || name == "" || strings.ContainsAny(name, oaPatternChars) {
				return w, fmt.Errorf("%w: bad name %q", ErrWildcard, s)
			}
			w.Path = append(w.Path, name)
		}
	}
	if len(w.Path) == 0 && !w.Hash {
		return w, fmt.Errorf("%w: missing datapoint", ErrWildcard)
	}
	return w, nil
}

// IsRoot reports a filter that covers every datapoint of the system (no
// datapoint name and no type restriction). AllowRootWildcardSubscription
// governs it like '#'.
func (w WildTarget) IsRoot() bool {
	if w.TypeName != "" {
		return false
	}
	return len(w.Path) == 0 || w.Path[0] == "+"
}

// Pattern returns the dpQuery FROM pattern.
func (w WildTarget) Pattern() string {
	seg := func(s string) string {
		if s == "+" {
			return "*"
		}
		return s
	}
	dp := "*"
	if len(w.Path) > 0 {
		dp = seg(w.Path[0])
	}
	var els []string
	for _, e := range w.Path[min(1, len(w.Path)):] {
		els = append(els, seg(e))
	}
	base := dp
	if len(els) > 0 {
		base += "." + strings.Join(els, ".")
	}
	switch {
	case w.Hash && len(els) == 0:
		// DP.** includes the root of a scalar datapoint (verified on 3.21).
		return base + ".**"
	case w.Hash:
		return "{" + base + "," + base + ".**}"
	case len(els) == 0:
		return base + "."
	}
	return base
}

// Query returns the dpQueryConnectSingle statement. _online.._stime is the
// published time and identifies one value change when several queries
// overlap.
func (w WildTarget) Query() string {
	// WinCC OA expects REMOTE directly after FROM, before WHERE.
	q := "SELECT '_online.._value', '_online.._stime' FROM '" + w.Pattern() + "'"
	if w.Remote {
		q += " REMOTE '" + w.System + "'"
	}
	if w.TypeName != "" {
		q += ` WHERE _DPT = "` + w.TypeName + `"`
	}
	return q
}

// Key identifies the query and its publishing form; subscribers with the
// same key share one registration.
func (w WildTarget) Key() string {
	form := "tags"
	if w.Types {
		form = "types"
	}
	// The system part keeps the shortcut and the explicit form apart: the
	// same query publishes under different topics.
	return form + "|" + w.System + "|" + w.Query()
}

// RowTarget maps a query row name ("Sys:DP.el.el" or "Sys:DP.") to the
// exact target it is published under. typeName is used for the types form.
func (w WildTarget) RowTarget(row, typeName string) (Target, bool) {
	_, rest, ok := strings.Cut(row, ":")
	if !ok {
		return Target{}, false
	}
	dp, el, _ := strings.Cut(rest, ".")
	if dp == "" || Protected(dp) {
		return Target{}, false
	}
	// The row keeps the filter's form: "" stays the local shortcut.
	t := Target{System: w.System, DP: dp, Attr: DefaultAttr, names: w.names}
	if w.Types {
		t.TypeName = typeName
	}
	if el != "" {
		t.Elements = strings.Split(el, ".")
	}
	return t, true
}
