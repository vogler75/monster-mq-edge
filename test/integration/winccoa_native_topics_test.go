package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"monstermq.io/edge/internal/broker"
	"monstermq.io/edge/internal/oahost"
	"monstermq.io/edge/internal/winccoanative"
)

// The topics branch keeps MQTT topics in MMQTopic datapoints: publishes are
// written to WinCC OA and subscribers get them through the datapoint
// connection, so brokers on other systems see them too.
func TestNativeTopics(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	env := startNative(t, 27190, filepath.Join(t.TempDir(), "n.db"), sim, client, nil, broker.Options{})
	defer env.srv.Close()

	topic := "winccoa/topics/plant/a"
	explicit := "winccoa/systems/System1/topics/plant/a"
	dp := "System1:" + winccoanative.TopicDP("plant/a")

	s, _ := dialRaw(t, env.port, rawConnect{ClientID: "ts", Version: 5, Clean: true})
	defer s.Close()
	// Subscribing before the datapoint exists is accepted; it waits.
	if got := s.Subscribe(sub(topic, 1)); got[0] != 1 {
		t.Fatalf("suback %x", got)
	}
	plain, _ := dialRaw(t, env.port, rawConnect{ClientID: "tplain", Version: 5, Clean: true})
	defer plain.Close()
	plain.Subscribe(sub("plant/a", 1))

	p, _ := dialRaw(t, env.port, rawConnect{ClientID: "tp", Version: 5, Clean: true})
	defer p.Close()

	// A non-retained publish creates the datapoint and reaches the
	// subscriber through OA.
	if code := p.Publish(rawPub{Topic: topic, Payload: []byte("hello"), QoS: 1}); code != 0 {
		t.Fatalf("puback 0x%02x", code)
	}
	pk, ok := s.NextOn(topic, 3*time.Second)
	if !ok || string(pk.Payload) != "hello" || pk.FixedHeader.Retain {
		t.Fatalf("live message: ok=%v retain=%v %q", ok, pk.FixedHeader.Retain, pk.Payload)
	}
	if v, err := sim.Get(dp + ".topic"); err != nil || v.Str != "plant/a" {
		t.Fatalf("topic element %+v %v", v, err)
	}
	if v, err := sim.Get(dp + ".value"); err != nil || string(v.Bytes) != "hello" {
		t.Fatalf("value element %+v %v", v, err)
	}
	api := oahost.API{C: client}
	vals, err := api.DpGet(context.Background(), []string{dp + ".value:_original.._last_value_storage_off"}, time.Second)
	if err != nil || !vals[0].Bool {
		t.Fatalf("last value storage of value not turned off: %+v %v", vals, err)
	}
	if v, _ := sim.Get(dp + ".retained"); len(v.Bytes) != 0 {
		t.Fatalf("non-retained publish set retained: %q", v.Bytes)
	}
	if _, ok := plain.NextOn("plant/a", 300*time.Millisecond); ok {
		t.Fatal("topics publish leaked to the plain topic")
	}

	// No retained message yet: a new subscriber gets nothing.
	n, _ := dialRaw(t, env.port, rawConnect{ClientID: "tn", Version: 5, Clean: true})
	defer n.Close()
	n.Subscribe(sub(explicit, 1))
	if pk, ok := n.NextOn(explicit, 400*time.Millisecond); ok {
		t.Fatalf("non-retained value replayed: %q", pk.Payload)
	}

	// A retained publish goes to the retained element and is delivered live
	// (retain flag unset) and as the initial value of new subscribers.
	if code := p.Publish(rawPub{Topic: topic, Payload: []byte("r1"), QoS: 1, Retain: true}); code != 0 {
		t.Fatalf("retained puback 0x%02x", code)
	}
	if pk, ok := s.NextOn(topic, 3*time.Second); !ok || string(pk.Payload) != "r1" || pk.FixedHeader.Retain {
		t.Fatalf("retained live: ok=%v retain=%v %q", ok, pk.FixedHeader.Retain, pk.Payload)
	}
	if pk, ok := n.NextOn(explicit, 3*time.Second); !ok || string(pk.Payload) != "r1" {
		t.Fatal("retained live on the explicit alias")
	}
	if v, _ := sim.Get(dp + ".retained"); string(v.Bytes) != "r1" {
		t.Fatalf("retained element %q", v.Bytes)
	}
	r, _ := dialRaw(t, env.port, rawConnect{ClientID: "tr", Version: 5, Clean: true})
	defer r.Close()
	r.Subscribe(sub(topic, 1))
	if pk, ok := r.NextOn(topic, 3*time.Second); !ok || string(pk.Payload) != "r1" || !pk.FixedHeader.Retain {
		t.Fatalf("initial retained: ok=%v retain=%v %q", ok, pk.FixedHeader.Retain, pk.Payload)
	}
	if n := sim.Connections(); n != 1 {
		t.Fatalf("OA connections = %d, want 1 shared", n)
	}

	// A write by another broker (any manager) reaches the subscribers.
	if err := sim.Set(dp+".value", oahost.Value{Kind: oahost.KindBytes, Bytes: []byte("remote")}); err != nil {
		t.Fatal(err)
	}
	if pk, ok := s.NextOn(topic, 3*time.Second); !ok || string(pk.Payload) != "remote" {
		t.Fatal("foreign write not delivered")
	}

	// A retained publish with an empty payload is delivered and deletes the
	// datapoint.
	if code := p.Publish(rawPub{Topic: topic, QoS: 1, Retain: true}); code != 0 {
		t.Fatalf("clear puback 0x%02x", code)
	}
	if pk, ok := s.NextOn(topic, 3*time.Second); !ok || len(pk.Payload) != 0 {
		t.Fatalf("empty retained not delivered: ok=%v %q", ok, pk.Payload)
	}
	time.Sleep(300 * time.Millisecond)
	r.Drain(100 * time.Millisecond)
	if _, err := sim.Get(dp + ".value"); err == nil {
		t.Fatal("datapoint not deleted")
	}
	if n := sim.Connections(); n != 0 {
		t.Fatalf("connection of deleted datapoint kept: %d", n)
	}
	// A new publish recreates it and the waiting subscribers connect again.
	if code := p.Publish(rawPub{Topic: topic, Payload: []byte("again"), QoS: 1}); code != 0 {
		t.Fatalf("recreate puback 0x%02x", code)
	}
	if pk, ok := s.NextOn(topic, 3*time.Second); !ok || string(pk.Payload) != "again" {
		t.Fatal("no delivery after recreation")
	}
	if pk, ok := r.NextOn(topic, time.Second); !ok || string(pk.Payload) != "again" {
		t.Fatal("no delivery to second subscriber after recreation")
	}

	// A datapoint created by another broker connects waiting subscribers
	// at once.
	other := "winccoa/topics/other"
	odp := "System1:" + winccoanative.TopicDP("other")
	s.Subscribe(sub(other, 1))
	time.Sleep(200 * time.Millisecond)
	if err := api.DpCreate(context.Background(), odp, winccoanative.TopicType, time.Second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := sim.Set(odp+".value", oahost.Value{Kind: oahost.KindBytes, Bytes: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if pk, ok := s.NextOn(other, 3*time.Second); !ok || string(pk.Payload) != "x" {
		t.Fatal("datapoint created elsewhere not connected")
	}

	// Shared subscriptions are rejected in the topics branch.
	if got := s.Subscribe(sub("$share/g/winccoa/topics/plant/a", 1)); got[0] != 0x9E {
		t.Fatalf("shared suback 0x%02x", got[0])
	}
	if code := p.Publish(rawPub{Topic: "winccoa/systems/System1/topics", Payload: []byte("x"), QoS: 1}); code != 0x90 {
		t.Fatalf("publish without topic: 0x%02x", code)
	}
	if st := env.srv.Native().Stats(); st.TopicDPs != 2 || st.TopicSubs != 4 {
		t.Fatalf("stats %+v", st)
	}
	s.Unsubscribe(topic, other)
	r.Unsubscribe(topic)
	n.Unsubscribe(explicit)
	time.Sleep(300 * time.Millisecond)
	if c := sim.Connections(); c != 0 {
		t.Fatalf("connections after unsubscribe: %d", c)
	}
}

func TestNativeTopicsRemote(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	env := startNative(t, 27191, filepath.Join(t.TempDir(), "n.db"), sim, client, nil, broker.Options{})
	defer env.srv.Close()

	topic := "winccoa/systems/SubstationA/topics/feeder/state"
	dp := "SubstationA:" + winccoanative.TopicDP("feeder/state")
	s, _ := dialRaw(t, env.port, rawConnect{ClientID: "rs", Version: 5, Clean: true})
	defer s.Close()
	if got := s.Subscribe(sub(topic, 1)); got[0] != 1 {
		t.Fatalf("suback %x", got)
	}
	p, _ := dialRaw(t, env.port, rawConnect{ClientID: "rp", Version: 5, Clean: true})
	defer p.Close()
	if code := p.Publish(rawPub{Topic: topic, Payload: []byte("on"), QoS: 1, Retain: true}); code != 0 {
		t.Fatalf("puback 0x%02x", code)
	}
	if pk, ok := s.NextOn(topic, 3*time.Second); !ok || string(pk.Payload) != "on" {
		t.Fatal("remote topic not delivered")
	}
	if v, err := sim.Get(dp + ".retained"); err != nil || string(v.Bytes) != "on" {
		t.Fatalf("remote datapoint %+v %v", v, err)
	}

	// Outage: no stale replay, writes fail, recovery delivers the retained
	// value again.
	sim.SetSystemAvailable("SubstationA", false)
	time.Sleep(200 * time.Millisecond)
	if code := p.Publish(rawPub{Topic: topic, Payload: []byte("off"), QoS: 1}); code != 0x83 {
		t.Fatalf("publish during outage: 0x%02x", code)
	}
	sim.SetSystemAvailable("SubstationA", true)
	if pk, ok := s.NextOn(topic, 3*time.Second); !ok || string(pk.Payload) != "on" || !pk.FixedHeader.Retain {
		t.Fatalf("retained after recovery: ok=%v %q", ok, pk.Payload)
	}
	if code := p.Publish(rawPub{Topic: topic, Payload: []byte("off"), QoS: 1}); code != 0 {
		t.Fatalf("publish after recovery: 0x%02x", code)
	}
	if pk, ok := s.NextOn(topic, 3*time.Second); !ok || string(pk.Payload) != "off" {
		t.Fatal("live after recovery")
	}

	// An unavailable system rejects subscribe and publish.
	down := "winccoa/systems/SubstationB/topics/x"
	if got := s.Subscribe(sub(down, 1)); got[0] != 0x83 {
		t.Fatalf("subscribe to unavailable system: 0x%02x", got[0])
	}
	if code := p.Publish(rawPub{Topic: down, Payload: []byte("x"), QoS: 1}); code != 0x83 {
		t.Fatalf("publish to unavailable system: 0x%02x", code)
	}
}

// Persisted topics subscriptions are restored after a restart, also for a
// datapoint that does not exist yet, and stop cleanly.
func TestNativeTopicsRestore(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	db := filepath.Join(t.TempDir(), "n.db")
	env := startNative(t, 27192, db, sim, client, nil, broker.Options{})
	c, _ := dialRaw(t, env.port, rawConnect{ClientID: "tpers", Version: 4, Clean: false})
	if got := c.Subscribe(sub("winccoa/topics/kept", 1), sub("winccoa/topics/later", 1), sub("winccoa/topics/w/#", 1)); string(got) != "\x01\x01\x01" {
		t.Fatalf("suback % x", got)
	}
	p, _ := dialRaw(t, env.port, rawConnect{ClientID: "tpub", Version: 5, Clean: true})
	if code := p.Publish(rawPub{Topic: "winccoa/topics/kept", Payload: []byte("1"), QoS: 1}); code != 0 {
		t.Fatalf("puback 0x%02x", code)
	}
	if _, ok := c.NextOn("winccoa/topics/kept", 3*time.Second); !ok {
		t.Fatal("not delivered before restart")
	}
	c.Close()
	p.Close()
	time.Sleep(300 * time.Millisecond)
	env.srv.Close()
	if n := sim.Connections(); n != 0 {
		t.Fatalf("stop left %d registrations", n)
	}

	env2 := startNative(t, 27192, db, sim, client, nil, broker.Options{})
	defer env2.srv.Close()
	c2, _ := dialRaw(t, env2.port, rawConnect{ClientID: "tpers", Version: 4, Clean: false})
	defer c2.Close()
	p2, _ := dialRaw(t, env2.port, rawConnect{ClientID: "tpub2", Version: 5, Clean: true})
	defer p2.Close()
	time.Sleep(300 * time.Millisecond)
	for _, topic := range []string{"winccoa/topics/kept", "winccoa/topics/later", "winccoa/topics/w/1"} {
		if code := p2.Publish(rawPub{Topic: topic, Payload: []byte("2"), QoS: 1}); code != 0 {
			t.Fatalf("puback 0x%02x", code)
		}
		if pk, ok := c2.NextOn(topic, 3*time.Second); !ok || string(pk.Payload) != "2" {
			t.Fatalf("restored subscription on %s not delivered", topic)
		}
	}
}

func TestNativeTopicsWildcard(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	env := startNative(t, 27193, filepath.Join(t.TempDir(), "n.db"), sim, client, nil, broker.Options{})
	defer env.srv.Close()
	api := oahost.API{C: client}

	p, _ := dialRaw(t, env.port, rawConnect{ClientID: "wp", Version: 5, Clean: true})
	defer p.Close()
	if code := p.Publish(rawPub{Topic: "winccoa/topics/plant/a", Payload: []byte("r0"), QoS: 1, Retain: true}); code != 0 {
		t.Fatalf("puback 0x%02x", code)
	}

	const prefix = "winccoa/systems/System1/topics/"
	w, _ := dialRaw(t, env.port, rawConnect{ClientID: "ww", Version: 5, Clean: true})
	defer w.Close()
	if got := w.Subscribe(sub(prefix+"plant/#", 1)); got[0] != 1 {
		t.Fatalf("suback 0x%02x", got[0])
	}
	// Existing topic: its retained message, under the subscribed form.
	if pk, ok := w.NextOn(prefix+"plant/a", 3*time.Second); !ok || string(pk.Payload) != "r0" || !pk.FixedHeader.Retain {
		t.Fatalf("initial retained: ok=%v %q", ok, pk.Payload)
	}
	// A topic created after the subscription, first message included.
	if code := p.Publish(rawPub{Topic: "winccoa/topics/plant/b", Payload: []byte("b1"), QoS: 1}); code != 0 {
		t.Fatalf("puback 0x%02x", code)
	}
	if pk, ok := w.NextOn(prefix+"plant/b", 3*time.Second); !ok || string(pk.Payload) != "b1" || pk.FixedHeader.Retain {
		t.Fatalf("new topic: ok=%v %q", ok, pk.Payload)
	}
	if code := p.Publish(rawPub{Topic: "winccoa/topics/plant/a", Payload: []byte("a1"), QoS: 1}); code != 0 {
		t.Fatalf("puback 0x%02x", code)
	}
	if pk, ok := w.NextOn(prefix+"plant/a", 3*time.Second); !ok || string(pk.Payload) != "a1" {
		t.Fatal("live on an existing topic")
	}
	// Single-level wildcard inside the topic.
	if code := p.Publish(rawPub{Topic: "winccoa/topics/1/2/3", Payload: []byte("s"), QoS: 1, Retain: true}); code != 0 {
		t.Fatalf("puback 0x%02x", code)
	}
	plus, _ := dialRaw(t, env.port, rawConnect{ClientID: "wplus", Version: 5, Clean: true})
	defer plus.Close()
	plus.Subscribe(sub(prefix+"1/+/3", 1))
	if pk, ok := plus.NextOn(prefix+"1/2/3", 3*time.Second); !ok || string(pk.Payload) != "s" {
		t.Fatal("'+' filter: retained of existing topic")
	}
	for _, topic := range []string{"1/x/3", "1/x/4", "1/x/3/4"} {
		if code := p.Publish(rawPub{Topic: "winccoa/topics/" + topic, Payload: []byte(topic), QoS: 1}); code != 0 {
			t.Fatalf("puback 0x%02x", code)
		}
	}
	if pk, ok := plus.NextOn(prefix+"1/x/3", 3*time.Second); !ok || string(pk.Payload) != "1/x/3" {
		t.Fatal("'+' filter: new matching topic")
	}
	if pk, ok := plus.Next(400 * time.Millisecond); ok {
		t.Fatalf("'+' filter: unexpected %s", pk.TopicName)
	}
	plus.Unsubscribe(prefix + "1/+/3")
	w.Drain(300 * time.Millisecond)

	// Not matching.
	if code := p.Publish(rawPub{Topic: "winccoa/topics/other/x", Payload: []byte("x"), QoS: 1}); code != 0 {
		t.Fatalf("puback 0x%02x", code)
	}
	if pk, ok := w.Next(400 * time.Millisecond); ok {
		t.Fatalf("unexpected %s %q", pk.TopicName, pk.Payload)
	}

	// A topic datapoint created by another broker.
	cdp := "System1:" + winccoanative.TopicDP("plant/c")
	if err := api.DpCreate(context.Background(), cdp, winccoanative.TopicType, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := sim.Set(cdp+".topic", oahost.Value{Kind: oahost.KindString, Str: "plant/c"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := sim.Set(cdp+".value", oahost.Value{Kind: oahost.KindBytes, Bytes: []byte("c1")}); err != nil {
		t.Fatal(err)
	}
	if pk, ok := w.NextOn(prefix+"plant/c", 3*time.Second); !ok || string(pk.Payload) != "c1" {
		t.Fatal("topic created elsewhere not delivered")
	}
	if n := sim.Queries(); n != 1 {
		t.Fatalf("directory queries %d, want 1", n)
	}

	// Another system, shortcut form.
	r, _ := dialRaw(t, env.port, rawConnect{ClientID: "wr", Version: 5, Clean: true})
	defer r.Close()
	if got := r.Subscribe(sub("winccoa/systems/SubstationA/topics/#", 1), sub("winccoa/topics/+/b", 1)); string(got) != "\x01\x01" {
		t.Fatalf("suback % x", got)
	}
	if pk, ok := r.NextOn("winccoa/topics/plant/b", 500*time.Millisecond); ok {
		t.Fatalf("non-retained replayed %q", pk.Payload)
	}
	if code := p.Publish(rawPub{Topic: "winccoa/systems/SubstationA/topics/f/1", Payload: []byte("f"), QoS: 1}); code != 0 {
		t.Fatalf("puback 0x%02x", code)
	}
	if pk, ok := r.NextOn("winccoa/systems/SubstationA/topics/f/1", 3*time.Second); !ok || string(pk.Payload) != "f" {
		t.Fatal("remote wildcard")
	}
	if code := p.Publish(rawPub{Topic: "winccoa/topics/plant/b", Payload: []byte("b2"), QoS: 1}); code != 0 {
		t.Fatalf("puback 0x%02x", code)
	}
	if pk, ok := r.NextOn("winccoa/topics/plant/b", 3*time.Second); !ok || string(pk.Payload) != "b2" {
		t.Fatal("shortcut wildcard")
	}
	if n := sim.Queries(); n != 2 {
		t.Fatalf("directory queries %d, want one per system", n)
	}

	// Outage of the remote system and recovery.
	sim.SetSystemAvailable("SubstationA", false)
	time.Sleep(200 * time.Millisecond)
	sim.SetSystemAvailable("SubstationA", true)
	if pk, ok := r.NextOn("winccoa/systems/SubstationA/topics/f/1", 500*time.Millisecond); ok {
		t.Fatalf("non-retained replayed after recovery %q", pk.Payload)
	}
	if code := p.Publish(rawPub{Topic: "winccoa/systems/SubstationA/topics/f/2", Payload: []byte("g"), QoS: 1}); code != 0 {
		t.Fatalf("puback 0x%02x", code)
	}
	if pk, ok := r.NextOn("winccoa/systems/SubstationA/topics/f/2", 3*time.Second); !ok || string(pk.Payload) != "g" {
		t.Fatal("remote wildcard after recovery")
	}

	w.Unsubscribe(prefix + "plant/#")
	r.Unsubscribe("winccoa/systems/SubstationA/topics/#", "winccoa/topics/+/b")
	time.Sleep(300 * time.Millisecond)
	if q, c := sim.Queries(), sim.Connections(); q != 0 || c != 0 {
		t.Fatalf("after unsubscribe: queries %d connections %d", q, c)
	}
	if st := env.srv.Native().Stats(); st.TopicDPs != 0 || st.TopicWilds != 0 || st.TopicDirs != 0 {
		t.Fatalf("stats %+v", st)
	}
}
