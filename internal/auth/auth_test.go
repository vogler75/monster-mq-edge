package auth

import (
	"context"
	"testing"

	"monstermq.io/edge/internal/stores"
)

type aclCase struct {
	topic string
	write bool
	want  bool
}

func newTestCache(aclCheckOnSubscribe bool, rules ...stores.AclRule) *Cache {
	c := NewCache(nil, false, aclCheckOnSubscribe)
	c.users = map[string]stores.User{
		"alice": {Username: "alice", Enabled: true, CanPublish: true, CanSubscribe: true},
	}
	c.rulesByUser = map[string][]stores.AclRule{"alice": rules}
	return c
}

func runACLCases(t *testing.T, c *Cache, cases []aclCase) {
	t.Helper()
	for _, tc := range cases {
		if got := c.Allow("alice", tc.topic, tc.write); got != tc.want {
			t.Errorf("Allow(alice, %q, write=%v) = %v, want %v", tc.topic, tc.write, got, tc.want)
		}
	}
}

// A rule that grants only one operation must not block the other operation
// from being granted by a lower-priority rule.
func TestAllowSingleOperationRulesDoNotCrossContaminate(t *testing.T) {
	c := newTestCache(true,
		stores.AclRule{TopicPattern: "telemetry/#", CanPublish: true, Priority: 30},
		stores.AclRule{TopicPattern: "commands/#", CanSubscribe: true, Priority: 20},
		stores.AclRule{TopicPattern: "#", CanSubscribe: true, Priority: 10},
	)
	runACLCases(t, c, []aclCase{
		{"telemetry/a", true, true},
		{"telemetry/a", false, true}, // granted by lower-priority "#" subscribe rule
		{"commands/a", false, true},
		{"commands/a", true, false}, // no publish rule covers commands/#
		{"other/a", false, true},
		{"other/a", true, false},
	})
}

// A rule with both flags false is a deny rule that overrides lower-priority
// allow rules.
func TestAllowDenyRule(t *testing.T) {
	c := newTestCache(true,
		stores.AclRule{TopicPattern: "secret/public/#", CanPublish: true, CanSubscribe: true, Priority: 200},
		stores.AclRule{TopicPattern: "secret/#", Priority: 100},
		stores.AclRule{TopicPattern: "#", CanPublish: true, CanSubscribe: true, Priority: 1},
	)
	runACLCases(t, c, []aclCase{
		{"data/x", true, true},
		{"data/x", false, true},
		{"secret/x", true, false},
		{"secret/x", false, false},
		{"secret", false, false},         // "secret/#" also matches the parent level
		{"secret/public/x", true, true},  // higher-priority allow wins
		{"secret/public/x", false, true}, // higher-priority allow wins
		{"secret/#", false, false},       // filter inside the denied subtree
		{"secret/+", false, false},       // filter inside the denied subtree
		{"secret/public/#", false, true}, // covered by the higher-priority allow
		{"#", false, true},               // admitted; secret/ is filtered on delivery
		{"+/x", false, true},             // admitted; secret/x is filtered on delivery
	})
}

// Deny-only operation: subscribe allowed, publish denied on a subtree that a
// lower-priority rule grants read-write.
func TestAllowDenyPublishOnly(t *testing.T) {
	c := newTestCache(true,
		stores.AclRule{TopicPattern: "machine/#", CanSubscribe: true, Priority: 20},
		stores.AclRule{TopicPattern: "machine/#", Priority: 10},
		stores.AclRule{TopicPattern: "#", CanPublish: true, CanSubscribe: true, Priority: 1},
	)
	runACLCases(t, c, []aclCase{
		{"machine/x", false, true},
		{"machine/x", true, false},
		{"other/x", true, true},
	})
}

// On equal priority the deny rule is evaluated first.
func TestAllowDenyWinsOnEqualPriority(t *testing.T) {
	rules := []stores.AclRule{
		{Username: "alice", TopicPattern: "#", CanPublish: true, CanSubscribe: true, Priority: 5},
		{Username: "alice", TopicPattern: "secret/#", Priority: 5},
	}
	store := &fakeUserStore{
		users: []stores.User{{Username: "alice", Enabled: true, CanPublish: true, CanSubscribe: true}},
		rules: rules,
	}
	c := NewCache(store, false, true)
	if err := c.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	runACLCases(t, c, []aclCase{
		{"secret/x", true, false},
		{"data/x", true, true},
	})
}

// With AclCheckOnSubscription=false, wildcard subscriptions are admitted and
// the concrete topic is checked on delivery.
func TestAllowDenyRuleWithoutSubscribeCheck(t *testing.T) {
	c := newTestCache(false,
		stores.AclRule{TopicPattern: "secret/#", Priority: 100},
		stores.AclRule{TopicPattern: "#", CanPublish: true, CanSubscribe: true, Priority: 1},
	)
	runACLCases(t, c, []aclCase{
		{"#", false, true},
		{"secret/x", false, false},
		{"data/x", false, true},
	})
}

type fakeUserStore struct {
	stores.UserStore
	users []stores.User
	rules []stores.AclRule
}

func (f *fakeUserStore) LoadAll(context.Context) ([]stores.User, []stores.AclRule, error) {
	return f.users, f.rules, nil
}

func TestAllowPlaceholders(t *testing.T) {
	c := newTestCache(true,
		stores.AclRule{TopicPattern: "devices/%c/#", CanPublish: true, CanSubscribe: true, Priority: 10},
		stores.AclRule{TopicPattern: "users/%u/status", CanSubscribe: true, Priority: 10},
	)
	if !c.AllowClient("alice", "sensor-42", "devices/sensor-42/data", true) {
		t.Error("client ID placeholder should match the client ID")
	}
	if c.AllowClient("alice", "sensor-42", "devices/sensor-99/data", true) {
		t.Error("client ID placeholder must not match another client ID")
	}
	if c.Allow("alice", "devices/sensor-42/data", true) {
		t.Error("client ID placeholder must not match without a client ID")
	}
	if !c.Allow("alice", "users/alice/status", false) || c.Allow("alice", "users/bob/status", false) {
		t.Error("username placeholder should match only the username")
	}
}

func TestAllowFilterCoverage(t *testing.T) {
	c := newTestCache(true,
		stores.AclRule{TopicPattern: "a/+", CanSubscribe: true, Priority: 10},
		stores.AclRule{TopicPattern: "#", CanPublish: true, Priority: 1},
	)
	runACLCases(t, c, []aclCase{
		{"a/b", false, true},
		{"a/+", false, true},
		{"a/#", false, false},   // a/+ does not cover a/b/c
		{"$SYS/x", true, false}, // "#" does not cover $-topics
		{"data/x", true, true},
	})
}

func TestAllowAnonymousUserRecord(t *testing.T) {
	c := NewCache(nil, true, true)
	if !c.Validate("", "") || !c.Allow("", "any/topic", true) {
		t.Fatal("without an Anonymous record anonymous access follows AnonymousEnabled")
	}
	c.users = map[string]stores.User{
		AnonymousUser: {Username: AnonymousUser, Enabled: true, CanSubscribe: true},
	}
	c.rulesByUser = map[string][]stores.AclRule{
		AnonymousUser: {{TopicPattern: "public/#", CanSubscribe: true, Priority: 1}},
	}
	if !c.Validate("", "") {
		t.Error("enabled Anonymous record should allow connecting")
	}
	if !c.Allow("", "public/x", false) || c.Allow("", "private/x", false) || c.Allow("", "public/x", true) {
		t.Error("Anonymous record permissions and ACL rules should apply")
	}
	c.users[AnonymousUser] = stores.User{Username: AnonymousUser, Enabled: false, CanSubscribe: true}
	if c.Validate("", "") || c.Allow("", "public/x", false) {
		t.Error("disabled Anonymous record should deny")
	}
	if off := NewCache(nil, false, true); off.Validate("", "") || off.Allow("", "public/x", false) {
		t.Error("AnonymousEnabled=false should deny regardless of the record")
	}
}
