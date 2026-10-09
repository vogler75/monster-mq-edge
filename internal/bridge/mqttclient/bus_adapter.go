package mqttclient

import (
	"monstermq.io/edge/internal/pubsub"
	"monstermq.io/edge/internal/stores"
)

// BusAdapter wraps the in-process pubsub.Bus to satisfy LocalSubscriber.
//
// Bus messages that arrived over PeerLink from another broker (OriginNode
// set) are dropped unless BridgeOutbound is true: an outbound bridge would
// otherwise forward a peer's publishes too, which loops when its remote is
// that peer and duplicates when every node runs the bridge.
type BusAdapter struct {
	Bus            *pubsub.Bus
	BridgeOutbound bool // PeerLink.Receive.BridgeOutbound
}

func (a *BusAdapter) Subscribe(filters []string, buffer int) (int, <-chan LocalMessage) {
	var id int
	var raw <-chan stores.BrokerMessage
	if a.BridgeOutbound {
		id, raw = a.Bus.Subscribe(filters, buffer)
	} else {
		id, raw = a.Bus.SubscribeLocal(filters, buffer)
	}
	out := make(chan LocalMessage, buffer)
	go func() {
		defer close(out)
		for m := range raw {
			if !a.forwards(m) {
				continue
			}
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

func (a *BusAdapter) forwards(m stores.BrokerMessage) bool {
	return m.OriginNode == "" || a.BridgeOutbound
}

func (a *BusAdapter) Unsubscribe(id int) { a.Bus.Unsubscribe(id) }
