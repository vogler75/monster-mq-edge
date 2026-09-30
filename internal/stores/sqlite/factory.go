package sqlite

import (
	"context"
	"time"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/stores"
	storememory "monstermq.io/edge/internal/stores/memory"
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
	return build(ctx, cfg, db)
}

// BuildMemory is Build on an in-memory database: nothing is written to a
// file. Used as the base for DefaultStoreType WINCCOA, where the persistent
// stores are replaced by WinCC OA datapoints and the rest is volatile.
func BuildMemory(ctx context.Context, cfg *config.Config) (*stores.Storage, error) {
	db, err := OpenMemory("monstermq-base-" + cfg.NodeID)
	if err != nil {
		return nil, err
	}
	st, _, err := build(ctx, cfg, db)
	return st, err
}

func build(ctx context.Context, cfg *config.Config, db *DB) (*stores.Storage, *DB, error) {
	var err error
	closers := []func() error{db.Close}

	var retained stores.MessageStore = NewMessageStore("retainedmessages", db)
	if cfg.RetainedStore() == config.StoreMemory {
		retained = storememory.NewMessageStore("retainedmessages")
	}
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
