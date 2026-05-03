package memory

import (
	"context"
	"testing"
	"time"

	"monstermq.io/edge/internal/stores"
)

func TestMetricsStoreLatestHistoryPurgeAndBounds(t *testing.T) {
	ctx := context.Background()
	ms := NewMetricsStore(2)
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Second)
	t3 := t2.Add(time.Second)

	if err := ms.StoreMetrics(ctx, stores.MetricBroker, t1, "node-1", `{"messagesIn":1}`); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMetrics(ctx, stores.MetricBroker, t2, "node-1", `{"messagesIn":2}`); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMetrics(ctx, stores.MetricBroker, t3, "node-1", `{"messagesIn":3}`); err != nil {
		t.Fatal(err)
	}

	ts, payload, err := ms.GetLatest(ctx, stores.MetricBroker, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ts.Equal(t3) || payload != `{"messagesIn":3}` {
		t.Fatalf("latest = %s %s", ts, payload)
	}

	history, err := ms.GetHistory(ctx, stores.MetricBroker, "node-1", t1, t3, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || !history[0].Timestamp.Equal(t2) || !history[1].Timestamp.Equal(t3) {
		t.Fatalf("bounded history = %#v", history)
	}

	deleted, err := ms.PurgeOlderThan(ctx, t3)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d", deleted)
	}
	history, err = ms.GetHistory(ctx, stores.MetricBroker, "node-1", t1, t3, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || !history[0].Timestamp.Equal(t3) {
		t.Fatalf("purged history = %#v", history)
	}
}
