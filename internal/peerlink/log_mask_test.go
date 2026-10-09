package peerlink

import (
	"errors"
	"testing"
)

func logTestAppendMask(t testing.TB, l *Log, mask uint64) uint64 {
	t.Helper()
	next := l.LEO()
	off, ok := l.AppendMask(logTestFrame(32, next, 0), LogKindClient, mask)
	if !ok || off != next {
		t.Fatalf("AppendMask = %d,%v, want %d,true", off, ok, next)
	}
	return off
}

func TestLogMaskedTooManyConsumers(t *testing.T) {
	names := make([]string, 65)
	for i := range names {
		names[i] = string(rune('a'+i%26)) + string(rune('0'+i/26))
	}
	if _, err := NewLog(LogConfig{Masked: true, Consumers: names}); !errors.Is(err, ErrLogTooManyConsumers) {
		t.Fatalf("NewLog 65 masked consumers: %v", err)
	}
	l := logTestNew(t, LogConfig{Masked: true, Consumers: names[:64]})
	if l.AllConsumers() != ^uint64(0) {
		t.Fatalf("AllConsumers = %x", l.AllConsumers())
	}
}

func TestLogMaskedChunkAccounting(t *testing.T) {
	l := logTestNew(t, LogConfig{Masked: true, Consumers: []string{"a"}})
	logTestAppendMask(t, l, 1)
	if got := l.Stats().Bytes; got != logChunkBytes+logMasksBytes+32 {
		t.Fatalf("bytes = %d, want %d", got, logChunkBytes+logMasksBytes+32)
	}
}

func TestLogSparseRead(t *testing.T) {
	// Consumer 1 pins the log so nothing is trimmed.
	l := logTestNew(t, LogConfig{Masked: true, Consumers: []string{"a", "pin"}})
	// offsets 1..10: consumer 0 wants 2, 5, 6, 9.
	want := map[uint64]bool{2: true, 5: true, 6: true, 9: true}
	for off := uint64(1); off <= 10; off++ {
		m := uint64(2)
		if want[off] {
			m |= 1
		}
		logTestAppendMask(t, l, m)
	}
	var out [][]byte
	var deltas []uint32
	res, err := l.ReadSparse(0, 1, 100, 0, &out, &deltas)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Sparse || res.Base != 1 || res.Span != 10 || res.Count != 4 {
		t.Fatalf("res = %+v", res)
	}
	for i, f := range out {
		a, _ := logTestMarkers(f)
		if a != res.Base+uint64(deltas[i]) || !want[a] {
			t.Fatalf("frame %d offset %d delta %d", i, a, deltas[i])
		}
	}
	// maxRecords bounds included records; the span ends before the next wanted one.
	res, _ = l.ReadSparse(0, 1, 2, 0, &out, &deltas)
	if res.Count != 2 || res.Span != 5 {
		t.Fatalf("maxRecords: %+v", res)
	}
	// A dense run stays dense.
	res, _ = l.ReadSparse(0, 5, 2, 0, &out, &deltas)
	if res.Sparse || res.Count != 2 || res.Span != 2 || len(deltas) != 0 {
		t.Fatalf("dense run: %+v deltas %v", res, deltas)
	}
	// Nothing wanted in the tail: an empty batch that still covers the span.
	res, _ = l.ReadSparse(0, 10, 100, 0, &out, &deltas)
	if res.Count != 0 || res.Span != 1 || !res.Sparse {
		t.Fatalf("empty tail: %+v", res)
	}
	// Consumer 1 reads everything dense.
	res, _ = l.ReadSparse(1, 1, 100, 0, &out, &deltas)
	if res.Sparse || res.Count != 10 {
		t.Fatalf("consumer 1: %+v", res)
	}
}

func TestLogSparseReadMaxScan(t *testing.T) {
	l := logTestNew(t, LogConfig{Masked: true, MaxScan: 1024, Consumers: []string{"a", "pin"}})
	for range 3000 {
		logTestAppendMask(t, l, 2)
	}
	var out [][]byte
	var deltas []uint32
	res, _ := l.ReadSparse(0, 1, 100, 0, &out, &deltas)
	if res.Count != 0 || res.Span != 1024 {
		t.Fatalf("res = %+v", res)
	}
}

func TestLogSparseReadByteLimit(t *testing.T) {
	l := logTestNew(t, LogConfig{Masked: true, Consumers: []string{"a", "pin"}})
	logTestAppendMask(t, l, 3) // 1
	logTestAppendMask(t, l, 2) // 2 skipped
	logTestAppendMask(t, l, 3) // 3
	var out [][]byte
	var deltas []uint32
	// Room for one 32-byte record only: the skip would need table bytes, so the batch stays dense.
	res, _ := l.ReadSparse(0, 1, 100, 33, &out, &deltas)
	if res.Sparse || res.Count != 1 || res.Span != 1 {
		t.Fatalf("res = %+v", res)
	}
	// Two records plus table (4 + 4*2) fit in 76.
	res, _ = l.ReadSparse(0, 1, 100, 76, &out, &deltas)
	if !res.Sparse || res.Count != 2 || res.Span != 3 {
		t.Fatalf("res = %+v", res)
	}
	res, _ = l.ReadSparse(0, 1, 100, 75, &out, &deltas)
	if res.Count != 1 {
		t.Fatalf("res = %+v", res)
	}
}

func TestLogMaskedCaughtUpAdvance(t *testing.T) {
	l := logTestNew(t, LogConfig{Masked: true, Consumers: []string{"a", "b"}})
	// Nobody wants these: both caught-up consumers skip them and the log trims.
	for range 5 {
		logTestAppendMask(t, l, 0)
	}
	if c0, c1 := l.Committed(0), l.Committed(1); c0 != 6 || c1 != 6 {
		t.Fatalf("committed %d,%d, want 6,6", c0, c1)
	}
	if lso, _ := l.Bounds(); lso != 6 {
		t.Fatalf("lso %d, want 6", lso)
	}
	// Only a wants 6; b moves past it.
	logTestAppendMask(t, l, 1)
	if c0, c1 := l.Committed(0), l.Committed(1); c0 != 6 || c1 != 7 {
		t.Fatalf("committed %d,%d, want 6,7", c0, c1)
	}
}

func TestLogMaskedLaggingAdvance(t *testing.T) {
	l := logTestNew(t, LogConfig{Masked: true, Consumers: []string{"a", "b"}})
	logTestAppendMask(t, l, 3) // 1 both
	logTestAppendMask(t, l, 2) // 2 b only
	logTestAppendMask(t, l, 2) // 3 b only
	logTestAppendMask(t, l, 3) // 4 both
	if err := l.Commit(0, 2); err != nil {
		t.Fatal(err)
	}
	if c := l.Committed(0); c != 4 {
		t.Fatalf("committed after commit = %d, want 4", c)
	}
	logTestAppendMask(t, l, 2) // 5 b only, a not caught up
	if err := l.Commit(0, 5); err != nil {
		t.Fatal(err)
	}
	if c := l.Committed(0); c != 6 {
		t.Fatalf("committed = %d, want 6", c)
	}
	// AdvanceSkipped moves b nowhere (b wants 1).
	l.AdvanceSkipped()
	if c := l.Committed(1); c != 1 {
		t.Fatalf("b committed = %d, want 1", c)
	}
}

func TestLogMaskedLossOnlyWanted(t *testing.T) {
	// Consumer 1 pins; consumer 0 wants every other record and never reads.
	l := logTestNew(t, LogConfig{Masked: true, MaxMessages: 100, Consumers: []string{"a", "pin"}})
	for i := range 300 {
		m := uint64(2)
		if i%2 == 0 {
			m |= 1
		}
		logTestAppendMask(t, l, m)
	}
	lso, _ := l.Bounds()
	if got, want := l.ObserveConsumer(0).LostTotal, (lso-1+1)/2; got != want {
		t.Fatalf("lostTotal %d, want %d (lso %d)", got, want, lso)
	}
}

func TestLogClearConsumerBits(t *testing.T) {
	l := logTestNew(t, LogConfig{Masked: true, Consumers: []string{"a", "b"}})
	for range 10 {
		logTestAppendMask(t, l, 3)
	}
	if err := l.Commit(1, 11); err != nil {
		t.Fatal(err)
	}
	next, cleared := l.ClearConsumerBits(0, 1, func(f []byte) bool {
		a, _ := logTestMarkers(f)
		return a <= 7
	})
	if next != 11 || cleared != 7 {
		t.Fatalf("next %d cleared %d", next, cleared)
	}
	if c := l.Committed(0); c != 8 {
		t.Fatalf("committed %d, want 8", c)
	}
	if lso, _ := l.Bounds(); lso != 8 {
		t.Fatalf("lso %d, want 8", lso)
	}
}

func TestLogUnmaskedReadSparseIsDense(t *testing.T) {
	l := logTestNew(t, LogConfig{Consumers: []string{"a"}})
	logTestAppendSeq(t, l, 5, 32)
	var out [][]byte
	var deltas []uint32
	res, err := l.ReadSparse(0, 1, 100, 0, &out, &deltas)
	if err != nil || res.Sparse || res.Count != 5 || res.Span != 5 {
		t.Fatalf("res %+v err %v", res, err)
	}
}
