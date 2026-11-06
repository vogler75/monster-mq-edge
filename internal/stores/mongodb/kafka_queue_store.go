package mongodb

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"monstermq.io/edge/internal/stores"
)

// KafkaQueueStore is the MongoDB stores.KafkaQueueStore for one Kafka server
// stream: collections kafka_queue_<suffix> and kafka_offsets_<suffix>;
// offset_id comes from the shared "counters" collection (_id = queue name).
type KafkaQueueStore struct {
	db         *DB
	suffix     string
	queueName  string
	offsetName string
}

func NewKafkaQueueStore(db *DB, suffix string) *KafkaQueueStore {
	q, o := stores.KafkaQueueNames(suffix)
	return &KafkaQueueStore{db: db, suffix: suffix, queueName: q, offsetName: o}
}

var _ stores.KafkaQueueStore = (*KafkaQueueStore)(nil)

func (k *KafkaQueueStore) queue() *mongo.Collection   { return k.db.db.Collection(k.queueName) }
func (k *KafkaQueueStore) offsets() *mongo.Collection { return k.db.db.Collection(k.offsetName) }

func (k *KafkaQueueStore) Close() error { return nil }

func (k *KafkaQueueStore) EnsureTable(ctx context.Context) error {
	_, err := k.queue().Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "topic", Value: 1}, {Key: "offset_id", Value: 1}}, Options: options.Index().SetName(k.queueName + "_topic_offset_idx")},
		{Keys: bson.D{{Key: "creation_time", Value: 1}}, Options: options.Index().SetName(k.queueName + "_creation_idx")},
	})
	return err
}

// reserve atomically takes n consecutive offsets and returns the first one.
func (k *KafkaQueueStore) reserve(ctx context.Context, n int) (int64, error) {
	var res struct {
		Seq int64 `bson:"seq"`
	}
	err := k.db.db.Collection("counters").FindOneAndUpdate(ctx,
		bson.M{"_id": k.queueName},
		bson.M{"$inc": bson.M{"seq": int64(n)}},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After),
	).Decode(&res)
	if err != nil {
		return 0, err
	}
	return res.Seq - int64(n) + 1, nil
}

func (k *KafkaQueueStore) Enqueue(ctx context.Context, msgs []stores.BrokerMessage) error {
	if len(msgs) == 0 {
		return nil
	}
	first, err := k.reserve(ctx, len(msgs))
	if err != nil {
		return err
	}
	docs := make([]any, len(msgs))
	for i, m := range msgs {
		docs[i] = bson.D{
			{Key: "offset_id", Value: first + int64(i)},
			{Key: "topic", Value: m.TopicName},
			{Key: "payload", Value: bson.Binary{Data: m.Payload}},
			{Key: "qos", Value: int32(m.QoS)},
			{Key: "publisher_id", Value: m.ClientID},
			{Key: "creation_time", Value: m.Time.UnixMilli()},
			{Key: "message_uuid", Value: m.MessageUUID},
		}
	}
	_, err = k.queue().InsertMany(ctx, docs, options.InsertMany().SetOrdered(true))
	return err
}

func (k *KafkaQueueStore) Fetch(ctx context.Context, topic string, startOffset int64, limit int) ([]stores.KafkaQueueEntry, error) {
	filter := bson.M{"offset_id": bson.M{"$gte": startOffset}}
	if k.suffix == "" {
		filter["topic"] = topic
	}
	cur, err := k.queue().Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "offset_id", Value: 1}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []stores.KafkaQueueEntry
	for cur.Next(ctx) {
		var doc bson.M
		if err := cur.Decode(&doc); err != nil {
			return nil, err
		}
		e := stores.KafkaQueueEntry{Offset: kafkaInt64(doc, "offset_id")}
		e.Message.TopicName = getStr(doc, "topic")
		if b, ok := doc["payload"].(bson.Binary); ok {
			e.Message.Payload = b.Data
		}
		e.Message.QoS = byte(kafkaInt64(doc, "qos"))
		e.Message.ClientID = getStr(doc, "publisher_id")
		e.Message.MessageUUID = getStr(doc, "message_uuid")
		e.Message.IsQueued = true
		e.Message.Time = time.UnixMilli(kafkaInt64(doc, "creation_time"))
		out = append(out, e)
	}
	return out, cur.Err()
}

func kafkaInt64(d bson.M, key string) int64 {
	switch v := d[key].(type) {
	case int64:
		return v
	case int32:
		return int64(v)
	case float64:
		return int64(v)
	}
	return 0
}

func kafkaOffsetID(group, topic string, partition int) string {
	return fmt.Sprintf("%s:%s:%d", group, topic, partition)
}

func (k *KafkaQueueStore) GetOffset(ctx context.Context, groupID, topic string, partition int) (int64, bool, error) {
	var doc bson.M
	err := k.offsets().FindOne(ctx, bson.M{"_id": kafkaOffsetID(groupID, topic, partition)}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return kafkaInt64(doc, "committed_offset"), true, nil
}

func (k *KafkaQueueStore) CommitOffset(ctx context.Context, groupID, topic string, partition int, offset int64) error {
	_, err := k.offsets().UpdateOne(ctx,
		bson.M{"_id": kafkaOffsetID(groupID, topic, partition)},
		bson.M{"$set": bson.M{
			"group_id":         groupID,
			"topic":            topic,
			"partition_id":     int32(partition),
			"committed_offset": offset,
			"last_commit_time": bson.NewDateTimeFromTime(time.Now()),
		}},
		options.UpdateOne().SetUpsert(true))
	return err
}

func (k *KafkaQueueStore) edge(ctx context.Context, dir int, topic string) (int64, error) {
	filter := bson.M{}
	if k.suffix == "" {
		filter["topic"] = topic
	}
	var doc bson.M
	err := k.queue().FindOne(ctx, filter, options.FindOne().SetSort(bson.D{{Key: "offset_id", Value: dir}})).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return kafkaInt64(doc, "offset_id"), nil
}

func (k *KafkaQueueStore) GetLatestOffset(ctx context.Context, topic string) (int64, error) {
	return k.edge(ctx, -1, topic)
}

func (k *KafkaQueueStore) GetEarliestOffset(ctx context.Context, topic string) (int64, error) {
	return k.edge(ctx, 1, topic)
}

func (k *KafkaQueueStore) PruneExpired(ctx context.Context, olderThan time.Time) (int, error) {
	res, err := k.queue().DeleteMany(ctx, bson.M{"creation_time": bson.M{"$lt": olderThan.UnixMilli()}})
	if err != nil {
		return 0, err
	}
	return int(res.DeletedCount), nil
}

func (k *KafkaQueueStore) GetConsumerGroups(ctx context.Context) ([]stores.KafkaConsumerGroup, error) {
	cur, err := k.offsets().Find(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	groups := map[string]*stores.KafkaConsumerGroup{}
	for cur.Next(ctx) {
		var doc bson.M
		if err := cur.Decode(&doc); err != nil {
			return nil, err
		}
		g := getStr(doc, "group_id")
		if g == "" {
			continue
		}
		cg := groups[g]
		if cg == nil {
			cg = &stores.KafkaConsumerGroup{GroupID: g}
			groups[g] = cg
		}
		t := getStr(doc, "topic")
		dup := false
		for _, e := range cg.Topics {
			if e == t {
				dup = true
				break
			}
		}
		if !dup {
			cg.Topics = append(cg.Topics, t)
		}
		if dt, ok := doc["last_commit_time"].(bson.DateTime); ok {
			if tm := dt.Time(); tm.After(cg.LastCommitTime) {
				cg.LastCommitTime = tm
			}
		}
	}
	if err := cur.Err(); err != nil {
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
	res, err := k.offsets().DeleteMany(ctx, bson.M{"group_id": groupID})
	if err != nil {
		return false, err
	}
	return res.DeletedCount > 0, nil
}
