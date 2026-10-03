package resolvers

import (
	"testing"

	"monstermq.io/edge/internal/metrics"
)

func TestSnapshotToBrokerMetricsFillsMessageBus(t *testing.T) {
	bm := snapshotToBrokerMetrics(metrics.BrokerSnapshot{MessagesIn: 1, MessageBusIn: 3, MessageBusOut: 4})
	if bm.MessageBusIn != 3 || bm.MessageBusOut != 4 || bm.MessagesIn != 1 {
		t.Fatalf("BrokerMetrics = %+v, want messageBusIn 3, messageBusOut 4, messagesIn 1", bm)
	}
}
