package config

import "fmt"

type StoreType string

const (
	StoreNone     StoreType = "NONE"
	StoreMemory   StoreType = "MEMORY"
	StoreSQLite   StoreType = "SQLITE"
	StorePostgres StoreType = "POSTGRES"
	StoreMongoDB  StoreType = "MONGODB"
)

// validBackends is the set of store types that can back the persistent storage
// stack (sessions, config, etc.). MEMORY is allowed for selected volatile stores.
var validBackends = []StoreType{StoreSQLite, StorePostgres, StoreMongoDB}

// validRetainedBackends extends validBackends with MEMORY: when set, retained
// messages are not persisted to a database and not pre-loaded at startup —
// mochi-mqtt keeps them in its own in-memory map only.
var validRetainedBackends = []StoreType{StoreSQLite, StorePostgres, StoreMongoDB, StoreMemory}

func (s StoreType) isValidBackend() bool {
	for _, v := range validBackends {
		if s == v {
			return true
		}
	}
	return false
}

func (s StoreType) isValidRetainedBackend() bool {
	for _, v := range validRetainedBackends {
		if s == v {
			return true
		}
	}
	return false
}

func (s StoreType) isValidVolatileBackend() bool {
	return s == StoreMemory || s.isValidBackend()
}

type Listener struct {
	Enabled          bool   `yaml:"Enabled"`
	Address          string `yaml:"Address,omitempty"`
	Port             int    `yaml:"Port"`
	KeyStorePath     string `yaml:"KeyStorePath,omitempty"`
	KeyStorePassword string `yaml:"KeyStorePassword,omitempty"`
}

// ListenAddress returns the address to bind, defaulting to 0.0.0.0 when unset.
// An explicit empty string in config.yaml would still fall back to 0.0.0.0.
func (l *Listener) ListenAddress() string {
	if l.Address == "" {
		return "0.0.0.0"
	}
	return l.Address
}

type SQLiteConfig struct {
	Path string `yaml:"Path"`
}

type PostgresConfig struct {
	URL  string `yaml:"Url"`
	User string `yaml:"User"`
	Pass string `yaml:"Pass"`
}

type MongoDBConfig struct {
	URL      string `yaml:"Url"`
	Database string `yaml:"Database"`
}

type UserManagementConfig struct {
	Enabled           bool   `yaml:"Enabled"`
	PasswordAlgorithm string `yaml:"PasswordAlgorithm"`
	AnonymousEnabled  bool   `yaml:"AnonymousEnabled"`
	AclCacheEnabled   bool   `yaml:"AclCacheEnabled"`
}

type MetricsConfig struct {
	Enabled                   bool      `yaml:"Enabled"`
	StoreType                 StoreType `yaml:"StoreType"`
	CollectionIntervalSeconds int       `yaml:"CollectionIntervalSeconds"`
	RetentionHours            int       `yaml:"RetentionHours"`
	MaxHistoryRows            int       `yaml:"MaxHistoryRows"`
}

type LoggingConfig struct {
	Level             string `yaml:"Level"`
	MqttSyslogEnabled bool   `yaml:"MqttSyslogEnabled"`
	RingBufferSize    int    `yaml:"RingBufferSize"`
}

type GraphQLConfig struct {
	Enabled bool `yaml:"Enabled"`
	Port    int  `yaml:"Port"`
}

// FeaturesConfig is a flat set of feature toggles, mirroring the Features
// section in the Java monster-mq broker. Each field enables/disables a
// subsystem at startup. Add new flags here as they come online.
type FeaturesConfig struct {
	MqttClient bool `yaml:"MqttClient"`
	WinCCUa    bool `yaml:"WinCCUa"`
	WinCCOa    bool `yaml:"WinCCOa"`
}

type Config struct {
	NodeID         string   `yaml:"NodeId"`
	TCP            Listener `yaml:"TCP"`
	TCPS           Listener `yaml:"TCPS"`
	WS             Listener `yaml:"WS"`
	WSS            Listener `yaml:"WSS"`
	MaxMessageSize int      `yaml:"MaxMessageSize"`

	DefaultStoreType  StoreType `yaml:"DefaultStoreType"`
	SessionStoreType  StoreType `yaml:"SessionStoreType"`
	RetainedStoreType StoreType `yaml:"RetainedStoreType"`
	ConfigStoreType   StoreType `yaml:"ConfigStoreType"`
	QueueStoreType    StoreType `yaml:"QueueStoreType"`

	SQLite   SQLiteConfig   `yaml:"SQLite"`
	Postgres PostgresConfig `yaml:"Postgres"`
	MongoDB  MongoDBConfig  `yaml:"MongoDB"`

	UserManagement UserManagementConfig `yaml:"UserManagement"`
	Metrics        MetricsConfig        `yaml:"Metrics"`
	Logging        LoggingConfig        `yaml:"Logging"`
	GraphQL        GraphQLConfig        `yaml:"GraphQL"`
	Features       FeaturesConfig       `yaml:"Features"`

	// QueuedMessagesEnabled selects how messages for offline persistent (clean=false)
	// sessions are held until the client reconnects.
	//
	//   true  → use QueueStoreType. Persistent queues survive broker restart;
	//           MEMORY queues are process-local.
	//   false → rely on mochi-mqtt's in-memory inflight buffer. Messages are lost
	//           on broker restart but lower latency / no DB writes per publish.
	QueuedMessagesEnabled bool `yaml:"QueuedMessagesEnabled"`
}

func Default() *Config {
	return &Config{
		NodeID:                "edge",
		TCP:                   Listener{Enabled: true, Port: 1883},
		TCPS:                  Listener{Enabled: false, Port: 8883},
		WS:                    Listener{Enabled: false, Port: 1884},
		WSS:                   Listener{Enabled: false, Port: 8884},
		MaxMessageSize:        1048576,
		DefaultStoreType:      StoreSQLite,
		SessionStoreType:      StoreSQLite,
		RetainedStoreType:     StoreSQLite,
		ConfigStoreType:       StoreSQLite,
		QueueStoreType:        StoreSQLite,
		SQLite:                SQLiteConfig{Path: "./data/monstermq.db"},
		UserManagement:        UserManagementConfig{Enabled: false, PasswordAlgorithm: "BCRYPT", AnonymousEnabled: true, AclCacheEnabled: true},
		Metrics:               MetricsConfig{Enabled: true, CollectionIntervalSeconds: 1, RetentionHours: 168, MaxHistoryRows: 3600},
		Logging:               LoggingConfig{Level: "INFO", MqttSyslogEnabled: false, RingBufferSize: 1000},
		GraphQL:               GraphQLConfig{Enabled: true, Port: 8080},
		QueuedMessagesEnabled: true,
	}
}

// SessionStore returns the effective store type for sessions, falling back to DefaultStoreType.
func (c *Config) SessionStore() StoreType {
	if c.SessionStoreType != "" {
		return c.SessionStoreType
	}
	return c.DefaultStoreType
}

func (c *Config) RetainedStore() StoreType {
	if c.RetainedStoreType != "" {
		return c.RetainedStoreType
	}
	return c.DefaultStoreType
}

func (c *Config) ConfigStore() StoreType {
	if c.ConfigStoreType != "" {
		return c.ConfigStoreType
	}
	return c.DefaultStoreType
}

func (c *Config) QueueStore() StoreType {
	if c.QueueStoreType != "" {
		return c.QueueStoreType
	}
	return c.DefaultStoreType
}

func (c *Config) MetricsStore() StoreType {
	if c.Metrics.StoreType != "" {
		return c.Metrics.StoreType
	}
	return c.DefaultStoreType
}

// Validate checks that all settings are recognised and self-consistent.
// Called after the YAML is parsed so the broker fails fast on bad config
// instead of silently falling back to a default.
func (c *Config) Validate() error {
	if c.DefaultStoreType == "" {
		return fmt.Errorf("DefaultStoreType is required")
	}
	if !c.DefaultStoreType.isValidBackend() {
		return fmt.Errorf("invalid DefaultStoreType %q (must be one of SQLITE, POSTGRES, MONGODB)", c.DefaultStoreType)
	}
	overrides := []struct {
		name  string
		value StoreType
	}{
		{"ConfigStoreType", c.ConfigStoreType},
	}
	for _, f := range overrides {
		if f.value != "" && !f.value.isValidBackend() {
			return fmt.Errorf("invalid %s %q (must be one of SQLITE, POSTGRES, MONGODB)", f.name, f.value)
		}
	}
	if c.RetainedStoreType != "" && !c.RetainedStoreType.isValidRetainedBackend() {
		return fmt.Errorf("invalid RetainedStoreType %q (must be one of SQLITE, POSTGRES, MONGODB, MEMORY)", c.RetainedStoreType)
	}
	if c.SessionStoreType != "" && !c.SessionStoreType.isValidVolatileBackend() {
		return fmt.Errorf("invalid SessionStoreType %q (must be one of SQLITE, POSTGRES, MONGODB, MEMORY)", c.SessionStoreType)
	}
	if c.QueueStoreType != "" && !c.QueueStoreType.isValidVolatileBackend() {
		return fmt.Errorf("invalid QueueStoreType %q (must be one of SQLITE, POSTGRES, MONGODB, MEMORY)", c.QueueStoreType)
	}
	if c.QueueStoreType != "" && c.QueueStoreType != StoreMemory && c.QueueStoreType != c.DefaultStoreType {
		return fmt.Errorf("QueueStoreType %q is not supported with DefaultStoreType %q; use MEMORY or the default backend", c.QueueStoreType, c.DefaultStoreType)
	}
	if c.Metrics.StoreType != "" {
		switch c.Metrics.StoreType {
		case StoreNone, StoreMemory, StoreSQLite, StorePostgres, StoreMongoDB:
		default:
			return fmt.Errorf("invalid Metrics.StoreType %q (must be one of NONE, MEMORY, SQLITE, POSTGRES, MONGODB)", c.Metrics.StoreType)
		}
	}
	return nil
}
