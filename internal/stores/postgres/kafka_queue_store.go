package postgres

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"monstermq.io/edge/internal/stores"
)

// KafkaQueueStore is the PostgreSQL stores.KafkaQueueStore for one Kafka
// server stream: tables kafka_queue_<suffix> and kafka_offsets_<suffix>.
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
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS ` + k.queueTable + ` (
            offset_id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
            topic TEXT NOT NULL,
            payload BYTEA,
            qos INT NOT NULL DEFAULT 1,
            publisher_id VARCHAR(65535),
            creation_time BIGINT NOT NULL,
            message_uuid VARCHAR(36) NOT NULL
        )`,
		`CREATE TABLE IF NOT EXISTS ` + k.offsetTable + ` (
            group_id VARCHAR(255) NOT NULL,
            topic TEXT NOT NULL,
            partition_id INT NOT NULL DEFAULT 0,
            committed_offset BIGINT NOT NULL,
            last_commit_time TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now(),
            PRIMARY KEY (group_id, topic, partition_id)
        )`,
		`CREATE INDEX IF NOT EXISTS ` + k.queueTable + `_topic_offset_idx ON ` + k.queueTable + ` (topic, offset_id ASC)`,
		`CREATE INDEX IF NOT EXISTS ` + k.queueTable + `_creation_time_idx ON ` + k.queueTable + ` (creation_time)`,
	} {
		if _, err := k.db.pool.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func (k *KafkaQueueStore) Enqueue(ctx context.Context, msgs []stores.BrokerMessage) error {
	if len(msgs) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, m := range msgs {
		batch.Queue(`INSERT INTO `+k.queueTable+
			` (topic, payload, qos, publisher_id, creation_time, message_uuid) VALUES ($1, $2, $3, $4, $5, $6)`,
			m.TopicName, m.Payload, int(m.QoS), m.ClientID, m.Time.UnixMilli(), m.MessageUUID)
	}
	tx, err := k.db.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	br := tx.SendBatch(ctx, batch)
	for range msgs {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return err
		}
	}
	if err := br.Close(); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (k *KafkaQueueStore) Fetch(ctx context.Context, topic string, startOffset int64, limit int) ([]stores.KafkaQueueEntry, error) {
	q := `SELECT offset_id, topic, payload, qos, COALESCE(publisher_id, ''), creation_time, message_uuid FROM ` + k.queueTable
	var args []any
	if k.suffix == "" {
		q += ` WHERE topic = $1 AND offset_id >= $2 ORDER BY offset_id ASC LIMIT $3`
		args = []any{topic, startOffset, limit}
	} else {
		q += ` WHERE offset_id >= $1 ORDER BY offset_id ASC LIMIT $2`
		args = []any{startOffset, limit}
	}
	rows, err := k.db.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []stores.KafkaQueueEntry
	for rows.Next() {
		var (
			e       stores.KafkaQueueEntry
			qos     int32
			created int64
		)
		if err := rows.Scan(&e.Offset, &e.Message.TopicName, &e.Message.Payload, &qos, &e.Message.ClientID, &created, &e.Message.MessageUUID); err != nil {
			return nil, err
		}
		e.Message.QoS = byte(qos)
		e.Message.IsQueued = true
		e.Message.Time = time.UnixMilli(created)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (k *KafkaQueueStore) GetOffset(ctx context.Context, groupID, topic string, partition int) (int64, bool, error) {
	var off int64
	err := k.db.pool.QueryRow(ctx, `SELECT committed_offset FROM `+k.offsetTable+
		` WHERE group_id = $1 AND topic = $2 AND partition_id = $3`, groupID, topic, partition).Scan(&off)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return off, true, nil
}

func (k *KafkaQueueStore) CommitOffset(ctx context.Context, groupID, topic string, partition int, offset int64) error {
	_, err := k.db.pool.Exec(ctx, `INSERT INTO `+k.offsetTable+
		` (group_id, topic, partition_id, committed_offset, last_commit_time) VALUES ($1, $2, $3, $4, now())
        ON CONFLICT (group_id, topic, partition_id) DO UPDATE SET
            committed_offset = excluded.committed_offset,
            last_commit_time = now()`, groupID, topic, partition, offset)
	return err
}

func (k *KafkaQueueStore) aggregate(ctx context.Context, fn, topic string) (int64, error) {
	q := `SELECT ` + fn + `(offset_id) FROM ` + k.queueTable
	var args []any
	if k.suffix == "" {
		q += ` WHERE topic = $1`
		args = append(args, topic)
	}
	var v *int64
	if err := k.db.pool.QueryRow(ctx, q, args...).Scan(&v); err != nil {
		return 0, err
	}
	if v == nil {
		return 0, nil
	}
	return *v, nil
}

func (k *KafkaQueueStore) GetLatestOffset(ctx context.Context, topic string) (int64, error) {
	return k.aggregate(ctx, "MAX", topic)
}

func (k *KafkaQueueStore) GetEarliestOffset(ctx context.Context, topic string) (int64, error) {
	return k.aggregate(ctx, "MIN", topic)
}

func (k *KafkaQueueStore) PruneExpired(ctx context.Context, olderThan time.Time) (int, error) {
	tag, err := k.db.pool.Exec(ctx, `DELETE FROM `+k.queueTable+` WHERE creation_time < $1`, olderThan.UnixMilli())
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (k *KafkaQueueStore) GetConsumerGroups(ctx context.Context) ([]stores.KafkaConsumerGroup, error) {
	rows, err := k.db.pool.Query(ctx, `SELECT group_id, topic, MAX(last_commit_time) FROM `+k.offsetTable+` GROUP BY group_id, topic`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := map[string]*stores.KafkaConsumerGroup{}
	for rows.Next() {
		var g, t string
		var ts time.Time
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
		if ts.After(cg.LastCommitTime) {
			cg.LastCommitTime = ts
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
	tag, err := k.db.pool.Exec(ctx, `DELETE FROM `+k.offsetTable+` WHERE group_id = $1`, groupID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
