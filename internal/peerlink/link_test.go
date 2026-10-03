package peerlink

import (
	"bytes"
	"testing"
	"time"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/mqtt/packets"
)

// pair starts A (serves B) and B (pulls from A), one direction.
func pair(t *testing.T, aOpts, bOpts []nodeOpt) (a, b *testNode) {
	t.Helper()
	a = startNode(t, "node-a", "127.0.0.1:0", []config.PeerConfig{{NodeID: "node-b"}}, aOpts...)
	b = startNode(t, "node-b", "", []config.PeerConfig{{NodeID: "node-a", Address: a.addr, Serve: boolp(false)}}, bOpts...)
	waitStreaming(t, b, "node-a")
	return a, b
}

func TestLinkForwardsWithFidelity(t *testing.T) {
	a, b := pair(t, nil, nil)

	pk := packets.Packet{
		FixedHeader: packets.FixedHeader{Qos: 1},
		TopicName:   "plant/line1/temp",
		Payload:     []byte("21.5"),
		Properties: packets.Properties{
			ContentType:           "text/plain",
			ResponseTopic:         "plant/reply",
			CorrelationData:       []byte{1, 2, 3},
			PayloadFormat:         1,
			PayloadFormatFlag:     true,
			MessageExpiryInterval: 600,
			User:                  []packets.UserProperty{{Key: "k", Val: "1"}, {Key: "k", Val: "2"}, {Key: "a", Val: "b"}},
		},
	}
	clientPublish(t, a, "dev1", packets.Packet{FixedHeader: packets.FixedHeader{Qos: 0}, TopicName: "plant/line1/state", Payload: []byte("run")})
	publishPkt(t, a, pk)

	eventually(t, 5*time.Second, "delivery on B", func() bool { return len(b.recv.byTopic("plant/line1/temp")) == 1 })
	got := b.recv.byTopic("plant/line1/temp")[0]
	if !bytes.Equal(got.Payload, pk.Payload) || got.FixedHeader.Qos != 1 {
		t.Fatalf("payload/qos: %q qos %d", got.Payload, got.FixedHeader.Qos)
	}
	p := got.Properties
	if p.ContentType != "text/plain" || p.ResponseTopic != "plant/reply" || !bytes.Equal(p.CorrelationData, []byte{1, 2, 3}) ||
		!p.PayloadFormatFlag || p.PayloadFormat != 1 {
		t.Fatalf("properties not carried: %+v", p)
	}
	if len(p.User) != 3 || p.User[0] != (packets.UserProperty{Key: "k", Val: "1"}) || p.User[1].Val != "2" || p.User[2].Key != "a" {
		t.Fatalf("user properties order/duplicates lost: %+v", p.User)
	}
	if p.MessageExpiryInterval == 0 || p.MessageExpiryInterval > 600 {
		t.Fatalf("expiry %d", p.MessageExpiryInterval)
	}
	if got.Forward == nil || got.Forward.SourceNode != "node-a" || got.Forward.ClientID != "inline" || got.Forward.Epoch == 0 || got.Forward.Offset == 0 {
		t.Fatalf("forward metadata: %+v", got.Forward)
	}
	if got.Origin != "inline" {
		t.Fatalf("origin %q", got.Origin)
	}
	eventually(t, 5*time.Second, "client publish on B", func() bool { return len(b.recv.byTopic("plant/line1/state")) == 1 })
	st := b.recv.byTopic("plant/line1/state")[0]
	if st.Forward.ClientID != "dev1" || st.Origin != "dev1" {
		t.Fatalf("publisher not carried: %+v origin %q", st.Forward, st.Origin)
	}

	// Split horizon: B counted every injected replica in skipPeer and appended nothing.
	eventually(t, 5*time.Second, "counters", func() bool {
		ss := sourceStatus(b, "node-a")
		return ss.Injected == 2 && b.m.Status().Log.SkipPeer == 2
	})
	as := a.m.Status()
	if as.Log.Appended.Client != 1 || as.Log.Appended.Inline != 1 {
		t.Fatalf("appended on A: %+v", as.Log.Appended)
	}
	eventually(t, 5*time.Second, "commit on A", func() bool {
		c := consumerStatus(a, "node-b")
		return c.Committed == 3 && c.Lag == 0 && c.State == "CONNECTED"
	})
	eventually(t, 5*time.Second, "trim on A", func() bool {
		ls := a.m.Status().Log
		return ls.LSO == ls.LEO && ls.Records == 0
	})
}

func TestLinkRetainedSetAndDelete(t *testing.T) {
	a, b := pair(t, nil, nil)
	publishPkt(t, a, packets.Packet{FixedHeader: packets.FixedHeader{Retain: true, Qos: 1}, TopicName: "cfg/x", Payload: []byte("v1")})
	eventually(t, 5*time.Second, "retained on B", func() bool {
		pk, ok := b.srv.Topics.Retained.Get("cfg/x")
		return ok && string(pk.Payload) == "v1"
	})
	pk, _ := b.srv.Topics.Retained.Get("cfg/x")
	if pk.Origin != "inline" || pk.Created == 0 {
		t.Fatalf("retained replica origin %q created %d", pk.Origin, pk.Created)
	}
	publishPkt(t, a, packets.Packet{FixedHeader: packets.FixedHeader{Retain: true}, TopicName: "cfg/x"})
	eventually(t, 5*time.Second, "retained delete on B", func() bool {
		_, ok := b.srv.Topics.Retained.Get("cfg/x")
		return !ok
	})
	// The retained publish was captured once (in OnRetainMessage), not again in OnPublished.
	if got := a.m.Status().Log.Appended.Inline; got != 2 {
		t.Fatalf("appended inline %d, want 2", got)
	}
}

func TestBidirectionalNoEcho(t *testing.T) {
	addrA, addrB := freeAddr(t), freeAddr(t)
	a := startNode(t, "node-a", addrA, []config.PeerConfig{{NodeID: "node-b", Address: addrB}})
	b := startNode(t, "node-b", addrB, []config.PeerConfig{{NodeID: "node-a", Address: addrA}})
	waitStreaming(t, a, "node-b")
	waitStreaming(t, b, "node-a")

	for i := 0; i < 20; i++ {
		clientPublish(t, a, "c-a", packets.Packet{TopicName: "x/a", Payload: []byte{byte(i)}})
		clientPublish(t, b, "c-b", packets.Packet{TopicName: "x/b", Payload: []byte{byte(i)}})
	}
	eventually(t, 5*time.Second, "both directions", func() bool {
		return a.recv.count("x/b") == 20 && b.recv.count("x/a") == 20
	})
	time.Sleep(300 * time.Millisecond)
	if a.recv.count("x/a") != 20 || b.recv.count("x/b") != 20 || a.recv.count("x/b") != 20 || b.recv.count("x/a") != 20 {
		t.Fatalf("echo: A x/a %d x/b %d, B x/a %d x/b %d", a.recv.count("x/a"), a.recv.count("x/b"), b.recv.count("x/a"), b.recv.count("x/b"))
	}
	for _, n := range []*testNode{a, b} {
		st := n.m.Status()
		if st.Log.Appended.Client != 20 {
			t.Fatalf("%s appended client %d", n.id, st.Log.Appended.Client)
		}
		if st.Log.SkipPeer != st.Sources[0].Injected || st.Log.SkipPeer != 20 {
			t.Fatalf("%s skipPeer %d injected %d", n.id, st.Log.SkipPeer, st.Sources[0].Injected)
		}
	}
}

func TestNamespaceExclusionOnlyWhenNativeActive(t *testing.T) {
	native := false
	root := func() string {
		if native {
			return "winccoa"
		}
		return ""
	}
	withRoot := func(_ *config.PeerLinkConfig, d *Deps) { d.NamespaceRoot = root }
	a, b := pair(t, []nodeOpt{withRoot}, nil)

	publishPkt(t, a, packets.Packet{TopicName: "winccoa/sys/a", Payload: []byte("1")})
	publishPkt(t, a, packets.Packet{TopicName: "$SYS/x", Payload: []byte("1")})
	publishPkt(t, a, packets.Packet{TopicName: "monstermq/hmi/sync/cmd", Payload: []byte("1")})
	eventually(t, 5*time.Second, "standalone forwards the winccoa topic", func() bool {
		return len(b.recv.byTopic("winccoa/sys/a")) == 1
	})
	native = true
	publishPkt(t, a, packets.Packet{TopicName: "winccoa/sys/b", Payload: []byte("1")})
	publishPkt(t, a, packets.Packet{TopicName: "winccoax/sys/b", Payload: []byte("1")})
	eventually(t, 5*time.Second, "sibling topic forwarded", func() bool { return len(b.recv.byTopic("winccoax/sys/b")) == 1 })
	if len(b.recv.byTopic("winccoa/sys/b")) != 0 || len(b.recv.byTopic("$SYS/x")) != 0 {
		t.Fatal("namespace or $ topic forwarded")
	}
	// $SYS, the default HMI sync exclusion and the namespace topic while native mode is active.
	if got := a.m.Status().Log.Filtered; got != 3 {
		t.Fatalf("filtered %d, want 3", got)
	}
	_ = b
}

func TestDefaultExcludeHMISync(t *testing.T) {
	hmi := func(_ *config.PeerLinkConfig, d *Deps) { d.HMISyncBaseTopic = "monstermq/hmi/sync" }
	a, b := pair(t, []nodeOpt{hmi}, nil)
	publishPkt(t, a, packets.Packet{TopicName: "monstermq/hmi/sync/cmd", Payload: []byte("1")})
	publishPkt(t, a, packets.Packet{TopicName: "after", Payload: []byte("1")})
	eventually(t, 5*time.Second, "after", func() bool { return len(b.recv.byTopic("after")) == 1 })
	if len(b.recv.byTopic("monstermq/hmi/sync/cmd")) != 0 {
		t.Fatal("HMI sync channel forwarded by default")
	}
}

func TestOverflowGapAccounting(t *testing.T) {
	addrA := freeAddr(t)
	small := func(c *config.PeerLinkConfig, _ *Deps) {
		c.Log.MaxMessages = intp(1000)
		c.Fetch.MaxRecords = intp(500)
	}
	a := startNode(t, "node-a", addrA, []config.PeerConfig{{NodeID: "node-b"}}, small)
	for i := 0; i < 2500; i++ {
		publishPkt(t, a, packets.Packet{TopicName: "ovf", Payload: []byte{byte(i), byte(i >> 8)}})
	}
	b := startNode(t, "node-b", "", []config.PeerConfig{{NodeID: "node-a", Address: addrA, Serve: boolp(false)}}, small)
	eventually(t, 10*time.Second, "newest 1000 on B", func() bool { return b.recv.count("ovf") == 1000 })
	pk := b.recv.byTopic("ovf")[0]
	if v := int(pk.Payload[0]) | int(pk.Payload[1])<<8; v != 1500 {
		t.Fatalf("first record %d, want 1500", v)
	}
	if got := sourceStatus(b, "node-a").GapLostTotal; got != 1500 {
		t.Fatalf("gapLostTotal %d", got)
	}
	eventually(t, 5*time.Second, "lostTotal on A", func() bool { return consumerStatus(a, "node-b").LostTotal == 1500 })
}

func TestConsumerRestartResumesFromSourceCommit(t *testing.T) {
	addrA := freeAddr(t)
	a := startNode(t, "node-a", addrA, []config.PeerConfig{{NodeID: "node-b"}})
	peers := []config.PeerConfig{{NodeID: "node-a", Address: addrA, Serve: boolp(false)}}
	b1 := startNode(t, "node-b", "", peers)
	waitStreaming(t, b1, "node-a")
	for i := 0; i < 50; i++ {
		publishPkt(t, a, packets.Packet{TopicName: "r/1", Payload: []byte{byte(i)}})
	}
	eventually(t, 5*time.Second, "first 50", func() bool { return b1.recv.count("r/1") == 50 })
	b1.m.StopPullers(ctxTimeout(t, 5*time.Second))
	_ = b1.m.Close()
	eventually(t, 5*time.Second, "disconnected", func() bool { return consumerStatus(a, "node-b").State == "DISCONNECTED" })
	for i := 0; i < 30; i++ {
		publishPkt(t, a, packets.Packet{TopicName: "r/2", Payload: []byte{byte(i)}})
	}
	b2 := startNode(t, "node-b", "", peers)
	eventually(t, 5*time.Second, "next 30", func() bool { return b2.recv.count("r/2") == 30 })
	time.Sleep(200 * time.Millisecond)
	if b2.recv.count("r/1") != 0 {
		t.Fatalf("restarted consumer received %d already committed records", b2.recv.count("r/1"))
	}
	if consumerStatus(a, "node-b").Sessions != 2 {
		t.Fatalf("sessions %d", consumerStatus(a, "node-b").Sessions)
	}
}

func TestSnapshotFillOnFirstContact(t *testing.T) {
	addrA := freeAddr(t)
	a := startNode(t, "node-a", addrA, []config.PeerConfig{{NodeID: "node-b"}})
	// Retained values that exist before the consumer connects, e.g. loaded at startup.
	a.srv.Topics.RetainMessage(packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Publish, Retain: true},
		TopicName: "snap/one", Payload: []byte("a1"), Created: time.Now().Unix() - 5, Origin: "dev"})
	a.srv.Topics.RetainMessage(packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Publish, Retain: true},
		TopicName: "snap/two", Payload: []byte("a2"), Created: time.Now().Unix() - 5})
	a.srv.Topics.RetainMessage(packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Publish, Retain: true},
		TopicName: "$SYS/snap", Payload: []byte("no")})

	b := newNode(t, "node-b", "", []config.PeerConfig{{NodeID: "node-a", Address: addrA, Serve: boolp(false)}})
	b.srv.Topics.RetainMessage(packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Publish, Retain: true},
		TopicName: "snap/two", Payload: []byte("b-local"), Created: time.Now().Unix()})
	if err := b.m.Start(); err != nil {
		t.Fatal(err)
	}
	waitStreaming(t, b, "node-a")
	pk, ok := b.srv.Topics.Retained.Get("snap/one")
	if !ok || string(pk.Payload) != "a1" || pk.Origin != "dev" {
		t.Fatalf("snapshot value missing: %v %+v", ok, pk)
	}
	if pk, _ := b.srv.Topics.Retained.Get("snap/two"); string(pk.Payload) != "b-local" {
		t.Fatalf("present value overwritten: %q", pk.Payload)
	}
	if _, ok := b.srv.Topics.Retained.Get("$SYS/snap"); ok {
		t.Fatal("$ topic in snapshot")
	}
	ss := sourceStatus(b, "node-a")
	if ss.SnapshotFilled != 1 || ss.SnapshotSkippedPresent != 1 || ss.Snapshots != 1 {
		t.Fatalf("snapshot counters %+v", ss)
	}
	// Snapshot values are replicas: never captured on B.
	if b.m.Status().Log.Appended != (KindCounts{}) {
		t.Fatal("snapshot captured on B")
	}
}

func TestResyncNewer(t *testing.T) {
	a, b := pair(t, nil, nil)
	now := time.Now().Unix()
	b.srv.Topics.RetainMessage(packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Publish, Retain: true},
		TopicName: "rs/old", Payload: []byte("b-old"), Created: now - 100})
	b.srv.Topics.RetainMessage(packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Publish, Retain: true},
		TopicName: "rs/new", Payload: []byte("b-new"), Created: now})
	a.srv.Topics.RetainMessage(packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Publish, Retain: true},
		TopicName: "rs/old", Payload: []byte("a"), Created: now - 10})
	a.srv.Topics.RetainMessage(packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Publish, Retain: true},
		TopicName: "rs/new", Payload: []byte("a"), Created: now - 50})
	if err := b.m.Resync("node-a"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "resync", func() bool {
		pk, _ := b.srv.Topics.Retained.Get("rs/old")
		return string(pk.Payload) == "a"
	})
	if pk, _ := b.srv.Topics.Retained.Get("rs/new"); string(pk.Payload) != "b-new" {
		t.Fatalf("newer local value overwritten: %q", pk.Payload)
	}
	waitStreaming(t, b, "node-a")
	if sourceStatus(b, "node-a").SnapshotNewer != 1 {
		t.Fatalf("snapshotNewer %d", sourceStatus(b, "node-a").SnapshotNewer)
	}
	if err := b.m.Resync("nope"); err == nil {
		t.Fatal("resync of unknown source accepted")
	}
}

func TestGracefulDrain(t *testing.T) {
	a, b := pair(t, nil, nil)
	for i := 0; i < 200; i++ {
		publishPkt(t, a, packets.Packet{TopicName: "d/x", Payload: []byte{byte(i)}})
	}
	a.m.BeginDrain()
	res := a.m.Drain(ctxTimeout(t, 5*time.Second))
	if res.ShutdownUnserved["node-b"] != 0 || !res.Complete {
		t.Fatalf("drain %+v", res)
	}
	if b.recv.count("d/x") != 200 {
		t.Fatalf("B got %d before drain completed", b.recv.count("d/x"))
	}
	publishPkt(t, a, packets.Packet{TopicName: "d/late", Payload: []byte("x")})
	if got := a.m.Status().Log.UncapturedAtShutdown; got != 1 {
		t.Fatalf("uncapturedAtShutdown %d", got)
	}
	eventually(t, 5*time.Second, "B sees the shutdown", func() bool { return sourceStatus(b, "node-a").State != "STREAMING" })
}

func TestStopPullersCommitsAndSendsGoAway(t *testing.T) {
	a, b := pair(t, nil, nil)
	for i := 0; i < 10; i++ {
		publishPkt(t, a, packets.Packet{TopicName: "s/x", Payload: []byte{byte(i)}})
	}
	eventually(t, 5*time.Second, "delivered", func() bool { return b.recv.count("s/x") == 10 })
	b.m.StopPullers(ctxTimeout(t, 5*time.Second))
	eventually(t, 5*time.Second, "A sees the consumer leave", func() bool {
		c := consumerStatus(a, "node-b")
		return c.State == "DISCONNECTED" && c.Committed == 11
	})
	if sourceStatus(b, "node-a").State != "STOPPED" {
		t.Fatalf("state %s", sourceStatus(b, "node-a").State)
	}
}

func TestWillCaptureAndShutdownWills(t *testing.T) {
	a, b := pair(t, nil, nil)
	cl := a.srv.NewClient(nil, "tcp", "dev-will", false)
	will := packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Publish}, TopicName: "dev/state", Payload: []byte("offline"), Will: true}
	a.m.Hook().OnWillSent(cl, will)
	eventually(t, 5*time.Second, "will forwarded", func() bool { return b.recv.count("dev/state") == 1 })
	if pk := b.recv.byTopic("dev/state")[0]; pk.Forward == nil || !pk.Forward.Will || pk.Forward.ClientID != "dev-will" {
		t.Fatalf("will metadata %+v", pk.Forward)
	}
	a.m.BeginDrain()
	a.m.Hook().OnWillSent(cl, will)
	if st := a.m.Status().Log; st.SkipWill != 1 || st.Appended.Will != 1 {
		t.Fatalf("shutdown will captured: %+v", st)
	}
}

func TestWillSupersededOnReceiver(t *testing.T) {
	a, b := pair(t, nil, nil)
	// dev-x failed over to B and is connected there when A fires its will.
	cl := a.srv.NewClient(nil, "tcp", "dev-x", false)
	will := packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Publish}, TopicName: "dev/x/state", Payload: []byte("offline"), Will: true}
	bcl := b.srv.NewClient(nil, "tcp", "dev-x", false)
	b.srv.Clients.Add(bcl)
	defer b.srv.Clients.Delete("dev-x")
	b.m.Hook().OnSessionEstablished(bcl, packets.Packet{})
	a.m.Hook().OnWillSent(cl, will)
	publishPkt(t, a, packets.Packet{TopicName: "marker", Payload: []byte("1")})
	eventually(t, 5*time.Second, "marker", func() bool { return b.recv.count("marker") == 1 })
	if b.recv.count("dev/x/state") != 0 {
		t.Fatal("superseded will delivered")
	}
	if got := sourceStatus(b, "node-a").Dropped["will_superseded"]; got != 1 {
		t.Fatalf("will_superseded %d", got)
	}
}

func TestSharedSubscriptionSkip(t *testing.T) {
	_, b := pair(t, nil, nil)
	h := b.m.Hook()
	subs := &mqttSubscribersForTest{}
	out := h.OnSelectSubscribers(subs.make(), packets.Packet{Forward: &packets.Forward{}})
	if len(out.Shared) != 0 || len(out.SharedSelected) != 0 {
		t.Fatal("replica delivered to shared group")
	}
	out = h.OnSelectSubscribers(subs.make(), packets.Packet{})
	if len(out.Shared) == 0 {
		t.Fatal("local publish lost its shared group")
	}
	if b.m.Status().Log.SharedSkipped != 1 {
		t.Fatal("sharedSkipped not counted")
	}
}

func TestReservedClientIDsRefused(t *testing.T) {
	n := newNode(t, "node-a", "127.0.0.1:0", []config.PeerConfig{{NodeID: "node-b"}})
	for _, id := range []string{"inline", "peerlink:node-b"} {
		cl := n.srv.NewClient(nil, "tcp", id, false)
		if err := n.m.Hook().OnConnect(cl, packets.Packet{}); err != packets.ErrClientIdentifierNotValid {
			t.Fatalf("%s: %v", id, err)
		}
	}
	if err := n.m.Hook().OnConnect(n.srv.NewClient(nil, "tcp", "peer", false), packets.Packet{}); err != nil {
		t.Fatal(err)
	}
}

func TestEchoSuppression(t *testing.T) {
	echo := func(c *config.PeerLinkConfig, _ *Deps) { c.Capture.EchoSuppressMs = 2000 }
	addrA, addrB := freeAddr(t), freeAddr(t)
	a := startNode(t, "node-a", addrA, []config.PeerConfig{{NodeID: "node-b", Address: addrB}}, echo)
	b := startNode(t, "node-b", addrB, []config.PeerConfig{{NodeID: "node-a", Address: addrA}}, echo)
	waitStreaming(t, a, "node-b")
	waitStreaming(t, b, "node-a")
	clientPublish(t, a, "c1", packets.Packet{TopicName: "e/t", Payload: []byte("v")})
	eventually(t, 5*time.Second, "replica on B", func() bool { return b.recv.count("e/t") == 1 })
	// An external client on B republishes what it received.
	clientPublish(t, b, "relay", packets.Packet{TopicName: "e/t", Payload: []byte("v")})
	clientPublish(t, b, "relay", packets.Packet{TopicName: "e/t", Payload: []byte("other")})
	eventually(t, 5*time.Second, "other forwarded", func() bool { return a.recv.count("e/t") == 2 })
	if got := b.m.Status().Log.EchoSuppressed; got != 1 {
		t.Fatalf("echoSuppressed %d", got)
	}
}

func TestMarkReplicas(t *testing.T) {
	mark := func(c *config.PeerLinkConfig, _ *Deps) { c.Receive.MarkReplicas = true }
	a, b := pair(t, nil, []nodeOpt{mark})
	publishPkt(t, a, packets.Packet{TopicName: "m/x", Payload: []byte("1")})
	eventually(t, 5*time.Second, "delivered", func() bool { return b.recv.count("m/x") == 1 })
	u := b.recv.byTopic("m/x")[0].Properties.User
	if len(u) != 1 || u[0].Key != MarkReplicasKey || u[0].Val != "node-a" {
		t.Fatalf("user properties %+v", u)
	}
}

func TestReceiveFiltersAndSize(t *testing.T) {
	filters := func(c *config.PeerLinkConfig, d *Deps) {
		c.Peers[0].Receive = config.PeerReceive{Include: []string{"in/#"}, Exclude: []string{"in/secret/#"}}
		d.MaxMessageSize = 8
	}
	a, b := pair(t, nil, []nodeOpt{filters})
	publishPkt(t, a, packets.Packet{TopicName: "out/x", Payload: []byte("1")})
	publishPkt(t, a, packets.Packet{TopicName: "in/secret/x", Payload: []byte("1")})
	publishPkt(t, a, packets.Packet{TopicName: "in/big", Payload: []byte("123456789"), FixedHeader: packets.FixedHeader{Retain: true}})
	publishPkt(t, a, packets.Packet{TopicName: "in/ok", Payload: []byte("1")})
	eventually(t, 5*time.Second, "in/ok", func() bool { return b.recv.count("in/ok") == 1 })
	ss := sourceStatus(b, "node-a")
	if ss.Dropped["filtered"] != 2 || ss.Dropped["size"] != 1 || ss.RetainedDiverged["size"] != 1 || b.recv.count("in/big") != 0 {
		t.Fatalf("drops %+v diverged %+v", ss.Dropped, ss.RetainedDiverged)
	}
}

func TestStatusEndpoint(t *testing.T) {
	a, _ := pair(t, nil, nil)
	code, body := httpRaw(t, a.addr, "GET /peerlink/v1/status HTTP/1.1\r\nHost: 127.0.0.1:1890\r\n\r\n")
	if code != 200 || !bytes.Contains(body, []byte(`"nodeId":"node-a"`)) || !bytes.Contains(body, []byte(`"consumers"`)) {
		t.Fatalf("status %d %s", code, body)
	}
	code, _ = httpRaw(t, a.addr, "POST /peerlink/v1/resync?source=nope HTTP/1.1\r\nHost: 127.0.0.1:1890\r\nContent-Length: 0\r\n\r\n")
	if code != 400 {
		t.Fatalf("resync of unknown source: %d", code)
	}
	code, _ = httpRaw(t, a.addr, "GET /other HTTP/1.1\r\nHost: 127.0.0.1:1890\r\n\r\n")
	if code != 404 {
		t.Fatalf("unknown path: %d", code)
	}
}

func TestPipelinedFetchKeepsOrder(t *testing.T) {
	opts := func(c *config.PeerLinkConfig, _ *Deps) {
		c.Fetch.Pipeline = intp(2)
		c.Fetch.MaxRecords = intp(64)
	}
	a, b := pair(t, []nodeOpt{opts}, []nodeOpt{opts})
	const n = 5000
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < n; i++ {
			_ = a.srv.PublishPacket(packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Publish}, TopicName: "o/x",
				Payload: []byte{byte(i), byte(i >> 8)}})
		}
	}()
	eventually(t, 20*time.Second, "all records", func() bool { return b.recv.count("o/x") == n })
	<-done
	for i, pk := range b.recv.byTopic("o/x") {
		if v := int(pk.Payload[0]) | int(pk.Payload[1])<<8; v != i {
			t.Fatalf("record %d has value %d", i, v)
		}
	}
	ss := sourceStatus(b, "node-a")
	if ss.DupSkipped != 0 || ss.GapLostTotal != 0 || ss.Injected != n {
		t.Fatalf("status %+v", ss)
	}
	eventually(t, 5*time.Second, "committed", func() bool { return consumerStatus(a, "node-b").Committed == n+1 })
}
