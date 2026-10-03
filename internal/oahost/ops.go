package oahost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Operation codes (spec section 3.1).
const (
	OpResolve         uint32 = 1
	OpSysInfo         uint32 = 2
	OpQueryConnect    uint32 = 3
	OpQueryDisconnect uint32 = 4
	OpDpConnect       uint32 = 5
	OpDpDisconnect    uint32 = 6
	OpDpSet           uint32 = 7
	OpDpGet           uint32 = 8
	OpDpNames         uint32 = 9
	OpDpCreate        uint32 = 10
	OpDpDelete        uint32 = 11
	OpTypeCheck       uint32 = 12
)

// Request flags.
const (
	FlagAnswer   uint32 = 1 << 0 // query/connect: deliver the initial answer as an event
	FlagNoSource uint32 = 1 << 1 // connect: dpConnectNoSource
	FlagWait     uint32 = 1 << 2 // set: require the OA answer (always set by this package)
	FlagMore     uint32 = 1 << 3 // event: the query answer continues in another event
	FlagCreate   uint32 = 1 << 4 // type check: create a missing type from the given elements
)

// Resolution is the answer to OpResolve for one name.
type Resolution struct {
	Name     string // fully qualified DPE with config, as OA reports it
	Exists   bool
	TypeName string
	ElemType uint32 // 0 means structure node (not a value element)
	System   string
}

type SysInfo struct {
	LocalSystem string
	// Redundant is true for a redundant project. Hosts are the event hosts
	// 1 and 2 (empty for a non-redundant project), LocalHost the name of
	// the computer the manager runs on, Replica the manager's replica number.
	Redundant bool
	Replica   uint32
	Hosts     []string
	LocalHost string
}

// OA API is the typed front end used by the bridge, stores and namespace.
type API struct{ C *Client }

func (a API) call(ctx context.Context, w *Writer, timeout time.Duration) (Message, error) {
	return a.C.Call(ctx, w.Bytes(), timeout)
}

// logCall writes one DEBUG line per completed call: the WinCC OA function,
// its arguments, the duration and the error, if any.
func (a API) logCall(op string, start time.Time, err error, attrs ...any) {
	if a.C == nil {
		return
	}
	l := a.C.logger.Load()
	if l == nil || !l.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	attrs = append(attrs, "duration", time.Since(start).Round(time.Microsecond))
	if err != nil {
		attrs = append(attrs, "err", err)
	}
	l.Debug("oa "+op, attrs...)
}

// maxLoggedItems bounds the names/values listed in one log line.
const maxLoggedItems = 20

// nameList formats a name batch only when the line is actually written.
type nameList []string

func (n nameList) LogValue() slog.Value {
	s := []string(n)
	more := 0
	if len(s) > maxLoggedItems {
		s, more = s[:maxLoggedItems], len(s)-maxLoggedItems
	}
	out := "[" + strings.Join(s, " ") + "]"
	if more > 0 {
		out += fmt.Sprintf(" (+%d more)", more)
	}
	return slog.StringValue(out)
}

type valueList []Value

func (v valueList) LogValue() slog.Value {
	s := []Value(v)
	more := 0
	if len(s) > maxLoggedItems {
		s, more = s[:maxLoggedItems], len(s)-maxLoggedItems
	}
	parts := make([]string, len(s))
	for i, x := range s {
		b, _ := json.Marshal(x.JSON())
		parts[i] = string(b)
	}
	out := "[" + strings.Join(parts, " ") + "]"
	if more > 0 {
		out += fmt.Sprintf(" (+%d more)", more)
	}
	return slog.StringValue(out)
}

func (a API) SysInfo(ctx context.Context) (SysInfo, error) {
	var w Writer
	w.U32(TagOp, OpSysInfo)
	start := time.Now()
	m, err := a.call(ctx, &w, 0)
	a.logCall("sysInfo", start, err, "system", m.String(TagSysName))
	if err != nil {
		return SysInfo{}, err
	}
	info := SysInfo{LocalSystem: m.String(TagSysName), Redundant: m.Bool(TagRedundant), LocalHost: m.String(TagLocalHost)}
	info.Replica, _ = m.U32(TagReplica)
	for _, h := range m.All(TagHost) {
		info.Hosts = append(info.Hosts, string(h))
	}
	return info, nil
}

// Resolve checks a DPE name (with optional system prefix and config) on the
// manager thread. Missing names are reported as Exists=false, not as errors;
// an unreachable system yields ErrUnavailable.
func (a API) Resolve(ctx context.Context, name string) (Resolution, error) {
	var w Writer
	w.U32(TagOp, OpResolve)
	w.String(TagName, name)
	start := time.Now()
	m, err := a.call(ctx, &w, 0)
	a.logCall("resolve", start, err, "name", name, "exists", m.Bool(TagExists), "type", m.String(TagTypeName))
	if err != nil {
		return Resolution{}, err
	}
	et, _ := m.U32(TagElemType)
	return Resolution{
		Name:     m.String(TagName),
		Exists:   m.Bool(TagExists),
		TypeName: m.String(TagTypeName),
		ElemType: et,
		System:   m.String(TagSysName),
	}, nil
}

// QueryConnect registers dpQueryConnectSingle. Rows arrive as events on the
// returned ref; the call returns only after OA answered the registration.
func (a API) QueryConnect(ctx context.Context, query string, answer bool, h EventHandler, timeout time.Duration) (uint64, error) {
	ref := a.C.NewRef(h)
	var w Writer
	w.U32(TagOp, OpQueryConnect)
	w.String(TagQuery, query)
	w.U64(TagRef, ref)
	if answer {
		w.U32(TagFlags, FlagAnswer)
	}
	start := time.Now()
	_, err := a.call(ctx, &w, timeout)
	a.logCall("dpQueryConnectSingle", start, err, "ref", ref, "query", query, "answer", answer)
	if err != nil {
		// The host owns any registration it sent; it disconnects it itself
		// on a failed answer. Only the Go-side route is dropped here.
		a.C.DropRef(ref)
		return 0, err
	}
	return ref, nil
}

func (a API) QueryDisconnect(ctx context.Context, ref uint64) error {
	var w Writer
	w.U32(TagOp, OpQueryDisconnect)
	w.U64(TagRef, ref)
	start := time.Now()
	_, err := a.call(ctx, &w, 0)
	a.logCall("dpQueryDisconnect", start, err, "ref", ref)
	a.C.DropRef(ref)
	return err
}

// DpConnect connects a batch of DPEs (len(names) <= connect batch size) under
// one reference. Each hotlink event carries name/value pairs.
func (a API) DpConnect(ctx context.Context, names []string, flags uint32, h EventHandler, timeout time.Duration) (uint64, error) {
	if len(names) == 0 {
		return 0, ErrInvalid
	}
	ref := a.C.NewRef(h)
	var w Writer
	w.U32(TagOp, OpDpConnect)
	w.U64(TagRef, ref)
	w.U32(TagFlags, flags)
	for _, n := range names {
		w.String(TagName, n)
	}
	start := time.Now()
	_, err := a.call(ctx, &w, timeout)
	op := "dpConnect"
	if flags&FlagNoSource != 0 {
		op = "dpConnectNoSource"
	}
	a.logCall(op, start, err, "ref", ref, "count", len(names), "names", nameList(names), "answer", flags&FlagAnswer != 0)
	if err != nil {
		a.C.DropRef(ref)
		return 0, err
	}
	return ref, nil
}

func (a API) DpDisconnect(ctx context.Context, ref uint64) error {
	var w Writer
	w.U32(TagOp, OpDpDisconnect)
	w.U64(TagRef, ref)
	start := time.Now()
	_, err := a.call(ctx, &w, 0)
	a.logCall("dpDisconnect", start, err, "ref", ref)
	a.C.DropRef(ref)
	return err
}

// DpSet writes values and waits for the OA answer. names and values are
// fully qualified config paths and must have equal length.
func (a API) DpSet(ctx context.Context, names []string, values []Value, timeout time.Duration) error {
	if len(names) == 0 || len(names) != len(values) {
		return ErrInvalid
	}
	var w Writer
	w.U32(TagOp, OpDpSet)
	w.U32(TagFlags, FlagWait)
	for i := range names {
		w.String(TagName, names[i])
		w.Value(TagValue, values[i])
	}
	start := time.Now()
	_, err := a.call(ctx, &w, timeout)
	a.logCall("dpSetWait", start, err, "count", len(names), "names", nameList(names), "values", valueList(values))
	return err
}

func (a API) DpGet(ctx context.Context, names []string, timeout time.Duration) ([]Value, error) {
	var w Writer
	w.U32(TagOp, OpDpGet)
	for _, n := range names {
		w.String(TagName, n)
	}
	start := time.Now()
	m, err := a.call(ctx, &w, timeout)
	a.logCall("dpGet", start, err, "count", len(names), "names", nameList(names))
	if err != nil {
		return nil, err
	}
	raw := m.All(TagValue)
	if len(raw) != len(names) {
		return nil, fmt.Errorf("%w: dpGet returned %d values for %d names", ErrOA, len(raw), len(names))
	}
	out := make([]Value, len(raw))
	for i, b := range raw {
		v, err := DecodeValue(b)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// DpNames enumerates datapoints matching pattern with the given DPT.
func (a API) DpNames(ctx context.Context, pattern, typeName string, timeout time.Duration) ([]string, error) {
	var w Writer
	w.U32(TagOp, OpDpNames)
	w.String(TagName, pattern)
	w.String(TagTypeName, typeName)
	start := time.Now()
	m, err := a.call(ctx, &w, timeout)
	names := m.All(TagName)
	a.logCall("dpNames", start, err, "pattern", pattern, "type", typeName, "found", len(names))
	if err != nil {
		return nil, err
	}
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = string(n)
	}
	return out, nil
}

// DpCreate creates a datapoint by DP name (no trailing dot).
func (a API) DpCreate(ctx context.Context, dpName, typeName string, timeout time.Duration) error {
	var w Writer
	w.U32(TagOp, OpDpCreate)
	w.String(TagName, dpName)
	w.String(TagTypeName, typeName)
	start := time.Now()
	_, err := a.call(ctx, &w, timeout)
	a.logCall("dpCreate", start, err, "dp", dpName, "type", typeName)
	return err
}

// DpDelete deletes a datapoint by DP name (no trailing dot).
func (a API) DpDelete(ctx context.Context, dpName string, timeout time.Duration) error {
	var w Writer
	w.U32(TagOp, OpDpDelete)
	w.String(TagName, dpName)
	start := time.Now()
	_, err := a.call(ctx, &w, timeout)
	a.logCall("dpDelete", start, err, "dp", dpName)
	return err
}

// TypeCheck reports whether typeName exists and, if elements are given,
// that each element exists with the expected element type.
func (a API) TypeCheck(ctx context.Context, typeName string, elements []string, elemTypes []uint32) error {
	var w Writer
	w.U32(TagOp, OpTypeCheck)
	w.String(TagTypeName, typeName)
	for i, e := range elements {
		w.String(TagName, e)
		w.U32(TagElemType, elemTypes[i])
	}
	start := time.Now()
	_, err := a.call(ctx, &w, 0)
	a.logCall("typeCheck", start, err, "type", typeName, "elements", nameList(elements))
	return err
}

// EnsureType creates typeName as a flat structure of the given elements
// when it does not exist, then checks its layout. An existing type is never
// changed; a different layout is reported like TypeCheck does.
func (a API) EnsureType(ctx context.Context, typeName string, elements []string, elemTypes []uint32) error {
	var w Writer
	w.U32(TagOp, OpTypeCheck)
	w.U32(TagFlags, FlagCreate)
	w.String(TagTypeName, typeName)
	for i, e := range elements {
		w.String(TagName, e)
		w.U32(TagElemType, elemTypes[i])
	}
	start := time.Now()
	_, err := a.call(ctx, &w, 0)
	a.logCall("ensureType", start, err, "type", typeName, "elements", nameList(elements))
	if err != nil {
		return err
	}
	// A created type reaches the manager's type container shortly after
	// the answer; check until it is known.
	for {
		err = a.TypeCheck(ctx, typeName, elements, elemTypes)
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// HotlinkItems decodes a DP_CONNECT event: repeated (name, value) pairs.
func HotlinkItems(m Message) ([]string, []Value, error) {
	var names []string
	var values []Value
	for _, f := range m.Fields {
		switch f.Tag {
		case TagName:
			names = append(names, string(f.Data))
		case TagValue:
			v, err := DecodeValue(f.Data)
			if err != nil {
				return nil, nil, err
			}
			values = append(values, v)
		}
	}
	if len(names) != len(values) {
		return nil, nil, ErrMalformed
	}
	return names, values, nil
}

// QueryRows decodes a QUERY_CONNECT event: a table whose first row is the
// header, as in the WinCC OA dyn_dyn_anytype query result.
func QueryRows(m Message) ([][]Value, error) {
	var rows [][]Value
	for _, f := range m.Fields {
		if f.Tag != TagRow {
			continue
		}
		fields, err := Parse(f.Data)
		if err != nil {
			return nil, err
		}
		var row []Value
		for _, cf := range fields {
			if cf.Tag != TagValue {
				continue
			}
			v, err := DecodeValue(cf.Data)
			if err != nil {
				return nil, err
			}
			row = append(row, v)
		}
		rows = append(rows, row)
	}
	return rows, nil
}
