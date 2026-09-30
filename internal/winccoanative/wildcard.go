package winccoanative

import (
	"errors"
	"fmt"
	"strings"
)

// WildTarget is a native filter with MQTT wildcards. It is served by one
// dpQueryConnectSingle (spec-winccoa-native.md section 4.2):
//
//	winccoa/this/tags/#                 -> '*.**'
//	winccoa/this/tags/Pump1/#           -> 'Pump1.**'
//	winccoa/this/tags/Pump1/value/#     -> '{Pump1.value,Pump1.value.**}'
//	winccoa/this/tags/+/speed           -> '*.speed'
//	winccoa/this/types/Pump/#           -> '*.**' WHERE _DPT = "Pump"
//	winccoa/this/types/Pump/+/value/#   -> '{*.value,*.value.**}' WHERE _DPT = "Pump"
//	winccoa/remote/Sys/tags/...          -> ... REMOTE 'Sys'
type WildTarget struct {
	Remote   bool
	System   string   // remote system; empty for local
	Types    bool     // types/ form: publish to .../types/<DPT>/...
	TypeName string   // fixed DPT; empty = any type
	Path     []string // DP then elements; "+" = one level
	Hash     bool     // trailing '#'
}

var ErrWildcard = errors.New("unsupported wildcard filter")

// oaPatternChars cannot appear in a literal name inside a query pattern.
const oaPatternChars = "*?[]{},'\""

// ParseWildcard parses a native filter that contains '+' or '#'.
func ParseWildcard(filter string) (WildTarget, error) {
	var w WildTarget
	if Classify(filter) != KindNative || !HasWildcard(filter) {
		return w, fmt.Errorf("%w: not a native wildcard filter", ErrWildcard)
	}
	segs := strings.Split(filter, "/")[1:]
	i := 0
	switch segs[i] {
	case SegThis:
		i++
	case SegRemote:
		w.Remote = true
		i++
		if i >= len(segs) || segs[i] == "+" || segs[i] == "#" {
			return w, fmt.Errorf("%w: the remote system must be named", ErrWildcard)
		}
		sys, err := decodeSegment(segs[i], false)
		if err != nil || sys == "" {
			return w, fmt.Errorf("%w: bad remote system", ErrWildcard)
		}
		w.System = sys
		i++
	}
	if i >= len(segs) {
		return w, fmt.Errorf("%w: missing tags/types", ErrWildcard)
	}
	switch segs[i] {
	case SegTags:
		i++
	case SegTypes:
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
		return w, fmt.Errorf("%w: expected tags or types after the scope", ErrWildcard)
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

// Query returns the dpQueryConnectSingle statement. _online.._stime is
// selected to identify one value change when several queries overlap.
func (w WildTarget) Query() string {
	q := "SELECT '_online.._value', '_online.._stime' FROM '" + w.Pattern() + "'"
	if w.TypeName != "" {
		q += ` WHERE _DPT = "` + w.TypeName + `"`
	}
	if w.Remote {
		q += " REMOTE '" + w.System + "'"
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
	return form + "|" + w.Query()
}

// RowTarget maps a query row name ("Sys:DP.el.el" or "Sys:DP.") to the
// exact target it is published under. typeName is used for the types form.
func (w WildTarget) RowTarget(row, typeName string) (Target, bool) {
	sys, rest, ok := strings.Cut(row, ":")
	if !ok {
		return Target{}, false
	}
	dp, el, _ := strings.Cut(rest, ".")
	if dp == "" || Protected(dp) {
		return Target{}, false
	}
	t := Target{Remote: w.Remote, DP: dp, Attr: DefaultAttr}
	if w.Remote {
		t.System = sys
	}
	if w.Types {
		t.TypeName = typeName
	}
	if el != "" {
		t.Elements = strings.Split(el, ".")
	}
	return t, true
}
