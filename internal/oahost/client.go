package oahost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Status codes shared with the C ABI (spec section 3).
const (
	StatusOK           int32 = 0
	StatusInvalid      int32 = -1
	StatusState        int32 = -2
	StatusNotFound     int32 = -3
	StatusUnauthorized int32 = -4
	StatusType         int32 = -5
	StatusTimeout      int32 = -6
	StatusOverload     int32 = -7
	StatusOA           int32 = -8
	StatusPersist      int32 = -9
	StatusABI          int32 = -10
	StatusTooLarge     int32 = -11
	StatusUnavailable  int32 = -12
)

var (
	ErrInvalid      = errors.New("oahost: invalid argument")
	ErrState        = errors.New("oahost: invalid state")
	ErrNotFound     = errors.New("oahost: not found")
	ErrUnauthorized = errors.New("oahost: unauthorized")
	ErrType         = errors.New("oahost: unsupported type")
	ErrTimeout      = errors.New("oahost: timeout")
	ErrOverload     = errors.New("oahost: overload")
	ErrOA           = errors.New("oahost: WinCC OA error")
	ErrPersist      = errors.New("oahost: persistence failure")
	ErrTooLarge     = errors.New("oahost: too large")
	ErrUnavailable  = errors.New("oahost: unavailable")
	ErrStopped      = errors.New("oahost: client stopped")
)

func StatusError(status int32) error {
	switch status {
	case StatusOK:
		return nil
	case StatusInvalid, StatusABI:
		return ErrInvalid
	case StatusState:
		return ErrState
	case StatusNotFound:
		return ErrNotFound
	case StatusUnauthorized:
		return ErrUnauthorized
	case StatusType:
		return ErrType
	case StatusTimeout:
		return ErrTimeout
	case StatusOverload:
		return ErrOverload
	case StatusOA:
		return ErrOA
	case StatusPersist:
		return ErrPersist
	case StatusTooLarge:
		return ErrTooLarge
	case StatusUnavailable:
		return ErrUnavailable
	}
	return fmt.Errorf("%w: status %d", ErrOA, status)
}

func ErrorStatus(err error) int32 {
	switch {
	case err == nil:
		return StatusOK
	case errors.Is(err, ErrInvalid):
		return StatusInvalid
	case errors.Is(err, ErrState), errors.Is(err, ErrStopped):
		return StatusState
	case errors.Is(err, ErrNotFound):
		return StatusNotFound
	case errors.Is(err, ErrUnauthorized):
		return StatusUnauthorized
	case errors.Is(err, ErrType):
		return StatusType
	case errors.Is(err, ErrTimeout):
		return StatusTimeout
	case errors.Is(err, ErrOverload):
		return StatusOverload
	case errors.Is(err, ErrPersist):
		return StatusPersist
	case errors.Is(err, ErrTooLarge):
		return StatusTooLarge
	case errors.Is(err, ErrUnavailable):
		return StatusUnavailable
	}
	return StatusOA
}

// Host is implemented by the embedding process. Submit is called from
// arbitrary goroutines; it must copy msg and return without waiting for the
// WinCC OA manager thread. It returns ErrOverload when its queue is full.
type Host interface {
	Submit(id uint64, deadline time.Time, msg []byte) error
}

// Limits mirrors spec section 7.
type Limits struct {
	MaxPending     int
	EventQueue     int // total event capacity across workers
	DefaultTimeout time.Duration
	// EventWorkers process events in parallel; one reference is always
	// handled by the same worker, so its events stay in order.
	EventWorkers int
}

func DefaultLimits() Limits {
	return Limits{MaxPending: 4096, EventQueue: 16384, DefaultTimeout: 5 * time.Second, EventWorkers: 4}
}

type result struct {
	status int32
	msg    Message
}

type call struct {
	ch       chan result
	deadline time.Time
}

type event struct {
	ref  uint64
	data []byte
}

// EventHandler receives hotlink data for a reference. It runs on the client's
// single event goroutine and may call back into the Client, but must not wait
// on a completion that depends on further events being processed.
type EventHandler func(Message)

type Stats struct {
	Submitted        uint64
	Completed        uint64
	TimedOut         uint64
	Overloaded       uint64
	LateCompletions  uint64
	EventsDelivered  uint64
	EventsDropped    uint64
	EventsUnrouted   uint64
	PendingHighWater uint64
}

// Client tracks requests to the host and routes completions and events.
type Client struct {
	host   Host
	limits Limits

	mu         sync.Mutex
	nextID     uint64
	nextRef    uint64
	pending    map[uint64]*call
	handlers   map[uint64]EventHandler
	watchers   map[uint64]SystemWatcher
	dpWatchers map[uint64]DPWatcher
	nextWatch  uint64
	closed     bool

	events []chan event
	done   chan struct{}
	wg     sync.WaitGroup

	submitted, completed, timedOut, overloaded, late atomic.Uint64
	delivered, dropped, unrouted, highWater          atomic.Uint64

	logger atomic.Pointer[slog.Logger]
}

func NewClient(host Host, limits Limits) *Client {
	if limits.MaxPending <= 0 || limits.EventQueue <= 0 || limits.DefaultTimeout <= 0 || limits.EventWorkers <= 0 {
		d := DefaultLimits()
		if limits.EventWorkers <= 0 {
			limits.EventWorkers = d.EventWorkers
		}
		if limits.MaxPending <= 0 {
			limits.MaxPending = d.MaxPending
		}
		if limits.EventQueue <= 0 {
			limits.EventQueue = d.EventQueue
		}
		if limits.DefaultTimeout <= 0 {
			limits.DefaultTimeout = d.DefaultTimeout
		}
	}
	c := &Client{
		host:       host,
		limits:     limits,
		pending:    map[uint64]*call{},
		handlers:   map[uint64]EventHandler{},
		watchers:   map[uint64]SystemWatcher{},
		dpWatchers: map[uint64]DPWatcher{},
		done:       make(chan struct{}),
	}
	per := limits.EventQueue / limits.EventWorkers
	if per < 1 {
		per = 1
	}
	for i := 0; i < limits.EventWorkers; i++ {
		ch := make(chan event, per)
		c.events = append(c.events, ch)
		c.wg.Add(1)
		go c.eventLoop(ch)
	}
	return c
}

func (c *Client) Limits() Limits { return c.limits }

// SetLogger sets the logger for the per-call DEBUG lines of API.
func (c *Client) SetLogger(l *slog.Logger) { c.logger.Store(l) }

// Call submits msg and waits for its completion, the context, or the
// deadline, whichever comes first. The request is never retried.
func (c *Client) Call(ctx context.Context, msg []byte, timeout time.Duration) (Message, error) {
	if len(msg) > MaxMessage {
		return Message{}, ErrTooLarge
	}
	if timeout <= 0 {
		timeout = c.limits.DefaultTimeout
	}
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return Message{}, ErrStopped
	}
	if len(c.pending) >= c.limits.MaxPending {
		c.mu.Unlock()
		c.overloaded.Add(1)
		return Message{}, ErrOverload
	}
	c.nextID++
	id := c.nextID
	cl := &call{ch: make(chan result, 1), deadline: deadline}
	c.pending[id] = cl
	if n := uint64(len(c.pending)); n > c.highWater.Load() {
		c.highWater.Store(n)
	}
	c.mu.Unlock()

	if err := c.host.Submit(id, deadline, msg); err != nil {
		c.forget(id)
		if errors.Is(err, ErrOverload) {
			c.overloaded.Add(1)
		}
		return Message{}, err
	}
	c.submitted.Add(1)

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case r := <-cl.ch:
		if err := StatusError(r.status); err != nil {
			if text := r.msg.String(TagError); text != "" {
				return r.msg, fmt.Errorf("%w: %s", err, text)
			}
			return r.msg, err
		}
		return r.msg, nil
	case <-timer.C:
		c.forget(id)
		c.timedOut.Add(1)
		return Message{}, ErrTimeout
	case <-ctx.Done():
		c.forget(id)
		return Message{}, ctx.Err()
	case <-c.done:
		return Message{}, ErrStopped
	}
}

func (c *Client) forget(id uint64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// Complete delivers a host completion. It never blocks. Unknown, duplicate
// and late completions are counted and dropped.
func (c *Client) Complete(id uint64, status int32, data []byte) error {
	c.mu.Lock()
	cl, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if !ok {
		c.late.Add(1)
		return ErrNotFound
	}
	msg, err := ParseMessage(append([]byte(nil), data...))
	if err != nil {
		cl.ch <- result{status: StatusInvalid}
		c.completed.Add(1)
		return ErrInvalid
	}
	cl.ch <- result{status: status, msg: msg}
	c.completed.Add(1)
	return nil
}

// NewRef allocates a subscription reference and registers its handler.
func (c *Client) NewRef(h EventHandler) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextRef++
	ref := c.nextRef
	c.handlers[ref] = h
	return ref
}

func (c *Client) DropRef(ref uint64) {
	c.mu.Lock()
	delete(c.handlers, ref)
	c.mu.Unlock()
}

// Event enqueues hotlink data for ref. It never blocks; a full queue drops
// the event and reports ErrOverload so the host can count it too.
func (c *Client) Event(ref uint64, data []byte) error {
	if len(data) > MaxMessage {
		c.dropped.Add(1)
		return ErrTooLarge
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrStopped
	}
	select {
	case c.events[ref%uint64(len(c.events))] <- event{ref: ref, data: append([]byte(nil), data...)}:
		return nil
	default:
		c.dropped.Add(1)
		return ErrOverload
	}
}

func (c *Client) eventLoop(events chan event) {
	defer c.wg.Done()
	for {
		select {
		case <-c.done:
			return
		case ev := <-events:
			if ev.ref == StateRef {
				c.deliverState(ev.data)
				continue
			}
			c.mu.Lock()
			h := c.handlers[ev.ref]
			c.mu.Unlock()
			if h == nil {
				c.unrouted.Add(1)
				continue
			}
			msg, err := ParseMessage(ev.data)
			if err != nil {
				c.unrouted.Add(1)
				continue
			}
			c.deliver(h, msg)
		}
	}
}

func (c *Client) deliver(h EventHandler, msg Message) {
	defer func() {
		if r := recover(); r != nil {
			c.unrouted.Add(1)
		}
	}()
	h(msg)
	c.delivered.Add(1)
}

// StateRef is the reserved event reference for host state changes: a
// message with TagSysName and TagExists (system available) per affected
// system. The local system name reports the Event/Data connection itself.
const StateRef uint64 = 0

// SystemWatcher is told when a WinCC OA system becomes available or
// unavailable. It runs on the event goroutine.
type SystemWatcher func(system string, available bool)

// WatchSystems registers w and returns a function that removes it.
func (c *Client) WatchSystems(w SystemWatcher) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextWatch++
	id := c.nextWatch
	c.watchers[id] = w
	return func() {
		c.mu.Lock()
		delete(c.watchers, id)
		c.mu.Unlock()
	}
}

func (c *Client) deliverState(data []byte) {
	m, err := ParseMessage(data)
	if err != nil {
		c.unrouted.Add(1)
		return
	}
	c.mu.Lock()
	ws := make([]SystemWatcher, 0, len(c.watchers))
	for _, w := range c.watchers {
		ws = append(ws, w)
	}
	dws := make([]DPWatcher, 0, len(c.dpWatchers))
	for _, w := range c.dpWatchers {
		dws = append(dws, w)
	}
	c.mu.Unlock()
	// Fields come in pairs: TagSysName or TagName, then TagExists.
	var sys, dp string
	for _, f := range m.Fields {
		switch f.Tag {
		case TagSysName:
			sys, dp = string(f.Data), ""
		case TagName:
			sys, dp = "", string(f.Data)
		case TagExists:
			up := len(f.Data) == 1 && f.Data[0] != 0
			switch {
			case sys != "":
				for _, w := range ws {
					safeCall(func() { w(sys, up) })
				}
			case dp != "" && !up:
				for _, w := range dws {
					safeCall(func() { w(dp) })
				}
			}
			sys, dp = "", ""
		}
	}
	c.delivered.Add(1)
}

func safeCall(f func()) {
	defer func() { _ = recover() }()
	f()
}

// DPWatcher is told that a datapoint ("Sys:DP") was deleted or its type
// changed, so cached resolutions and values of it are stale.
type DPWatcher func(sysDP string)

// WatchDatapoints registers w and returns a function that removes it.
func (c *Client) WatchDatapoints(w DPWatcher) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextWatch++
	id := c.nextWatch
	c.dpWatchers[id] = w
	return func() {
		c.mu.Lock()
		delete(c.dpWatchers, id)
		c.mu.Unlock()
	}
}

// Close fails every pending call with ErrStopped and stops event delivery.
func (c *Client) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.pending = map[uint64]*call{}
	c.handlers = map[uint64]EventHandler{}
	c.mu.Unlock()
	close(c.done)
	c.wg.Wait()
}

func (c *Client) Stats() Stats {
	return Stats{
		Submitted:        c.submitted.Load(),
		Completed:        c.completed.Load(),
		TimedOut:         c.timedOut.Load(),
		Overloaded:       c.overloaded.Load(),
		LateCompletions:  c.late.Load(),
		EventsDelivered:  c.delivered.Load(),
		EventsDropped:    c.dropped.Load(),
		EventsUnrouted:   c.unrouted.Load(),
		PendingHighWater: c.highWater.Load(),
	}
}

// Refs reports the number of registered event routes.
func (c *Client) Refs() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.handlers)
}

func (c *Client) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending)
}
