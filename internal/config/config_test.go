package config

import (
	"strings"
	"testing"
)

func TestValidateRejectsUnsupportedPersistentQueueOverride(t *testing.T) {
	cfg := Default()
	cfg.DefaultStoreType = StoreSQLite
	cfg.QueueStoreType = StorePostgres

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "QueueStoreType") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateAllowsMemoryQueueOverride(t *testing.T) {
	cfg := Default()
	cfg.DefaultStoreType = StoreSQLite
	cfg.QueueStoreType = StoreMemory

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}
