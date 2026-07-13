package zenoh

import (
	"sync"
	"time"
)

type SeenCache struct {
	mu       sync.Mutex
	maxSize  int
	ttl      time.Duration
	messages map[string]time.Time
}

func NewSeenCache(maxSize int, ttlSeconds int64) *SeenCache {
	if maxSize <= 0 {
		maxSize = 100000
	}
	if ttlSeconds <= 0 {
		ttlSeconds = 300
	}
	return &SeenCache{
		maxSize:  maxSize,
		ttl:      time.Duration(ttlSeconds) * time.Second,
		messages: make(map[string]time.Time),
	}
}

// Remember checks if the uuid was already seen.
// If it has not been seen, it registers it and returns true.
// If it has been seen, it returns false.
func (c *SeenCache) Remember(uuid string) bool {
	if uuid == "" {
		return true // don't block empty UUIDs but don't track them either
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()

	// Clean up expired entries
	for k, v := range c.messages {
		if now.Sub(v) > c.ttl {
			delete(c.messages, k)
		}
	}

	if _, ok := c.messages[uuid]; ok {
		return false
	}

	// Enforce max size (evict random elements to shrink if still too large)
	if len(c.messages) >= c.maxSize {
		for k := range c.messages {
			delete(c.messages, k)
			if len(c.messages) < c.maxSize {
				break
			}
		}
	}

	c.messages[uuid] = now
	return true
}
