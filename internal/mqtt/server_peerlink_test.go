package mqtt

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"monstermq.io/edge/internal/mqtt/listeners"
	"monstermq.io/edge/internal/mqtt/packets"
)

// recHook records the hook events the peer link relies on.
type recHook struct {
	HookBase
	mu          sync.Mutex
	published   []packets.Packet
	retained    []packets.Packet
	retainedR   []int64
	willSent    []packets.Packet
	selected    []packets.Packet
	established []string
	refuseID    string // OnConnect refuses this client id with CONNACK 0x85
	skipShared  bool   // OnSelectSubscribers drops shared groups for replicas
	srv         *Server
	authCalls   atomic.Int32
	onStopped   atomic.Int32
	stopCalls   atomic.Int32
}

func (h *recHook) ID() string { return "rec" }

func (h *recHook) Provides(b byte) bool {
	switch b {
	case OnConnect, OnConnectAuthenticate, OnACLCheck, OnSessionEstablished, OnPublished,
		OnRetainMessage, OnWillSent, OnSelectSubscribers, OnStopped:
		return true
	}
	return false
}

func (h *recHook) Stop() error { h.stopCalls.Add(1); return nil }
func (h *recHook) OnStopped()  { h.onStopped.Add(1) }

func (h *recHook) OnConnect(cl *Client, pk packets.Packet) error {
	if h.refuseID != "" && cl.ID == h.refuseID {
		_ = h.srv.SendConnack(cl, packets.ErrClientIdentifierNotValid, false, nil)
		return packets.ErrClientIdentifierNotValid
	}
	return nil
}

func (h *recHook) OnConnectAuthenticate(*Client, packets.Packet) bool {
	h.authCalls.Add(1)
	return true
}

func (h *recHook) OnACLCheck(*Client, string, bool) bool { return true }

func (h *recHook) OnSessionEstablished(cl *Client, _ packets.Packet) {
	h.mu.Lock()
	h.established = append(h.established, cl.ID)
	h.mu.Unlock()
}

func (h *recHook) OnPublished(_ *Client, pk packets.Packet) {
	h.mu.Lock()
	h.published = append(h.published, pk)
	h.mu.Unlock()
}

func (h *recHook) OnRetainMessage(_ *Client, pk packets.Packet, r int64) {
	h.mu.Lock()
	h.retained = append(h.retained, pk)
	h.retainedR = append(h.retainedR, r)
	h.mu.Unlock()
}

func (h *recHook) OnWillSent(_ *Client, pk packets.Packet) {
	h.mu.Lock()
	h.willSent = append(h.willSent, pk)
	h.mu.Unlock()
}

func (h *recHook) OnSelectSubscribers(subs *Subscribers, pk packets.Packet) *Subscribers {
	h.mu.Lock()
	h.selected = append(h.selected, pk)
	h.mu.Unlock()
	if h.skipShared && pk.Forward != nil {
		subs.Shared = map[string]map[string]packets.Subscription{}
		subs.SharedSelected = map[string]packets.Subscription{}
	}
	return subs
}

func (h *recHook) snapshot() (published, retained, willSent, selected []packets.Packet, established []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]packets.Packet(nil), h.published...),
		append([]packets.Packet(nil), h.retained...),
		append([]packets.Packet(nil), h.willSent...),
		append([]packets.Packet(nil), h.selected...),
		append([]string(nil), h.established...)
}

func newTestServer(t *testing.T) (*Server, *recHook) {
	t.Helper()
	s := New(&Options{InlineClient: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	h := &recHook{srv: s}
	if err := s.AddHook(h, nil); err != nil {
		t.Fatal(err)
	}
	return s, h
}

func newInjector(s *Server, sourceNode string) *Client {
	cl := s.NewClient(nil, "peerlink", "peerlink:"+sourceNode, true)
	cl.Properties.ProtocolVersion = 5
	return cl
}

// addSubscriber registers a connected v5 client without running its write
// loop, so deliveries stay in its outbound channel.
func addSubscriber(t *testing.T, s *Server, id string, subs ...packets.Subscription) *Client {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
	cl := s.NewClient(c1, "t1", id, false)
	cl.Properties.ProtocolVersion = 5
	s.Clients.Add(cl)
	for _, sub := range subs {
		s.Topics.Subscribe(id, sub)
		cl.State.Subscriptions.Add(sub.Filter, sub)
	}
	return cl
}

func outbound(cl *Client) []packets.Packet {
	var out []packets.Packet
	for {
		select {
		case pk := <-cl.State.outbound:
			out = append(out, *pk)
		default:
			return out
		}
	}
}

func publishPk(topic string, payload string) packets.Packet {
	return packets.Packet{
		FixedHeader: packets.FixedHeader{Type: packets.Publish},
		TopicName:   topic,
		Payload:     []byte(payload),
	}
}

func TestInjectInlinePresetOriginAndCreated(t *testing.T) {
	now := time.Now().Unix()
	cases := []struct {
		name        string
		origin      string
		created     int64
		wantOrigin  string
		wantCreated int64 // 0: within [now, now+2]
	}{
		{"preset kept", "c1", now - 100, "c1", now - 100},
		{"empty origin and zero created", "", 0, "peerlink:oa-a", 0},
		{"future created", "c1", now + 3600, "c1", 0},
		{"negative created", "c1", -5, "c1", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, h := newTestServer(t)
			inj := newInjector(s, "oa-a")
			fwd := &packets.Forward{SourceNode: "oa-a", ClientID: "c1", Epoch: 1, Offset: 1}
			pk := publishPk("a/b", "v")
			pk.Origin = tc.origin
			pk.Created = tc.created
			pk.Properties.MessageExpiryInterval = 300
			pk.Forward = fwd
			if err := s.InjectPacket(inj, pk); err != nil {
				t.Fatal(err)
			}

			published, _, _, _, _ := h.snapshot()
			if len(published) != 1 {
				t.Fatalf("OnPublished fired %d times, want 1", len(published))
			}
			got := published[0]
			if got.Origin != tc.wantOrigin {
				t.Errorf("Origin = %q, want %q", got.Origin, tc.wantOrigin)
			}
			if tc.wantCreated != 0 {
				if got.Created != tc.wantCreated {
					t.Errorf("Created = %d, want %d", got.Created, tc.wantCreated)
				}
			} else if got.Created < now || got.Created > now+2 {
				t.Errorf("Created = %d, want about %d", got.Created, now)
			}
			if got.Expiry != got.Created+300 {
				t.Errorf("Expiry = %d, want Created+300 = %d", got.Expiry, got.Created+300)
			}
			if got.Forward != fwd {
				t.Error("OnPublished did not see pk.Forward")
			}
			if _, ok := s.Clients.Get(inj.ID); ok {
				t.Error("injector client must not be in s.Clients")
			}
		})
	}
}

func TestInjectNetworkClientIgnoresPreset(t *testing.T) {
	s, h := newTestServer(t)
	cl := s.NewClient(nil, "t1", "net1", false)
	cl.Properties.ProtocolVersion = 5
	cl.State.Inflight.ResetReceiveQuota(10)

	now := time.Now().Unix()
	pk := publishPk("a/b", "v")
	pk.Origin = "spoofed"
	pk.Created = now - 100
	if err := s.InjectPacket(cl, pk); err != nil {
		t.Fatal(err)
	}

	published, _, _, _, _ := h.snapshot()
	if len(published) != 1 {
		t.Fatalf("OnPublished fired %d times, want 1", len(published))
	}
	if got := published[0]; got.Origin != "net1" || got.Created < now {
		t.Errorf("network client preset kept: Origin=%q Created=%d (now %d)", got.Origin, got.Created, now)
	}
}

func TestInlinePublishStampsSharedInlineClient(t *testing.T) {
	s, h := newTestServer(t)
	now := time.Now().Unix()
	if err := s.Publish("a/b", []byte("v"), false, 0); err != nil {
		t.Fatal(err)
	}
	published, _, _, _, _ := h.snapshot()
	if len(published) != 1 {
		t.Fatalf("OnPublished fired %d times, want 1", len(published))
	}
	if got := published[0]; got.Origin != InlineClientId || got.Created < now || got.Forward != nil {
		t.Errorf("Publish: Origin=%q Created=%d Forward=%v", got.Origin, got.Created, got.Forward)
	}
}

func TestInjectNoLocalUsesPresetOrigin(t *testing.T) {
	s, _ := newTestServer(t)
	self := addSubscriber(t, s, "c1", packets.Subscription{Filter: "a/#", NoLocal: true})
	other := addSubscriber(t, s, "c2", packets.Subscription{Filter: "a/#"})
	inj := newInjector(s, "oa-a")

	pk := publishPk("a/b", "v")
	pk.Origin = "c1"
	pk.Forward = &packets.Forward{SourceNode: "oa-a", ClientID: "c1"}
	if err := s.InjectPacket(inj, pk); err != nil {
		t.Fatal(err)
	}

	if got := outbound(self); len(got) != 0 {
		t.Errorf("NoLocal subscriber c1 got %d messages of its own replica", len(got))
	}
	got := outbound(other)
	if len(got) != 1 {
		t.Fatalf("subscriber c2 got %d messages, want 1", len(got))
	}
	if got[0].Forward != nil || got[0].Will {
		t.Error("delivered copy carries Forward or Will")
	}
}

func TestSelectSubscribersSeesForward(t *testing.T) {
	s, h := newTestServer(t)
	h.skipShared = true
	member := addSubscriber(t, s, "g1", packets.Subscription{Filter: "$share/g/a/#"})
	inj := newInjector(s, "oa-a")

	pk := publishPk("a/b", "replica")
	pk.Forward = &packets.Forward{SourceNode: "oa-a"}
	if err := s.InjectPacket(inj, pk); err != nil {
		t.Fatal(err)
	}
	if got := outbound(member); len(got) != 0 {
		t.Errorf("shared group got %d replicas, want 0", len(got))
	}

	if err := s.Publish("a/b", []byte("local"), false, 0); err != nil {
		t.Fatal(err)
	}
	if got := outbound(member); len(got) != 1 {
		t.Errorf("shared group got %d local messages, want 1", len(got))
	}

	_, _, _, selected, _ := h.snapshot()
	if len(selected) != 2 || selected[0].Forward == nil || selected[1].Forward != nil {
		t.Fatalf("OnSelectSubscribers calls = %d, want replica then local", len(selected))
	}
}

func TestRetainOnlyStoresWithoutDelivery(t *testing.T) {
	s, h := newTestServer(t)
	sub := addSubscriber(t, s, "c2", packets.Subscription{Filter: "a/#"})
	inj := newInjector(s, "oa-a")

	now := time.Now().Unix()
	fwd := &packets.Forward{SourceNode: "oa-a", ClientID: "c1"}
	pk := publishPk("a/b", "v")
	pk.FixedHeader.Qos = 1
	pk.PacketID = 1
	pk.Origin = "c1"
	pk.Created = now - 50
	pk.Properties.MessageExpiryInterval = 300
	pk.Forward = fwd
	if err := s.RetainOnly(inj, pk); err != nil {
		t.Fatal(err)
	}

	stored, ok := s.Topics.Retained.Get("a/b")
	if !ok {
		t.Fatal("retained message not stored")
	}
	if string(stored.Payload) != "v" || !stored.FixedHeader.Retain || stored.Origin != "c1" ||
		stored.Created != now-50 || stored.Expiry != now-50+300 {
		t.Errorf("stored retained = %+v", stored)
	}
	if stored.Forward != nil {
		t.Error("stored retained copy keeps Forward")
	}
	if got := atomic.LoadInt64(&s.Info.Retained); got != 1 {
		t.Errorf("Info.Retained = %d, want 1", got)
	}

	published, retained, _, _, _ := h.snapshot()
	if len(published) != 0 {
		t.Errorf("OnPublished fired %d times, want 0", len(published))
	}
	if len(retained) != 1 || retained[0].Forward != fwd || h.retainedR[0] != 1 {
		t.Fatalf("OnRetainMessage calls = %d, want 1 with Forward and r=1", len(retained))
	}
	if got := outbound(sub); len(got) != 0 {
		t.Errorf("subscriber got %d messages, want 0", len(got))
	}
	if r, p := atomic.LoadInt64(&s.Info.MessagesReceived), atomic.LoadInt64(&s.Info.PacketsReceived); r != 0 || p != 0 {
		t.Errorf("counters changed: messages/received=%d packets/received=%d", r, p)
	}

	notRetained := publishPk("a/c", "w")
	if err := s.RetainOnly(inj, notRetained); err != nil {
		t.Fatal(err)
	}
	if stored, ok := s.Topics.Retained.Get("a/c"); !ok || !stored.FixedHeader.Retain {
		t.Error("RetainOnly did not store with the retain flag set")
	}

	if err := s.RetainOnly(inj, publishPk("a/b", "")); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Topics.Retained.Get("a/b"); ok {
		t.Error("empty payload did not delete the retained message")
	}
	if h.retainedR[len(h.retainedR)-1] != -1 {
		t.Errorf("delete reported r=%d, want -1", h.retainedR[len(h.retainedR)-1])
	}
}

func TestRetainOnlyValidates(t *testing.T) {
	s, h := newTestServer(t)
	inj := newInjector(s, "oa-a")

	noID := publishPk("a/b", "v")
	noID.FixedHeader.Qos = 1
	wildcard := publishPk("a/+", "v")
	notPublish := publishPk("a/b", "v")
	notPublish.FixedHeader.Type = packets.Subscribe
	emptyTopic := publishPk("", "v")

	for _, tc := range []struct {
		name string
		pk   packets.Packet
		want packets.Code
	}{
		{"qos1 without packet id", noID, packets.ErrProtocolViolationNoPacketID},
		{"wildcard topic", wildcard, packets.ErrProtocolViolationSurplusWildcard},
		{"not a publish", notPublish, packets.ErrProtocolViolation},
		{"empty topic", emptyTopic, packets.ErrProtocolViolationNoTopic},
	} {
		if err := s.RetainOnly(inj, tc.pk); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if n := s.Topics.Retained.Len(); n != 0 {
		t.Errorf("%d retained messages stored by invalid packets", n)
	}
	if _, retained, _, _, _ := h.snapshot(); len(retained) != 0 {
		t.Errorf("OnRetainMessage fired %d times for invalid packets", len(retained))
	}
}

func TestSendLWTMarksWill(t *testing.T) {
	s, h := newTestServer(t)
	sub := addSubscriber(t, s, "c2", packets.Subscription{Filter: "w/#"})

	cl := s.NewClient(nil, "t1", "c9", false)
	cl.Properties.ProtocolVersion = 5
	cl.Properties.Will = Will{Flag: 1, TopicName: "w/c9", Payload: []byte("gone"), Retain: true}
	s.sendLWT(cl)

	_, retained, willSent, _, _ := h.snapshot()
	if len(retained) != 1 || !retained[0].Will {
		t.Fatalf("OnRetainMessage: %d calls, Will must be set", len(retained))
	}
	if len(willSent) != 1 || !willSent[0].Will {
		t.Fatalf("OnWillSent: %d calls, Will must be set", len(willSent))
	}
	if stored, ok := s.Topics.Retained.Get("w/c9"); !ok || stored.Will {
		t.Error("retained will missing, or its stored copy keeps Will")
	}
	if got := outbound(sub); len(got) != 1 || got[0].Will {
		t.Error("subscriber must get the will once, without the Will mark")
	}

	plain := s.NewClient(nil, "t1", "c10", false)
	plain.Properties.ProtocolVersion = 5
	plain.Properties.Will = Will{Flag: 1, TopicName: "w/c10", Payload: []byte("gone")}
	s.sendLWT(plain)
	_, retained, willSent, _, _ = h.snapshot()
	if len(retained) != 1 {
		t.Errorf("non-retained will reached OnRetainMessage")
	}
	if len(willSent) != 2 || !willSent[1].Will {
		t.Error("OnWillSent for a non-retained will must have Will set")
	}
}

func TestSendDelayedLWTMarksWill(t *testing.T) {
	s, h := newTestServer(t)
	cl := s.NewClient(nil, "t1", "c9", false)
	cl.Properties.ProtocolVersion = 5
	cl.Properties.Will = Will{Flag: 1, TopicName: "w/c9", Payload: []byte("gone"), Retain: true, WillDelayInterval: 5}
	s.Clients.Add(cl)

	s.sendLWT(cl)
	pending, ok := s.loop.willDelayed.Get("c9")
	if !ok || !pending.Will {
		t.Fatal("delayed will not queued with Will set")
	}
	if _, retained, willSent, _, _ := h.snapshot(); len(retained) != 0 || len(willSent) != 0 {
		t.Fatal("delayed will fired before its delay")
	}

	s.sendDelayedLWT(time.Now().Unix() + 10)
	_, retained, willSent, _, _ := h.snapshot()
	if len(retained) != 1 || !retained[0].Will {
		t.Error("delayed retained will: OnRetainMessage must see Will")
	}
	if len(willSent) != 1 || !willSent[0].Will {
		t.Error("delayed will: OnWillSent must see Will")
	}
}

// wirePackets splits a byte stream into fixed-header type bytes and bodies.
func wirePackets(t *testing.T, b []byte) (types []byte, bodies [][]byte) {
	t.Helper()
	for len(b) > 0 {
		typ := b[0]
		n, mult, i := 0, 1, 1
		for {
			if i >= len(b) {
				t.Fatalf("truncated packet header")
			}
			n += int(b[i]&0x7f) * mult
			mult *= 128
			i++
			if b[i-1]&0x80 == 0 {
				break
			}
		}
		if i+n > len(b) {
			t.Fatalf("truncated packet body")
		}
		types = append(types, typ)
		bodies = append(bodies, b[i:i+n])
		b = b[i+n:]
	}
	return types, bodies
}

// dialPipe connects a v5 client over net.Pipe through EstablishConnection and
// records everything the server writes to it.
func dialPipe(t *testing.T, s *Server, listener string, connect packets.Packet) (wire func() []byte, done chan error) {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c2.Close() })

	var mu sync.Mutex
	var buf bytes.Buffer
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := c2.Read(b)
			mu.Lock()
			buf.Write(b[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()

	done = make(chan error, 1)
	go func() { done <- s.EstablishConnection(listener, c1) }()

	var enc bytes.Buffer
	if err := connect.ConnectEncode(&enc); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Write(enc.Bytes()); err != nil {
		t.Fatal(err)
	}

	return func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return append([]byte(nil), buf.Bytes()...)
	}, done
}

func connectPk(id string, will bool) packets.Packet {
	pk := packets.Packet{
		FixedHeader:     packets.FixedHeader{Type: packets.Connect},
		ProtocolVersion: 5,
		Connect: packets.ConnectParams{
			ProtocolName:     []byte("MQTT"),
			Clean:            true,
			Keepalive:        30,
			ClientIdentifier: id,
		},
	}
	if will {
		pk.Connect.WillFlag = true
		pk.Connect.WillTopic = "w/" + id
		pk.Connect.WillPayload = []byte("gone")
		pk.Connect.WillRetain = true
	}
	return pk
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCloseListenersThenClose(t *testing.T) {
	s, h := newTestServer(t)
	ml := listeners.NewMockListener("t1", ":1882")
	if err := s.AddListener(ml); err != nil {
		t.Fatal(err)
	}
	if err := s.Serve(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "listener serving", ml.IsServing)

	wire, connDone := dialPipe(t, s, "t1", connectPk("c1", true))
	waitFor(t, "session established", func() bool {
		_, _, _, _, est := h.snapshot()
		return len(est) == 1 && est[0] == "c1"
	})

	s.CloseListeners()

	select {
	case <-connDone:
	case <-time.After(5 * time.Second):
		t.Fatal("connection goroutine still running after CloseListeners")
	}
	_, _, willSent, _, _ := h.snapshot()
	if len(willSent) != 1 || !willSent[0].Will || willSent[0].TopicName != "w/c1" {
		t.Fatalf("the shutdown will must be sent before CloseListeners returns, got %d", len(willSent))
	}
	if _, ok := s.Clients.Get("c1"); ok {
		t.Error("clean-start client still in s.Clients")
	}

	types, bodies := wirePackets(t, wire())
	if len(types) != 2 || types[0]>>4 != packets.Connack || types[1]>>4 != packets.Disconnect {
		t.Fatalf("wire packet types = %x, want CONNACK, DISCONNECT", types)
	}
	if len(bodies[1]) == 0 || bodies[1][0] != packets.ErrServerShuttingDown.Code {
		t.Fatalf("DISCONNECT reason = %x, want %x", bodies[1], packets.ErrServerShuttingDown.Code)
	}

	select {
	case <-s.done:
		t.Fatal("CloseListeners closed s.done")
	default:
	}
	if h.onStopped.Load() != 0 || h.stopCalls.Load() != 0 {
		t.Fatal("CloseListeners must not call OnStopped or stop hooks")
	}
	if ml.IsServing() || s.Listeners.Len() != 0 {
		t.Error("listener not closed and removed")
	}

	if err := s.Publish("x/y", []byte("still up"), false, 0); err != nil {
		t.Fatal(err)
	}
	if published, _, _, _, _ := h.snapshot(); len(published) != 1 {
		t.Error("inline publish after CloseListeners did not reach OnPublished")
	}

	s.CloseListeners()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.done:
	default:
		t.Error("Close did not close s.done")
	}
	if h.onStopped.Load() != 1 || h.stopCalls.Load() != 1 {
		t.Errorf("OnStopped=%d hook Stop=%d after Close, want 1 and 1", h.onStopped.Load(), h.stopCalls.Load())
	}
}

func TestCloseWithoutCloseListeners(t *testing.T) {
	s, h := newTestServer(t)
	ml := listeners.NewMockListener("t1", ":1882")
	if err := s.AddListener(ml); err != nil {
		t.Fatal(err)
	}
	if err := s.Serve(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "listener serving", ml.IsServing)

	_, connDone := dialPipe(t, s, "t1", connectPk("c1", false))
	waitFor(t, "session established", func() bool {
		_, _, _, _, est := h.snapshot()
		return len(est) == 1
	})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-connDone:
	case <-time.After(5 * time.Second):
		t.Fatal("connection goroutine still running after Close")
	}
	if ml.IsServing() || h.onStopped.Load() != 1 {
		t.Error("Close did not stop the listener or fire OnStopped")
	}
}

func TestOnConnectRefusalEndsConnection(t *testing.T) {
	s, h := newTestServer(t)
	h.refuseID = "peerlink:x"
	wire, connDone := dialPipe(t, s, "t1", connectPk("peerlink:x", false))

	var err error
	select {
	case err = <-connDone:
	case <-time.After(5 * time.Second):
		t.Fatal("refused connection not ended")
	}
	if !errors.Is(err, packets.ErrClientIdentifierNotValid) {
		t.Errorf("EstablishConnection err = %v", err)
	}
	if h.authCalls.Load() != 0 {
		t.Error("OnConnectAuthenticate ran after an OnConnect refusal")
	}
	if _, ok := s.Clients.Get("peerlink:x"); ok {
		t.Error("refused client added to s.Clients")
	}
	waitFor(t, "CONNACK", func() bool { return len(wire()) > 0 })
	types, bodies := wirePackets(t, wire())
	if types[0]>>4 != packets.Connack || len(bodies[0]) < 2 || bodies[0][1] != packets.ErrClientIdentifierNotValid.Code {
		t.Errorf("CONNACK = %x %x, want reason %x", types, bodies, packets.ErrClientIdentifierNotValid.Code)
	}
}

// slowRetainHook records retained payloads after a short random delay, which widens the window
// between the retained store update and the hook.
type slowRetainHook struct {
	HookBase
	mu   sync.Mutex
	last string
	n    int
}

func (h *slowRetainHook) ID() string           { return "slow-retain" }
func (h *slowRetainHook) Provides(b byte) bool { return b == OnRetainMessage }
func (h *slowRetainHook) OnRetainMessage(_ *Client, pk packets.Packet, _ int64) {
	time.Sleep(time.Duration(len(pk.Payload)%3) * 50 * time.Microsecond)
	h.mu.Lock()
	h.last = string(pk.Payload)
	h.n++
	h.mu.Unlock()
}

// With SerializeRetained the hooks see the retained writes of one topic in the order they were
// applied, so the last value a hook saw is the stored one (PeerLink retained capture order).
func TestSerializeRetainedOrdersHooks(t *testing.T) {
	s := New(&Options{InlineClient: true, SerializeRetained: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	h := &slowRetainHook{}
	if err := s.AddHook(h, nil); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 5; round++ {
		var wg sync.WaitGroup
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				cl := s.NewClient(nil, "t", "p"+string(rune('a'+g)), true)
				cl.Properties.ProtocolVersion = 5
				for i := 0; i < 30; i++ {
					pk := publishPk("order/t", string(rune('a'+g))+string(rune('0'+i%10))+string(make([]byte, i%3)))
					pk.FixedHeader.Retain = true
					if err := s.InjectPacket(cl, pk); err != nil {
						t.Error(err)
						return
					}
				}
			}(g)
		}
		wg.Wait()
		stored, _ := s.Topics.Retained.Get("order/t")
		h.mu.Lock()
		last := h.last
		h.mu.Unlock()
		if string(stored.Payload) != last {
			t.Fatalf("round %d: stored %q, hook saw last %q", round, stored.Payload, last)
		}
	}
}

// A replica is not backlogged in the inflight store of an offline session unless
// QueueOfflineReplicas is set (plan 12.4, Receive.Queue).
func TestOfflineSessionSkipsReplicas(t *testing.T) {
	for _, queue := range []bool{false, true} {
		s, _ := newTestServer(t)
		s.Options.QueueOfflineReplicas = queue
		cl := addSubscriber(t, s, "x", packets.Subscription{Filter: "rv/q", Qos: 1})
		cl.Stop(errors.New("gone"))
		inj := newInjector(s, "node-a")
		pk := publishPk("rv/q", "r")
		pk.FixedHeader.Qos = 1
		pk.PacketID = 1
		pk.Origin = "y"
		pk.Forward = &packets.Forward{SourceNode: "node-a", ClientID: "y", Offset: 1}
		if err := s.InjectPacket(inj, pk); err != nil {
			t.Fatal(err)
		}
		want := 0
		if queue {
			want = 1
		}
		if n := cl.State.Inflight.Len(); n != want {
			t.Fatalf("queue %v: inflight %d after a replica, want %d", queue, n, want)
		}
		local := publishPk("rv/q", "l")
		local.FixedHeader.Qos = 1
		local.PacketID = 1
		if err := s.InjectPacket(s.NewClient(nil, "t", "local", true), local); err != nil {
			t.Fatal(err)
		}
		if n := cl.State.Inflight.Len(); n != want+1 {
			t.Fatalf("queue %v: local publish not kept for the offline session (%d)", queue, n)
		}
	}
}
