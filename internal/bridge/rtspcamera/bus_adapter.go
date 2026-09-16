package rtspcamera

import (
	"monstermq.io/edge/internal/pubsub"
)

// BusAdapter wraps the in-process pubsub.Bus to satisfy LocalSubscriber.
type BusAdapter struct {
	Bus *pubsub.Bus
}

func (a *BusAdapter) Subscribe(filters []string, buffer int) (int, <-chan LocalMessage) {
	if a == nil || a.Bus == nil {
		ch := make(chan LocalMessage)
		close(ch)
		return 0, ch
	}
	id, raw := a.Bus.Subscribe(filters, buffer)
	out := make(chan LocalMessage, buffer)
	go func() {
		defer close(out)
		for m := range raw {
			out <- LocalMessage{
				Topic:   m.TopicName,
				Payload: m.Payload,
				QoS:     m.QoS,
				Retain:  m.IsRetain,
			}
		}
	}()
	return id, out
}

func (a *BusAdapter) Unsubscribe(id int) {
	if a != nil && a.Bus != nil {
		a.Bus.Unsubscribe(id)
	}
}
