package config

import (
	"fmt"
	"os"
)

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

type ClientAuthType string

const (
	ClientAuthNone     ClientAuthType = "NONE"
	ClientAuthRequest  ClientAuthType = "REQUEST"
	ClientAuthRequired ClientAuthType = "REQUIRED"
)

type Listener struct {
	Enabled               bool           `yaml:"Enabled"`
	Address               string         `yaml:"Address,omitempty"`
	Port                  int            `yaml:"Port"`
	KeyStorePath          string         `yaml:"KeyStorePath,omitempty"`
	KeyStorePassword      string         `yaml:"KeyStorePassword,omitempty"`
	ClientAuth            ClientAuthType `yaml:"ClientAuth,omitempty"`
	TrustStorePath        string         `yaml:"TrustStorePath,omitempty"`
	TrustStorePassword    string         `yaml:"TrustStorePassword,omitempty"`
	TrustStoreType        string         `yaml:"TrustStoreType,omitempty"`
	UseIdentityAsUsername *bool          `yaml:"UseIdentityAsUsername,omitempty"`
	AutoCreateUser        *bool          `yaml:"AutoCreateUser,omitempty"`
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

type QuestDBConfig struct {
	URL  string `yaml:"Url"`
	User string `yaml:"User"`
	Pass string `yaml:"Pass"`
}

type MongoDBConfig struct {
	URL      string `yaml:"Url"`
	Database string `yaml:"Database"`
}

type UserManagementConfig struct {
	Enabled                bool   `yaml:"Enabled"`
	PasswordAlgorithm      string `yaml:"PasswordAlgorithm"`
	AnonymousEnabled       bool   `yaml:"AnonymousEnabled"`
	AclCacheEnabled        bool   `yaml:"AclCacheEnabled"`
	AclCheckOnSubscription *bool  `yaml:"AclCheckOnSubscription,omitempty"`
}

// AclCheckOnSub returns the effective value: default true (subscribe-time check).
func (u *UserManagementConfig) AclCheckOnSub() bool {
	if u.AclCheckOnSubscription == nil {
		return true
	}
	return *u.AclCheckOnSubscription
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

type MCPConfig struct {
	Enabled bool `yaml:"Enabled"`
	Port    int  `yaml:"Port"`
}

type HostMonitoringConfig struct {
	Enabled         bool   `yaml:"Enabled"`
	BaseTopic       string `yaml:"BaseTopic"`
	IntervalSeconds int    `yaml:"IntervalSeconds"`
	QoS             int    `yaml:"QoS"`
}

type HMIConfig struct {
	Enabled   bool   `yaml:"Enabled"`
	Path      string `yaml:"Path"`
	MountPath string `yaml:"MountPath"`
}

type RedfishConfig struct {
	Enabled          bool   `yaml:"Enabled"`
	Port             int    `yaml:"Port"`
	MountPath        string `yaml:"MountPath"`
	DefaultChassisId string `yaml:"DefaultChassisId"`
	DefaultSystemId  string `yaml:"DefaultSystemId"`
	DefaultManagerId string `yaml:"DefaultManagerId"`
	AnonymousEnabled bool   `yaml:"AnonymousEnabled"`
}

// FeaturesConfig is a flat set of feature toggles, mirroring the Features
// section in the Java monster-mq broker. Each field enables/disables a
// subsystem at startup. Add new flags here as they come online.
type FeaturesConfig struct {
	MqttClient         bool `yaml:"MqttClient"`
	WinCCUa            bool `yaml:"WinCCUa"`
	WinCCOa            bool `yaml:"WinCCOa"`
	DeviceImportExport bool `yaml:"DeviceImportExport"`
	Mcp                bool `yaml:"Mcp"`
	Hmi                bool `yaml:"Hmi"`
	Redfish            bool `yaml:"Redfish"`
}

type WSSOverrideConfig struct {
	KeyStorePath     string `yaml:"KeyStorePath,omitempty"`
	KeyStorePassword string `yaml:"KeyStorePassword,omitempty"`
	KeyPath          string `yaml:"KeyPath,omitempty"`
}

type SSLConfig struct {
	KeyStorePath          string            `yaml:"KeyStorePath,omitempty"`
	KeyStorePassword      string            `yaml:"KeyStorePassword,omitempty"`
	KeyStoreType          string            `yaml:"KeyStoreType,omitempty"`
	KeyPath               string            `yaml:"KeyPath,omitempty"`
	ClientAuth            ClientAuthType    `yaml:"ClientAuth,omitempty"`
	TrustStorePath        string            `yaml:"TrustStorePath,omitempty"`
	TrustStorePassword    string            `yaml:"TrustStorePassword,omitempty"`
	TrustStoreType        string            `yaml:"TrustStoreType,omitempty"`
	UseIdentityAsUsername bool              `yaml:"UseIdentityAsUsername"`
	AutoCreateUser        bool              `yaml:"AutoCreateUser"`
	WSS                   WSSOverrideConfig `yaml:"WSS,omitempty"`
}

type Config struct {
	NodeID         string    `yaml:"NodeId"`
	TCP            Listener  `yaml:"TCP"`
	TCPS           Listener  `yaml:"TCPS"`
	WS             Listener  `yaml:"WS"`
	WSS            Listener  `yaml:"WSS"`
	SSL            SSLConfig `yaml:"SSL"`
	MaxMessageSize int       `yaml:"MaxMessageSize"`

	DefaultStoreType  StoreType `yaml:"DefaultStoreType"`
	SessionStoreType  StoreType `yaml:"SessionStoreType"`
	RetainedStoreType StoreType `yaml:"RetainedStoreType"`
	ConfigStoreType   StoreType `yaml:"ConfigStoreType"`
	QueueStoreType    StoreType `yaml:"QueueStoreType"`

	SQLite   SQLiteConfig   `yaml:"SQLite"`
	Postgres PostgresConfig `yaml:"Postgres"`
	QuestDB  QuestDBConfig  `yaml:"QuestDB"`
	MongoDB  MongoDBConfig  `yaml:"MongoDB"`

	UserManagement UserManagementConfig `yaml:"UserManagement"`
	Metrics        MetricsConfig        `yaml:"Metrics"`
	Logging        LoggingConfig        `yaml:"Logging"`
	GraphQL        GraphQLConfig        `yaml:"GraphQL"`
	MCP            MCPConfig            `yaml:"MCP"`
	Features       FeaturesConfig       `yaml:"Features"`
	HostMonitoring HostMonitoringConfig `yaml:"HostMonitoring"`
	HMI            HMIConfig            `yaml:"HMI"`
	Redfish        RedfishConfig        `yaml:"Redfish"`

	// QueuedMessagesEnabled selects how messages for offline persistent (clean=false)
	// sessions are held until the client reconnects.
	//
	//   true  → use QueueStoreType. Persistent queues survive broker restart;
	//           MEMORY queues are process-local.
	//   false → rely on mochi-mqtt's in-memory inflight buffer. Messages are lost
	//           on broker restart but lower latency / no DB writes per publish.
	QueuedMessagesEnabled bool `yaml:"QueuedMessagesEnabled"`
	MaxQueueMessages      *int `yaml:"MaxQueueMessages"`
	QueueBatchSize        *int `yaml:"QueueBatchSize"`
	QueueFlushIntervalMs  *int `yaml:"QueueFlushIntervalMs"`
}

func Default() *Config {
	return &Config{
		NodeID:                "",
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
		GraphQL:               GraphQLConfig{Enabled: true, Port: 4000},
		MCP:                   MCPConfig{Enabled: false, Port: 3000},
		Features:              FeaturesConfig{MqttClient: false, WinCCUa: false, WinCCOa: false, DeviceImportExport: false, Mcp: false, Hmi: false, Redfish: false},
		HostMonitoring: HostMonitoringConfig{
			Enabled:         false,
			BaseTopic:       "nodes/{NodeId}/host",
			IntervalSeconds: 10,
			QoS:             0,
		},
		HMI: HMIConfig{
			Enabled:   false,
			Path:      "./data/hmi",
			MountPath: "/hmi",
		},
		Redfish: RedfishConfig{
			Enabled:          false,
			Port:             8000,
			MountPath:        "/redfish/v1",
			DefaultChassisId: "EdgeNode",
			DefaultSystemId:  "edge-node",
			DefaultManagerId: "monstermq-edge",
			AnonymousEnabled: true,
		},
		QueuedMessagesEnabled: true,
		MaxQueueMessages:      nil,
		QueueBatchSize:        nil,
		QueueFlushIntervalMs:  nil,
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
	if c.NodeID == "" {
		if hn, err := os.Hostname(); err == nil && hn != "" {
			c.NodeID = hn
		} else {
			c.NodeID = "edge"
		}
	}
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
	if c.HostMonitoring.Enabled {
		if c.HostMonitoring.IntervalSeconds <= 0 {
			return fmt.Errorf("HostMonitoring.IntervalSeconds must be greater than 0")
		}
		if c.HostMonitoring.QoS < 0 || c.HostMonitoring.QoS > 2 {
			return fmt.Errorf("HostMonitoring.QoS must be between 0 and 2")
		}
		if c.HostMonitoring.BaseTopic == "" {
			return fmt.Errorf("HostMonitoring.BaseTopic cannot be empty when enabled")
		}
	}
	switch c.EffectiveTCPSClientAuth() {
	case "", ClientAuthNone, ClientAuthRequest, ClientAuthRequired:
	default:
		return fmt.Errorf("invalid ClientAuth %q (must be one of NONE, REQUEST, REQUIRED)", c.EffectiveTCPSClientAuth())
	}
	return nil
}

// EffectiveTCPSClientAuth returns the client certificate verification mode for TCPS.
func (c *Config) EffectiveTCPSClientAuth() ClientAuthType {
	if c.TCPS.ClientAuth != "" {
		return c.TCPS.ClientAuth
	}
	if c.SSL.ClientAuth != "" {
		return c.SSL.ClientAuth
	}
	return ClientAuthNone
}

func (c *Config) EffectiveTCPSTrustStorePath() string {
	if c.TCPS.TrustStorePath != "" {
		return c.TCPS.TrustStorePath
	}
	return c.SSL.TrustStorePath
}

func (c *Config) EffectiveTCPSTrustStorePassword() string {
	if c.TCPS.TrustStorePassword != "" {
		return c.TCPS.TrustStorePassword
	}
	return c.SSL.TrustStorePassword
}

func (c *Config) EffectiveTCPSTrustStoreType() string {
	if c.TCPS.TrustStoreType != "" {
		return c.TCPS.TrustStoreType
	}
	if c.SSL.TrustStoreType != "" {
		return c.SSL.TrustStoreType
	}
	return "PEM"
}

func (c *Config) EffectiveUseIdentityAsUsername() bool {
	if c.TCPS.UseIdentityAsUsername != nil {
		return *c.TCPS.UseIdentityAsUsername
	}
	return c.SSL.UseIdentityAsUsername
}

func (c *Config) EffectiveAutoCreateUser() bool {
	if c.TCPS.AutoCreateUser != nil {
		return *c.TCPS.AutoCreateUser
	}
	return c.SSL.AutoCreateUser
}

func (c *Config) EffectiveTCPSKeyStorePath() string {
	if c.TCPS.KeyStorePath != "" {
		return c.TCPS.KeyStorePath
	}
	return c.SSL.KeyStorePath
}

func (c *Config) EffectiveTCPSKeyStorePassword() string {
	if c.TCPS.KeyStorePassword != "" {
		return c.TCPS.KeyStorePassword
	}
	return c.SSL.KeyStorePassword
}

func (c *Config) EffectiveTCPSKeyPath() string {
	return c.SSL.KeyPath
}

func (c *Config) EffectiveWSSKeyStorePath() string {
	if c.WSS.KeyStorePath != "" {
		return c.WSS.KeyStorePath
	}
	if c.SSL.WSS.KeyStorePath != "" {
		return c.SSL.WSS.KeyStorePath
	}
	return c.SSL.KeyStorePath
}

func (c *Config) EffectiveWSSKeyStorePassword() string {
	if c.WSS.KeyStorePassword != "" {
		return c.WSS.KeyStorePassword
	}
	if c.SSL.WSS.KeyStorePassword != "" {
		return c.SSL.WSS.KeyStorePassword
	}
	return c.SSL.KeyStorePassword
}

func (c *Config) EffectiveWSSKeyPath() string {
	if c.SSL.WSS.KeyPath != "" {
		return c.SSL.WSS.KeyPath
	}
	return c.SSL.KeyPath
}

// GetMaxQueueMessages returns the effective max queue size for offline sessions.
// If unset (nil), it returns 1000 for MEMORY queue store type, and 0 (unlimited) for other stores.
func (c *Config) GetMaxQueueMessages() int {
	if c.MaxQueueMessages != nil {
		return *c.MaxQueueMessages
	}
	if c.QueueStore() == StoreMemory {
		return 1000
	}
	return 0
}

// GetQueueBatchSize returns the configured bulk enqueue batch size, defaulting to 1000.
func (c *Config) GetQueueBatchSize() int {
	if c.QueueBatchSize != nil {
		return *c.QueueBatchSize
	}
	return 1000
}

// GetQueueFlushIntervalMs returns the configured bulk enqueue flush interval in milliseconds, defaulting to 50.
func (c *Config) GetQueueFlushIntervalMs() int {
	if c.QueueFlushIntervalMs != nil {
		return *c.QueueFlushIntervalMs
	}
	return 50
}
