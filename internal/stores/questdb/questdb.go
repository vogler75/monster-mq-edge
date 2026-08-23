// Package questdb implements the MessageArchive storage interface against QuestDB
// using github.com/questdb/go-questdb-client/v4 for QWP data ingestion and jackc/pgx/v5
// for PGWire DDL, queries, retention purging, and statistics.
package questdb

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	qdb "github.com/questdb/go-questdb-client/v4"

	"monstermq.io/edge/internal/stores"
)

// DB wraps both a QuestDB QWP client handle (for ingestion) and a pgx connection pool (for queries/DDL).
type DB struct {
	qdbPool *qdb.QuestDB
	pgPool  *pgxpool.Pool
	mu      sync.Mutex
}

// Open initializes a connection to QuestDB given connection parameters.
// qwpConf may be in QWP format (e.g. "ws::addr=localhost:9000;") or a plain host:port / URL.
// pgDsn is the PostgreSQL wire protocol connection string (e.g. "postgres://localhost:8812/qdb").
func Open(ctx context.Context, rawURL, username, password string) (*DB, error) {
	qwpConf := NormalizeQWPConf(rawURL, username, password)
	pgDSN := NormalizePGDSN(rawURL, username, password)

	qdbClient, err := qdb.Connect(ctx, qwpConf)
	if err != nil {
		return nil, fmt.Errorf("connect questdb qwp (%s): %w", qwpConf, err)
	}

	pgCfg, err := pgxpool.ParseConfig(pgDSN)
	if err != nil {
		_ = qdbClient.Close(ctx)
		return nil, fmt.Errorf("parse questdb pg dsn (%s): %w", pgDSN, err)
	}

	pgPool, err := pgxpool.NewWithConfig(ctx, pgCfg)
	if err != nil {
		_ = qdbClient.Close(ctx)
		return nil, fmt.Errorf("connect questdb pgwire: %w", err)
	}

	return &DB{
		qdbPool: qdbClient,
		pgPool:  pgPool,
	}, nil
}

func (d *DB) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if d.qdbPool != nil {
		_ = d.qdbPool.Close(ctx)
	}
	if d.pgPool != nil {
		d.pgPool.Close()
	}
	return nil
}

func (d *DB) BorrowSender(ctx context.Context) (qdb.LineSender, error) {
	return d.qdbPool.BorrowSender(ctx)
}

func (d *DB) PGPool() *pgxpool.Pool {
	return d.pgPool
}

// NormalizeQWPConf converts various URL styles into a valid QuestDB QWP connection string.
func NormalizeQWPConf(rawURL, username, password string) string {
	raw := strings.TrimSpace(rawURL)
	raw = strings.TrimPrefix(raw, "jdbc:")

	if strings.HasPrefix(raw, "ws::") || strings.HasPrefix(raw, "http::") || strings.HasPrefix(raw, "https::") {
		return raw
	}

	host := "localhost"
	port := "9000"

	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "postgres://") || strings.HasPrefix(raw, "postgresql://") {
		if u, err := url.Parse(raw); err == nil {
			if h := u.Hostname(); h != "" {
				host = h
			}
			if p := u.Port(); p != "" {
				if p == "8812" {
					port = "9000"
				} else {
					port = p
				}
			}
		}
	} else if strings.Contains(raw, ":") {
		parts := strings.Split(raw, ":")
		if len(parts) >= 2 {
			host = strings.TrimPrefix(parts[0], "//")
			portPart := strings.Split(parts[1], "/")[0]
			portPart = strings.Split(portPart, "?")[0]
			if portPart == "8812" {
				port = "9000"
			} else if portPart != "" {
				port = portPart
			}
		}
	} else if raw != "" {
		host = raw
	}

	conf := fmt.Sprintf("ws::addr=%s:%s;", host, port)
	if username != "" || password != "" {
		conf += fmt.Sprintf("username=%s;password=%s;", username, password)
	}
	return conf
}

// NormalizePGDSN converts various URL styles into a valid PGWire PostgreSQL connection string for QuestDB.
func NormalizePGDSN(rawURL, username, password string) string {
	raw := strings.TrimSpace(rawURL)
	raw = strings.TrimPrefix(raw, "jdbc:")

	host := "localhost"
	port := "8812"
	user := username
	if user == "" {
		user = "admin"
	}
	pass := password
	if pass == "" {
		pass = "quest"
	}

	if strings.HasPrefix(raw, "ws::") || strings.HasPrefix(raw, "http::") || strings.HasPrefix(raw, "https::") {
		// extract addr
		for _, part := range strings.Split(raw, ";") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, "addr=") {
				addr := strings.TrimPrefix(part, "addr=")
				if h, p, err := parseHostPort(addr); err == nil {
					host = h
					if p == "9000" {
						port = "8812"
					} else if p != "" {
						port = p
					}
				}
			} else if strings.HasPrefix(part, "username=") {
				if username == "" {
					user = strings.TrimPrefix(part, "username=")
				}
			} else if strings.HasPrefix(part, "password=") {
				if password == "" {
					pass = strings.TrimPrefix(part, "password=")
				}
			}
		}
	} else if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "postgres://") || strings.HasPrefix(raw, "postgresql://") {
		if u, err := url.Parse(raw); err == nil {
			if h := u.Hostname(); h != "" {
				host = h
			}
			if p := u.Port(); p != "" {
				if p == "9000" {
					port = "8812"
				} else {
					port = p
				}
			}
			if u.User != nil {
				if u.User.Username() != "" && username == "" {
					user = u.User.Username()
				}
				if pwd, ok := u.User.Password(); ok && password == "" {
					pass = pwd
				}
			}
		}
	} else if strings.Contains(raw, ":") {
		parts := strings.Split(raw, ":")
		if len(parts) >= 2 {
			host = strings.TrimPrefix(parts[0], "//")
			portPart := strings.Split(parts[1], "/")[0]
			portPart = strings.Split(portPart, "?")[0]
			if portPart == "9000" {
				port = "8812"
			} else if portPart != "" {
				port = portPart
			}
		}
	} else if raw != "" {
		host = raw
	}

	return fmt.Sprintf("postgres://%s:%s@%s:%s/qdb?sslmode=disable",
		url.QueryEscape(user), url.QueryEscape(pass), host, port)
}

func parseHostPort(addr string) (string, string, error) {
	parts := strings.Split(addr, ":")
	if len(parts) == 2 {
		return parts[0], parts[1], nil
	}
	if len(parts) == 1 {
		return parts[0], "", nil
	}
	return "", "", fmt.Errorf("invalid addr %s", addr)
}

// MessageArchive -----------------------------------------------------------

type MessageArchive struct {
	name string
	db   *DB
	fmt  stores.PayloadFormat
}

func NewMessageArchive(name string, db *DB, fmt stores.PayloadFormat) *MessageArchive {
	return &MessageArchive{name: name, db: db, fmt: fmt}
}

func (a *MessageArchive) Name() string                    { return a.name }
func (a *MessageArchive) Type() stores.MessageArchiveType { return stores.ArchiveQuestDB }
func (a *MessageArchive) Close() error                    { return nil }
func (a *MessageArchive) tableName() string               { return strings.ToLower(a.name) }

func (a *MessageArchive) EnsureTable(ctx context.Context) error {
	t := a.tableName()
	q := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
        topic SYMBOL,
        timestamp TIMESTAMP,
        payload_b64 VARCHAR,
        payload_json VARCHAR,
        qos INT,
        retained BOOLEAN,
        client_id SYMBOL,
        message_uuid VARCHAR
    ) TIMESTAMP(timestamp) PARTITION BY DAY WAL DEDUP UPSERT KEYS(timestamp, topic);`, t)

	_, err := a.db.PGPool().Exec(ctx, q)
	return err
}

func (a *MessageArchive) AddHistory(ctx context.Context, msgs []stores.BrokerMessage) error {
	if len(msgs) == 0 {
		return nil
	}
	sender, err := a.db.BorrowSender(ctx)
	if err != nil {
		return fmt.Errorf("borrow questdb sender: %w", err)
	}
	defer sender.Close(ctx)

	t := a.tableName()
	for _, m := range msgs {
		sender.Table(t).
			Symbol("topic", m.TopicName).
			Symbol("client_id", m.ClientID).
			StringColumn("message_uuid", m.MessageUUID).
			Int64Column("qos", int64(m.QoS)).
			BoolColumn("retained", m.IsRetain)

		if a.fmt == stores.PayloadJSON && len(m.Payload) > 0 && json.Valid(m.Payload) {
			sender.StringColumn("payload_json", string(m.Payload)).
				StringColumn("payload_b64", "")
		} else {
			sender.StringColumn("payload_json", "").
				StringColumn("payload_b64", base64.StdEncoding.EncodeToString(m.Payload))
		}

		if err := sender.At(ctx, m.Time.UTC()); err != nil {
			return fmt.Errorf("append questdb record: %w", err)
		}
	}

	if err := sender.Flush(ctx); err != nil {
		return fmt.Errorf("flush questdb sender: %w", err)
	}
	return nil
}

func (a *MessageArchive) getTimestampColumn(ctx context.Context) string {
	var col string
	err := a.db.PGPool().QueryRow(ctx, "SELECT \"column\" FROM table_columns($1) WHERE designated = true", a.tableName()).Scan(&col)
	if err == nil && col != "" {
		return col
	}
	return "timestamp"
}

func (a *MessageArchive) GetHistory(ctx context.Context, topic string, from, to *time.Time, limit int) ([]stores.ArchivedMessage, error) {
	if limit <= 0 {
		limit = 100
	}
	tsCol := a.getTimestampColumn(ctx)
	pattern := strings.ReplaceAll(strings.ReplaceAll(topic, "#", "%"), "+", "%")
	q := fmt.Sprintf(`SELECT topic, %s, payload_b64, payload_json, qos, client_id FROM %s WHERE topic LIKE $1`, tsCol, a.tableName())
	args := []any{pattern}
	if from != nil {
		q += fmt.Sprintf(` AND %s >= $%d`, tsCol, len(args)+1)
		args = append(args, from.UTC())
	}
	if to != nil {
		q += fmt.Sprintf(` AND %s <= $%d`, tsCol, len(args)+1)
		args = append(args, to.UTC())
	}
	q += fmt.Sprintf(` ORDER BY %s DESC LIMIT $%d`, tsCol, len(args)+1)
	args = append(args, limit)

	rows, err := a.db.PGPool().Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []stores.ArchivedMessage{}
	for rows.Next() {
		var (
			t           string
			ts          time.Time
			payloadB64  *string
			payloadJSON *string
			qos         int
			cid         *string
		)
		if err := rows.Scan(&t, &ts, &payloadB64, &payloadJSON, &qos, &cid); err != nil {
			return nil, err
		}
		var payload []byte
		if payloadB64 != nil && *payloadB64 != "" {
			if decoded, err := base64.StdEncoding.DecodeString(*payloadB64); err == nil {
				payload = decoded
			}
		}
		if len(payload) == 0 && payloadJSON != nil && *payloadJSON != "" {
			payload = []byte(*payloadJSON)
		}
		am := stores.ArchivedMessage{Topic: t, Timestamp: ts, Payload: payload, QoS: byte(qos)}
		if cid != nil {
			am.ClientID = *cid
		}
		out = append(out, am)
	}
	return out, rows.Err()
}

func (a *MessageArchive) GetArchiveStats(ctx context.Context, startTime, endTime *time.Time) (minTimestamp *time.Time, dailyCounts []stores.DailyCount, err error) {
	dailyCounts = []stores.DailyCount{}
	tsCol := a.getTimestampColumn(ctx)

	whereClause := " WHERE 1=1"
	var args []any
	paramIndex := 1
	if startTime != nil {
		whereClause += fmt.Sprintf(" AND %s >= $%d", tsCol, paramIndex)
		args = append(args, startTime.UTC())
		paramIndex++
	}
	if endTime != nil {
		whereClause += fmt.Sprintf(" AND %s <= $%d", tsCol, paramIndex)
		args = append(args, endTime.UTC())
		paramIndex++
	}

	// 1. Get min timestamp
	var minTs *time.Time
	minQ := fmt.Sprintf("SELECT MIN(%s) FROM %s%s", tsCol, a.tableName(), whereClause)
	err = a.db.PGPool().QueryRow(ctx, minQ, args...).Scan(&minTs)
	if err != nil && !errorsIsNoRows(err) {
		return nil, nil, err
	}
	minTimestamp = minTs

	// 2. Get daily counts
	countsQ := fmt.Sprintf(`
		SELECT date_trunc('day', %s) AS day, count(*) AS count
		FROM %s%s
		GROUP BY 1
		ORDER BY 1 ASC
	`, tsCol, a.tableName(), whereClause)

	rows, err := a.db.PGPool().Query(ctx, countsQ, args...)
	if err != nil {
		return minTimestamp, dailyCounts, err
	}
	defer rows.Close()

	for rows.Next() {
		var dayTs time.Time
		var count int64
		if err := rows.Scan(&dayTs, &count); err != nil {
			return minTimestamp, dailyCounts, err
		}
		dailyCounts = append(dailyCounts, stores.DailyCount{
			Date:  dayTs.Format("2006-01-02"),
			Count: count,
		})
	}
	return minTimestamp, dailyCounts, rows.Err()
}

func errorsIsNoRows(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no rows")
}

func (a *MessageArchive) PurgeOlderThan(ctx context.Context, t time.Time) (stores.PurgeResult, error) {
	tsCol := a.getTimestampColumn(ctx)
	res, err := a.db.PGPool().Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE %s < $1`, a.tableName(), tsCol), t.UTC())
	if err != nil {
		return stores.PurgeResult{Err: err}, err
	}
	return stores.PurgeResult{DeletedRows: res.RowsAffected()}, nil
}

func (a *MessageArchive) GetAggregatedHistory(ctx context.Context, topics []string, startTime, endTime time.Time, intervalMinutes int, functions []string, fields []string) (*stores.AggregatedResult, error) {
	if len(topics) == 0 {
		return &stores.AggregatedResult{
			Columns:    []string{"timestamp"},
			Rows:       [][]any{},
			Interval:   fmt.Sprintf("%d", intervalMinutes),
			StartTime:  startTime.UTC().Format(time.RFC3339),
			EndTime:    endTime.UTC().Format(time.RFC3339),
			TopicCount: 0,
			RowCount:   0,
		}, nil
	}

	if intervalMinutes <= 0 {
		intervalMinutes = 5
	}
	if len(functions) == 0 {
		functions = []string{"AVG"}
	}

	tsCol := a.getTimestampColumn(ctx)
	columns := []string{"timestamp"}
	selectClauses := make([]string, 0)
	columnNames := make([]string, 0)
	var args []any
	paramIndex := 1

	effectiveFields := fields
	if len(effectiveFields) == 0 {
		effectiveFields = []string{""}
	}

	for _, topic := range topics {
		for _, field := range effectiveFields {
			fieldAlias := ""
			if field != "" {
				fieldAlias = fmt.Sprintf(".%s", field)
			}

			for _, fn := range functions {
				fnUpper := strings.ToUpper(fn)
				colName := fmt.Sprintf("%s%s (%s)", topic, fieldAlias, fnUpper)
				columns = append(columns, colName)
				columnNames = append(columnNames, colName)

				aggFn := fnUpper
				if aggFn != "AVG" && aggFn != "MIN" && aggFn != "MAX" && aggFn != "COUNT" {
					aggFn = "AVG"
				}

				var valExpr string
				if field != "" {
					valExpr = fmt.Sprintf("CASE WHEN topic = $%d THEN json_extract(payload_json, '$.%s')::double ELSE NULL END", paramIndex, field)
				} else {
					valExpr = fmt.Sprintf("CASE WHEN topic = $%d THEN payload_json::double ELSE NULL END", paramIndex)
				}

				selectClauses = append(selectClauses, fmt.Sprintf("%s(%s)", aggFn, valExpr))
			}
			args = append(args, topic)
			paramIndex++
		}
	}

	sampleUnit := fmt.Sprintf("%dm", intervalMinutes)
	if intervalMinutes%1440 == 0 {
		sampleUnit = fmt.Sprintf("%dd", intervalMinutes/1440)
	} else if intervalMinutes%60 == 0 {
		sampleUnit = fmt.Sprintf("%dh", intervalMinutes/60)
	}

	q := fmt.Sprintf(`
		SELECT
			to_char(%s, 'YYYY-MM-DD"T"HH24:MI:00"Z"') AS bucket,
			%s
		FROM %s
		WHERE %s >= $%d AND %s <= $%d
		SAMPLE BY %s ALIGN TO CALENDAR
	`, tsCol, strings.Join(selectClauses, ", "), a.tableName(), tsCol, paramIndex, tsCol, paramIndex+1, sampleUnit)

	args = append(args, startTime.UTC(), endTime.UTC())

	rows, err := a.db.PGPool().Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("questdb aggregated query: %w", err)
	}
	defer rows.Close()

	resultRows := make([][]any, 0)
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return nil, err
		}
		resultRows = append(resultRows, values)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return &stores.AggregatedResult{
		Columns:    columns,
		Rows:       resultRows,
		Interval:   fmt.Sprintf("%d", intervalMinutes),
		StartTime:  startTime.UTC().Format(time.RFC3339),
		EndTime:    endTime.UTC().Format(time.RFC3339),
		TopicCount: len(topics),
		RowCount:   len(resultRows),
	}, nil
}
