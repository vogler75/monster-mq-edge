package pubsub

import (
	"strings"
	"sync"
	"sync/atomic"

	"monstermq.io/edge/internal/stores"
)

// Bus is an in-process pub/sub used by GraphQL subscriptions to deliver
// MQTT-style topic events. Subscribers can express MQTT-style topic filters
// (with + and # wildcards).
type busSnapshot struct {
	exact    map[string][]*sub
	wildcard []*sub
}

type Bus struct {
	mu       sync.RWMutex
	next     int
	subs     map[int]*sub
	snap     atomic.Pointer[busSnapshot]
	count    atomic.Int32
	observer Observer
	interest InterestHook
}

// InterestHook gives bus subscribers the same side effects as an MQTT
// SUBSCRIBE (e.g. connecting WinCC OA datapoints). Calls happen outside the
// bus lock; the hook delivers current values with Deliver.
type InterestHook interface {
	Validate(filters []string) error
	Subscribed(id int, filters []string)
	Unsubscribed(id int, filters []string)
}

// Observer is told which filters bus subscribers hold (PeerLink interest routing). Calls happen
// outside the bus lock, except the replay in SetObserver.
type Observer interface {
	FiltersAdded(filters []string)
	FiltersRemoved(filters []string)
}

type sub struct {
	filters  []string
	local    bool
	ch       chan stores.BrokerMessage
	fn       func(stores.BrokerMessage)
	overflow chan struct{}
	once     sync.Once
}

func (s *sub) deliver(msg stores.BrokerMessage) {
	if s.fn != nil {
		s.fn(msg)
		return
	}
	select {
	case s.ch <- msg:
	default:
		// drop on slow subscriber to keep the broker hot path nonblocking
		s.once.Do(func() { close(s.overflow) })
	}
}

func NewBus() *Bus { return &Bus{subs: map[int]*sub{}} }

func (b *Bus) Subscribe(filters []string, buffer int) (id int, ch <-chan stores.BrokerMessage) {
	id, ch, _ = b.SubscribeWithOverflow(filters, buffer)
	return id, ch
}

// SubscribeWithOverflow reports a dropped event so streaming clients can close.
func (b *Bus) SubscribeWithOverflow(filters []string, buffer int) (id int, ch <-chan stores.BrokerMessage, overflow <-chan struct{}) {
	return b.subscribe(filters, buffer, false)
}

// SubscribeLocal is Subscribe for a subscriber that drops messages from other brokers, so its
// filters are not reported to the observer.
func (b *Bus) SubscribeLocal(filters []string, buffer int) (id int, ch <-chan stores.BrokerMessage) {
	id, ch, _ = b.subscribe(filters, buffer, true)
	return id, ch
}

func (b *Bus) SubscribeFunc(filters []string, fn func(stores.BrokerMessage)) int {
	b.mu.Lock()
	b.next++
	id := b.next
	s := &sub{filters: filters, fn: fn}
	b.subs[id] = s
	b.rebuildSnapshotLocked()
	o := b.observer
	ih := b.interest
	b.mu.Unlock()
	if o != nil && len(filters) > 0 {
		o.FiltersAdded(filters)
	}
	if ih != nil && len(filters) > 0 {
		ih.Subscribed(id, filters)
	}
	return id
}

func (b *Bus) rebuildSnapshotLocked() {
	b.count.Store(int32(len(b.subs)))
	if len(b.subs) == 0 {
		b.snap.Store(nil)
		return
	}
	snap := &busSnapshot{
		exact: make(map[string][]*sub),
	}
	for _, s := range b.subs {
		if len(s.filters) == 1 && !strings.ContainsAny(s.filters[0], "+#") {
			f := s.filters[0]
			snap.exact[f] = append(snap.exact[f], s)
		} else {
			snap.wildcard = append(snap.wildcard, s)
		}
	}
	b.snap.Store(snap)
}

func (b *Bus) subscribe(filters []string, buffer int, local bool) (id int, ch <-chan stores.BrokerMessage, overflow <-chan struct{}) {
	if buffer <= 0 {
		buffer = 16
	}
	c := make(chan stores.BrokerMessage, buffer)
	b.mu.Lock()
	b.next++
	id = b.next
	s := &sub{filters: filters, local: local, ch: c, overflow: make(chan struct{})}
	b.subs[id] = s
	b.rebuildSnapshotLocked()
	o := b.observer
	ih := b.interest
	b.mu.Unlock()
	if o != nil && !local && len(filters) > 0 {
		o.FiltersAdded(filters)
	}
	if ih != nil && len(filters) > 0 {
		ih.Subscribed(id, filters)
	}
	return id, c, s.overflow
}

// SetInterestHook installs h. Call it before subscribers exist.
func (b *Bus) SetInterestHook(h InterestHook) {
	b.mu.Lock()
	b.interest = h
	b.mu.Unlock()
}

// Validate reports an error when a filter would be rejected by an MQTT
// SUBSCRIBE (e.g. an invalid WinCC OA native filter).
func (b *Bus) Validate(filters []string) error {
	b.mu.RLock()
	ih := b.interest
	b.mu.RUnlock()
	if ih == nil {
		return nil
	}
	return ih.Validate(filters)
}

// Deliver sends msg to one subscriber only, like a retained message at
// subscribe time. It reports false when the subscriber is gone.
func (b *Bus) Deliver(id int, msg stores.BrokerMessage) bool {
	b.mu.RLock()
	s, ok := b.subs[id]
	b.mu.RUnlock()
	if !ok {
		return false
	}
	s.deliver(msg)
	return true
}

// Active reports whether subscriber id still exists.
func (b *Bus) Active(id int) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.subs[id]
	return ok
}

// SetObserver installs o and reports the filters of the current subscribers to it.
func (b *Bus) SetObserver(o Observer) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.observer = o
	if o == nil {
		return
	}
	for _, s := range b.subs {
		if !s.local && len(s.filters) > 0 {
			o.FiltersAdded(s.filters)
		}
	}
}

// HasSubscribers reports whether any subscription is active. Lock-free so the
// broker publish hot path can skip message construction when the bus is idle.
func (b *Bus) HasSubscribers() bool {
	return b.count.Load() > 0
}

func (b *Bus) Unsubscribe(id int) {
	b.mu.Lock()
	s, ok := b.subs[id]
	if ok {
		if s.ch != nil {
			close(s.ch)
		}
		delete(b.subs, id)
		b.rebuildSnapshotLocked()
	}
	o := b.observer
	ih := b.interest
	b.mu.Unlock()
	if ok && o != nil && !s.local && len(s.filters) > 0 {
		o.FiltersRemoved(s.filters)
	}
	if ok && ih != nil && len(s.filters) > 0 {
		ih.Unsubscribed(id, s.filters)
	}
}

func (b *Bus) Publish(msg stores.BrokerMessage) {
	snap := b.snap.Load()
	if snap == nil {
		return
	}
	if subs := snap.exact[msg.TopicName]; len(subs) > 0 {
		for _, s := range subs {
			s.deliver(msg)
		}
	}
	for _, s := range snap.wildcard {
		if anyMatch(s.filters, msg.TopicName) {
			s.deliver(msg)
		}
	}
}

func anyMatch(filters []string, topic string) bool {
	for _, f := range filters {
		if matchTopic(f, topic) {
			return true
		}
	}
	return false
}

func matchTopic(pattern, topic string) bool {
	// MQTT 4.7.2: a filter starting with a wildcard must not match $-topics ($SYS/...).
	if strings.HasPrefix(topic, "$") && (strings.HasPrefix(pattern, "#") || strings.HasPrefix(pattern, "+")) {
		return false
	}
	if pattern == "#" || pattern == topic {
		return true
	}
	pi, ti := 0, 0
	plen, tlen := len(pattern), len(topic)
	for pi <= plen {
		ps := pi
		for pi < plen && pattern[pi] != '/' {
			pi++
		}
		pseg := pattern[ps:pi]
		if pseg == "#" {
			return true
		}
		if ti > tlen {
			return false
		}
		ts := ti
		for ti < tlen && topic[ti] != '/' {
			ti++
		}
		tseg := topic[ts:ti]
		if pseg != "+" && pseg != tseg {
			return false
		}
		pi++
		ti++
	}
	return ti > tlen
}
