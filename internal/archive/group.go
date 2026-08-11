package archive

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"monstermq.io/edge/internal/queue"
	"monstermq.io/edge/internal/stores"
)

// Group is one archive group: it owns a last-value MessageStore and a
// MessageArchive. Incoming messages that match any of the group's topic filters
// are buffered and flushed on a tick to amortize disk I/O. If configured with a
// QueueType ("MEMORY" or "DISK"), store-and-forward is used so messages are not lost
// if the target archive store is temporarily unavailable.
type Group struct {
	cfg      stores.ArchiveGroupConfig
	lastVal  stores.MessageStore
	archive  stores.MessageArchive
	queue    queue.MessageQueue
	logger   *slog.Logger
	mu       sync.Mutex
	pending  []stores.BrokerMessage
	flushCh  chan struct{}
	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
	flushDur time.Duration
	closers  []func() error

	outCount  atomic.Int64
	metricsMu sync.RWMutex
	latest    MetricsSnapshot
}

type MetricsSnapshot struct {
	MessagesOut float64   `json:"messagesOut"`
	BufferSize  int       `json:"bufferSize"`
	Timestamp   time.Time `json:"timestamp"`
}

func NewGroup(cfg stores.ArchiveGroupConfig, lastVal stores.MessageStore, archive stores.MessageArchive, logger *slog.Logger, closers ...func() error) *Group {
	g := &Group{
		cfg:      cfg,
		lastVal:  lastVal,
		archive:  archive,
		logger:   logger,
		flushCh:  make(chan struct{}, 1),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
		flushDur: 250 * time.Millisecond,
		closers:  closers,
	}

	qType := strings.ToUpper(strings.TrimSpace(cfg.QueueType))
	if qType == "" || qType == "NONE" {
		g.queue = nil
	} else if qType == "MEMORY" || qType == "DISK" {
		size := cfg.QueueSize
		if size <= 0 {
			size = 100000
		}
		blockSize := cfg.BulkSize
		if blockSize <= 0 {
			blockSize = 4000
		}
		pollTimeout := 250 * time.Millisecond
		if cfg.BulkTimeoutMs > 0 {
			pollTimeout = time.Duration(cfg.BulkTimeoutMs) * time.Millisecond
		}
		diskPath := strings.TrimSpace(cfg.QueueDiskPath)
		if diskPath == "" {
			diskPath = "data/queue"
		}
		if qType == "DISK" {
			q, err := queue.NewDiskQueue("archive", cfg.Name, logger, size, blockSize, pollTimeout, diskPath)
			if err != nil {
				if logger != nil {
					logger.Warn("failed to initialize disk queue for archive group; falling back to unbuffered", "group", cfg.Name, "err", err)
				}
			} else {
				g.queue = q
			}
		} else {
			g.queue = queue.NewMemoryQueue(logger, size, blockSize, pollTimeout)
		}
		if g.queue != nil && logger != nil {
			logger.Info("archive group store-and-forward queue initialized", "group", cfg.Name, "type", qType, "capacity", size)
		}
	}

	return g
}

func (g *Group) Name() string                      { return g.cfg.Name }
func (g *Group) Config() stores.ArchiveGroupConfig { return g.cfg }
func (g *Group) LastValue() stores.MessageStore    { return g.lastVal }
func (g *Group) Archive() stores.MessageArchive    { return g.archive }

// Start begins the background flush loop.
func (g *Group) Start() {
	go g.run()
}

func (g *Group) Stop() {
	g.stopOnce.Do(func() {
		close(g.stopCh)
		<-g.doneCh
		if g.queue != nil {
			if err := g.queue.Close(); err != nil && g.logger != nil {
				g.logger.Warn("archive group queue close failed", "group", g.cfg.Name, "err", err)
			}
		}
		for _, closeFn := range g.closers {
			if closeFn == nil {
				continue
			}
			if err := closeFn(); err != nil {
				if g.logger != nil {
					g.logger.Warn("archive group close failed", "group", g.cfg.Name, "err", err)
				}
			}
		}
	})
}

// Matches returns true if topic should be archived by this group.
func (g *Group) Matches(topic string, retain bool) bool {
	if !g.cfg.Enabled {
		return false
	}
	if g.cfg.RetainedOnly && !retain {
		return false
	}
	for _, f := range g.cfg.TopicFilters {
		if matchTopic(strings.TrimSpace(f), topic) {
			return true
		}
	}
	return false
}

func (g *Group) Submit(msg stores.BrokerMessage) {
	g.outCount.Add(1)
	if g.lastVal != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := g.lastVal.AddAll(ctx, []stores.BrokerMessage{msg}); err != nil && g.logger != nil {
			g.logger.Warn("archive lastval write failed", "group", g.cfg.Name, "err", err)
		}
		cancel()
	}
	if g.archive == nil {
		return
	}
	if g.queue != nil {
		g.queue.Add(msg)
		return
	}
	g.mu.Lock()
	g.pending = append(g.pending, msg)
	pendingLen := len(g.pending)
	g.mu.Unlock()
	if pendingLen >= 100 {
		select {
		case g.flushCh <- struct{}{}:
		default:
		}
	}
}

func (g *Group) BufferSize() int {
	if g.queue != nil {
		return g.queue.Size()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.pending)
}

func (g *Group) SampleMetrics(now time.Time, interval time.Duration) MetricsSnapshot {
	seconds := interval.Seconds()
	if seconds <= 0 {
		seconds = 1
	}
	snap := MetricsSnapshot{
		MessagesOut: float64(g.outCount.Swap(0)) / seconds,
		BufferSize:  g.BufferSize(),
		Timestamp:   now.UTC(),
	}
	g.metricsMu.Lock()
	g.latest = snap
	g.metricsMu.Unlock()
	return snap
}

func (g *Group) LatestMetrics() MetricsSnapshot {
	g.metricsMu.RLock()
	snap := g.latest
	g.metricsMu.RUnlock()
	snap.BufferSize = g.BufferSize()
	if snap.Timestamp.IsZero() {
		snap.Timestamp = time.Now().UTC()
	}
	return snap
}

func (g *Group) run() {
	t := time.NewTicker(g.flushDur)
	defer t.Stop()
	defer close(g.doneCh)
	for {
		select {
		case <-g.stopCh:
			g.flush()
			return
		case <-t.C:
			g.flush()
		case <-g.flushCh:
			g.flush()
		}
	}
}

func (g *Group) flush() {
	if g.queue != nil {
		g.flushQueue()
		return
	}
	g.mu.Lock()
	if len(g.pending) == 0 {
		g.mu.Unlock()
		return
	}
	batch := g.pending
	g.pending = nil
	g.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if g.archive != nil {
		if err := g.archive.AddHistory(ctx, batch); err != nil && g.logger != nil {
			g.logger.Warn("archive history write failed", "group", g.cfg.Name, "n", len(batch), "err", err)
		}
	}
}

func (g *Group) flushQueue() {
	var batch []stores.BrokerMessage
	count := g.queue.PollBlock(func(msg stores.BrokerMessage) {
		batch = append(batch, msg)
	})
	if count == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if g.archive != nil {
		if err := g.archive.AddHistory(ctx, batch); err != nil {
			if g.logger != nil {
				g.logger.Warn("archive history write failed; will retry via store-and-forward", "group", g.cfg.Name, "n", len(batch), "err", err)
			}
			return
		}
	}
	g.queue.PollCommit()
}

func matchTopic(pattern, topic string) bool {
	pp := strings.Split(pattern, "/")
	tt := strings.Split(topic, "/")
	for i, p := range pp {
		if p == "#" {
			return true
		}
		if i >= len(tt) {
			return false
		}
		if p == "+" {
			continue
		}
		if p != tt[i] {
			return false
		}
	}
	return len(pp) == len(tt)
}
