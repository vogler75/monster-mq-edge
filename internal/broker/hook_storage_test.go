package broker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	mqtt "monstermq.io/edge/internal/mqtt"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/pubsub"
	"monstermq.io/edge/internal/stores"
	storememory "monstermq.io/edge/internal/stores/memory"
	storesqlite "monstermq.io/edge/internal/stores/sqlite"
)

type recCounter struct{ in, out, busIn atomic.Int64 }

func (c *recCounter) IncIn()    { c.in.Add(1) }
func (c *recCounter) IncOut()   { c.out.Add(1) }
func (c *recCounter) IncBusIn() { c.busIn.Add(1) }

func (c *recCounter) IncClientIn(string)  {}
func (c *recCounter) IncClientOut(string) {}
func (c *recCounter) ForgetClient(string) {}

type recArchive struct {
	mu   sync.Mutex
	msgs []stores.BrokerMessage
}

func (a *recArchive) Dispatch(msg stores.BrokerMessage) {
	a.mu.Lock()
	a.msgs = append(a.msgs, msg)
	a.mu.Unlock()
}

func (a *recArchive) HasGroups() bool { return true }

func (a *recArchive) take() []stores.BrokerMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.msgs
	a.msgs = nil
	return out
}

// countingRetained counts the batched writes reaching the retained store and
// can hold the next AddAll until released.
type countingRetained struct {
	stores.MessageStore
	adds, dels atomic.Int32
	holdNext   atomic.Bool
	held       chan struct{}
	release    chan struct{}
	fail       atomic.Int32 // the next fail AddAll/DelAll calls return an error
}

var errStoreDown = errors.New("store down")

func (c *countingRetained) AddAll(ctx context.Context, msgs []stores.BrokerMessage) error {
	c.adds.Add(1)
	if c.holdNext.CompareAndSwap(true, false) {
		close(c.held)
		<-c.release
	}
	if c.fail.Add(-1) >= 0 {
		return errStoreDown
	}
	c.fail.Store(0)
	return c.MessageStore.AddAll(ctx, msgs)
}

func (c *countingRetained) DelAll(ctx context.Context, topics []string) error {
	c.dels.Add(1)
	if c.fail.Add(-1) >= 0 {
		return errStoreDown
	}
	c.fail.Store(0)
	return c.MessageStore.DelAll(ctx, topics)
}

type peerEnv struct {
	srv      *mqtt.Server
	hook     *StorageHook
	retained *countingRetained
	bus      <-chan stores.BrokerMessage
	arch     *recArchive
	counts   *recCounter
}

func newPeerEnv(t *testing.T, retainedInMemory bool) *peerEnv {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var base stores.MessageStore
	if retainedInMemory {
		base = storememory.NewMessageStore("retainedmessages")
	} else {
		db, err := storesqlite.OpenMemory("hookstorage-" + strings.ReplaceAll(t.Name(), "/", "-"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		ms := storesqlite.NewMessageStore("retainedmessages", db)
		if err := ms.EnsureTable(context.Background()); err != nil {
			t.Fatal(err)
		}
		base = ms
	}
	retained := &countingRetained{MessageStore: base, held: make(chan struct{}), release: make(chan struct{})}
	bus := pubsub.NewBus()
	_, ch := bus.Subscribe([]string{"#"}, 64)
	srv := mqtt.New(&mqtt.Options{InlineClient: true, Logger: logger})
	e := &peerEnv{srv: srv, retained: retained, bus: ch, arch: &recArchive{}, counts: &recCounter{}}
	e.hook = NewStorageHook(&stores.Storage{Retained: retained}, bus, nil, e.arch, "node-b", logger, e.counts, retainedInMemory, srv)
	if err := srv.AddHook(e.hook, nil); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *peerEnv) inject(t *testing.T, f *packets.Forward, topic, payload string, retain bool, created int64) {
	t.Helper()
	inj := e.srv.NewClient(nil, "peerlink", "peerlink:"+f.SourceNode, true)
	inj.Properties.ProtocolVersion = 5
	pk := packets.Packet{
		FixedHeader: packets.FixedHeader{Type: packets.Publish, Qos: 1, Retain: retain},
		PacketID:    1,
		TopicName:   topic,
		Payload:     []byte(payload),
		Origin:      f.ClientID,
		Created:     created,
		Forward:     f,
	}
	pk.Properties.MessageExpiryInterval = 600
	if err := e.srv.InjectPacket(inj, pk); err != nil {
		t.Fatal(err)
	}
}

func (e *peerEnv) busMsgs() []stores.BrokerMessage {
	var out []stores.BrokerMessage
	for {
		select {
		case m := <-e.bus:
			out = append(out, m)
		default:
			return out
		}
	}
}

func (e *peerEnv) get(t *testing.T, topic string) *stores.BrokerMessage {
	t.Helper()
	m, err := e.retained.Get(context.Background(), topic)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestStorageHookReplicaDispatch(t *testing.T) {
	e := newPeerEnv(t, false)
	ts := time.Date(2026, 10, 3, 12, 0, 0, 123456789, time.UTC)
	f := &packets.Forward{SourceNode: "oa-a", ClientID: "pub1", Username: "u1", TimeNs: ts.UnixNano(), Epoch: 7, Offset: 42, Dup: true}
	e.inject(t, f, "plant/a", "v1", false, 0)

	bus := e.busMsgs()
	arch := e.arch.take()
	if len(bus) != 1 || len(arch) != 1 {
		t.Fatalf("bus got %d, archive got %d, want 1 each", len(bus), len(arch))
	}
	for _, m := range []stores.BrokerMessage{bus[0], arch[0]} {
		if m.TopicName != "plant/a" || string(m.Payload) != "v1" || m.QoS != 1 {
			t.Errorf("message = %+v", m)
		}
		if m.ClientID != "pub1" || !m.Time.Equal(ts) || !m.IsDup || m.OriginNode != "oa-a" {
			t.Errorf("replica fields: ClientID=%q Time=%v IsDup=%v OriginNode=%q", m.ClientID, m.Time, m.IsDup, m.OriginNode)
		}
		if m.MessageUUID != replicaUUID(f) {
			t.Errorf("MessageUUID = %q, want derived %q", m.MessageUUID, replicaUUID(f))
		}
		if m.MessageExpiryInterval == nil || *m.MessageExpiryInterval != 600 {
			t.Errorf("MessageExpiryInterval = %v", m.MessageExpiryInterval)
		}
	}
	if in, busIn := e.counts.in.Load(), e.counts.busIn.Load(); in != 0 || busIn != 1 {
		t.Errorf("IncIn=%d IncBusIn=%d, want 0 and 1", in, busIn)
	}

	if err := e.srv.Publish("plant/b", []byte("local"), false, 0); err != nil {
		t.Fatal(err)
	}
	bus = e.busMsgs()
	if len(bus) != 1 || bus[0].OriginNode != "" || bus[0].ClientID != mqtt.InlineClientId {
		t.Fatalf("local publish on the bus: %+v", bus)
	}
	if in, busIn := e.counts.in.Load(), e.counts.busIn.Load(); in != 1 || busIn != 1 {
		t.Errorf("after local publish IncIn=%d IncBusIn=%d, want 1 and 1", in, busIn)
	}
}

func TestStorageHookReplicaPolicy(t *testing.T) {
	cases := []struct {
		name        string
		policy      PeerPolicy
		will, snap  bool
		bus, archiv bool
	}{
		{"default", DefaultPeerPolicy(), false, false, true, true},
		{"bus off", PeerPolicy{Archive: true}, false, false, false, true},
		{"archive off", PeerPolicy{Bus: true}, false, false, true, false},
		{"both off", PeerPolicy{}, false, false, false, false},
		{"will", DefaultPeerPolicy(), true, false, false, false},
		{"snapshot", DefaultPeerPolicy(), false, true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newPeerEnv(t, true)
			e.hook.SetPeerPolicy(tc.policy)
			f := &packets.Forward{SourceNode: "oa-a", ClientID: "pub1", Epoch: 1, Offset: 1, Will: tc.will, Snapshot: tc.snap}
			e.inject(t, f, "plant/a", "v", true, 0)
			if got := len(e.busMsgs()) == 1; got != tc.bus {
				t.Errorf("bus delivery = %v, want %v", got, tc.bus)
			}
			if got := len(e.arch.take()) == 1; got != tc.archiv {
				t.Errorf("archive delivery = %v, want %v", got, tc.archiv)
			}
			if e.counts.busIn.Load() != 1 || e.counts.in.Load() != 0 {
				t.Errorf("IncBusIn=%d IncIn=%d, want 1 and 0", e.counts.busIn.Load(), e.counts.in.Load())
			}
			if m := e.get(t, "plant/a"); m == nil || string(m.Payload) != "v" {
				t.Errorf("retained store must always get the replica, got %+v", m)
			}
		})
	}
}

func TestReplicaUUID(t *testing.T) {
	f := &packets.Forward{SourceNode: "oa-a", Epoch: 7, Offset: 42}
	a := replicaUUID(f)
	if a != replicaUUID(&packets.Forward{SourceNode: "oa-a", Epoch: 7, Offset: 42}) {
		t.Fatal("derived UUID is not stable")
	}
	u, err := uuid.Parse(a)
	if err != nil {
		t.Fatal(err)
	}
	if u.Version() != 8 || u.Variant() != uuid.RFC4122 {
		t.Errorf("version %d variant %v, want 8 and RFC 4122", u.Version(), u.Variant())
	}
	for _, other := range []*packets.Forward{
		{SourceNode: "oa-a", Epoch: 7, Offset: 43},
		{SourceNode: "oa-a", Epoch: 8, Offset: 42},
		{SourceNode: "oa-b", Epoch: 7, Offset: 42},
	} {
		if replicaUUID(other) == a {
			t.Errorf("%+v collides with %+v", other, f)
		}
	}
	s1, s2 := replicaUUID(&packets.Forward{SourceNode: "oa-a", Snapshot: true}), replicaUUID(&packets.Forward{SourceNode: "oa-a", Snapshot: true})
	if s1 == s2 {
		t.Error("records without an offset must get distinct UUIDs")
	}
}

func TestStorageHookRetainedReplicasBatched(t *testing.T) {
	e := newPeerEnv(t, false)
	if err := e.srv.Publish("r/3", []byte("old"), true, 0); err != nil {
		t.Fatal(err)
	}
	e.retained.adds.Store(0)

	created := time.Now().Unix() - 100
	fwd := func(off uint64) *packets.Forward {
		return &packets.Forward{SourceNode: "oa-a", ClientID: "pub1", Username: "u1", Epoch: 7, Offset: off}
	}
	e.inject(t, fwd(1), "r/1", "a", true, created)
	e.inject(t, fwd(2), "r/2", "b", true, created)
	e.inject(t, fwd(3), "r/1", "c", true, created)
	e.inject(t, fwd(4), "r/3", "", true, created)
	e.inject(t, &packets.Forward{SourceNode: "oa-c", ClientID: "pub2", Epoch: 9, Offset: 1}, "r/4", "x", true, created)

	if e.retained.adds.Load() != 0 || e.retained.dels.Load() != 0 {
		t.Fatalf("replica writes before the flush: adds=%d dels=%d", e.retained.adds.Load(), e.retained.dels.Load())
	}
	if m := e.get(t, "r/1"); m != nil {
		t.Fatalf("r/1 stored before the flush: %+v", m)
	}
	if m := e.get(t, "r/3"); m == nil {
		t.Fatal("r/3 deleted before the flush")
	}
	if n := e.hook.PendingReplicas("oa-a"); n != 3 {
		t.Fatalf("PendingReplicas = %d, want 3", n)
	}

	if err := e.hook.FlushReplicas("oa-a"); err != nil {
		t.Fatal(err)
	}
	if e.retained.adds.Load() != 1 || e.retained.dels.Load() != 1 {
		t.Errorf("flush wrote adds=%d dels=%d, want one of each", e.retained.adds.Load(), e.retained.dels.Load())
	}
	m := e.get(t, "r/1")
	if m == nil || string(m.Payload) != "c" || m.ClientID != "pub1" {
		t.Fatalf("r/1 = %+v, want the last replica value c from pub1", m)
	}
	if m.Time.Unix() != created {
		t.Errorf("r/1 time = %v, want the backdated %v", m.Time, time.Unix(created, 0))
	}
	if m.MessageExpiryInterval == nil || *m.MessageExpiryInterval != 600 {
		t.Errorf("r/1 expiry = %v, want 600", m.MessageExpiryInterval)
	}
	if m.MessageUUID != replicaUUID(fwd(3)) {
		t.Errorf("r/1 uuid = %q, want %q", m.MessageUUID, replicaUUID(fwd(3)))
	}
	if m := e.get(t, "r/2"); m == nil || string(m.Payload) != "b" {
		t.Errorf("r/2 = %+v", m)
	}
	if m := e.get(t, "r/3"); m != nil {
		t.Errorf("r/3 not deleted: %+v", m)
	}
	if m := e.get(t, "r/4"); m != nil {
		t.Errorf("another source's replica was flushed: %+v", m)
	}
	if n := e.hook.PendingReplicas("oa-c"); n != 1 {
		t.Errorf("PendingReplicas(oa-c) = %d, want 1", n)
	}

	if err := e.hook.FlushReplicas("oa-a"); err != nil {
		t.Fatal(err)
	}
	if e.retained.adds.Load() != 1 || e.retained.dels.Load() != 1 {
		t.Error("an empty flush wrote to the store")
	}
	if err := e.hook.FlushReplicas("oa-c"); err != nil {
		t.Fatal(err)
	}
	if m := e.get(t, "r/4"); m == nil || m.ClientID != "pub2" {
		t.Errorf("r/4 = %+v", m)
	}
	if n := e.hook.replicas.active.Load(); n != 0 {
		t.Errorf("active = %d after all flushes, want 0", n)
	}
}

func TestStorageHookLocalRetainSupersedesPendingReplica(t *testing.T) {
	e := newPeerEnv(t, false)
	e.inject(t, &packets.Forward{SourceNode: "oa-a", ClientID: "pub1", Epoch: 1, Offset: 1}, "x", "replica", true, 0)
	if err := e.srv.Publish("x", []byte("local"), true, 0); err != nil {
		t.Fatal(err)
	}
	if n := e.hook.PendingReplicas("oa-a"); n != 0 {
		t.Fatalf("PendingReplicas = %d after a local write, want 0", n)
	}
	adds := e.retained.adds.Load()
	if err := e.hook.FlushReplicas("oa-a"); err != nil {
		t.Fatal(err)
	}
	if e.retained.adds.Load() != adds {
		t.Error("flush wrote the superseded replica")
	}
	if m := e.get(t, "x"); m == nil || string(m.Payload) != "local" {
		t.Errorf("x = %+v, want the local value", m)
	}
	if n := e.hook.replicas.active.Load(); n != 0 {
		t.Errorf("active = %d, want 0", n)
	}
}

func TestStorageHookRetainedReplicaNewestSourceWins(t *testing.T) {
	e := newPeerEnv(t, false)
	e.inject(t, &packets.Forward{SourceNode: "oa-a", ClientID: "pub1", Epoch: 1, Offset: 1}, "x", "from-a", true, 0)
	e.inject(t, &packets.Forward{SourceNode: "oa-c", ClientID: "pub2", Epoch: 2, Offset: 1}, "x", "from-c", true, 0)
	if a, c := e.hook.PendingReplicas("oa-a"), e.hook.PendingReplicas("oa-c"); a != 0 || c != 1 {
		t.Fatalf("pending oa-a=%d oa-c=%d, want 0 and 1", a, c)
	}
	if err := e.hook.FlushReplicas("oa-c"); err != nil {
		t.Fatal(err)
	}
	if err := e.hook.FlushReplicas("oa-a"); err != nil {
		t.Fatal(err)
	}
	if m := e.get(t, "x"); m == nil || string(m.Payload) != "from-c" {
		t.Errorf("x = %+v, want the later arrival from oa-c", m)
	}
	if n := e.hook.replicas.active.Load(); n != 0 {
		t.Errorf("active = %d, want 0", n)
	}
}

func TestStorageHookLocalRetainWaitsForRunningFlush(t *testing.T) {
	e := newPeerEnv(t, false)
	e.inject(t, &packets.Forward{SourceNode: "oa-a", ClientID: "pub1", Epoch: 1, Offset: 1}, "x", "replica", true, 0)

	e.retained.holdNext.Store(true)
	flushed := make(chan error, 1)
	go func() { flushed <- e.hook.FlushReplicas("oa-a") }()
	select {
	case <-e.retained.held:
	case <-time.After(5 * time.Second):
		t.Fatal("the flush did not write the pending replica")
	}

	published := make(chan error, 1)
	go func() { published <- e.srv.Publish("x", []byte("local"), true, 0) }()
	select {
	case err := <-published:
		t.Fatalf("local retained write finished while the flush of the same topic was running (err %v)", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(e.retained.release)
	if err := <-flushed; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-published:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("local retained write still blocked after the flush")
	}
	if m := e.get(t, "x"); m == nil || string(m.Payload) != "local" {
		t.Errorf("x = %+v, want the local value to land last", m)
	}
}

func TestStorageHookRetainedReplicaMemoryMode(t *testing.T) {
	e := newPeerEnv(t, true)
	e.inject(t, &packets.Forward{SourceNode: "oa-a", ClientID: "pub1", Epoch: 1, Offset: 1}, "x", "v", true, 0)
	if m := e.get(t, "x"); m == nil || string(m.Payload) != "v" || m.ClientID != "pub1" {
		t.Fatalf("x = %+v, want the replica stored at once", m)
	}
	if n := e.hook.PendingReplicas("oa-a"); n != 0 {
		t.Errorf("PendingReplicas = %d in memory mode, want 0", n)
	}
	e.inject(t, &packets.Forward{SourceNode: "oa-a", ClientID: "pub1", Epoch: 1, Offset: 2}, "x", "", true, 0)
	if m := e.get(t, "x"); m != nil {
		t.Errorf("x = %+v after a replicated delete", m)
	}
}

// A failed replica flush keeps its values pending; the next flush writes them (review finding 12).
func TestStorageHookFailedFlushIsRetried(t *testing.T) {
	e := newPeerEnv(t, false)
	if err := e.srv.Publish("f/2", []byte("old"), true, 0); err != nil {
		t.Fatal(err)
	}
	f := func(off uint64) *packets.Forward {
		return &packets.Forward{SourceNode: "oa-a", ClientID: "pub1", Epoch: 1, Offset: off}
	}
	e.inject(t, f(1), "f/1", "a", true, 0)
	e.inject(t, f(2), "f/2", "", true, 0)
	e.retained.fail.Store(2)
	if err := e.hook.FlushReplicas("oa-a"); !errors.Is(err, errStoreDown) {
		t.Fatalf("flush error %v", err)
	}
	if n := e.hook.PendingReplicas("oa-a"); n != 2 {
		t.Fatalf("pending after a failed flush %d, want 2", n)
	}
	if err := e.hook.FlushReplicas("oa-a"); err != nil {
		t.Fatal(err)
	}
	if m := e.get(t, "f/1"); m == nil || string(m.Payload) != "a" {
		t.Fatalf("f/1 = %+v", m)
	}
	if m := e.get(t, "f/2"); m != nil {
		t.Fatalf("f/2 not deleted: %+v", m)
	}
	if n := e.hook.PendingReplicas("oa-a"); n != 0 || e.hook.replicas.active.Load() != 0 {
		t.Fatalf("pending %d active %d", n, e.hook.replicas.active.Load())
	}
}

// A local write that arrives during a failing flush of the same topic wins: the failed replica
// value is not put back (review finding 12).
func TestStorageHookFailedFlushDoesNotOverwriteLocal(t *testing.T) {
	e := newPeerEnv(t, false)
	e.inject(t, &packets.Forward{SourceNode: "oa-a", ClientID: "pub1", Epoch: 1, Offset: 1}, "x", "replica", true, 0)
	e.retained.holdNext.Store(true)
	e.retained.fail.Store(1)
	flushed := make(chan error, 1)
	go func() { flushed <- e.hook.FlushReplicas("oa-a") }()
	<-e.retained.held
	published := make(chan error, 1)
	go func() { published <- e.srv.Publish("x", []byte("local"), true, 0) }()
	time.Sleep(50 * time.Millisecond)
	close(e.retained.release)
	if err := <-flushed; !errors.Is(err, errStoreDown) {
		t.Fatalf("flush error %v", err)
	}
	if err := <-published; err != nil {
		t.Fatal(err)
	}
	if n := e.hook.PendingReplicas("oa-a"); n != 0 {
		t.Fatalf("superseded replica put back (%d pending)", n)
	}
	if err := e.hook.FlushReplicas("oa-a"); err != nil {
		t.Fatal(err)
	}
	if m := e.get(t, "x"); m == nil || string(m.Payload) != "local" {
		t.Fatalf("x = %+v, want the local value", m)
	}
}
