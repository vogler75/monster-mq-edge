package peerlink

import (
	"bufio"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/peerlink/wire"
)

// fakeSource is a scripted source built on the wire codec. Each FETCH takes the next scripted
// response; when the script is used up it answers EMPTY batches after maxWaitMs.
type fakeSource struct {
	t       *testing.T
	ln      net.Listener
	addr    string
	helloOK func(h *wire.Hello) wire.HelloOK

	mu       sync.Mutex
	script   []func(f *wire.Fetch) []byte
	hellos   []wire.Hello
	goAways  []wire.GoAway
	commit   atomic.Uint64
	sessions atomic.Int32
	emptyLeo atomic.Uint64 // leo announced in EMPTY batches; 0 = the fetch offset
}

const fakeEpoch = 77

func newFakeSource(t *testing.T) *fakeSource {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeSource{t: t, ln: ln, addr: ln.Addr().String()}
	fs.helloOK = func(h *wire.Hello) wire.HelloOK {
		return wire.HelloOK{Capabilities: wire.CapsV1 & h.Capabilities, Epoch: fakeEpoch, ResumeAt: max(h.ResumeOffset, 1),
			LogStart: 1, Leo: 1, MaxRecordBytes: 1 << 20, SourceNodeID: "node-a"}
	}
	go fs.acceptLoop()
	t.Cleanup(func() { _ = ln.Close() })
	return fs
}

func (fs *fakeSource) push(fns ...func(f *wire.Fetch) []byte) {
	fs.mu.Lock()
	fs.script = append(fs.script, fns...)
	fs.mu.Unlock()
}

func (fs *fakeSource) acceptLoop() {
	for {
		c, err := fs.ln.Accept()
		if err != nil {
			return
		}
		go fs.serve(c)
	}
}

func (fs *fakeSource) serve(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	if _, _, err := wire.ReadPreamble(br); err != nil {
		return
	}
	_ = wire.WriteFrame(c, &wire.ServerHello{VersionMajor: 1, Capabilities: wire.CapsV1})
	fr := wire.NewFrameReader(br, wire.MaxConsumerFrame)
	t, body, err := fr.ReadFrame()
	if err != nil || t != wire.FrameHello {
		return
	}
	var h wire.Hello
	_ = h.Decode(body)
	fs.mu.Lock()
	fs.hellos = append(fs.hellos, h)
	fs.mu.Unlock()
	ok := fs.helloOK(&h)
	_ = wire.WriteFrame(c, &ok)
	fs.sessions.Add(1)
	for {
		t, body, err := fr.ReadFrame()
		if err != nil {
			return
		}
		switch t {
		case wire.FrameFetch:
			var f wire.Fetch
			_ = f.Decode(body)
			if f.Commit > 0 {
				fs.commit.Store(f.Commit)
			}
			fs.mu.Lock()
			var fn func(f *wire.Fetch) []byte
			if len(fs.script) > 0 {
				fn, fs.script = fs.script[0], fs.script[1:]
			}
			fs.mu.Unlock()
			var out []byte
			if fn != nil {
				out = fn(&f)
			}
			if out == nil {
				time.Sleep(time.Duration(min(f.MaxWaitMs, 100)) * time.Millisecond)
				flags := wire.BatchFlagEmpty
				if f.Flags&wire.FetchFlagSnapshot != 0 {
					flags |= wire.BatchFlagSnapshot | wire.BatchFlagSnapshotEnd
				}
				out = batchFrame(f.FetchID, f.Offset, flags, 0)
				if leo := fs.emptyLeo.Load(); leo > 0 {
					var bt wire.Batch
					_ = bt.Decode(out[wire.FrameHeaderLen:])
					bt.Header.Leo = leo
					bt.Header.CRC32C = 0
					bt.Header.Flags &^= wire.BatchFlagCRC
					out = bt.AppendFrame(nil)
				}
			}
			if _, err := c.Write(out); err != nil {
				return
			}
		case wire.FrameCommit:
			var cm wire.Commit
			_ = cm.Decode(body)
			fs.commit.Store(cm.Commit)
		case wire.FramePing:
			var p wire.Ping
			_ = p.Decode(body)
			_ = wire.WriteFrame(c, &wire.Pong{Token: p.Token})
		case wire.FrameGoAway:
			var g wire.GoAway
			_ = g.Decode(body)
			fs.mu.Lock()
			fs.goAways = append(fs.goAways, g)
			fs.mu.Unlock()
			return
		}
	}
}

func (fs *fakeSource) lastGoAway() (wire.GoAway, bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.goAways) == 0 {
		return wire.GoAway{}, false
	}
	return fs.goAways[len(fs.goAways)-1], true
}

const fakeMonoNow = 100000

func rec(topic, payload string, mod func(*wire.Record)) []byte {
	r := wire.Record{Topic: topic, Payload: []byte(payload), ClientID: "c1", CaptureMonoMs: fakeMonoNow - 10,
		PublishWallNs: time.Now().UnixNano()}
	if mod != nil {
		mod(&r)
	}
	return wire.AppendRecord(nil, &r)
}

// batchFrame encodes a BATCH with a valid CRC.
func batchFrame(fetchID uint32, base uint64, flags uint16, count uint32, recs ...[]byte) []byte {
	var region []byte
	for _, r := range recs {
		region = append(region, r...)
	}
	b := wire.Batch{Header: wire.BatchHeader{FetchID: fetchID, Flags: flags | wire.BatchFlagCRC, BaseOffset: base, Count: count,
		RecordsBytes: uint32(len(region)), LogStart: 1, Leo: base + uint64(count), SourceMonoMs: fakeMonoNow,
		SourceWallMs: time.Now().UnixMilli()}, Records: region}
	if flags&wire.BatchFlagSnapshot != 0 {
		b.Header.Leo = 1
	}
	b.Header.CRC32C = b.ComputeCRC()
	return b.AppendFrame(nil)
}

func batchOf(recs ...[]byte) func(f *wire.Fetch) []byte {
	return func(f *wire.Fetch) []byte { return batchFrame(f.FetchID, f.Offset, 0, uint32(len(recs)), recs...) }
}

func consumerOf(t *testing.T, fs *fakeSource, opts ...nodeOpt) *testNode {
	return startNode(t, "node-b", "", []config.PeerConfig{{NodeID: "node-a", Address: fs.addr, Serve: boolp(false)}}, opts...)
}

func TestReceiverValidationAndPolicies(t *testing.T) {
	fs := newFakeSource(t)
	inner := fs.helloOK
	fs.helloOK = func(h *wire.Hello) wire.HelloOK {
		ok := inner(h)
		ok.TopicRoot = "other"
		return ok
	}
	badVersion := rec("v/bad", "x", nil)
	badVersion[4] = 9
	retainedStale := rec("p/stale-retained", "sr", func(r *wire.Record) { r.Flags = wire.FlagRetain; r.CaptureMonoMs = fakeMonoNow - 5000 })
	fs.push(batchOf(
		rec("winccoa/x", "1", nil),
		rec("other/x", "1", nil),
		rec("$SYS/x", "1", nil),
		badVersion,
		wire.AppendTombstone(nil, rec("p/huge", "big", func(r *wire.Record) { r.Flags = wire.FlagRetain })),
		rec("p/expired", "e", func(r *wire.Record) { r.ExpirySec = 2; r.CaptureMonoMs = fakeMonoNow - 5000 }),
		rec("p/stale", "s", func(r *wire.Record) { r.CaptureMonoMs = fakeMonoNow - 5000 }),
		retainedStale,
		rec("p/flags", "f", func(r *wire.Record) { r.Flags = 1 << 12 }),
		rec("p/ok", "ok", func(r *wire.Record) { r.Flags = 1; r.ExpirySec = 60; r.Username = []byte("user1") }),
	))
	b := consumerOf(t, fs, func(c *config.PeerLinkConfig, d *Deps) {
		c.Receive.MaxRecordAgeMs = 1000
		d.NamespaceRoot = func() string { return "winccoa" }
	})
	eventually(t, 5*time.Second, "batch applied", func() bool { return fs.commit.Load() == 11 })
	ss := sourceStatus(b, "node-a")
	want := map[string]uint64{"namespace": 3, "malformed": 1, "size_source": 1, "expired": 1, "stale": 1}
	for k, v := range want {
		if ss.Dropped[k] != v {
			t.Errorf("dropped[%s] = %d, want %d (all %+v)", k, ss.Dropped[k], v, ss.Dropped)
		}
	}
	if ss.RetainedDiverged["size_source"] != 1 || ss.RetainOnly != 1 || ss.Injected != 2 {
		t.Fatalf("diverged %+v retainOnly %d injected %d", ss.RetainedDiverged, ss.RetainOnly, ss.Injected)
	}
	if b.recv.count("p/stale-retained") != 0 {
		t.Fatal("stale retained value delivered live")
	}
	if pk, ok := b.srv.Topics.Retained.Get("p/stale-retained"); !ok || string(pk.Payload) != "sr" {
		t.Fatal("stale retained value not stored silently")
	}
	got := b.recv.byTopic("p/ok")
	if len(got) != 1 || got[0].FixedHeader.Qos != 1 || got[0].Forward.Username != "user1" || got[0].Forward.Epoch != fakeEpoch || got[0].Forward.Offset != 10 {
		t.Fatalf("p/ok %+v", got)
	}
	if e := got[0].Properties.MessageExpiryInterval; e == 0 || e > 60 {
		t.Fatalf("remaining expiry %d", e)
	}
	if !sourceStatus(b, "node-a").TopicRootMismatch {
		t.Fatal("topicRootMismatch not set")
	}
	fs.mu.Lock()
	h := fs.hellos[0]
	fs.mu.Unlock()
	if h.ConsumerNodeID != "node-b" || h.ExpectedSourceNodeID != "node-a" || h.TopicRoot != "winccoa" || h.LastEpoch != 0 {
		t.Fatalf("hello %+v", h)
	}
}

func TestReceiverBatchStructuralFault(t *testing.T) {
	fs := newFakeSource(t)
	fs.push(func(f *wire.Fetch) []byte {
		return batchFrame(f.FetchID, f.Offset, 0, 4, rec("s/1", "1", nil), rec("s/2", "2", nil))
	})
	fs.push(batchOf(rec("s/3", "3", nil)))
	b := consumerOf(t, fs)
	eventually(t, 5*time.Second, "advance past the fault", func() bool { return fs.commit.Load() == 6 })
	ss := sourceStatus(b, "node-a")
	if ss.Dropped["malformed"] != 2 || b.recv.count("s/") != 3 {
		t.Fatalf("malformed %d delivered %d", ss.Dropped["malformed"], b.recv.count("s/"))
	}
}

func TestReceiverCRCPoisonAfterThreeFailures(t *testing.T) {
	fs := newFakeSource(t)
	bad := func(f *wire.Fetch) []byte {
		fr := batchFrame(f.FetchID, f.Offset, 0, 1, rec("c/x", "1", nil))
		fr[len(fr)-1] ^= 0xff
		return fr
	}
	fs.push(bad, bad, bad, batchOf(rec("c/y", "2", nil)))
	b := consumerOf(t, fs)
	eventually(t, 10*time.Second, "poison skipped", func() bool { return b.recv.count("c/y") == 1 })
	ss := sourceStatus(b, "node-a")
	if ss.CRCErrors != 3 || ss.Dropped["malformed"] != 1 || b.recv.count("c/x") != 0 || fs.sessions.Load() != 3 {
		t.Fatalf("crcErrors %d malformed %d sessions %d", ss.CRCErrors, ss.Dropped["malformed"], fs.sessions.Load())
	}
	// Reconnects in the same epoch resume where the consumer stopped.
	fs.mu.Lock()
	h := fs.hellos[len(fs.hellos)-1]
	fs.mu.Unlock()
	if h.LastEpoch != fakeEpoch || h.ResumeOffset != 1 {
		t.Fatalf("resume hello %+v", h)
	}
}

func TestReceiverRefusesWrongSource(t *testing.T) {
	for _, tc := range []struct {
		id   string
		code wire.GoAwayCode
	}{{"node-z", wire.GoAwayWrongNode}, {"node-b", wire.GoAwaySelfConnection}} {
		fs := newFakeSource(t)
		inner := fs.helloOK
		fs.helloOK = func(h *wire.Hello) wire.HelloOK {
			ok := inner(h)
			ok.SourceNodeID = tc.id
			return ok
		}
		b := consumerOf(t, fs)
		eventually(t, 5*time.Second, "refusal", func() bool {
			g, ok := fs.lastGoAway()
			return ok && g.Code == tc.code
		})
		eventually(t, 2*time.Second, "lastError", func() bool { return sourceStatus(b, "node-a").LastError != "" })
	}
}

func TestReceiverSourceResetAndGap(t *testing.T) {
	fs := newFakeSource(t)
	var epoch atomic.Uint64
	epoch.Store(fakeEpoch)
	inner := fs.helloOK
	fs.helloOK = func(h *wire.Hello) wire.HelloOK {
		ok := inner(h)
		ok.Epoch = epoch.Load()
		ok.Leo = 10
		return ok
	}
	fs.push(func(f *wire.Fetch) []byte {
		fr := batchFrame(f.FetchID, f.Offset+3, wire.BatchFlagGap, 1, rec("g/x", "1", nil))
		var bt wire.Batch
		_ = bt.Decode(fr[wire.FrameHeaderLen:])
		bt.Header.Lost = 3
		bt.Header.Leo = 10
		bt.Header.CRC32C = 0
		bt.Header.Flags &^= wire.BatchFlagCRC
		return bt.AppendFrame(nil)
	})
	fs.emptyLeo.Store(10)
	b := consumerOf(t, fs)
	eventually(t, 5*time.Second, "gap applied", func() bool { return fs.commit.Load() == 5 })
	if ss := sourceStatus(b, "node-a"); ss.GapLostTotal != 3 {
		t.Fatalf("gapLostTotal %d", ss.GapLostTotal)
	}
	// The source restarts with a new epoch: the consumer counts the reset.
	epoch.Store(fakeEpoch + 1)
	fs.push(func(f *wire.Fetch) []byte {
		return (&wire.GoAway{Code: wire.GoAwayShutdown}).AppendFrame(nil)
	})
	eventually(t, 5*time.Second, "source reset", func() bool { return sourceStatus(b, "node-a").SourceResets == 1 })
	if ss := sourceStatus(b, "node-a"); ss.ResetLostLowerBound != 5 || ss.Epoch != fakeEpoch+1 {
		t.Fatalf("reset lower bound %d epoch %d", ss.ResetLostLowerBound, ss.Epoch)
	}
}
