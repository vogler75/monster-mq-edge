package sqlite

import (
	"context"
	"database/sql"
	"sort"
	"time"

	"monstermq.io/edge/internal/stores"
)

// KafkaQueueStore is the SQLite stores.KafkaQueueStore for one Kafka server
// stream: tables kafka_queue_<suffix> and kafka_offsets_<suffix>.
type KafkaQueueStore struct {
	db          *DB
	suffix      string
	queueTable  string
	offsetTable string
}

func NewKafkaQueueStore(db *DB, suffix string) *KafkaQueueStore {
	q, o := stores.KafkaQueueNames(suffix)
	return &KafkaQueueStore{db: db, suffix: suffix, queueTable: q, offsetTable: o}
}

var _ stores.KafkaQueueStore = (*KafkaQueueStore)(nil)

func (k *KafkaQueueStore) Close() error { return nil }

func (k *KafkaQueueStore) EnsureTable(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS ` + k.queueTable + ` (
            offset_id INTEGER PRIMARY KEY AUTOINCREMENT,
            topic TEXT NOT NULL,
            payload BLOB,
            qos INTEGER NOT NULL DEFAULT 1,
            publisher_id TEXT,
            creation_time INTEGER NOT NULL,
            message_uuid TEXT NOT NULL
        )`,
		`CREATE INDEX IF NOT EXISTS ` + k.queueTable + `_topic_offset_idx ON ` + k.queueTable + ` (topic, offset_id ASC)`,
		`CREATE INDEX IF NOT EXISTS ` + k.queueTable + `_creation_idx ON ` + k.queueTable + ` (creation_time)`,
		`CREATE TABLE IF NOT EXISTS ` + k.offsetTable + ` (
            group_id TEXT NOT NULL,
            topic TEXT NOT NULL,
            partition_id INTEGER NOT NULL DEFAULT 0,
            committed_offset INTEGER NOT NULL,
            last_commit_time INTEGER NOT NULL DEFAULT (strftime('%s', 'now')),
            PRIMARY KEY (group_id, topic, partition_id)
        )`,
	}
	for _, s := range stmts {
		if _, err := k.db.Exec(s); err != nil {
			return err
		}
	}
	return nil
}

func (k *KafkaQueueStore) Enqueue(ctx context.Context, msgs []stores.BrokerMessage) error {
	if len(msgs) == 0 {
		return nil
	}
	k.db.Lock()
	defer k.db.Unlock()
	tx, err := k.db.Conn().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO `+k.queueTable+
		` (topic, payload, qos, publisher_id, creation_time, message_uuid) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, m := range msgs {
		if _, err := stmt.ExecContext(ctx, m.TopicName, m.Payload, int(m.QoS), m.ClientID, m.Time.UnixMilli(), m.MessageUUID); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (k *KafkaQueueStore) Fetch(ctx context.Context, topic string, startOffset int64, limit int) ([]stores.KafkaQueueEntry, error) {
	q := `SELECT offset_id, topic, payload, qos, publisher_id, creation_time, message_uuid FROM ` + k.queueTable
	var args []any
	if k.suffix == "" {
		q += ` WHERE topic = ? AND offset_id >= ?`
		args = append(args, topic, startOffset)
	} else {
		q += ` WHERE offset_id >= ?`
		args = append(args, startOffset)
	}
	q += ` ORDER BY offset_id ASC LIMIT ?`
	args = append(args, limit)
	rows, err := k.db.Conn().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []stores.KafkaQueueEntry
	for rows.Next() {
		var (
			e         stores.KafkaQueueEntry
			payload   []byte
			qos       int
			publisher sql.NullString
			created   int64
		)
		if err := rows.Scan(&e.Offset, &e.Message.TopicName, &payload, &qos, &publisher, &created, &e.Message.MessageUUID); err != nil {
			return nil, err
		}
		e.Message.Payload = payload
		e.Message.QoS = byte(qos)
		e.Message.ClientID = publisher.String
		e.Message.IsQueued = true
		e.Message.Time = time.UnixMilli(created)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (k *KafkaQueueStore) GetOffset(ctx context.Context, groupID, topic string, partition int) (int64, bool, error) {
	var off int64
	err := k.db.Conn().QueryRowContext(ctx, `SELECT committed_offset FROM `+k.offsetTable+
		` WHERE group_id = ? AND topic = ? AND partition_id = ?`, groupID, topic, partition).Scan(&off)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return off, true, nil
}

func (k *KafkaQueueStore) CommitOffset(ctx context.Context, groupID, topic string, partition int, offset int64) error {
	k.db.Lock()
	defer k.db.Unlock()
	_, err := k.db.Conn().ExecContext(ctx, `INSERT INTO `+k.offsetTable+
		` (group_id, topic, partition_id, committed_offset, last_commit_time) VALUES (?, ?, ?, ?, ?)
        ON CONFLICT (group_id, topic, partition_id) DO UPDATE SET
            committed_offset = excluded.committed_offset,
            last_commit_time = excluded.last_commit_time`,
		groupID, topic, partition, offset, time.Now().Unix())
	return err
}

func (k *KafkaQueueStore) aggregate(ctx context.Context, fn, topic string) (int64, error) {
	q := `SELECT ` + fn + `(offset_id) FROM ` + k.queueTable
	var args []any
	if k.suffix == "" {
		q += ` WHERE topic = ?`
		args = append(args, topic)
	}
	var v sql.NullInt64
	if err := k.db.Conn().QueryRowContext(ctx, q, args...).Scan(&v); err != nil {
		return 0, err
	}
	return v.Int64, nil
}

func (k *KafkaQueueStore) GetLatestOffset(ctx context.Context, topic string) (int64, error) {
	return k.aggregate(ctx, "MAX", topic)
}

func (k *KafkaQueueStore) GetEarliestOffset(ctx context.Context, topic string) (int64, error) {
	return k.aggregate(ctx, "MIN", topic)
}

func (k *KafkaQueueStore) PruneExpired(ctx context.Context, olderThan time.Time) (int, error) {
	k.db.Lock()
	defer k.db.Unlock()
	res, err := k.db.Conn().ExecContext(ctx, `DELETE FROM `+k.queueTable+` WHERE creation_time < ?`, olderThan.UnixMilli())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (k *KafkaQueueStore) GetConsumerGroups(ctx context.Context) ([]stores.KafkaConsumerGroup, error) {
	rows, err := k.db.Conn().QueryContext(ctx, `SELECT group_id, topic, MAX(last_commit_time) FROM `+k.offsetTable+` GROUP BY group_id, topic`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := map[string]*stores.KafkaConsumerGroup{}
	for rows.Next() {
		var g, t string
		var ts int64
		if err := rows.Scan(&g, &t, &ts); err != nil {
			return nil, err
		}
		if g == "" {
			continue
		}
		cg := groups[g]
		if cg == nil {
			cg = &stores.KafkaConsumerGroup{GroupID: g}
			groups[g] = cg
		}
		cg.Topics = append(cg.Topics, t)
		if tm := time.Unix(ts, 0); tm.After(cg.LastCommitTime) {
			cg.LastCommitTime = tm
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]stores.KafkaConsumerGroup, 0, len(groups))
	for _, g := range groups {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GroupID < out[j].GroupID })
	return out, nil
}

func (k *KafkaQueueStore) DeleteConsumerGroup(ctx context.Context, groupID string) (bool, error) {
	k.db.Lock()
	defer k.db.Unlock()
	res, err := k.db.Conn().ExecContext(ctx, `DELETE FROM `+k.offsetTable+` WHERE group_id = ?`, groupID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
