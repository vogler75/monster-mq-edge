package broker

import (
	"context"
	"fmt"
	"strings"

	"monstermq.io/edge/internal/archive"
	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/kafka"
	"monstermq.io/edge/internal/stores"
	storemongo "monstermq.io/edge/internal/stores/mongodb"
	storepg "monstermq.io/edge/internal/stores/postgres"
	storesqlite "monstermq.io/edge/internal/stores/sqlite"
)

// kafkaStoreFactory opens the per-stream queue stores of Kafka servers.
//
// The effective store type of a stream is, in order: the stream override, the
// server's storeType (both handed in by the Kafka server), QueueStoreType,
// DefaultStoreType, SessionStoreType (unless MEMORY), else SQLITE. The value
// MEMORY is an in-memory SQLite database; SQLITE, POSTGRES (also POSTGRESQL)
// and MONGODB (case-insensitive) use the broker's own backend handles; any
// other value names a stored database connection.
type kafkaStoreFactory struct {
	cfg      *config.Config
	storage  *stores.Storage
	sqliteDB *storesqlite.DB
	pgDB     *storepg.DB
	mongoDB  *storemongo.DB
}

func (f *kafkaStoreFactory) factory() kafka.StoreFactory {
	return func(sc kafka.ServerConfig) func(context.Context, *string, string) (stores.KafkaQueueStore, error) {
		return func(ctx context.Context, storeType *string, suffix string) (stores.KafkaQueueStore, error) {
			return f.open(ctx, sc.Name, storeType, suffix)
		}
	}
}

func (f *kafkaStoreFactory) defaultType() string {
	for _, t := range []config.StoreType{f.cfg.QueueStoreType, f.cfg.DefaultStoreType, f.cfg.SessionStoreType} {
		if t != "" && t != config.StoreMemory {
			return string(t)
		}
	}
	if f.cfg.QueueStoreType == config.StoreMemory {
		return string(config.StoreMemory)
	}
	return string(config.StoreSQLite)
}

// ownedKafkaStore closes the database handle opened for the store as well.
type ownedKafkaStore struct {
	stores.KafkaQueueStore
	closeDB func() error
}

func (o ownedKafkaStore) Close() error {
	err := o.KafkaQueueStore.Close()
	if cerr := o.closeDB(); err == nil {
		err = cerr
	}
	return err
}

func (f *kafkaStoreFactory) open(ctx context.Context, server string, storeType *string, suffix string) (stores.KafkaQueueStore, error) {
	name := ""
	if storeType != nil {
		name = strings.TrimSpace(*storeType)
	}
	if name == "" {
		name = f.defaultType()
	}
	if archive.IsDefaultDatabaseConnectionName(name) {
		name = f.defaultType()
	}
	switch strings.ToUpper(name) {
	case "MEMORY":
		db, err := storesqlite.OpenMemory("kafka_" + server + "_" + suffix)
		if err != nil {
			return nil, err
		}
		return ownedKafkaStore{storesqlite.NewKafkaQueueStore(db, suffix), db.Close}, nil
	case "SQLITE", "SQLITE3":
		if f.sqliteDB != nil {
			return storesqlite.NewKafkaQueueStore(f.sqliteDB, suffix), nil
		}
		db, err := storesqlite.Open(f.cfg.SQLite.Path)
		if err != nil {
			return nil, err
		}
		return ownedKafkaStore{storesqlite.NewKafkaQueueStore(db, suffix), db.Close}, nil
	case "POSTGRES", "POSTGRESQL":
		if f.pgDB == nil {
			return nil, fmt.Errorf("store type %q: PostgreSQL is not configured on this broker", name)
		}
		return storepg.NewKafkaQueueStore(f.pgDB, suffix), nil
	case "MONGODB", "MONGO":
		if f.mongoDB == nil {
			return nil, fmt.Errorf("store type %q: MongoDB is not configured on this broker", name)
		}
		return storemongo.NewKafkaQueueStore(f.mongoDB, suffix), nil
	}

	// Named database connection.
	if f.storage == nil || f.storage.ArchiveConfig == nil {
		return nil, fmt.Errorf("database connection %q not found", name)
	}
	conn, err := f.storage.ArchiveConfig.GetDatabaseConnection(ctx, name)
	if err != nil {
		return nil, err
	}
	if conn == nil {
		return nil, fmt.Errorf("database connection %q not found", name)
	}
	switch conn.Type {
	case stores.DatabaseConnectionSQLite:
		db, err := storesqlite.Open(conn.URL)
		if err != nil {
			return nil, err
		}
		return ownedKafkaStore{storesqlite.NewKafkaQueueStore(db, suffix), db.Close}, nil
	case stores.DatabaseConnectionPostgres:
		db, err := storepg.Open(ctx, archive.PostgresDSN(conn.URL, conn.Username, conn.Password))
		if err != nil {
			return nil, err
		}
		return ownedKafkaStore{storepg.NewKafkaQueueStore(db, suffix), db.Close}, nil
	case stores.DatabaseConnectionMongoDB:
		dbName := conn.Database
		if dbName == "" {
			dbName = "monstermq"
		}
		db, err := storemongo.Open(ctx, archive.MongoURLWithCredentials(conn.URL, conn.Username, conn.Password), dbName)
		if err != nil {
			return nil, err
		}
		return ownedKafkaStore{storemongo.NewKafkaQueueStore(db, suffix), db.Close}, nil
	}
	return nil, fmt.Errorf("database connection %q has unsupported type %q for a Kafka queue", name, conn.Type)
}
