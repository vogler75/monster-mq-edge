package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	"monstermq.io/edge/internal/stores"
)

type MetricsStore struct {
	mu      sync.RWMutex
	maxRows int
	rows    []metricsRecord
}

type metricsRecord struct {
	kind       stores.MetricKind
	identifier string
	timestamp  time.Time
	payload    string
}

func NewMetricsStore(maxRows int) *MetricsStore {
	if maxRows <= 0 {
		maxRows = 3600
	}
	return &MetricsStore{maxRows: maxRows}
}

func (m *MetricsStore) Close() error { return nil }

func (m *MetricsStore) StoreMetrics(_ context.Context, kind stores.MetricKind, ts time.Time, identifier, payload string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ts = ts.UTC()
	for i := range m.rows {
		if m.rows[i].kind == kind && m.rows[i].identifier == identifier && m.rows[i].timestamp.Equal(ts) {
			m.rows[i].payload = payload
			return nil
		}
	}
	m.rows = append(m.rows, metricsRecord{kind: kind, identifier: identifier, timestamp: ts, payload: payload})
	sort.SliceStable(m.rows, func(i, j int) bool {
		return m.rows[i].timestamp.Before(m.rows[j].timestamp)
	})
	if len(m.rows) > m.maxRows {
		m.rows = append([]metricsRecord(nil), m.rows[len(m.rows)-m.maxRows:]...)
	}
	return nil
}

func (m *MetricsStore) GetLatest(_ context.Context, kind stores.MetricKind, identifier string) (time.Time, string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for i := len(m.rows) - 1; i >= 0; i-- {
		row := m.rows[i]
		if row.kind == kind && row.identifier == identifier {
			return row.timestamp, row.payload, nil
		}
	}
	return time.Time{}, "", nil
}

func (m *MetricsStore) GetHistory(_ context.Context, kind stores.MetricKind, identifier string, from, to time.Time, limit int) ([]stores.MetricsRow, error) {
	if limit <= 0 {
		limit = 1000
	}
	from = from.UTC()
	to = to.UTC()
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []stores.MetricsRow{}
	for _, row := range m.rows {
		if row.kind != kind || row.identifier != identifier {
			continue
		}
		if row.timestamp.Before(from) || row.timestamp.After(to) {
			continue
		}
		out = append(out, stores.MetricsRow{Timestamp: row.timestamp, Payload: row.payload})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *MetricsStore) PurgeOlderThan(_ context.Context, t time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t = t.UTC()
	kept := m.rows[:0]
	var n int64
	for _, row := range m.rows {
		if row.timestamp.Before(t) {
			n++
			continue
		}
		kept = append(kept, row)
	}
	m.rows = kept
	return n, nil
}

var _ stores.MetricsStore = (*MetricsStore)(nil)
