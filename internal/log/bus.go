package log

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Entry mirrors the GraphQL SystemLogEntry shape.
type Entry struct {
	Timestamp    time.Time
	Level        string
	Logger       string
	Message      string
	Thread       int64 // best-effort goroutine id (0 in Go since runtime IDs are private)
	Node         string
	SourceClass  string
	SourceMethod string
	Parameters   []string
	Exception    *Exception
}

type Exception struct {
	Class      string
	Message    string
	StackTrace string
}

// Bus distributes log entries to subscribed listeners and keeps a ring buffer
// of recent ones for the systemLogs query (history).
type Bus struct {
	mu        sync.RWMutex
	ring      []Entry
	ringHead  int
	ringSize  int
	subs      map[int]chan Entry
	nextSubID int
}

func NewBus(ringSize int) *Bus {
	if ringSize <= 0 {
		ringSize = 1000
	}
	return &Bus{
		ring:     make([]Entry, 0, ringSize),
		ringSize: ringSize,
		subs:     map[int]chan Entry{},
	}
}

// Publish records an entry in the ring and fans it out to subscribers. Drops
// to a slow subscriber rather than blocking the producer.
func (b *Bus) Publish(e Entry) {
	b.mu.Lock()
	if len(b.ring) < b.ringSize {
		b.ring = append(b.ring, e)
	} else {
		b.ring[b.ringHead] = e
		b.ringHead = (b.ringHead + 1) % b.ringSize
	}
	subs := make([]chan Entry, 0, len(b.subs))
	for _, ch := range b.subs {
		subs = append(subs, ch)
	}
	b.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- e:
		default:
		}
	}
}

func (b *Bus) Subscribe(buffer int) (id int, ch <-chan Entry) {
	if buffer <= 0 {
		buffer = 64
	}
	c := make(chan Entry, buffer)
	b.mu.Lock()
	b.nextSubID++
	id = b.nextSubID
	b.subs[id] = c
	b.mu.Unlock()
	return id, c
}

func (b *Bus) Unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subs[id]; ok {
		close(ch)
		delete(b.subs, id)
	}
}

// Snapshot returns a chronologically-ordered copy of the ring buffer.
func (b *Bus) Snapshot() []Entry {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Entry, 0, len(b.ring))
	if len(b.ring) < b.ringSize {
		out = append(out, b.ring...)
		return out
	}
	out = append(out, b.ring[b.ringHead:]...)
	out = append(out, b.ring[:b.ringHead]...)
	return out
}

// Handler is an slog.Handler that fans every record to the Bus while
// delegating writes to an inner handler (so logs still hit stderr / a file).
type Handler struct {
	bus    *Bus
	inner  slog.Handler
	nodeID string
	attrs  []slog.Attr
}

func NewHandler(bus *Bus, inner slog.Handler, nodeID string) *Handler {
	return &Handler{bus: bus, inner: inner, nodeID: nodeID}
}

func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	entry := Entry{
		Timestamp: r.Time,
		Level:     levelName(r.Level),
		Logger:    "monstermq",
		Message:   r.Message,
		Node:      h.nodeID,
	}
	params := make([]string, 0, len(h.attrs)+r.NumAttrs())
	visit := func(a slog.Attr) {
		a.Value = a.Value.Resolve()
		switch a.Key {
		case "logger":
			entry.Logger = a.Value.String()
		case "source_class", "sourceClass":
			entry.SourceClass = a.Value.String()
		case "source_method", "sourceMethod":
			entry.SourceMethod = a.Value.String()
		case "thread":
			entry.Thread = a.Value.Int64()
		case "err", "error":
			msg := attrValueString(a.Value)
			entry.Exception = &Exception{Class: "error", Message: msg}
			params = append(params, fmt.Sprintf("%s=%s", a.Key, msg))
		default:
			params = append(params, fmt.Sprintf("%s=%s", a.Key, attrValueString(a.Value)))
		}
	}
	for _, a := range h.attrs {
		visit(a)
	}
	r.Attrs(func(a slog.Attr) bool {
		visit(a)
		return true
	})
	entry.Parameters = params
	if entry.Message == "" && len(params) > 0 {
		entry.Message = strings.Join(params, " ")
	}
	h.bus.Publish(entry)
	return h.inner.Handle(ctx, r)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := &Handler{bus: h.bus, inner: h.inner.WithAttrs(attrs), nodeID: h.nodeID}
	next.attrs = append(next.attrs, h.attrs...)
	next.attrs = append(next.attrs, attrs...)
	return next
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{bus: h.bus, inner: h.inner.WithGroup(name), nodeID: h.nodeID, attrs: append([]slog.Attr(nil), h.attrs...)}
}

func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "SEVERE"
	case l >= slog.LevelWarn:
		return "WARNING"
	case l >= slog.LevelInfo:
		return "INFO"
	default:
		return "FINE"
	}
}

func attrValueString(v slog.Value) string {
	if v.Kind() == slog.KindAny {
		if err, ok := v.Any().(error); ok {
			return err.Error()
		}
	}
	return v.String()
}
