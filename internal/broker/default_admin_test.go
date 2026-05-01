package broker

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"monstermq.io/edge/internal/config"
	storesqlite "monstermq.io/edge/internal/stores/sqlite"
)

func TestEnsureDefaultAdminCreatesAdminWhenUserManagementEnabled(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.UserManagement.Enabled = true
	cfg.SQLite.Path = filepath.Join(t.TempDir(), "monstermq.db")

	storage, db, err := storesqlite.Build(ctx, cfg)
	if err != nil {
		t.Fatalf("build storage: %v", err)
	}
	defer db.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := ensureDefaultAdmin(ctx, cfg, storage, logger); err != nil {
		t.Fatalf("ensure default admin: %v", err)
	}

	user, err := storage.Users.ValidateCredentials(ctx, defaultAdminUsername, defaultAdminPassword)
	if err != nil {
		t.Fatalf("validate credentials: %v", err)
	}
	if user == nil {
		t.Fatal("default admin credentials did not validate")
	}
	if !user.Enabled || !user.IsAdmin || !user.CanSubscribe || !user.CanPublish {
		t.Fatalf("unexpected default admin flags: %+v", user)
	}
}

func TestEnsureDefaultAdminLeavesExistingAdminPassword(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.UserManagement.Enabled = true
	cfg.SQLite.Path = filepath.Join(t.TempDir(), "monstermq.db")

	storage, db, err := storesqlite.Build(ctx, cfg)
	if err != nil {
		t.Fatalf("build storage: %v", err)
	}
	defer db.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := ensureDefaultAdmin(ctx, cfg, storage, logger); err != nil {
		t.Fatalf("ensure default admin: %v", err)
	}
	existing, err := storage.Users.GetUser(ctx, defaultAdminUsername)
	if err != nil {
		t.Fatalf("get default admin: %v", err)
	}
	existing.PasswordHash, err = storesqlite.HashPassword("Changed")
	if err != nil {
		t.Fatalf("hash changed password: %v", err)
	}
	if err := storage.Users.UpdateUser(ctx, *existing); err != nil {
		t.Fatalf("update default admin: %v", err)
	}

	if err := ensureDefaultAdmin(ctx, cfg, storage, logger); err != nil {
		t.Fatalf("ensure existing default admin: %v", err)
	}
	if user, err := storage.Users.ValidateCredentials(ctx, defaultAdminUsername, defaultAdminPassword); err != nil {
		t.Fatalf("validate default password: %v", err)
	} else if user != nil {
		t.Fatal("default admin password was overwritten")
	}
	if user, err := storage.Users.ValidateCredentials(ctx, defaultAdminUsername, "Changed"); err != nil {
		t.Fatalf("validate changed password: %v", err)
	} else if user == nil {
		t.Fatal("changed password did not validate")
	}
}
