package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"monstermq.io/edge/internal/stores"
)

const archiveConfigTable = "archiveconfigs"
const databaseConnectionsTable = "databaseconnections"

type ArchiveConfigStore struct {
	db *DB
}

func NewArchiveConfigStore(db *DB) *ArchiveConfigStore { return &ArchiveConfigStore{db: db} }

func (a *ArchiveConfigStore) Close() error { return nil }

func (a *ArchiveConfigStore) EnsureTable(ctx context.Context) error {
	if _, err := a.db.Exec(`CREATE TABLE IF NOT EXISTS ` + archiveConfigTable + ` (
        name TEXT PRIMARY KEY,
        enabled INTEGER NOT NULL DEFAULT 0,
        topic_filter TEXT NOT NULL,
        retained_only INTEGER NOT NULL DEFAULT 0,
        last_val_type TEXT NOT NULL,
        archive_type TEXT NOT NULL,
        database_connection_name TEXT,
        last_val_retention TEXT,
        archive_retention TEXT,
        purge_interval TEXT,
        created_at TEXT DEFAULT CURRENT_TIMESTAMP,
        updated_at TEXT DEFAULT CURRENT_TIMESTAMP,
        payload_format TEXT DEFAULT 'DEFAULT'
    )`); err != nil {
		return err
	}
	if _, err := a.db.Exec(`ALTER TABLE ` + archiveConfigTable + ` ADD COLUMN database_connection_name TEXT`); err != nil && !isDuplicateColumnErr(err) {
		return err
	}
	_, err := a.db.Exec(`CREATE TABLE IF NOT EXISTS ` + databaseConnectionsTable + ` (
        name TEXT PRIMARY KEY,
        type TEXT NOT NULL,
        url TEXT NOT NULL,
        username TEXT,
        password TEXT,
        database_name TEXT,
        schema_name TEXT,
        read_only INTEGER NOT NULL DEFAULT 0,
        created_at TEXT DEFAULT CURRENT_TIMESTAMP,
        updated_at TEXT DEFAULT CURRENT_TIMESTAMP
    )`)
	return err
}

func isDuplicateColumnErr(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "duplicate column")
}

func rowToArchive(scanner interface{ Scan(...any) error }) (*stores.ArchiveGroupConfig, error) {
	var (
		cfg           stores.ArchiveGroupConfig
		enabled       int
		retainedOnly  int
		topicFilter   string
		lvType        string
		arType        string
		dbConn        sql.NullString
		lvRet         sql.NullString
		arRet         sql.NullString
		purgeInt      sql.NullString
		payloadFormat sql.NullString
	)
	if err := scanner.Scan(&cfg.Name, &enabled, &topicFilter, &retainedOnly, &lvType, &arType, &dbConn, &lvRet, &arRet, &purgeInt, &payloadFormat); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	cfg.Enabled = enabled == 1
	cfg.RetainedOnly = retainedOnly == 1
	cfg.TopicFilters = splitFilter(topicFilter)
	cfg.LastValType = stores.MessageStoreType(lvType)
	cfg.ArchiveType = stores.MessageArchiveType(arType)
	cfg.DatabaseConnectionName = dbConn.String
	cfg.LastValRetention = lvRet.String
	cfg.ArchiveRetention = arRet.String
	cfg.PurgeInterval = purgeInt.String
	if payloadFormat.Valid {
		cfg.PayloadFormat = stores.PayloadFormat(payloadFormat.String)
	} else {
		cfg.PayloadFormat = stores.PayloadDefault
	}
	return &cfg, nil
}

func splitFilter(s string) []string {
	if s == "" {
		return nil
	}
	out := strings.Split(s, ",")
	for i := range out {
		out[i] = strings.TrimSpace(out[i])
	}
	return out
}

func joinFilter(s []string) string { return strings.Join(s, ",") }

func (a *ArchiveConfigStore) GetAll(ctx context.Context) ([]stores.ArchiveGroupConfig, error) {
	rows, err := a.db.Conn().QueryContext(ctx,
		`SELECT name, enabled, topic_filter, retained_only, last_val_type, archive_type, database_connection_name, last_val_retention, archive_retention, purge_interval, payload_format FROM `+archiveConfigTable+` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []stores.ArchiveGroupConfig{}
	for rows.Next() {
		c, err := rowToArchive(rows)
		if err != nil {
			return nil, err
		}
		if c != nil {
			out = append(out, *c)
		}
	}
	return out, rows.Err()
}

func (a *ArchiveConfigStore) Get(ctx context.Context, name string) (*stores.ArchiveGroupConfig, error) {
	row := a.db.Conn().QueryRowContext(ctx,
		`SELECT name, enabled, topic_filter, retained_only, last_val_type, archive_type, database_connection_name, last_val_retention, archive_retention, purge_interval, payload_format FROM `+archiveConfigTable+` WHERE name = ?`, name)
	return rowToArchive(row)
}

func (a *ArchiveConfigStore) Save(ctx context.Context, cfg stores.ArchiveGroupConfig) error {
	_, err := a.db.Exec(`INSERT INTO `+archiveConfigTable+` (name, enabled, topic_filter, retained_only, last_val_type, archive_type, database_connection_name, last_val_retention, archive_retention, purge_interval, payload_format)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT (name) DO UPDATE SET
            enabled = excluded.enabled,
            topic_filter = excluded.topic_filter,
            retained_only = excluded.retained_only,
            last_val_type = excluded.last_val_type,
            archive_type = excluded.archive_type,
            database_connection_name = excluded.database_connection_name,
            last_val_retention = excluded.last_val_retention,
            archive_retention = excluded.archive_retention,
            purge_interval = excluded.purge_interval,
            payload_format = excluded.payload_format,
            updated_at = CURRENT_TIMESTAMP`,
		cfg.Name, boolToInt(cfg.Enabled), joinFilter(cfg.TopicFilters), boolToInt(cfg.RetainedOnly),
		string(cfg.LastValType), string(cfg.ArchiveType), nullStr(cfg.DatabaseConnectionName), nullStr(cfg.LastValRetention), nullStr(cfg.ArchiveRetention), nullStr(cfg.PurgeInterval), string(cfg.PayloadFormat))
	return err
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (a *ArchiveConfigStore) Update(ctx context.Context, cfg stores.ArchiveGroupConfig) error {
	return a.Save(ctx, cfg)
}

func (a *ArchiveConfigStore) Delete(ctx context.Context, name string) error {
	_, err := a.db.Exec(`DELETE FROM `+archiveConfigTable+` WHERE name = ?`, name)
	return err
}

func rowToDatabaseConnection(scanner interface{ Scan(...any) error }) (*stores.DatabaseConnectionConfig, error) {
	var (
		cfg       stores.DatabaseConnectionConfig
		typ       string
		username  sql.NullString
		password  sql.NullString
		database  sql.NullString
		schema    sql.NullString
		readOnly  int
		createdAt sql.NullString
		updatedAt sql.NullString
	)
	if err := scanner.Scan(&cfg.Name, &typ, &cfg.URL, &username, &password, &database, &schema, &readOnly, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	cfg.Type = stores.DatabaseConnectionType(typ)
	cfg.Username = username.String
	cfg.Password = password.String
	cfg.Database = database.String
	cfg.Schema = schema.String
	cfg.ReadOnly = readOnly == 1
	cfg.CreatedAt = parseSQLiteTime(createdAt.String)
	cfg.UpdatedAt = parseSQLiteTime(updatedAt.String)
	return &cfg, nil
}

func parseSQLiteTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func (a *ArchiveConfigStore) GetAllDatabaseConnections(ctx context.Context) ([]stores.DatabaseConnectionConfig, error) {
	rows, err := a.db.Conn().QueryContext(ctx,
		`SELECT name, type, url, username, password, database_name, schema_name, read_only, created_at, updated_at FROM `+databaseConnectionsTable+` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []stores.DatabaseConnectionConfig{}
	for rows.Next() {
		c, err := rowToDatabaseConnection(rows)
		if err != nil {
			return nil, err
		}
		if c != nil {
			out = append(out, *c)
		}
	}
	return out, rows.Err()
}

func (a *ArchiveConfigStore) GetDatabaseConnection(ctx context.Context, name string) (*stores.DatabaseConnectionConfig, error) {
	row := a.db.Conn().QueryRowContext(ctx,
		`SELECT name, type, url, username, password, database_name, schema_name, read_only, created_at, updated_at FROM `+databaseConnectionsTable+` WHERE name = ?`, name)
	return rowToDatabaseConnection(row)
}

func (a *ArchiveConfigStore) SaveDatabaseConnection(ctx context.Context, cfg stores.DatabaseConnectionConfig) error {
	_, err := a.db.Exec(`INSERT INTO `+databaseConnectionsTable+` (name, type, url, username, password, database_name, schema_name, read_only)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT (name) DO UPDATE SET
            type = excluded.type,
            url = excluded.url,
            username = excluded.username,
            password = excluded.password,
            database_name = excluded.database_name,
            schema_name = excluded.schema_name,
            read_only = excluded.read_only,
            updated_at = CURRENT_TIMESTAMP`,
		cfg.Name, string(cfg.Type), cfg.URL, nullStr(cfg.Username), nullStr(cfg.Password), nullStr(cfg.Database), nullStr(cfg.Schema), boolToInt(cfg.ReadOnly))
	return err
}

func (a *ArchiveConfigStore) DeleteDatabaseConnection(ctx context.Context, name string) error {
	_, err := a.db.Exec(`DELETE FROM `+databaseConnectionsTable+` WHERE name = ?`, name)
	return err
}
