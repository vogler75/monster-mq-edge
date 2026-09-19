package scripting

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"monstermq.io/edge/internal/stores"
)

const (
	StorageNamespace = "script-storage"
	StorageType      = "ScriptStorage"
	StoragePrefix    = "__script_kv_"
)

// ScriptKVStore provides persistent key-value storage for a script that survives restarts.
type ScriptKVStore struct {
	scriptName string
	store      stores.DeviceConfigStore
	nodeID     string

	mu   sync.RWMutex
	data map[string]any
}

func NewScriptKVStore(scriptName string, store stores.DeviceConfigStore, nodeID string) *ScriptKVStore {
	return &ScriptKVStore{
		scriptName: scriptName,
		store:      store,
		nodeID:     nodeID,
		data:       make(map[string]any),
	}
}

// Load loads persisted key-value pairs from the store into memory.
func (s *ScriptKVStore) Load(ctx context.Context) error {
	if s.store == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	dc, err := s.store.Get(ctx, StoragePrefix+s.scriptName)
	if err != nil || dc == nil || dc.Config == "" {
		return nil
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(dc.Config), &m); err == nil {
		s.data = m
	}
	return nil
}

// Get retrieves a key with an optional fallback default.
func (s *ScriptKVStore) Get(key string, defaultValue any) any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if val, ok := s.data[key]; ok {
		return val
	}
	return defaultValue
}

// Set saves a key-value pair and persists it.
func (s *ScriptKVStore) Set(key string, value any) error {
	s.mu.Lock()
	s.data[key] = value
	s.mu.Unlock()
	return s.persist()
}

// Delete removes a key and persists the update.
func (s *ScriptKVStore) Delete(key string) error {
	s.mu.Lock()
	delete(s.data, key)
	s.mu.Unlock()
	return s.persist()
}

// List returns a copy of all stored keys and values.
func (s *ScriptKVStore) List() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]any, len(s.data))
	for k, v := range s.data {
		out[k] = v
	}
	return out
}

func (s *ScriptKVStore) persist() error {
	if s.store == nil {
		return nil
	}
	s.mu.RLock()
	raw, err := json.Marshal(s.data)
	s.mu.RUnlock()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	return s.store.Save(ctx, stores.DeviceConfig{
		Name:      StoragePrefix + s.scriptName,
		Namespace: StorageNamespace,
		NodeID:    s.nodeID,
		Type:      StorageType,
		Enabled:   true,
		Config:    string(raw),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	})
}
