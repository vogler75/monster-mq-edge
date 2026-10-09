package peerlink

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// The source log (plan 8): a chunked index of immutable, pre-encoded record frames with absolute offsets.
//
// A frame is one exact-size heap allocation whose first four bytes are its little-endian recLen
// (bytes after the prefix). A slot stores only the pointer to the frame's first byte; the length is
// read back from the prefix. Slots are written and read with atomic pointer operations, so readers
// copy frame pointers outside the lock. Chunks are never moved and never reused: a reader that
// snapshotted a chunk pointer keeps reading the offsets it was snapshotted for, or nil.

const (
	logChunkShift = 10
	logChunkSlots = 1 << logChunkShift
	logChunkMask  = logChunkSlots - 1

	// logChunkBytes is the accounted footprint of an attached chunk (slots + cum).
	logChunkBytes = uint64(unsafe.Sizeof(logChunk{}))

	// logReadMaxChunks bounds the chunk pointers one Read copies under the lock, so the snapshot
	// stays O(1) and needs no allocation. One Read returns at most (logReadMaxChunks-1)*1024+1 records.
	logReadMaxChunks = 16

	// logSpareChunks preallocated chunks absorb the refill goroutine's wake-up latency during bursts.
	logSpareChunks = 4

	logDefaultMaxMessages    = 2_000_000
	logDefaultMaxBytes       = 256 << 20
	logDefaultMaxRecordBytes = 1<<20 + 64<<10

	// logMasksBytes is the accounted footprint of a chunk's consumer mask array (interest routing only).
	logMasksBytes = uint64(unsafe.Sizeof(logMasks{}))

	logDefaultMaxScan = 65536
	logMinMaxScan     = 1024
)

var (
	// ErrLogOffsetOutOfRange is returned by Read for offset 0 or an offset above leo, and by Resume for a
	// same-epoch resume offset above leo (GOAWAY offset_out_of_range).
	ErrLogOffsetOutOfRange = errors.New("peerlink: log offset out of range")
	// ErrLogCommitBeyondEnd is returned by Commit for a commit above leo (GOAWAY protocol).
	ErrLogCommitBeyondEnd = errors.New("peerlink: commit beyond log end")
	// ErrLogUnknownConsumer is returned for a consumer index outside the static consumer set.
	ErrLogUnknownConsumer = errors.New("peerlink: unknown log consumer")
	// ErrLogTooManyConsumers is returned by NewLog for a masked log with more than 64 consumers.
	ErrLogTooManyConsumers = errors.New("peerlink: interest routing supports at most 64 consumers")
)

// LogKind classifies an appended record for the appended{client,inline,will} counters.
type LogKind uint8

const (
	LogKindClient LogKind = iota
	LogKindInline
	LogKindWill
	logKindCount
)

func (k LogKind) String() string {
	switch k {
	case LogKindClient:
		return "client"
	case LogKindInline:
		return "inline"
	case LogKindWill:
		return "will"
	}
	return "unknown"
}

// LogConsumerState is a consumer's connection state as seen by the source (plan 11.3).
type LogConsumerState uint8

const (
	LogConsumerNeverConnected LogConsumerState = iota
	LogConsumerConnected
	LogConsumerDisconnected
)

func (s LogConsumerState) String() string {
	switch s {
	case LogConsumerNeverConnected:
		return "NEVER_CONNECTED"
	case LogConsumerConnected:
		return "CONNECTED"
	case LogConsumerDisconnected:
		return "DISCONNECTED"
	}
	return "UNKNOWN"
}

// LogConfig configures NewLog. Zero limits take the plan defaults (18.1).
type LogConfig struct {
	MaxMessages uint64 // default 2,000,000
	MaxBytes    uint64 // default 256 MiB
	// MaxRecordBytes caps a single frame; the effective cap is min(MaxRecordBytes, MaxBytes/4).
	// Default 1 MiB + 64 KiB (the caller passes MaxMessageSize + 64 KiB when it is set).
	MaxRecordBytes int
	// Consumers is the static consumer set: the canonical NodeIds of the Serve peers. The index of a
	// NodeId in this slice is the consumer index used by every per-consumer method.
	Consumers []string
	// Masked enables per-record consumer masks (interest routing, plan-peerlink-interest-routing 6.3).
	// It requires at most 64 consumers.
	Masked bool
	// MaxScan bounds the offsets one sparse read or one lagging advance scans (MaxScanPerFetch).
	MaxScan int
}

// logMasks holds a chunk's per-record consumer bitmasks. Entries are written before the slot pointer is
// published and changed later only by expiry sweeps, always with atomic operations.
type logMasks [logChunkSlots]uint64

type logChunk struct {
	slots [logChunkSlots]unsafe.Pointer
	// cum[i] is the cumulative accounted byte count of all records appended before slot i's record.
	// It makes the byte accounting of a trim range O(1). Accessed under Log.mu only.
	cum [logChunkSlots]uint64
}

type logConsumer struct {
	nodeID    string
	committed uint64 // C[c]: next offset the consumer needs; 1 at epoch start
	served    uint64 // S[c]: one past the highest offset ever served; 1 before the first batch
	acctNext  uint64 // loss is accounted below this offset (8.5)
	lostTotal uint64
	state     LogConsumerState
	// reading counts ReadFor calls between snapshot and served update. Observation waits for them:
	// counting then could charge records the read already returned, or skip a truncated tail.
	reading int
}

// LogWaiter is a long-poll registration (8.6). Allocate one per session with NewLogWaiter and reuse it:
// waiting allocates nothing.
type LogWaiter struct {
	wakeAt uint64
	idx    int // position in Log.waiters; -1 when not registered
	notify chan struct{}
	timer  *time.Timer
}

// NewLogWaiter returns a waiter with its notify channel (cap 1) and a stopped reusable timer.
func NewLogWaiter() *LogWaiter {
	t := time.NewTimer(time.Hour)
	t.Stop()
	return &LogWaiter{idx: -1, notify: make(chan struct{}, 1), timer: t}
}

// C returns the channel that receives one token when leo reaches the registered wakeAt or the log closes.
func (w *LogWaiter) C() <-chan struct{} { return w.notify }

// Log is the per-source in-memory log (plan 8).
type Log struct {
	mu        sync.Mutex
	epoch     uint64
	startMono time.Time

	chunks    []*logChunk // ascending; chunks[0] covers [firstBase, firstBase+1024) and contains lso
	masks     []*logMasks // parallel to chunks when masked
	firstBase uint64
	spares    [logSpareChunks]atomic.Pointer[logChunk] // filled by refillLoop, taken under mu
	lso, leo  uint64
	bytes     uint64 // accounted footprint: record size classes + attached chunks
	total     uint64 // cumulative accounted record bytes ever appended
	lwm       uint64 // cached min(C[c])
	consumers []logConsumer
	waiters   []*LogWaiter
	minWakeAt uint64
	sealed    bool
	closed    bool

	maxMessages    uint64
	maxBytes       uint64
	maxRecordBytes int
	masked         bool
	allMask        uint64
	maxScan        int
	chunkBytes     uint64

	// Counters guarded by mu.
	appended       [logKindCount]uint64
	trimmed        uint64
	evictedUnread  uint64
	evictedByCount uint64
	evictedByBytes uint64
	spareMisses    uint64

	// Counters updated outside mu.
	droppedSize atomic.Uint64
	droppedInv  atomic.Uint64
	uncaptured  atomic.Uint64

	sealedFlag    atomic.Bool   // mirrors sealed for the hook's lock-free check
	refill        chan struct{} // cap 1: wakes refillLoop
	commitSig     chan struct{} // cap 1: wakes Drain after a commit
	stop          chan struct{}
	refillStopped chan struct{}
	closeOnce     sync.Once
}

// NewLog creates a log with a fresh non-zero epoch from crypto/rand and starts the spare-chunk refill
// goroutine. Close stops it.
func NewLog(cfg LogConfig) (*Log, error) {
	epoch, err := newLogEpoch()
	if err != nil {
		return nil, err
	}
	l := &Log{
		epoch:          epoch,
		startMono:      time.Now(),
		lso:            1,
		leo:            1,
		lwm:            1,
		minWakeAt:      math.MaxUint64,
		maxMessages:    cfg.MaxMessages,
		maxBytes:       cfg.MaxBytes,
		maxRecordBytes: cfg.MaxRecordBytes,
		refill:         make(chan struct{}, 1),
		commitSig:      make(chan struct{}, 1),
		stop:           make(chan struct{}),
		refillStopped:  make(chan struct{}),
	}
	if l.maxMessages == 0 {
		l.maxMessages = logDefaultMaxMessages
	}
	if l.maxBytes == 0 {
		l.maxBytes = logDefaultMaxBytes
	}
	if l.maxRecordBytes <= 0 {
		l.maxRecordBytes = logDefaultMaxRecordBytes
	}
	if q := l.maxBytes / 4; uint64(l.maxRecordBytes) > q {
		l.maxRecordBytes = int(min(q, math.MaxInt32))
	}
	if cfg.Masked && len(cfg.Consumers) > 64 {
		return nil, ErrLogTooManyConsumers
	}
	l.masked = cfg.Masked
	l.maxScan = cfg.MaxScan
	if l.maxScan <= 0 {
		l.maxScan = logDefaultMaxScan
	}
	l.chunkBytes = logChunkBytes
	if l.masked {
		l.chunkBytes += logMasksBytes
	}
	if n := len(cfg.Consumers); n >= 64 {
		l.allMask = math.MaxUint64
	} else {
		l.allMask = 1<<n - 1
	}
	l.consumers = make([]logConsumer, len(cfg.Consumers))
	for i, id := range cfg.Consumers {
		l.consumers[i] = logConsumer{nodeID: id, committed: 1, served: 1, acctNext: 1}
	}
	l.fillSpares()
	go l.refillLoop()
	return l, nil
}

func newLogEpoch() (uint64, error) {
	var b [8]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, err
		}
		if e := binary.LittleEndian.Uint64(b[:]); e != 0 {
			return e, nil
		}
	}
}

func (l *Log) fillSpares() {
	for i := range l.spares {
		if l.spares[i].Load() == nil {
			l.spares[i].CompareAndSwap(nil, new(logChunk))
		}
	}
}

func (l *Log) refillLoop() {
	defer close(l.refillStopped)
	for {
		select {
		case <-l.refill:
			l.fillSpares()
		case <-l.stop:
			return
		}
	}
}

// Close stops the refill goroutine and wakes every waiter. Later Waits return at once. It does not seal.
func (l *Log) Close() {
	l.closeOnce.Do(func() {
		close(l.stop)
		<-l.refillStopped
		l.mu.Lock()
		l.closed = true
		for _, w := range l.waiters {
			w.idx = -1
			select {
			case w.notify <- struct{}{}:
			default:
			}
		}
		clear(l.waiters)
		l.waiters = l.waiters[:0]
		l.minWakeAt = math.MaxUint64
		l.mu.Unlock()
	})
}

// Epoch is the random non-zero id of this log, new on every process start.
func (l *Log) Epoch() uint64 { return l.epoch }

// StartMono is the monotonic time base of captureMonoMs.
func (l *Log) StartMono() time.Time { return l.startMono }

// MonoMs converts t to milliseconds since the epoch start (captureMonoMs, sourceMonoMs). t must carry
// a monotonic reading (time.Now()).
func (l *Log) MonoMs(t time.Time) uint64 {
	d := t.Sub(l.startMono)
	if d < 0 {
		return 0
	}
	return uint64(d.Milliseconds())
}

// MaxRecordBytes is the effective capture cap per frame, min(MaxRecordBytes, MaxBytes/4).
func (l *Log) MaxRecordBytes() int { return l.maxRecordBytes }

// CheckRecordSize reports whether a frame of size bytes may be appended. When it may not, it counts
// captureDropped{size}. The hook calls it with wire.RecordSize before allocating the frame.
func (l *Log) CheckRecordSize(size int) bool {
	if size > l.maxRecordBytes {
		l.droppedSize.Add(1)
		return false
	}
	return true
}

// CountCaptureInvalid counts captureDropped{invalid}.
func (l *Log) CountCaptureInvalid() { l.droppedInv.Add(1) }

// CountUncapturedAtShutdown counts a publish that reached the hook after capture was switched off (15.6).
func (l *Log) CountUncapturedAtShutdown() { l.uncaptured.Add(1) }

// Sealed reports whether Seal was called. It is a single atomic load, for the hook's inactive path.
func (l *Log) Sealed() bool { return l.sealedFlag.Load() }

// Seal switches capture off for good (15.6 step 3): every later Append is discarded and counted as
// uncapturedAtShutdown. It returns the final leo.
func (l *Log) Seal() uint64 {
	l.mu.Lock()
	l.sealed = true
	l.sealedFlag.Store(true)
	leo := l.leo
	l.mu.Unlock()
	return leo
}

// Append is AppendMask for every consumer.
func (l *Log) Append(frame []byte, kind LogKind) (uint64, bool) {
	return l.AppendMask(frame, kind, l.allMask)
}

// AllConsumers is the mask with a bit for every consumer.
func (l *Log) AllConsumers() uint64 { return l.allMask }

// Masked reports whether the log keeps per-record consumer masks.
func (l *Log) Masked() bool { return l.masked }

// AppendMask stores frame at the next offset and returns that offset. frame must be a complete record
// frame (4+recLen == len(frame)) in its own allocation; the log keeps it, unmodified, until the record
// is trimmed or evicted. mask holds a bit per consumer index that needs the record; it is ignored when
// the log is not masked. It returns false when the frame is malformed (captureDropped{invalid}), too
// large (captureDropped{size}) or the log is sealed (uncapturedAtShutdown).
func (l *Log) AppendMask(frame []byte, kind LogKind, mask uint64) (uint64, bool) {
	n := len(frame)
	if n < 4 || uint64(binary.LittleEndian.Uint32(frame))+4 != uint64(n) {
		l.droppedInv.Add(1)
		return 0, false
	}
	if n > l.maxRecordBytes {
		l.droppedSize.Add(1)
		return 0, false
	}
	if kind >= logKindCount {
		kind = LogKindClient
	}
	acc := logFrameAccounted(n)
	p := unsafe.Pointer(unsafe.SliceData(frame))

	l.mu.Lock()
	if l.sealed {
		l.mu.Unlock()
		l.uncaptured.Add(1)
		return 0, false
	}
	off := l.leo
	l.appended[kind]++
	if len(l.consumers) == 0 {
		// Nobody can pull it: the record is trimmed at once.
		l.leo = off + 1
		l.lso = l.leo
		l.lwm = l.leo
		l.trimmed++
		l.mu.Unlock()
		return off, true
	}
	if len(l.chunks) == 0 {
		l.firstBase = off
	}
	rel := off - l.firstBase
	ci := rel >> logChunkShift
	// Spares are refilled as soon as one is taken rather than at half a chunk: goroutine wake-up
	// latency can exceed the time a burst needs to fill half a chunk.
	refill := ci == uint64(len(l.chunks))
	if refill {
		l.attachChunkLocked()
	}
	ck := l.chunks[ci]
	slot := rel & logChunkMask
	ck.cum[slot] = l.total
	if l.masked {
		atomic.StoreUint64(&l.masks[ci][slot], mask)
	}
	atomic.StorePointer(&ck.slots[slot], p)
	l.total += acc
	l.bytes += acc
	l.leo = off + 1
	var tck *logChunk
	var ta, tb uint64
	if l.masked && mask&l.allMask != l.allMask {
		tck, ta, tb = l.skipCaughtUpLocked(off, mask)
	}
	if l.leo-l.lso > l.maxMessages || l.bytes > l.maxBytes {
		l.evictLocked(off)
	}
	if l.leo >= l.minWakeAt {
		l.wakeDueLocked()
	}
	l.mu.Unlock()
	clearLogSlots(tck, ta, tb)
	if refill {
		select {
		case l.refill <- struct{}{}:
		default:
		}
	}
	return off, true
}

func (l *Log) attachChunkLocked() {
	var ck *logChunk
	for i := range l.spares {
		// Only refillLoop stores into an empty slot and only the holder of mu empties one.
		if ck = l.spares[i].Load(); ck != nil {
			l.spares[i].Store(nil)
			break
		}
	}
	if ck == nil {
		ck = new(logChunk)
		l.spareMisses++
	}
	l.chunks = append(l.chunks, ck)
	if l.masked {
		l.masks = append(l.masks, new(logMasks))
	}
	l.bytes += l.chunkBytes
}

// skipCaughtUpLocked advances C[c] over the record at off for every caught-up consumer whose bit is not
// in mask (6.5 rule 1), so records a consumer never needs do not pin the log.
func (l *Log) skipCaughtUpLocked(off, mask uint64) (*logChunk, uint64, uint64) {
	moved := false
	for i := range l.consumers {
		con := &l.consumers[i]
		if con.committed == off && mask&(1<<uint(i)) == 0 {
			con.committed = off + 1
			moved = true
		}
	}
	if !moved {
		return nil, 0, 0
	}
	return l.updateLWMLocked()
}

// maskAtLocked returns the consumer mask of the record at x, lso <= x < leo, of a masked log.
func (l *Log) maskAtLocked(x uint64) uint64 {
	rel := x - l.firstBase
	return atomic.LoadUint64(&l.masks[rel>>logChunkShift][rel&logChunkMask])
}

// skipLaggingLocked advances C[c] over the leading run of records without bit c, scanning at most
// maxScan offsets (6.5 rule 2).
func (l *Log) skipLaggingLocked(c int) bool {
	con := &l.consumers[c]
	x := max(con.committed, l.lso)
	if x >= l.leo {
		return false
	}
	bit := uint64(1) << uint(c)
	end := min(l.leo, x+uint64(l.maxScan))
	start := x
	for x < end && l.maskAtLocked(x)&bit == 0 {
		x++
	}
	if x == start || x <= con.committed {
		return false
	}
	con.committed = x
	return true
}

// AdvanceSkipped applies the lagging advance (6.5 rule 2) to every consumer. The manager calls it every
// 100 ms; it is a no-op on an unmasked log.
func (l *Log) AdvanceSkipped() {
	if !l.masked {
		return
	}
	l.mu.Lock()
	moved := false
	for c := range l.consumers {
		if l.skipLaggingLocked(c) {
			moved = true
		}
	}
	var ck *logChunk
	var a, b uint64
	if moved {
		ck, a, b = l.updateLWMLocked()
	}
	l.mu.Unlock()
	clearLogSlots(ck, a, b)
}

// ClearConsumerBits is the persistent-expiry sweep (6.5): starting at from (raised to max(C[c], lso)), it
// scans at most maxScan offsets and clears bit c of every record for which drop returns true. drop gets
// the record frame and is called under the log lock; it must be cheap and must not call into the log.
// It returns the offset after the last scanned one (leo when done) and the number of bits cleared.
func (l *Log) ClearConsumerBits(c int, from uint64, drop func(frame []byte) bool) (next uint64, cleared uint64) {
	if !l.masked || c < 0 || c >= len(l.consumers) {
		return 0, 0
	}
	bit := uint64(1) << uint(c)
	l.mu.Lock()
	x := max(from, l.consumers[c].committed, l.lso)
	end := min(l.leo, x+uint64(l.maxScan))
	for ; x < end; x++ {
		rel := x - l.firstBase
		mp := &l.masks[rel>>logChunkShift][rel&logChunkMask]
		m := atomic.LoadUint64(mp)
		if m&bit == 0 {
			continue
		}
		p := atomic.LoadPointer(&l.chunks[rel>>logChunkShift].slots[rel&logChunkMask])
		if p == nil {
			continue
		}
		size := 4 + int(binary.LittleEndian.Uint32(unsafe.Slice((*byte)(p), 4)))
		if drop(unsafe.Slice((*byte)(p), size)) {
			atomic.StoreUint64(mp, m&^bit)
			cleared++
		}
	}
	var ck *logChunk
	var a, b uint64
	if cleared > 0 && l.skipLaggingLocked(c) {
		ck, a, b = l.updateLWMLocked()
	}
	l.mu.Unlock()
	clearLogSlots(ck, a, b)
	return x, cleared
}

// detachLocked drops the leading chunks that lie entirely below lso.
func (l *Log) detachLocked() {
	k := 0
	for k < len(l.chunks) && l.lso-l.firstBase >= logChunkSlots {
		l.firstBase += logChunkSlots
		k++
	}
	if k > 0 {
		clear(l.chunks[:k])
		l.chunks = l.chunks[k:]
		if l.masked {
			clear(l.masks[:k])
			l.masks = l.masks[k:]
		}
		l.bytes -= uint64(k) * l.chunkBytes
	}
}

// cumAtLocked returns the cumulative accounted bytes before offset x, lso <= x <= leo.
func (l *Log) cumAtLocked(x uint64) uint64 {
	if x == l.leo {
		return l.total
	}
	rel := x - l.firstBase
	return l.chunks[rel>>logChunkShift].cum[rel&logChunkMask]
}

// evictLocked drops the oldest records while a bound is exceeded, never the record at keep (8.5).
func (l *Log) evictLocked(keep uint64) {
	for l.lso < keep {
		byCount := l.leo-l.lso > l.maxMessages
		if !byCount && l.bytes <= l.maxBytes {
			return
		}
		off := l.lso
		rel := off - l.firstBase
		ck := l.chunks[rel>>logChunkShift]
		slot := rel & logChunkMask
		l.bytes -= l.cumAtLocked(off+1) - ck.cum[slot]
		atomic.StorePointer(&ck.slots[slot], nil)
		if byCount {
			l.evictedByCount++
		} else {
			l.evictedByBytes++
		}
		if off >= l.lwm {
			l.evictedUnread++
		}
		if l.masked {
			l.accountEvictLocked(off, l.maskAtLocked(off))
		}
		l.lso = off + 1
		if l.lso-l.firstBase >= logChunkSlots {
			l.detachLocked()
		}
	}
}

// accountEvictLocked charges the evicted record at off as lost only to the consumers that needed it.
// On a masked log loss is accounted at eviction, where the record's mask is still known; observeLocked
// then finds nothing left to charge. A consumer with a read in progress is left to observeLocked.
func (l *Log) accountEvictLocked(off, mask uint64) {
	for i := range l.consumers {
		con := &l.consumers[i]
		if con.reading > 0 || off < max(con.acctNext, con.committed, con.served) {
			continue
		}
		if mask&(1<<uint(i)) != 0 {
			con.lostTotal++
		}
		con.acctNext = off + 1
	}
}

func (l *Log) wakeDueLocked() {
	minWake := uint64(math.MaxUint64)
	for i := 0; i < len(l.waiters); {
		w := l.waiters[i]
		if w.wakeAt > l.leo {
			minWake = min(minWake, w.wakeAt)
			i++
			continue
		}
		select {
		case w.notify <- struct{}{}:
		default:
		}
		l.removeWaiterLocked(i)
	}
	l.minWakeAt = minWake
}

func (l *Log) removeWaiterLocked(i int) {
	w := l.waiters[i]
	last := len(l.waiters) - 1
	if i != last {
		l.waiters[i] = l.waiters[last]
		l.waiters[i].idx = i
	}
	l.waiters[last] = nil
	l.waiters = l.waiters[:last]
	w.idx = -1
}

// Wait registers w to be notified when leo reaches wakeAt (offset+minRecords, 8.6). It returns false
// without registering when leo already reached wakeAt or the log is closed. A registered waiter is
// removed when it fires; call Unwait after waking for any reason.
func (l *Log) Wait(w *LogWaiter, wakeAt uint64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.leo >= wakeAt {
		return false
	}
	w.wakeAt = wakeAt
	if w.idx < 0 {
		w.idx = len(l.waiters)
		l.waiters = append(l.waiters, w)
	}
	l.minWakeAt = min(l.minWakeAt, wakeAt)
	return true
}

// Unwait deregisters w (if it is still registered) and drops a pending token, so the next Wait cannot
// return on a stale notification.
func (l *Log) Unwait(w *LogWaiter) {
	l.mu.Lock()
	if w.idx >= 0 {
		l.removeWaiterLocked(w.idx)
		l.minWakeAt = math.MaxUint64
		for _, o := range l.waiters {
			l.minWakeAt = min(l.minWakeAt, o.wakeAt)
		}
	}
	select {
	case <-w.notify:
	default:
	}
	l.mu.Unlock()
}

// WaitFor blocks until leo reaches wakeAt, maxWait elapses, ctx ends or the log closes, using the
// waiter's reusable timer. It reports whether leo reached wakeAt.
func (l *Log) WaitFor(ctx context.Context, w *LogWaiter, wakeAt uint64, maxWait time.Duration) bool {
	if l.Wait(w, wakeAt) {
		w.timer.Reset(maxWait)
		select {
		case <-w.notify:
		case <-w.timer.C:
		case <-ctx.Done():
		}
		w.timer.Stop()
		l.Unwait(w)
	}
	return l.LEO() >= wakeAt
}

// LogReadResult describes the frames Read put into out.
type LogReadResult struct {
	Base      uint64 // offset of out[0]; equals from unless the range below lso was gone (GAP)
	Count     int    // frames in out, at offsets Base..Base+Count-1 (dense) or Base+deltas[i] (sparse)
	Span      uint64 // offsets covered: Base..Base+Span-1; equals Count unless records were skipped
	Sparse    bool   // records without the consumer's bit were skipped; deltas holds the offsets
	Bytes     int    // sum of the frame lengths (recordsBytes)
	Lost      uint64 // Base - from: offsets requested but no longer held (GAP when > 0)
	Truncated bool   // stopped at a record evicted after the snapshot (TRUNCATED)
	LSO, LEO  uint64 // bounds at the snapshot
}

// Read snapshots the log and copies up to maxRecords frames starting at from into *out (reused;
// truncated to length 0 first). It returns at least one record when any is available, even if that
// record is larger than maxBytes (maxBytes <= 0: no byte limit). The frames are shared and immutable;
// the caller must clear its slice after writing them so it does not pin evicted frames. from must be
// in 1..leo; from < lso yields the records from lso with Lost = lso - from.
func (l *Log) Read(from uint64, maxRecords, maxBytes int, out *[][]byte) (LogReadResult, error) {
	return l.readRetry(-1, from, maxRecords, maxBytes, out, nil)
}

// ReadFor is Read on behalf of consumer c, the serve path of a session. It observes c (8.5) at the
// snapshot and advances S[c] to the end of the returned frames, so the source counts exactly the gaps
// the consumer sees. Records returned but never written count as served: the source count stays a
// lower bound.
func (l *Log) ReadFor(c int, from uint64, maxRecords, maxBytes int, out *[][]byte) (LogReadResult, error) {
	if c < 0 || c >= len(l.consumers) {
		*out = (*out)[:0]
		return LogReadResult{Base: from}, ErrLogUnknownConsumer
	}
	return l.readRetry(c, from, maxRecords, maxBytes, out, nil)
}

// ReadSparse is ReadFor that skips the records without consumer c's bit (6.4). The offsets of the
// returned frames relative to Base go to *deltas (reused) when the result is Sparse. It scans at most
// MaxScan offsets. On an unmasked log it is ReadFor.
func (l *Log) ReadSparse(c int, from uint64, maxRecords, maxBytes int, out *[][]byte, deltas *[]uint32) (LogReadResult, error) {
	if c < 0 || c >= len(l.consumers) {
		*out = (*out)[:0]
		return LogReadResult{Base: from}, ErrLogUnknownConsumer
	}
	if !l.masked {
		deltas = nil
	}
	return l.readRetry(c, from, maxRecords, maxBytes, out, deltas)
}

func (l *Log) readRetry(c int, from uint64, maxRecords, maxBytes int, out *[][]byte, deltas *[]uint32) (LogReadResult, error) {
	for attempt := 0; ; attempt++ {
		res, err := l.read(c, from, maxRecords, maxBytes, out, deltas)
		// An eviction between the snapshot and the first slot load leaves nothing to serve; a fresh
		// snapshot reports it as a GAP instead of an empty truncated batch.
		if err != nil || res.Span > 0 || !res.Truncated || attempt == 2 {
			return res, err
		}
	}
}

func (l *Log) read(c int, from uint64, maxRecords, maxBytes int, out *[][]byte, deltas *[]uint32) (LogReadResult, error) {
	if deltas != nil {
		return l.readSparse(c, from, maxRecords, maxBytes, out, deltas)
	}
	var cks [logReadMaxChunks]*logChunk
	dst := (*out)[:0]

	l.mu.Lock()
	lso, leo := l.lso, l.leo
	if from == 0 || from > leo {
		l.mu.Unlock()
		*out = dst
		return LogReadResult{Base: from, LSO: lso, LEO: leo}, ErrLogOffsetOutOfRange
	}
	if c >= 0 {
		con := &l.consumers[c]
		l.observeLocked(con)
		con.reading++
	}
	base := max(from, lso)
	n := leo - base
	if maxRecords <= 0 {
		n = 0
	} else if n > uint64(maxRecords) {
		n = uint64(maxRecords)
	}
	fb := l.firstBase
	var c0 uint64
	if n > 0 {
		c0 = (base - fb) >> logChunkShift
		cLast := (base + n - 1 - fb) >> logChunkShift
		if cLast-c0 >= logReadMaxChunks {
			cLast = c0 + logReadMaxChunks - 1
			n = fb + (cLast+1)<<logChunkShift - base
		}
		copy(cks[:], l.chunks[c0:cLast+1])
	}
	l.mu.Unlock()

	res := LogReadResult{Base: base, Lost: base - from, LSO: lso, LEO: leo}
	total := 0
	for i := uint64(0); i < n; i++ {
		rel := base + i - fb
		p := atomic.LoadPointer(&cks[(rel>>logChunkShift)-c0].slots[rel&logChunkMask])
		if p == nil {
			res.Truncated = true
			break
		}
		size := 4 + int(binary.LittleEndian.Uint32(unsafe.Slice((*byte)(p), 4)))
		if maxBytes > 0 && len(dst) > 0 && total+size > maxBytes {
			break
		}
		dst = append(dst, unsafe.Slice((*byte)(p), size))
		total += size
	}
	*out = dst
	res.Count = len(dst)
	res.Span = uint64(res.Count)
	res.Bytes = total
	if c >= 0 {
		l.mu.Lock()
		con := &l.consumers[c]
		con.served = max(con.served, base+uint64(res.Count))
		con.reading--
		l.mu.Unlock()
	}
	return res, nil
}

func (l *Log) readSparse(c int, from uint64, maxRecords, maxBytes int, out *[][]byte, deltas *[]uint32) (LogReadResult, error) {
	var cks [logReadMaxChunks]*logChunk
	var mks [logReadMaxChunks]*logMasks
	dst := (*out)[:0]
	ds := (*deltas)[:0]

	l.mu.Lock()
	lso, leo := l.lso, l.leo
	if from == 0 || from > leo {
		l.mu.Unlock()
		*out, *deltas = dst, ds
		return LogReadResult{Base: from, LSO: lso, LEO: leo}, ErrLogOffsetOutOfRange
	}
	con := &l.consumers[c]
	l.observeLocked(con)
	con.reading++
	base := max(from, lso)
	n := min(leo-base, uint64(l.maxScan))
	if maxRecords <= 0 {
		n = 0
	}
	fb := l.firstBase
	var c0 uint64
	if n > 0 {
		c0 = (base - fb) >> logChunkShift
		cLast := (base + n - 1 - fb) >> logChunkShift
		if cLast-c0 >= logReadMaxChunks {
			cLast = c0 + logReadMaxChunks - 1
			n = fb + (cLast+1)<<logChunkShift - base
		}
		copy(cks[:], l.chunks[c0:cLast+1])
		copy(mks[:], l.masks[c0:cLast+1])
	}
	l.mu.Unlock()

	res := LogReadResult{Base: base, Lost: base - from, LSO: lso, LEO: leo}
	bit := uint64(1) << uint(c)
	total := 0
	skipped := false
	i := uint64(0)
	for ; i < n; i++ {
		rel := base + i - fb
		k := (rel >> logChunkShift) - c0
		s := rel & logChunkMask
		p := atomic.LoadPointer(&cks[k].slots[s])
		if p == nil {
			res.Truncated = true
			break
		}
		if atomic.LoadUint64(&mks[k][s])&bit == 0 {
			// The span table (4 bytes per record plus span) counts against maxBytes once the batch is sparse.
			if !skipped && maxBytes > 0 && len(dst) > 0 && total+4+4*len(dst) > maxBytes {
				break
			}
			skipped = true
			continue
		}
		size := 4 + int(binary.LittleEndian.Uint32(unsafe.Slice((*byte)(p), 4)))
		cost := size
		if skipped {
			cost += 4
		}
		if maxBytes > 0 && len(dst) > 0 && total+cost+sparseOverhead(skipped, len(dst)) > maxBytes {
			break
		}
		dst = append(dst, unsafe.Slice((*byte)(p), size))
		ds = append(ds, uint32(i))
		total += size
		if len(dst) >= maxRecords {
			i++
			break
		}
	}
	res.Span = i
	res.Count = len(dst)
	res.Bytes = total
	res.Sparse = res.Count < int(res.Span)
	if !res.Sparse {
		ds = ds[:0]
	}
	*out, *deltas = dst, ds

	l.mu.Lock()
	con = &l.consumers[c]
	con.served = max(con.served, base+res.Span)
	con.reading--
	l.mu.Unlock()
	return res, nil
}

// sparseOverhead is the span-table size already owed for n records of a sparse batch.
func sparseOverhead(sparse bool, n int) int {
	if !sparse {
		return 0
	}
	return 4 + 4*n
}

// Commit sets C[c] = max(C[c], off) (8.4). Records below the new low-water mark are trimmed: the
// bounds and accounting change under the lock, the slots of a partially trimmed chunk are cleared
// after it. A commit above leo is a protocol error.
func (l *Log) Commit(c int, off uint64) error {
	if c < 0 || c >= len(l.consumers) {
		return ErrLogUnknownConsumer
	}
	l.mu.Lock()
	if off > l.leo {
		l.mu.Unlock()
		return ErrLogCommitBeyondEnd
	}
	con := &l.consumers[c]
	if off <= con.committed {
		l.mu.Unlock()
		return nil
	}
	con.committed = off
	if l.masked {
		l.skipLaggingLocked(c)
	}
	ck, a, b := l.updateLWMLocked()
	l.mu.Unlock()
	select {
	case l.commitSig <- struct{}{}:
	default:
	}
	clearLogSlots(ck, a, b)
	return nil
}

// updateLWMLocked recomputes the low-water mark and trims below it. It returns the chunk and slot
// range [a, b) that the caller clears after unlocking.
func (l *Log) updateLWMLocked() (*logChunk, uint64, uint64) {
	lwm := uint64(math.MaxUint64)
	for i := range l.consumers {
		lwm = min(lwm, l.consumers[i].committed)
	}
	if lwm == l.lwm {
		return nil, 0, 0
	}
	l.lwm = lwm
	if lwm <= l.lso {
		return nil, 0, 0
	}
	from := l.lso
	l.bytes -= l.cumAtLocked(lwm) - l.cumAtLocked(from)
	l.trimmed += lwm - from
	l.lso = lwm
	l.detachLocked()
	if len(l.chunks) == 0 || l.firstBase >= lwm {
		return nil, 0, 0
	}
	a := uint64(0)
	if from > l.firstBase {
		a = from - l.firstBase
	}
	return l.chunks[0], a, lwm - l.firstBase
}

func clearLogSlots(ck *logChunk, a, b uint64) {
	if ck == nil {
		return
	}
	for i := a; i < b; i++ {
		atomic.StorePointer(&ck.slots[i], nil)
	}
}

// MarkServed sets S[c] = max(S[c], upTo). ReadFor already does this; MarkServed is for frames served
// by other means.
func (l *Log) MarkServed(c int, upTo uint64) {
	if c < 0 || c >= len(l.consumers) {
		return
	}
	l.mu.Lock()
	con := &l.consumers[c]
	con.served = max(con.served, min(upTo, l.leo))
	l.mu.Unlock()
}

// SetConsumerState records the consumer's connection state (11.3). Drain waits for CONNECTED
// consumers when it is given no predicate.
func (l *Log) SetConsumerState(c int, st LogConsumerState) {
	if c < 0 || c >= len(l.consumers) {
		return
	}
	l.mu.Lock()
	l.consumers[c].state = st
	l.mu.Unlock()
}

// ConsumerIndex returns the index of a canonical NodeId in the static consumer set, or -1.
func (l *Log) ConsumerIndex(nodeID string) int {
	for i := range l.consumers {
		if l.consumers[i].nodeID == nodeID {
			return i
		}
	}
	return -1
}

// NumConsumers returns the size of the static consumer set.
func (l *Log) NumConsumers() int { return len(l.consumers) }

// Committed returns C[c].
func (l *Log) Committed(c int) uint64 {
	if c < 0 || c >= len(l.consumers) {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.consumers[c].committed
}

// Bounds returns lso and leo.
func (l *Log) Bounds() (lso, leo uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lso, l.leo
}

// LEO returns the offset the next append gets.
func (l *Log) LEO() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.leo
}

// observeLocked applies the source-side loss formula of 8.5: records evicted below lso that were
// neither committed nor served to c are added to lostTotal exactly once.
func (l *Log) observeLocked(con *logConsumer) {
	if con.reading > 0 {
		return
	}
	a := max(con.acctNext, con.committed, con.served)
	if l.lso > a {
		con.lostTotal += l.lso - a
		a = l.lso
	}
	con.acctNext = a
}

// LogConsumerStats is a per-consumer snapshot (20.1).
type LogConsumerStats struct {
	NodeID    string
	State     LogConsumerState
	Committed uint64 // C[c]
	Served    uint64 // S[c]
	Lag       uint64 // leo - C[c]; after Seal and the end of the sessions this is shutdownUnserved
	LostTotal uint64
}

// ObserveConsumer accounts loss for c (call it on HELLO, FETCH and status snapshots) and returns its stats.
func (l *Log) ObserveConsumer(c int) LogConsumerStats {
	if c < 0 || c >= len(l.consumers) {
		return LogConsumerStats{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.consumerStatsLocked(c)
}

func (l *Log) consumerStatsLocked(c int) LogConsumerStats {
	con := &l.consumers[c]
	l.observeLocked(con)
	return LogConsumerStats{
		NodeID:    con.nodeID,
		State:     con.state,
		Committed: con.committed,
		Served:    con.served,
		Lag:       l.leo - min(con.committed, l.leo),
		LostTotal: con.lostTotal,
	}
}

// ConsumerStats observes every consumer and returns their stats in consumer index order.
func (l *Log) ConsumerStats() []LogConsumerStats {
	out := make([]LogConsumerStats, len(l.consumers))
	l.mu.Lock()
	for i := range l.consumers {
		out[i] = l.consumerStatsLocked(i)
	}
	l.mu.Unlock()
	return out
}

// LogResume is the source's resume decision for a HELLO (9.6).
type LogResume struct {
	ResumeAt          uint64
	LSO, LEO          uint64
	Committed         uint64 // C[c] after the decision
	LostOnResume      uint64
	ConsumerStateUsed bool // HELLO_OK flag CONSUMER_STATE_USED
	SourceReset       bool // HELLO_OK flag SOURCE_RESET
}

// Resume applies the resume rules of 9.6 for consumer c atomically: lastEpoch and resumeOffset come
// from HELLO. A same-epoch resumeOffset above leo returns ErrLogOffsetOutOfRange.
func (l *Log) Resume(c int, lastEpoch, resumeOffset uint64) (LogResume, error) {
	if c < 0 || c >= len(l.consumers) {
		return LogResume{}, ErrLogUnknownConsumer
	}
	if resumeOffset == 0 {
		resumeOffset = 1 // "none": nothing applied in this epoch
	}
	l.mu.Lock()
	con := &l.consumers[c]
	r := LogResume{LSO: l.lso, LEO: l.leo}
	var ck *logChunk
	var a, b uint64
	switch {
	case lastEpoch == l.epoch && resumeOffset > l.leo:
		l.mu.Unlock()
		return r, ErrLogOffsetOutOfRange
	case lastEpoch == l.epoch:
		if resumeOffset > con.committed {
			con.committed = resumeOffset
			ck, a, b = l.updateLWMLocked()
		}
		if resumeOffset >= l.lso {
			r.ResumeAt = resumeOffset
			r.ConsumerStateUsed = true
		} else {
			r.ResumeAt = l.lso
			r.LostOnResume = l.lso - resumeOffset
		}
	default:
		r.SourceReset = lastEpoch != 0
		r.ResumeAt = max(con.committed, l.lso)
		if l.lso > con.committed {
			r.LostOnResume = l.lso - con.committed
		}
	}
	l.observeLocked(con)
	r.LSO = l.lso
	r.Committed = con.committed
	l.mu.Unlock()
	clearLogSlots(ck, a, b)
	return r, nil
}

// LogStats is a snapshot of the log gauges and counters (20.1).
type LogStats struct {
	Epoch                 uint64
	LSO, LEO              uint64
	LWM                   uint64
	Records               uint64
	Bytes                 uint64 // accounted footprint (8.7)
	MaxBytes              uint64
	MaxMessages           uint64
	Chunks                int
	AppendedClient        uint64
	AppendedInline        uint64
	AppendedWill          uint64
	AppendedBytes         uint64 // cumulative accounted record bytes; its rate feeds capacitySeconds
	Trimmed               uint64
	EvictedUnread         uint64
	EvictedByCount        uint64
	EvictedByBytes        uint64
	CaptureDroppedSize    uint64
	CaptureDroppedInvalid uint64
	SpareMisses           uint64
	UncapturedAtShutdown  uint64
	Sealed                bool
}

// Stats returns a consistent snapshot of the log counters.
func (l *Log) Stats() LogStats {
	l.mu.Lock()
	s := LogStats{
		Epoch:          l.epoch,
		LSO:            l.lso,
		LEO:            l.leo,
		LWM:            l.lwm,
		Records:        l.leo - l.lso,
		Bytes:          l.bytes,
		MaxBytes:       l.maxBytes,
		MaxMessages:    l.maxMessages,
		Chunks:         len(l.chunks),
		AppendedClient: l.appended[LogKindClient],
		AppendedInline: l.appended[LogKindInline],
		AppendedWill:   l.appended[LogKindWill],
		AppendedBytes:  l.total,
		Trimmed:        l.trimmed,
		EvictedUnread:  l.evictedUnread,
		EvictedByCount: l.evictedByCount,
		EvictedByBytes: l.evictedByBytes,
		SpareMisses:    l.spareMisses,
		Sealed:         l.sealed,
	}
	l.mu.Unlock()
	s.CaptureDroppedSize = l.droppedSize.Load()
	s.CaptureDroppedInvalid = l.droppedInv.Load()
	s.UncapturedAtShutdown = l.uncaptured.Load()
	return s
}

// LogDrainResult reports a drain (15.6).
type LogDrainResult struct {
	Target   uint64 // drainTarget: leo when the drain started
	FinalLEO uint64 // leo after Seal; final from here on
	Complete bool   // every connected consumer committed Target before the deadline
	Waited   time.Duration
	// Unserved is FinalLEO - C[c] per consumer at the seal. Sessions may still commit afterwards;
	// ConsumerStats().Lag after the sessions closed is the final shutdownUnserved.
	Unserved []uint64
}

// Drain fixes drainTarget = leo, waits up to timeout (<= 0: no wait) or until ctx ends for every
// connected consumer to commit it, then seals the log. connected decides which consumers are waited
// for; nil means those whose state is CONNECTED. Disconnected consumers are never waited for.
func (l *Log) Drain(ctx context.Context, timeout time.Duration, connected func(c int) bool) LogDrainResult {
	start := time.Now()
	if connected == nil {
		connected = l.isConnected
	}
	res := LogDrainResult{Target: l.LEO()}
	res.Complete = l.drainedTo(res.Target, connected)
	if !res.Complete && timeout > 0 {
		deadline := time.NewTimer(timeout)
		poll := time.NewTicker(20 * time.Millisecond)
	wait:
		for !res.Complete {
			select {
			case <-l.commitSig:
			case <-poll.C:
			case <-deadline.C:
				break wait
			case <-ctx.Done():
				break wait
			}
			res.Complete = l.drainedTo(res.Target, connected)
		}
		deadline.Stop()
		poll.Stop()
	}
	res.FinalLEO = l.Seal()
	res.Waited = time.Since(start)
	res.Unserved = make([]uint64, len(l.consumers))
	l.mu.Lock()
	for i := range l.consumers {
		res.Unserved[i] = res.FinalLEO - min(l.consumers[i].committed, res.FinalLEO)
	}
	l.mu.Unlock()
	return res
}

func (l *Log) isConnected(c int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.consumers[c].state == LogConsumerConnected
}

func (l *Log) drainedTo(target uint64, connected func(int) bool) bool {
	for c := range l.consumers {
		if connected(c) && l.Committed(c) < target {
			return false
		}
	}
	return true
}

// logSizeClasses are the Go runtime's small object size classes (internal/runtime/gc/sizeclasses.go).
var logSizeClasses = [...]uint16{
	8, 16, 24, 32, 48, 64, 80, 96, 112, 128, 144, 160, 176, 192, 208, 224, 240, 256, 288, 320, 352,
	384, 416, 448, 480, 512, 576, 640, 704, 768, 896, 1024, 1152, 1280, 1408, 1536, 1792, 2048, 2304,
	2688, 3072, 3200, 3456, 4096, 4864, 5376, 6144, 6528, 6784, 6912, 8192, 9472, 9728, 10240, 10880,
	12288, 13568, 14336, 16384, 18432, 19072, 20480, 21760, 24576, 27264, 28672, 32768,
}

// logSizeBy8 and logSizeBy128 map a size to its size class in O(1), like the runtime's
// size_to_class tables: sizes up to 1024 in steps of 8, larger small sizes in steps of 128.
var logSizeBy8, logSizeBy128 = logSizeTables()

func logSizeTables() (by8 [1024/8 + 1]uint16, by128 [(32768-1024)/128 + 1]uint16) {
	class := func(n int) uint16 {
		for _, c := range logSizeClasses {
			if int(c) >= n {
				return c
			}
		}
		return 0
	}
	for i := range by8 {
		by8[i] = class(i * 8)
	}
	for i := range by128 {
		by128[i] = class(1024 + i*128)
	}
	return
}

// logFrameAccounted is the heap footprint of an n-byte frame allocation (8.7): its size class up to
// 32 KiB, whole 8 KiB pages above.
func logFrameAccounted(n int) uint64 {
	switch {
	case n <= 1024:
		return uint64(logSizeBy8[(n+7)>>3])
	case n <= 32768:
		return uint64(logSizeBy128[(n-1024+127)>>7])
	}
	return (uint64(n) + 8191) &^ 8191
}
