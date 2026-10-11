package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/peerlink"
	"monstermq.io/edge/internal/stores"
	"monstermq.io/edge/internal/stores/sqlite"
)

// Interest routing integration tests (plan-peerlink-interest-routing 15.2) use ports 27400-27549.

// plInterest enables interest routing. Archive interest is off because the provisioned Default
// archive group holds "#", which would make every node interested in everything.
func plInterest(c *config.Config) {
	c.PeerLink.Interest.Enabled = boolPtr(true)
	c.PeerLink.Interest.FlushMs = intPtr(5)
	c.PeerLink.Receive.Archive = boolPtr(false)
}

// plOnly replaces the node's inline "#" recorder with one subscription per filter.
func plOnly(filters ...string) func(n *plNode) {
	return func(n *plNode) {
		if err := n.srv.MQTT().Unsubscribe("#", 9001); err != nil {
			n.t.Fatal(err)
		}
		for i, f := range filters {
			if err := n.srv.MQTT().Subscribe(f, 9100+i, n.rec.handle); err != nil {
				n.t.Fatal(err)
			}
		}
	}
}

// startInterestPL starts a node whose recorder only subscribes filters.
func startInterestPL(t *testing.T, id string, mqttPort, peerPort int, peers []config.PeerConfig, filters []string, opts ...plOpt) *plNode {
	t.Helper()
	n := newPL(t, id, mqttPort, peerPort, peers, opts...)
	n.beforeServe = plOnly(filters...)
	n.start()
	return n
}

func (n *plNode) interestCounts() peerlink.InterestCounts {
	n.t.Helper()
	if in := n.status().Interest; in != nil {
		return *in
	}
	return peerlink.InterestCounts{}
}

func (n *plNode) peerInterest(peer string) peerlink.InterestStatus {
	n.t.Helper()
	if in := n.consumer(peer).Interest; in != nil {
		return *in
	}
	return peerlink.InterestStatus{}
}

// waitFilters waits until source src holds want filters for consumer peer.
func waitFilters(t *testing.T, src *plNode, peer string, want int) {
	t.Helper()
	plEventually(t, 10*time.Second, fmt.Sprintf("%s holds %d filters for %s", src.id, want, peer), func() bool {
		in := src.peerInterest(peer)
		return in.Mode == "FILTERED" && in.Filters == want
	})
}

// IR-1 and IR-8: only the subscribed topic is appended and forwarded; an oversize filter is not
// announced, is counted, and does not widen the interest.
func TestPeerLinkInterestBasic(t *testing.T) {
	small := func(c *config.Config) { c.PeerLink.Interest.MaxFilterBytes = intPtr(16) }
	a := startPL(t, "ir1a", 27400, 27401, []config.PeerConfig{plPeer("ir1b", 0)}, plInterest, small)
	b := startInterestPL(t, "ir1b", 27402, 0, []config.PeerConfig{plPullOnly("ir1a", 27401)},
		[]string{"x/1", "long/topic/filter/over/limit/#"}, plInterest, small)
	b.waitStreaming("ir1a")
	waitFilters(t, a, "ir1b", 1)

	before := a.status().Log.Appended.Inline
	a.publish("x/1", "1", 1, false)
	a.publish("x/2", "2", 1, false)
	a.publish("long/topic/filter/over/limit/z", "3", 1, false)
	plWaitCount(t, b, "x/", 1, 5*time.Second, 300*time.Millisecond)
	if n := b.rec.count("long/"); n != 0 {
		t.Fatalf("topic under the rejected filter forwarded %d times", n)
	}
	if got := a.status().Log.Appended.Inline - before; got != 1 {
		t.Fatalf("A appended %d records, want 1", got)
	}
	if c := a.interestCounts(); c.InterestSkipped < 2 || c.InterestMatched < 1 {
		t.Fatalf("source counters %+v", c)
	}
	if l := b.interestCounts().Local; l == nil || l.Rejected < 1 || l.Filters != 1 {
		t.Fatalf("consumer tracker %+v", l)
	}
}

// IR-2: three nodes with overlapping interest; one record per publish carries both bits.
func TestPeerLinkInterestThreeNodes(t *testing.T) {
	a := startPL(t, "ir2a", 27410, 27411, []config.PeerConfig{plPeer("ir2b", 0), plPeer("ir2c", 0)}, plInterest)
	b := startInterestPL(t, "ir2b", 27412, 0, []config.PeerConfig{plPullOnly("ir2a", 27411)}, []string{"a/#"}, plInterest)
	c := startInterestPL(t, "ir2c", 27414, 0, []config.PeerConfig{plPullOnly("ir2a", 27411)}, []string{"a/b"}, plInterest)
	b.waitStreaming("ir2a")
	c.waitStreaming("ir2a")
	waitFilters(t, a, "ir2b", 1)
	waitFilters(t, a, "ir2c", 1)

	before := a.status().Log.Appended.Inline
	a.publish("a/b", "1", 1, false)
	a.publish("a/c", "2", 1, false)
	a.publish("z/z", "3", 1, false)
	plWaitCount(t, b, "a/", 2, 5*time.Second, 0)
	plWaitCount(t, c, "a/", 1, 5*time.Second, 300*time.Millisecond)
	if len(c.rec.prefix("a/b")) != 1 {
		t.Fatal("C did not get a/b")
	}
	if got := a.status().Log.Appended.Inline - before; got != 2 {
		t.Fatalf("A appended %d records, want 2", got)
	}
}

// IR-3: the refcount announces a filter once for many sessions and withdraws it with the last.
func TestPeerLinkInterestRefcount(t *testing.T) {
	a := startPL(t, "ir3a", 27420, 27421, []config.PeerConfig{plPeer("ir3b", 0)}, plInterest)
	b := startInterestPL(t, "ir3b", 27422, 0, []config.PeerConfig{plPullOnly("ir3a", 27421)}, nil, plInterest)
	b.waitStreaming("ir3a")
	deltas := func() uint64 {
		if in := b.source("ir3a").Interest; in != nil {
			return in.DeltasSent
		}
		return 0
	}
	base := deltas()

	clients := make([]*rawClient, 100)
	for i := range clients {
		clients[i], _ = dialRaw(t, b.mqttPort, rawConnect{ClientID: fmt.Sprintf("ir3-%d", i), Clean: true})
		clients[i].Subscribe(packets.Subscription{Filter: "s/#", Qos: 1})
	}
	waitFilters(t, a, "ir3b", 1)
	time.Sleep(100 * time.Millisecond)
	if got := deltas() - base; got != 1 {
		t.Fatalf("%d deltas for 100 subscriptions, want 1", got)
	}
	for _, cl := range clients[:99] {
		cl.Close()
	}
	time.Sleep(300 * time.Millisecond)
	if got := deltas() - base; got != 1 {
		t.Fatalf("%d deltas after 99 unsubscribes, want 1", got)
	}
	clients[99].Close()
	plEventually(t, 5*time.Second, "NONE delta", func() bool { return deltas()-base == 2 })
	waitFilters(t, a, "ir3b", 0)

	skipped := a.interestCounts().InterestSkipped
	a.publish("s/1", "x", 1, false)
	if got := a.interestCounts().InterestSkipped - skipped; got != 1 {
		t.Fatalf("publish without interest: skipped +%d, want +1", got)
	}
}

// IR-4: retained publishes reach the retained store of an uninterested peer.
func TestPeerLinkInterestRetained(t *testing.T) {
	a := startPL(t, "ir4a", 27430, 27431, []config.PeerConfig{plPeer("ir4b", 0)}, plInterest)
	b := startInterestPL(t, "ir4b", 27432, 0, []config.PeerConfig{plPullOnly("ir4a", 27431)}, []string{"other/#"}, plInterest)
	b.waitStreaming("ir4a")
	waitFilters(t, a, "ir4b", 1)

	a.publish("r/1", "kept", 1, true)
	plEventually(t, 5*time.Second, "retained r/1 on B", func() bool {
		cl, _ := dialRaw(t, b.mqttPort, rawConnect{ClientID: "ir4-probe", Clean: true})
		defer cl.Close()
		cl.Subscribe(packets.Subscription{Filter: "r/1", Qos: 1})
		pk, ok := cl.NextOn("r/1", 200*time.Millisecond)
		return ok && string(pk.Payload) == "kept"
	})
}

// IR-5: a consumer restart drops its volatile interest at once; persistent interest survives and
// the queued message reaches the persistent session.
func TestPeerLinkInterestPeerRestart(t *testing.T) {
	a := startPL(t, "ir5a", 27440, 27441, []config.PeerConfig{plPeer("ir5b", 0)}, plInterest)
	b := startInterestPL(t, "ir5b", 27442, 0, []config.PeerConfig{plPullOnly("ir5a", 27441)}, nil, plInterest)
	b.waitStreaming("ir5a")

	vol, _ := dialRaw(t, b.mqttPort, rawConnect{ClientID: "ir5-v", Clean: true})
	vol.Subscribe(packets.Subscription{Filter: "v/#", Qos: 1})
	per, _ := dialRaw(t, b.mqttPort, rawConnect{ClientID: "ir5-p", SessionExpiry: 3600})
	per.Subscribe(packets.Subscription{Filter: "p/#", Qos: 1})
	per.Close()
	plEventually(t, 5*time.Second, "A holds v/# and persistent p/#", func() bool {
		in := a.peerInterest("ir5b")
		return in.Filters == 2 && in.FiltersPersistent == 1
	})
	oldInstance := a.peerInterest("ir5b").InstanceID

	b.Close()
	plEventually(t, 5*time.Second, "B disconnected on A", func() bool {
		return a.peerInterest("ir5b").State == "DISCONNECTED"
	})
	a.publish("v/1", "v", 1, false)
	a.publish("p/1", "p", 1, false)

	b.start()
	b.waitStreaming("ir5a")
	plEventually(t, 10*time.Second, "A holds only p/# for the restarted B", func() bool {
		in := a.peerInterest("ir5b")
		return in.InstanceID != oldInstance && in.State == "LIVE" && in.Filters == 1 && in.FiltersPersistent == 1
	})
	if a.interestCounts().VolatileDropped < 1 {
		t.Fatalf("volatileDropped not counted: %+v", a.interestCounts())
	}
	time.Sleep(300 * time.Millisecond) // queue batch flush
	back, _ := dialRaw(t, b.mqttPort, rawConnect{ClientID: "ir5-p", SessionExpiry: 3600})
	defer back.Close()
	if pk, ok := back.NextOn("p/1", 5*time.Second); !ok || string(pk.Payload) != "p" {
		t.Fatal("persistent session did not get p/1 published while B was down")
	}
}

// IR-9: after a source restart, Unknown decides whether publishes before the consumer's snapshot
// are kept for it.
func TestPeerLinkInterestUnknown(t *testing.T) {
	for i, unknown := range []string{config.PeerLinkInterestAll, config.PeerLinkInterestNone} {
		t.Run(unknown, func(t *testing.T) {
			base := 27450 + i*4
			mode := func(c *config.Config) { c.PeerLink.Interest.Unknown = unknown }
			a := startPL(t, "ir9a", base, base+1, []config.PeerConfig{plPeer("ir9b", 0)}, plInterest, mode)
			b := startInterestPL(t, "ir9b", base+2, 0, []config.PeerConfig{plPullOnly("ir9a", base+1)}, []string{"u/#"},
				plInterest, func(c *config.Config) {
					c.PeerLink.Fetch.ReconnectMaxMs = intPtr(2000)
				})
			b.waitStreaming("ir9a")
			waitFilters(t, a, "ir9b", 1)

			a.beforeServe = func(n *plNode) {} // keep A's "#" recorder
			a.restart()
			if st := a.peerInterest("ir9b").State; st != "UNKNOWN" {
				t.Fatalf("A state for B after restart %q, want UNKNOWN", st)
			}
			a.publish("u/1", "early", 1, false)
			b.waitStreaming("ir9a")
			waitFilters(t, a, "ir9b", 1)
			a.publish("u/2", "late", 1, false)
			plWaitCount(t, b, "u/2", 1, 5*time.Second, 0)
			time.Sleep(300 * time.Millisecond)
			early := b.rec.count("u/1")
			if unknown == config.PeerLinkInterestAll && early != 1 {
				t.Fatalf("Unknown ALL: u/1 forwarded %d times, want 1", early)
			}
			if unknown == config.PeerLinkInterestNone {
				if early != 0 {
					t.Fatalf("Unknown NONE: u/1 forwarded %d times, want 0", early)
				}
				if a.interestCounts().InterestSkipped < 1 {
					t.Fatal("Unknown NONE: the skipped publish is not counted")
				}
			}
		})
	}
}

// IR-10 and IR-11: a consumer without interest routing and a peer with Interest OFF get the
// dense feed while another consumer of the same source is filtered.
func TestPeerLinkInterestMixed(t *testing.T) {
	off := plPeer("ir10d", 0)
	off.Interest = config.PeerLinkInterestOff
	a := startPL(t, "ir10a", 27460, 27461, []config.PeerConfig{plPeer("ir10b", 0), plPeer("ir10c", 0), off}, plInterest)
	b := startInterestPL(t, "ir10b", 27462, 0, []config.PeerConfig{plPullOnly("ir10a", 27461)}, []string{"m/1"}, plInterest)
	c := startInterestPL(t, "ir10c", 27464, 0, []config.PeerConfig{plPullOnly("ir10a", 27461)}, []string{"m/#"})
	d := startInterestPL(t, "ir10d", 27466, 0, []config.PeerConfig{plPullOnly("ir10a", 27461)}, []string{"m/#"}, plInterest)
	b.waitStreaming("ir10a")
	c.waitStreaming("ir10a")
	d.waitStreaming("ir10a")
	waitFilters(t, a, "ir10b", 1)
	for _, p := range []string{"ir10c", "ir10d"} {
		if in := a.peerInterest(p); in.Mode != "ALL" {
			t.Fatalf("A serves %s in mode %q, want ALL", p, in.Mode)
		}
	}
	if in := d.source("ir10a").Interest; in != nil && in.Active {
		t.Fatal("Interest OFF peer negotiated interest routing")
	}

	for i := 1; i <= 3; i++ {
		a.publish(fmt.Sprintf("m/%d", i), "x", 1, false)
	}
	plWaitCount(t, c, "m/", 3, 5*time.Second, 0)
	plWaitCount(t, d, "m/", 3, 5*time.Second, 0)
	plWaitCount(t, b, "m/", 1, 5*time.Second, 300*time.Millisecond)
	if s := b.source("ir10a"); s.Interest == nil || !s.Interest.Active {
		t.Fatalf("B source interest %+v", s.Interest)
	}
}

// IR-11, per direction: Interest OFF on the consumer only, and on both ends, clears the final
// agreement; the source serves that consumer everything (15, per-peer OFF). Source-only OFF is in
// TestPeerLinkInterestMixed.
func TestPeerLinkInterestOffDirections(t *testing.T) {
	for i, both := range []bool{false, true} {
		name := "consumer-off"
		if both {
			name = "both-off"
		}
		t.Run(name, func(t *testing.T) {
			base := 27550 + i*4
			src := plPeer(fmt.Sprintf("ir11b%d", i), 0)
			if both {
				src.Interest = config.PeerLinkInterestOff
			}
			aID := fmt.Sprintf("ir11a%d", i)
			a := startPL(t, aID, base, base+1, []config.PeerConfig{src}, plInterest)
			up := plPullOnly(aID, base+1)
			up.Interest = config.PeerLinkInterestOff
			b := startInterestPL(t, src.NodeID, base+2, 0, []config.PeerConfig{up}, []string{"m/1"}, plInterest)
			b.waitStreaming(aID)
			// With no interest peer left A keeps an unmasked log and reports no interest object.
			if in := a.peerInterest(src.NodeID); in.Mode != "ALL" && !(both && in.Mode == "") {
				t.Fatalf("A serves B in mode %q, want ALL", in.Mode)
			}
			if in := b.source(aID).Interest; in != nil && in.Active {
				t.Fatal("Interest OFF consumer negotiated interest routing")
			}
			inj := b.source(aID).Injected
			for j := 1; j <= 3; j++ {
				a.publish(fmt.Sprintf("m/%d", j), "x", 1, false)
			}
			plEventually(t, 5*time.Second, "B gets the dense feed", func() bool {
				return b.source(aID).Injected-inj >= 3
			})
			if c := a.interestCounts(); c.InterestSkipped != 0 {
				t.Fatalf("A skipped records for an OFF link: %+v", c)
			}
		})
	}
}

// IR-12: archive groups are interest without an MQTT subscriber; the Default group on "#"
// forwards everything.
func TestPeerLinkInterestArchive(t *testing.T) {
	archive := func(c *config.Config) { c.PeerLink.Receive.Archive = boolPtr(true) }
	a := startPL(t, "ir12a", 27470, 27471, []config.PeerConfig{plPeer("ir12b", 0)}, plInterest)
	b := startInterestPL(t, "ir12b", 27472, 0, []config.PeerConfig{plPullOnly("ir12a", 27471)}, nil, plInterest, archive)
	b.waitStreaming("ir12a")
	plEventually(t, 10*time.Second, "Default group announced as #", func() bool {
		in := a.peerInterest("ir12b")
		return in.Mode == "FILTERED" && in.Filters == 1
	})
	before := a.status().Log.Appended.Inline
	a.publish("any/1", "x", 1, false)
	plEventually(t, 5*time.Second, "any/1 appended", func() bool { return a.status().Log.Appended.Inline-before == 1 })

	deltas := a.interestCounts().DeltasReceived
	ctx := context.Background()
	store := b.srv.Storage().ArchiveConfig
	groups, err := store.GetAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if g.Name == "Default" {
			g.Enabled = false
			if err := store.Save(ctx, g); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.Save(ctx, stores.ArchiveGroupConfig{Name: "G1", Enabled: true, TopicFilters: []string{"g1/#"},
		LastValType: stores.MessageStoreMemory, ArchiveType: stores.ArchiveNone, PayloadFormat: stores.PayloadDefault}); err != nil {
		t.Fatal(err)
	}
	if err := b.srv.Archives().Reload(ctx); err != nil {
		t.Fatal(err)
	}
	plEventually(t, 5*time.Second, "A applied the g1/# delta", func() bool {
		return a.interestCounts().DeltasReceived > deltas
	})
	waitFilters(t, a, "ir12b", 1)
	skipped := a.interestCounts().InterestSkipped
	a.publish("g1/x", "1", 1, false)
	a.publish("g2/x", "2", 1, false)
	plEventually(t, 5*time.Second, "g1/x appended, g2/x skipped", func() bool {
		return a.status().Log.Appended.Inline-before == 2 && a.interestCounts().InterestSkipped-skipped == 1
	})
}

// IR-14 and IR-15: message bus subscriptions are announced with Receive.Bus; local-only bus
// subscriptions (outbound bridges without Receive.BridgeOutbound) never are.
func TestPeerLinkInterestBus(t *testing.T) {
	for i, bus := range []bool{true, false} {
		t.Run(fmt.Sprintf("bus=%v", bus), func(t *testing.T) {
			base := 27480 + i*4
			recv := func(c *config.Config) { c.PeerLink.Receive.Bus = boolPtr(bus) }
			a := startPL(t, "ir14a", base, base+1, []config.PeerConfig{plPeer("ir14b", 0)}, plInterest)
			b := startInterestPL(t, "ir14b", base+2, 0, []config.PeerConfig{plPullOnly("ir14a", base+1)}, nil, plInterest, recv)
			b.waitStreaming("ir14a")

			_, _ = b.srv.Bus().SubscribeLocal([]string{"bridge/#"}, 16)
			id, ch := b.srv.Bus().Subscribe([]string{"g/#"}, 16)
			defer b.srv.Bus().Unsubscribe(id)
			want := 0
			if bus {
				want = 1
			}
			plEventually(t, 5*time.Second, "B's tracker settled", func() bool {
				l := b.interestCounts().Local
				return l != nil && l.Filters == want
			})
			waitFilters(t, a, "ir14b", want)

			skipped := a.interestCounts().InterestSkipped
			a.publish("g/1", "x", 1, false)
			a.publish("bridge/1", "y", 1, false)
			if bus {
				select {
				case m := <-ch:
					if m.TopicName != "g/1" {
						t.Fatalf("bus got %s", m.TopicName)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("g/1 not forwarded to the bus subscriber")
				}
			}
			plEventually(t, 5*time.Second, "skips counted", func() bool {
				return a.interestCounts().InterestSkipped-skipped == uint64(2-want)
			})
		})
	}
}

// IR-16 and IR-7: when a persistent session on the consumer expires, a NONE delta follows and
// the source stops appending for it.
func TestPeerLinkInterestSessionExpiry(t *testing.T) {
	a := startPL(t, "ir16a", 27490, 27491, []config.PeerConfig{plPeer("ir16b", 0)}, plInterest)
	b := startInterestPL(t, "ir16b", 27492, 0, []config.PeerConfig{plPullOnly("ir16a", 27491)}, nil, plInterest)
	b.waitStreaming("ir16a")

	per, _ := dialRaw(t, b.mqttPort, rawConnect{ClientID: "ir16-p", SessionExpiry: 1})
	per.Subscribe(packets.Subscription{Filter: "e/#", Qos: 1})
	plEventually(t, 5*time.Second, "persistent e/# on A", func() bool {
		in := a.peerInterest("ir16b")
		return in.Filters == 1 && in.FiltersPersistent == 1
	})
	per.Close()
	waitFilters(t, a, "ir16b", 0)

	skipped := a.interestCounts().InterestSkipped
	a.publish("e/1", "x", 1, false)
	if got := a.interestCounts().InterestSkipped - skipped; got != 1 {
		t.Fatalf("publish after session expiry: skipped +%d, want +1", got)
	}
	if strings.Contains(fmt.Sprint(b.interestCounts().Local), "e/#") {
		t.Fatal("expired filter still tracked")
	}
}

// IR-6: a cut link keeps the consumer's interest (same InstanceId); everything published during the
// partition arrives after resume. Records evicted from a small log are counted as lost, as without
// interest routing.
func TestPeerLinkInterestPartition(t *testing.T) {
	small := func(c *config.Config) {
		c.PeerLink.Log.MaxMessages = intPtr(100)
		c.PeerLink.Fetch.MaxRecords = intPtr(50)
	}
	a := startPL(t, "ir6a", 27494, 27495, []config.PeerConfig{plPeer("ir6b", 0)}, plInterest, small)
	proxy := startProxy(t, 27496, "127.0.0.1:27495")
	b := startInterestPL(t, "ir6b", 27497, 0, []config.PeerConfig{plPullOnly("ir6a", 27496)}, nil, plInterest, small)
	b.waitStreaming("ir6a")

	vol, _ := dialRaw(t, b.mqttPort, rawConnect{ClientID: "ir6-v", Clean: true})
	defer vol.Close()
	vol.Subscribe(packets.Subscription{Filter: "v/#", Qos: 1})
	per, _ := dialRaw(t, b.mqttPort, rawConnect{ClientID: "ir6-p", SessionExpiry: 3600})
	per.Subscribe(packets.Subscription{Filter: "p/#", Qos: 1})
	per.Close()
	plEventually(t, 5*time.Second, "A holds v/# and p/#", func() bool {
		in := a.peerInterest("ir6b")
		return in.State == "LIVE" && in.Filters == 2 && in.FiltersPersistent == 1
	})
	instance := a.peerInterest("ir6b").InstanceID
	dropped := a.interestCounts().VolatileDropped

	cut := func() {
		proxy.blocked.Store(true)
		proxy.Kill()
		plEventually(t, 10*time.Second, "A sees B disconnected", func() bool {
			return a.peerInterest("ir6b").State == "DISCONNECTED"
		})
	}
	cut()
	a.publish("v/1", "v", 1, false)
	a.publish("p/1", "p", 1, false)
	proxy.blocked.Store(false)
	b.waitStreaming("ir6a")
	if pk, ok := vol.NextOn("v/1", 5*time.Second); !ok || string(pk.Payload) != "v" {
		t.Fatal("clean session did not get v/1 published during the partition")
	}
	plEventually(t, 5*time.Second, "B back to LIVE with the same instance", func() bool {
		in := a.peerInterest("ir6b")
		return in.State == "LIVE" && in.InstanceID == instance && in.Filters == 2
	})
	if got := a.interestCounts().VolatileDropped; got != dropped {
		t.Fatalf("volatile interest dropped across a partition (%d -> %d)", dropped, got)
	}
	time.Sleep(300 * time.Millisecond) // queue batch flush
	back, _ := dialRaw(t, b.mqttPort, rawConnect{ClientID: "ir6-p", SessionExpiry: 3600})
	if pk, ok := back.NextOn("p/1", 5*time.Second); !ok || string(pk.Payload) != "p" {
		t.Fatal("persistent session did not get p/1 published during the partition")
	}
	back.Close()

	cut()
	for i := 0; i < 300; i++ {
		a.publish("v/e", fmt.Sprint(i), 0, false)
	}
	proxy.blocked.Store(false)
	b.waitStreaming("ir6a")
	plEventually(t, 5*time.Second, "evicted records counted as lost", func() bool {
		return b.source("ir6a").GapLostTotal >= 200
	})
	if n := vol.Drain(500 * time.Millisecond); n > 100 {
		t.Fatalf("%d records after eviction, the log holds at most 100", n)
	}
}

// IR-7: a persistent session on a consumer that is down longer than its expiry stops new capture
// for it, and its uncovered non-retained backlog is abandoned. Retained records stay protected.
func TestPeerLinkInterestDownExpiry(t *testing.T) {
	for i, version := range []byte{5, 4} {
		t.Run(fmt.Sprintf("mqtt%d", version), func(t *testing.T) {
			base := 27500 + i*4
			a := startPL(t, fmt.Sprintf("ir7a%d", version), base, base+1, []config.PeerConfig{plPeer("ir7b", 0)}, plInterest)
			src := a.id
			b := newPL(t, "ir7b", base+2, 0, []config.PeerConfig{plPullOnly(src, base+1)}, plInterest)
			b.beforeServe = func(n *plNode) {
				plOnly()(n)
				n.srv.MQTT().Options.Capabilities.MaximumSessionExpiryInterval = 2
			}
			b.start()
			b.waitStreaming(src)

			conn := rawConnect{ClientID: "ir7-p", Version: version, SessionExpiry: 2}
			if version < 5 {
				conn.SessionExpiry = 0 // CleanSession false; the broker maximum bounds the session
			}
			per, _ := dialRaw(t, b.mqttPort, conn)
			per.Subscribe(packets.Subscription{Filter: "p/#", Qos: 1})
			per.Close()
			plEventually(t, 5*time.Second, "A holds persistent p/#", func() bool {
				in := a.peerInterest("ir7b")
				return in.Filters == 1 && in.FiltersPersistent == 1
			})

			b.Close()
			plEventually(t, 5*time.Second, "B down on A", func() bool { return a.peerInterest("ir7b").State == "DISCONNECTED" })
			a.publish("p/1", "lost", 1, false)
			a.publish("p/r", "kept", 1, true)
			plEventually(t, 15*time.Second, "persistent interest expired while B is down", func() bool {
				c := a.interestCounts()
				return c.PersistentExpired >= 1 && c.InterestBacklogDiscarded >= 1 && a.peerInterest("ir7b").Filters == 0
			})
			skipped := a.interestCounts().InterestSkipped
			a.publish("p/2", "new", 1, false)
			if got := a.interestCounts().InterestSkipped - skipped; got != 1 {
				t.Fatalf("publish after expiry: skipped +%d, want +1", got)
			}

			b.beforeServe = plOnly("p/#")
			b.start()
			b.waitStreaming(src)
			waitFilters(t, a, "ir7b", 1)
			a.publish("p/3", "after", 1, false)
			plWaitCount(t, b, "p/3", 1, 5*time.Second, 300*time.Millisecond)
			if n := b.rec.count("p/1") + b.rec.count("p/2"); n != 0 {
				t.Fatalf("abandoned or skipped records forwarded %d times", n)
			}
			probe, _ := dialRaw(t, b.mqttPort, rawConnect{ClientID: "ir7-probe", Clean: true})
			defer probe.Close()
			probe.Subscribe(packets.Subscription{Filter: "p/r", Qos: 1})
			if pk, ok := probe.NextOn("p/r", 5*time.Second); !ok || string(pk.Payload) != "kept" {
				t.Fatal("retained p/r was not kept for B")
			}
		})
	}
}

// standbyBridge is an outbound MQTT bridge device whose remote broker never answers.
func standbyBridge(name, mode, localTopic string, enabled bool) stores.DeviceConfig {
	raw, _ := json.Marshal(map[string]any{
		"brokerUrl": "tcp://127.0.0.1:1", "clientId": name, "cleanSession": true, "reconnectDelay": 60000,
		"redundancy": mode,
		"addresses":  []map[string]any{{"mode": "PUBLISH", "localTopic": localTopic, "remoteTopic": "r"}},
	})
	return stores.DeviceConfig{Name: name, Namespace: "bridge", NodeID: "*", Type: "MQTT_CLIENT",
		Enabled: enabled, Config: string(raw)}
}

// IR-13: the outbound filters of HOT_STANDBY and COLD_STANDBY bridges are announced with
// Receive.BridgeOutbound false, also for a COLD bridge that is not running; ALWAYS bridges are
// not. A configuration change updates the announcement.
func TestPeerLinkInterestStandbyBridges(t *testing.T) {
	a := startPL(t, "ir13a", 27506, 27507, []config.PeerConfig{plPeer("ir13b", 0)}, plInterest)
	b := newPL(t, "ir13b", 27508, 0, []config.PeerConfig{plPullOnly("ir13a", 27507)}, plInterest,
		func(c *config.Config) { c.Features.MqttClient = true })
	db, err := sqlite.Open(b.cfg.SQLite.Path)
	if err != nil {
		t.Fatal(err)
	}
	dcs := sqlite.NewDeviceConfigStore(db)
	if err := dcs.EnsureTable(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, d := range []stores.DeviceConfig{
		standbyBridge("hot", "HOT_STANDBY", "hot/+", true),
		standbyBridge("cold", "COLD_STANDBY", "cold/x", false),
		standbyBridge("always", "ALWAYS", "alw/#", true),
	} {
		if err := dcs.Save(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	b.beforeServe = plOnly()
	b.start()
	b.waitStreaming("ir13a")
	waitFilters(t, a, "ir13b", 2)

	before := a.interestCounts()
	a.publish("hot/1", "h", 1, false)
	a.publish("cold/x", "c", 1, false)
	a.publish("cold/x/y", "c", 1, false)
	a.publish("alw/1", "a", 1, false)
	after := a.interestCounts()
	if m, s := after.InterestMatched-before.InterestMatched, after.InterestSkipped-before.InterestSkipped; m != 3 || s != 1 {
		t.Fatalf("matched +%d skipped +%d, want +3 and +1", m, s)
	}

	ctx := context.Background()
	if err := b.srv.Storage().DeviceConfig.Save(ctx, standbyBridge("cold", "ALWAYS", "cold/x", false)); err != nil {
		t.Fatal(err)
	}
	if err := b.srv.Bridges().Reload(ctx); err != nil {
		t.Fatal(err)
	}
	waitFilters(t, a, "ir13b", 1)
	if err := b.srv.Storage().DeviceConfig.Save(ctx, standbyBridge("always", "COLD_STANDBY", "alw/#", true)); err != nil {
		t.Fatal(err)
	}
	if err := b.srv.Bridges().Reload(ctx); err != nil {
		t.Fatal(err)
	}
	waitFilters(t, a, "ir13b", 2)
	skipped := a.interestCounts().InterestSkipped
	a.publish("alw/2", "a", 1, false)
	a.publish("cold/x", "c", 1, false)
	if got := a.interestCounts().InterestSkipped - skipped; got != 1 {
		t.Fatalf("after the change: skipped +%d, want +1 (cold/x)", got)
	}
}
