package mqttclient

import (
	"testing"
	"time"

	"monstermq.io/edge/internal/pubsub"
	"monstermq.io/edge/internal/stores"
)

func TestBusAdapterSkipsPeerReplicas(t *testing.T) {
	for _, outbound := range []bool{false, true} {
		bus := pubsub.NewBus()
		a := &BusAdapter{Bus: bus, BridgeOutbound: outbound}
		id, ch := a.Subscribe([]string{"#"}, 8)

		bus.Publish(stores.BrokerMessage{TopicName: "peer/x", Payload: []byte("replica"), OriginNode: "oa-a"})
		bus.Publish(stores.BrokerMessage{TopicName: "local/x", Payload: []byte("local")})

		var got []string
		timeout := time.After(time.Second)
	read:
		for {
			select {
			case m := <-ch:
				got = append(got, m.Topic)
				if m.Topic == "local/x" {
					break read
				}
			case <-timeout:
				t.Fatalf("BridgeOutbound=%v: local message not delivered, got %v", outbound, got)
			}
		}
		want := []string{"local/x"}
		if outbound {
			want = []string{"peer/x", "local/x"}
		}
		if len(got) != len(want) || got[0] != want[0] {
			t.Errorf("BridgeOutbound=%v: got %v, want %v", outbound, got, want)
		}
		a.Unsubscribe(id)
	}
}
