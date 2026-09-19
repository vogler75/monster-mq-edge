package scripting

import (
	"sync"
	"time"
)

// CircularLogBuffer maintains a fixed-size FIFO ring buffer of formatted log lines.
type CircularLogBuffer struct {
	mu       sync.RWMutex
	capacity int
	entries  []string
	head     int
	count    int
}

func NewCircularLogBuffer(capacity int) *CircularLogBuffer {
	if capacity <= 0 {
		capacity = 100
	}
	return &CircularLogBuffer{
		capacity: capacity,
		entries:  make([]string, capacity),
	}
}

func (b *CircularLogBuffer) Add(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	timestamp := time.Now().Format("15:04:05.000")
	b.entries[b.head] = timestamp + " " + line
	b.head = (b.head + 1) % b.capacity
	if b.count < b.capacity {
		b.count++
	}
}

func (b *CircularLogBuffer) Snapshot() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	out := make([]string, b.count)
	if b.count < b.capacity {
		copy(out, b.entries[:b.count])
		return out
	}

	// Ring buffer wrapped around
	idx := 0
	for i := b.head; i < b.capacity; i++ {
		out[idx] = b.entries[i]
		idx++
	}
	for i := 0; i < b.head; i++ {
		out[idx] = b.entries[i]
		idx++
	}
	return out
}

func (b *CircularLogBuffer) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.head = 0
	b.count = 0
}
