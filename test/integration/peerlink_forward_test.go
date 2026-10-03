package integration

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/peerlink"
)

func plPaho(t *testing.T, port int, id string) paho.Client {
	t.Helper()
	c := paho.NewClient(mqttOpts(port, id))
	if tok := c.Connect(); !tok.WaitTimeout(3*time.Second) || tok.Error() != nil {
		t.Fatalf("connect %s: %v", id, tok.Error())
	}
	t.Cleanup(func() { c.Disconnect(50) })
	return c
}

type plMsg struct {
	topic   string
	payload string
	qos     byte
	retain  bool
}

type plSink struct {
	mu   sync.Mutex
	msgs []plMsg
}

func (s *plSink) handler(_ paho.Client, m paho.Message) {
	s.mu.Lock()
	s.msgs = append(s.msgs, plMsg{m.Topic(), string(m.Payload()), m.Qos(), m.Retained()})
	s.mu.Unlock()
}

func (s *plSink) all() []plMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]plMsg(nil), s.msgs...)
}

func plSubscribe(t *testing.T, c paho.Client, filter string, qos byte, s *plSink) {
	t.Helper()
	if tok := c.Subscribe(filter, qos, s.handler); !tok.WaitTimeout(3*time.Second) || tok.Error() != nil {
		t.Fatalf("subscribe %s: %v", filter, tok.Error())
	}
}

func plPahoPublish(t *testing.T, c paho.Client, topic string, qos byte, retain bool, payload string) {
	t.Helper()
	if tok := c.Publish(topic, qos, retain, payload); !tok.WaitTimeout(3*time.Second) || tok.Error() != nil {
		t.Fatalf("publish %s: %v", topic, tok.Error())
	}
}

// plRetained reads the retained row of topic on n (nil when absent).
func plRetained(n *plNode, topic string) []byte {
	msg, err := n.srv.Storage().Retained.Get(context.Background(), topic)
	if err != nil || msg == nil {
		return nil
	}
	return msg.Payload
}

// plPair starts two nodes that pull from each other.
func plPair(t *testing.T, a, b string, aMQTT, aPeer, bMQTT, bPeer int, aOpts, bOpts []plOpt) (*plNode, *plNode) {
	t.Helper()
	na := startPL(t, a, aMQTT, aPeer, []config.PeerConfig{plPeer(b, bPeer)}, aOpts...)
	nb := startPL(t, b, bMQTT, bPeer, []config.PeerConfig{plPeer(a, aPeer)}, bOpts...)
	na.waitStreaming(b)
	nb.waitStreaming(a)
	return na, nb
}

// PL-01: QoS 0/1/2, retained set and delete across the link.
func TestPeerLinkForwardQoSRetain(t *testing.T) {
	a, b := plPair(t, "pl01a", "pl01b", 27300, 27301, 27302, 27303, nil, nil)

	pub := plPaho(t, a.mqttPort, "pl01-pub")
	sub := plPaho(t, b.mqttPort, "pl01-sub")
	var got plSink
	plSubscribe(t, sub, "pl01/q/#", 1, &got)

	for q := byte(0); q <= 2; q++ {
		plPahoPublish(t, pub, fmt.Sprintf("pl01/q/%d", q), q, false, fmt.Sprintf("qos-%d", q))
	}
	plEventually(t, 5*time.Second, "three messages on B", func() bool { return len(got.all()) == 3 })
	for _, m := range got.all() {
		var q byte
		fmt.Sscanf(m.topic, "pl01/q/%d", &q)
		if m.payload != fmt.Sprintf("qos-%d", q) {
			t.Fatalf("%s payload %q", m.topic, m.payload)
		}
		if want := min(q, 1); m.qos != want {
			t.Fatalf("%s delivered at QoS %d, want %d", m.topic, m.qos, want)
		}
	}

	plPahoPublish(t, pub, "pl01/ret", 1, true, "r1")
	plEventually(t, 5*time.Second, "retained value stored on B", func() bool { return string(plRetained(b, "pl01/ret")) == "r1" })
	late := plPaho(t, b.mqttPort, "pl01-late")
	var lateGot plSink
	plSubscribe(t, late, "pl01/ret", 1, &lateGot)
	plEventually(t, 3*time.Second, "retained delivery to a late subscriber on B", func() bool { return len(lateGot.all()) == 1 })
	if m := lateGot.all()[0]; m.payload != "r1" || !m.retain {
		t.Fatalf("late subscriber got %+v", m)
	}

	plPahoPublish(t, pub, "pl01/ret", 1, true, "")
	plEventually(t, 5*time.Second, "retained delete on B", func() bool { return plRetained(b, "pl01/ret") == nil })
	late2 := plPaho(t, b.mqttPort, "pl01-late2")
	var late2Got plSink
	plSubscribe(t, late2, "pl01/ret", 1, &late2Got)
	time.Sleep(300 * time.Millisecond)
	if n := len(late2Got.all()); n != 0 {
		t.Fatalf("deleted retained value still delivered: %+v", late2Got.all())
	}
}

// PL-02 and PL-03: MQTT 5 properties, remaining expiry, publisher identity
// and time on B's live subscribers, bus and retained store.
func TestPeerLinkMQTT5Fidelity(t *testing.T) {
	a, b := plPair(t, "pl02a", "pl02b", 27304, 27305, 27306, 27307, nil, nil)

	busID, bus := b.srv.Bus().Subscribe([]string{"pl02/#"}, 16)
	defer b.srv.Bus().Unsubscribe(busID)

	sub, _ := dialRaw(t, b.mqttPort, rawConnect{ClientID: "pl02-sub", Clean: true})
	defer sub.Close()
	sub.Subscribe(packets.Subscription{Filter: "pl02/#", Qos: 1})

	pub, _ := dialRaw(t, a.mqttPort, rawConnect{ClientID: "pl02-pub", Clean: true, Username: "alice", Password: "x"})
	defer pub.Close()
	users := []packets.UserProperty{{Key: "k1", Val: "v1"}, {Key: "k2", Val: "v2"}, {Key: "k1", Val: "v3"}}
	pk := packets.Packet{
		FixedHeader: packets.FixedHeader{Type: packets.Publish, Qos: 1, Retain: true},
		TopicName:   "pl02/full",
		Payload:     []byte(`{"v":1}`),
		Properties: packets.Properties{
			ContentType:           "application/json",
			ResponseTopic:         "pl02/resp",
			CorrelationData:       []byte{1, 2, 3},
			PayloadFormat:         1,
			PayloadFormatFlag:     true,
			MessageExpiryInterval: 100,
			User:                  users,
		},
	}
	pk.Mods.AllowResponseInfo = true
	sent := time.Now()
	if r, err := pub.send(pk, (*packets.Packet).PublishEncode); err != nil || r.ReasonCode != 0 {
		t.Fatalf("publish: %v reason 0x%02x", err, r.ReasonCode)
	}

	got, ok := sub.NextOn("pl02/full", 5*time.Second)
	if !ok {
		t.Fatal("no delivery on B")
	}
	p := got.Properties
	if string(got.Payload) != `{"v":1}` || p.ContentType != "application/json" || p.ResponseTopic != "pl02/resp" ||
		!bytes.Equal(p.CorrelationData, []byte{1, 2, 3}) || !p.PayloadFormatFlag || p.PayloadFormat != 1 {
		t.Fatalf("properties not forwarded: payload %q props %+v", got.Payload, p)
	}
	if len(p.User) != len(users) {
		t.Fatalf("user properties %+v, want %+v", p.User, users)
	}
	for i := range users {
		if p.User[i] != users[i] {
			t.Fatalf("user property %d = %+v, want %+v (order and duplicates kept)", i, p.User[i], users[i])
		}
	}
	if p.MessageExpiryInterval < 98 || p.MessageExpiryInterval > 100 {
		t.Fatalf("remaining expiry %d, want 98..100", p.MessageExpiryInterval)
	}

	select {
	case m := <-bus:
		if m.ClientID != "pl02-pub" || m.OriginNode != "pl02a" {
			t.Fatalf("bus message publisher %q origin %q", m.ClientID, m.OriginNode)
		}
		if d := m.Time.Sub(sent); d < -time.Second || d > 2*time.Second {
			t.Fatalf("bus message time %v is not the A capture time (sent %v)", m.Time, sent)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("replica not on B's bus")
	}

	var row *struct {
		client, user string
		created      int64
	}
	plEventually(t, 5*time.Second, "retained row on B", func() bool {
		msg, err := b.srv.Storage().Retained.Get(context.Background(), "pl02/full")
		if err != nil || msg == nil {
			return false
		}
		row = &struct {
			client, user string
			created      int64
		}{msg.ClientID, msg.Username, msg.Time.Unix()}
		return true
	})
	if row.client != "pl02-pub" {
		t.Fatalf("retained row publisher %q", row.client)
	}
	// The SQLite retained table has no username column; the username
	// travels as replica metadata.
	recs := b.rec.prefix("pl02/full")
	if len(recs) != 1 || recs[0].Forward == nil || recs[0].Forward.Username != "alice" || recs[0].Origin != "pl02-pub" {
		t.Fatalf("replica metadata on B: %+v", recs)
	}
	if d := row.created - sent.Unix(); d < -1 || d > 1 {
		t.Fatalf("retained row time %d, A capture second %d", row.created, sent.Unix())
	}
}

// PL-04: bidirectional link without echo; replicas are never captured.
func TestPeerLinkBidirectionalNoEcho(t *testing.T) {
	a, b := plPair(t, "pl04a", "pl04b", 27308, 27309, 0, 27310, nil, nil)

	pub := plPaho(t, a.mqttPort, "pl04-pub")
	for i := 0; i < 5; i++ {
		plPahoPublish(t, pub, "pl04/a/client", 1, false, fmt.Sprint(i))
	}
	for i := 0; i < 10; i++ {
		a.publish("pl04/a/inline", fmt.Sprint(i), 1, false)
		b.publish("pl04/b/inline", fmt.Sprint(i), 1, false)
	}
	for _, n := range []*plNode{a, b} {
		plWaitCount(t, n, "pl04/a/", 15, 5*time.Second, 0)
		plWaitCount(t, n, "pl04/b/", 10, 5*time.Second, 0)
	}
	time.Sleep(time.Second)
	for _, n := range []*plNode{a, b} {
		if c := n.rec.count("pl04/a/"); c != 15 {
			t.Fatalf("%s: %d messages from A after idle, want 15", n.id, c)
		}
		if c := n.rec.count("pl04/b/"); c != 10 {
			t.Fatalf("%s: %d messages from B after idle, want 10", n.id, c)
		}
	}

	sa, sb := a.status(), b.status()
	if sa.Log.Appended.Client != 5 || sa.Log.Appended.Inline != 10 {
		t.Fatalf("A appended %+v, want client 5 inline 10 (replicas must not be captured)", sa.Log.Appended)
	}
	if sb.Log.Appended.Client != 0 || sb.Log.Appended.Inline != 10 {
		t.Fatalf("B appended %+v, want client 0 inline 10", sb.Log.Appended)
	}
	if got, want := sa.Log.SkipPeer, a.source("pl04b").Injected; got != want || got != 10 {
		t.Fatalf("A skipPeer %d, injected %d, want both 10", got, want)
	}
	if got, want := sb.Log.SkipPeer, b.source("pl04a").Injected; got != want || got != 15 {
		t.Fatalf("B skipPeer %d, injected %d, want both 15", got, want)
	}
}

// PL-04 variant: an external client on B republishes every replica. Echo
// suppression keeps the copy from flowing back; MarkReplicas tags replicas.
func TestPeerLinkEchoSuppressAndMarkReplicas(t *testing.T) {
	bOpts := []plOpt{func(c *config.Config) {
		c.PeerLink.Capture.EchoSuppressMs = 2000
		c.PeerLink.Receive.MarkReplicas = true
	}}
	a, b := plPair(t, "pl04ea", "pl04eb", 0, 27311, 27312, 27313, nil, bOpts)

	echo, _ := dialRaw(t, b.mqttPort, rawConnect{ClientID: "pl04e-echo", Clean: true})
	defer echo.Close()
	echo.Subscribe(packets.Subscription{Filter: "pl04e/#", Qos: 1})

	a.publish("pl04e/x", "hello", 1, false)
	got, ok := echo.NextOn("pl04e/x", 5*time.Second)
	if !ok {
		t.Fatal("replica not delivered on B")
	}
	marked := false
	for _, u := range got.Properties.User {
		if u.Key == peerlink.MarkReplicasKey && u.Val == "pl04ea" {
			marked = true
		}
	}
	if !marked {
		t.Fatalf("replica without %s user property: %+v", peerlink.MarkReplicasKey, got.Properties.User)
	}
	if r := echo.Publish(rawPub{Topic: got.TopicName, Payload: got.Payload, QoS: 1}); r != 0 {
		t.Fatalf("republish reason 0x%02x", r)
	}
	plEventually(t, 3*time.Second, "echo suppressed on B", func() bool { return b.status().Log.EchoSuppressed >= 1 })
	time.Sleep(500 * time.Millisecond)
	if c := a.rec.count("pl04e/"); c != 1 {
		t.Fatalf("A received %d messages on pl04e/, want 1 (no echo back)", c)
	}
}

// PL-05: full mesh delivers once per node; a chain delivers one hop only.
func TestPeerLinkThreeNodeMeshAndChain(t *testing.T) {
	t.Run("mesh", func(t *testing.T) {
		ports := map[string]int{"pl05a": 27314, "pl05b": 27315, "pl05c": 27316}
		var nodes []*plNode
		for id, port := range ports {
			var peers []config.PeerConfig
			for other, op := range ports {
				peers = append(peers, plPeer(other, op))
			}
			_ = id
			nodes = append(nodes, newPL(t, id, 0, port, peers))
		}
		for _, n := range nodes {
			n.start()
		}
		for _, n := range nodes {
			for other := range ports {
				if other != n.id {
					n.waitStreaming(other)
				}
			}
		}
		for _, n := range nodes {
			for i := 0; i < 5; i++ {
				n.publish("pl05/"+n.id, fmt.Sprint(i), 1, false)
			}
		}
		for _, n := range nodes {
			for _, from := range nodes {
				plWaitCount(t, n, "pl05/"+from.id, 5, 5*time.Second, 0)
			}
		}
		time.Sleep(500 * time.Millisecond)
		for _, n := range nodes {
			if c := n.rec.count("pl05/"); c != 15 {
				t.Fatalf("%s got %d messages, want exactly 15", n.id, c)
			}
		}
		// PL-12: once both consumers committed, the log is empty.
		for _, n := range nodes {
			plEventually(t, 5*time.Second, n.id+" log trimmed", func() bool {
				l := n.status().Log
				return l.LSO == l.LEO && l.Records == 0
			})
		}
	})

	t.Run("chain", func(t *testing.T) {
		a := startPL(t, "pl05x", 0, 27317, []config.PeerConfig{plPeer("pl05y", 27318)})
		b := startPL(t, "pl05y", 0, 27318, []config.PeerConfig{plPeer("pl05x", 27317), plPeer("pl05z", 27319)})
		c := startPL(t, "pl05z", 0, 27319, []config.PeerConfig{plPeer("pl05y", 27318)})
		a.waitStreaming("pl05y")
		b.waitStreaming("pl05x", "pl05z")
		c.waitStreaming("pl05y")
		a.publish("pl05c/from-a", "1", 1, false)
		c.publish("pl05c/from-c", "1", 1, false)
		plWaitCount(t, b, "pl05c/from-a", 1, 5*time.Second, 0)
		plWaitCount(t, b, "pl05c/from-c", 1, 5*time.Second, 0)
		time.Sleep(700 * time.Millisecond)
		if n := c.rec.count("pl05c/from-a"); n != 0 {
			t.Fatalf("C received %d messages published on A through B (one hop only)", n)
		}
		if n := a.rec.count("pl05c/from-c"); n != 0 {
			t.Fatalf("A received %d messages published on C through B (one hop only)", n)
		}
	})
}
