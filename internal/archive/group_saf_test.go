package archive

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"monstermq.io/edge/internal/stores"
)

type mockArchiveStore struct {
	mu      sync.Mutex
	fail    bool
	history []stores.BrokerMessage
}

func (m *mockArchiveStore) Name() string                     { return "mock" }
func (m *mockArchiveStore) Type() stores.MessageArchiveType { return stores.ArchiveNone }
func (m *mockArchiveStore) EnsureTable(ctx context.Context) error { return nil }
func (m *mockArchiveStore) TableExists(ctx context.Context) (bool, error) {
	return true, nil
}
func (m *mockArchiveStore) AddHistory(ctx context.Context, msgs []stores.BrokerMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return errors.New("database connection down")
	}
	m.history = append(m.history, msgs...)
	return nil
}
func (m *mockArchiveStore) GetHistory(ctx context.Context, topic string, from, to *time.Time, limit int) ([]stores.ArchivedMessage, error) {
	return nil, nil
}
func (m *mockArchiveStore) GetAggregatedHistory(ctx context.Context, topics []string, startTime, endTime time.Time, intervalMinutes int, functions []string, fields []string) (*stores.AggregatedResult, error) {
	return nil, nil
}
func (m *mockArchiveStore) GetArchiveStats(ctx context.Context, startTime, endTime *time.Time) (*time.Time, []stores.DailyCount, error) {
	return nil, nil, nil
}
func (m *mockArchiveStore) PurgeOlderThan(ctx context.Context, olderThan time.Time) (stores.PurgeResult, error) {
	return stores.PurgeResult{}, nil
}
func (m *mockArchiveStore) Close() error { return nil }

func TestGroupStoreAndForwardMemory(t *testing.T) {
	mock := &mockArchiveStore{fail: true}
	cfg := stores.ArchiveGroupConfig{
		Name:         "test-memory-group",
		Enabled:      true,
		TopicFilters: []string{"test/#"},
		QueueType:    "MEMORY",
		QueueSize:    100,
	}

	group := NewGroup(cfg, nil, mock, nil)
	group.Start()
	defer group.Stop()

	msg := stores.BrokerMessage{
		TopicName: "test/saf",
		Payload:   []byte("data-1"),
		Time:      time.Now().UTC(),
	}
	group.Submit(msg)

	time.Sleep(350 * time.Millisecond)

	mock.mu.Lock()
	gotHistory := len(mock.history)
	mock.mu.Unlock()

	if gotHistory != 0 {
		t.Fatalf("expected 0 messages written while DB was failing, got %d", gotHistory)
	}
	if group.BufferSize() == 0 {
		t.Fatalf("expected queued message retained in memory buffer, got buffer size 0")
	}

	// Recover database connection
	mock.mu.Lock()
	mock.fail = false
	mock.mu.Unlock()

	// Wait for retry flush cycle (PollBlock retries after 1s delay)
	time.Sleep(1500 * time.Millisecond)

	mock.mu.Lock()
	gotHistory = len(mock.history)
	mock.mu.Unlock()

	if gotHistory != 1 {
		t.Fatalf("expected 1 message recovered after DB recovery, got %d", gotHistory)
	}
	if group.BufferSize() != 0 {
		t.Fatalf("expected buffer size 0 after commit, got %d", group.BufferSize())
	}
}

func TestGroupStoreAndForwardDisk(t *testing.T) {
	tmpDir := t.TempDir()
	mock := &mockArchiveStore{fail: true}
	cfg := stores.ArchiveGroupConfig{
		Name:         "test-disk-group",
		Enabled:      true,
		TopicFilters: []string{"test/#"},
		QueueType:    "DISK",
		QueueSize:    100,
		QueueDiskPath: tmpDir,
	}

	group := NewGroup(cfg, nil, mock, nil)
	group.Start()
	defer group.Stop()

	msg := stores.BrokerMessage{
		TopicName: "test/saf-disk",
		Payload:   []byte("data-disk-1"),
		Time:      time.Now().UTC(),
	}
	group.Submit(msg)

	time.Sleep(350 * time.Millisecond)

	mock.mu.Lock()
	gotHistory := len(mock.history)
	mock.mu.Unlock()

	if gotHistory != 0 {
		t.Fatalf("expected 0 messages written while DB was failing, got %d", gotHistory)
	}
	if group.BufferSize() == 0 {
		t.Fatalf("expected queued message retained in disk buffer, got buffer size 0")
	}

	// Recover database connection
	mock.mu.Lock()
	mock.fail = false
	mock.mu.Unlock()

	// Wait for retry flush cycle (PollBlock retries after 1s delay)
	time.Sleep(1500 * time.Millisecond)

	mock.mu.Lock()
	gotHistory = len(mock.history)
	mock.mu.Unlock()

	if gotHistory != 1 {
		t.Fatalf("expected 1 message recovered after DB recovery, got %d", gotHistory)
	}
	if group.BufferSize() != 0 {
		t.Fatalf("expected buffer size 0 after commit, got %d", group.BufferSize())
	}
}
