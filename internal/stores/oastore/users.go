package oastore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"monstermq.io/edge/internal/oahost"
	"monstermq.io/edge/internal/stores"
)

// UserType is the datapoint type of MQTT users: one datapoint per user,
// named MMQUsers_k<hash of the user name>, holding the user's ACL rules too.
const UserType = "MMQUsers"

// Elements of MMQUsers.
var userElements = []string{"user", "passwordHash", "enabled", "canSubscribe", "canPublish", "isAdmin", "acl", "created", "updated"}

func userElementKinds() []uint32 {
	str, bl, tim := uint32(oahost.KindString), uint32(oahost.KindBool), uint32(oahost.KindTime)
	return []uint32{
		str, // user: user name (the DP name is a hash)
		str, // passwordHash: bcrypt hash
		bl,  // enabled
		bl,  // canSubscribe
		bl,  // canPublish
		bl,  // isAdmin
		str, // acl: JSON list of the user's ACL rules
		tim, // created
		tim, // updated
	}
}

// aclRecord is one ACL rule in the acl element.
type aclRecord struct {
	ID        string    `json:"id"`
	Topic     string    `json:"topic"`
	Subscribe bool      `json:"subscribe"`
	Publish   bool      `json:"publish"`
	Priority  int       `json:"priority"`
	Created   time.Time `json:"created"`
}

var (
	ErrUserExists   = errors.New("oastore: user already exists")
	ErrUserNotFound = errors.New("oastore: user not found")
	ErrRuleNotFound = errors.New("oastore: ACL rule not found")
)

// UserStore implements stores.UserStore on MMQUsers datapoints. All users
// are loaded once and kept in memory; writes go through to WinCC OA and
// are confirmed before they count.
type UserStore struct {
	api     oahost.API
	timeout time.Duration
	logger  *slog.Logger

	wmu    sync.Mutex // serializes writes
	mu     sync.RWMutex
	loaded bool
	users  map[string]*userEntry // user name -> entry
}

type userEntry struct {
	dp    string
	user  stores.User
	rules []aclRecord
}

var _ stores.UserStore = (*UserStore)(nil)

func newUserStore(api oahost.API, timeout time.Duration, logger *slog.Logger) *UserStore {
	return &UserStore{api: api, timeout: timeout, logger: logger, users: map[string]*userEntry{}}
}

func userDP(name string) string { return DPName(UserType, "user", name) }

func (s *UserStore) Close() error                          { return nil }
func (s *UserStore) EnsureTable(ctx context.Context) error { return s.Load(ctx) }

// Load reads every MMQUsers datapoint once. Datapoints whose user name does
// not match their name, or with an unreadable ACL, are skipped and left
// untouched.
func (s *UserStore) Load(parent context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, 10*s.timeout)
	defer cancel()
	names, err := s.api.DpNames(ctx, UserType+"_*", UserType, s.timeout)
	if err != nil {
		return fmt.Errorf("oastore: enumerate %s: %w", UserType, err)
	}
	for i := 0; i < len(names); i += enumBatch / len(userElements) {
		batch := names[i:min(i+enumBatch/len(userElements), len(names))]
		addrs := make([]string, 0, len(batch)*len(userElements))
		for _, n := range batch {
			for _, el := range userElements {
				addrs = append(addrs, n+"."+el+":_online.._value")
			}
		}
		vals, err := s.api.DpGet(ctx, addrs, s.timeout)
		if err != nil {
			return fmt.Errorf("oastore: read %s: %w", UserType, err)
		}
		for j, n := range batch {
			dp := stripSystem(n)
			v := vals[j*len(userElements) : (j+1)*len(userElements)]
			name := v[0].Str
			if name == "" {
				continue // created but never committed
			}
			if userDP(name) != dp {
				s.logger.Warn("oastore: user datapoint name does not match its user; ignored", "dp", dp, "user", name)
				continue
			}
			var rules []aclRecord
			if v[6].Str != "" {
				if err := json.Unmarshal([]byte(v[6].Str), &rules); err != nil {
					s.logger.Warn("oastore: unreadable ACL of user; user ignored", "dp", dp, "user", name, "err", err)
					continue
				}
			}
			s.users[name] = &userEntry{dp: dp, rules: rules, user: stores.User{
				Username: name, PasswordHash: v[1].Str, Enabled: v[2].Bool, CanSubscribe: v[3].Bool,
				CanPublish: v[4].Bool, IsAdmin: v[5].Bool, CreatedAt: v[7].Time, UpdatedAt: v[8].Time,
			}}
		}
	}
	s.loaded = true
	return nil
}

func (s *UserStore) ensureLoaded(ctx context.Context) error {
	s.mu.RLock()
	loaded := s.loaded
	s.mu.RUnlock()
	if loaded {
		return nil
	}
	return s.Load(ctx)
}

// write stores e completely (creating the datapoint when create is set)
// and replaces the cached entry after WinCC OA confirmed it.
func (s *UserStore) write(parent context.Context, e *userEntry, create bool) error {
	ctx, cancel := context.WithTimeout(parent, s.timeout)
	defer cancel()
	if create {
		// Lifecycle calls take the DP name without a trailing dot.
		if err := s.api.DpCreate(ctx, e.dp, UserType, s.timeout); err != nil && !isExists(err) {
			return fmt.Errorf("%w: create %s for user %s: %v", oahost.ErrPersist, e.dp, e.user.Username, err)
		}
	}
	if e.rules == nil {
		e.rules = []aclRecord{}
	}
	acl, err := json.Marshal(e.rules)
	if err != nil {
		return err
	}
	u := e.user
	names := make([]string, len(userElements))
	for i, el := range userElements {
		names[i] = e.dp + "." + el + ":_original.._value"
	}
	values := []oahost.Value{
		{Kind: oahost.KindString, Str: u.Username},
		{Kind: oahost.KindString, Str: u.PasswordHash},
		{Kind: oahost.KindBool, Bool: u.Enabled},
		{Kind: oahost.KindBool, Bool: u.CanSubscribe},
		{Kind: oahost.KindBool, Bool: u.CanPublish},
		{Kind: oahost.KindBool, Bool: u.IsAdmin},
		{Kind: oahost.KindString, Str: string(acl)},
		{Kind: oahost.KindTime, Time: u.CreatedAt},
		{Kind: oahost.KindTime, Time: u.UpdatedAt},
	}
	if err := s.api.DpSet(ctx, names, values, s.timeout); err != nil {
		return fmt.Errorf("%w: write %s for user %s: %v", oahost.ErrPersist, e.dp, u.Username, err)
	}
	s.mu.Lock()
	s.users[u.Username] = e
	s.mu.Unlock()
	return nil
}

// entry returns a copy of the cached entry of name (nil if absent).
func (s *UserStore) entry(name string) *userEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e := s.users[name]
	if e == nil {
		return nil
	}
	c := *e
	c.rules = append([]aclRecord(nil), e.rules...)
	return &c
}

func (s *UserStore) CreateUser(ctx context.Context, u stores.User) error {
	if err := s.ensureLoaded(ctx); err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.entry(u.Username) != nil {
		return fmt.Errorf("%w: %s", ErrUserExists, u.Username)
	}
	now := time.Now().UTC()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	u.UpdatedAt = now
	return s.write(ctx, &userEntry{dp: userDP(u.Username), user: u}, true)
}

func (s *UserStore) UpdateUser(ctx context.Context, u stores.User) error {
	if err := s.ensureLoaded(ctx); err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	e := s.entry(u.Username)
	if e == nil {
		return fmt.Errorf("%w: %s", ErrUserNotFound, u.Username)
	}
	u.CreatedAt = e.user.CreatedAt
	u.UpdatedAt = time.Now().UTC()
	e.user = u
	return s.write(ctx, e, false)
}

// DeleteUser removes the user's datapoint and with it the user's ACL rules.
func (s *UserStore) DeleteUser(ctx context.Context, username string) error {
	if err := s.ensureLoaded(ctx); err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	e := s.entry(username)
	if e == nil {
		return nil
	}
	dctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	if err := s.api.DpDelete(dctx, e.dp, s.timeout); err != nil && !errors.Is(err, oahost.ErrNotFound) {
		return fmt.Errorf("%w: delete %s for user %s: %v", oahost.ErrPersist, e.dp, username, err)
	}
	s.mu.Lock()
	delete(s.users, username)
	s.mu.Unlock()
	return nil
}

func (s *UserStore) GetUser(ctx context.Context, username string) (*stores.User, error) {
	if err := s.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	e := s.entry(username)
	if e == nil {
		return nil, nil
	}
	u := e.user
	return &u, nil
}

func (s *UserStore) GetAllUsers(ctx context.Context) ([]stores.User, error) {
	if err := s.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	s.mu.RLock()
	out := make([]stores.User, 0, len(s.users))
	for _, e := range s.users {
		out = append(out, e.user)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out, nil
}

func (s *UserStore) ValidateCredentials(ctx context.Context, username, password string) (*stores.User, error) {
	u, err := s.GetUser(ctx, username)
	if err != nil || u == nil || !u.Enabled {
		return nil, err
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return nil, nil
	}
	return u, nil
}

func (s *UserStore) CreateAclRule(ctx context.Context, r stores.AclRule) error {
	if err := s.ensureLoaded(ctx); err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	e := s.entry(r.Username)
	if e == nil {
		return fmt.Errorf("%w: %s", ErrUserNotFound, r.Username)
	}
	e.rules = append(e.rules, aclRecord{
		ID: uuid.NewString(), Topic: r.TopicPattern, Subscribe: r.CanSubscribe, Publish: r.CanPublish,
		Priority: r.Priority, Created: time.Now().UTC(),
	})
	return s.write(ctx, e, false)
}

// ruleOwner finds the user holding rule id (caller holds s.wmu).
func (s *UserStore) ruleOwner(id string) (*userEntry, int) {
	s.mu.RLock()
	var owner string
	for name, e := range s.users {
		for _, r := range e.rules {
			if r.ID == id {
				owner = name
			}
		}
	}
	s.mu.RUnlock()
	if owner == "" {
		return nil, -1
	}
	e := s.entry(owner)
	for i, r := range e.rules {
		if r.ID == id {
			return e, i
		}
	}
	return nil, -1
}

// UpdateAclRule changes a rule; a changed user name moves it to that user.
func (s *UserStore) UpdateAclRule(ctx context.Context, r stores.AclRule) error {
	if err := s.ensureLoaded(ctx); err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	from, i := s.ruleOwner(r.ID)
	if from == nil {
		return fmt.Errorf("%w: %s", ErrRuleNotFound, r.ID)
	}
	rec := from.rules[i]
	rec.Topic, rec.Subscribe, rec.Publish, rec.Priority = r.TopicPattern, r.CanSubscribe, r.CanPublish, r.Priority
	if r.Username == "" || r.Username == from.user.Username {
		from.rules[i] = rec
		return s.write(ctx, from, false)
	}
	to := s.entry(r.Username)
	if to == nil {
		return fmt.Errorf("%w: %s", ErrUserNotFound, r.Username)
	}
	to.rules = append(to.rules, rec)
	if err := s.write(ctx, to, false); err != nil {
		return err
	}
	from.rules = append(from.rules[:i], from.rules[i+1:]...)
	return s.write(ctx, from, false)
}

func (s *UserStore) DeleteAclRule(ctx context.Context, id string) error {
	if err := s.ensureLoaded(ctx); err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	e, i := s.ruleOwner(id)
	if e == nil {
		return nil
	}
	e.rules = append(e.rules[:i], e.rules[i+1:]...)
	return s.write(ctx, e, false)
}

func toAclRule(user string, r aclRecord) stores.AclRule {
	return stores.AclRule{ID: r.ID, Username: user, TopicPattern: r.Topic, CanSubscribe: r.Subscribe,
		CanPublish: r.Publish, Priority: r.Priority, CreatedAt: r.Created}
}

// rulesOf returns the rules of a user ordered by priority (highest first),
// then by creation, like the SQL stores.
func rulesOf(user string, recs []aclRecord) []stores.AclRule {
	out := make([]stores.AclRule, 0, len(recs))
	for _, r := range recs {
		out = append(out, toAclRule(user, r))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority > out[j].Priority })
	return out
}

func (s *UserStore) GetUserAclRules(ctx context.Context, username string) ([]stores.AclRule, error) {
	if err := s.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	e := s.entry(username)
	if e == nil {
		return []stores.AclRule{}, nil
	}
	return rulesOf(username, e.rules), nil
}

func (s *UserStore) GetAllAclRules(ctx context.Context) ([]stores.AclRule, error) {
	users, err := s.GetAllUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := []stores.AclRule{}
	for _, u := range users {
		if e := s.entry(u.Username); e != nil {
			out = append(out, rulesOf(u.Username, e.rules)...)
		}
	}
	return out, nil
}

func (s *UserStore) LoadAll(ctx context.Context) ([]stores.User, []stores.AclRule, error) {
	users, err := s.GetAllUsers(ctx)
	if err != nil {
		return nil, nil, err
	}
	rules, err := s.GetAllAclRules(ctx)
	if err != nil {
		return nil, nil, err
	}
	return users, rules, nil
}
