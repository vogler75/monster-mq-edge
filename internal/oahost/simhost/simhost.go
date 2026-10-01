// Package simhost is an in-process stand-in for the WinCC OA embedding
// manager. It executes the spec-winccoa-native.md operations against an
// in-memory datapoint model on a single "manager" goroutine, with the same
// queueing, deadline and batch rules as the C++ host. It exists to drive the
// broker through its real listeners in tests; it does not establish OA
// acceptance.
package simhost

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"monstermq.io/edge/internal/oahost"
)

// ElemStruct marks a structure node (not a value element).
const ElemStruct uint32 = 0

type Type struct {
	Name string
	// Elements maps element path ("" for the root, "a.b" for nested) to the
	// element kind (oahost.Kind as uint32) or ElemStruct.
	Elements map[string]uint32
}

type dp struct {
	typ    string
	values map[string]oahost.Value // key: element path + ":" + attr
}

type system struct {
	name      string
	available bool
	dps       map[string]*dp
}

type connection struct {
	names    []string // fully qualified read addresses
	noSource bool
}

type query struct {
	attrs  []string
	re     *regexp.Regexp
	typ    string
	system string
}

type request struct {
	id       uint64
	deadline time.Time
	msg      []byte
}

// Host implements oahost.Host.
type Host struct {
	client *oahost.Client

	queue       chan request
	stop        chan struct{}
	done        chan struct{}
	paused      atomic.Bool
	resume      chan struct{}
	TickBudget  int
	BatchLimit  int
	ManagerName string

	mu      sync.Mutex
	local   string
	types   map[string]*Type
	systems map[string]*system
	conns   map[uint64]*connection
	queries map[uint64]*query
	// ownWrites marks addresses written by this manager for NoSource.
	sets             atomic.Uint64
	orphans          atomic.Uint64
	expired          atomic.Uint64
	calls            map[uint32]*atomic.Uint64
	threadViolations atomic.Uint64
	typesCreated     atomic.Uint64
	managerGoroutine atomic.Int64
}

func New(localSystem string, queueCap int) *Host {
	if queueCap <= 0 {
		queueCap = 4096
	}
	h := &Host{
		queue:      make(chan request, queueCap),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
		resume:     make(chan struct{}, 1),
		TickBudget: 256,
		BatchLimit: 100,
		local:      localSystem,
		types:      map[string]*Type{},
		systems:    map[string]*system{},
		conns:      map[uint64]*connection{},
		queries:    map[uint64]*query{},
		calls:      map[uint32]*atomic.Uint64{},
	}
	for op := oahost.OpResolve; op <= oahost.OpTypeCheck; op++ {
		h.calls[op] = new(atomic.Uint64)
	}
	h.systems[localSystem] = &system{name: localSystem, available: true, dps: map[string]*dp{}}
	return h
}

// Attach binds the Go client and starts the manager goroutine.
func (h *Host) Attach(c *oahost.Client) {
	h.client = c
	go h.run()
}

func (h *Host) Submit(id uint64, deadline time.Time, msg []byte) error {
	select {
	case <-h.stop:
		return oahost.ErrStopped
	default:
	}
	select {
	case h.queue <- request{id: id, deadline: deadline, msg: append([]byte(nil), msg...)}:
		return nil
	default:
		return oahost.ErrOverload
	}
}

func (h *Host) Close() {
	select {
	case <-h.stop:
		return
	default:
	}
	close(h.stop)
	h.Resume()
	<-h.done
}

// Pause stops request processing, as if the OA manager thread were blocked.
func (h *Host) Pause() { h.paused.Store(true) }

func (h *Host) Resume() {
	h.paused.Store(false)
	select {
	case h.resume <- struct{}{}:
	default:
	}
}

func (h *Host) Expired() uint64        { return h.expired.Load() }
func (h *Host) Sets() uint64           { return h.sets.Load() }
func (h *Host) Calls(op uint32) uint64 { return h.calls[op].Load() }
func (h *Host) QueueLen() int          { return len(h.queue) }

// Connections returns the number of live dpConnect registrations.
func (h *Host) Connections() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}

// ConnectedNames returns every read address with a live dpConnect.
func (h *Host) ConnectedNames() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, c := range h.conns {
		out = append(out, c.names...)
	}
	sort.Strings(out)
	return out
}

func (h *Host) Queries() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.queries)
}

func (h *Host) AddType(t Type) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cp := Type{Name: t.Name, Elements: map[string]uint32{}}
	for k, v := range t.Elements {
		cp.Elements[k] = v
	}
	h.types[t.Name] = &cp
}

func (h *Host) AddSystem(name string, available bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.systems[name]; ok {
		s.available = available
		return
	}
	h.systems[name] = &system{name: name, available: available, dps: map[string]*dp{}}
}

// SetSystemAvailable changes a system's reachability and, like the C++ host,
// drops the registrations that depended on it and reports the change on
// StateRef.
func (h *Host) SetSystemAvailable(name string, available bool) {
	h.mu.Lock()
	if s, ok := h.systems[name]; ok {
		s.available = available
	}
	if !available {
		for ref, q := range h.queries {
			if q.system == name {
				delete(h.queries, ref)
			}
		}
		for ref, c := range h.conns {
			for _, n := range c.names {
				if sys, _, _ := splitAddress(n, h.local); sys == name {
					delete(h.conns, ref)
					break
				}
			}
		}
	}
	h.mu.Unlock()
	var w oahost.Writer
	w.String(oahost.TagSysName, name)
	w.Bool(oahost.TagExists, available)
	_ = h.client.Event(oahost.StateRef, w.Bytes())
}

func (h *Host) CreateDP(sys, name, typ string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.createLocked(sys, name, typ)
}

func (h *Host) createLocked(sys, name, typ string) error {
	s := h.systems[sys]
	if s == nil {
		return oahost.ErrUnavailable
	}
	t := h.types[typ]
	if t == nil {
		return fmt.Errorf("%w: type %s", oahost.ErrNotFound, typ)
	}
	if _, exists := s.dps[name]; exists {
		return fmt.Errorf("%w: datapoint %s exists", oahost.ErrInvalid, name)
	}
	d := &dp{typ: typ, values: map[string]oahost.Value{}}
	for el, k := range t.Elements {
		if k == ElemStruct {
			continue
		}
		d.values[el] = zero(oahost.Kind(k))
	}
	s.dps[name] = d
	return nil
}

// DeleteDP deletes a datapoint and reports it on StateRef like the C++
// host does when OA announces the identification change.
func (h *Host) DeleteDP(sys, name string) {
	h.mu.Lock()
	if s := h.systems[sys]; s != nil {
		delete(s.dps, name)
	}
	h.mu.Unlock()
	var w oahost.Writer
	w.String(oahost.TagName, sys+":"+name)
	w.Bool(oahost.TagExists, false)
	_ = h.client.Event(oahost.StateRef, w.Bytes())
}

// dpChangeEvent reports a created or deleted datapoint on StateRef.
func dpChangeEvent(sysDP string, exists bool) pendingEvent {
	var w oahost.Writer
	w.String(oahost.TagName, sysDP)
	w.Bool(oahost.TagExists, exists)
	return pendingEvent{ref: oahost.StateRef, data: w.Bytes()}
}

func zero(k oahost.Kind) oahost.Value {
	v := oahost.Value{Kind: k}
	if k == oahost.KindTime {
		v.Time = time.UnixMilli(0).UTC()
	}
	return v
}

// Set changes a value as if a driver or another manager wrote it.
func (h *Host) Set(address string, v oahost.Value) error {
	h.mu.Lock()
	events, err := h.setLocked(address, v, false)
	h.mu.Unlock()
	h.emit(events)
	return err
}

// Get reads the current value of an address.
func (h *Host) Get(address string) (oahost.Value, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, d, el, _, err := h.lookup(address)
	if err != nil {
		return oahost.Value{}, err
	}
	return d.values[el], nil
}

type pendingEvent struct {
	ref  uint64
	data []byte
}

func (h *Host) emit(evs []pendingEvent) {
	for _, e := range evs {
		_ = h.client.Event(e.ref, e.data)
	}
}

// lookup parses "Sys:DP.el:_config.._attr" into its parts.
func (h *Host) lookup(address string) (*system, *dp, string, string, error) {
	sysName, rest, attr := splitAddress(address, h.local)
	s := h.systems[sysName]
	if s == nil {
		return nil, nil, "", "", fmt.Errorf("%w: system %s", oahost.ErrNotFound, sysName)
	}
	if !s.available {
		return s, nil, "", "", fmt.Errorf("%w: system %s", oahost.ErrUnavailable, sysName)
	}
	dpName, el, hasDot := strings.Cut(rest, ".")
	if !hasDot {
		return s, nil, "", "", fmt.Errorf("%w: %s has no element separator", oahost.ErrInvalid, address)
	}
	d := s.dps[dpName]
	if d == nil {
		return s, nil, "", "", fmt.Errorf("%w: %s", oahost.ErrNotFound, address)
	}
	t := h.types[d.typ]
	if _, ok := t.Elements[el]; !ok {
		return s, d, "", "", fmt.Errorf("%w: element %s", oahost.ErrNotFound, address)
	}
	return s, d, el, attr, nil
}

func (h *Host) setLocked(address string, v oahost.Value, own bool) ([]pendingEvent, error) {
	s, d, el, attr, err := h.lookup(address)
	if err != nil {
		return nil, err
	}
	t := h.types[d.typ]
	k := t.Elements[el]
	if k == ElemStruct {
		return nil, fmt.Errorf("%w: %s is a structure node", oahost.ErrType, address)
	}
	if attr == attrLVSOff {
		if v.Kind != oahost.KindBool {
			return nil, fmt.Errorf("%w: %s expects bool", oahost.ErrType, address)
		}
		d.values[el+"#"+attrLVSOff] = v
		return nil, nil
	}
	if attr != "" && attr != "_original.._value" && attr != "_online.._value" {
		return nil, fmt.Errorf("%w: attribute %s not writable", oahost.ErrInvalid, attr)
	}
	if oahost.Kind(k) != v.Kind {
		return nil, fmt.Errorf("%w: element %s expects kind %d, got %d", oahost.ErrType, address, k, v.Kind)
	}
	d.values[el] = v
	d.values[el+"#stime"] = oahost.Value{Kind: oahost.KindTime, Time: time.Now().UTC()}
	_, dpe, _ := splitAddress(address, h.local)
	full := s.name + ":" + dpe
	var evs []pendingEvent
	for ref, c := range h.conns {
		if own && c.noSource {
			continue
		}
		for _, n := range c.names {
			if h.dpeOf(n) != full {
				continue
			}
			var w oahost.Writer
			w.String(oahost.TagName, n)
			w.Value(oahost.TagValue, h.readAttr(d, el, attrOf(n)))
			evs = append(evs, pendingEvent{ref: ref, data: w.Bytes()})
		}
	}
	for ref, q := range h.queries {
		if q.typ != "" && q.typ != d.typ {
			continue
		}
		if !q.re.MatchString(full) {
			continue
		}
		evs = append(evs, pendingEvent{ref: ref, data: h.queryTable([]string{full}, q, false)})
	}
	return evs, nil
}

// splitAddress splits "[Sys:]DP.el[:_config.._attr]" into system, DPE and
// attribute. A leading segment counts as the system only when a second
// colon follows or the remainder contains the element separator.
func splitAddress(address, local string) (sys, dpe, attr string) {
	sys = local
	rest := address
	if i := strings.Index(rest, ":"); i >= 0 && !strings.Contains(rest[:i], ".") {
		sys = rest[:i]
		rest = rest[i+1:]
	}
	if i := strings.Index(rest, ":"); i >= 0 {
		attr = rest[i+1:]
		rest = rest[:i]
	}
	return sys, rest, attr
}

func (h *Host) dpeOf(address string) string {
	sys, dpe, _ := splitAddress(address, h.local)
	return sys + ":" + dpe
}

func attrOf(address string) string {
	parts := strings.SplitN(address, ":", 3)
	if len(parts) == 3 {
		return parts[2]
	}
	return "_online.._value"
}

func goid() int64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	f := strings.Fields(strings.TrimPrefix(string(buf[:n]), "goroutine "))
	if len(f) == 0 {
		return 0
	}
	id, _ := strconv.ParseInt(f[0], 10, 64)
	return id
}

// attrLVSOff turns off the last value storage of an element.
const attrLVSOff = "_original.._last_value_storage_off"

func (h *Host) readAttr(d *dp, el, attr string) oahost.Value {
	switch attr {
	case attrLVSOff:
		if v, ok := d.values[el+"#"+attrLVSOff]; ok {
			return v
		}
		return oahost.Value{Kind: oahost.KindBool}
	case "_online.._stime":
		if v, ok := d.values[el+"#stime"]; ok {
			return v
		}
		return oahost.Value{Kind: oahost.KindTime, Time: time.UnixMilli(0).UTC()}
	case "_online.._status":
		return oahost.Value{Kind: oahost.KindBit32, Uint: 0x101}
	case "_online.._invalid":
		return oahost.Value{Kind: oahost.KindBool}
	}
	return d.values[el]
}

func (h *Host) run() {
	defer close(h.done)
	h.managerGoroutine.Store(goid())
	for {
		for h.paused.Load() {
			select {
			case <-h.stop:
				return
			case <-h.resume:
			case <-time.After(10 * time.Millisecond):
			}
		}
		select {
		case <-h.stop:
			return
		case r := <-h.queue:
			h.handle(r)
			for i := 1; i < h.TickBudget && !h.paused.Load(); i++ {
				select {
				case r := <-h.queue:
					h.handle(r)
				default:
					i = h.TickBudget
				}
			}
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (h *Host) handle(r request) {
	for h.paused.Load() {
		select {
		case <-h.stop:
			return
		case <-h.resume:
		case <-time.After(10 * time.Millisecond):
		}
	}
	if goid() != h.managerGoroutine.Load() {
		h.threadViolations.Add(1)
	}
	if time.Now().After(r.deadline) {
		h.expired.Add(1)
		_ = h.client.Complete(r.id, oahost.StatusTimeout, nil)
		return
	}
	m, err := oahost.ParseMessage(r.msg)
	if err != nil {
		h.complete(r.id, err, nil)
		return
	}
	op, _ := m.U32(oahost.TagOp)
	if c := h.calls[op]; c != nil {
		c.Add(1)
	}
	var w oahost.Writer
	var evs []pendingEvent
	h.mu.Lock()
	switch op {
	case oahost.OpSysInfo:
		w.String(oahost.TagSysName, h.local)
	case oahost.OpResolve:
		err = h.resolve(m.String(oahost.TagName), &w)
	case oahost.OpDpSet:
		evs, err = h.dpSet(m)
	case oahost.OpDpGet:
		err = h.dpGet(m, &w)
	case oahost.OpDpConnect:
		evs, err = h.dpConnect(m)
	case oahost.OpDpDisconnect:
		ref, _ := m.U64(oahost.TagRef)
		if _, ok := h.conns[ref]; !ok {
			err = oahost.ErrNotFound
		}
		delete(h.conns, ref)
	case oahost.OpQueryConnect:
		evs, err = h.queryConnect(m)
	case oahost.OpQueryDisconnect:
		ref, _ := m.U64(oahost.TagRef)
		if _, ok := h.queries[ref]; !ok {
			err = oahost.ErrNotFound
		}
		delete(h.queries, ref)
	case oahost.OpDpNames:
		err = h.dpNames(m, &w)
	case oahost.OpDpCreate:
		name := m.String(oahost.TagName)
		sys, dpn := splitSys(name, h.local)
		if strings.Contains(dpn, ".") {
			err = fmt.Errorf("%w: dpCreate takes a DP name without element or dot: %q", oahost.ErrInvalid, name)
		} else if err = h.createLocked(sys, dpn, m.String(oahost.TagTypeName)); err == nil {
			evs = append(evs, dpChangeEvent(sys+":"+dpn, true))
		}
	case oahost.OpDpDelete:
		name := m.String(oahost.TagName)
		sys, dpn := splitSys(name, h.local)
		if strings.Contains(dpn, ".") {
			err = fmt.Errorf("%w: dpDelete takes a DP name without element or dot: %q", oahost.ErrInvalid, name)
		} else if s := h.systems[sys]; s == nil || s.dps[dpn] == nil {
			err = oahost.ErrNotFound
		} else {
			delete(s.dps, dpn)
			evs = append(evs, dpChangeEvent(sys+":"+dpn, false))
		}
	case oahost.OpTypeCheck:
		err = h.typeCheck(m)
	default:
		err = fmt.Errorf("%w: op %d", oahost.ErrInvalid, op)
	}
	h.mu.Unlock()
	if cerr := h.complete(r.id, err, &w); cerr != nil && err == nil {
		// Nobody waits for this registration any more (timeout or
		// cancellation): release it instead of leaking it (spec 3).
		if op == oahost.OpDpConnect || op == oahost.OpQueryConnect {
			ref, _ := m.U64(oahost.TagRef)
			h.mu.Lock()
			delete(h.conns, ref)
			delete(h.queries, ref)
			h.mu.Unlock()
			h.orphans.Add(1)
			return
		}
	}
	h.emit(evs)
}

func (h *Host) complete(id uint64, err error, w *oahost.Writer) error {
	if err != nil {
		var ew oahost.Writer
		ew.String(oahost.TagError, err.Error())
		return h.client.Complete(id, oahost.ErrorStatus(err), ew.Bytes())
	}
	var data []byte
	if w != nil {
		data = w.Bytes()
	}
	return h.client.Complete(id, oahost.StatusOK, data)
}

// Orphans counts registrations released because their requester was gone.
func (h *Host) Orphans() uint64 { return h.orphans.Load() }

func splitSys(name, local string) (string, string) {
	if i := strings.Index(name, ":"); i >= 0 {
		return name[:i], name[i+1:]
	}
	return local, name
}

func (h *Host) resolve(name string, w *oahost.Writer) error {
	s, d, el, _, err := h.lookup(name)
	if err != nil {
		if errors.Is(err, oahost.ErrNotFound) && s != nil {
			w.Bool(oahost.TagExists, false)
			w.String(oahost.TagSysName, s.name)
			return nil
		}
		if errors.Is(err, oahost.ErrNotFound) {
			return oahost.ErrUnavailable
		}
		return err
	}
	w.Bool(oahost.TagExists, true)
	w.String(oahost.TagName, name)
	w.String(oahost.TagTypeName, d.typ)
	w.U32(oahost.TagElemType, h.types[d.typ].Elements[el])
	w.String(oahost.TagSysName, s.name)
	return nil
}

func (h *Host) dpSet(m oahost.Message) ([]pendingEvent, error) {
	names := m.All(oahost.TagName)
	vals := m.All(oahost.TagValue)
	if len(names) == 0 || len(names) != len(vals) {
		return nil, oahost.ErrInvalid
	}
	// Validate everything first so a grouped set is all-or-nothing.
	decoded := make([]oahost.Value, len(vals))
	for i := range names {
		v, err := oahost.DecodeValue(vals[i])
		if err != nil {
			return nil, err
		}
		_, d, el, _, err := h.lookup(string(names[i]))
		if err != nil {
			return nil, err
		}
		k := h.types[d.typ].Elements[el]
		if attrOf(string(names[i])) == attrLVSOff {
			k = uint32(oahost.KindBool)
		}
		if k == ElemStruct || oahost.Kind(k) != v.Kind {
			return nil, fmt.Errorf("%w: %s", oahost.ErrType, names[i])
		}
		decoded[i] = v
	}
	var all []pendingEvent
	for i := range names {
		evs, err := h.setLocked(string(names[i]), decoded[i], true)
		if err != nil {
			return all, err
		}
		h.sets.Add(1)
		all = append(all, evs...)
	}
	return all, nil
}

func (h *Host) dpGet(m oahost.Message, w *oahost.Writer) error {
	for _, n := range m.All(oahost.TagName) {
		_, d, el, attr, err := h.lookup(string(n))
		if err != nil {
			return err
		}
		w.Value(oahost.TagValue, h.readAttr(d, el, attr))
	}
	return nil
}

func (h *Host) dpConnect(m oahost.Message) ([]pendingEvent, error) {
	ref, _ := m.U64(oahost.TagRef)
	flags, _ := m.U32(oahost.TagFlags)
	names := m.All(oahost.TagName)
	if len(names) > h.BatchLimit {
		return nil, fmt.Errorf("%w: %d names exceed maxConnectMessageSize %d", oahost.ErrOA, len(names), h.BatchLimit)
	}
	c := &connection{noSource: flags&oahost.FlagNoSource != 0}
	var w oahost.Writer
	w.U32(oahost.TagFlags, oahost.FlagAnswer)
	for _, raw := range names {
		n := string(raw)
		_, d, el, attr, err := h.lookup(n)
		if err != nil {
			return nil, err
		}
		c.names = append(c.names, n)
		w.String(oahost.TagName, n)
		w.Value(oahost.TagValue, h.readAttr(d, el, attr))
	}
	h.conns[ref] = c
	if flags&oahost.FlagAnswer == 0 {
		return nil, nil
	}
	return []pendingEvent{{ref: ref, data: w.Bytes()}}, nil
}

var (
	queryRe = regexp.MustCompile(`(?i)^\s*SELECT\s+(.+?)\s+FROM\s+'([^']+)'(?:\s+WHERE\s+_DPT\s*=\s*"([^"]+)")?(?:\s+REMOTE\s+'([^']+)')?\s*$`)
	attrRe  = regexp.MustCompile(`'([^']+)'`)
)

func (h *Host) queryConnect(m oahost.Message) ([]pendingEvent, error) {
	ref, _ := m.U64(oahost.TagRef)
	flags, _ := m.U32(oahost.TagFlags)
	sub := queryRe.FindStringSubmatch(m.String(oahost.TagQuery))
	if sub == nil {
		return nil, fmt.Errorf("%w: unsupported query", oahost.ErrOA)
	}
	pattern := sub[2]
	sysName := h.local
	if sub[4] != "" {
		sysName = sub[4]
	} else if i := strings.Index(pattern, ":"); i >= 0 && !strings.Contains(pattern[:i], ".") {
		sysName, pattern = pattern[:i], pattern[i+1:]
	}
	s := h.systems[sysName]
	if s == nil || !s.available {
		return nil, fmt.Errorf("%w: system %s", oahost.ErrUnavailable, sysName)
	}
	re, err := oaPatternRe(sysName, pattern)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", oahost.ErrOA, err)
	}
	q := &query{re: re, typ: sub[3], system: sysName}
	for _, a := range attrRe.FindAllStringSubmatch(sub[1], -1) {
		q.attrs = append(q.attrs, a[1])
	}
	if len(q.attrs) == 0 {
		return nil, fmt.Errorf("%w: no attributes selected", oahost.ErrOA)
	}
	h.queries[ref] = q
	if flags&oahost.FlagAnswer == 0 {
		return nil, nil
	}
	var matches []string
	for name, d := range s.dps {
		if q.typ != "" && q.typ != d.typ {
			continue
		}
		for el, k := range h.types[d.typ].Elements {
			if k == ElemStruct {
				continue
			}
			full := s.name + ":" + name + "." + el
			if re.MatchString(full) {
				matches = append(matches, full)
			}
		}
	}
	sort.Strings(matches)
	return []pendingEvent{{ref: ref, data: h.queryTable(matches, q, true)}}, nil
}

// oaPatternRe compiles a dpQuery FROM pattern with WinCC OA semantics: '*'
// matches within one name level, "X.**" matches X's elements at any depth
// (the root of a scalar DP included), "{a,b}" lists alternatives. A DP
// root is named "DP." and an element "DP.a.b".
func oaPatternRe(system, pattern string) (*regexp.Regexp, error) {
	alts := []string{pattern}
	if strings.HasPrefix(pattern, "{") && strings.HasSuffix(pattern, "}") {
		alts = strings.Split(pattern[1:len(pattern)-1], ",")
	}
	var parts []string
	for _, a := range alts {
		if !strings.Contains(a, ".") {
			a += "."
		}
		var b strings.Builder
		for i := 0; i < len(a); i++ {
			switch {
			case strings.HasPrefix(a[i:], ".**"):
				b.WriteString(`\..*`)
				i += 2
			case a[i] == '*':
				b.WriteString(`[^.]*`)
			case a[i] == '?':
				b.WriteString(`[^.]`)
			default:
				b.WriteString(regexp.QuoteMeta(string(a[i])))
			}
		}
		parts = append(parts, b.String())
	}
	return regexp.Compile("^" + regexp.QuoteMeta(system) + ":(" + strings.Join(parts, "|") + ")$")
}


// queryTable renders a dyn_dyn_anytype-like table: header row ["", ":attr"]
// followed by [dpe, value] rows.
func (h *Host) queryTable(dpes []string, q *query, answer bool) []byte {
	var w oahost.Writer
	if answer {
		w.U32(oahost.TagFlags, oahost.FlagAnswer)
	}
	var hdr oahost.Writer
	hdr.Value(oahost.TagValue, oahost.Value{Kind: oahost.KindString, Str: ""})
	for _, a := range q.attrs {
		hdr.Value(oahost.TagValue, oahost.Value{Kind: oahost.KindString, Str: ":" + a})
	}
	w.Raw(oahost.TagRow, hdr.Bytes())
	for _, full := range dpes {
		_, d, el, _, err := h.lookup(full)
		if err != nil {
			continue
		}
		var row oahost.Writer
		name := full
		row.Value(oahost.TagValue, oahost.Value{Kind: oahost.KindString, Str: name})
		for _, a := range q.attrs {
			row.Value(oahost.TagValue, h.readAttr(d, el, a))
		}
		w.Raw(oahost.TagRow, row.Bytes())
	}
	return w.Bytes()
}

func (h *Host) dpNames(m oahost.Message, w *oahost.Writer) error {
	pattern := m.String(oahost.TagName)
	typ := m.String(oahost.TagTypeName)
	sysName, p := splitSys(pattern, h.local)
	s := h.systems[sysName]
	if s == nil || !s.available {
		return oahost.ErrUnavailable
	}
	var out []string
	for name, d := range s.dps {
		if typ != "" && d.typ != typ {
			continue
		}
		if ok, _ := path.Match(p, name); ok {
			out = append(out, s.name+":"+name)
		}
	}
	sort.Strings(out)
	for _, n := range out {
		w.String(oahost.TagName, n)
	}
	return nil
}

func (h *Host) typeCheck(m oahost.Message) error {
	name := m.String(oahost.TagTypeName)
	names := m.All(oahost.TagName)
	var kinds []uint32
	for _, f := range m.Fields {
		if f.Tag == oahost.TagElemType && len(f.Data) == 4 {
			kinds = append(kinds, uint32(f.Data[0])|uint32(f.Data[1])<<8|uint32(f.Data[2])<<16|uint32(f.Data[3])<<24)
		}
	}
	t := h.types[name]
	if flags, _ := m.U32(oahost.TagFlags); t == nil && flags&oahost.FlagCreate != 0 {
		// Like the manager: create a flat structure, answer without checking.
		nt := &Type{Name: name, Elements: map[string]uint32{"": ElemStruct}}
		for i, n := range names {
			if i >= len(kinds) {
				return oahost.ErrInvalid
			}
			nt.Elements[string(n)] = kinds[i]
		}
		h.types[name] = nt
		h.typesCreated.Add(1)
		return nil
	}
	if t == nil {
		return oahost.ErrNotFound
	}
	for i, n := range names {
		k, ok := t.Elements[string(n)]
		if !ok {
			return fmt.Errorf("%w: element %s", oahost.ErrNotFound, n)
		}
		if i < len(kinds) && kinds[i] != k {
			return fmt.Errorf("%w: element %s has type %d, want %d", oahost.ErrType, n, k, kinds[i])
		}
	}
	return nil
}

// TypesCreated counts datapoint types created through a type check.
func (h *Host) TypesCreated() uint64 { return h.typesCreated.Load() }

// ThreadViolations counts operations executed off the manager goroutine.
func (h *Host) ThreadViolations() uint64 { return h.threadViolations.Load() }
