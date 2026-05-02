package archive

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/stores"
	storememory "monstermq.io/edge/internal/stores/memory"
	storemongo "monstermq.io/edge/internal/stores/mongodb"
	storepg "monstermq.io/edge/internal/stores/postgres"
	storesqlite "monstermq.io/edge/internal/stores/sqlite"
)

// Manager owns the active set of archive groups and dispatches every published
// message to all matching groups. It supports SQLite, PostgreSQL and MongoDB
// per-group last-value and archive stores.
type Manager struct {
	cfg      *config.Config
	storage  *stores.Storage
	logger   *slog.Logger
	sqliteDB *storesqlite.DB
	pgDB     *storepg.DB
	mongoDB  *storemongo.DB

	mu          sync.RWMutex
	groups      map[string]*Group
	deployError map[string]string // last deploy error per group name (for the dashboard)
}

func NewManager(cfg *config.Config, storage *stores.Storage,
	sqliteDB *storesqlite.DB, pgDB *storepg.DB, mongoDB *storemongo.DB,
	logger *slog.Logger) *Manager {
	return &Manager{
		cfg: cfg, storage: storage, logger: logger,
		sqliteDB: sqliteDB, pgDB: pgDB, mongoDB: mongoDB,
		groups:      map[string]*Group{},
		deployError: map[string]string{},
	}
}

// DeployError returns the last error encountered while trying to start the
// named group, or empty string if the group is healthy.
func (m *Manager) DeployError(name string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.deployError[name]
}

// Load creates archive groups from the persistent ArchiveConfigStore. If no
// "Default" group exists yet, one is provisioned with in-memory last-value
// storage and no history archive.
func (m *Manager) Load(ctx context.Context) error {
	configs, err := m.storage.ArchiveConfig.GetAll(ctx)
	if err != nil {
		return err
	}
	hasDefault := false
	for _, c := range configs {
		if c.Name == "Default" {
			hasDefault = true
			break
		}
	}
	if !hasDefault {
		def := stores.ArchiveGroupConfig{
			Name:          "Default",
			Enabled:       true,
			TopicFilters:  []string{"#"},
			LastValType:   stores.MessageStoreMemory,
			ArchiveType:   stores.ArchiveNone,
			PayloadFormat: stores.PayloadDefault,
		}
		if err := m.storage.ArchiveConfig.Save(ctx, def); err != nil {
			return err
		}
		configs = append(configs, def)
	}
	for _, c := range configs {
		if err := m.startGroup(ctx, c); err != nil {
			m.recordDeployError(c.Name, err)
			m.logger.Error("archive group start failed", "name", c.Name, "err", err)
		}
	}
	return nil
}

func (m *Manager) recordDeployError(name string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		delete(m.deployError, name)
		return
	}
	m.deployError[name] = err.Error()
}

func (m *Manager) startGroup(ctx context.Context, c stores.ArchiveGroupConfig) error {
	if !c.Enabled {
		return nil
	}
	handles, err := m.openGroupDatabaseHandles(ctx, c)
	if err != nil {
		return err
	}
	closeOnErr := true
	defer func() {
		if closeOnErr {
			handles.close(m.logger, c.Name)
		}
	}()
	lastVal, err := m.buildLastValStore(ctx, c, handles)
	if err != nil {
		return err
	}
	arch, err := m.buildArchiveStore(ctx, c, handles)
	if err != nil {
		return err
	}
	g := NewGroup(c, lastVal, arch, m.logger, handles.closeFunc(m.logger, c.Name))
	g.Start()
	m.mu.Lock()
	m.groups[c.Name] = g
	delete(m.deployError, c.Name)
	m.mu.Unlock()
	closeOnErr = false
	m.logger.Info("archive group started",
		"name", c.Name, "filters", c.TopicFilters,
		"lastValType", c.LastValType, "archiveType", c.ArchiveType)
	return nil
}

// Per-group store and archive names match the Kotlin/JVM broker:
//   last-value store →  "<groupName>Lastval"   (SQL table: <groupname>lastval)
//   message archive  →  "<groupName>Archive"   (SQL table: <groupname>archive)
// so the same physical database can be opened by either implementation.

// LastValName / ArchiveName are exported so tests and the dashboard can derive
// the same names production uses. Both are deterministic from the group name.
func LastValName(group string) string { return group + "Lastval" }
func ArchiveName(group string) string { return group + "Archive" }

// validGroupNamePattern restricts group names to characters that are safe to
// embed in a SQL identifier. Group names flow into CREATE TABLE statements via
// fmt.Sprintf, so anything outside this set could be SQL injection.
var validGroupNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,62}$`)

func ValidateGroupName(name string) error {
	if !validGroupNamePattern.MatchString(name) {
		return fmt.Errorf("invalid group name %q: must match [A-Za-z][A-Za-z0-9_]{0,62}", name)
	}
	return nil
}

type groupDatabaseHandles struct {
	pgDB    *storepg.DB
	mongoDB *storemongo.DB
	owned   []func() error
}

func (h groupDatabaseHandles) close(logger *slog.Logger, group string) {
	for _, closeFn := range h.owned {
		if closeFn == nil {
			continue
		}
		if err := closeFn(); err != nil {
			logger.Warn("archive group database close failed", "group", group, "err", err)
		}
	}
}

func (h groupDatabaseHandles) closeFunc(logger *slog.Logger, group string) func() error {
	if len(h.owned) == 0 {
		return nil
	}
	return func() error {
		h.close(logger, group)
		return nil
	}
}

func (m *Manager) openGroupDatabaseHandles(ctx context.Context, c stores.ArchiveGroupConfig) (groupDatabaseHandles, error) {
	handles := groupDatabaseHandles{pgDB: m.pgDB, mongoDB: m.mongoDB}
	selectedName := strings.TrimSpace(c.DatabaseConnectionName)
	required := RequiredDatabaseConnectionTypes(c.LastValType, c.ArchiveType)
	if selectedName == "" {
		return handles, nil
	}
	if len(required) == 0 {
		return handles, fmt.Errorf("group %s: databaseConnectionName can only be used with Postgres or MongoDB stores", c.Name)
	}
	if len(required) > 1 {
		if IsDefaultDatabaseConnectionName(selectedName) {
			return handles, nil
		}
		return handles, fmt.Errorf("group %s: cannot use one named database connection for mixed Postgres and MongoDB stores", c.Name)
	}
	if IsDefaultDatabaseConnectionName(selectedName) {
		switch required[0] {
		case stores.DatabaseConnectionPostgres:
			if m.pgDB == nil {
				return handles, fmt.Errorf("group %s: default Postgres database connection is not configured", c.Name)
			}
		case stores.DatabaseConnectionMongoDB:
			if m.mongoDB == nil {
				return handles, fmt.Errorf("group %s: default MongoDB database connection is not configured", c.Name)
			}
		}
		return handles, nil
	}
	conn, err := m.storage.ArchiveConfig.GetDatabaseConnection(ctx, selectedName)
	if err != nil {
		return handles, err
	}
	if conn == nil {
		return handles, fmt.Errorf("group %s: database connection %q not found", c.Name, selectedName)
	}
	if conn.Type != required[0] {
		return handles, fmt.Errorf("group %s: selected %s connection %q but requires %s", c.Name, conn.Type, selectedName, required[0])
	}
	switch conn.Type {
	case stores.DatabaseConnectionPostgres:
		db, err := storepg.Open(ctx, postgresDSN(conn.URL, conn.Username, conn.Password))
		if err != nil {
			return handles, err
		}
		handles.pgDB = db
		handles.owned = append(handles.owned, db.Close)
	case stores.DatabaseConnectionMongoDB:
		dbName := conn.Database
		if dbName == "" {
			dbName = "monstermq"
		}
		db, err := storemongo.Open(ctx, MongoURLWithCredentials(conn.URL, conn.Username, conn.Password), dbName)
		if err != nil {
			return handles, err
		}
		handles.mongoDB = db
		handles.owned = append(handles.owned, db.Close)
	default:
		return handles, fmt.Errorf("group %s: unsupported database connection type %q", c.Name, conn.Type)
	}
	return handles, nil
}

func postgresDSN(raw, username, password string) string {
	if username == "" && password == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err == nil && u.Scheme != "" {
		if password != "" {
			u.User = url.UserPassword(username, password)
		} else {
			u.User = url.User(username)
		}
		return u.String()
	}
	sep := "?"
	if strings.Contains(raw, "?") {
		sep = "&"
	}
	return fmt.Sprintf("%s%suser=%s&password=%s", raw, sep, url.QueryEscape(username), url.QueryEscape(password))
}

func (m *Manager) buildLastValStore(ctx context.Context, c stores.ArchiveGroupConfig, handles groupDatabaseHandles) (stores.MessageStore, error) {
	name := LastValName(c.Name)
	switch c.LastValType {
	case stores.MessageStoreMemory:
		return storememory.NewMessageStore(name), nil
	case stores.MessageStoreSQLite:
		if m.sqliteDB == nil {
			return nil, fmt.Errorf("group %s: lastValType=SQLITE but no SQLite DB configured", c.Name)
		}
		s := storesqlite.NewMessageStore(name, m.sqliteDB)
		if err := s.EnsureTable(ctx); err != nil {
			return nil, fmt.Errorf("ensure %s on sqlite: %w", name, err)
		}
		return s, nil
	case stores.MessageStorePostgres:
		if handles.pgDB == nil {
			return nil, fmt.Errorf("group %s: lastValType=POSTGRES but no Postgres DB configured", c.Name)
		}
		s := storepg.NewMessageStore(name, handles.pgDB)
		if err := s.EnsureTable(ctx); err != nil {
			return nil, fmt.Errorf("ensure %s on postgres: %w", name, err)
		}
		return s, nil
	case stores.MessageStoreMongoDB:
		if handles.mongoDB == nil {
			return nil, fmt.Errorf("group %s: lastValType=MONGODB but no MongoDB configured", c.Name)
		}
		s := storemongo.NewMessageStore(name, handles.mongoDB)
		if err := s.EnsureTable(ctx); err != nil {
			return nil, fmt.Errorf("ensure %s on mongo: %w", name, err)
		}
		return s, nil
	case stores.MessageStoreNone, "":
		return nil, nil
	}
	return nil, fmt.Errorf("group %s: unsupported lastValType %q", c.Name, c.LastValType)
}

func (m *Manager) buildArchiveStore(ctx context.Context, c stores.ArchiveGroupConfig, handles groupDatabaseHandles) (stores.MessageArchive, error) {
	name := ArchiveName(c.Name)
	switch c.ArchiveType {
	case stores.ArchiveSQLite:
		if m.sqliteDB == nil {
			return nil, fmt.Errorf("group %s: archiveType=SQLITE but no SQLite DB configured", c.Name)
		}
		a := storesqlite.NewMessageArchive(name, m.sqliteDB, c.PayloadFormat)
		if err := a.EnsureTable(ctx); err != nil {
			return nil, fmt.Errorf("ensure %s on sqlite: %w", name, err)
		}
		return a, nil
	case stores.ArchivePostgres:
		if handles.pgDB == nil {
			return nil, fmt.Errorf("group %s: archiveType=POSTGRES but no Postgres DB configured", c.Name)
		}
		a := storepg.NewMessageArchive(name, handles.pgDB, c.PayloadFormat)
		if err := a.EnsureTable(ctx); err != nil {
			return nil, fmt.Errorf("ensure %s on postgres: %w", name, err)
		}
		return a, nil
	case stores.ArchiveMongoDB:
		if handles.mongoDB == nil {
			return nil, fmt.Errorf("group %s: archiveType=MONGODB but no MongoDB configured", c.Name)
		}
		a := storemongo.NewMessageArchive(name, handles.mongoDB, c.PayloadFormat)
		if err := a.EnsureTable(ctx); err != nil {
			return nil, fmt.Errorf("ensure %s on mongo: %w", name, err)
		}
		return a, nil
	case stores.ArchiveNone, "":
		return nil, nil
	}
	return nil, fmt.Errorf("group %s: unsupported archiveType %q", c.Name, c.ArchiveType)
}

// Reload re-reads the archive config table and reconciles the running groups
// with the persisted state: starts new ones, restarts changed ones, stops
// removed ones. Call this after any GraphQL mutation that touched an archive
// group config.
func (m *Manager) Reload(ctx context.Context) error {
	configs, err := m.storage.ArchiveConfig.GetAll(ctx)
	if err != nil {
		return err
	}
	wanted := map[string]stores.ArchiveGroupConfig{}
	for _, c := range configs {
		if c.Enabled {
			wanted[c.Name] = c
		}
	}

	m.mu.Lock()
	current := m.groups
	m.mu.Unlock()

	// Stop groups that are gone or now disabled or whose config changed.
	for name, g := range current {
		w, keep := wanted[name]
		if !keep || !configsEqual(g.Config(), w) {
			g.Stop()
			m.mu.Lock()
			delete(m.groups, name)
			m.mu.Unlock()
		}
	}
	// Start groups that should be running but aren't.
	for name, c := range wanted {
		m.mu.RLock()
		_, running := m.groups[name]
		m.mu.RUnlock()
		if running {
			continue
		}
		if err := m.startGroup(ctx, c); err != nil {
			m.recordDeployError(name, err)
			m.logger.Error("archive group reload failed", "name", name, "err", err)
		}
	}
	return nil
}

func configsEqual(a, b stores.ArchiveGroupConfig) bool {
	if a.Name != b.Name || a.Enabled != b.Enabled || a.RetainedOnly != b.RetainedOnly {
		return false
	}
	if a.LastValType != b.LastValType || a.ArchiveType != b.ArchiveType {
		return false
	}
	if a.DatabaseConnectionName != b.DatabaseConnectionName {
		return false
	}
	if a.PayloadFormat != b.PayloadFormat {
		return false
	}
	if a.LastValRetention != b.LastValRetention || a.ArchiveRetention != b.ArchiveRetention {
		return false
	}
	if len(a.TopicFilters) != len(b.TopicFilters) {
		return false
	}
	for i := range a.TopicFilters {
		if a.TopicFilters[i] != b.TopicFilters[i] {
			return false
		}
	}
	return true
}

// Dispatch routes msg to every matching group.
func (m *Manager) Dispatch(msg stores.BrokerMessage) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, g := range m.groups {
		if g.Matches(msg.TopicName, msg.IsRetain) {
			g.Submit(msg)
		}
	}
}

func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, g := range m.groups {
		g.Stop()
	}
	m.groups = map[string]*Group{}
}

// Snapshot returns a stable view of the current groups for resolvers.
func (m *Manager) Snapshot() []*Group {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Group, 0, len(m.groups))
	for _, g := range m.groups {
		out = append(out, g)
	}
	return out
}
