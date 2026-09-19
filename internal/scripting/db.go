package scripting

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"monstermq.io/edge/internal/stores"
	storepg "monstermq.io/edge/internal/stores/postgres"
	storesqlite "monstermq.io/edge/internal/stores/sqlite"
)

// DatabaseManager handles database queries and statement executions from scripts.
type DatabaseManager struct {
	archiveConfigStore stores.ArchiveConfigStore
	sqliteDB           *storesqlite.DB
	pgDB               *storepg.DB

	mu          sync.RWMutex
	customSqlDB map[string]*sql.DB
	customPgPool map[string]*pgxpool.Pool
}

func NewDatabaseManager(archiveConfigStore stores.ArchiveConfigStore, sqliteDB *storesqlite.DB, pgDB *storepg.DB) *DatabaseManager {
	return &DatabaseManager{
		archiveConfigStore: archiveConfigStore,
		sqliteDB:           sqliteDB,
		pgDB:               pgDB,
		customSqlDB:        make(map[string]*sql.DB),
		customPgPool:       make(map[string]*pgxpool.Pool),
	}
}

func (m *DatabaseManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, db := range m.customSqlDB {
		_ = db.Close()
	}
	m.customSqlDB = make(map[string]*sql.DB)

	for _, p := range m.customPgPool {
		p.Close()
	}
	m.customPgPool = make(map[string]*pgxpool.Pool)
}

// Query executes a read-only or data query and returns rows as []map[string]any.
func (m *DatabaseManager) Query(ctx context.Context, connName, query string, args []any) ([]map[string]any, error) {
	connName = strings.TrimSpace(connName)
	if connName == "" || strings.EqualFold(connName, "default") {
		if m.sqliteDB != nil && m.sqliteDB.Conn() != nil {
			return querySqlDB(ctx, m.sqliteDB.Conn(), query, args)
		}
		if m.pgDB != nil && m.pgDB.Pool() != nil {
			return queryPgxPool(ctx, m.pgDB.Pool(), query, args)
		}
		return nil, fmt.Errorf("no default database connection configured")
	}

	cfg, err := m.getConnConfig(ctx, connName)
	if err != nil {
		return nil, err
	}

	switch cfg.Type {
	case stores.DatabaseConnectionSQLite:
		sdb, err := m.getOrOpenSqlite(cfg)
		if err != nil {
			return nil, err
		}
		return querySqlDB(ctx, sdb, query, args)
	case stores.DatabaseConnectionPostgres, stores.DatabaseConnectionCrateDB, stores.DatabaseConnectionQuestDB:
		pgp, err := m.getOrOpenPostgres(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return queryPgxPool(ctx, pgp, query, args)
	default:
		return nil, fmt.Errorf("unsupported database connection type: %s", cfg.Type)
	}
}

// Execute executes an INSERT/UPDATE/DELETE statement and returns affected rows.
func (m *DatabaseManager) Execute(ctx context.Context, connName, query string, args []any) (map[string]any, error) {
	connName = strings.TrimSpace(connName)
	if connName == "" || strings.EqualFold(connName, "default") {
		if m.sqliteDB != nil {
			res, err := m.sqliteDB.Exec(query, args...)
			if err != nil {
				return nil, err
			}
			affected, _ := res.RowsAffected()
			return map[string]any{"affected_rows": affected, "success": true}, nil
		}
		if m.pgDB != nil && m.pgDB.Pool() != nil {
			tag, err := m.pgDB.Pool().Exec(ctx, query, args...)
			if err != nil {
				return nil, err
			}
			return map[string]any{"affected_rows": tag.RowsAffected(), "success": true}, nil
		}
		return nil, fmt.Errorf("no default database connection configured")
	}

	cfg, err := m.getConnConfig(ctx, connName)
	if err != nil {
		return nil, err
	}
	if cfg.ReadOnly {
		return nil, fmt.Errorf("database connection %s is read-only", connName)
	}

	switch cfg.Type {
	case stores.DatabaseConnectionSQLite:
		sdb, err := m.getOrOpenSqlite(cfg)
		if err != nil {
			return nil, err
		}
		res, err := sdb.ExecContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		affected, _ := res.RowsAffected()
		return map[string]any{"affected_rows": affected, "success": true}, nil
	case stores.DatabaseConnectionPostgres, stores.DatabaseConnectionCrateDB, stores.DatabaseConnectionQuestDB:
		pgp, err := m.getOrOpenPostgres(ctx, cfg)
		if err != nil {
			return nil, err
		}
		tag, err := pgp.Exec(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		return map[string]any{"affected_rows": tag.RowsAffected(), "success": true}, nil
	default:
		return nil, fmt.Errorf("unsupported database connection type: %s", cfg.Type)
	}
}

func (m *DatabaseManager) getConnConfig(ctx context.Context, name string) (*stores.DatabaseConnectionConfig, error) {
	if m.archiveConfigStore == nil {
		return nil, fmt.Errorf("archive config store not available")
	}
	cfg, err := m.archiveConfigStore.GetDatabaseConnection(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("database connection %q: %w", name, err)
	}
	if cfg == nil {
		return nil, fmt.Errorf("database connection %q not found", name)
	}
	return cfg, nil
}

func (m *DatabaseManager) getOrOpenSqlite(cfg *stores.DatabaseConnectionConfig) (*sql.DB, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if db, ok := m.customSqlDB[cfg.Name]; ok {
		return db, nil
	}
	db, err := sql.Open("sqlite", cfg.URL)
	if err != nil {
		return nil, err
	}
	m.customSqlDB[cfg.Name] = db
	return db, nil
}

func (m *DatabaseManager) getOrOpenPostgres(ctx context.Context, cfg *stores.DatabaseConnectionConfig) (*pgxpool.Pool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.customPgPool[cfg.Name]; ok {
		return p, nil
	}
	dsn := cfg.URL
	if cfg.Username != "" || cfg.Password != "" {
		dsn = fmt.Sprintf("%s?user=%s&password=%s", dsn, cfg.Username, cfg.Password)
	}
	dsn = strings.TrimPrefix(dsn, "jdbc:")
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	m.customPgPool[cfg.Name] = p
	return p, nil
}

func querySqlDB(ctx context.Context, db *sql.DB, query string, args []any) ([]map[string]any, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	results := []map[string]any{}
	for rows.Next() {
		colVals := make([]any, len(cols))
		colPointers := make([]any, len(cols))
		for i := range colVals {
			colPointers[i] = &colVals[i]
		}
		if err := rows.Scan(colPointers...); err != nil {
			return nil, err
		}
		rowMap := make(map[string]any, len(cols))
		for i, col := range cols {
			val := colVals[i]
			if b, ok := val.([]byte); ok {
				val = string(b)
			}
			rowMap[col] = val
		}
		results = append(results, rowMap)
	}
	return results, rows.Err()
}

func queryPgxPool(ctx context.Context, pool *pgxpool.Pool, query string, args []any) ([]map[string]any, error) {
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	fieldDescs := rows.FieldDescriptions()
	results := []map[string]any{}

	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		rowMap := make(map[string]any, len(fieldDescs))
		for i, fd := range fieldDescs {
			val := vals[i]
			if b, ok := val.([]byte); ok {
				val = string(b)
			}
			rowMap[fd.Name] = val
		}
		results = append(results, rowMap)
	}
	return results, rows.Err()
}
