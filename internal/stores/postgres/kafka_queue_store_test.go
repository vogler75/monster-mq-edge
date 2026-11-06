package postgres

import (
	"context"
	"os"
	"testing"

	"monstermq.io/edge/internal/stores/storetest"
)

// Runs against a real database when EDGE_TEST_POSTGRES_DSN is set.
func TestKafkaQueueStore(t *testing.T) {
	dsn := os.Getenv("EDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("EDGE_TEST_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := NewKafkaQueueStore(db, "edge_test_queue")
	t.Cleanup(func() {
		_, _ = db.pool.Exec(ctx, "DROP TABLE IF EXISTS "+s.queueTable+", "+s.offsetTable)
	})
	_, _ = db.pool.Exec(ctx, "DROP TABLE IF EXISTS "+s.queueTable+", "+s.offsetTable)
	if err := s.EnsureTable(ctx); err != nil {
		t.Fatal(err)
	}
	storetest.CheckKafkaQueue(t, s)
}
