package integration

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"monstermq.io/edge/internal/broker"
	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/oahost"
)

// collectTopics gathers publishes for d and returns topic -> payloads.
func collectTopics(c *rawClient, d time.Duration) map[string][]string {
	out := map[string][]string{}
	deadline := time.Now().Add(d)
	for {
		pk, ok := c.Next(time.Until(deadline))
		if !ok {
			return out
		}
		out[pk.TopicName] = append(out[pk.TopicName], string(pk.Payload))
	}
}

func topicList(m map[string][]string) []string {
	var out []string
	for t := range m {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Wildcard filters in the native namespace are served by shared
// dpQueryConnectSingle registrations (spec 4.2).
func TestNativeWildcardSubscriptions(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	env := startNative(t, 27180, filepath.Join(t.TempDir(), "n.db"), sim, client, nil, broker.Options{})
	defer env.srv.Close()
	_ = sim.Set("System1:Pump1.speed", oahost.Value{Kind: oahost.KindFloat, Float: 11})
	_ = sim.Set("System1:Pump1.nested.a", oahost.Value{Kind: oahost.KindFloat, Float: 1.5})

	// Subtree of one datapoint: initial values of every value element.
	a, _ := dialRaw(t, env.port, rawConnect{ClientID: "wa", Version: 5, Clean: true})
	defer a.Close()
	if got := a.Subscribe(sub("winccoa/systems/System1/tags/Pump1/#", 1)); got[0] != 1 {
		t.Fatalf("suback 0x%02x", got[0])
	}
	init := collectTopics(a, time.Second)
	want := []string{"%73et", "blob", "count", "name", "nested/a", "running", "speed", "ts", "unsigned"}
	var got []string
	for _, tp := range topicList(init) {
		got = append(got, strings.TrimPrefix(tp, "winccoa/systems/System1/tags/Pump1/"))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Pump1/# initial topics %v", got)
	}
	if !strings.Contains(init["winccoa/systems/System1/tags/Pump1/speed"][0], `"value":11`) {
		t.Fatalf("initial speed %v", init["winccoa/systems/System1/tags/Pump1/speed"])
	}

	// Element subtree of every datapoint of a type.
	b, _ := dialRaw(t, env.port, rawConnect{ClientID: "wb", Version: 5, Clean: true})
	defer b.Close()
	b.Subscribe(sub("winccoa/systems/System1/types/AnalogDrive/+/nested/#", 1))
	if got := topicList(collectTopics(b, time.Second)); strings.Join(got, ",") != "winccoa/systems/System1/types/AnalogDrive/Pump1/nested/a,winccoa/systems/System1/types/AnalogDrive/Pump101/nested/a" {
		t.Fatalf("types/+/nested/# initial %v", got)
	}

	// One element of every datapoint.
	c, _ := dialRaw(t, env.port, rawConnect{ClientID: "wc", Version: 5, Clean: true})
	defer c.Close()
	c.Subscribe(sub("winccoa/systems/System1/tags/+/speed", 1))
	if got := topicList(collectTopics(c, time.Second)); strings.Join(got, ",") != "winccoa/systems/System1/tags/Pump1/speed,winccoa/systems/System1/tags/Pump101/speed" {
		t.Fatalf("+/speed initial %v", got)
	}

	// Whole system: scalar roots included, internal and store DPs never.
	d, _ := dialRaw(t, env.port, rawConnect{ClientID: "wd", Version: 5, Clean: true})
	defer d.Close()
	d.Subscribe(sub("winccoa/systems/System1/tags/#", 0))
	all := collectTopics(d, time.Second)
	if _, ok := all["winccoa/systems/System1/tags/ScalarTag"]; !ok {
		t.Fatalf("scalar root missing from tags/#: %v", topicList(all))
	}
	for tp := range all {
		if strings.Contains(tp, "/_Users") || strings.Contains(tp, "MMQConfigs_") {
			t.Fatalf("protected datapoint published: %s", tp)
		}
	}
	if len(all) < 18 {
		t.Fatalf("tags/# covered %d elements", len(all))
	}

	// An exact subscriber on a covered element and a second client with the
	// same wildcard: one registration per filter, one message per change.
	e, _ := dialRaw(t, env.port, rawConnect{ClientID: "we", Version: 5, Clean: true})
	defer e.Close()
	e.Subscribe(sub("winccoa/systems/System1/tags/Pump1/speed", 1))
	e.Drain(300 * time.Millisecond)
	f, _ := dialRaw(t, env.port, rawConnect{ClientID: "wf", Version: 5, Clean: true})
	defer f.Close()
	f.Subscribe(sub("winccoa/systems/System1/tags/Pump1/#", 1))
	if n := len(collectTopics(f, 500*time.Millisecond)); n != 9 {
		t.Fatalf("second subscriber got %d cached current values, want 9", n)
	}
	if n := sim.Queries(); n != 4 {
		t.Fatalf("queries %d, want 4 (Pump1/#, types/+/nested/#, +/speed, #)", n)
	}
	for _, cl := range []*rawClient{a, b, c, d} {
		cl.Drain(200 * time.Millisecond)
	}
	pubBefore := env.srv.Native().Stats().Published
	_ = sim.Set("System1:Pump1.speed", oahost.Value{Kind: oahost.KindFloat, Float: 12})
	time.Sleep(500 * time.Millisecond)
	for name, cl := range map[string]*rawClient{"a": a, "c": c, "d": d, "e": e, "f": f} {
		msgs := collectTopics(cl, 300*time.Millisecond)["winccoa/systems/System1/tags/Pump1/speed"]
		if len(msgs) != 1 || !strings.Contains(msgs[0], `"value":12`) {
			t.Errorf("client %s: %d messages for one change: %v", name, len(msgs), msgs)
		}
	}
	if delta := env.srv.Native().Stats().Published - pubBefore; delta != 1 {
		t.Errorf("one change published %d times", delta)
	}
	if m := collectTopics(b, 200*time.Millisecond); len(m) != 0 {
		t.Errorf("types/+/nested/# got an unrelated change: %v", m)
	}

	// A new matching element change reaches the type subscriber.
	_ = sim.Set("System1:Pump101.nested.a", oahost.Value{Kind: oahost.KindFloat, Float: 9})
	if pk, ok := b.NextOn("winccoa/systems/System1/types/AnalogDrive/Pump101/nested/a", 2*time.Second); !ok || !strings.Contains(string(pk.Payload), `"value":9`) {
		t.Fatalf("types wildcard live value: %s", pk.Payload)
	}

	// Last unsubscribe releases each query.
	a.Unsubscribe("winccoa/systems/System1/tags/Pump1/#")
	time.Sleep(200 * time.Millisecond)
	if n := sim.Queries(); n != 4 {
		t.Fatalf("query released while f still subscribed: %d", n)
	}
	f.Unsubscribe("winccoa/systems/System1/tags/Pump1/#")
	b.Unsubscribe("winccoa/systems/System1/types/AnalogDrive/+/nested/#")
	c.Unsubscribe("winccoa/systems/System1/tags/+/speed")
	d.Unsubscribe("winccoa/systems/System1/tags/#")
	time.Sleep(300 * time.Millisecond)
	if n := sim.Queries(); n != 0 {
		t.Fatalf("queries left: %d", n)
	}
	if st := env.srv.Native().Stats(); st.WildQueries != 0 || st.WildSubs != 0 {
		t.Fatalf("stats %+v", st)
	}
}

// AllowRootWildcardSubscription=false rejects '#' and native filters that
// cover every datapoint; scoped filters stay allowed.
func TestNativeWildcardRootPolicy(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	no := false
	env := startNative(t, 27181, filepath.Join(t.TempDir(), "n.db"), sim, client, func(c *config.Config) {
		c.AllowRootWildcardSubscription = &no
	}, broker.Options{})
	defer env.srv.Close()
	for _, v := range []byte{5, 4} {
		c, _ := dialRaw(t, env.port, rawConnect{ClientID: fmt.Sprintf("root%d", v), Version: v, Clean: true})
		got := c.Subscribe(
			sub("#", 0),
			sub("$share/g/#", 0),
			sub("winccoa/systems/System1/tags/#", 0),
			sub("winccoa/systems/System1/types/#", 0),
			sub("winccoa/systems/System1/tags/+/speed", 0),
			sub("winccoa/systems/System1/types/+/+/speed", 0),
			sub("winccoa/systems/System1/tags/Pump1/#", 0),
			sub("winccoa/systems/System1/types/AnalogDrive/#", 0),
			sub("winccoa/systems/System1/types/Nope/#", 0),
			sub("winccoa/systems/SubstationB/tags/Feeder1/#", 0),
			sub("winccoa/systems/System1/tags/_Users/#", 0),
			sub("plain/#", 0),
		)
		want := []byte{0x8F, 0x8F, 0x8F, 0x8F, 0x8F, 0x8F, 0x00, 0x00, 0x8F, 0x83, 0x8F, 0x00}
		if v == 4 {
			for i := range want {
				if want[i] > 2 {
					want[i] = 0x80
				}
			}
		}
		if string(got) != string(want) {
			t.Errorf("v%d SUBACK\n got % x\nwant % x", v, got, want)
		}
		c.Close()
	}
}

// Remote wildcard filters use REMOTE and follow the system's availability;
// persistent wildcard subscriptions are restored after a restart.
func TestNativeWildcardRemoteAndRestore(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	db := filepath.Join(t.TempDir(), "n.db")
	env := startNative(t, 27182, db, sim, client, nil, broker.Options{})
	_ = sim.Set("SubstationA:Feeder1.voltage", oahost.Value{Kind: oahost.KindFloat, Float: 230})
	c, _ := dialRaw(t, env.port, rawConnect{ClientID: "wr", Version: 5, Clean: true, SessionExpiry: 3600})
	c.Subscribe(sub("winccoa/systems/SubstationA/tags/Feeder1/#", 1), sub("winccoa/systems/System1/types/AnalogDrive/+/speed", 1))
	init := collectTopics(c, time.Second)
	if _, ok := init["winccoa/systems/SubstationA/tags/Feeder1/voltage"]; !ok {
		t.Fatalf("remote initial %v", topicList(init))
	}
	sim.SetSystemAvailable("SubstationA", false)
	time.Sleep(300 * time.Millisecond)
	sim.SetSystemAvailable("SubstationA", true)
	time.Sleep(500 * time.Millisecond)
	_ = sim.Set("SubstationA:Feeder1.voltage", oahost.Value{Kind: oahost.KindFloat, Float: 231})
	// The re-registration first delivers the current value again.
	var seen []string
	for _, p := range collectTopics(c, 2*time.Second)["winccoa/systems/SubstationA/tags/Feeder1/voltage"] {
		seen = append(seen, p)
	}
	if len(seen) == 0 || !strings.Contains(seen[len(seen)-1], "231") {
		t.Fatalf("remote wildcard not re-registered after reconnect: %v", seen)
	}
	if n := sim.Queries(); n != 2 {
		t.Fatalf("queries after reconnect %d, want 2", n)
	}
	c.Close()
	env.srv.Close()
	if n := sim.Queries(); n != 0 {
		t.Fatalf("stop left %d queries", n)
	}

	// Restart while SubstationA is offline: its restored query waits for
	// the system instead of failing a registration against it.
	sim.SetSystemAvailable("SubstationA", false)
	calls := sim.Calls(oahost.OpQueryConnect)
	env2 := startNative(t, 27182, db, sim, client, func(cfg *config.Config) { cfg.QueuedMessagesEnabled = true }, broker.Options{})
	defer env2.srv.Close()
	time.Sleep(500 * time.Millisecond)
	if n, c := sim.Queries(), sim.Calls(oahost.OpQueryConnect)-calls; n != 1 || c != 1 {
		t.Fatalf("restored queries %d (%d registrations), want only the local one", n, c)
	}
	sim.SetSystemAvailable("SubstationA", true)
	time.Sleep(500 * time.Millisecond)
	if n := sim.Queries(); n != 2 {
		t.Fatalf("restored queries %d after SubstationA returned, want 2", n)
	}
	r, ack := dialRaw(t, env2.port, rawConnect{ClientID: "wr", Version: 5, Clean: false, SessionExpiry: 3600})
	defer r.Close()
	if !ack.SessionPresent {
		t.Fatal("session not present")
	}
	_ = sim.Set("System1:Pump1.speed", oahost.Value{Kind: oahost.KindFloat, Float: 77})
	if pk, ok := r.NextOn("winccoa/systems/System1/types/AnalogDrive/Pump1/speed", 2*time.Second); !ok || !strings.Contains(string(pk.Payload), "77") {
		t.Fatal("restored wildcard subscription inactive")
	}
}

var _ = packets.Subscription{}
