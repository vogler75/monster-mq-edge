package broker

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"monstermq.io/edge/internal/config"
	mqtt "monstermq.io/edge/internal/mqtt"
	"monstermq.io/edge/internal/mqtt/hooks/auth"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/stores"
	storememory "monstermq.io/edge/internal/stores/memory"
	storesqlite "monstermq.io/edge/internal/stores/sqlite"
	"monstermq.io/edge/internal/topic"
)

type queueEnv struct {
	srv     *mqtt.Server
	storage *stores.Storage
	subs    *topic.SubscriptionIndex
	logger  *slog.Logger
}

// newQueueEnv stores persistent sessions "own" (this node, written with a
// different case) and "peer" (another node sharing the session store), both
// offline and subscribed to a/#.
func newQueueEnv(t *testing.T) *queueEnv {
	t.Helper()
	ctx := context.Background()
	db, err := storesqlite.OpenMemory("hookqueue-" + strings.ReplaceAll(t.Name(), "/", "-"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sessions := storesqlite.NewSessionStore(db)
	if err := sessions.EnsureTable(ctx); err != nil {
		t.Fatal(err)
	}
	for _, s := range []stores.SessionInfo{
		{ClientID: "own", NodeID: "Node-A"},
		{ClientID: "peer", NodeID: "node-b"},
		{ClientID: "clean", NodeID: "node-a", CleanSession: true},
	} {
		s.UpdateTime = time.Now()
		if err := sessions.SetClient(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	subs := topic.NewSubscriptionIndex()
	for _, id := range []string{"own", "peer", "clean"} {
		subs.Subscribe(id, "a/#", 1)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := mqtt.New(&mqtt.Options{InlineClient: true, Logger: logger})
	if err := srv.AddHook(new(auth.AllowHook), nil); err != nil {
		t.Fatal(err)
	}
	return &queueEnv{
		srv:     srv,
		storage: &stores.Storage{Sessions: sessions, Subscriptions: sessions, Queue: storememory.NewQueueStore(30 * time.Second)},
		subs:    subs,
		logger:  logger,
	}
}

func (e *queueEnv) hook(t *testing.T, opts ...QueueHookOption) *QueueHook {
	t.Helper()
	h := NewQueueHook(e.storage, e.subs, e.srv, e.logger, 100, opts...)
	if err := e.srv.AddHook(h, nil); err != nil {
		t.Fatal(err)
	}
	return h
}

func (e *queueEnv) count(t *testing.T, clientID string) int64 {
	t.Helper()
	n, err := e.storage.Queue.Count(context.Background(), clientID)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *queueEnv) injectReplica(t *testing.T, f *packets.Forward, topicName string) {
	t.Helper()
	inj := e.srv.NewClient(nil, "peerlink", "peerlink:"+f.SourceNode, true)
	inj.Properties.ProtocolVersion = 5
	pk := packets.Packet{
		FixedHeader: packets.FixedHeader{Type: packets.Publish, Qos: 1},
		PacketID:    1,
		TopicName:   topicName,
		Payload:     []byte("v"),
		Origin:      f.ClientID,
		Forward:     f,
	}
	if err := e.srv.InjectPacket(inj, pk); err != nil {
		t.Fatal(err)
	}
}

func TestQueueHookHydratesOwnNodeSessionsWithPeerLink(t *testing.T) {
	t.Run("without PeerLink", func(t *testing.T) {
		e := newQueueEnv(t)
		e.hook(t)
		if err := e.srv.Publish("a/1", []byte("v"), false, 1); err != nil {
			t.Fatal(err)
		}
		if own, peer := e.count(t, "own"), e.count(t, "peer"); own != 1 || peer != 1 {
			t.Errorf("queued own=%d peer=%d, want 1 and 1", own, peer)
		}
	})
	t.Run("with PeerLink", func(t *testing.T) {
		e := newQueueEnv(t)
		e.hook(t, WithPeerLink(PeerPolicy{}, "node-a"))
		if err := e.srv.Publish("a/1", []byte("v"), false, 1); err != nil {
			t.Fatal(err)
		}
		if own, peer, clean := e.count(t, "own"), e.count(t, "peer"), e.count(t, "clean"); own != 1 || peer != 0 || clean != 0 {
			t.Errorf("queued own=%d peer=%d clean=%d, want 1, 0 and 0", own, peer, clean)
		}
	})
}

func TestQueueHookReplicas(t *testing.T) {
	cases := []struct {
		name   string
		queue  bool
		fwd    packets.Forward
		queued int64
	}{
		{"Receive.Queue false skips replicas", false, packets.Forward{}, 0},
		{"Receive.Queue queues replicas", true, packets.Forward{}, 1},
		{"wills never queued", true, packets.Forward{Will: true}, 0},
		{"snapshot values never queued", true, packets.Forward{Snapshot: true}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newQueueEnv(t)
			e.hook(t, WithPeerLink(PeerPolicy{Queue: tc.queue}, "node-a"))
			f := tc.fwd
			f.SourceNode, f.ClientID, f.Epoch, f.Offset = "node-b", "pub1", 1, 1
			e.injectReplica(t, &f, "a/1")
			if got := e.count(t, "own"); got != tc.queued {
				t.Errorf("queued %d, want %d", got, tc.queued)
			}
		})
	}
}

// An unset Receive section queues forwarded QoS 1 publishes for offline persistent sessions
// (plan-peerlink-interest-routing IR-M0): interest routing makes such a session the reason the
// peer receives the topic at all.
func TestQueueHookQueuesReplicasByDefault(t *testing.T) {
	e := newQueueEnv(t)
	e.hook(t, WithPeerLink(NewPeerPolicy(config.PeerLinkReceive{}), "node-a"))
	f := packets.Forward{SourceNode: "node-b", ClientID: "pub1", Epoch: 1, Offset: 1}
	e.injectReplica(t, &f, "a/1")
	if got := e.count(t, "own"); got != 1 {
		t.Fatalf("queued %d, want 1", got)
	}
}

// A replica is not queued for the offline session of its own publisher, which moved to the peer
// (plan 12.4, NoLocal); other offline sessions still get it with Receive.Queue.
func TestQueueHookSkipsReplicaPublisher(t *testing.T) {
	e := newQueueEnv(t)
	e.hook(t, WithPeerLink(PeerPolicy{Queue: true}, "node-a"))
	f := packets.Forward{SourceNode: "node-b", ClientID: "own", Epoch: 1, Offset: 1}
	e.injectReplica(t, &f, "a/1")
	if got := e.count(t, "own"); got != 0 {
		t.Fatalf("publisher's own session queued %d", got)
	}
	f = packets.Forward{SourceNode: "node-b", ClientID: "pub1", Epoch: 1, Offset: 2}
	e.injectReplica(t, &f, "a/1")
	if got := e.count(t, "own"); got != 1 {
		t.Fatalf("other replica queued %d, want 1", got)
	}
}

// Sessions stored under the configured NodeId are hydrated when PeerLink adopted another id.
func TestQueueHookHydratesEveryOwnNodeID(t *testing.T) {
	e := newQueueEnv(t)
	e.hook(t, WithPeerLink(PeerPolicy{}, "node-x.plant.local", "node-a"))
	if err := e.srv.Publish("a/1", []byte("v"), false, 1); err != nil {
		t.Fatal(err)
	}
	if own, peer := e.count(t, "own"), e.count(t, "peer"); own != 1 || peer != 0 {
		t.Fatalf("queued own=%d peer=%d", own, peer)
	}
}
