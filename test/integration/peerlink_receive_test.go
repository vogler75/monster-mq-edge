package integration

import (
	"bytes"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/mqtt/packets"
)

// rawPublishExpiry publishes a QoS 1 MQTT 5 message with an expiry interval.
func rawPublishExpiry(t *testing.T, c *rawClient, topic, payload string, retain bool, expiry uint32) {
	t.Helper()
	pk := packets.Packet{
		FixedHeader: packets.FixedHeader{Type: packets.Publish, Qos: 1, Retain: retain},
		TopicName:   topic,
		Payload:     []byte(payload),
		Properties:  packets.Properties{MessageExpiryInterval: expiry},
	}
	if r, err := c.send(pk, (*packets.Packet).PublishEncode); err != nil || r.ReasonCode != 0 {
		t.Fatalf("publish %s: %v reason 0x%02x", topic, err, r.ReasonCode)
	}
}

// rawCount counts the PUBLISH packets a raw client receives within wait.
func rawCount(c *rawClient, wait time.Duration) int {
	return c.Drain(wait)
}

// PL-32: a shared group with members on both nodes processes each message
// once with SKIP and once per node with DELIVER.
func TestPeerLinkSharedSubscriptions(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want int
	}{{config.PeerLinkSharedSkip, 20}, {config.PeerLinkSharedDeliver, 40}} {
		t.Run(tc.mode, func(t *testing.T) {
			mode := func(c *config.Config) { c.PeerLink.Receive.SharedSubscriptions = tc.mode }
			a, b := plPair(t, "pl32a", "pl32b", 27331, 27332, 27333, 27334, []plOpt{mode}, []plOpt{mode})
			sa, _ := dialRaw(t, a.mqttPort, rawConnect{ClientID: "pl32-sa", Clean: true})
			defer sa.Close()
			sb, _ := dialRaw(t, b.mqttPort, rawConnect{ClientID: "pl32-sb", Clean: true})
			defer sb.Close()
			sa.Subscribe(packets.Subscription{Filter: "$share/g/pl32/#", Qos: 1})
			sb.Subscribe(packets.Subscription{Filter: "$share/g/pl32/#", Qos: 1})
			for i := 0; i < 20; i++ {
				a.publish("pl32/t", fmt.Sprint(i), 1, false)
			}
			plWaitCount(t, b, "pl32/t", 20, 5*time.Second, 0)
			total := rawCount(sa, 300*time.Millisecond) + rawCount(sb, 300*time.Millisecond)
			if total != tc.want {
				t.Fatalf("shared group processed %d messages, want %d", total, tc.want)
			}
		})
	}
}

// PL-14: offline queues skip replicas unless Receive.Queue; NoLocal holds
// for the original publisher's client id.
func TestPeerLinkOfflineQueueAndNoLocal(t *testing.T) {
	for _, queue := range []bool{false, true} {
		t.Run(fmt.Sprintf("queue=%v", queue), func(t *testing.T) {
			q := func(c *config.Config) { c.PeerLink.Receive.Queue = queue }
			a := startPL(t, "pl14a", 27335, 27336, []config.PeerConfig{plPeer("pl14b", 0)})
			b := startPL(t, "pl14b", 27337, 0, []config.PeerConfig{plPullOnly("pl14a", 27336)}, q)
			b.waitStreaming("pl14a")

			persistent, _ := dialRaw(t, b.mqttPort, rawConnect{ClientID: "pl14-q", SessionExpiry: 300})
			persistent.Subscribe(packets.Subscription{Filter: "pl14/q", Qos: 1})
			persistent.Close()
			time.Sleep(100 * time.Millisecond)

			same, _ := dialRaw(t, b.mqttPort, rawConnect{ClientID: "pl14-same", Clean: true})
			defer same.Close()
			same.Subscribe(packets.Subscription{Filter: "pl14/nl", Qos: 1, NoLocal: true})
			other, _ := dialRaw(t, b.mqttPort, rawConnect{ClientID: "pl14-other", Clean: true})
			defer other.Close()
			other.Subscribe(packets.Subscription{Filter: "pl14/nl", Qos: 1})

			pub, _ := dialRaw(t, a.mqttPort, rawConnect{ClientID: "pl14-same", Clean: true})
			defer pub.Close()
			if r := pub.Publish(rawPub{Topic: "pl14/nl", Payload: []byte("x"), QoS: 1}); r != 0 {
				t.Fatalf("publish reason 0x%02x", r)
			}
			for i := 0; i < 3; i++ {
				a.publish("pl14/q", fmt.Sprint(i), 1, false)
			}
			if _, ok := other.NextOn("pl14/nl", 5*time.Second); !ok {
				t.Fatal("other subscriber on B got nothing")
			}
			if _, ok := same.NextOn("pl14/nl", 500*time.Millisecond); ok {
				t.Fatal("NoLocal subscriber with the publisher's client id received its own message on B")
			}
			plWaitCount(t, b, "pl14/q", 3, 5*time.Second, 0)
			time.Sleep(300 * time.Millisecond) // queue batch flush

			back, _ := dialRaw(t, b.mqttPort, rawConnect{ClientID: "pl14-q", SessionExpiry: 300})
			defer back.Close()
			want := 0
			if queue {
				want = 3
			}
			if got := rawCount(back, 700*time.Millisecond); got != want {
				t.Fatalf("persistent session got %d queued replicas, want %d", got, want)
			}
		})
	}
}

// PL-06 and PL-07 on standalone brokers: internal publishes (inline client,
// GraphQL) are forwarded, the winccoa namespace is an ordinary topic tree
// without native mode, $ topics and the HMI sync tree are not captured by
// default, and Exclude: [] forwards the HMI sync tree.
func TestPeerLinkInlineNamespaceAndHMI(t *testing.T) {
	gql := func(c *config.Config) {
		c.GraphQL.Enabled = true
		c.GraphQL.Port = 27340
		c.GraphQL.TLSPort = 0
	}
	a := startPL(t, "pl07a", 0, 27338, []config.PeerConfig{plPeer("pl07b", 0)}, gql)
	b := startPL(t, "pl07b", 0, 0, []config.PeerConfig{plPullOnly("pl07a", 27338)})
	b.waitStreaming("pl07a")

	a.publish("winccoa/plain/topic", "w", 1, false)
	a.publish("monstermq/hmi/sync/state", "h", 1, false)
	a.publish("$pl07/internal", "d", 1, false)
	if err := a.srv.MQTT().Publish("pl07/bad\xff", []byte("u"), false, 1); err == nil {
		plEventually(t, 3*time.Second, "invalid inline topic counted", func() bool {
			return a.status().Log.CaptureDropped["invalid"] >= 1
		})
	}
	waitForHTTP(t, "http://127.0.0.1:27340/health")
	res := gqlQuery(t, "http://127.0.0.1:27340/graphql", `mutation { publish(input: { topic: "pl07/gql", payload: "g", qos: 1 }) { success } }`, nil)
	if pub, _ := res["publish"].(map[string]any); pub == nil || pub["success"] != true {
		t.Fatalf("graphql publish: %v", res)
	}
	a.publish("pl07/marker", "m", 1, false)
	plWaitCount(t, b, "pl07/marker", 1, 5*time.Second, 0)

	if n := b.rec.count("winccoa/plain/topic"); n != 1 {
		t.Fatalf("standalone winccoa topic forwarded %d times, want 1", n)
	}
	if n := b.rec.count("pl07/gql"); n != 1 {
		t.Fatalf("GraphQL publish forwarded %d times, want 1", n)
	}
	if pk := b.rec.prefix("pl07/gql")[0]; pk.Origin != "inline" || pk.Forward == nil {
		t.Fatalf("GraphQL replica origin %q forward %+v", pk.Origin, pk.Forward)
	}
	if n := b.rec.count("monstermq/hmi/sync/"); n != 0 {
		t.Fatalf("HMI sync forwarded %d times by default", n)
	}
	sa := a.status().Log
	if sa.Filtered < 2 {
		t.Fatalf("A filtered %d, want at least 2 ($ topic and HMI sync)", sa.Filtered)
	}
	if sa.Appended.Inline < 3 {
		t.Fatalf("A appended %+v, want at least 3 inline records", sa.Appended)
	}

	a.Close()
	a.cfg.PeerLink.Capture.Exclude = strsPtr([]string{})
	a.cfg.GraphQL.Enabled = false
	a.start()
	b.waitStreaming("pl07a")
	a.publish("monstermq/hmi/sync/state", "h2", 1, false)
	plEventually(t, 5*time.Second, "HMI sync forwarded with Exclude: []", func() bool {
		for _, pk := range b.rec.prefix("monstermq/hmi/sync/state") {
			if string(pk.Payload) == "h2" {
				return true
			}
		}
		return false
	})
}

// PL-13: expiry is applied on the receiver with the age of the record.
func TestPeerLinkExpiry(t *testing.T) {
	_, b := plOneWay(t, "pl13a", "pl13b", 27341, 27342, []plOpt{func(c *config.Config) {
		c.TCP.Enabled, c.TCP.Port = true, 27343
	}}, nil)
	waitPort(t, 27343)
	pub, _ := dialRaw(t, 27343, rawConnect{ClientID: "pl13-pub", Clean: true})
	defer pub.Close()
	rawPublishExpiry(t, pub, "pl13/short", "s", false, 2)
	rawPublishExpiry(t, pub, "pl13/long", "l", true, 10)
	time.Sleep(3 * time.Second)

	b.start()
	plEventually(t, 5*time.Second, "expired record dropped on B", func() bool {
		return b.source("pl13a").Dropped["expired"] == 1
	})
	if n := b.rec.count("pl13/short"); n != 0 {
		t.Fatalf("expired message delivered %d times", n)
	}
	plEventually(t, 5*time.Second, "retained copy on B", func() bool { return plRetained(b, "pl13/long") != nil })
	sub, _ := dialRaw(t, b.mqttPort, rawConnect{ClientID: "pl13-late", Clean: true})
	defer sub.Close()
	sub.Subscribe(packets.Subscription{Filter: "pl13/long", Qos: 1})
	got, ok := sub.NextOn("pl13/long", 3*time.Second)
	if !ok {
		t.Fatal("retained value with remaining expiry not delivered on B")
	}
	if e := got.Properties.MessageExpiryInterval; e < 5 || e > 8 {
		t.Fatalf("remaining expiry %d on B, want about 7", e)
	}
}

// PL-38: a stale retained record is applied silently, a stale live record
// is dropped.
func TestPeerLinkStaleRecords(t *testing.T) {
	stale := func(c *config.Config) {
		c.PeerLink.Receive.MaxRecordAgeMs = 500
		c.PeerLink.Snapshot.Mode = config.PeerLinkSnapshotOff
	}
	a, b := plOneWay(t, "pl38a", "pl38b", 27344, 27345, nil, []plOpt{stale})
	a.publish("pl38/r", "retained", 1, true)
	a.publish("pl38/n", "live", 1, false)
	time.Sleep(1200 * time.Millisecond)
	b.start()
	plEventually(t, 5*time.Second, "both records applied", func() bool {
		s := b.source("pl38a")
		return s.Dropped["stale"] == 1 && s.RetainOnly == 1
	})
	if n := b.rec.count("pl38/"); n != 0 {
		t.Fatalf("stale records delivered live %d times", n)
	}
	plEventually(t, 3*time.Second, "stale retained value stored", func() bool { return string(plRetained(b, "pl38/r")) == "retained" })
	sub, _ := dialRaw(t, b.mqttPort, rawConnect{ClientID: "pl38-late", Clean: true})
	defer sub.Close()
	sub.Subscribe(packets.Subscription{Filter: "pl38/#", Qos: 1})
	if got, ok := sub.NextOn("pl38/r", 3*time.Second); !ok || string(got.Payload) != "retained" {
		t.Fatalf("late subscriber did not get the stale retained value: %v %q", ok, got.Payload)
	}
}

// PL-26: a record above the consumer's size limit becomes a tombstone; the
// link keeps going.
func TestPeerLinkSizeTombstone(t *testing.T) {
	a, b := plOneWay(t, "pl26a", "pl26b", 27346, 0,
		[]plOpt{func(c *config.Config) { c.MaxMessageSize = 4 << 20 }},
		[]plOpt{func(c *config.Config) { c.MaxMessageSize = 1 << 20 }})
	b.start()
	b.waitStreaming("pl26a")
	if err := a.srv.MQTT().Publish("pl26/big", bytes.Repeat([]byte("x"), 2<<20), false, 1); err != nil {
		t.Fatal(err)
	}
	a.publish("pl26/after", "ok", 1, false)
	plWaitCount(t, b, "pl26/after", 1, 5*time.Second, 0)
	if n := b.rec.count("pl26/big"); n != 0 {
		t.Fatalf("oversize record delivered %d times", n)
	}
	if d := b.source("pl26a").Dropped["size_source"]; d != 1 {
		t.Fatalf("B dropped size_source %d, want 1", d)
	}
	var skipped uint64
	for _, v := range a.consumer("pl26b").ServedSkipped {
		skipped += v
	}
	if skipped != 1 {
		t.Fatalf("A servedSkipped %d, want 1", skipped)
	}
}

// PL-35: after an outage the consumer catches up paced, and a live
// subscriber on the consumer loses nothing.
func TestPeerLinkCatchUpPacing(t *testing.T) {
	a := startPL(t, "pl35a", 0, 27347, []config.PeerConfig{plPeer("pl35b", 0)})
	proxy := startProxy(t, 27348, "127.0.0.1:27347")
	b := startPL(t, "pl35b", 27349, 0, []config.PeerConfig{plPullOnly("pl35a", 27348)},
		func(c *config.Config) { c.PeerLink.Fetch.MaxRecords = intPtr(500) })
	b.waitStreaming("pl35a")

	sub := paho.NewClient(mqttOpts(b.mqttPort, "pl35-sub"))
	if tok := sub.Connect(); !tok.WaitTimeout(3*time.Second) || tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer sub.Disconnect(50)
	var got atomic.Int64
	if tok := sub.Subscribe("pl35/#", 0, func(paho.Client, paho.Message) { got.Add(1) }); !tok.WaitTimeout(3*time.Second) || tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	dropped0 := atomic.LoadInt64(&b.srv.MQTT().Info.MessagesDropped)

	// The source publishes 2000/s for 4 s; the link is down for the first 2 s.
	proxy.blocked.Store(true)
	proxy.Kill()
	const total = 8000
	start := time.Now()
	for i := 0; i < total; i++ {
		if i == total/2 {
			proxy.blocked.Store(false)
		}
		a.publish("pl35/n", fmt.Sprint(i), 0, false)
		if wait := time.Duration(i+1)*time.Second/2000 - time.Since(start); wait > 0 {
			time.Sleep(wait)
		}
	}
	plEventually(t, 15*time.Second, fmt.Sprintf("all %d messages at B's subscriber (have %d)", total, got.Load()), func() bool {
		return got.Load() == total
	})
	if d := atomic.LoadInt64(&b.srv.MQTT().Info.MessagesDropped) - dropped0; d != 0 {
		t.Fatalf("B dropped %d messages to its subscriber during catch-up", d)
	}
	if s := b.source("pl35a"); s.Paced == 0 {
		t.Fatalf("catch-up was not paced: %+v", s)
	}
}
