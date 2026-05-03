package sqlite

import (
	"context"
	"time"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/stores"
)

// Build wires up a full Storage backed by a single SQLite file. The schema is
// byte-compatible with the Kotlin MonsterMQ broker so the same DB can be opened
// by either implementation.
//
// The returned *DB is exposed so the archive manager can create per-group
// last-value/archive tables on the same connection.
func Build(ctx context.Context, cfg *config.Config) (*stores.Storage, *DB, error) {
	db, err := Open(cfg.SQLite.Path)
	if err != nil {
		return nil, nil, err
	}
	closers := []func() error{db.Close}

	retained := NewMessageStore("retainedmessages", db)
	users := NewUserStore(db)
	archives := NewArchiveConfigStore(db)
	devices := NewDeviceConfigStore(db)
	var metrics stores.MetricsStore
	if cfg.MetricsStore() == config.StoreSQLite {
		metrics = NewMetricsStore(db)
	}

	sessionDB := db
	if cfg.SessionStore() == config.StoreMemory {
		sessionDB, err = OpenMemory("monstermq-sessions-" + cfg.NodeID)
		if err != nil {
			_ = closeAll(closers)
			return nil, nil, err
		}
		closers = append(closers, sessionDB.Close)
	}
	sessions := NewSessionStore(sessionDB)

	queueDB := db
	if cfg.QueueStore() == config.StoreMemory {
		queueDB, err = OpenMemory("monstermq-queue-" + cfg.NodeID)
		if err != nil {
			_ = closeAll(closers)
			return nil, nil, err
		}
		closers = append(closers, queueDB.Close)
	}
	queue := NewQueueStore(queueDB, 30*time.Second)

	toEnsure := []interface{ EnsureTable(context.Context) error }{retained, users, archives, devices, sessions, queue}
	if metrics != nil {
		toEnsure = append(toEnsure, metrics.(interface{ EnsureTable(context.Context) error }))
	}
	for _, t := range toEnsure {
		if err := t.EnsureTable(ctx); err != nil {
			_ = closeAll(closers)
			return nil, nil, err
		}
	}

	storage := &stores.Storage{
		Backend:       config.StoreSQLite,
		Sessions:      sessions,
		Subscriptions: sessions,
		Queue:         queue,
		Retained:      retained,
		Users:         users,
		ArchiveConfig: archives,
		DeviceConfig:  devices,
		Metrics:       metrics,
		Closer:        func() error { return closeAll(closers) },
	}
	return storage, db, nil
}

func closeAll(closers []func() error) error {
	var first error
	for _, closeFn := range closers {
		if closeFn == nil {
			continue
		}
		if err := closeFn(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
