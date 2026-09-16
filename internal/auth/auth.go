package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"monstermq.io/edge/internal/stores"
)

// Cache holds users and ACL rules in memory and refreshes from the UserStore.
type Cache struct {
	store               stores.UserStore
	mu                  sync.RWMutex
	users               map[string]stores.User
	rulesByUser         map[string][]stores.AclRule
	anonymousAllow      bool
	aclCheckOnSubscribe bool
	sessions            map[string]session
}

type session struct {
	username     string
	passwordHash string
	expiresAt    time.Time
}

func NewCache(store stores.UserStore, anonymousAllow bool, aclCheckOnSubscribe bool) *Cache {
	return &Cache{
		store:               store,
		users:               map[string]stores.User{},
		rulesByUser:         map[string][]stores.AclRule{},
		anonymousAllow:      anonymousAllow,
		aclCheckOnSubscribe: aclCheckOnSubscribe,
		sessions:            map[string]session{},
	}
}

// Authenticate validates credentials and returns the enabled user.
func (c *Cache) Authenticate(ctx context.Context, username, password string) (*stores.User, bool) {
	if username == "" || password == "" {
		return nil, false
	}
	u, err := c.store.ValidateCredentials(ctx, username, password)
	if err != nil || u == nil || !u.Enabled {
		return nil, false
	}
	return u, true
}

// CreateSession creates an opaque, process-local bearer token. Sessions expire
// after 24 hours and are revalidated against the user cache on every request.
func (c *Cache) CreateSession(username string) (string, error) {
	u, ok := c.Lookup(username)
	if !ok || !u.Enabled {
		return "", fmt.Errorf("user is not enabled")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	c.mu.Lock()
	c.sessions[token] = session{username: username, passwordHash: u.PasswordHash, expiresAt: time.Now().Add(24 * time.Hour)}
	c.mu.Unlock()
	return token, nil
}

// ValidateSession resolves a bearer token to an enabled user.
func (c *Cache) ValidateSession(token string) (stores.User, bool) {
	c.mu.RLock()
	s, ok := c.sessions[token]
	u, userOK := c.users[s.username]
	c.mu.RUnlock()
	if !ok || !userOK || !u.Enabled || u.PasswordHash != s.passwordHash {
		return stores.User{}, false
	}
	if time.Now().After(s.expiresAt) {
		c.mu.Lock()
		delete(c.sessions, token)
		c.mu.Unlock()
		return stores.User{}, false
	}
	return u, true
}

func (c *Cache) Refresh(ctx context.Context) error {
	users, rules, err := c.store.LoadAll(ctx)
	if err != nil {
		return err
	}
	rulesByUser := map[string][]stores.AclRule{}
	for _, r := range rules {
		rulesByUser[r.Username] = append(rulesByUser[r.Username], r)
	}
	for u, list := range rulesByUser {
		sort.Slice(list, func(i, j int) bool { return list[i].Priority > list[j].Priority })
		rulesByUser[u] = list
	}
	usersMap := map[string]stores.User{}
	for _, u := range users {
		usersMap[u.Username] = u
	}
	c.mu.Lock()
	c.users = usersMap
	c.rulesByUser = rulesByUser
	c.mu.Unlock()
	return nil
}

// StartRefresher periodically reloads users/ACL from the underlying store.
func (c *Cache) StartRefresher(ctx context.Context, every time.Duration) {
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = c.Refresh(ctx)
			}
		}
	}()
}

// Validate returns true if (username, password) match an enabled user.
func (c *Cache) Validate(username, password string) bool {
	if username == "" {
		return c.anonymousAllow
	}
	u, ok := c.lookup(username)
	if !ok || !u.Enabled {
		return false
	}
	// Bcrypt verification goes through the store to avoid duplicating the cost.
	res, err := c.store.ValidateCredentials(context.Background(), username, password)
	return err == nil && res != nil
}

// Lookup returns the user from cache if present.
func (c *Cache) Lookup(username string) (stores.User, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	u, ok := c.users[username]
	return u, ok
}

func (c *Cache) lookup(username string) (stores.User, bool) {
	return c.Lookup(username)
}

// Allow returns true if the user is permitted to publish (write=true) or subscribe
// (write=false) to topic.
//
// When aclCheckOnSubscribe is false, subscribe-time checks (write=false with a
// wildcard topic) always pass. Delivery-time checks (write=false with a concrete
// topic) are still evaluated against ACL rules — mochi calls OnACLCheck in
// publishToClient with the actual topic before delivering each message.
func (c *Cache) Allow(username, topic string, write bool) bool {
	if username == "" {
		return c.anonymousAllow
	}
	u, ok := c.lookup(username)
	if !ok || !u.Enabled {
		return false
	}
	if u.IsAdmin {
		return true
	}
	if write && !u.CanPublish {
		return false
	}
	if !write && !u.CanSubscribe {
		return false
	}
	// When AclCheckOnSubscription is false and this is a read/subscribe check
	// with a wildcard filter, skip ACL enforcement here. Delivery-time checks
	// (concrete topics) will still be enforced below.
	if !write && !c.aclCheckOnSubscribe && containsWildcard(topic) {
		return true
	}
	c.mu.RLock()
	rules := c.rulesByUser[username]
	c.mu.RUnlock()
	if len(rules) == 0 {
		return true
	}
	for _, r := range rules {
		if topicMatches(r.TopicPattern, topic) {
			if write {
				return r.CanPublish
			}
			return r.CanSubscribe
		}
	}
	return false
}

func containsWildcard(topic string) bool {
	return strings.Contains(topic, "#") || strings.Contains(topic, "+")
}

func topicMatches(pattern, topic string) bool {
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
