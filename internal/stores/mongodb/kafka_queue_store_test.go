package mongodb

import (
	"context"
	"os"
	"testing"

	"monstermq.io/edge/internal/stores/storetest"
)

// Runs against a real database when EDGE_TEST_MONGODB_URI is set
// (EDGE_TEST_MONGODB_DB names the database, default monstermq_test).
func TestKafkaQueueStore(t *testing.T) {
	uri := os.Getenv("EDGE_TEST_MONGODB_URI")
	if uri == "" {
		t.Skip("EDGE_TEST_MONGODB_URI not set")
	}
	name := os.Getenv("EDGE_TEST_MONGODB_DB")
	if name == "" {
		name = "monstermq_test"
	}
	ctx := context.Background()
	db, err := Open(ctx, uri, name)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := NewKafkaQueueStore(db, "edge_test_queue")
	cleanup := func() {
		_ = s.queue().Drop(ctx)
		_ = s.offsets().Drop(ctx)
		_, _ = db.db.Collection("counters").DeleteOne(ctx, map[string]any{"_id": s.queueName})
	}
	cleanup()
	t.Cleanup(cleanup)
	if err := s.EnsureTable(ctx); err != nil {
		t.Fatal(err)
	}
	storetest.CheckKafkaQueue(t, s)
}
