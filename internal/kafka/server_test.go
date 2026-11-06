package kafka

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"golang.org/x/crypto/bcrypt"

	"monstermq.io/edge/internal/auth"
	"monstermq.io/edge/internal/kafka/wire"
	"monstermq.io/edge/internal/pubsub"
	"monstermq.io/edge/internal/stores"
)

// ---- fakes ----

type fakeStore struct {
	mu      sync.Mutex
	next    int64
	entries []stores.KafkaQueueEntry
	offsets map[string]int64 // group|topic|partition
	commit  map[string]time.Time
	failEnq bool
	closed  bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{offsets: map[string]int64{}, commit: map[string]time.Time{}}
}

func (f *fakeStore) Enqueue(_ context.Context, msgs []stores.BrokerMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failEnq {
		return fmt.Errorf("disk full")
	}
	for _, m := range msgs {
		f.next++
		f.entries = append(f.entries, stores.KafkaQueueEntry{Offset: f.next, Message: m})
	}
	return nil
}

func (f *fakeStore) Fetch(_ context.Context, _ string, start int64, limit int) ([]stores.KafkaQueueEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []stores.KafkaQueueEntry
	for _, e := range f.entries {
		if e.Offset >= start && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func okey(g, t string, p int) string { return fmt.Sprintf("%s|%s|%d", g, t, p) }

func (f *fakeStore) GetOffset(_ context.Context, g, t string, p int) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.offsets[okey(g, t, p)]
	return v, ok, nil
}

func (f *fakeStore) CommitOffset(_ context.Context, g, t string, p int, off int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offsets[okey(g, t, p)] = off
	f.commit[g] = time.Now()
	return nil
}

func (f *fakeStore) GetLatestOffset(context.Context, string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.entries) == 0 {
		return 0, nil
	}
	return f.entries[len(f.entries)-1].Offset, nil
}

func (f *fakeStore) GetEarliestOffset(context.Context, string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.entries) == 0 {
		return 0, nil
	}
	return f.entries[0].Offset, nil
}

func (f *fakeStore) PruneExpired(_ context.Context, olderThan time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keep []stores.KafkaQueueEntry
	n := 0
	for _, e := range f.entries {
		if e.Message.Time.Before(olderThan) {
			n++
		} else {
			keep = append(keep, e)
		}
	}
	f.entries = keep
	return n, nil
}

func (f *fakeStore) GetConsumerGroups(context.Context) ([]stores.KafkaConsumerGroup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	byGroup := map[string]map[string]bool{}
	for k := range f.offsets {
		parts := splitKey(k)
		g, t := parts[0], parts[1]
		if byGroup[g] == nil {
			byGroup[g] = map[string]bool{}
		}
		byGroup[g][t] = true
	}
	var out []stores.KafkaConsumerGroup
	for g, ts := range byGroup {
		cg := stores.KafkaConsumerGroup{GroupID: g, LastCommitTime: f.commit[g]}
		for t := range ts {
			cg.Topics = append(cg.Topics, t)
		}
		out = append(out, cg)
	}
	return out, nil
}

func splitKey(k string) []string {
	var parts []string
	cur := ""
	for _, r := range k {
		if r == '|' {
			parts = append(parts, cur)
			cur = ""
		} else {
			cur += string(r)
		}
	}
	return append(parts, cur)
}

func (f *fakeStore) DeleteConsumerGroup(_ context.Context, g string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	found := false
	for k := range f.offsets {
		if splitKey(k)[0] == g {
			delete(f.offsets, k)
			found = true
		}
	}
	return found, nil
}

func (f *fakeStore) EnsureTable(context.Context) error { return nil }
func (f *fakeStore) Close() error                      { f.closed = true; return nil }

func (f *fakeStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.entries)
}

type env struct {
	srv    *Server
	bus    *pubsub.Bus
	stores map[string]*fakeStore // by table suffix
	mu     sync.Mutex
	pub    []stores.BrokerMessage
}

func (e *env) published() []stores.BrokerMessage {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]stores.BrokerMessage(nil), e.pub...)
}

func ptr[T any](v T) *T { return &v }

func startServer(t *testing.T, cfg ServerConfig, mod func(*Deps)) *env {
	t.Helper()
	e := &env{bus: pubsub.NewBus(), stores: map[string]*fakeStore{}}
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.Name == "" {
		cfg.Name = "test"
	}
	if cfg.Port == 0 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		cfg.Port = ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()
	}
	for i := range cfg.Streams {
		if cfg.Streams[i].RetentionHours == 0 {
			cfg.Streams[i].RetentionHours = 168
		}
	}
	deps := Deps{
		Bus: e.bus,
		// the real MQTT engine delivers a publish to bus subscribers
		Publish: func(clientID string, msg stores.BrokerMessage) error {
			e.mu.Lock()
			e.pub = append(e.pub, msg)
			e.mu.Unlock()
			e.bus.Publish(msg)
			return nil
		},
		NewStore: func(_ context.Context, _ *string, suffix string) (stores.KafkaQueueStore, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			if s, ok := e.stores[suffix]; ok {
				return s, nil
			}
			s := newFakeStore()
			e.stores[suffix] = s
			return s, nil
		},
	}
	if mod != nil {
		mod(&deps)
	}
	srv, err := New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	e.srv = srv
	t.Cleanup(srv.Stop)
	return e
}

func sensorsCfg() ServerConfig {
	return ServerConfig{Streams: []StreamMapping{{StreamName: "sensors", TopicFilter: "sensors/#", AllowWrite: true}}}
}

func newClient(t *testing.T, e *env, opts ...kgo.Opt) *kgo.Client {
	t.Helper()
	base := []kgo.Opt{
		kgo.SeedBrokers(fmt.Sprintf("127.0.0.1:%d", e.srv.Port())),
		kgo.DisableIdempotentWrite(),
		kgo.RecordPartitioner(kgo.ManualPartitioner()),
		kgo.WithLogger(nil),
	}
	cl, err := kgo.NewClient(append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cl.Close)
	return cl
}

func consume(t *testing.T, cl *kgo.Client, n int, timeout time.Duration) []*kgo.Record {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var got []*kgo.Record
	for len(got) < n {
		fs := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			t.Fatalf("timeout after %v: got %d/%d records (errs %v)", timeout, len(got), n, fs.Errors())
		}
		fs.EachRecord(func(r *kgo.Record) { got = append(got, r) })
	}
	return got
}

// ---- tests ----

func TestProduceFetchStoresOnce(t *testing.T) {
	e := startServer(t, sensorsCfg(), nil)
	_, ch := e.bus.Subscribe([]string{"sensors/#"}, 16)

	p := newClient(t, e, kgo.ProducerBatchCompression(kgo.GzipCompression()))
	res := p.ProduceSync(context.Background(), &kgo.Record{Topic: "sensors", Key: []byte("sensors/t1"), Value: []byte("21.5")})
	if err := res.FirstErr(); err != nil {
		t.Fatal(err)
	}
	// MQTT side saw it exactly once, at QoS 1 from the kafka producer.
	select {
	case m := <-ch:
		if m.TopicName != "sensors/t1" || string(m.Payload) != "21.5" || m.QoS != 1 {
			t.Fatalf("mqtt message %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no MQTT publish")
	}
	// Give the orchestrator several flush intervals: it must not store the echo.
	time.Sleep(5 * flushInterval)
	if n := e.stores["sensors"].count(); n != 1 {
		t.Fatalf("queue holds %d entries, want exactly 1 (double storage)", n)
	}

	c := newClient(t, e, kgo.ConsumeTopics("sensors"), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	recs := consume(t, c, 1, 5*time.Second)
	if string(recs[0].Key) != "sensors/t1" || string(recs[0].Value) != "21.5" {
		t.Fatalf("record %+v", recs[0])
	}
	if n := len(e.srv.selfPubKeys()); n != 0 {
		t.Fatalf("selfPub leak: %d", n)
	}
}

func (s *Server) selfPubKeys() []string {
	var k []string
	s.selfPub.Range(func(key, _ any) bool { k = append(k, key.(string)); return true })
	return k
}

func TestMQTTToKafkaWakesLongPoll(t *testing.T) {
	e := startServer(t, sensorsCfg(), nil)
	c := newClient(t, e, kgo.ConsumeTopics("sensors"), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.FetchMaxWait(10*time.Second))

	got := make(chan []*kgo.Record, 1)
	go func() { got <- consume(t, c, 1, 8*time.Second) }()
	time.Sleep(500 * time.Millisecond) // consumer is parked in the long-poll

	start := time.Now()
	// The MQTT topic differs from the Kafka topic name: the wake-up must be keyed by stream.
	e.bus.Publish(stores.BrokerMessage{MessageUUID: "u1", TopicName: "sensors/room/temp", Payload: []byte("19"), QoS: 0, Time: time.Now()})
	recs := <-got
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("long-poll woke after %v (waited out max_wait)", d)
	}
	if string(recs[0].Key) != "sensors/room/temp" || string(recs[0].Value) != "19" {
		t.Fatalf("record %+v", recs[0])
	}
	// a message not matching any filter is ignored
	e.bus.Publish(stores.BrokerMessage{MessageUUID: "u2", TopicName: "other/x", Payload: []byte("1")})
	time.Sleep(3 * flushInterval)
	if n := e.stores["sensors"].count(); n != 1 {
		t.Fatalf("count=%d", n)
	}
}

func TestOffsetsCommitFetchAndGroups(t *testing.T) {
	e := startServer(t, sensorsCfg(), nil)
	for i := 0; i < 3; i++ {
		e.bus.Publish(stores.BrokerMessage{MessageUUID: fmt.Sprint("m", i), TopicName: "sensors/a", Payload: []byte("x")})
	}
	time.Sleep(3 * flushInterval)
	cl := newClient(t, e)
	ctx := context.Background()

	of := kmsg.NewPtrOffsetFetchRequest()
	of.Group = "g1"
	of.Topics = []kmsg.OffsetFetchRequestTopic{{Topic: "sensors", Partitions: []int32{0}}}
	resp, err := cl.Request(ctx, of)
	if err != nil {
		t.Fatal(err)
	}
	if o := resp.(*kmsg.OffsetFetchResponse).Topics[0].Partitions[0].Offset; o != -1 {
		t.Fatalf("uncommitted offset = %d, want -1", o)
	}

	oc := kmsg.NewPtrOffsetCommitRequest()
	oc.Group = "g1"
	oc.Generation = -1
	oc.Topics = []kmsg.OffsetCommitRequestTopic{{Topic: "sensors", Partitions: []kmsg.OffsetCommitRequestTopicPartition{{Partition: 0, Offset: 2}}}}
	cresp, err := cl.Request(ctx, oc)
	if err != nil {
		t.Fatal(err)
	}
	if code := cresp.(*kmsg.OffsetCommitResponse).Topics[0].Partitions[0].ErrorCode; code != 0 {
		t.Fatalf("commit error %d", code)
	}
	resp, err = cl.Request(ctx, of)
	if err != nil {
		t.Fatal(err)
	}
	if o := resp.(*kmsg.OffsetFetchResponse).Topics[0].Partitions[0].Offset; o != 2 {
		t.Fatalf("committed offset = %d, want 2", o)
	}

	groups, err := e.srv.ConsumerGroups(ctx)
	if err != nil || len(groups) != 1 || groups[0].GroupID != "g1" || groups[0].Lag != 2 { // offsets 2,3 pending
		t.Fatalf("groups=%+v err=%v", groups, err)
	}
	if ok, _ := e.srv.DeleteConsumerGroup(ctx, "g1"); !ok {
		t.Fatal("delete group")
	}
	if g, _ := e.srv.ConsumerGroups(ctx); len(g) != 0 {
		t.Fatalf("groups after delete: %+v", g)
	}

	early, latest, ok, err := e.srv.TopicOffsets(ctx, "sensors")
	if err != nil || !ok || early != 1 || latest != 3 {
		t.Fatalf("offsets %d %d %v %v", early, latest, ok, err)
	}
	msgs, err := e.srv.Messages(ctx, "sensors", -1, 2)
	if err != nil || len(msgs) != 2 || msgs[0].Offset != 2 || *msgs[1].Key != "sensors/a" || *msgs[1].Value != "x" {
		t.Fatalf("messages %+v err=%v", msgs, err)
	}
}

func TestListOffsetsAndLegacyAPIs(t *testing.T) {
	e := startServer(t, sensorsCfg(), nil)
	e.bus.Publish(stores.BrokerMessage{MessageUUID: "a", TopicName: "sensors/a", Payload: []byte("1")})
	e.bus.Publish(stores.BrokerMessage{MessageUUID: "b", TopicName: "sensors/b", Payload: []byte("2")})
	time.Sleep(3 * flushInterval)
	cl := newClient(t, e)
	for _, c := range []struct {
		ts   int64
		want int64
	}{{-1, 3}, {-2, 1}} {
		req := kmsg.NewPtrListOffsetsRequest()
		req.Topics = []kmsg.ListOffsetsRequestTopic{{Topic: "sensors", Partitions: []kmsg.ListOffsetsRequestTopicPartition{{Partition: 0, Timestamp: c.ts}}}}
		resp, err := cl.Request(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if o := resp.(*kmsg.ListOffsetsResponse).Topics[0].Partitions[0].Offset; o != c.want {
			t.Fatalf("ts %d: offset %d want %d", c.ts, o, c.want)
		}
	}
}

func TestProduceRejectedByStreamConfig(t *testing.T) {
	cfg := ServerConfig{Streams: []StreamMapping{
		{StreamName: "ro", TopicFilter: "ro/#", AllowWrite: false},
		{StreamName: "rw", TopicFilter: "rw/#", AllowWrite: true},
	}}
	e := startServer(t, cfg, nil)
	p := newClient(t, e)
	ctx := context.Background()
	if err := p.ProduceSync(ctx, &kgo.Record{Topic: "ro", Key: []byte("ro/x"), Value: []byte("1")}).FirstErr(); err == nil {
		t.Fatal("write to read-only stream accepted")
	}
	if err := p.ProduceSync(ctx, &kgo.Record{Topic: "rw", Key: []byte("other/x"), Value: []byte("1")}).FirstErr(); err == nil {
		t.Fatal("key outside the stream filter accepted")
	}
	if err := p.ProduceSync(ctx, &kgo.Record{Topic: "rw", Key: []byte("rw/x"), Value: []byte("1")}).FirstErr(); err != nil {
		t.Fatal(err)
	}
	if e.stores["ro"].count() != 0 || e.stores["rw"].count() != 1 {
		t.Fatalf("counts ro=%d rw=%d", e.stores["ro"].count(), e.stores["rw"].count())
	}
	if len(e.published()) != 1 {
		t.Fatalf("published %d, rejected records must not reach MQTT", len(e.published()))
	}
}

func TestProduceStorageFailureIsReported(t *testing.T) {
	e := startServer(t, sensorsCfg(), nil)
	e.stores["sensors"].failEnq = true
	p := newClient(t, e, kgo.RecordRetries(0))
	err := p.ProduceSync(context.Background(), &kgo.Record{Topic: "sensors", Key: []byte("sensors/x"), Value: []byte("1")}).FirstErr()
	if err == nil {
		t.Fatal("storage failure answered with success")
	}
	if len(e.published()) != 0 {
		t.Fatal("a record that was not stored must not be published")
	}
}

func TestProduceToTopicWithoutStreamKey(t *testing.T) {
	e := startServer(t, sensorsCfg(), nil)
	p := newClient(t, e)
	// null key -> the Kafka topic is the MQTT topic; it matches no stream -> published only
	if err := p.ProduceSync(context.Background(), &kgo.Record{Topic: "free/topic", Value: []byte("v")}).FirstErr(); err != nil {
		t.Fatal(err)
	}
	pub := e.published()
	if len(pub) != 1 || pub[0].TopicName != "free/topic" {
		t.Fatalf("published %+v", pub)
	}
}

func TestDeterministicRoutingAndPerStreamStores(t *testing.T) {
	cfg := ServerConfig{Streams: []StreamMapping{
		{StreamName: "first", TopicFilter: "a/#", AllowWrite: true},
		{StreamName: "second", TopicFilter: "a/b/#", AllowWrite: true},
		{StreamName: "other.Stream-1", TopicFilter: "z/+", AllowWrite: true},
	}}
	e := startServer(t, cfg, nil)
	for i := 0; i < 5; i++ {
		e.bus.Publish(stores.BrokerMessage{MessageUUID: fmt.Sprint(i), TopicName: "a/b/c", Payload: []byte("x")})
	}
	e.bus.Publish(stores.BrokerMessage{MessageUUID: "z", TopicName: "z/1", Payload: []byte("x")})
	time.Sleep(3 * flushInterval)
	if e.stores["first"].count() != 5 || e.stores["second"].count() != 0 {
		t.Fatalf("first=%d second=%d (config order must win)", e.stores["first"].count(), e.stores["second"].count())
	}
	if _, ok := e.stores["other_stream_1"]; !ok || e.stores["other_stream_1"].count() != 1 {
		t.Fatalf("suffix table naming: %v", e.stores)
	}
	if TableSuffix("!!") != "default" || TableSuffix("sensors/Temp") != "sensors_temp" {
		t.Fatal("TableSuffix")
	}
}

func TestPerStreamRetention(t *testing.T) {
	cfg := ServerConfig{Streams: []StreamMapping{
		{StreamName: "short", TopicFilter: "s/#", RetentionHours: 1, AllowWrite: true},
		{StreamName: "long", TopicFilter: "l/#", RetentionHours: 1000, AllowWrite: true},
	}}
	e := startServer(t, cfg, nil)
	old := time.Now().Add(-5 * time.Hour)
	for _, name := range []string{"short", "long"} {
		_ = e.stores[name].Enqueue(context.Background(), []stores.BrokerMessage{{TopicName: name + "/x", Payload: []byte("1"), Time: old}})
	}
	e.srv.orch.prune(context.Background())
	if e.stores["short"].count() != 0 || e.stores["long"].count() != 1 {
		t.Fatalf("short=%d long=%d: each stream must use its own retention", e.stores["short"].count(), e.stores["long"].count())
	}
}

// ---- SASL / ACL ----

type userStore struct {
	stores.UserStore
	users []stores.User
	rules []stores.AclRule
}

func (u userStore) LoadAll(context.Context) ([]stores.User, []stores.AclRule, error) {
	return u.users, u.rules, nil
}

func authedEnv(t *testing.T) *env {
	t.Helper()
	hash, _ := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	cache := auth.NewCache(userStore{
		users: []stores.User{{Username: "alice", PasswordHash: string(hash), Enabled: true, CanPublish: true, CanSubscribe: true}},
		rules: []stores.AclRule{{ID: "1", Username: "alice", TopicPattern: "sensors/#", CanPublish: true, CanSubscribe: true}},
	}, false, true)
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return startServer(t, sensorsCfg(), func(d *Deps) { d.UserManagementEnabled = true; d.AuthCache = cache })
}

func TestSaslPlainAndACL(t *testing.T) {
	e := authedEnv(t)
	ctx := context.Background()

	short, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	anon := newClient(t, e, kgo.RecordDeliveryTimeout(2*time.Second))
	if err := anon.ProduceSync(short, &kgo.Record{Topic: "sensors", Key: []byte("sensors/a"), Value: []byte("1")}).FirstErr(); err == nil {
		t.Fatal("unauthenticated produce accepted")
	}
	bad := newClient(t, e, kgo.RecordDeliveryTimeout(2*time.Second), kgo.SASL(plain.Auth{User: "alice", Pass: "wrong"}.AsMechanism()))
	if err := bad.ProduceSync(short, &kgo.Record{Topic: "sensors", Key: []byte("sensors/a"), Value: []byte("1")}).FirstErr(); err == nil {
		t.Fatal("wrong password accepted")
	}
	good := newClient(t, e, kgo.SASL(plain.Auth{User: "alice", Pass: "secret"}.AsMechanism()))
	if err := good.ProduceSync(ctx, &kgo.Record{Topic: "sensors", Key: []byte("sensors/a"), Value: []byte("1")}).FirstErr(); err != nil {
		t.Fatal(err)
	}
	// ACL: alice may only publish below sensors/
	if err := good.ProduceSync(ctx, &kgo.Record{Topic: "free", Key: []byte("secret/x"), Value: []byte("1")}).FirstErr(); err == nil {
		t.Fatal("publish outside ACL accepted")
	}
	c := newClient(t, e, kgo.SASL(plain.Auth{User: "alice", Pass: "secret"}.AsMechanism()),
		kgo.ConsumeTopics("sensors"), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if recs := consume(t, c, 1, 5*time.Second); string(recs[0].Key) != "sensors/a" {
		t.Fatalf("%+v", recs[0])
	}
}

// ---- raw protocol checks ----

type rawConn struct {
	t  *testing.T
	nc net.Conn
}

func dialRaw(t *testing.T, e *env) *rawConn {
	nc, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", e.srv.Port()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	return &rawConn{t: t, nc: nc}
}

func (c *rawConn) send(apiKey, version int16, corr int32, body func(w *wire.Writer)) {
	w := wire.NewWriter()
	w.Int16(apiKey)
	w.Int16(version)
	w.Int32(corr)
	w.String("raw")
	if body != nil {
		body(w)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(w.Len()))
	if _, err := c.nc.Write(append(hdr[:], w.Bytes()...)); err != nil {
		c.t.Fatal(err)
	}
}

func (c *rawConn) recv() *wire.Reader {
	_ = c.nc.SetReadDeadline(time.Now().Add(3 * time.Second))
	var hdr [4]byte
	if _, err := io.ReadFull(c.nc, hdr[:]); err != nil {
		c.t.Fatalf("read: %v", err)
	}
	b := make([]byte, binary.BigEndian.Uint32(hdr[:]))
	if _, err := io.ReadFull(c.nc, b); err != nil {
		c.t.Fatalf("read body: %v", err)
	}
	return wire.NewReader(b)
}

func TestApiVersionsAndMetadataLayout(t *testing.T) {
	e := startServer(t, sensorsCfg(), func(d *Deps) {})
	c := dialRaw(t, e)

	c.send(wire.APIApiVersions, 0, 11, nil)
	r := c.recv()
	if r.Int32() != 11 || r.Int16() != 0 || r.Int32() != 15 {
		t.Fatal("ApiVersions v0 header")
	}
	max := map[int16]int16{}
	for i := 0; i < 15; i++ {
		k, min, mx := r.Int16(), r.Int16(), r.Int16()
		if min != 0 {
			t.Fatalf("key %d min %d", k, min)
		}
		max[k] = mx
	}
	if max[0] != 7 || max[1] != 10 || max[3] != 4 || max[36] != 1 || r.Remaining() != 0 {
		t.Fatalf("ranges %v", max)
	}

	// KIP-511: ApiVersions v3 gets UNSUPPORTED_VERSION plus the v0-shaped list.
	c.send(wire.APIApiVersions, 3, 12, nil)
	r = c.recv()
	if r.Int32() != 12 || r.Int16() != wire.ErrUnsupportedVersion || r.Int32() != 15 {
		t.Fatal("ApiVersions v3 fallback")
	}

	for _, v := range []int16{0, 4} {
		c.send(wire.APIMetadata, v, 20+int32(v), func(w *wire.Writer) {
			w.Int32(0) // no topics requested -> all streams
			if v >= 4 {
				w.Int8(0)
			}
		})
		r = c.recv()
		r.Int32()
		if v >= 3 {
			r.Int32() // throttle
		}
		if r.Int32() != 1 || r.Int32() != 0 || r.String() != "127.0.0.1" || int(r.Int32()) != e.srv.Port() {
			t.Fatalf("v%d broker", v)
		}
		if v >= 1 {
			r.NullableString() // rack
		}
		if v >= 2 && r.String() != "monstermq-cluster" {
			t.Fatalf("v%d cluster id", v)
		}
		if v >= 1 {
			r.Int32() // controller
		}
		if r.Int32() != 1 || r.Int16() != 0 || r.String() != "sensors" {
			t.Fatalf("v%d topic", v)
		}
		if v >= 1 {
			r.Int8()
		}
		if r.Int32() != 1 || r.Int16() != 0 || r.Int32() != 0 || r.Int32() != 0 {
			t.Fatalf("v%d partition", v)
		}
		if r.Int32() != 1 || r.Int32() != 0 || r.Int32() != 1 || r.Int32() != 0 || r.Remaining() != 0 || r.Err() != nil {
			t.Fatalf("v%d replicas/isr remaining=%d err=%v", v, r.Remaining(), r.Err())
		}
	}
}

func TestProduceAcksZeroSendsNoResponse(t *testing.T) {
	e := startServer(t, sensorsCfg(), nil)
	c := dialRaw(t, e)
	set := wire.EncodeMessageSetV0([]wire.MessageV0{{Key: []byte("sensors/raw"), Value: []byte("v")}})
	c.send(wire.APIProduce, 0, 1, func(w *wire.Writer) {
		w.Int16(0) // acks
		w.Int32(1000)
		w.Int32(1)
		w.String("sensors")
		w.Int32(1)
		w.Int32(0)
		w.Int32(int32(len(set)))
		w.Raw(set)
	})
	c.send(wire.APIApiVersions, 0, 2, nil)
	r := c.recv()
	if corr := r.Int32(); corr != 2 {
		t.Fatalf("first response has correlation %d, acks=0 must not be answered", corr)
	}
	deadline := time.Now().Add(2 * time.Second)
	for e.stores["sensors"].count() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if e.stores["sensors"].count() != 1 {
		t.Fatal("acks=0 record not stored")
	}
}

func TestFetchGroupsPartitionsPerTopic(t *testing.T) {
	e := startServer(t, sensorsCfg(), nil)
	e.bus.Publish(stores.BrokerMessage{MessageUUID: "a", TopicName: "sensors/a", Payload: []byte("1")})
	time.Sleep(3 * flushInterval)
	c := dialRaw(t, e)
	// one topic, partitions 0 and 5 (the latter does not exist)
	c.send(wire.APIFetch, 4, 7, func(w *wire.Writer) {
		w.Int32(-1)
		w.Int32(0)
		w.Int32(0)
		w.Int32(1 << 20)
		w.Int8(0)
		w.Int32(1)
		w.String("sensors")
		w.Int32(2)
		for _, p := range []int32{0, 5} {
			w.Int32(p)
			w.Int64(1)
			w.Int32(1 << 20)
		}
	})
	r := c.recv()
	r.Int32()
	r.Int32() // throttle
	if r.Int32() != 1 || r.String() != "sensors" || r.Int32() != 2 {
		t.Fatal("topic/partition counts")
	}
	for i, want := range []int16{0, wire.ErrUnknownTopicOrPart} {
		if r.Int32() == -1 || r.Int16() != want {
			t.Fatalf("partition %d code", i)
		}
		hwm := r.Int64()
		r.Int64() // last stable
		r.Int32() // aborted txns
		set := r.Bytes()
		if i == 0 && (hwm != 2 || len(set) == 0) {
			t.Fatalf("hwm=%d set=%d", hwm, len(set))
		}
	}
	if r.Remaining() != 0 || r.Err() != nil {
		t.Fatalf("remaining=%d err=%v", r.Remaining(), r.Err())
	}
}

func TestRawSaslHandshakeV0(t *testing.T) {
	e := authedEnv(t)
	c := dialRaw(t, e)
	c.send(wire.APISaslHandshake, 0, 1, func(w *wire.Writer) { w.String("PLAIN") })
	r := c.recv()
	if r.Int32() != 1 || r.Int16() != 0 {
		t.Fatal("handshake")
	}
	tok := []byte("\x00alice\x00secret")
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(tok)))
	_, _ = c.nc.Write(append(hdr[:], tok...))
	r = c.recv() // bare empty token frame
	if r.Remaining() != 0 {
		t.Fatal("expected empty frame")
	}
	c.send(wire.APIMetadata, 0, 2, func(w *wire.Writer) { w.Int32(0) })
	if c.recv().Int32() != 2 {
		t.Fatal("metadata after raw auth")
	}
}

func TestStopClosesEverything(t *testing.T) {
	e := startServer(t, sensorsCfg(), nil)
	if e.srv.Status() != StatusRunning {
		t.Fatal(e.srv.Status())
	}
	cl := dialRaw(t, e)
	cl.send(wire.APIApiVersions, 0, 1, nil)
	cl.recv()
	e.srv.Stop()
	e.srv.Stop() // idempotent
	if e.srv.Status() != StatusStopped || !e.stores["sensors"].closed {
		t.Fatal("not stopped")
	}
	_ = cl.nc.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := cl.nc.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection still open")
	}
	if e.bus.HasSubscribers() {
		t.Fatal("orchestrator subscription leaked")
	}
}

func TestConfigParseDefaults(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"name":"k1","namespace":"ns","port":9093,"streams":[{"streamName":"s","topicFilter":"a/#"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "0.0.0.0" || cfg.Port != 9093 || len(cfg.Streams) != 1 ||
		cfg.Streams[0].RetentionHours != 168 || !cfg.Streams[0].AllowWrite {
		t.Fatalf("%+v", cfg)
	}
	if errs := cfg.Validate(); len(errs) != 0 {
		t.Fatal(errs)
	}
	cfg.Name = "bad name"
	if len(cfg.Validate()) == 0 {
		t.Fatal("name not validated")
	}
	_ = ptr(1)
}

func TestConsumerGroupStubWithFranz(t *testing.T) {
	e := startServer(t, sensorsCfg(), nil)
	e.bus.Publish(stores.BrokerMessage{MessageUUID: "a", TopicName: "sensors/a", Payload: []byte("1")})
	time.Sleep(3 * flushInterval)
	c := newClient(t, e, kgo.ConsumerGroup("grp"), kgo.ConsumeTopics("sensors"),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()), kgo.DisableAutoCommit())
	consume(t, c, 1, 10*time.Second)
	if err := c.CommitUncommittedOffsets(context.Background()); err != nil {
		t.Fatal(err)
	}
	off, ok, _ := e.stores["sensors"].GetOffset(context.Background(), "grp", "sensors", 0)
	if !ok || off != 2 {
		t.Fatalf("committed offset %d ok=%v, want 2 (next offset)", off, ok)
	}
}
