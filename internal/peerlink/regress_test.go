package peerlink

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/peerlink/wire"
)

// scriptedRetained wraps the engine map; the first Snapshot call runs first instead.
type scriptedRetained struct {
	memoryRetained
	calls      atomic.Int32
	first      func(ctx context.Context, fn func(pk packets.Packet) bool) error
	flushFails atomic.Int32
}

func (r *scriptedRetained) FlushReplicas(string) error {
	if r.flushFails.Add(-1) >= 0 {
		return errors.New("store down")
	}
	r.flushFails.Store(0)
	return nil
}

func (r *scriptedRetained) Snapshot(ctx context.Context, fn func(pk packets.Packet) bool) error {
	if r.calls.Add(1) == 1 && r.first != nil {
		return r.first(ctx, fn)
	}
	return r.memoryRetained.Snapshot(ctx, fn)
}

func retainedOn(t *testing.T, n *testNode, topic, payload string, created int64) {
	t.Helper()
	n.srv.Topics.RetainMessage(packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Publish, Retain: true},
		TopicName: topic, Payload: []byte(payload), Created: created, Origin: "dev"})
}

// An interrupted FILL snapshot is pulled again on the next session, although that HELLO resumes the
// same epoch (review finding 1/21).
func TestSnapshotRetriedAfterInterruption(t *testing.T) {
	addrA := freeAddr(t)
	ra := &scriptedRetained{}
	a := startNode(t, "node-a", addrA, []config.PeerConfig{{NodeID: "node-b"}}, func(_ *config.PeerLinkConfig, d *Deps) {
		d.Retained = ra
	})
	ra.srv = a.srv
	ra.first = func(ctx context.Context, fn func(pk packets.Packet) bool) error {
		return errors.New("store unavailable")
	}
	now := time.Now().Unix()
	retainedOn(t, a, "snap/one", "a1", now-5)
	retainedOn(t, a, "snap/two", "a2", now-5)

	b := startNode(t, "node-b", "", []config.PeerConfig{{NodeID: "node-a", Address: addrA, Serve: boolp(false)}})
	waitStreaming(t, b, "node-a")
	for _, topic := range []string{"snap/one", "snap/two"} {
		if _, ok := b.srv.Topics.Retained.Get(topic); !ok {
			t.Fatalf("%s missing after the retried snapshot", topic)
		}
	}
	ss := sourceStatus(b, "node-a")
	if ss.SnapshotsInterrupted != 1 || ss.Snapshots != 2 || ss.SnapshotFilled != 2 || ss.Sessions != 2 {
		t.Fatalf("interrupted %d snapshots %d filled %d sessions %d", ss.SnapshotsInterrupted, ss.Snapshots, ss.SnapshotFilled, ss.Sessions)
	}
	if hs := b.m.pullerByID["node-a"].snapPending.Load(); hs {
		t.Fatal("snapshot still pending after SNAPSHOT_END")
	}
}

// A snapshot build slower than the consumer's read deadline does not end the session: the consumer
// pings during the snapshot phase and the PONGs keep both deadlines alive.
func TestSlowSnapshotKeptAlive(t *testing.T) {
	addrA := freeAddr(t)
	ra := &scriptedRetained{}
	a := startNode(t, "node-a", addrA, []config.PeerConfig{{NodeID: "node-b"}}, func(_ *config.PeerLinkConfig, d *Deps) {
		d.Retained = ra
	})
	ra.srv = a.srv
	ra.first = func(ctx context.Context, fn func(pk packets.Packet) bool) error {
		select {
		case <-time.After(3 * time.Second): // > MaxWaitMs + KeepAlive = 2.2 s on the consumer
		case <-ctx.Done():
			return ctx.Err()
		}
		return ra.memoryRetained.Snapshot(ctx, fn)
	}
	retainedOn(t, a, "slow/one", "a1", time.Now().Unix())

	b := startNode(t, "node-b", "", []config.PeerConfig{{NodeID: "node-a", Address: addrA, Serve: boolp(false)}})
	eventually(t, 8*time.Second, "streaming after the slow snapshot", func() bool {
		return sourceStatus(b, "node-a").State == "STREAMING"
	})
	ss := sourceStatus(b, "node-a")
	if ss.SnapshotsInterrupted != 0 || ss.Sessions != 1 || ss.SnapshotFilled != 1 {
		t.Fatalf("interrupted %d sessions %d filled %d (%s)", ss.SnapshotsInterrupted, ss.Sessions, ss.SnapshotFilled, ss.LastError)
	}
}

// One BATCH frame with an absurd count must not make the consumer allocate per announced record
// (review finding 2/13): the batch is refused as a protocol fault and the link recovers.
func TestBatchCountBeyondRegionRefused(t *testing.T) {
	fs := newFakeSource(t)
	fs.push(func(f *wire.Fetch) []byte { return batchFrame(f.FetchID, f.Offset, 0, 0xFFFFFFFF) })
	fs.push(batchOf(rec("ok/1", "1", nil)))
	b := consumerOf(t, fs)
	eventually(t, 5*time.Second, "record after the refused batch", func() bool { return b.recv.count("ok/1") == 1 })
	g, ok := fs.lastGoAway()
	if !ok || g.Code != wire.GoAwayProtocol {
		t.Fatalf("goaway %+v %v", g, ok)
	}
	eventually(t, 5*time.Second, "commit after the refused batch", func() bool { return fs.commit.Load() == 2 })
}

// A session that supersedes a connected one and then fails before HELLO_OK leaves the consumer
// DISCONNECTED, so Drain does not wait for a session that does not exist (review finding 3).
func TestTakeoverFailureResetsConsumerState(t *testing.T) {
	a := sourceNode(t, func(c *config.PeerLinkConfig, _ *Deps) { c.Log.DrainOnShutdownMs = intp(3000) })
	r1 := dialRaw(t, a.addr)
	ok, isOK := r1.hello("node-b", "node-a", nil).(*wire.HelloOK)
	if !isOK {
		t.Fatal("first session refused")
	}
	eventually(t, 2*time.Second, "connected", func() bool { return consumerStatus(a, "node-b").State == "CONNECTED" })
	r2 := dialRaw(t, a.addr)
	expectGoAway(t, r2.hello("node-b", "node-a", func(h *wire.Hello) {
		h.LastEpoch = ok.Epoch
		h.ResumeOffset = 1 << 40
	}), wire.GoAwayOffsetOutOfRange)
	expectGoAway(t, r1.read(), wire.GoAwaySuperseded)
	eventually(t, 2*time.Second, "disconnected", func() bool { return consumerStatus(a, "node-b").State == "DISCONNECTED" })
	res := a.m.Drain(ctxTimeout(t, 5*time.Second))
	if res.Waited > time.Second {
		t.Fatalf("drain waited %v for a consumer without a session", res.Waited)
	}
}

// Refused HELLOs with attacker-chosen consumer ids do not grow the rate limiter (findings 4/14).
func TestRefusedHelloDoesNotGrowRateLimiter(t *testing.T) {
	a := sourceNode(t)
	for i := 0; i < 50; i++ {
		r := dialRaw(t, a.addr)
		expectGoAway(t, r.hello(fmt.Sprintf("x%06d", i), "node-a", nil), wire.GoAwayUnknownPeer)
		_ = r.c.Close()
	}
	a.m.rate.mu.Lock()
	n := len(a.m.rate.m)
	a.m.rate.mu.Unlock()
	if n > 2 {
		t.Fatalf("rate limiter holds %d keys after 50 refused ids", n)
	}
	var r rateLimiter
	for i := 0; i < 3*rateLimiterMax; i++ {
		r.allow(fmt.Sprint(i), time.Hour)
	}
	if len(r.m) > rateLimiterMax {
		t.Fatalf("rate limiter not bounded: %d", len(r.m))
	}
}

// Fetch.MaxWaitMs 0 (rejected by config validation, clamped here) must not turn an idle link into
// a busy loop of EMPTY batches (review finding 5).
func TestIdleLinkWithZeroMaxWaitIsNotBusy(t *testing.T) {
	_, b := pair(t, nil, []nodeOpt{func(c *config.PeerLinkConfig, _ *Deps) { c.Fetch.MaxWaitMs = intp(0) }})
	before := sourceStatus(b, "node-a").Batches
	time.Sleep(time.Second)
	if n := sourceStatus(b, "node-a").Batches - before; n > 150 {
		t.Fatalf("%d EMPTY batches in 1 s on an idle link", n)
	}
}

// A snapshot value older than the source process keeps its original Created on the receiver; its
// absolute expiry is unchanged (review finding 6).
func TestSnapshotKeepsOriginalCreated(t *testing.T) {
	addrA := freeAddr(t)
	a := startNode(t, "node-a", addrA, []config.PeerConfig{{NodeID: "node-b"}})
	now := time.Now().Unix()
	retainedOn(t, a, "old/plain", "p", now-3600)
	a.srv.Topics.RetainMessage(packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Publish, Retain: true},
		TopicName: "old/expiring", Payload: []byte("e"), Created: now - 3600, Expiry: now + 3600,
		Properties: packets.Properties{MessageExpiryInterval: 7200}})
	b := startNode(t, "node-b", "", []config.PeerConfig{{NodeID: "node-a", Address: addrA, Serve: boolp(false)}})
	waitStreaming(t, b, "node-a")
	pk, ok := b.srv.Topics.Retained.Get("old/plain")
	if !ok || pk.Created < now-3602 || pk.Created > now-3598 {
		t.Fatalf("receiver Created %d, source %d", pk.Created, now-3600)
	}
	pk, ok = b.srv.Topics.Retained.Get("old/expiring")
	if !ok || pk.Created < now-3602 || pk.Created > now-3598 {
		t.Fatalf("receiver Created %d, source %d", pk.Created, now-3600)
	}
	if pk.Expiry < now+3597 || pk.Expiry > now+3603 {
		t.Fatalf("receiver Expiry %d, source %d", pk.Expiry, now+3600)
	}
}

// A network client whose CONNECT username is not valid UTF-8 still has its publishes forwarded,
// without the username (review finding 8).
func TestInvalidUsernameForwardedWithoutIt(t *testing.T) {
	a, b := pair(t, nil, nil)
	for _, u := range [][]byte{{0xff, 0x00, 'x'}, []byte("m\xfcller"), []byte("ok-user")} {
		cl := a.srv.NewClient(nil, "tcp", "dev-"+string(u[len(u)-1:]), false)
		cl.Properties.ProtocolVersion = 5
		cl.Properties.Username = u
		cl.State.Inflight.ResetReceiveQuota(1024)
		if err := a.srv.InjectPacket(cl, packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Publish},
			TopicName: "rv/u", Payload: u}); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, 5*time.Second, "three records on B", func() bool { return b.recv.count("rv/u") == 3 })
	got := b.recv.byTopic("rv/u")
	if got[0].Forward.Username != "" || got[1].Forward.Username != "" || got[2].Forward.Username != "ok-user" {
		t.Fatalf("usernames %q %q %q", got[0].Forward.Username, got[1].Forward.Username, got[2].Forward.Username)
	}
	if n := a.m.Status().Log.UsernameStripped; n != 2 {
		t.Fatalf("usernameStripped %d", n)
	}
	if d := sourceStatus(b, "node-a").Dropped["malformed"]; d != 0 {
		t.Fatalf("malformed %d", d)
	}
}

// An expired retained delete still clears the receiver's value, silently (review finding 9).
func TestExpiredRetainedDeleteApplied(t *testing.T) {
	fs := newFakeSource(t)
	fs.push(batchOf(
		rec("rv/d", "v", func(r *wire.Record) { r.Flags = wire.FlagRetain }),
		rec("rv/d", "", func(r *wire.Record) { r.Flags = wire.FlagRetain; r.ExpirySec = 1; r.CaptureMonoMs = fakeMonoNow - 5000 }),
	))
	b := consumerOf(t, fs)
	eventually(t, 5*time.Second, "batch applied", func() bool { return fs.commit.Load() == 3 })
	if _, ok := b.srv.Topics.Retained.Get("rv/d"); ok {
		t.Fatal("expired retained delete not applied")
	}
	ss := sourceStatus(b, "node-a")
	if ss.Dropped["expired"] != 0 || ss.RetainedDiverged["expired"] != 0 || ss.RetainOnly != 1 {
		t.Fatalf("dropped %v diverged %v retainOnly %d", ss.Dropped, ss.RetainedDiverged, ss.RetainOnly)
	}
}

// A failing retained replica flush is counted in the source status (review finding 12).
func TestFlushErrorsCounted(t *testing.T) {
	rb := &scriptedRetained{}
	rb.flushFails.Store(1)
	a, b := pair(t, nil, []nodeOpt{func(_ *config.PeerLinkConfig, d *Deps) { d.Retained = rb }})
	rb.srv = b.srv
	publishPkt(t, a, packets.Packet{TopicName: "fe/1", Payload: []byte("x")})
	eventually(t, 5*time.Second, "flush error counted", func() bool { return sourceStatus(b, "node-a").RetainedFlushErrors >= 1 })
}

// The loopback endpoints refuse browser requests (Origin, rebinding Host), bound the request size
// and keep counting against the pre-auth limits while a request is served (findings 18/19).
func TestStatusEndpointHardening(t *testing.T) {
	a := sourceNode(t)
	for _, req := range []string{
		"POST /peerlink/v1/resync?source=node-c HTTP/1.1\r\nHost: 127.0.0.1\r\nOrigin: http://evil.example\r\n\r\n",
		"POST /peerlink/v1/resync?source=node-c HTTP/1.1\r\nHost: evil.example:1890\r\n\r\n",
		"GET /peerlink/v1/status HTTP/1.1\r\nHost: rebind.example\r\n\r\n",
	} {
		if code, _ := httpRaw(t, a.addr, req); code != 403 {
			t.Fatalf("%q: HTTP %d, want 403", req, code)
		}
	}
	for _, host := range []string{"localhost:1890", "[::1]:1890", "127.0.0.1"} {
		if code, _ := httpRaw(t, a.addr, "GET /peerlink/v1/status HTTP/1.1\r\nHost: "+host+"\r\n\r\n"); code != 200 {
			t.Fatalf("Host %s: HTTP %d", host, code)
		}
	}

	// Oversized headers: the connection ends without a response.
	c, err := net.Dial("tcp", a.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	go func() {
		_, _ = c.Write([]byte("GET /peerlink/v1/status HTTP/1.1\r\nHost: 127.0.0.1\r\n"))
		line := []byte("X-Pad: " + strings.Repeat("a", 8<<10) + "\r\n")
		for i := 0; i < 64; i++ {
			if _, err := c.Write(line); err != nil {
				return
			}
		}
	}()
	resp, err := io.ReadAll(c)
	if len(resp) > 0 {
		t.Fatalf("oversized request answered: %q (err %v)", resp[:min(len(resp), 40)], err)
	}

	// Idle HTTP connections hold pre-auth slots (MaxPreAuthPerIp 2): a third one is refused.
	var held []net.Conn
	for i := 0; i < 2; i++ {
		c, err := net.Dial("tcp", a.addr)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
		_, _ = c.Write([]byte("GET /peerlink/v1/status HTTP/1.1\r\n"))
	}
	defer func() {
		for _, c := range held {
			_ = c.Close()
		}
	}()
	eventually(t, 2*time.Second, "two held", func() bool { return a.m.Status().Admission.PreAuth == 2 })
	busy := a.m.Status().Admission.RefusedBusy
	c3, err := net.Dial("tcp", a.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c3.Close()
	eventually(t, 2*time.Second, "third refused", func() bool { return a.m.Status().Admission.RefusedBusy == busy+1 })
}

// Idle connections from other hosts cannot lock out a consumer without a configured Address once
// it authenticated from its IP (review finding 15).
func TestPreAuthCapDoesNotStarveAuthenticatedPeer(t *testing.T) {
	a := sourceNode(t)
	r := dialRaw(t, a.addr)
	if _, ok := r.hello("node-b", "node-a", nil).(*wire.HelloOK); !ok {
		t.Fatal("first session refused")
	}
	_ = r.c.Close()
	var held []net.Conn
	defer func() {
		for _, c := range held {
			_ = c.Close()
		}
	}()
	for i := 2; i <= 9; i++ {
		d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, byte(i))}}
		for j := 0; j < 2; j++ {
			c, err := d.Dial("tcp", a.addr)
			if err != nil {
				t.Skipf("cannot bind 127.0.0.%d: %v", i, err)
			}
			held = append(held, c)
		}
	}
	eventually(t, 2*time.Second, "16 idle pre-auth connections", func() bool { return a.m.Status().Admission.PreAuth == maxPreAuthTotal })
	r2 := dialRaw(t, a.addr) // fails the test unless SERVER_HELLO arrives
	if _, ok := r2.hello("node-b", "node-a", nil).(*wire.HelloOK); !ok {
		t.Fatal("authenticated peer locked out by idle connections")
	}
}

func TestAdmissionKey(t *testing.T) {
	a := admissionKey(net.ParseIP("2001:db8:1:2::1"))
	b := admissionKey(net.ParseIP("2001:db8:1:2:ffff::9"))
	c := admissionKey(net.ParseIP("2001:db8:1:3::1"))
	if a != b || a == c {
		t.Fatalf("IPv6 keys %q %q %q", a, b, c)
	}
	if k := admissionKey(net.ParseIP("10.1.2.3")); k != "10.1.2.3" {
		t.Fatalf("IPv4 key %q", k)
	}
}

// A clock difference to the source above 1 s is logged as a WARN (review finding 28).
func TestClockSkewWarning(t *testing.T) {
	var buf syncBuffer
	n := newNode(t, "node-b", "", []config.PeerConfig{{NodeID: "node-a", Address: "127.0.0.1:1", Serve: boolp(false)}},
		func(_ *config.PeerLinkConfig, d *Deps) { d.Logger = slog.New(slog.NewTextHandler(&buf, nil)) })
	p := n.m.pullerByID["node-a"]
	p.warnSkew(400)
	if strings.Contains(buf.String(), "NTP") {
		t.Fatal("WARN below the threshold")
	}
	p.warnSkew(-2500)
	p.warnSkew(3000)
	if c := strings.Count(buf.String(), "NTP"); c != 1 {
		t.Fatalf("%d skew WARNs, want 1 (rate limited): %s", c, buf.String())
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
