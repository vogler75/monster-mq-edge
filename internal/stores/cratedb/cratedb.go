// Package cratedb implements the archive-group history store against CrateDB
// through its PostgreSQL wire protocol (jackc/pgx/v5). The table layout
// mirrors the Kotlin MonsterMQ broker (MessageArchiveCrateDB.kt) so the same
// table can be read and written by either implementation.
//
// CrateDB has no transactions: BEGIN and COMMIT are accepted and ignored, so
// every batch is written with single statements and nothing is rolled back.
package cratedb

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"monstermq.io/edge/internal/stores"
)

// DefaultUser is CrateDB's built-in superuser, used when no user is given
// (the Java broker's default as well).
const DefaultUser = "crate"

// DB wraps a pgx connection pool to CrateDB.
type DB struct {
	pool *pgxpool.Pool
}

// Open connects to CrateDB. rawURL may carry the JDBC prefix of the Java
// broker config (jdbc:postgresql://host:5432/doc); username and password
// override the ones in the URL.
func Open(ctx context.Context, rawURL, username, password string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(DSN(rawURL, username, password))
	if err != nil {
		return nil, fmt.Errorf("parse cratedb url: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect cratedb: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping cratedb: %w", err)
	}
	return &DB{pool: pool}, nil
}

func (d *DB) Close() error        { d.pool.Close(); return nil }
func (d *DB) Pool() *pgxpool.Pool { return d.pool }

// DSN turns a CrateDB URL into a pgx connection string: the jdbc: prefix is
// removed, username and password replace the URL's credentials, and the user
// defaults to "crate".
func DSN(rawURL, username, password string) string {
	raw := strings.TrimPrefix(strings.TrimSpace(rawURL), "jdbc:")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return raw
	}
	if username == "" && u.User != nil {
		username = u.User.Username()
		if p, ok := u.User.Password(); ok && password == "" {
			password = p
		}
	}
	if username == "" {
		username = DefaultUser
	}
	if password != "" {
		u.User = url.UserPassword(username, password)
	} else {
		u.User = url.User(username)
	}
	return u.String()
}

// MessageArchive --------------------------------------------------------------

// insertChunk bounds the rows of one INSERT (8 parameters each).
const insertChunk = 500

type MessageArchive struct {
	name string
	db   *DB
	fmt  stores.PayloadFormat
}

func NewMessageArchive(name string, db *DB, fmt stores.PayloadFormat) *MessageArchive {
	return &MessageArchive{name: name, db: db, fmt: fmt}
}

func (a *MessageArchive) Name() string                    { return a.name }
func (a *MessageArchive) Type() stores.MessageArchiveType { return stores.ArchiveCrateDB }
func (a *MessageArchive) Close() error                    { return nil }
func (a *MessageArchive) tableName() string               { return strings.ToLower(a.name) }

func (a *MessageArchive) EnsureTable(ctx context.Context) error {
	_, err := a.db.pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
        topic VARCHAR,
        time TIMESTAMPTZ,
        payload_b64 VARCHAR INDEX OFF STORAGE WITH (columnstore = false),
        payload_obj OBJECT(IGNORED),
        qos INT,
        retained BOOLEAN,
        client_id VARCHAR(65535),
        message_uuid VARCHAR(36),
        PRIMARY KEY (topic, time)
    )`, a.tableName()))
	return err
}

// payloadColumns splits a payload into payload_b64 and payload_obj. With the
// JSON format a JSON object goes to payload_obj; anything else, including
// valid JSON that is not an object (an OBJECT column cannot hold it), is
// stored base64 in payload_b64.
func (a *MessageArchive) payloadColumns(p []byte) (b64, obj *string) {
	if a.fmt == stores.PayloadJSON && isJSONObject(p) {
		s := string(p)
		return nil, &s
	}
	s := base64.StdEncoding.EncodeToString(p)
	return &s, nil
}

func isJSONObject(p []byte) bool {
	for _, c := range p {
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			continue
		}
		if c != '{' {
			return false
		}
		break
	}
	var m map[string]any
	return json.Unmarshal(p, &m) == nil
}

func (a *MessageArchive) AddHistory(ctx context.Context, msgs []stores.BrokerMessage) error {
	for start := 0; start < len(msgs); start += insertChunk {
		end := min(start+insertChunk, len(msgs))
		q, args := a.insertSQL(msgs[start:end])
		if _, err := a.db.pool.Exec(ctx, q, args...); err != nil {
			return err
		}
	}
	return nil
}

func (a *MessageArchive) insertSQL(msgs []stores.BrokerMessage) (string, []any) {
	var sb strings.Builder
	fmt.Fprintf(&sb, `INSERT INTO %s (topic, time, payload_b64, payload_obj, qos, retained, client_id, message_uuid) VALUES `, a.tableName())
	args := make([]any, 0, len(msgs)*8)
	for i, m := range msgs {
		if i > 0 {
			sb.WriteString(", ")
		}
		n := len(args)
		fmt.Fprintf(&sb, "($%d, $%d, $%d, $%d::OBJECT, $%d, $%d, $%d, $%d)", n+1, n+2, n+3, n+4, n+5, n+6, n+7, n+8)
		b64, obj := a.payloadColumns(m.Payload)
		args = append(args, m.TopicName, m.Time.UTC(), b64, obj, int32(m.QoS), m.IsRetain, m.ClientID, m.MessageUUID)
	}
	sb.WriteString(" ON CONFLICT (topic, time) DO NOTHING")
	return sb.String(), args
}

func (a *MessageArchive) GetHistory(ctx context.Context, topic string, from, to *time.Time, limit int) ([]stores.ArchivedMessage, error) {
	if limit <= 0 {
		limit = 100
	}
	pattern := strings.ReplaceAll(strings.ReplaceAll(topic, "#", "%"), "+", "%")
	q := fmt.Sprintf(`SELECT topic, time, payload_b64, payload_obj::TEXT, qos, client_id FROM %s WHERE topic LIKE $1`, a.tableName())
	args := []any{pattern}
	if from != nil {
		q += fmt.Sprintf(` AND time >= $%d`, len(args)+1)
		args = append(args, from.UTC())
	}
	if to != nil {
		q += fmt.Sprintf(` AND time <= $%d`, len(args)+1)
		args = append(args, to.UTC())
	}
	q += fmt.Sprintf(` ORDER BY time DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit)
	rows, err := a.db.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []stores.ArchivedMessage{}
	for rows.Next() {
		var (
			t        string
			ts       time.Time
			b64, obj *string
			qos      *int32
			cid      *string
		)
		if err := rows.Scan(&t, &ts, &b64, &obj, &qos, &cid); err != nil {
			return nil, err
		}
		am := stores.ArchivedMessage{Topic: t, Timestamp: ts, Payload: decodePayload(b64, obj)}
		if qos != nil {
			am.QoS = byte(*qos)
		}
		if cid != nil {
			am.ClientID = *cid
		}
		out = append(out, am)
	}
	return out, rows.Err()
}

// decodePayload prefers the JSON object, then the base64 column.
func decodePayload(b64, obj *string) []byte {
	if obj != nil && *obj != "" {
		return []byte(*obj)
	}
	if b64 == nil {
		return nil
	}
	p, err := base64.StdEncoding.DecodeString(*b64)
	if err != nil {
		return []byte(*b64)
	}
	return p
}

func timeRange(startTime, endTime *time.Time) (string, []any) {
	where := " WHERE 1=1"
	var args []any
	if startTime != nil {
		args = append(args, startTime.UTC())
		where += fmt.Sprintf(" AND time >= $%d", len(args))
	}
	if endTime != nil {
		args = append(args, endTime.UTC())
		where += fmt.Sprintf(" AND time <= $%d", len(args))
	}
	return where, args
}

func (a *MessageArchive) GetArchiveStats(ctx context.Context, startTime, endTime *time.Time) (minTimestamp *time.Time, dailyCounts []stores.DailyCount, err error) {
	dailyCounts = []stores.DailyCount{}
	where, args := timeRange(startTime, endTime)
	if err = a.db.pool.QueryRow(ctx, fmt.Sprintf("SELECT MIN(time) FROM %s%s", a.tableName(), where), args...).Scan(&minTimestamp); err != nil {
		return nil, dailyCounts, err
	}
	rows, err := a.db.pool.Query(ctx, fmt.Sprintf(
		"SELECT DATE_TRUNC('day', time) AS day, COUNT(*) AS count FROM %s%s GROUP BY 1 ORDER BY 1 ASC", a.tableName(), where), args...)
	if err != nil {
		return minTimestamp, dailyCounts, err
	}
	defer rows.Close()
	for rows.Next() {
		var day time.Time
		var count int64
		if err := rows.Scan(&day, &count); err != nil {
			return minTimestamp, dailyCounts, err
		}
		dailyCounts = append(dailyCounts, stores.DailyCount{Date: day.UTC().Format("2006-01-02"), Count: count})
	}
	return minTimestamp, dailyCounts, rows.Err()
}

func (a *MessageArchive) PurgeOlderThan(ctx context.Context, t time.Time) (stores.PurgeResult, error) {
	res, err := a.db.pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE time < $1`, a.tableName()), t.UTC())
	if err != nil {
		return stores.PurgeResult{Err: err}, err
	}
	return stores.PurgeResult{DeletedRows: res.RowsAffected()}, nil
}

// bucketExpr is the time bucket of an aggregation, as in the Java broker.
func bucketExpr(intervalMinutes int) string {
	switch intervalMinutes {
	case 1:
		return "DATE_TRUNC('minute', time)"
	case 60:
		return "DATE_TRUNC('hour', time)"
	case 1440:
		return "DATE_TRUNC('day', time)"
	}
	return fmt.Sprintf("(DATE_TRUNC('hour', time) + (FLOOR(EXTRACT(minute FROM time) / %d) * %d) * INTERVAL '1' MINUTE)", intervalMinutes, intervalMinutes)
}

// valueExpr is the numeric value of a row: the raw payload, or a field path
// (a.b.c) inside the JSON object. decode(.., 'base64') yields hex text
// (\x3230), so encode(.., 'escape') turns it back into the payload text.
func valueExpr(field string) string {
	if field == "" {
		return "COALESCE(TRY_CAST(payload_obj AS DOUBLE), TRY_CAST(encode(decode(payload_b64, 'base64'), 'escape') AS DOUBLE))"
	}
	var sb strings.Builder
	sb.WriteString("TRY_CAST(payload_obj")
	for _, part := range strings.Split(field, ".") {
		fmt.Fprintf(&sb, "['%s']", strings.ReplaceAll(part, "'", "''"))
	}
	sb.WriteString(" AS DOUBLE)")
	return sb.String()
}

func aggFunc(fn string) string {
	switch strings.ToUpper(fn) {
	case "MIN", "MAX", "COUNT", "SUM":
		return strings.ToUpper(fn)
	}
	return "AVG"
}

func (a *MessageArchive) aggregateSQL(topics []string, startTime, endTime time.Time, intervalMinutes int, functions, fields []string) (string, []any, []string) {
	effectiveFields := fields
	if len(effectiveFields) == 0 {
		effectiveFields = []string{""}
	}
	var (
		selects []string
		columns []string
		args    []any
	)
	for _, topic := range topics {
		for _, field := range effectiveFields {
			fieldAlias := ""
			if field != "" {
				fieldAlias = "." + strings.ReplaceAll(field, ".", "_")
			}
			val := valueExpr(field)
			for _, fn := range functions {
				columns = append(columns, fmt.Sprintf("%s%s_%s", topic, fieldAlias, strings.ToLower(fn)))
				args = append(args, topic)
				selects = append(selects, fmt.Sprintf("%s(CASE WHEN topic = $%d THEN %s END)", aggFunc(fn), len(args), val))
			}
		}
	}
	in := make([]string, len(topics))
	for i, t := range topics {
		args = append(args, t)
		in[i] = fmt.Sprintf("$%d", len(args))
	}
	args = append(args, startTime.UTC(), endTime.UTC())
	q := fmt.Sprintf(`SELECT %s AS bucket, %s FROM %s WHERE topic IN (%s) AND time >= $%d AND time <= $%d GROUP BY 1 ORDER BY 1 ASC`,
		bucketExpr(intervalMinutes), strings.Join(selects, ", "), a.tableName(), strings.Join(in, ", "), len(args)-1, len(args))
	return q, args, columns
}

func (a *MessageArchive) GetAggregatedHistory(ctx context.Context, topics []string, startTime, endTime time.Time, intervalMinutes int, functions []string, fields []string) (*stores.AggregatedResult, error) {
	res := &stores.AggregatedResult{
		Columns:   []string{"timestamp"},
		Rows:      [][]any{},
		StartTime: startTime.UTC().Format(time.RFC3339),
		EndTime:   endTime.UTC().Format(time.RFC3339),
	}
	if intervalMinutes <= 0 {
		intervalMinutes = 5
	}
	res.Interval = fmt.Sprintf("%d", intervalMinutes)
	if len(topics) == 0 {
		return res, nil
	}
	if len(functions) == 0 {
		functions = []string{"AVG"}
	}
	q, args, columns := a.aggregateSQL(topics, startTime, endTime, intervalMinutes, functions, fields)
	res.Columns = append(res.Columns, columns...)
	rows, err := a.db.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var bucket time.Time
		vals := make([]*float64, len(columns))
		targets := make([]any, len(columns)+1)
		targets[0] = &bucket
		for i := range vals {
			targets[i+1] = &vals[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		row := make([]any, len(columns)+1)
		row[0] = bucket.UTC().Format("2006-01-02T15:04:05Z")
		for i, v := range vals {
			if v != nil {
				row[i+1] = *v
			}
		}
		res.Rows = append(res.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	res.TopicCount = len(topics)
	res.RowCount = len(res.Rows)
	return res, nil
}
