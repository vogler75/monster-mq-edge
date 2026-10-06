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
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].Priority != list[j].Priority {
				return list[i].Priority > list[j].Priority
			}
			return isDenyRule(list[i]) && !isDenyRule(list[j])
		})
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

// AnonymousUser is the user record that holds the permissions and ACL rules
// for unauthenticated clients (same name as in the Kotlin broker).
const AnonymousUser = "Anonymous"

// Validate returns true if (username, password) match an enabled user.
func (c *Cache) Validate(username, password string) bool {
	if username == "" {
		if !c.anonymousAllow {
			return false
		}
		// An existing Anonymous user record decides, as in the Kotlin broker.
		if u, ok := c.lookup(AnonymousUser); ok {
			return u.Enabled
		}
		return true
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
// (write=false) to topic. ACL patterns containing %c never match, because no
// client ID is known; use AllowClient for MQTT clients.
//
// An unauthenticated caller (empty username) is checked against the Anonymous
// user record when it exists, like in the Kotlin broker; without that record
// anonymous access is all-or-nothing by AnonymousEnabled.
//
// When aclCheckOnSubscribe is false, subscribe-time checks (write=false with a
// wildcard topic) always pass. Delivery-time checks (write=false with a concrete
// topic) are still evaluated against ACL rules — the MQTT engine calls OnACLCheck in
// publishToClient with the actual topic before delivering each message.
func (c *Cache) Allow(username, topic string, write bool) bool {
	return c.AllowClient(username, "", topic, write)
}

// AllowClient is Allow with the MQTT client ID used for %c substitution in ACL
// patterns (%u is replaced with the username).
func (c *Cache) AllowClient(username, clientID, topic string, write bool) bool {
	if username == "" {
		if !c.anonymousAllow {
			return false
		}
		if _, ok := c.lookup(AnonymousUser); !ok {
			return true
		}
		username = AnonymousUser
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
	// Rules are checked in priority order (deny first on equal priority);
	// the first matching rule that decides wins. Same semantics as the Kotlin
	// AclCache:
	//   - canPublish=false and canSubscribe=false: deny rule for both
	//     operations.
	//   - otherwise: allow rule for the operations set to true; it is skipped
	//     for the other operation.
	// No deciding rule means deny. A wildcard subscription that only partly
	// overlaps a deny rule is admitted; the engine re-checks every delivered
	// message against its concrete topic, so denied topics are filtered out.
	for _, r := range rules {
		deny := isDenyRule(r)
		grants := r.CanSubscribe
		if write {
			grants = r.CanPublish
		}
		if !deny && !grants {
			continue
		}
		pattern, ok := resolvePattern(r.TopicPattern, username, clientID)
		if ok && topicMatches(pattern, topic) {
			return !deny
		}
	}
	return false
}

func isDenyRule(r stores.AclRule) bool {
	return !r.CanPublish && !r.CanSubscribe
}

func containsWildcard(topic string) bool {
	return strings.Contains(topic, "#") || strings.Contains(topic, "+")
}

// resolvePattern replaces %u with the username and %c with the client ID. It
// reports false when the pattern needs a client ID and none is known.
func resolvePattern(pattern, username, clientID string) (string, bool) {
	if !strings.Contains(pattern, "%") {
		return pattern, true
	}
	resolved := strings.ReplaceAll(pattern, "%u", username)
	if strings.Contains(resolved, "%c") {
		if clientID == "" {
			return "", false
		}
		resolved = strings.ReplaceAll(resolved, "%c", clientID)
	}
	return resolved, true
}

// topicMatches reports whether the ACL pattern covers topic, which is either a
// concrete topic or a subscription filter (then every topic the filter can
// match must be covered). Wildcards at the first level do not cover topics
// starting with '$' (MQTT 4.7.2). Same as AclCache.aclMatches in Kotlin.
func topicMatches(pattern, topic string) bool {
	pp := strings.Split(pattern, "/")
	tt := strings.Split(topic, "/")
	if strings.HasPrefix(tt[0], "$") && !strings.HasPrefix(pp[0], "$") && (pp[0] == "+" || pp[0] == "#") {
		return false
	}
	for i, p := range pp {
		if p == "#" {
			return true
		}
		if i >= len(tt) {
			return false
		}
		if p == "+" {
			// A single-level wildcard does not cover a multi-level filter.
			if tt[i] == "#" {
				return false
			}
			continue
		}
		if p != tt[i] {
			return false
		}
	}
	return len(pp) == len(tt)
}
