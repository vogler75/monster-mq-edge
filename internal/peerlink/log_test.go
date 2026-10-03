package peerlink

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"runtime"
	"runtime/metrics"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// logTestFrame builds a frame of size bytes (size >= 20) carrying two markers after the recLen prefix.
func logTestFrame(size int, a, b uint64) []byte {
	f := make([]byte, size)
	binary.LittleEndian.PutUint32(f, uint32(size-4))
	binary.LittleEndian.PutUint64(f[4:], a)
	binary.LittleEndian.PutUint64(f[12:], b)
	return f
}

func logTestMarkers(f []byte) (uint64, uint64) {
	return binary.LittleEndian.Uint64(f[4:]), binary.LittleEndian.Uint64(f[12:])
}

func logTestNew(t testing.TB, cfg LogConfig) *Log {
	t.Helper()
	l, err := NewLog(cfg)
	if err != nil {
		t.Fatalf("NewLog: %v", err)
	}
	t.Cleanup(l.Close)
	return l
}

// logTestAppendSeq appends n frames of size bytes whose first marker is their expected offset.
func logTestAppendSeq(t testing.TB, l *Log, n, size int) {
	t.Helper()
	for range n {
		next := l.LEO()
		off, ok := l.Append(logTestFrame(size, next, 0), LogKindClient)
		if !ok || off != next {
			t.Fatalf("Append = %d,%v, want %d,true", off, ok, next)
		}
	}
}

func logTestCheckSeq(t testing.TB, res LogReadResult, out [][]byte) {
	t.Helper()
	for i, f := range out {
		if a, _ := logTestMarkers(f); a != res.Base+uint64(i) {
			t.Fatalf("frame %d carries offset %d, want %d", i, a, res.Base+uint64(i))
		}
	}
}

func TestLogEpochAndDefaults(t *testing.T) {
	a := logTestNew(t, LogConfig{Consumers: []string{"b"}})
	b := logTestNew(t, LogConfig{Consumers: []string{"b"}})
	if a.Epoch() == 0 || b.Epoch() == 0 || a.Epoch() == b.Epoch() {
		t.Fatalf("epochs %d, %d: want non-zero and distinct", a.Epoch(), b.Epoch())
	}
	if lso, leo := a.Bounds(); lso != 1 || leo != 1 {
		t.Fatalf("bounds %d,%d, want 1,1", lso, leo)
	}
	s := a.Stats()
	if s.MaxMessages != 2_000_000 || s.MaxBytes != 256<<20 || a.MaxRecordBytes() != 1<<20+64<<10 {
		t.Fatalf("defaults %d/%d/%d", s.MaxMessages, s.MaxBytes, a.MaxRecordBytes())
	}
	if c := a.Committed(0); c != 1 {
		t.Fatalf("C[c] at epoch start = %d, want 1", c)
	}
	if a.ConsumerIndex("b") != 0 || a.ConsumerIndex("x") != -1 || a.NumConsumers() != 1 {
		t.Fatal("ConsumerIndex")
	}
	if ms := a.MonoMs(a.StartMono().Add(1500 * time.Millisecond)); ms != 1500 {
		t.Fatalf("MonoMs = %d", ms)
	}
}

func TestLogFrameAccounted(t *testing.T) {
	for _, c := range []struct {
		n    int
		want uint64
	}{{4, 8}, {8, 8}, {9, 16}, {200, 208}, {208, 208}, {209, 224}, {1000, 1024}, {32768, 32768}, {32769, 40960}, {1 << 20, 1 << 20}} {
		if got := logFrameAccounted(c.n); got != c.want {
			t.Errorf("logFrameAccounted(%d) = %d, want %d", c.n, got, c.want)
		}
	}
	for n := 1; n <= 40000; n++ {
		i, _ := slices.BinarySearch(logSizeClasses[:], uint16(min(n, 32768)))
		want := uint64(logSizeClasses[i])
		if n > 32768 {
			want = (uint64(n) + 8191) &^ 8191
		}
		if got := logFrameAccounted(n); got != want {
			t.Fatalf("logFrameAccounted(%d) = %d, want %d", n, got, want)
		}
	}
	if logChunkBytes != 16384 {
		t.Errorf("logChunkBytes = %d, want 16384 (one size class, no waste)", logChunkBytes)
	}
}

func TestLogFrameValidationAndSize(t *testing.T) {
	l := logTestNew(t, LogConfig{MaxBytes: 1 << 20, MaxRecordBytes: 1 << 20, Consumers: []string{"b"}})
	if l.MaxRecordBytes() != 1<<18 {
		t.Fatalf("MaxRecordBytes = %d, want MaxBytes/4", l.MaxRecordBytes())
	}
	bad := logTestFrame(100, 0, 0)
	binary.LittleEndian.PutUint32(bad, 50)
	for _, f := range [][]byte{nil, {1, 0}, bad} {
		if _, ok := l.Append(f, LogKindClient); ok {
			t.Fatalf("malformed frame %v accepted", f)
		}
	}
	if _, ok := l.Append(logTestFrame(1<<18+1, 0, 0), LogKindClient); ok {
		t.Fatal("oversize frame accepted")
	}
	if l.CheckRecordSize(1<<18) != true || l.CheckRecordSize(1<<18+1) != false {
		t.Fatal("CheckRecordSize")
	}
	l.CountCaptureInvalid()
	s := l.Stats()
	if s.CaptureDroppedInvalid != 4 || s.CaptureDroppedSize != 2 || s.LEO != 1 {
		t.Fatalf("invalid %d size %d leo %d", s.CaptureDroppedInvalid, s.CaptureDroppedSize, s.LEO)
	}
	if _, ok := l.Append(logTestFrame(1<<18, 0, 0), LogKindClient); !ok {
		t.Fatal("frame at the cap rejected")
	}
}

func TestLogAppendKinds(t *testing.T) {
	l := logTestNew(t, LogConfig{Consumers: []string{"b"}})
	for _, k := range []LogKind{LogKindClient, LogKindClient, LogKindInline, LogKindWill, LogKind(9)} {
		if _, ok := l.Append(logTestFrame(64, 0, 0), k); !ok {
			t.Fatal("Append failed")
		}
	}
	s := l.Stats()
	if s.AppendedClient != 3 || s.AppendedInline != 1 || s.AppendedWill != 1 {
		t.Fatalf("appended %d/%d/%d", s.AppendedClient, s.AppendedInline, s.AppendedWill)
	}
}

func TestLogAppendReadCommitTrim(t *testing.T) {
	l := logTestNew(t, LogConfig{Consumers: []string{"b", "c"}})
	logTestAppendSeq(t, l, 3000, 200)
	s := l.Stats()
	if s.LSO != 1 || s.LEO != 3001 || s.Records != 3000 || s.Chunks != 3 {
		t.Fatalf("stats %+v", s)
	}
	if want := uint64(3000*208 + 3*16384); s.Bytes != want || s.AppendedBytes != 3000*208 {
		t.Fatalf("bytes %d appended %d, want %d", s.Bytes, s.AppendedBytes, want)
	}

	var out [][]byte
	res, err := l.Read(1, 4096, 0, &out)
	if err != nil || res.Base != 1 || res.Count != 3000 || res.Bytes != 600000 || res.Lost != 0 || res.Truncated {
		t.Fatalf("Read all = %+v, %v", res, err)
	}
	logTestCheckSeq(t, res, out)

	res, _ = l.Read(1000, 10, 0, &out)
	if res.Base != 1000 || res.Count != 10 || len(out) != 10 {
		t.Fatalf("Read(1000,10) = %+v", res)
	}
	logTestCheckSeq(t, res, out)

	if res, _ = l.Read(1, 100, 1000, &out); res.Count != 5 || res.Bytes != 1000 {
		t.Fatalf("maxBytes 1000: %+v", res)
	}
	if res, _ = l.Read(1, 100, 100, &out); res.Count != 1 {
		t.Fatalf("a record above maxBytes must still be returned: %+v", res)
	}
	if res, _ = l.Read(5, 0, 0, &out); res.Count != 0 || res.Base != 5 {
		t.Fatalf("maxRecords 0: %+v", res)
	}
	if res, err = l.Read(3001, 10, 0, &out); err != nil || res.Count != 0 || res.LEO != 3001 {
		t.Fatalf("Read at leo = %+v, %v", res, err)
	}
	for _, from := range []uint64{0, 3002} {
		if _, err := l.Read(from, 10, 0, &out); !errors.Is(err, ErrLogOffsetOutOfRange) {
			t.Fatalf("Read(%d) err = %v", from, err)
		}
	}

	// The laggard pins the log.
	if err := l.Commit(0, 2000); err != nil {
		t.Fatal(err)
	}
	if lso, _ := l.Bounds(); lso != 1 {
		t.Fatalf("lso = %d after one consumer committed", lso)
	}
	if err := l.Commit(1, 1500); err != nil {
		t.Fatal(err)
	}
	s = l.Stats()
	if s.LSO != 1500 || s.LWM != 1500 || s.Trimmed != 1499 || s.Chunks != 2 {
		t.Fatalf("after trim to 1500: %+v", s)
	}
	if want := uint64(1501*208 + 2*16384); s.Bytes != want {
		t.Fatalf("bytes %d, want %d", s.Bytes, want)
	}
	// Chunk 0 now starts at 1025: slots 1025..1499 were cleared after the unlock, 1500 is held.
	for i := range 1500 - 1025 {
		if atomic.LoadPointer(&l.chunks[0].slots[i]) != nil {
			t.Fatalf("trimmed slot %d not cleared", 1025+i)
		}
	}
	if atomic.LoadPointer(&l.chunks[0].slots[1500-1025]) == nil {
		t.Fatal("slot 1500 cleared")
	}

	res, _ = l.Read(1000, 10, 0, &out)
	if res.Base != 1500 || res.Lost != 500 || res.Count != 10 {
		t.Fatalf("Read below lso = %+v", res)
	}
	logTestCheckSeq(t, res, out)

	if err := l.Commit(1, 1400); err != nil || l.Committed(1) != 1500 {
		t.Fatalf("lower commit: %v, C=%d", err, l.Committed(1))
	}
	if err := l.Commit(0, 3002); !errors.Is(err, ErrLogCommitBeyondEnd) {
		t.Fatalf("commit beyond leo: %v", err)
	}
	if err := l.Commit(2, 10); !errors.Is(err, ErrLogUnknownConsumer) {
		t.Fatalf("unknown consumer: %v", err)
	}

	_ = l.Commit(0, 3001)
	_ = l.Commit(1, 3001)
	s = l.Stats()
	if s.LSO != 3001 || s.Records != 0 || s.Trimmed != 3000 || s.Chunks != 1 || s.Bytes != 16384 {
		t.Fatalf("after full trim: %+v", s)
	}
	if s.EvictedByCount+s.EvictedByBytes+s.EvictedUnread != 0 {
		t.Fatalf("unexpected eviction: %+v", s)
	}

	logTestAppendSeq(t, l, 100, 200)
	res, _ = l.Read(3001, 1000, 0, &out)
	if res.Count != 100 {
		t.Fatalf("after refill: %+v", res)
	}
	logTestCheckSeq(t, res, out)
}

func TestLogTrimToChunkBoundary(t *testing.T) {
	l := logTestNew(t, LogConfig{Consumers: []string{"b"}})
	logTestAppendSeq(t, l, 2048, 64)
	_ = l.Commit(0, 2049)
	s := l.Stats()
	if s.Chunks != 0 || s.Bytes != 0 || s.Records != 0 {
		t.Fatalf("after trimming two full chunks: %+v", s)
	}
	logTestAppendSeq(t, l, 3, 64)
	var out [][]byte
	res, _ := l.Read(2049, 10, 0, &out)
	if res.Count != 3 || l.Stats().Chunks != 1 {
		t.Fatalf("reattach: %+v", res)
	}
	logTestCheckSeq(t, res, out)
}

func TestLogNoConsumers(t *testing.T) {
	l := logTestNew(t, LogConfig{})
	for range 5 {
		if _, ok := l.Append(logTestFrame(64, 0, 0), LogKindInline); !ok {
			t.Fatal("Append failed")
		}
	}
	s := l.Stats()
	if s.LSO != 6 || s.LEO != 6 || s.Bytes != 0 || s.Trimmed != 5 || s.AppendedInline != 5 {
		t.Fatalf("stats %+v", s)
	}
}

// PL-11 shape: a consumer that never connected, 2500 records into a 1000-record log.
func TestLogEvictionByCount(t *testing.T) {
	l := logTestNew(t, LogConfig{MaxMessages: 1000, Consumers: []string{"b"}})
	logTestAppendSeq(t, l, 2500, 100)
	s := l.Stats()
	if s.LSO != 1501 || s.Records != 1000 || s.EvictedByCount != 1500 || s.EvictedByBytes != 0 || s.EvictedUnread != 1500 {
		t.Fatalf("stats %+v", s)
	}
	if want := uint64(1000*112 + 2*16384); s.Bytes != want { // 1501..2500 spans chunks [1025,2049) and [2049,3073)
		t.Fatalf("bytes %d, want %d", s.Bytes, want)
	}
	if st := l.ObserveConsumer(0); st.LostTotal != 1500 || st.Lag != 2500 || st.State != LogConsumerNeverConnected {
		t.Fatalf("consumer %+v", st)
	}
	if st := l.ObserveConsumer(0); st.LostTotal != 1500 {
		t.Fatalf("second observation counted again: %d", st.LostTotal)
	}

	var out [][]byte
	res, _ := l.Read(1, 500, 0, &out)
	if res.Base != 1501 || res.Lost != 1500 || res.Count != 500 {
		t.Fatalf("GAP read %+v", res)
	}
	logTestCheckSeq(t, res, out)
	l.MarkServed(0, res.Base+uint64(res.Count))
	_ = l.Commit(0, 2001)

	logTestAppendSeq(t, l, 1000, 100) // evicts 1501..2500: 1501..2000 are trimmed already, so 2001..2500
	s = l.Stats()
	if s.LSO != 2501 || s.EvictedByCount != 2000 || s.EvictedUnread != 2000 {
		t.Fatalf("stats %+v", s)
	}
	if st := l.ObserveConsumer(0); st.LostTotal != 2000 || st.Served != 2001 || st.Committed != 2001 {
		t.Fatalf("consumer %+v", st)
	}
}

func TestLogEvictionByBytes(t *testing.T) {
	const maxBytes = 1 << 20
	l := logTestNew(t, LogConfig{MaxBytes: maxBytes, Consumers: []string{"on", "off"}})
	l.SetConsumerState(0, LogConsumerConnected)
	l.SetConsumerState(1, LogConsumerDisconnected)
	var out [][]byte
	next := uint64(1)
	for i := range 5000 {
		logTestAppendSeq(t, l, 1, 1000)
		if s := l.Stats(); s.Bytes > maxBytes {
			t.Fatalf("append %d: bytes %d above MaxBytes", i, s.Bytes)
		}
		// The connected consumer is served everything at once and commits with a lag of 100.
		res, err := l.ReadFor(0, next, 4096, 0, &out)
		if err != nil {
			t.Fatal(err)
		}
		logTestCheckSeq(t, res, out)
		next = res.Base + uint64(res.Count)
		if next > 100 {
			_ = l.Commit(0, next-100)
		}
	}
	s := l.Stats()
	if s.EvictedByBytes == 0 || s.EvictedByCount != 0 {
		t.Fatalf("stats %+v", s)
	}
	if want := s.Records*1024 + uint64(s.Chunks)*16384; s.Bytes != want {
		t.Fatalf("bytes %d, want %d", s.Bytes, want)
	}
	// "off" pins lwm at 1, so every eviction is unread.
	if s.EvictedUnread != s.EvictedByBytes || s.Trimmed != 0 {
		t.Fatalf("stats %+v", s)
	}
	cs := l.ConsumerStats()
	if cs[0].LostTotal != 0 || cs[0].State != LogConsumerConnected {
		t.Fatalf("connected consumer %+v", cs[0])
	}
	if cs[1].LostTotal != s.LSO-1 || cs[1].LostTotal != s.EvictedByBytes || cs[1].State != LogConsumerDisconnected {
		t.Fatalf("disconnected consumer %+v, lso %d", cs[1], s.LSO)
	}
}

// Table-driven source-side loss formula (8.5, 22.3).
func TestLogLossFormula(t *testing.T) {
	type step struct {
		appendTo  uint64 // append until leo == appendTo (lso follows from MaxMessages = 100)
		commit    uint64 // consumer 0 commit (0 = none)
		served    uint64 // consumer 0 served mark (0 = none)
		wantLost  uint64 // consumer 0 lostTotal after observing
		wantLSO   uint64
		noObserve bool
	}
	cases := []struct {
		name  string
		steps []step
	}{
		{"never connected", []step{{appendTo: 251, wantLSO: 151, wantLost: 150}}},
		{"served ahead of commit", []step{
			{appendTo: 60, commit: 10, served: 50, wantLSO: 1},
			{appendTo: 201, wantLSO: 101, wantLost: 51}, // 50..100 were never served
		}},
		{"served beyond lso", []step{
			{appendTo: 160, commit: 10, served: 150, wantLSO: 60},
			{appendTo: 201, wantLSO: 101, wantLost: 0},
		}},
		{"committed beyond lso", []step{
			{appendTo: 130, commit: 120, served: 120, wantLSO: 30},
			{appendTo: 201, wantLSO: 101, wantLost: 0},
		}},
		{"observed twice", []step{
			{appendTo: 150, wantLSO: 50, wantLost: 49},
			{appendTo: 201, wantLSO: 101, wantLost: 100},
			{appendTo: 201, wantLSO: 101, wantLost: 100},
		}},
		{"observed late", []step{
			{appendTo: 150, wantLSO: 50, noObserve: true},
			{appendTo: 300, wantLSO: 200, wantLost: 199},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Consumer 1 never commits, so lso moves only by eviction.
			l := logTestNew(t, LogConfig{MaxMessages: 100, Consumers: []string{"c", "pin"}})
			for i, st := range tc.steps {
				logTestAppendSeq(t, l, int(st.appendTo-l.LEO()), 64)
				if st.commit != 0 {
					if err := l.Commit(0, st.commit); err != nil {
						t.Fatal(err)
					}
				}
				if st.served != 0 {
					l.MarkServed(0, st.served)
				}
				if lso, _ := l.Bounds(); lso != st.wantLSO {
					t.Fatalf("step %d: lso %d, want %d", i, lso, st.wantLSO)
				}
				if st.noObserve {
					continue
				}
				if got := l.ObserveConsumer(0).LostTotal; got != st.wantLost {
					t.Fatalf("step %d: lostTotal %d, want %d", i, got, st.wantLost)
				}
			}
		})
	}
}

func TestLogTruncatedRead(t *testing.T) {
	l := logTestNew(t, LogConfig{Consumers: []string{"b"}})
	logTestAppendSeq(t, l, 10, 64)
	// Simulate an eviction between the reader's snapshot and its slot loads.
	atomic.StorePointer(&l.chunks[0].slots[5], nil)
	var out [][]byte
	res, err := l.Read(1, 10, 0, &out)
	if err != nil || res.Count != 5 || !res.Truncated {
		t.Fatalf("Read = %+v, %v", res, err)
	}
	logTestCheckSeq(t, res, out)
	res, err = l.Read(6, 10, 0, &out)
	if err != nil || res.Count != 0 || !res.Truncated || len(out) != 0 {
		t.Fatalf("Read at the cleared slot = %+v, %v", res, err)
	}
}

// Readers race eviction: every frame they get carries its own offset, a nil slot truncates the
// batch, and the following read reports the evicted range as a GAP.
func TestLogReadersRaceEviction(t *testing.T) {
	l := logTestNew(t, LogConfig{MaxMessages: 1500, Consumers: []string{"b"}})
	const total = 200_000
	var done atomic.Bool
	var wg sync.WaitGroup
	wg.Go(func() {
		defer done.Store(true)
		for i := range total {
			if _, ok := l.Append(logTestFrame(48, uint64(i+1), 0), LogKindClient); !ok {
				t.Error("Append failed")
				return
			}
		}
	})
	var truncations, gaps atomic.Int64
	for range 3 {
		wg.Go(func() {
			var out [][]byte
			from := uint64(1)
			afterTrunc := false
			for !done.Load() || from < l.LEO() {
				res, err := l.Read(from, 700, 0, &out)
				if err != nil {
					t.Errorf("Read(%d): %v", from, err)
					return
				}
				if res.Base != from+res.Lost || res.Base < res.LSO {
					t.Errorf("Read(%d) = %+v", from, res)
					return
				}
				if afterTrunc && res.Lost == 0 {
					t.Errorf("read after truncation at %d reported no gap: %+v", from, res)
					return
				}
				for i, f := range out {
					if a, _ := logTestMarkers(f); a != res.Base+uint64(i) {
						t.Errorf("frame at %d carries %d", res.Base+uint64(i), a)
						return
					}
				}
				if res.Lost > 0 {
					gaps.Add(1)
				}
				afterTrunc = res.Truncated
				if res.Truncated {
					truncations.Add(1)
				}
				from = res.Base + uint64(res.Count)
				clear(out)
				if res.Count == 0 && !res.Truncated {
					runtime.Gosched()
				}
			}
		})
	}
	wg.Wait()
	s := l.Stats()
	if s.LEO != total+1 || s.Records != 1500 || s.EvictedByCount != total-1500 {
		t.Fatalf("stats %+v", s)
	}
	t.Logf("gaps %d, truncations %d", gaps.Load(), truncations.Load())
}

func TestLogWaiters(t *testing.T) {
	l := logTestNew(t, LogConfig{Consumers: []string{"b"}})
	w := NewLogWaiter()
	recv := func() bool {
		select {
		case <-w.C():
			return true
		case <-time.After(20 * time.Millisecond):
			return false
		}
	}

	if !l.Wait(w, 2) {
		t.Fatal("Wait(2) with leo 1 did not register")
	}
	logTestAppendSeq(t, l, 1, 64)
	if !recv() || w.idx != -1 || len(l.waiters) != 0 || l.minWakeAt != ^uint64(0) {
		t.Fatal("waiter not woken and removed on append")
	}
	l.Unwait(w)

	// minRecords 3: wakeAt = offset + 3.
	if !l.Wait(w, l.LEO()+3) {
		t.Fatal("Wait did not register")
	}
	logTestAppendSeq(t, l, 2, 64)
	if recv() {
		t.Fatal("woken before minRecords were available")
	}
	logTestAppendSeq(t, l, 1, 64)
	if !recv() {
		t.Fatal("not woken at minRecords")
	}
	l.Unwait(w)

	if l.Wait(w, l.LEO()) {
		t.Fatal("Wait registered although leo reached wakeAt")
	}

	// A token sent while the waiter gave up must not survive Unwait.
	l.Wait(w, l.LEO()+1)
	logTestAppendSeq(t, l, 1, 64)
	l.Unwait(w)
	if len(w.notify) != 0 {
		t.Fatal("stale token after Unwait")
	}

	// Two waiters with different wake points.
	w2 := NewLogWaiter()
	l.Wait(w, l.LEO()+1)
	l.Wait(w2, l.LEO()+2)
	logTestAppendSeq(t, l, 1, 64)
	if !recv() || len(w2.notify) != 0 || l.minWakeAt != w2.wakeAt {
		t.Fatal("first waiter not woken alone")
	}
	logTestAppendSeq(t, l, 1, 64)
	select {
	case <-w2.C():
	case <-time.After(time.Second):
		t.Fatal("second waiter not woken")
	}
	l.Unwait(w)
	l.Unwait(w2)

	ctx := context.Background()
	start := time.Now()
	if l.WaitFor(ctx, w, l.LEO()+1, 30*time.Millisecond) {
		t.Fatal("WaitFor reported records after a timeout")
	}
	if d := time.Since(start); d < 25*time.Millisecond {
		t.Fatalf("WaitFor returned after %v", d)
	}

	go func() {
		time.Sleep(10 * time.Millisecond)
		l.Append(logTestFrame(64, 0, 0), LogKindClient)
	}()
	if !l.WaitFor(ctx, w, l.LEO()+1, 5*time.Second) {
		t.Fatal("WaitFor not woken by an append")
	}

	cctx, cancel := context.WithCancel(ctx)
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	start = time.Now()
	if l.WaitFor(cctx, w, l.LEO()+1, 5*time.Second) || time.Since(start) > 2*time.Second {
		t.Fatal("WaitFor ignored ctx")
	}

	// Close wakes registered waiters; later Waits return at once.
	l.Wait(w, l.LEO()+10)
	l.Close()
	if !recv() {
		t.Fatal("Close did not wake the waiter")
	}
	if l.Wait(w, l.LEO()+10) {
		t.Fatal("Wait registered on a closed log")
	}
}

func TestLogAppendAllocations(t *testing.T) {
	l := logTestNew(t, LogConfig{Consumers: []string{"b"}})
	frames := make([][]byte, 200)
	for i := range frames {
		frames[i] = logTestFrame(200, 0, 0)
	}
	w := NewLogWaiter()
	l.Wait(w, 1_000_000) // never due, so every append scans the waiters
	l.Append(frames[0], LogKindClient)
	i := 1
	allocs := testing.AllocsPerRun(100, func() {
		l.Append(frames[i], LogKindClient)
		i++
	})
	if allocs != 0 {
		t.Fatalf("Append allocates %.1f per call", allocs)
	}
	w2 := NewLogWaiter()
	allocs = testing.AllocsPerRun(50, func() {
		l.Wait(w2, l.LEO()+1)
		l.Append(frames[i], LogKindClient)
		<-w2.C()
		l.Unwait(w2)
		i++
	})
	if allocs != 0 {
		t.Fatalf("wake cycle allocates %.1f per call", allocs)
	}
}

func TestLogSeal(t *testing.T) {
	l := logTestNew(t, LogConfig{Consumers: []string{"b"}})
	logTestAppendSeq(t, l, 10, 64)
	if l.Sealed() {
		t.Fatal("sealed too early")
	}
	if leo := l.Seal(); leo != 11 {
		t.Fatalf("Seal = %d", leo)
	}
	if !l.Sealed() {
		t.Fatal("not sealed")
	}
	if _, ok := l.Append(logTestFrame(64, 0, 0), LogKindClient); ok {
		t.Fatal("Append after Seal accepted")
	}
	l.CountUncapturedAtShutdown()
	s := l.Stats()
	if s.LEO != 11 || s.UncapturedAtShutdown != 2 || !s.Sealed || s.AppendedClient != 10 {
		t.Fatalf("stats %+v", s)
	}
	// Records captured before the seal are still served and committed.
	var out [][]byte
	if res, _ := l.Read(1, 100, 0, &out); res.Count != 10 {
		t.Fatalf("read after seal %+v", res)
	}
	if err := l.Commit(0, 11); err != nil {
		t.Fatal(err)
	}
}

func TestLogDrain(t *testing.T) {
	ctx := context.Background()

	t.Run("connected consumer commits", func(t *testing.T) {
		l := logTestNew(t, LogConfig{Consumers: []string{"on", "off"}})
		l.SetConsumerState(0, LogConsumerConnected)
		l.SetConsumerState(1, LogConsumerDisconnected)
		logTestAppendSeq(t, l, 100, 64)
		go func() {
			time.Sleep(30 * time.Millisecond)
			// A late publish after drainTarget is still captured but not waited for.
			l.Append(logTestFrame(64, 0, 0), LogKindInline)
			_ = l.Commit(0, 101)
		}()
		res := l.Drain(ctx, 5*time.Second, nil)
		if !res.Complete || res.Target != 101 || res.FinalLEO != 102 || res.Waited > 4*time.Second {
			t.Fatalf("drain %+v", res)
		}
		if !slices.Equal(res.Unserved, []uint64{1, 101}) {
			t.Fatalf("unserved %v", res.Unserved)
		}
		if _, ok := l.Append(logTestFrame(64, 0, 0), LogKindClient); ok || l.Stats().UncapturedAtShutdown != 1 {
			t.Fatal("append after drain captured")
		}
		_ = l.Commit(0, 102)
		if cs := l.ConsumerStats(); cs[0].Lag != 0 || cs[1].Lag != 101 {
			t.Fatalf("shutdownUnserved %d/%d", cs[0].Lag, cs[1].Lag)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		l := logTestNew(t, LogConfig{Consumers: []string{"on"}})
		logTestAppendSeq(t, l, 5, 64)
		res := l.Drain(ctx, 60*time.Millisecond, func(int) bool { return true })
		if res.Complete || res.Waited < 50*time.Millisecond || res.Unserved[0] != 5 || !l.Sealed() {
			t.Fatalf("drain %+v", res)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		l := logTestNew(t, LogConfig{Consumers: []string{"on"}})
		l.SetConsumerState(0, LogConsumerConnected)
		logTestAppendSeq(t, l, 5, 64)
		res := l.Drain(ctx, 0, nil)
		if res.Complete || res.Waited > time.Second || res.FinalLEO != 6 || !l.Sealed() {
			t.Fatalf("drain %+v", res)
		}
	})

	t.Run("ctx", func(t *testing.T) {
		l := logTestNew(t, LogConfig{Consumers: []string{"on"}})
		l.SetConsumerState(0, LogConsumerConnected)
		logTestAppendSeq(t, l, 5, 64)
		cctx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
		defer cancel()
		if res := l.Drain(cctx, 10*time.Second, nil); res.Complete || res.Waited > 5*time.Second {
			t.Fatalf("drain %+v", res)
		}
	})

	t.Run("nothing to wait for", func(t *testing.T) {
		l := logTestNew(t, LogConfig{Consumers: []string{"off"}})
		logTestAppendSeq(t, l, 5, 64)
		if res := l.Drain(ctx, 10*time.Second, nil); !res.Complete || res.Waited > time.Second {
			t.Fatalf("drain %+v", res)
		}
	})
}

// Resume rules (9.6).
func TestLogResume(t *testing.T) {
	setup := func(t *testing.T) *Log {
		// MaxMessages 100, 250 records: lso 151, leo 251. Consumer 1 pins nothing in particular.
		l := logTestNew(t, LogConfig{MaxMessages: 100, Consumers: []string{"c", "other"}})
		logTestAppendSeq(t, l, 250, 64)
		return l
	}

	t.Run("same epoch in range", func(t *testing.T) {
		l := setup(t)
		r, err := l.Resume(0, l.Epoch(), 200)
		if err != nil || r.ResumeAt != 200 || !r.ConsumerStateUsed || r.SourceReset || r.LostOnResume != 0 || r.Committed != 200 {
			t.Fatalf("%+v %v", r, err)
		}
		if r, _ = l.Resume(0, l.Epoch(), 251); r.ResumeAt != 251 || !r.ConsumerStateUsed {
			t.Fatalf("resume at leo %+v", r)
		}
	})
	t.Run("same epoch below lso", func(t *testing.T) {
		l := setup(t)
		r, err := l.Resume(0, l.Epoch(), 120)
		if err != nil || r.ResumeAt != 151 || r.ConsumerStateUsed || r.LostOnResume != 31 || r.Committed != 120 {
			t.Fatalf("%+v %v", r, err)
		}
		// C[c] is raised to 120 before the observation, so the source counts the same 31 records.
		if st := l.ObserveConsumer(0); st.LostTotal != 31 {
			t.Fatalf("lostTotal %d", st.LostTotal)
		}
	})
	t.Run("same epoch beyond leo", func(t *testing.T) {
		l := setup(t)
		if _, err := l.Resume(0, l.Epoch(), 252); !errors.Is(err, ErrLogOffsetOutOfRange) {
			t.Fatal(err)
		}
	})
	t.Run("consumer restart", func(t *testing.T) {
		l := setup(t)
		_ = l.Commit(0, 180)
		r, err := l.Resume(0, 0, 0)
		if err != nil || r.ResumeAt != 180 || r.ConsumerStateUsed || r.SourceReset || r.LostOnResume != 0 {
			t.Fatalf("%+v %v", r, err)
		}
		l2 := setup(t)
		if r, _ = l2.Resume(0, 0, 0); r.ResumeAt != 151 || r.LostOnResume != 150 {
			t.Fatalf("restart below lso %+v", r)
		}
	})
	t.Run("source reset", func(t *testing.T) {
		l := setup(t)
		r, err := l.Resume(0, l.Epoch()+1, 999)
		if err != nil || !r.SourceReset || r.ResumeAt != 151 || r.LostOnResume != 150 || r.LEO != 251 {
			t.Fatalf("%+v %v", r, err)
		}
	})
	t.Run("resume trims", func(t *testing.T) {
		l := logTestNew(t, LogConfig{Consumers: []string{"c"}})
		logTestAppendSeq(t, l, 50, 64)
		r, _ := l.Resume(0, l.Epoch(), 40)
		if lso, _ := l.Bounds(); lso != 40 || r.LSO != 40 {
			t.Fatalf("lso %d, %+v", lso, r)
		}
	})
}

// Concurrent appenders, readers that commit, and status readers; run with -race.
func TestLogConcurrent(t *testing.T) {
	for _, tc := range []struct {
		name        string
		maxMessages uint64
	}{{"no eviction", 0}, {"eviction", 3000}} {
		t.Run(tc.name, func(t *testing.T) {
			const writers, perWriter = 16, 4000
			l := logTestNew(t, LogConfig{MaxMessages: tc.maxMessages, Consumers: []string{"b", "c"}})
			var wg, readers sync.WaitGroup
			for g := range writers {
				wg.Go(func() {
					kind := LogKind(g % 3)
					for i := range perWriter {
						if _, ok := l.Append(logTestFrame(40+g, uint64(g), uint64(i)), kind); !ok {
							t.Error("Append failed")
							return
						}
					}
				})
			}
			var done atomic.Bool
			var lost [2]uint64
			for c := range 2 {
				readers.Go(func() {
					var out [][]byte
					last := make([]int64, writers)
					for i := range last {
						last[i] = -1
					}
					from := uint64(1)
					for {
						finished := done.Load()
						res, err := l.ReadFor(c, from, 500, 64<<10, &out)
						if err != nil {
							t.Errorf("Read: %v", err)
							return
						}
						lost[c] += res.Lost
						for _, f := range out {
							g, i := logTestMarkers(f)
							if tc.maxMessages == 0 && int64(i) != last[g]+1 || int64(i) <= last[g] {
								t.Errorf("writer %d: record %d after %d", g, i, last[g])
								return
							}
							last[g] = int64(i)
						}
						from = res.Base + uint64(res.Count)
						clear(out)
						if err := l.Commit(c, from); err != nil {
							t.Errorf("Commit: %v", err)
							return
						}
						if finished && from == l.LEO() {
							return
						}
						if res.Count == 0 {
							runtime.Gosched()
						}
					}
				})
			}
			readers.Go(func() {
				for !done.Load() {
					l.Stats()
					l.ConsumerStats()
					runtime.Gosched()
				}
			})
			wg.Wait()
			done.Store(true)
			readers.Wait()

			s := l.Stats()
			if s.AppendedClient+s.AppendedInline+s.AppendedWill != writers*perWriter || s.LEO != writers*perWriter+1 {
				t.Fatalf("stats %+v", s)
			}
			if s.LSO != s.LEO || s.Records != 0 || s.Bytes > 16384 {
				t.Fatalf("not fully trimmed: %+v", s)
			}
			evicted := s.EvictedByCount + s.EvictedByBytes
			if s.Trimmed+evicted != writers*perWriter {
				t.Fatalf("trimmed %d + evicted %d != appended", s.Trimmed, evicted)
			}
			if tc.maxMessages == 0 && (evicted != 0 || lost != [2]uint64{}) {
				t.Fatalf("unexpected loss: evicted %d lost %v", evicted, lost)
			}
			// Without a crash the source count equals what each consumer saw as GAP.
			for c, st := range l.ConsumerStats() {
				if st.LostTotal != lost[c] {
					t.Fatalf("consumer %d: source lostTotal %d, consumer gap total %d", c, st.LostTotal, lost[c])
				}
			}
		})
	}
}

// logBusyCPUSeconds is the process CPU time spent on Go code, GC and scavenging, from runtime/metrics.
func logBusyCPUSeconds() float64 {
	s := []metrics.Sample{{Name: "/cpu/classes/total:cpu-seconds"}, {Name: "/cpu/classes/idle:cpu-seconds"}}
	metrics.Read(s)
	return s[0].Value.Float64() - s[1].Value.Float64()
}

// BenchmarkLogAppendParallel is gate G1 (21.2): g goroutines append 200-byte frames, each op
// allocating its own frame like the capture path. A consumer commits leo every millisecond.
// cpu-ns/op is busy CPU (all goroutines, GC included) per append.
func BenchmarkLogAppendParallel(b *testing.B) {
	for _, g := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("g=%d", g), func(b *testing.B) {
			benchLogAppend(b, g, LogConfig{Consumers: []string{"b"}}, true)
		})
	}
}

// BenchmarkLogAppendParallelEvicting is G1 with a full log: nobody commits, every append evicts.
func BenchmarkLogAppendParallelEvicting(b *testing.B) {
	for _, g := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("g=%d", g), func(b *testing.B) {
			benchLogAppend(b, g, LogConfig{MaxBytes: 64 << 20, Consumers: []string{"b"}}, false)
		})
	}
}

func benchLogAppend(b *testing.B, g int, cfg LogConfig, commit bool) {
	l := logTestNew(b, cfg)
	stop := make(chan struct{})
	var bg sync.WaitGroup
	if commit {
		bg.Go(func() {
			tk := time.NewTicker(time.Millisecond)
			defer tk.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tk.C:
					_ = l.Commit(0, l.LEO())
				}
			}
		})
	}
	b.ReportAllocs()
	b.SetBytes(200)
	runtime.GC()
	cpu0 := logBusyCPUSeconds()
	b.ResetTimer()
	var wg sync.WaitGroup
	for i := range g {
		n := b.N / g
		if i < b.N%g {
			n++
		}
		wg.Go(func() {
			for range n {
				f := make([]byte, 200)
				binary.LittleEndian.PutUint32(f, 196)
				l.Append(f, LogKindClient)
			}
		})
	}
	wg.Wait()
	b.StopTimer()
	cpu := logBusyCPUSeconds() - cpu0
	b.ReportMetric(cpu*1e9/float64(b.N), "cpu-ns/op")
	close(stop)
	bg.Wait()
	if s := l.Stats(); s.SpareMisses > 0 {
		b.ReportMetric(float64(s.SpareMisses), "spare-misses")
	}
}

// BenchmarkLogAppendWithReaders approximates G1b: 16 appenders, 2 consumers reading 4096-record
// batches and committing, continuous eviction (the slow consumer lags behind MaxBytes). It reports
// append latency percentiles at saturation and at a paced aggregate rate of 200k appends/s (10x the
// reference rate) with a full log that evicts on every append.
func BenchmarkLogAppendWithReaders(b *testing.B) {
	b.Run("saturated", func(b *testing.B) { benchLogAppendReaders(b, 0) })
	b.Run("rate=200k", func(b *testing.B) { benchLogAppendReaders(b, 200_000) })
}

func benchLogAppendReaders(b *testing.B, rate int) {
	const g = 16
	l := logTestNew(b, LogConfig{MaxBytes: 32 << 20, Consumers: []string{"fast", "slow"}})
	stop := make(chan struct{})
	var bg sync.WaitGroup
	for c, pause := range []time.Duration{0, 2 * time.Millisecond} {
		if rate > 0 && pause > 0 {
			continue // the slow consumer stays away: the log is full and evicts on every append
		}
		bg.Go(func() {
			var out [][]byte
			from := uint64(1)
			for {
				select {
				case <-stop:
					return
				default:
				}
				res, err := l.ReadFor(c, from, 4096, 1<<20, &out)
				if err != nil {
					return
				}
				from = res.Base + uint64(res.Count)
				clear(out)
				_ = l.Commit(c, from)
				if pause > 0 {
					time.Sleep(pause)
				} else if res.Count == 0 {
					runtime.Gosched()
				}
			}
		})
	}
	if rate > 0 {
		for range (32 << 20) / 208 {
			l.Append(logTestFrame(200, 0, 0), LogKindClient)
		}
	}
	misses0 := l.Stats().SpareMisses
	type result struct {
		hist  [logLatBuckets]uint64
		worst int64
		_     [64]byte
	}
	results := make([]result, g)
	b.ReportAllocs()
	b.ResetTimer()
	var wg sync.WaitGroup
	for i := range g {
		n := b.N / g
		if i < b.N%g {
			n++
		}
		wg.Go(func() {
			var hist [logLatBuckets]uint64
			var worst int64
			var interval time.Duration
			if rate > 0 {
				interval = time.Duration(int64(time.Second) * g / int64(rate))
			}
			next := time.Now()
			for range n {
				if interval > 0 {
					next = next.Add(interval)
					if d := time.Until(next); d > 0 {
						time.Sleep(d)
					}
				}
				f := make([]byte, 200)
				binary.LittleEndian.PutUint32(f, 196)
				t0 := time.Now()
				l.Append(f, LogKindClient)
				d := time.Since(t0).Nanoseconds()
				hist[logLatBucket(d)]++
				worst = max(worst, d)
			}
			results[i].hist, results[i].worst = hist, worst
		})
	}
	wg.Wait()
	b.StopTimer()
	close(stop)
	bg.Wait()
	var all [logLatBuckets]uint64
	var total uint64
	var worst int64
	for i := range results {
		for k, v := range results[i].hist {
			all[k] += v
			total += v
		}
		worst = max(worst, results[i].worst)
	}
	pct := func(p float64) float64 {
		want := uint64(p * float64(total))
		var acc uint64
		for k, v := range all {
			acc += v
			if acc >= want {
				return float64(logLatUpper(k))
			}
		}
		return 0
	}
	b.ReportMetric(pct(0.99), "p99-ns")
	b.ReportMetric(pct(0.999), "p99.9-ns")
	b.ReportMetric(float64(worst), "max-ns")
	s := l.Stats()
	b.ReportMetric(float64(s.EvictedByBytes), "evicted")
	b.ReportMetric(float64(s.SpareMisses-misses0), "spare-misses")
}

// Log-linear latency buckets: 8 sub-buckets per power of two, so a percentile is reported with at
// most 12.5 % overstatement.
const logLatBuckets = 512

func logLatBucket(d int64) int {
	if d < 8 {
		return int(max(d, 0))
	}
	e := bits.Len64(uint64(d)) - 1
	return min(logLatBuckets-1, (e-2)*8+int(uint64(d)>>(e-3)&7))
}

// logLatUpper is the largest latency in bucket k.
func logLatUpper(k int) int64 {
	if k < 8 {
		return int64(k)
	}
	e, sub := k/8+2, int64(k%8)
	return (8+sub+1)<<(e-3) - 1
}

// BenchmarkLogRead measures one 4096-record read of 200-byte frames.
func BenchmarkLogRead(b *testing.B) {
	l := logTestNew(b, LogConfig{Consumers: []string{"b"}})
	for range 8192 {
		l.Append(logTestFrame(200, 0, 0), LogKindClient)
	}
	out := make([][]byte, 0, 4096)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		res, _ := l.Read(1+uint64(i%4096), 4096, 1<<20, &out)
		if res.Count != 4096 {
			b.Fatalf("read %d", res.Count)
		}
		clear(out)
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/4096, "ns/record")
}

func TestLogLatBuckets(t *testing.T) {
	for _, d := range []int64{0, 1, 7, 8, 9, 15, 16, 17, 100, 1000, 1023, 1024, 1025, 123456, 1 << 30} {
		k := logLatBucket(d)
		if up := logLatUpper(k); up < d || float64(up) > float64(d)*1.125+1 {
			t.Errorf("logLatBucket(%d) = %d, upper %d", d, k, up)
		}
		if k > 0 && logLatUpper(k-1) >= d {
			t.Errorf("logLatBucket(%d) = %d, but bucket %d already covers it", d, k, k-1)
		}
	}
}
