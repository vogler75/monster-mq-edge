package queue

import (
	"log/slog"
	"sync"
	"time"

	"monstermq.io/edge/internal/stores"
)

// MemoryQueue mirrors the JVM MessageQueueMemory implementation: it keeps a
// bounded in-memory queue and retries an uncommitted output block.
type MemoryQueue struct {
	logger      *slog.Logger
	queue       chan stores.BrokerMessage
	capacity    int
	blockSize   int
	pollTimeout time.Duration

	mu              sync.Mutex
	queueFull       bool
	droppedMessages uint64
	outputBlock     []stores.BrokerMessage
}

func NewMemoryQueue(logger *slog.Logger, queueSize, blockSize int, pollTimeout time.Duration) *MemoryQueue {
	if queueSize <= 0 {
		queueSize = 5000
	}
	if blockSize <= 0 || blockSize > queueSize {
		blockSize = min(100, queueSize)
	}
	if pollTimeout <= 0 {
		pollTimeout = 100 * time.Millisecond
	}
	return &MemoryQueue{
		logger:      logger,
		queue:       make(chan stores.BrokerMessage, queueSize),
		capacity:    queueSize,
		blockSize:   blockSize,
		pollTimeout: pollTimeout,
	}
}

func (q *MemoryQueue) IsQueueFull() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.queueFull
}

func (q *MemoryQueue) Size() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.queue) + len(q.outputBlock)
}

func (q *MemoryQueue) Capacity() int { return q.capacity }

func (q *MemoryQueue) Add(message stores.BrokerMessage) {
	select {
	case q.queue <- message:
		q.mu.Lock()
		if q.queueFull {
			q.queueFull = false
			size := len(q.queue) + len(q.outputBlock)
			dropped := q.droppedMessages
			q.mu.Unlock()
			if q.logger != nil {
				q.logger.Warn("queue not full anymore", "size", size, "capacity", q.Capacity(), "dropped", dropped)
			}
			return
		}
		q.mu.Unlock()
	default:
		q.mu.Lock()
		q.droppedMessages++
		dropped := q.droppedMessages
		wasFull := q.queueFull
		q.queueFull = true
		size := len(q.queue) + len(q.outputBlock)
		q.mu.Unlock()
		if q.logger != nil {
			if !wasFull {
				q.logger.Warn("queue is full; dropping messages", "size", size, "capacity", q.Capacity(), "topic", message.TopicName)
			} else if dropped%1000 == 0 {
				q.logger.Warn("queue still full; dropping messages", "size", size, "capacity", q.Capacity(), "dropped", dropped)
			}
		}
	}
}

func (q *MemoryQueue) PollBlock(handler func(stores.BrokerMessage)) int {
	q.mu.Lock()
	if len(q.outputBlock) > 0 {
		block := append([]stores.BrokerMessage(nil), q.outputBlock...)
		q.mu.Unlock()
		time.Sleep(time.Second)
		for _, message := range block {
			handler(message)
		}
		return len(block)
	}
	q.mu.Unlock()

	timer := time.NewTimer(q.pollTimeout)
	defer timer.Stop()

	var first stores.BrokerMessage
	select {
	case first = <-q.queue:
	case <-timer.C:
		return 0
	}

	q.mu.Lock()
	q.outputBlock = append(q.outputBlock, first)
	q.mu.Unlock()
	handler(first)

	for i := 1; i < q.blockSize; i++ {
		select {
		case message := <-q.queue:
			q.mu.Lock()
			q.outputBlock = append(q.outputBlock, message)
			q.mu.Unlock()
			handler(message)
		default:
			q.mu.Lock()
			size := len(q.outputBlock)
			q.mu.Unlock()
			return size
		}
	}
	q.mu.Lock()
	size := len(q.outputBlock)
	q.mu.Unlock()
	return size
}

func (q *MemoryQueue) PollCommit() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.outputBlock = nil
}

func (q *MemoryQueue) Close() error { return nil }
