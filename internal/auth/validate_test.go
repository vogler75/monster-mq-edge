package auth

import (
	"context"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"monstermq.io/edge/internal/stores"
)

// ValidateCredentials mirrors the real stores: bcrypt against the stored hash.
func (f *fakeUserStore) ValidateCredentials(_ context.Context, username, password string) (*stores.User, error) {
	for _, u := range f.users {
		if u.Username == username && u.Enabled {
			if u.PasswordHash == "" {
				if password == "" {
					return &u, nil
				}
				continue
			}
			if compareHash([]byte(u.PasswordHash), []byte(password)) == nil {
				return &u, nil
			}
		}
	}
	return nil, nil
}

func hashPW(t testing.TB, pw string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}

// countBcrypt counts bcrypt comparisons for the rest of the test.
func countBcrypt(t *testing.T) *int {
	n := 0
	orig := compareHash
	compareHash = func(h, p []byte) error { n++; return orig(h, p) }
	t.Cleanup(func() { compareHash = orig })
	return &n
}

func newValidateCache(t *testing.T) (*Cache, *fakeUserStore) {
	t.Helper()
	store := &fakeUserStore{users: []stores.User{{Username: "alice", PasswordHash: hashPW(t, "secret"), Enabled: true}}}
	c := NewCache(store, false, true)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c, store
}

func TestValidateSkipsBcryptOnRepeat(t *testing.T) {
	c, _ := newValidateCache(t)
	n := countBcrypt(t)
	if !c.Validate("alice", "secret") || !c.Validate("alice", "secret") {
		t.Fatal("correct password rejected")
	}
	if *n != 1 {
		t.Errorf("bcrypt ran %d times, want 1", *n)
	}
}

func TestValidateWrongPasswordAfterCachedSuccess(t *testing.T) {
	c, _ := newValidateCache(t)
	c.Validate("alice", "secret")
	n := countBcrypt(t)
	if c.Validate("alice", "wrong") || c.Validate("alice", "wrong") {
		t.Fatal("wrong password accepted")
	}
	if *n != 2 {
		t.Errorf("bcrypt ran %d times, want 2 (failures are not cached)", *n)
	}
	if !c.Validate("alice", "secret") {
		t.Error("correct password rejected after failed attempts")
	}
}

func TestValidatePasswordChange(t *testing.T) {
	c, store := newValidateCache(t)
	c.Validate("alice", "secret")
	store.users[0].PasswordHash = hashPW(t, "new")
	_ = c.Refresh(context.Background())
	if c.Validate("alice", "secret") {
		t.Error("old password accepted after change")
	}
	if !c.Validate("alice", "new") {
		t.Error("new password rejected")
	}
}

func TestValidateDisabledUser(t *testing.T) {
	c, store := newValidateCache(t)
	c.Validate("alice", "secret")
	store.users[0].Enabled = false
	_ = c.Refresh(context.Background())
	if c.Validate("alice", "secret") {
		t.Error("disabled user accepted")
	}
}

func TestValidateDeletedUser(t *testing.T) {
	c, store := newValidateCache(t)
	c.Validate("alice", "secret")
	store.users = nil
	_ = c.Refresh(context.Background())
	if c.Validate("alice", "secret") {
		t.Error("deleted user accepted")
	}
	if len(c.verified) != 0 {
		t.Errorf("verified entries kept for deleted user: %d", len(c.verified))
	}
}

func TestValidateAnonymous(t *testing.T) {
	c, _ := newValidateCache(t)
	if c.Validate("", "") {
		t.Error("anonymous accepted with anonymousAllow=false")
	}
	c.anonymousAllow = true
	if !c.Validate("", "") {
		t.Error("anonymous rejected with anonymousAllow=true")
	}
}

func BenchmarkValidateWarm(b *testing.B) {
	store := &fakeUserStore{users: []stores.User{{Username: "alice", PasswordHash: hashPW(b, "secret"), Enabled: true}}}
	c := NewCache(store, false, true)
	_ = c.Refresh(context.Background())
	c.Validate("alice", "secret")
	for i := 0; i < b.N; i++ {
		c.Validate("alice", "secret")
	}
}

func BenchmarkBcryptCost10(b *testing.B) {
	h, _ := bcrypt.GenerateFromPassword([]byte("secret"), 10)
	for i := 0; i < b.N; i++ {
		_ = bcrypt.CompareHashAndPassword(h, []byte("secret"))
	}
}

// TestAnonymousUserRecordNeedsAnonymousEnabled: the default Anonymous record
// has no password, so logging in by its name must follow AnonymousEnabled.
func TestAnonymousUserRecordNeedsAnonymousEnabled(t *testing.T) {
	ctx := context.Background()
	store := &fakeUserStore{users: []stores.User{
		{Username: AnonymousUser, Enabled: true},
		{Username: "bob", Enabled: true},
	}}
	c := NewCache(store, false, true)
	if err := c.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if c.Validate(AnonymousUser, "") {
		t.Error("Validate: Anonymous accepted with anonymousAllow=false")
	}
	if _, ok := c.Authenticate(ctx, AnonymousUser, ""); ok {
		t.Error("Authenticate: Anonymous accepted with anonymousAllow=false")
	}
	if !c.Validate("bob", "") {
		t.Error("Validate: user with empty password hash rejected")
	}
	if _, ok := c.Authenticate(ctx, "bob", ""); !ok {
		t.Error("Authenticate: user with empty password hash rejected")
	}

	c.anonymousAllow = true
	if !c.Validate(AnonymousUser, "") {
		t.Error("Validate: Anonymous rejected with anonymousAllow=true")
	}
	if _, ok := c.Authenticate(ctx, AnonymousUser, ""); !ok {
		t.Error("Authenticate: Anonymous rejected with anonymousAllow=true")
	}
}
