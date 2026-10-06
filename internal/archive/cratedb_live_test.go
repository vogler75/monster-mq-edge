package archive

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/stores"
	storecratedb "monstermq.io/edge/internal/stores/cratedb"
)

// TestCrateDBGroupLive starts an archive group with archiveType CRATEDB on the
// default CrateDB connection given by MONSTERMQ_TEST_CRATEDB_URL.
func TestCrateDBGroupLive(t *testing.T) {
	url := os.Getenv("MONSTERMQ_TEST_CRATEDB_URL")
	if url == "" {
		t.Skip("MONSTERMQ_TEST_CRATEDB_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cfg := &config.Config{CrateDB: config.CrateDBConfig{URL: url}}
	m := NewManager(cfg, &stores.Storage{}, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	group := fmt.Sprintf("mmqedgeTest%d", time.Now().UnixNano())
	table := ArchiveName(group)
	db, err := storecratedb.Open(ctx, url, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	defer db.Pool().Exec(context.Background(), "DROP TABLE IF EXISTS "+table)

	if err := m.startGroup(ctx, stores.ArchiveGroupConfig{
		Name: group, Enabled: true, TopicFilters: []string{"live/#"},
		LastValType: stores.MessageStoreMemory, ArchiveType: stores.ArchiveCrateDB,
		PayloadFormat: stores.PayloadJSON, BulkSize: 10, BulkTimeoutMs: 50,
	}); err != nil {
		t.Fatal(err)
	}
	g := m.Get(group)
	now := time.Now().UTC().Truncate(time.Millisecond)
	for i := 0; i < 3; i++ {
		m.Dispatch(stores.BrokerMessage{TopicName: "live/x", Time: now.Add(time.Duration(i) * time.Second),
			Payload: []byte(fmt.Sprintf(`{"v":%d}`, i)), ClientID: "c"})
	}
	m.Dispatch(stores.BrokerMessage{TopicName: "other/x", Time: now, Payload: []byte("1")})
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, _ = db.Pool().Exec(ctx, "REFRESH TABLE "+table)
		h, err := g.Archive().GetHistory(ctx, "#", nil, nil, 10)
		if err == nil && len(h) == 3 {
			if string(h[0].Payload) != `{"v":2}` || h[0].Topic != "live/x" {
				t.Fatalf("history %+v", h[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("history: %d rows, %v", len(h), err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	m.Stop()
}
