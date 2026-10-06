package peerlink

import (
	"bufio"
	"net"
	"testing"
	"time"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/peerlink/wire"
)

// rawConsumer is a scripted consumer built on the wire codec, talking to a node's real listener.
type rawConsumer struct {
	t  *testing.T
	c  net.Conn
	fr *wire.FrameReader
	sh wire.ServerHello
}

func dialRaw(t *testing.T, addr string) *rawConsumer {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	r := &rawConsumer{t: t, c: c, fr: wire.NewFrameReader(bufio.NewReader(c), wire.DefaultMaxFrameBytes)}
	if err := wire.WritePreamble(c); err != nil {
		t.Fatal(err)
	}
	f := r.read()
	sh, ok := f.(*wire.ServerHello)
	if !ok {
		t.Fatalf("expected SERVER_HELLO, got %T", f)
	}
	r.sh = *sh
	return r
}

func (r *rawConsumer) read() wire.Frame {
	r.t.Helper()
	t, body, err := r.fr.ReadFrame()
	if err != nil {
		r.t.Fatalf("read: %v", err)
	}
	f, err := wire.DecodeFrame(t, body)
	if err != nil {
		r.t.Fatalf("decode %s: %v", t, err)
	}
	return f
}

func (r *rawConsumer) write(f wire.Frame) {
	r.t.Helper()
	if err := wire.WriteFrame(r.c, f); err != nil {
		r.t.Fatalf("write: %v", err)
	}
}

func (r *rawConsumer) hello(consumer, expected string, mod func(*wire.Hello)) wire.Frame {
	r.t.Helper()
	h := wire.Hello{Capabilities: wire.CapsV1, InstanceID: 7, ConsumerNodeID: consumer, ExpectedSourceNodeID: expected}
	if mod != nil {
		mod(&h)
	}
	r.write(&h)
	return r.read()
}

func expectGoAway(t *testing.T, f wire.Frame, code wire.GoAwayCode) *wire.GoAway {
	t.Helper()
	g, ok := f.(*wire.GoAway)
	if !ok {
		t.Fatalf("expected GOAWAY %s, got %T %+v", code, f, f)
	}
	if g.Code != code {
		t.Fatalf("GOAWAY %s (%q), want %s", g.Code, g.Reason, code)
	}
	return g
}

func sourceNode(t *testing.T, opts ...nodeOpt) *testNode {
	peers := []config.PeerConfig{{NodeID: "node-b"}, {NodeID: "node-c", Address: "127.0.0.1:1", Serve: boolp(false)}}
	return startNode(t, "node-a", "127.0.0.1:0", peers, opts...)
}

func TestHandshakeChecks(t *testing.T) {
	a := sourceNode(t, func(c *config.PeerLinkConfig, _ *Deps) { c.Fetch.ReconnectMaxMs = intp(60000) })
	cases := []struct {
		name               string
		consumer, expected string
		code               wire.GoAwayCode
	}{
		{"self connection", "node-a", "node-a", wire.GoAwaySelfConnection},
		{"self connection case-insensitive", "NODE-A", "node-a", wire.GoAwaySelfConnection},
		{"unknown peer", "node-x", "node-a", wire.GoAwayUnknownPeer},
		{"serve false", "node-c", "node-a", wire.GoAwayNotAllowed},
		{"crossed address", "node-b", "node-z", wire.GoAwayWrongNode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := dialRaw(t, a.addr)
			g := expectGoAway(t, r.hello(tc.consumer, tc.expected, nil), tc.code)
			if g.Reason == "" {
				t.Fatal("reason empty although no authentication is configured")
			}
		})
	}
	if a.m.Status().Admission.AuthFailures["unknown_peer"] != 1 {
		t.Fatalf("auth failures %+v", a.m.Status().Admission.AuthFailures)
	}
}

func TestHandshakeVersionMismatch(t *testing.T) {
	a := sourceNode(t)
	c, err := net.Dial("tcp", a.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(wire.AppendPreambleVersion(nil, 2, 0)); err != nil {
		t.Fatal(err)
	}
	r := &rawConsumer{t: t, c: c, fr: wire.NewFrameReader(bufio.NewReader(c), wire.MaxPreAuthFrame)}
	expectGoAway(t, r.read(), wire.GoAwayVersion)
}

func TestHandshakeGenericAuthFailedWhenAuthConfigured(t *testing.T) {
	// A secret configured on the listener makes every pre-auth refusal a reason-less auth_failed.
	a := sourceNode(t, func(c *config.PeerLinkConfig, _ *Deps) {
		c.Peers[0].SharedSecrets = []string{"MDEyMzQ1Njc4OWFiY2RlZg=="}
	})
	r := dialRaw(t, a.addr)
	if r.sh.AuthModes&wire.AuthSharedSecret == 0 {
		t.Fatal("authModes does not announce the shared secret")
	}
	g := expectGoAway(t, r.hello("node-x", "node-a", nil), wire.GoAwayAuthFailed)
	if g.Reason != "" {
		t.Fatalf("reason leaked before authentication: %q", g.Reason)
	}
	// Plaintext cannot carry the exporter-bound MAC, and the waiver does not admit a peer that has
	// a secret without it (review finding 16).
	expectGoAway(t, dialRaw(t, a.addr).hello("node-b", "node-a", nil), wire.GoAwayAuthFailed)
}

func TestHandshakeFailClosedWithoutWaiver(t *testing.T) {
	a := sourceNode(t, func(c *config.PeerLinkConfig, _ *Deps) { c.AllowUnauthenticatedPeers = false })
	expectGoAway(t, dialRaw(t, a.addr).hello("node-b", "node-a", nil), wire.GoAwayAuthFailed)
}

func TestResumeRules(t *testing.T) {
	a := sourceNode(t)
	for i := 0; i < 5; i++ {
		publishPkt(t, a, packets.Packet{TopicName: "r", Payload: []byte{byte(i)}})
	}
	epoch := a.m.Status().Epoch

	// First contact: resume at C[c] = 1, snapshot offered.
	ok := dialRaw(t, a.addr).hello("node-b", "node-a", nil).(*wire.HelloOK)
	if ok.ResumeAt != 1 || ok.Epoch != epoch || ok.Leo != 6 || ok.Flags&wire.HelloOKSnapshotAvailable == 0 ||
		ok.Flags&wire.HelloOKSourceReset != 0 || ok.SourceNodeID != "node-a" || ok.TopicRoot != "" {
		t.Fatalf("first contact %+v", ok)
	}
	// Same epoch, resume offset inside the log: consumer state used, the commit raised.
	r := dialRaw(t, a.addr)
	ok = r.hello("node-b", "node-a", func(h *wire.Hello) { h.LastEpoch, h.ResumeOffset = epoch, 4 }).(*wire.HelloOK)
	if ok.ResumeAt != 4 || ok.Flags&wire.HelloOKConsumerStateUsed == 0 || ok.Flags&wire.HelloOKSnapshotAvailable != 0 || ok.Committed != 4 {
		t.Fatalf("same epoch %+v", ok)
	}
	// Source reset: an unknown epoch.
	ok = dialRaw(t, a.addr).hello("node-b", "node-a", func(h *wire.Hello) { h.LastEpoch, h.ResumeOffset = epoch+1, 99 }).(*wire.HelloOK)
	if ok.Flags&wire.HelloOKSourceReset == 0 || ok.ResumeAt != 4 || ok.Flags&wire.HelloOKSnapshotAvailable == 0 {
		t.Fatalf("source reset %+v", ok)
	}
	// Same epoch beyond leo.
	expectGoAway(t, dialRaw(t, a.addr).hello("node-b", "node-a", func(h *wire.Hello) { h.LastEpoch, h.ResumeOffset = epoch, 100 }),
		wire.GoAwayOffsetOutOfRange)
	if st := consumerStatus(a, "node-b"); st.Sessions < 3 {
		t.Fatalf("sessions %d", st.Sessions)
	}
}

func TestFetchServesBatchesAndGap(t *testing.T) {
	a := sourceNode(t, func(c *config.PeerLinkConfig, _ *Deps) { c.Log.MaxMessages = intp(10) })
	for i := 0; i < 15; i++ {
		publishPkt(t, a, packets.Packet{TopicName: "g", Payload: []byte{byte(i)}})
	}
	r := dialRaw(t, a.addr)
	ok := r.hello("node-b", "node-a", nil).(*wire.HelloOK)
	if ok.LostOnResume != 5 || ok.ResumeAt != 6 {
		t.Fatalf("hello ok %+v", ok)
	}
	// FETCH below lso: GAP with the lost count.
	r.write(&wire.Fetch{FetchID: 1, Offset: 3, MaxRecords: 4, MaxBytes: 1 << 20, MinRecords: 1})
	b := r.read().(*wire.Batch)
	if b.Header.Flags&wire.BatchFlagGap == 0 || b.Header.Lost != 3 || b.Header.BaseOffset != 6 || b.Header.Count != 4 || b.Header.FetchID != 1 {
		t.Fatalf("gap batch %+v", b.Header)
	}
	if !b.CRCValid() || b.Header.Flags&wire.BatchFlagCRC == 0 {
		t.Fatal("plaintext batch without a valid CRC")
	}
	it := b.Iter()
	var v wire.RecordView
	for i := 0; ; i++ {
		ok, err := it.Next(&v)
		if !ok {
			if err != nil || i != 4 {
				t.Fatalf("records %d err %v", i, err)
			}
			break
		}
		if err != nil || string(v.Topic) != "g" || v.Payload[0] != byte(5+i) || !v.Inline() {
			t.Fatalf("record %d: %v %q %v", i, err, v.Topic, v.Payload)
		}
	}
	// Long poll at leo returns EMPTY after maxWaitMs.
	start := time.Now()
	r.write(&wire.Fetch{FetchID: 2, Offset: 16, Commit: 16, MaxRecords: 10, MaxBytes: 1 << 20, MinRecords: 1, MaxWaitMs: 150})
	b = r.read().(*wire.Batch)
	if b.Header.Flags&wire.BatchFlagEmpty == 0 || b.Header.Count != 0 || time.Since(start) < 100*time.Millisecond {
		t.Fatalf("empty batch %+v after %v", b.Header, time.Since(start))
	}
	eventually(t, 2*time.Second, "commit", func() bool { return consumerStatus(a, "node-b").Committed == 16 })
	// A publish wakes a waiting FETCH.
	r.write(&wire.Fetch{FetchID: 3, Offset: 16, MaxRecords: 10, MaxBytes: 1 << 20, MinRecords: 1, MaxWaitMs: 5000})
	time.Sleep(50 * time.Millisecond)
	publishPkt(t, a, packets.Packet{TopicName: "g", Payload: []byte("new")})
	b = r.read().(*wire.Batch)
	if b.Header.Count != 1 || b.Header.BaseOffset != 16 {
		t.Fatalf("woken batch %+v", b.Header)
	}
	// PING is answered.
	r.write(&wire.Ping{Token: 42})
	if p, ok := r.read().(*wire.Pong); !ok || p.Token != 42 {
		t.Fatalf("pong %+v", p)
	}
	// Commit beyond leo is a protocol error.
	r.write(&wire.Commit{Commit: 1000})
	expectGoAway(t, r.read(), wire.GoAwayProtocol)
}

func TestFetchBeyondLeoAndOversizeFrame(t *testing.T) {
	a := sourceNode(t)
	r := dialRaw(t, a.addr)
	r.hello("node-b", "node-a", nil)
	r.write(&wire.Fetch{FetchID: 1, Offset: 50, MaxRecords: 1, MinRecords: 1})
	expectGoAway(t, r.read(), wire.GoAwayOffsetOutOfRange)

	r = dialRaw(t, a.addr)
	r.hello("node-b", "node-a", nil)
	big := make([]byte, wire.MaxConsumerFrame+10)
	big[0], big[1], big[2], big[3] = byte(len(big)-4), byte((len(big)-4)>>8), byte((len(big)-4)>>16), 0
	big[4] = byte(wire.FrameCommit)
	_, _ = r.c.Write(big[:5])
	expectGoAway(t, r.read(), wire.GoAwayProtocol)
}

func TestTombstoneForOversizeRecord(t *testing.T) {
	a := sourceNode(t)
	publishPkt(t, a, packets.Packet{TopicName: "big", Payload: make([]byte, 4000), FixedHeader: packets.FixedHeader{Retain: true}})
	publishPkt(t, a, packets.Packet{TopicName: "small", Payload: []byte("x")})
	r := dialRaw(t, a.addr)
	r.hello("node-b", "node-a", func(h *wire.Hello) { h.MaxRecordBytes = 1000 })
	r.write(&wire.Fetch{FetchID: 1, Offset: 1, MaxRecords: 10, MaxBytes: 1 << 20, MinRecords: 1})
	b := r.read().(*wire.Batch)
	if b.Header.Count != 2 {
		t.Fatalf("count %d", b.Header.Count)
	}
	it := b.Iter()
	var v wire.RecordView
	if ok, err := it.Next(&v); !ok || err != nil || !v.Skipped() || !v.Retain() || len(v.Frame) != wire.TombstoneLen {
		t.Fatalf("tombstone %v %v %+v", ok, err, v)
	}
	if ok, err := it.Next(&v); !ok || err != nil || string(v.Topic) != "small" {
		t.Fatalf("second %v %v", ok, err)
	}
	if consumerStatus(a, "node-b").ServedSkipped["size"] != 1 {
		t.Fatal("servedSkipped not counted")
	}
}

func TestSessionTakeoverAndDuplicate(t *testing.T) {
	a := sourceNode(t)
	r1 := dialRaw(t, a.addr)
	r1.hello("node-b", "node-a", func(h *wire.Hello) { h.InstanceID = 1 })
	r2 := dialRaw(t, a.addr)
	r2.hello("node-b", "node-a", func(h *wire.Hello) { h.InstanceID = 1 })
	expectGoAway(t, r1.read(), wire.GoAwaySuperseded)

	// Two processes alternating on one NodeId.
	r3 := dialRaw(t, a.addr)
	r3.hello("node-b", "node-a", func(h *wire.Hello) { h.InstanceID = 2 })
	expectGoAway(t, r2.read(), wire.GoAwaySuperseded)
	r4 := dialRaw(t, a.addr)
	r4.hello("node-b", "node-a", func(h *wire.Hello) { h.InstanceID = 1 })
	expectGoAway(t, r3.read(), wire.GoAwaySuperseded)
	r5 := dialRaw(t, a.addr)
	expectGoAway(t, r5.hello("node-b", "node-a", func(h *wire.Hello) { h.InstanceID = 2 }), wire.GoAwayDuplicateNode)
	if consumerStatus(a, "node-b").DuplicateConsumer != 1 {
		t.Fatal("duplicateConsumer not counted")
	}
	// The refused instance stays refused; the established one keeps streaming.
	expectGoAway(t, dialRaw(t, a.addr).hello("node-b", "node-a", func(h *wire.Hello) { h.InstanceID = 2 }), wire.GoAwayDuplicateNode)
	r4.write(&wire.Ping{Token: 1})
	if _, ok := r4.read().(*wire.Pong); !ok {
		t.Fatal("established instance lost its session")
	}
}

func TestAlternating(t *testing.T) {
	now := time.Now()
	tk := func(f, to uint64) takeover { return takeover{at: now, from: f, to: to} }
	cases := []struct {
		ts   []takeover
		want bool
	}{
		{[]takeover{tk(1, 2)}, false},
		{[]takeover{tk(1, 2), tk(2, 1)}, false},
		{[]takeover{tk(1, 2), tk(2, 1), tk(1, 2)}, true},
		{[]takeover{tk(1, 1), tk(1, 1), tk(1, 1)}, false},
		{[]takeover{tk(1, 2), tk(2, 3), tk(3, 4)}, false},
	}
	for i, c := range cases {
		if got := alternating(c.ts); got != c.want {
			t.Errorf("case %d: %v", i, got)
		}
	}
}

func TestAdmission(t *testing.T) {
	a := sourceNode(t, func(c *config.PeerLinkConfig, _ *Deps) { c.Listener.MaxPreAuthPerIp = intp(2) })
	var held []net.Conn
	for i := 0; i < 2; i++ {
		c, err := net.Dial("tcp", a.addr)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	defer func() {
		for _, c := range held {
			_ = c.Close()
		}
	}()
	eventually(t, 2*time.Second, "two pre-auth connections", func() bool { return a.m.Status().Admission.PreAuth == 2 })
	c, err := net.Dial("tcp", a.addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("third pre-auth connection from one IP was served")
	}
	_ = c.Close()
	if a.m.Status().Admission.RefusedBusy != 1 {
		t.Fatalf("refusedBusy %d", a.m.Status().Admission.RefusedBusy)
	}

	b := sourceNode(t, func(c *config.PeerLinkConfig, _ *Deps) { c.Listener.AllowedNetworks = []string{"10.99.0.0/16"} })
	c, err = net.Dial("tcp", b.addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = c.Write(wire.AppendPreamble(nil))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("disallowed network served")
	}
	_ = c.Close()
	eventually(t, 2*time.Second, "refusedNetwork", func() bool { return b.m.Status().Admission.RefusedNetwork == 1 })
}

func TestSnapshotFetchSkippedForOARetained(t *testing.T) {
	oa := func(c *config.PeerLinkConfig, d *Deps) {
		c.Peers[0].RedundancyPartner = true
		d.RetainedClass = wire.RetainedWinCCOA
		d.OASystem = func() string { return "System1" }
		d.NamespaceRoot = func() string { return "winccoa" }
	}
	a := sourceNode(t, oa)
	ok := dialRaw(t, a.addr).hello("node-b", "node-a", func(h *wire.Hello) {
		h.RetainedClass, h.OASystem, h.TopicRoot = wire.RetainedWinCCOA, "System1", "other"
	}).(*wire.HelloOK)
	if ok.Flags&wire.HelloOKSnapshotAvailable != 0 || ok.OASystem != "System1" || ok.TopicRoot != "winccoa" || ok.RetainedClass != wire.RetainedWinCCOA {
		t.Fatalf("hello ok %+v", ok)
	}
	c := consumerStatus(a, "node-b")
	if !c.OARetained || !c.TopicRootMismatch || c.RetainedClassMismatch {
		t.Fatalf("consumer flags %+v", c)
	}
	ok = dialRaw(t, a.addr).hello("node-b", "node-a", func(h *wire.Hello) {
		h.RetainedClass, h.OASystem = wire.RetainedWinCCOA, "System2"
	}).(*wire.HelloOK)
	if ok.Flags&wire.HelloOKSnapshotAvailable == 0 || consumerStatus(a, "node-b").OARetained {
		t.Fatal("different OA systems treated as one")
	}
}

func TestNoOARetainedWithoutRedundancyPartner(t *testing.T) {
	a := sourceNode(t, func(_ *config.PeerLinkConfig, d *Deps) {
		d.RetainedClass = wire.RetainedWinCCOA
		d.OASystem = func() string { return "System1" }
	})
	ok := dialRaw(t, a.addr).hello("node-b", "node-a", func(h *wire.Hello) {
		h.RetainedClass, h.OASystem = wire.RetainedWinCCOA, "System1"
	}).(*wire.HelloOK)
	if ok.Flags&wire.HelloOKSnapshotAvailable == 0 || consumerStatus(a, "node-b").OARetained {
		t.Fatal("two standalone OA systems treated as one redundant system")
	}
}

func TestOARetainedDecision(t *testing.T) {
	cases := []struct {
		partner    bool
		l, r       wire.RetainedClass
		ls, rs     string
		oaRetained bool
		warn       bool
	}{
		{true, wire.RetainedWinCCOA, wire.RetainedWinCCOA, "System1", "System1", true, false},
		{false, wire.RetainedWinCCOA, wire.RetainedWinCCOA, "System1", "System1", false, false},
		{true, wire.RetainedWinCCOA, wire.RetainedWinCCOA, "System1", "System2", false, true},
		{true, wire.RetainedWinCCOA, wire.RetainedWinCCOA, "", "", false, true},
		{true, wire.RetainedWinCCOA, wire.RetainedDB, "System1", "System1", false, true},
		{false, wire.RetainedWinCCOA, wire.RetainedDB, "System1", "System1", false, false},
		{true, wire.RetainedMemory, wire.RetainedMemory, "System1", "System1", false, false},
	}
	for i, c := range cases {
		got, why := oaRetainedFor(c.partner, c.l, c.ls, c.r, c.rs)
		if got != c.oaRetained || (why != "") != c.warn {
			t.Errorf("case %d: %v %q", i, got, why)
		}
	}
}
