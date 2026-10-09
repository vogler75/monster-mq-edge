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
type Bus struct {
	mu       sync.RWMutex
	next     int
	subs     map[int]*sub
	count    atomic.Int32
	observer Observer
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
	overflow chan struct{}
	once     sync.Once
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
	b.count.Store(int32(len(b.subs)))
	o := b.observer
	b.mu.Unlock()
	if o != nil && !local && len(filters) > 0 {
		o.FiltersAdded(filters)
	}
	return id, c, s.overflow
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
		close(s.ch)
		delete(b.subs, id)
		b.count.Store(int32(len(b.subs)))
	}
	o := b.observer
	b.mu.Unlock()
	if ok && o != nil && !s.local && len(s.filters) > 0 {
		o.FiltersRemoved(s.filters)
	}
}

func (b *Bus) Publish(msg stores.BrokerMessage) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, s := range b.subs {
		if anyMatch(s.filters, msg.TopicName) {
			select {
			case s.ch <- msg:
			default:
				// drop on slow subscriber to keep the broker hot path nonblocking
				s.once.Do(func() { close(s.overflow) })
			}
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
	pp := strings.Split(pattern, "/")
	tt := strings.Split(topic, "/")
	for i, p := range pp {
		if p == "#" {
			return true
		}
		if i >= len(tt) {
			return false
		}
		if p == "+" {
			continue
		}
		if p != tt[i] {
			return false
		}
	}
	return len(pp) == len(tt)
}
