package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	"monstermq.io/edge/internal/stores"
)

// compareHash is bcrypt.CompareHashAndPassword; tests swap it to count calls.
var compareHash = bcrypt.CompareHashAndPassword

// Cache holds users and ACL rules in memory and refreshes from the UserStore.
type Cache struct {
	store               stores.UserStore
	mu                  sync.RWMutex
	users               map[string]stores.User
	rulesByUser         map[string][]stores.AclRule
	decisions           map[aclKey]bool // memoized AllowClient verdicts, reset by Refresh
	decisionsGen        uint64          // bumped by Refresh so in-flight checks don't memoize stale verdicts
	anonymousAllow      bool
	aclCheckOnSubscribe bool
	sessions            map[string]session
	canonical           func(topic string) (string, bool)
	verified            map[string]verifiedCred
}

// verifiedCred remembers a password that passed bcrypt for the given hash.
type verifiedCred struct {
	passwordHash string
	secret       [32]byte // sha256(password)
}

type aclKey struct {
	username, clientID, topic string
	write                     bool
}

// maxACLDecisions bounds the memo; a client publishing to random topics would
// otherwise grow it without limit. Full means reset, not eviction. Keys longer
// than maxACLMemoKeyLen are not memoized, so the cap also bounds bytes (~16 MiB).
const (
	maxACLDecisions  = 1 << 16
	maxACLMemoKeyLen = 256
)

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
		verified:            map[string]verifiedCred{},
	}
}

// Authenticate validates credentials and returns the enabled user.
func (c *Cache) Authenticate(ctx context.Context, username, password string) (*stores.User, bool) {
	if username == "" || (username == AnonymousUser && !c.IsAnonymousEnabled()) {
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
	c.decisions = nil
	c.decisionsGen++
	c.users = usersMap
	c.rulesByUser = rulesByUser
	for name, v := range c.verified {
		if usersMap[name].PasswordHash != v.passwordHash {
			delete(c.verified, name)
		}
	}
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

// IsAnonymousEnabled reports whether anonymous access is allowed.
// If an Anonymous user record exists in the user cache, its Enabled field decides.
// Without an Anonymous record, it follows the configured anonymousAllow setting.
func (c *Cache) IsAnonymousEnabled() bool {
	if !c.anonymousAllow {
		return false
	}
	if u, ok := c.lookup(AnonymousUser); ok {
		return u.Enabled
	}
	return true
}

// Validate returns true if (username, password) match an enabled user.
// The Anonymous user has no password, so it follows IsAnonymousEnabled.
func (c *Cache) Validate(username, password string) bool {
	if username == "" || username == AnonymousUser {
		return c.IsAnonymousEnabled()
	}
	u, ok := c.lookup(username)
	if !ok || !u.Enabled {
		return false
	}
	if u.PasswordHash == "" {
		return password == ""
	}
	// A password that already passed bcrypt against this exact hash skips it.
	// A changed, deleted or disabled user changes c.users on Refresh, so the
	// stored hash no longer matches (same idea as ValidateSession).
	secret := sha256.Sum256([]byte(password))
	c.mu.RLock()
	v, hit := c.verified[username]
	c.mu.RUnlock()
	if hit && v.passwordHash == u.PasswordHash && subtle.ConstantTimeCompare(secret[:], v.secret[:]) == 1 {
		return true
	}
	if compareHash([]byte(u.PasswordHash), []byte(password)) != nil {
		return false
	}
	c.mu.Lock()
	c.verified[username] = verifiedCred{passwordHash: u.PasswordHash, secret: secret}
	c.mu.Unlock()
	return true
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

// SetCanonical installs the alias mapping Allow and AllowClient apply so an
// alias (e.g. a WinCC OA type path) never widens access.
func (c *Cache) SetCanonical(f func(topic string) (string, bool)) {
	c.mu.Lock()
	c.canonical = f
	c.decisions = nil
	c.decisionsGen++
	c.mu.Unlock()
}

// AllowClient is Allow with the client ID used for %c substitution in ACL
// patterns (%u is replaced with the username).
//
// Verdicts are memoized until the next Refresh: with AclCheckOnSubscription
// off, every delivery to every subscriber lands here.
func (c *Cache) AllowClient(username, clientID, topic string, write bool) bool {
	key := aclKey{username, clientID, topic, write}
	c.mu.RLock()
	ok, hit := c.decisions[key]
	gen := c.decisionsGen
	canonical := c.canonical
	c.mu.RUnlock()
	if hit {
		return ok
	}
	ok = c.allowClient(username, clientID, topic, write)
	// An alias (e.g. a WinCC OA type path) must never widen access: its
	// canonical form has to be allowed too, as for MQTT clients.
	if ok && canonical != nil {
		if ct, mapped := canonical(topic); mapped && ct != topic {
			ok = c.allowClient(username, clientID, ct, write)
		}
	}
	c.mu.Lock()
	if c.decisionsGen == gen && len(topic)+len(clientID)+len(username) <= maxACLMemoKeyLen {
		if c.decisions == nil || len(c.decisions) >= maxACLDecisions {
			c.decisions = map[aclKey]bool{}
		}
		c.decisions[key] = ok
	}
	c.mu.Unlock()
	return ok
}

func (c *Cache) allowClient(username, clientID, topic string, write bool) bool {
	if username == "" {
		if !c.IsAnonymousEnabled() {
			return false
		}
		if _, ok := c.lookup(AnonymousUser); ok {
			username = AnonymousUser
		} else {
			return true
		}
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
