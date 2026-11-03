package virtualtrees

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"monstermq.io/edge/internal/stores"
)

// RetainedFinder looks up retained topics for a filter; the callback returns
// false to stop.
type RetainedFinder func(ctx context.Context, filter string, fn func(topic string) bool) error

// Manager owns the configured trees, persists them through the device config
// store and publishes an immutable resolver snapshot for the data path.
type Manager struct {
	store    stores.DeviceConfigStore
	logger   *slog.Logger
	mu       sync.Mutex
	snap     atomic.Pointer[Resolver]
	retained RetainedFinder
	memory   map[string]*Tree
}

func NewManager(store stores.DeviceConfigStore, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	m := &Manager{store: store, logger: logger, memory: map[string]*Tree{}}
	m.snap.Store(Empty)
	return m
}

// SetRetainedFinder enables the PHYSICAL_CONFLICT warning.
func (m *Manager) SetRetainedFinder(f RetainedFinder) { m.retained = f }

// Resolver returns the current routing snapshot; never nil.
func (m *Manager) Resolver() *Resolver {
	if m == nil {
		return Empty
	}
	return m.snap.Load()
}

func (m *Manager) Trees() []*Tree { return m.Resolver().Trees() }

func (m *Manager) Tree(id string) *Tree { return m.Resolver().Tree(id) }

// Load reads all trees from the store and swaps the routing snapshot.
func (m *Manager) Load(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	trees, err := m.loadTrees(ctx)
	if err != nil {
		return err
	}
	m.apply(trees)
	m.logger.Info("virtual topic trees loaded", "count", len(trees))
	return nil
}

func (m *Manager) loadTrees(ctx context.Context) ([]*Tree, error) {
	var out []*Tree
	if m.store == nil {
		for _, t := range m.memory {
			out = append(out, t)
		}
	} else {
		devs, err := m.store.GetByType(ctx, DeviceType)
		if err != nil {
			return nil, err
		}
		for _, d := range devs {
			t, err := ParseJSON([]byte(d.Config))
			if err != nil {
				m.logger.Warn("ignoring invalid virtual topic tree", "name", d.Name, "err", err)
				continue
			}
			t.ID = d.Name
			t.Enabled = d.Enabled
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *Manager) apply(trees []*Tree) {
	sort.Slice(trees, func(i, j int) bool { return trees[i].ID < trees[j].ID })
	m.snap.Store(NewResolver(trees))
}

// Validate checks a candidate against the active configuration.
func (m *Manager) Validate(candidate *Tree) []Issue {
	return Validate(candidate, others(m.Trees(), candidate.ID))
}

func others(trees []*Tree, id string) []*Tree {
	out := make([]*Tree, 0, len(trees))
	for _, t := range trees {
		if t.ID != id {
			out = append(out, t)
		}
	}
	return out
}

// Warnings reports retained physical data inside the claimed root.
func (m *Manager) Warnings(ctx context.Context, candidate *Tree) []Issue {
	if m.retained == nil || !validName(candidate.RootPath) || hasWildcard(candidate.RootPath) {
		return nil
	}
	found := ""
	for _, f := range []string{candidate.RootPath, candidate.RootPath + "/#"} {
		_ = m.retained(ctx, f, func(topic string) bool { found = topic; return false })
		if found != "" {
			break
		}
	}
	if found == "" {
		return nil
	}
	return []Issue{{Code: CodePhysicalConflict, Path: found,
		Message: fmt.Sprintf("Retained physical topic '%s' lies in the virtual root and will be hidden", found)}}
}

// MutationResult is the outcome of a save or delete.
type MutationResult struct {
	Success bool
	Tree    *Tree
	Errors  []Issue
}

func conflict(msg string) MutationResult {
	return MutationResult{Errors: []Issue{{Code: CodeRevisionConflict, Message: msg}}}
}

func notFound(id string) MutationResult {
	return MutationResult{Errors: []Issue{{Code: CodeNotFound, Message: fmt.Sprintf("Tree '%s' does not exist", id)}}}
}

func storeError(err error) MutationResult {
	return MutationResult{Errors: []Issue{{Code: CodeStoreError, Message: err.Error()}}}
}

func find(trees []*Tree, id string) *Tree {
	for _, t := range trees {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// Save creates or replaces a tree with optimistic concurrency: an existing
// tree requires its current revision, a new tree must not pass one.
func (m *Manager) Save(ctx context.Context, candidate *Tree, expectedRevision string) MutationResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, err := m.loadTrees(ctx)
	if err != nil {
		return storeError(err)
	}
	existing := find(stored, candidate.ID)
	switch {
	case existing != nil && expectedRevision == "":
		return conflict(fmt.Sprintf("Tree '%s' already exists; expectedRevision is required", candidate.ID))
	case existing != nil && existing.Revision != expectedRevision:
		return conflict(fmt.Sprintf("Tree '%s' was changed (revision %s)", candidate.ID, existing.Revision))
	case existing == nil && expectedRevision != "":
		return notFound(candidate.ID)
	}
	rest := others(stored, candidate.ID)
	if errs := Validate(candidate, rest); len(errs) > 0 {
		return MutationResult{Errors: errs}
	}
	tree := candidate.Clone()
	tree.Revision = uuid.NewString()
	if err := m.persist(ctx, tree); err != nil {
		return storeError(err)
	}
	m.apply(append(rest, tree))
	return MutationResult{Success: true, Tree: tree}
}

// Delete removes a tree if expectedRevision matches its current revision.
func (m *Manager) Delete(ctx context.Context, id, expectedRevision string) MutationResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, err := m.loadTrees(ctx)
	if err != nil {
		return storeError(err)
	}
	existing := find(stored, id)
	if existing == nil {
		return notFound(id)
	}
	if existing.Revision != expectedRevision {
		return conflict(fmt.Sprintf("Tree '%s' was changed (revision %s)", id, existing.Revision))
	}
	if m.store == nil {
		delete(m.memory, id)
	} else if err := m.store.Delete(ctx, id); err != nil {
		return storeError(err)
	}
	m.apply(others(stored, id))
	return MutationResult{Success: true, Tree: existing}
}

// SetEnabled enables or disables a tree (generic device toggle).
func (m *Manager) SetEnabled(ctx context.Context, id string, enabled bool) MutationResult {
	cur := m.Tree(id)
	if cur == nil {
		return notFound(id)
	}
	t := cur.Clone()
	t.Enabled = enabled
	return m.Save(ctx, t, cur.Revision)
}

// ImportEntry is one exported tree document; Config is a JSON object, a map or
// a JSON string.
type ImportEntry struct {
	Name   string
	Config any
}

// ImportResult summarizes an import.
type ImportResult struct {
	Imported int
	Failed   int
	Errors   []string
}

// Import stores exported tree documents. Imported trees are always disabled so
// an import never changes live routing; they are validated like a save.
func (m *Manager) Import(ctx context.Context, entries []ImportEntry) ImportResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := ImportResult{}
	stored, err := m.loadTrees(ctx)
	if err != nil {
		res.Failed = len(entries)
		res.Errors = append(res.Errors, err.Error())
		return res
	}
	current := map[string]*Tree{}
	for _, t := range stored {
		current[t.ID] = t
	}
	for _, e := range entries {
		t, err := importTree(e)
		if err == nil {
			var list []*Tree
			for _, o := range current {
				if o.ID != t.ID {
					list = append(list, o)
				}
			}
			if issues := Validate(t, list); len(issues) > 0 {
				parts := make([]string, len(issues))
				for i, is := range issues {
					parts[i] = is.Code + ": " + is.Message
				}
				err = fmt.Errorf("%s", strings.Join(parts, ", "))
			}
		}
		if err == nil {
			t.Revision = uuid.NewString()
			err = m.persist(ctx, t)
		}
		if err != nil {
			res.Failed++
			name := e.Name
			if name == "" {
				name = "?"
			}
			res.Errors = append(res.Errors, fmt.Sprintf("Virtual topic tree '%s': %s", name, err.Error()))
			continue
		}
		current[t.ID] = t
		res.Imported++
	}
	if res.Imported > 0 {
		list := make([]*Tree, 0, len(current))
		for _, t := range current {
			list = append(list, t)
		}
		m.apply(list)
	}
	return res
}

func importTree(e ImportEntry) (*Tree, error) {
	var t *Tree
	var err error
	switch c := e.Config.(type) {
	case string:
		t, err = ParseJSON([]byte(c))
	case []byte:
		t, err = ParseJSON(c)
	case map[string]any:
		b, merr := json.Marshal(c)
		if merr != nil {
			return nil, merr
		}
		t, err = ParseJSON(b)
	default:
		return nil, fmt.Errorf("config must be a JSON object")
	}
	if err != nil {
		return nil, err
	}
	t.Enabled = false
	t.Revision = ""
	if t.ID == "" {
		t.ID = e.Name
	}
	if e.Name != "" && e.Name != t.ID {
		return nil, fmt.Errorf("device name '%s' differs from tree id '%s'", e.Name, t.ID)
	}
	return t, nil
}

func (m *Manager) persist(ctx context.Context, t *Tree) error {
	if m.store == nil {
		m.memory[t.ID] = t
		return nil
	}
	dev, err := m.store.Get(ctx, t.ID)
	if err != nil {
		return err
	}
	if dev != nil && dev.Type != DeviceType {
		return fmt.Errorf("a device named '%s' of type '%s' already exists", t.ID, dev.Type)
	}
	now := time.Now().UTC()
	created := now
	if dev != nil && !dev.CreatedAt.IsZero() {
		created = dev.CreatedAt
	}
	return m.store.Save(ctx, stores.DeviceConfig{
		Name:      t.ID,
		Namespace: t.RootPath,
		NodeID:    DeviceNodeID,
		Type:      DeviceType,
		Enabled:   t.Enabled,
		Config:    string(t.Stored()),
		CreatedAt: created,
		UpdatedAt: now,
	})
}
