package peerlink

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/peerlink/wire"
)

func TestTopicFilterMatching(t *testing.T) {
	f, err := compileFilters([]string{"a/b", "pre/#", "x/+/z", "m/+/#", "+/leaf"})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"a/b": true, "a/b/c": false, "a": false,
		"pre": true, "pre/x": true, "pre/x/y": true, "prefix": false, "pr": false,
		"x/y/z": true, "x/y": false, "x/y/z/w": false, "x//z": true,
		"m/1": true, "m/1/2/3": true, "m": false,
		"q/leaf": true, "q/leaf/x": false,
		"other": false,
	}
	for topic, want := range cases {
		if got := f.match(topic); got != want {
			t.Errorf("match(%q) = %v, want %v", topic, got, want)
		}
	}
	all, _ := compileFilters([]string{"#"})
	if !all.match("anything/at/all") {
		t.Fatal("# does not match")
	}
	empty, _ := compileFilters(nil)
	if empty.match("x") {
		t.Fatal("empty list matches")
	}
	for _, bad := range []string{"a/#/b", "a+/b", "a/b#", ""} {
		if _, err := compileFilters([]string{bad}); err == nil {
			t.Errorf("filter %q accepted", bad)
		}
	}
	ie, err := newIncludeExclude(nil, []string{"secret/#"})
	if err != nil {
		t.Fatal(err)
	}
	if !ie.accept("public") || ie.accept("secret/x") || ie.accept("secret") {
		t.Fatal("include/exclude")
	}
}

func TestUnderRoot(t *testing.T) {
	cases := []struct {
		t, root string
		want    bool
	}{
		{"winccoa", "winccoa", true}, {"winccoa/x", "winccoa", true}, {"winccoax", "winccoa", false},
		{"winc", "winccoa", false}, {"x", "", false}, {"a/b/c", "a/b", true},
	}
	for _, c := range cases {
		if underRoot(c.t, c.root) != c.want || underRootBytes([]byte(c.t), c.root) != c.want {
			t.Errorf("underRoot(%q, %q) != %v", c.t, c.root, c.want)
		}
	}
}

func TestFilterMatchDoesNotAllocate(t *testing.T) {
	f, _ := compileFilters([]string{"pre/#", "x/+/z", "a/b"})
	allocs := testing.AllocsPerRun(1000, func() {
		f.match("x/yy/z")
		f.match("pre/a/b")
		f.match("nomatch/at/all")
	})
	if allocs != 0 {
		t.Fatalf("%v allocs per match", allocs)
	}
}

func TestPacer(t *testing.T) {
	pc := pacer{factor: 3}
	pc.observe(1000, 1000)
	pc.observe(2000, 2000) // 1000 records/s
	if r := pc.limit(10, 4096); r != 0 {
		t.Fatalf("paced without lag: %v", r)
	}
	if r := pc.limit(10000, 4096); r != 3000 {
		t.Fatalf("rate %v, want 3000", r)
	}
	pc.rate = 10
	if r := pc.limit(10000, 4096); r != pacerFloor {
		t.Fatalf("floor %v", r)
	}
	pc.maxRate = 500
	if r := pc.limit(0, 4096); r != 500 {
		t.Fatalf("absolute cap without lag %v", r)
	}
	if r := (&pacer{}).limit(1e9, 1); r != 0 {
		t.Fatal("factor 0 paces")
	}
	// Token bucket: 200 tokens at 1000/s takes about 100 ms after the initial burst.
	pc = pacer{}
	start := time.Now()
	slept := 0
	for i := 0; i < 200; i++ {
		if pc.take(context.Background(), 1000) {
			slept++
		}
	}
	if el := time.Since(start); el < 80*time.Millisecond || el > 2*time.Second || slept == 0 {
		t.Fatalf("200 tokens at 1000/s took %v (slept %d)", el, slept)
	}
}

func TestInternCache(t *testing.T) {
	c := newInternCache()
	c.maxEntries = 2
	a := c.get([]byte("a"))
	if c.get([]byte("a")) != a {
		t.Fatal("not interned")
	}
	c.get([]byte("b"))
	c.get([]byte("c")) // rotates: cur = {c}, old = {a, b}
	if _, ok := c.old["a"]; !ok {
		t.Fatal("generation not rotated")
	}
	c.get([]byte("a")) // promoted
	if _, ok := c.cur["a"]; !ok {
		t.Fatal("old entry not promoted")
	}
	c = newInternCache()
	c.get([]byte("topic/x"))
	if n := testing.AllocsPerRun(100, func() { c.get([]byte("topic/x")) }); n != 0 {
		t.Fatalf("hit allocates %v", n)
	}

	// Bounded by bytes, and long strings are not cached (review finding 17).
	c = newInternCache()
	long := bytes.Repeat([]byte("x"), internMaxLen+1)
	c.get(long)
	if len(c.cur) != 0 {
		t.Fatal("long string interned")
	}
	for i := 0; i < 200000; i++ {
		k := fmt.Appendf(nil, "%0250d", i)
		c.get(k)
	}
	if c.curBytes > internGenBytes || (len(c.cur)+len(c.old))*250 > 2*internGenBytes+250 {
		t.Fatalf("cache holds %d+%d entries, %d bytes", len(c.cur), len(c.old), c.curBytes)
	}
}

func TestLatencyHistogram(t *testing.T) {
	var h latencyHist
	if h.quantile(0.5) != -1 {
		t.Fatal("empty histogram")
	}
	for i := 0; i < 98; i++ {
		h.observe(1)
	}
	h.observe(400)
	h.observe(1e9)
	if h.quantile(0.5) != 1 || h.quantile(0.99) != 500 || h.quantile(1) <= 300000 {
		t.Fatalf("p50 %d p99 %d max %d", h.quantile(0.5), h.quantile(0.99), h.quantile(1))
	}
}

func TestSnapshotRecord(t *testing.T) {
	now := time.Now()
	nowSec := now.Unix()
	pk := packets.Packet{TopicName: "t", Payload: []byte("v"), Origin: "dev", Created: nowSec - 10, Expiry: nowSec + 50,
		Properties: packets.Properties{MessageExpiryInterval: 60}}
	rec, ok := snapshotRecord(pk, now, 100000)
	if !ok {
		t.Fatal("not encodable")
	}
	age := (100000 - rec.CaptureMonoMs) / 1000
	if age != 10 || int64(rec.ExpirySec)-int64(age) != 50 || rec.Flags&wire.FlagSnapshot == 0 || rec.Flags&wire.FlagRetain == 0 || rec.ClientID != "dev" {
		t.Fatalf("record %+v", rec)
	}
	// Older than the epoch: captureMonoMs clamps at 0 and the remaining expiry stays right.
	rec, ok = snapshotRecord(pk, now, 3000)
	if !ok || rec.CaptureMonoMs != 0 || int64(rec.ExpirySec)-3 != 50 {
		t.Fatalf("clamped %+v", rec)
	}
	pk.Expiry = nowSec - 1
	if _, ok := snapshotRecord(pk, now, 100000); ok {
		t.Fatal("expired value in snapshot")
	}
	pk.Expiry = 0
	pk.Properties.MessageExpiryInterval = 0
	if rec, ok := snapshotRecord(pk, now, 100000); !ok || rec.ExpirySec != 0 {
		t.Fatal("value without expiry")
	}
}

func TestCaptureValidatesInlineTopics(t *testing.T) {
	n := newNode(t, "node-a", "127.0.0.1:0", []config.PeerConfig{{NodeID: "node-b"}})
	// The inline client bypasses topic validation; capture applies the receiver's rules.
	_ = n.srv.PublishPacket(packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Publish}, TopicName: "bad\x00topic", Payload: []byte("x")})
	_ = n.srv.PublishPacket(packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Publish}, TopicName: "bad/\xff", Payload: []byte("x")})
	_ = n.srv.PublishPacket(packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Publish}, TopicName: "good", Payload: []byte("x")})
	st := n.m.Status().Log
	if st.CaptureDropped["invalid"] != 2 || st.Appended.Inline != 1 {
		t.Fatalf("captureDropped %+v appended %+v", st.CaptureDropped, st.Appended)
	}
}

func TestCaptureSizeGuard(t *testing.T) {
	n := newNode(t, "node-a", "127.0.0.1:0", []config.PeerConfig{{NodeID: "node-b"}},
		func(c *config.PeerLinkConfig, _ *Deps) { c.Log.MaxRecordBytes = 1024 })
	publishPkt(t, n, packets.Packet{TopicName: "big", Payload: make([]byte, 2048)})
	publishPkt(t, n, packets.Packet{TopicName: "small", Payload: make([]byte, 10)})
	st := n.m.Status().Log
	if st.CaptureDropped["size"] != 1 || st.Appended.Inline != 1 {
		t.Fatalf("captureDropped %+v appended %+v", st.CaptureDropped, st.Appended)
	}
}

func TestNoServePeersCapturesNothing(t *testing.T) {
	n := newNode(t, "node-b", "", []config.PeerConfig{{NodeID: "node-a", Address: "127.0.0.1:1", Serve: boolp(false)}})
	publishPkt(t, n, packets.Packet{TopicName: "x", Payload: []byte("1")})
	st := n.m.Status()
	if st.Log.Active || st.Log.LEO != 0 || len(st.Consumers) != 0 || len(st.Sources) != 1 {
		t.Fatalf("pull-only node %+v", st)
	}
}

// A pull-only node serves the loopback status and resync endpoints (review finding 24) and
// refuses the peer protocol.
func TestPullOnlyNodeServesStatus(t *testing.T) {
	n := startNode(t, "node-b", "127.0.0.1:0", []config.PeerConfig{{NodeID: "node-a", Address: "127.0.0.1:1", Serve: boolp(false)}})
	if n.addr == "" {
		t.Fatal("no status listener on a pull-only node")
	}
	code, body := httpRaw(t, n.addr, "GET /peerlink/v1/status HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")
	if code != 200 || !bytes.Contains(body, []byte(`"node-a"`)) {
		t.Fatalf("status %d %s", code, body)
	}
	if code, _ := httpRaw(t, n.addr, "POST /peerlink/v1/resync?source=node-a HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n"); code != 202 {
		t.Fatalf("resync %d", code)
	}
	expectGoAway(t, dialRaw(t, n.addr).hello("node-a", "node-b", nil), wire.GoAwayNotAllowed)
}

func TestSessionTimes(t *testing.T) {
	s := newSessionTimes()
	s.record("c1", 1000)
	if !s.since("c1", 1000) || s.since("c1", 1001) || s.since("c2", 0) {
		t.Fatal("since")
	}
	// Reconnects with ever new (long) client ids stay bounded (review finding 20).
	long := strings.Repeat("x", 100)
	for i := 0; i < hookShards*sessionTimesShardMax*2; i++ {
		s.record(long+strconv.Itoa(i), int64(2000+i))
	}
	total := 0
	for i := range s.shards {
		total += len(s.shards[i].m)
	}
	if total > hookShards*sessionTimesShardMax {
		t.Fatalf("%d session times kept", total)
	}
	last := long + strconv.Itoa(hookShards*sessionTimesShardMax*2-1)
	if !s.since(last, 0) {
		t.Fatal("newest session time evicted")
	}
}

func TestEchoTable(t *testing.T) {
	e := newEchoTable(100)
	ms := int64(time.Millisecond)
	e.record("t", []byte("v"), false, 0)
	if !e.match("t", []byte("v"), false, 50*ms) || e.match("t", []byte("w"), false, 50*ms) ||
		e.match("t", []byte("v"), true, 50*ms) || e.match("t", []byte("v"), false, 200*ms) || e.match("u", []byte("v"), false, 0) {
		t.Fatal("echo match")
	}
}

func TestCloseWithoutStartAndTwice(t *testing.T) {
	n := newNode(t, "node-a", "127.0.0.1:0", []config.PeerConfig{{NodeID: "node-b", Address: "127.0.0.1:1"}})
	if err := n.m.Close(); err != nil {
		t.Fatal(err)
	}
	_ = n.m.Close()
	n.m.StopPullers(context.Background())
	if err := n.m.Start(); err == nil {
		t.Fatal("start after close")
	}
}
