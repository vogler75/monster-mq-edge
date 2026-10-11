package peerlink

import (
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/mqtt"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/peerlink/wire"
)

func testTracker(classes map[string]holdClass) *interestTracker {
	return newInterestTracker(256, false, time.Hour, func(id string) holdClass { return classes[id] }, quietLogger())
}

func (t *interestTracker) flushNow() {
	t.mu.Lock()
	t.flushLocked()
	t.mu.Unlock()
}

// deltaEntries flattens the delta frames, checking the generations increase by one.
func deltaEntries(t *testing.T, fs []wire.Frame, gen *uint32) map[string]wire.InterestEntry {
	t.Helper()
	out := make(map[string]wire.InterestEntry)
	for _, f := range fs {
		d, ok := f.(*wire.InterestDelta)
		if !ok {
			t.Fatalf("frame %T, want delta", f)
		}
		if d.Generation != *gen+1 {
			t.Fatalf("generation %d after %d", d.Generation, *gen)
		}
		*gen = d.Generation
		for _, e := range d.Entries {
			out[e.Filter] = e
		}
	}
	return out
}

func snapshotEntries(t *testing.T, fs []wire.Frame) (uint32, map[string]wire.InterestEntry) {
	t.Helper()
	out := make(map[string]wire.InterestEntry)
	var gen uint32
	for i, f := range fs {
		s, ok := f.(*wire.InterestSnapshot)
		if !ok {
			t.Fatalf("frame %T, want snapshot", f)
		}
		if first := s.Flags&wire.InterestFlagFirst != 0; first != (i == 0) {
			t.Fatalf("frame %d FIRST=%v", i, first)
		}
		if last := s.Flags&wire.InterestFlagLast != 0; last != (i == len(fs)-1) {
			t.Fatalf("frame %d LAST=%v", i, last)
		}
		if i > 0 && s.Generation != gen {
			t.Fatalf("snapshot generations differ: %d, %d", gen, s.Generation)
		}
		gen = s.Generation
		for _, e := range s.Entries {
			out[e.Filter] = e
		}
	}
	return gen, out
}

func TestTrackerEmptySnapshot(t *testing.T) {
	tr := testTracker(nil)
	_, snap := tr.attach()
	if len(snap) != 1 {
		t.Fatalf("frames %d", len(snap))
	}
	s := snap[0].(*wire.InterestSnapshot)
	if s.Flags != wire.InterestFlagFirst|wire.InterestFlagLast || len(s.Entries) != 0 || s.Generation != 0 {
		t.Fatalf("empty snapshot %+v", s)
	}
}

func TestTrackerRefcountAndClasses(t *testing.T) {
	tr := testTracker(map[string]holdClass{
		"v":  {},
		"p1": {per: true, exp: 100},
		"p2": {per: true, exp: 3600},
	})
	cur, _ := tr.attach()
	var gen uint32

	tr.SubscriptionAdded("v", "a/#", "", false, 0)
	tr.SubscriptionAdded("v", "a/#", "", false, 0) // resubscribe: no second holder
	tr.FiltersAdded([]string{"a/#"})
	tr.flushNow()
	got := deltaEntries(t, cur.take(), &gen)
	if len(got) != 1 || got["a/#"].Class != wire.InterestVol {
		t.Fatalf("first delta %+v", got)
	}

	tr.SubscriptionAdded("p1", "a/#", "", false, 0)
	tr.flushNow()
	got = deltaEntries(t, cur.take(), &gen)
	if e := got["a/#"]; e.Class != wire.InterestPer || e.ExpirySec != 100 {
		t.Fatalf("persistent %+v", e)
	}

	tr.SubscriptionAdded("p2", "a/#", "", false, 0)
	tr.flushNow()
	if e := deltaEntries(t, cur.take(), &gen)["a/#"]; e.ExpirySec != 3600 {
		t.Fatalf("max expiry %+v", e)
	}
	// The holder of the maximum leaves: the expiry falls back to the remaining holder.
	tr.SubscriptionRemoved("p2", "a/#", "", false, 0)
	tr.flushNow()
	if e := deltaEntries(t, cur.take(), &gen)["a/#"]; e.ExpirySec != 100 {
		t.Fatalf("expiry after leave %+v", e)
	}
	tr.SubscriptionRemoved("p1", "a/#", "", false, 0)
	tr.SubscriptionRemoved("v", "a/#", "", false, 0)
	tr.flushNow()
	if e := deltaEntries(t, cur.take(), &gen)["a/#"]; e.Class != wire.InterestVol {
		t.Fatalf("bus holder keeps it volatile: %+v", e)
	}
	tr.FiltersRemoved([]string{"a/#"})
	tr.flushNow()
	if e := deltaEntries(t, cur.take(), &gen)["a/#"]; e.Class != wire.InterestNone {
		t.Fatalf("withdrawn %+v", e)
	}
	if st := tr.status(); st.Filters != 0 || st.Generation != gen {
		t.Fatalf("status %+v gen %d", st, gen)
	}

	// Add and remove inside one flush window announces nothing.
	tr.FiltersAdded([]string{"x"})
	tr.FiltersRemoved([]string{"x"})
	tr.flushNow()
	if fs := cur.take(); len(fs) != 0 {
		t.Fatalf("transient filter announced: %v", fs)
	}
}

func TestInterestChanged(t *testing.T) {
	per := func(exp uint32) wire.InterestEntry {
		return wire.InterestEntry{Class: wire.InterestPer, ExpirySec: exp}
	}
	vol := wire.InterestEntry{Class: wire.InterestVol}
	none := wire.InterestEntry{Class: wire.InterestNone}
	never := wire.InterestExpiryNever
	cases := []struct {
		old, next wire.InterestEntry
		want      bool
	}{
		{none, vol, true},
		{vol, per(10), true},
		{vol, vol, false},
		{per(1000), per(1100), false},
		{per(1000), per(1101), true},
		{per(1000), per(900), false},
		{per(1000), per(899), true},
		{per(1000), per(never), true},
		{per(never), per(1000), true},
		{per(0), per(1), true},
		{per(never), per(never), false},
	}
	for i, c := range cases {
		if got := interestChanged(c.old, c.next); got != c.want {
			t.Errorf("case %d %+v -> %+v: %v", i, c.old, c.next, got)
		}
	}
}

func TestTrackerSessionHoldClass(t *testing.T) {
	cases := []struct {
		proto   byte
		clean   bool
		exp     uint32
		want    holdClass
		comment string
	}{
		{5, true, 0, holdClass{}, "v5 expiry 0"},
		{5, true, 60, holdClass{per: true, exp: 60}, "v5 expiry"},
		{5, false, 1 << 30, holdClass{per: true, exp: 7200}, "v5 capped"},
		{4, true, 0, holdClass{}, "v3 clean"},
		{4, false, 0, holdClass{per: true, exp: 7200}, "v3 persistent"},
	}
	for _, c := range cases {
		if got := sessionHoldClass(c.proto, c.clean, c.exp, 7200); got != c.want {
			t.Errorf("%s: %+v", c.comment, got)
		}
	}
}

func TestTrackerSkips(t *testing.T) {
	tr := testTracker(nil)
	cur, _ := tr.attach()
	tr.SubscriptionAdded("c", "$SYS/#", "", false, 0)
	tr.SubscriptionAdded("c", "s/#", "g1", false, 0)
	tr.SubscriptionAdded(InjectorPrefix+"x", "inj/#", "", false, 0)
	tr.SubscriptionAdded("c", "bad/#/x", "", false, 0)
	tr.SubscriptionAdded("c", strings.Repeat("l", 300), "", false, 0)
	tr.FiltersAdded([]string{"also/+bad"})
	tr.flushNow()
	if fs := cur.take(); len(fs) != 0 {
		t.Fatalf("skipped filters announced: %v", fs)
	}
	if st := tr.status(); st.Rejected != 3 {
		t.Fatalf("rejected %d, want 3", st.Rejected)
	}

	deliver := newInterestTracker(256, true, time.Hour, func(string) holdClass { return holdClass{} }, quietLogger())
	cur, _ = deliver.attach()
	deliver.SubscriptionAdded("c", "s/#", "g1", false, 0)
	deliver.SubscriptionAdded("d", "s/#", "g1", false, 0)
	deliver.flushNow()
	var gen uint32
	if got := deltaEntries(t, cur.take(), &gen); got["s/#"].Class != wire.InterestVol {
		t.Fatalf("shared DELIVER not announced: %+v", got)
	}
	deliver.SubscriptionRemoved("c", "s/#", "g1", false, 0)
	deliver.flushNow()
	if fs := cur.take(); len(fs) != 0 {
		t.Fatalf("second shared holder lost: %v", fs)
	}
}

func TestTrackerInlineAndReclassify(t *testing.T) {
	classes := map[string]holdClass{}
	tr := testTracker(classes)
	cur, _ := tr.attach()
	var gen uint32
	tr.SubscriptionAdded("", "i/#", "", true, 7)
	tr.SubscriptionAdded("", "i/#", "", true, 7)
	tr.SubscriptionAdded("c", "r/#", "", false, 0) // restored before the session is known: volatile
	tr.flushNow()
	got := deltaEntries(t, cur.take(), &gen)
	if got["i/#"].Class != wire.InterestVol || got["r/#"].Class != wire.InterestVol {
		t.Fatalf("delta %+v", got)
	}
	classes["c"] = holdClass{per: true, exp: 500}
	tr.reclassify("c")
	tr.flushNow()
	if e := deltaEntries(t, cur.take(), &gen)["r/#"]; e.Class != wire.InterestPer || e.ExpirySec != 500 {
		t.Fatalf("reclassified %+v", e)
	}
	tr.SubscriptionRemoved("", "i/#", "", true, 7)
	tr.SubscriptionRemoved("c", "r/#", "", false, 0)
	tr.flushNow()
	got = deltaEntries(t, cur.take(), &gen)
	if got["i/#"].Class != wire.InterestNone || got["r/#"].Class != wire.InterestNone {
		t.Fatalf("removal %+v", got)
	}
}

func TestTrackerRestoreOfflineSessions(t *testing.T) {
	classes := map[string]holdClass{}
	tr := testTracker(classes)
	cur, _ := tr.attach()
	var gen uint32
	hc := holdClass{per: true, exp: 600}
	tr.restore("keep", []string{"a/#", "b/#", "$SYS/#"}, hc, 0)
	tr.restore("clean", []string{"c/#"}, hc, 0)
	tr.restore("gone", []string{"d/#"}, hc, 20*time.Millisecond)
	tr.flushNow()
	got := deltaEntries(t, cur.take(), &gen)
	if len(got) != 4 || got["a/#"].Class != wire.InterestPer || got["a/#"].ExpirySec != 600 || got["d/#"].Class != wire.InterestPer {
		t.Fatalf("restored %+v", got)
	}

	// "keep" reconnects and takes over a/# only; "clean" starts clean and adds nothing.
	tr.SubscriptionAdded("keep", "a/#", "", false, 0)
	classes["keep"] = hc
	tr.reclassify("keep")
	tr.reclassify("clean")
	tr.flushNow()
	got = deltaEntries(t, cur.take(), &gen)
	if len(got) != 2 || got["b/#"].Class != wire.InterestNone || got["c/#"].Class != wire.InterestNone {
		t.Fatalf("after reconnect %+v", got)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		tr.flushNow()
		if got = deltaEntries(t, cur.take(), &gen); len(got) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("offline session did not expire")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(got) != 1 || got["d/#"].Class != wire.InterestNone {
		t.Fatalf("expiry %+v", got)
	}
	if st := tr.status(); st.Filters != 1 {
		t.Fatalf("filters %d, want only a/#", st.Filters)
	}
}

func TestTrackerSetProvidedReplaces(t *testing.T) {
	tr := testTracker(nil)
	cur, _ := tr.attach()
	var gen uint32
	tr.SetProvided("archive", []string{"v/#"}, []string{"p/#"})
	tr.flushNow()
	got := deltaEntries(t, cur.take(), &gen)
	if got["v/#"].Class != wire.InterestVol || got["p/#"].Class != wire.InterestPer || got["p/#"].ExpirySec != wire.InterestExpiryNever {
		t.Fatalf("provided %+v", got)
	}
	tr.SetProvided("archive", nil, []string{"p/#", "q/#"})
	tr.flushNow()
	got = deltaEntries(t, cur.take(), &gen)
	if len(got) != 2 || got["v/#"].Class != wire.InterestNone || got["q/#"].Class != wire.InterestPer {
		t.Fatalf("replace %+v", got)
	}
	tr.SetProvided("archive", nil, nil)
	tr.flushNow()
	if got = deltaEntries(t, cur.take(), &gen); len(got) != 2 {
		t.Fatalf("clear %+v", got)
	}
}

func TestTrackerSplitAndSnapshot(t *testing.T) {
	tr := testTracker(nil)
	n := trackerFrameEntries*2 + 10
	fs := make([]string, n)
	for i := range fs {
		fs[i] = fmt.Sprintf("site/%05d/#", i)
	}
	cur, _ := tr.attach()
	tr.FiltersAdded(fs) // crosses trackerFlushPending: flushed without the timer
	tr.flushNow()
	var gen uint32
	frames := cur.take()
	for _, f := range frames {
		if d := f.(*wire.InterestDelta); len(d.Entries) > trackerFrameEntries {
			t.Fatalf("delta with %d entries", len(d.Entries))
		}
	}
	if got := deltaEntries(t, frames, &gen); len(got) != n {
		t.Fatalf("delta filters %d", len(got))
	}

	_, snap := tr.attach()
	if len(snap) < 3 {
		t.Fatalf("snapshot frames %d", len(snap))
	}
	sgen, got := snapshotEntries(t, snap)
	if sgen != gen || len(got) != n {
		t.Fatalf("snapshot gen %d (want %d), filters %d", sgen, gen, len(got))
	}

	// Long filters split by bytes before the entry cap.
	long := make([]wire.InterestEntry, 200)
	for i := range long {
		long[i] = wire.InterestEntry{Class: wire.InterestVol, Filter: fmt.Sprintf("%0900d", i)}
	}
	var parts []int
	splitInterest(long, wire.InterestDeltaHeaderLen, func(es []wire.InterestEntry) {
		size := 1 + wire.InterestDeltaHeaderLen
		for _, e := range es {
			size += e.Len()
		}
		if size > wire.MaxConsumerFrame {
			t.Fatalf("frame of %d bytes", size)
		}
		parts = append(parts, len(es))
	})
	total := 0
	for _, p := range parts {
		total += p
	}
	if len(parts) < 2 || total != len(long) {
		t.Fatalf("parts %v", parts)
	}
}

func TestTrackerCursorOverflowResnaps(t *testing.T) {
	tr := testTracker(nil)
	cur, _ := tr.attach()
	for i := 0; i <= trackerCursorFrames; i++ {
		tr.FiltersAdded([]string{fmt.Sprintf("f/%d", i)})
		tr.flushNow()
	}
	fs := cur.take()
	if _, ok := fs[0].(*wire.InterestSnapshot); !ok {
		t.Fatalf("overflow sent %T, want a snapshot", fs[0])
	}
	gen, got := snapshotEntries(t, fs)
	if len(got) != trackerCursorFrames+1 || gen != uint32(trackerCursorFrames+1) {
		t.Fatalf("resnap gen %d filters %d", gen, len(got))
	}
	tr.FiltersAdded([]string{"after"})
	tr.flushNow()
	if got := deltaEntries(t, cur.take(), &gen); got["after"].Class != wire.InterestVol {
		t.Fatalf("delta after resnap %+v", got)
	}
	cur.detach()
	tr.FiltersAdded([]string{"detached"})
	tr.flushNow()
	if fs := cur.take(); len(fs) != 0 {
		t.Fatalf("detached cursor got %d frames", len(fs))
	}
}

func TestTrackerFlushTimer(t *testing.T) {
	tr := newInterestTracker(256, false, 20*time.Millisecond, func(string) holdClass { return holdClass{} }, quietLogger())
	cur, _ := tr.attach()
	tr.FiltersAdded([]string{"t/#"})
	select {
	case <-cur.notifyChan():
	case <-time.After(2 * time.Second):
		t.Fatal("flush timer did not fire")
	}
	var gen uint32
	if got := deltaEntries(t, cur.take(), &gen); got["t/#"].Class != wire.InterestVol {
		t.Fatalf("timer delta %+v", got)
	}
	tr.close()
	tr.FiltersAdded([]string{"u/#"})
	select {
	case <-cur.notifyChan():
		t.Fatal("closed tracker announced")
	case <-time.After(60 * time.Millisecond):
	}
}

func withInterest(c *config.PeerLinkConfig, _ *Deps) {
	c.Interest.Enabled = boolp(true)
	c.Interest.FlushMs = intp(5)
}

func TestLinkInterestRouting(t *testing.T) {
	a, b := pair(t, []nodeOpt{withInterest}, []nodeOpt{withInterest})
	if err := b.srv.Unsubscribe("#", 1); err != nil {
		t.Fatal(err)
	}
	if err := b.srv.Subscribe("a/#", 2, b.recv.handle); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "A sees B's filter", func() bool {
		in := consumerStatus(a, "node-b").Interest
		return in != nil && in.Mode == "FILTERED" && in.Filters == 1 && a.m.Status().Interest.DeltasReceived >= 1
	})

	publishPkt(t, a, packets.Packet{TopicName: "b/x", Payload: []byte("1")})
	publishPkt(t, a, packets.Packet{TopicName: "a/x", Payload: []byte("2")})
	eventually(t, 5*time.Second, "a/x on B", func() bool { return len(b.recv.byTopic("a/x")) == 1 })
	eventually(t, 5*time.Second, "b/x skipped on A", func() bool {
		in := a.m.Status().Interest
		return in != nil && in.InterestSkipped >= 1
	})
	if n := len(b.recv.byTopic("b/x")); n != 0 {
		t.Fatalf("b/x forwarded %d times without interest", n)
	}

	if err := b.srv.Subscribe("b/#", 3, b.recv.handle); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "A sees the delta", func() bool {
		in := consumerStatus(a, "node-b").Interest
		return in != nil && in.Filters == 2 && a.m.Status().Interest.DeltasReceived >= 2
	})
	publishPkt(t, a, packets.Packet{TopicName: "b/y", Payload: []byte("3")})
	eventually(t, 5*time.Second, "b/y on B", func() bool { return len(b.recv.byTopic("b/y")) == 1 })

	src := sourceStatus(b, "node-a").Interest
	if src == nil || !src.Active || src.DeltasSent == 0 || src.SnapshotsSent == 0 {
		t.Fatalf("source interest status %+v", src)
	}
	local := b.m.Status().Interest
	if local == nil || local.Local == nil || local.Local.Filters != 2 {
		t.Fatalf("local tracker status %+v", local)
	}
	filters := make([]string, 0)
	b.m.tracker.mu.Lock()
	for f := range b.m.tracker.announced {
		filters = append(filters, f)
	}
	b.m.tracker.mu.Unlock()
	sort.Strings(filters)
	if strings.Join(filters, ",") != "a/#,b/#" {
		t.Fatalf("announced %v", filters)
	}
}

func vol(f string) wire.InterestEntry { return wire.InterestEntry{Class: wire.InterestVol, Filter: f} }

func per(f string, exp uint32) wire.InterestEntry {
	return wire.InterestEntry{Class: wire.InterestPer, ExpirySec: exp, Filter: f}
}

func gone(f string) wire.InterestEntry {
	return wire.InterestEntry{Class: wire.InterestNone, Filter: f}
}

func testTable(unknownAll bool, maxFilters int) *interestTable {
	return newInterestTable([]interestPeerConfig{{"p0", true}, {"p1", true}, {"off", false}}, unknownAll, maxFilters, 256, quietLogger())
}

func TestTableUnknownModes(t *testing.T) {
	all := testTable(true, 100)
	if m := all.match("x/y"); m != 0b111 {
		t.Fatalf("Unknown ALL mask %b", m)
	}
	if s := all.status(2); s.State != "OFF" || s.Mode != "ALL" {
		t.Fatalf("disabled peer %+v", s)
	}
	none := testTable(false, 100)
	if m := none.match("x/y"); m != 0b100 {
		t.Fatalf("Unknown NONE mask %b", m)
	}
	if s := none.status(0); s.State != "UNKNOWN" || s.Mode != "NONE" {
		t.Fatalf("unknown peer %+v", s)
	}
	// A peer that does not agree CapInterest is served dense.
	none.connect(0, 1, false)
	if m := none.match("x/y"); m != 0b101 {
		t.Fatalf("dense mask %b", m)
	}
}

func TestTableSnapshotAndDelta(t *testing.T) {
	tb := testTable(true, 100)
	now := time.Now()
	tb.connect(0, 1, true)
	tb.connect(1, 2, true)
	if err := tb.applySnapshot(0, &wire.InterestSnapshot{Generation: 5, Flags: wire.InterestFlagFirst,
		Entries: []wire.InterestEntry{vol("a/#")}}, now); err != nil {
		t.Fatal(err)
	}
	if m := tb.match("b/x"); m&1 == 0 {
		t.Fatal("an open snapshot must not change the served set")
	}
	if err := tb.applySnapshot(0, &wire.InterestSnapshot{Generation: 6, Entries: []wire.InterestEntry{vol("b/+")}}, now); err != errInterestGeneration {
		t.Fatalf("generation mismatch: %v", err)
	}
	if err := tb.applySnapshot(1, &wire.InterestSnapshot{Generation: 1, Flags: wire.InterestFlagLast}, now); err != errInterestNoSnapshot {
		t.Fatalf("continuation without FIRST: %v", err)
	}
	if err := tb.applySnapshot(0, &wire.InterestSnapshot{Generation: 5, Flags: wire.InterestFlagFirst,
		Entries: []wire.InterestEntry{vol("a/#")}}, now); err != nil {
		t.Fatal(err)
	}
	if err := tb.applySnapshot(0, &wire.InterestSnapshot{Generation: 5, Flags: wire.InterestFlagLast,
		Entries: []wire.InterestEntry{per("b/+", 60), gone("c"), vol("bad/#/x")}}, now); err != nil {
		t.Fatal(err)
	}
	if got := tb.rejected.Load(); got != 2 {
		t.Fatalf("rejected %d (NONE in a snapshot and an invalid filter)", got)
	}
	if err := tb.applySnapshot(1, &wire.InterestSnapshot{Generation: 1, Flags: wire.InterestFlagFirst | wire.InterestFlagLast}, now); err != nil {
		t.Fatal(err)
	}
	for topic, want := range map[string]uint64{"a/1/2": 0b101, "b/x": 0b101, "b/x/y": 0b100, "z": 0b100, "$SYS/x": 0b100} {
		if m := tb.match(topic); m != want {
			t.Errorf("match %s = %b, want %b", topic, m, want)
		}
	}
	if s := tb.status(0); s.State != "LIVE" || s.Mode != "FILTERED" || s.Filters != 2 || s.FiltersPersistent != 1 || s.SnapshotGeneration != 5 {
		t.Fatalf("status %+v", s)
	}

	tb.applyDelta(1, &wire.InterestDelta{Generation: 2, Entries: []wire.InterestEntry{vol("z")}})
	tb.applyDelta(0, &wire.InterestDelta{Generation: 6, Entries: []wire.InterestEntry{gone("a/#"), vol("z")}})
	tb.applyDelta(0, &wire.InterestDelta{Generation: 6, Entries: []wire.InterestEntry{vol("stale")}})
	if m := tb.match("z"); m != 0b111 {
		t.Fatalf("delta mask %b", m)
	}
	if m := tb.match("a/1"); m != 0b100 {
		t.Fatalf("withdrawn mask %b", m)
	}
	if m := tb.match("stale"); m != 0b100 {
		t.Fatal("a delta at an old generation was applied")
	}
	if got := tb.deltasReceived.Load(); got != 3 {
		t.Fatalf("deltasReceived %d", got)
	}
}

func TestTableOverLimit(t *testing.T) {
	tb := testTable(false, 2)
	tb.connect(0, 1, true)
	snap := func(gen uint32, es ...wire.InterestEntry) {
		t.Helper()
		if err := tb.applySnapshot(0, &wire.InterestSnapshot{Generation: gen, Flags: wire.InterestFlagFirst | wire.InterestFlagLast, Entries: es}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	snap(1, vol("a"), vol("b"))
	tb.applyDelta(0, &wire.InterestDelta{Generation: 2, Entries: []wire.InterestEntry{vol("c")}})
	if s := tb.status(0); s.Mode != "ALL" || tb.overLimit.Load() != 1 {
		t.Fatalf("delta over the limit: %+v", s)
	}
	tb.applyDelta(0, &wire.InterestDelta{Generation: 3, Entries: []wire.InterestEntry{gone("c")}})
	if s := tb.status(0); s.Mode != "ALL" {
		t.Fatal("deltas must not leave ALL; only a snapshot within the limit does")
	}
	snap(4, vol("a"), vol("b"), vol("c"))
	if s := tb.status(0); s.Mode != "ALL" || tb.overLimit.Load() != 1 {
		t.Fatalf("snapshot over the limit: %+v, transitions %d", s, tb.overLimit.Load())
	}
	snap(5, vol("a"))
	if s := tb.status(0); s.Mode != "FILTERED" || s.Filters != 1 || tb.match("b")&1 != 0 {
		t.Fatalf("back within the limit: %+v", s)
	}
}

func TestTableRestartAndExpiry(t *testing.T) {
	tb := testTable(false, 100)
	t0 := time.Now()
	tb.connect(0, 1, true)
	if err := tb.applySnapshot(0, &wire.InterestSnapshot{Generation: 1, Flags: wire.InterestFlagFirst | wire.InterestFlagLast,
		Entries: []wire.InterestEntry{vol("v"), per("short", 10), per("long", 1000), per("never", wire.InterestExpiryNever)}}, t0); err != nil {
		t.Fatal(err)
	}
	tb.disconnect(0, t0)
	if s := tb.status(0); s.State != "DISCONNECTED" || s.Mode != "FILTERED" {
		t.Fatalf("disconnected keeps filtering: %+v", s)
	}
	tb.expire(t0.Add(5 * time.Second))
	if tb.status(0).Filters != 4 {
		t.Fatal("expired too early")
	}
	tb.expire(t0.Add(11 * time.Second))
	if s := tb.status(0); s.Filters != 3 || tb.persistentExpiry.Load() != 1 || tb.match("short")&1 != 0 {
		t.Fatalf("after expiry %+v", s)
	}
	// Same instance reconnects: volatile interest stays until its snapshot.
	tb.connect(0, 1, true)
	if tb.match("v")&1 == 0 {
		t.Fatal("volatile interest dropped on a reconnect of the same instance")
	}
	tb.disconnect(0, t0)
	tb.connect(0, 2, true)
	if s := tb.status(0); s.Filters != 2 || tb.volatileDropped.Load() != 1 || tb.match("v")&1 != 0 || tb.match("long")&1 == 0 {
		t.Fatalf("after restart %+v", s)
	}
}

// gateProxy forwards B's connections to A; cut closes them and refuses new ones until reopen.
type gateProxy struct {
	ln     net.Listener
	target string
	closed atomic.Bool
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
}

func startGateProxy(t *testing.T, target string) *gateProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &gateProxy{ln: ln, target: target, conns: map[net.Conn]struct{}{}}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", target)
			if p.closed.Load() || err != nil {
				_ = c.Close()
				if up != nil {
					_ = up.Close()
				}
				continue
			}
			p.mu.Lock()
			p.conns[c], p.conns[up] = struct{}{}, struct{}{}
			p.mu.Unlock()
			go func() { _, _ = io.Copy(up, c); _ = up.Close(); _ = c.Close() }()
			go func() { _, _ = io.Copy(c, up); _ = up.Close(); _ = c.Close() }()
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); p.cut() })
	return p
}

func (p *gateProxy) cut() {
	p.closed.Store(true)
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		_ = c.Close()
	}
	clear(p.conns)
}

// TestLinkInterestThroughput is the in-process G-IR1 end-to-end check. It is
// hand-run (PEERLINK_BENCH=1 go test -run LinkInterestThroughput -v) because
// wall-clock ratios are meaningless under -race or a loaded CI host.
//
// The link is cut while the source captures the publishes, then reopened; the
// drain to the consumer is the link throughput the gate compares. Timing the
// publisher instead measures the local engine, which interest routing does not
// change.
func TestLinkInterestThroughput(t *testing.T) {
	if os.Getenv("PEERLINK_BENCH") == "" {
		t.Skip("set PEERLINK_BENCH=1")
	}
	const total = 300000
	// Catch-up pacing is off so the drain measures the link, not the pacer.
	fastReconnect := func(c *config.PeerLinkConfig, _ *Deps) {
		c.Fetch.ReconnectMaxMs = intp(5)
		c.Receive.CatchUpRateFactor = new(float64)
	}
	run := func(t *testing.T, interest bool, pct int) (ingest, drain time.Duration, bytes uint64) {
		aOpts, bOpts := []nodeOpt{}, []nodeOpt{fastReconnect}
		if interest {
			aOpts, bOpts = append(aOpts, withInterest), append(bOpts, withInterest)
		}
		a := startNode(t, "node-a", "127.0.0.1:0", []config.PeerConfig{{NodeID: "node-b"}}, aOpts...)
		px := startGateProxy(t, a.addr)
		b := newNode(t, "node-b", "", []config.PeerConfig{{NodeID: "node-a", Address: px.ln.Addr().String(), Serve: boolp(false)}}, bOpts...)
		if err := b.srv.Unsubscribe("#", 1); err != nil {
			t.Fatal(err)
		}
		var got atomic.Int64
		if err := b.srv.Subscribe("want/#", 2, func(*mqtt.Client, packets.Subscription, packets.Packet) { got.Add(1) }); err != nil {
			t.Fatal(err)
		}
		if err := b.m.Start(); err != nil {
			t.Fatal(err)
		}
		waitStreaming(t, b, "node-a")
		if interest {
			eventually(t, 5*time.Second, "A sees B's filter", func() bool {
				in := consumerStatus(a, "node-b").Interest
				return in != nil && in.Mode == "FILTERED"
			})
		}
		px.cut()
		eventually(t, 5*time.Second, "link down", func() bool { return sourceStatus(b, "node-a").State != "STREAMING" })
		topics := make([]string, 100)
		for i := range topics {
			if i < pct {
				topics[i] = fmt.Sprintf("want/area/line%d/machine/temp", i)
			} else {
				topics[i] = fmt.Sprintf("skip/area/line%d/machine/temp", i)
			}
		}
		payload := []byte("21.5")
		want := int64(total * pct / 100)
		start := time.Now()
		for i := 0; i < total; i++ {
			publishPkt(t, a, packets.Packet{TopicName: topics[i%100], Payload: payload})
		}
		ingest = time.Since(start)
		start = time.Now()
		px.closed.Store(false)
		for got.Load() < want {
			if time.Since(start) > 60*time.Second {
				t.Fatalf("drain: %d of %d, source %+v", got.Load(), want, sourceStatus(b, "node-a"))
			}
			time.Sleep(time.Millisecond)
		}
		return ingest, time.Since(start), sourceStatus(b, "node-a").AppliedBytes
	}
	median := func(d []time.Duration) time.Duration {
		slices.Sort(d)
		return d[len(d)/2]
	}
	const rounds = 5
	for _, pct := range []int{10, 100} {
		var offs, ons, offIns, onIns []time.Duration
		var offB, onB uint64
		for range rounds {
			in, d, by := run(t, false, pct)
			offIns, offs, offB = append(offIns, in), append(offs, d), by
			in, d, by = run(t, true, pct)
			onIns, ons, onB = append(onIns, in), append(ons, d), by
		}
		off, on := median(offs), median(ons)
		t.Logf("%3d%% interest (median of %d): link off %v (%.0f rec/s, %d B), on %v, link throughput %.2fx, bytes %.2fx; capture off %v, on %v",
			pct, rounds, off, total/off.Seconds(), offB, on, off.Seconds()/on.Seconds(), float64(offB)/float64(max(onB, 1)),
			median(offIns), median(onIns))
	}
}

// TestTrackerGenerationWrap checks that the delta generation never wraps: the tracker resets
// to generation 0 with a snapshot and the source table keeps applying later deltas.
func TestTrackerGenerationWrap(t *testing.T) {
	tr := testTracker(nil)
	cur, initial := tr.attach()
	tb := testTable(false, 100)
	tb.connect(0, 1, true)
	apply := func(fs []wire.Frame) {
		t.Helper()
		for _, f := range fs {
			switch f := f.(type) {
			case *wire.InterestSnapshot:
				if err := tb.applySnapshot(0, f, time.Now()); err != nil {
					t.Fatal(err)
				}
			case *wire.InterestDelta:
				tb.applyDelta(0, f)
			}
		}
	}
	apply(initial)
	tr.mu.Lock()
	tr.gen = math.MaxUint32 - 1
	tr.mu.Unlock()
	tr.FiltersAdded([]string{"a/#"})
	tr.flushNow()
	apply(cur.take())
	if tb.match("a/1")&1 == 0 {
		t.Fatal("delta before the wrap not applied")
	}
	tr.FiltersAdded([]string{"b/#"})
	tr.flushNow()
	fs := cur.take()
	if _, ok := fs[0].(*wire.InterestSnapshot); !ok {
		t.Fatalf("wrap sent %T, want a snapshot", fs[0])
	}
	if gen, got := snapshotEntries(t, fs); gen != 0 || len(got) != 2 {
		t.Fatalf("wrap snapshot gen %d filters %d", gen, len(got))
	}
	apply(fs)
	tr.FiltersAdded([]string{"c/#"})
	tr.flushNow()
	apply(cur.take())
	for _, topic := range []string{"a/1", "b/1", "c/1"} {
		if tb.match(topic)&1 == 0 {
			t.Fatalf("%s not served after the wrap", topic)
		}
	}
}

// TestTableExpirySweep expires one of overlapping persistent filters: the sweep clears only the
// expired peer's bit of uncovered, non-retained records, keeps other peers' bits and needs
// several ticks for a log larger than MaxScan.
func TestTableExpirySweep(t *testing.T) {
	tb := testTable(false, 100)
	lg := logTestNew(t, LogConfig{Masked: true, Consumers: []string{"p0", "p1", "off"}, MaxScan: logMinMaxScan})
	t0 := time.Now()
	tb.connect(0, 1, true)
	tb.connect(1, 1, true)
	snap := func(idx int, es ...wire.InterestEntry) {
		t.Helper()
		if err := tb.applySnapshot(idx, &wire.InterestSnapshot{Generation: 1, Flags: wire.InterestFlagFirst | wire.InterestFlagLast, Entries: es}, t0); err != nil {
			t.Fatal(err)
		}
	}
	snap(0, per("x/#", 10), vol("x/keep/#"), per("x/+/held", 1000), per("y/#", 1000))
	snap(1, vol("x/#"))

	topics := []string{"x/drop/a", "x/keep/a", "x/b/held", "y/a"}
	const n = 3000
	offs := map[string][]uint64{}
	for i := range n {
		topic := topics[i%len(topics)]
		var mod func(*wire.Record)
		if i == 0 {
			topic = "x/drop/retained"
			mod = func(r *wire.Record) { r.Flags |= wire.FlagRetain }
		}
		off, _ := lg.AppendMask(rec(topic, "p", mod), LogKindClient, tb.match(topic)|0b100)
		offs[topic] = append(offs[topic], off)
	}

	tb.disconnect(0, t0)
	tb.disconnect(1, t0)
	tb.expire(t0.Add(11 * time.Second))
	if tb.status(0).Filters != 3 {
		t.Fatalf("after expiry %+v", tb.status(0))
	}
	ticks := 0
	for {
		tb.sweep(lg)
		ticks++
		tb.mu.RLock()
		busy := tb.peers[0].sweeping
		tb.mu.RUnlock()
		if !busy {
			break
		}
		if ticks > 10 {
			t.Fatal("sweep did not finish")
		}
	}
	if ticks < 3 {
		t.Fatalf("sweep finished in %d ticks, want several for %d records", ticks, n)
	}
	lg.mu.Lock()
	defer lg.mu.Unlock()
	for topic, list := range offs {
		for _, off := range list {
			m := lg.maskAtLocked(off)
			want0 := uint64(1)
			if topic == "x/drop/a" {
				want0 = 0
			}
			if m&1 != want0 || m&0b100 == 0 || (topic[0] == 'x' && m&0b10 == 0) {
				t.Fatalf("%s at %d: mask %b", topic, off, m)
			}
		}
	}
	if got, want := tb.backlogDiscarded.Load(), uint64(len(offs["x/drop/a"])); got != want {
		t.Fatalf("backlogDiscarded %d, want %d", got, want)
	}
}
