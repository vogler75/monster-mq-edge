package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"monstermq.io/edge/internal/tlsutil"
)

type StoreType string

const (
	StoreNone     StoreType = "NONE"
	StoreMemory   StoreType = "MEMORY"
	StoreSQLite   StoreType = "SQLITE"
	StorePostgres StoreType = "POSTGRES"
	StoreMongoDB  StoreType = "MONGODB"
	// StoreWinCCOA keeps configs (ConfigStoreType), sessions
	// (SessionStoreType) or retained messages (RetainedStoreType) in WinCC OA
	// datapoints; only in the WinCC OA manager.
	StoreWinCCOA StoreType = "WINCCOA"
)

// validBackends is the set of store types that can back the persistent storage
// stack (sessions, config, etc.). MEMORY is allowed for selected volatile stores.
var validBackends = []StoreType{StoreSQLite, StorePostgres, StoreMongoDB}

// validRetainedBackends extends validBackends with MEMORY: when set, retained
// messages are not persisted to a database and not pre-loaded at startup —
// they are kept in the built-in in-memory map only.
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

// CrateDBConfig is the default CrateDB connection of archive groups
// (archiveType CRATEDB). Url may keep the JDBC form of the Java broker
// (jdbc:postgresql://host:5432/doc); User defaults to "crate".
type CrateDBConfig struct {
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
	Enabled                 bool   `yaml:"Enabled"`
	PasswordAlgorithm       string `yaml:"PasswordAlgorithm"`
	AnonymousEnabled        bool   `yaml:"AnonymousEnabled"`
	AclCacheEnabled         bool   `yaml:"AclCacheEnabled"`
	AclCheckOnSubscription  *bool  `yaml:"AclCheckOnSubscription,omitempty"`
	AllowAnonymousLocalhost bool   `yaml:"AllowAnonymousLocalhost"`
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
	Enabled                 bool   `yaml:"Enabled"`
	Address                 string `yaml:"Address,omitempty"`
	Port                    int    `yaml:"Port,omitempty"`
	TLSPort                 int    `yaml:"TLSPort,omitempty"`
	TLSAddress              string `yaml:"TLSAddress,omitempty"`
	RequireHTTPSFromOutside bool   `yaml:"RequireHTTPSFromOutside"`
	KeyStorePath            string `yaml:"KeyStorePath,omitempty"`
	KeyPath                 string `yaml:"KeyPath,omitempty"`
	KeyStorePassword        string `yaml:"KeyStorePassword,omitempty"`
}

// HTTPEnabled reports whether the HTTP listener is enabled (Port > 0).
func (g *GraphQLConfig) HTTPEnabled() bool {
	return g.Enabled && g.Port > 0
}

// TLSEnabled reports whether the HTTPS listener is enabled (TLSPort > 0).
func (g *GraphQLConfig) TLSEnabled() bool {
	return g.Enabled && g.TLSPort > 0
}

// UnmarshalYAML implements yaml.Unmarshaler so that when a GraphQL block
// is defined in YAML, Port and TLSPort are enabled only if explicitly defined (> 0).
func (g *GraphQLConfig) UnmarshalYAML(value *yaml.Node) error {
	type rawGraphQLConfig GraphQLConfig
	aux := rawGraphQLConfig{
		Enabled: true,
	}
	if err := value.Decode(&aux); err != nil {
		return err
	}
	*g = GraphQLConfig(aux)
	return nil
}

type DashboardConfig struct {
	Enabled bool   `yaml:"Enabled"`
	Path    string `yaml:"Path"`
}

type RestApiConfig struct {
	Enabled bool `yaml:"Enabled"`
}

type MCPConfig struct {
	Enabled bool `yaml:"Enabled"`
}

type HostMonitoringConfig struct {
	Enabled         bool   `yaml:"Enabled"`
	BaseTopic       string `yaml:"BaseTopic"`
	IntervalSeconds int    `yaml:"IntervalSeconds"`
	QoS             int    `yaml:"QoS"`
}

type HMIConfig struct {
	Enabled       bool   `yaml:"Enabled"`
	Path          string `yaml:"Path"`
	MountPath     string `yaml:"MountPath"`
	SyncEnabled   bool   `yaml:"SyncEnabled"`
	SyncBaseTopic string `yaml:"SyncBaseTopic"`
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
	RtspCamera         bool `yaml:"RtspCamera"`
	PythonScripts      bool `yaml:"PythonScripts"`
}

// WinCCOaNativeConfig is the bootstrap configuration for running the broker
// embedded in a WinCC OA API manager. It is read before any OA access and is
// ignored by the standalone binary, which has no embedding host.
type WinCCOaNativeConfig struct {
	Enabled   bool   `yaml:"Enabled"`
	Namespace bool   `yaml:"Namespace"` // <TopicRoot>/<SystemsName>/<system> namespace and writes
	TopicRoot string `yaml:"TopicRoot"` // first topic level(s), default "winccoa"
	TagsName  string `yaml:"TagsName"`  // level for tag access, default "tags"
	TypesName string `yaml:"TypesName"` // level for type access, default "types"
	// Every system is addressed as <TopicRoot>/<SystemsName>/<system>/....
	// LocalShortcut (default true) also offers the local system without the
	// system part: <TopicRoot>/<TagsName>/..., <TopicRoot>/<TypesName>/....
	LocalShortcut *bool  `yaml:"LocalShortcut"`
	SystemsName   string `yaml:"SystemsName"` // level before system names, default "systems"
	EchoPolicy    string `yaml:"EchoPolicy"`  // BROKER_TAG | NO_SOURCE
	// TopicsName is the level for MQTT topics kept in MMQTopic datapoints:
	// <TopicRoot>/<SystemsName>/<system>/<TopicsName>/<topic>, default "topics".
	TopicsName string `yaml:"TopicsName"`
	// TopicDpNames names MMQTopic datapoints by a hash of the topic (HASH,
	// default) or after the topic itself (NAME, e.g. MMQTopic_plant/line1). All brokers of a
	// distributed system must use the same naming.
	TopicDpNames string `yaml:"TopicDpNames"`

	// LegacyStores is the removed Stores list; set only to reject old configs.
	LegacyStores []string `yaml:"Stores,omitempty"`
}

const (
	WinCCOaEchoBrokerTag = "BROKER_TAG"
	WinCCOaEchoNoSource  = "NO_SOURCE"

	WinCCOaTopicDpHash = "HASH"
	WinCCOaTopicDpName = "NAME"
)

func (w *WinCCOaNativeConfig) validate() error {
	if w.EchoPolicy == "" {
		w.EchoPolicy = WinCCOaEchoBrokerTag
	}
	if w.TopicRoot == "" {
		w.TopicRoot = "winccoa"
	}
	if w.TagsName == "" {
		w.TagsName = "tags"
	}
	if w.TypesName == "" {
		w.TypesName = "types"
	}
	if w.SystemsName == "" {
		w.SystemsName = "systems"
	}
	if w.TopicsName == "" {
		w.TopicsName = "topics"
	}
	switch w.TopicDpNames {
	case "":
		w.TopicDpNames = WinCCOaTopicDpHash
	case WinCCOaTopicDpHash, WinCCOaTopicDpName:
	default:
		return fmt.Errorf("WinCCOaNative.TopicDpNames %q must be HASH or NAME", w.TopicDpNames)
	}
	switch w.EchoPolicy {
	case WinCCOaEchoBrokerTag, WinCCOaEchoNoSource:
	default:
		return fmt.Errorf("WinCCOaNative.EchoPolicy %q must be BROKER_TAG or NO_SOURCE", w.EchoPolicy)
	}
	if len(w.LegacyStores) > 0 {
		return fmt.Errorf("WinCCOaNative.Stores was removed; use ConfigStoreType: WINCCOA (device and archive configs) and SessionStoreType: WINCCOA")
	}
	return nil
}

// AllowRootWildcard returns the effective AllowRootWildcardSubscription.
func (c *Config) AllowRootWildcard() bool {
	return c.AllowRootWildcardSubscription == nil || *c.AllowRootWildcardSubscription
}

// Shortcut reports whether the local system is also reachable without the
// system part (LocalShortcut, default true).
func (w WinCCOaNativeConfig) Shortcut() bool {
	return w.LocalShortcut == nil || *w.LocalShortcut
}

// UsesWinCCOaStores reports whether configs, sessions or retained messages
// are kept in WinCC OA datapoints (store type WINCCOA).
func (c *Config) UsesWinCCOaStores() bool {
	return c.ConfigStore() == StoreWinCCOA || c.SessionStore() == StoreWinCCOA || c.RetainedStore() == StoreWinCCOA ||
		c.UserStore() == StoreWinCCOA
}

type PythonScriptsConfig struct {
	WorkerPoolSize   int   `yaml:"WorkerPoolSize"`
	QueueBufferSize  int   `yaml:"QueueBufferSize"`
	DefaultTimeoutMs int64 `yaml:"DefaultTimeoutMs"`
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
	UserStoreType     StoreType `yaml:"UserStoreType"` // users and ACL rules; only WINCCOA differs from DefaultStoreType

	SQLite   SQLiteConfig   `yaml:"SQLite"`
	Postgres PostgresConfig `yaml:"Postgres"`
	CrateDB  CrateDBConfig  `yaml:"CrateDB"`
	QuestDB  QuestDBConfig  `yaml:"QuestDB"`
	MongoDB  MongoDBConfig  `yaml:"MongoDB"`

	UserManagement UserManagementConfig `yaml:"UserManagement"`
	Metrics        MetricsConfig        `yaml:"Metrics"`
	Logging        LoggingConfig        `yaml:"Logging"`
	GraphQL        GraphQLConfig        `yaml:"GraphQL"`
	Dashboard      DashboardConfig      `yaml:"Dashboard"`
	RestApi        RestApiConfig        `yaml:"RestApi"`
	MCP            MCPConfig            `yaml:"MCP"`
	Features       FeaturesConfig       `yaml:"Features"`
	HostMonitoring HostMonitoringConfig `yaml:"HostMonitoring"`
	HMI            HMIConfig            `yaml:"HMI"`
	Redfish        RedfishConfig        `yaml:"Redfish"`
	PythonScripts  PythonScriptsConfig  `yaml:"PythonScripts"`
	WinCCOaNative  WinCCOaNativeConfig  `yaml:"WinCCOaNative"`
	Runtime        RuntimeConfig        `yaml:"Runtime"`
	PeerLink       PeerLinkConfig       `yaml:"PeerLink"`

	// QueuedMessagesEnabled selects how messages for offline persistent (clean=false)
	// sessions are held until the client reconnects.
	//
	//   true  → use QueueStoreType. Persistent queues survive broker restart;
	//           MEMORY queues are process-local.
	//   false → rely on the in-memory inflight buffer. Messages are lost
	//           on broker restart but lower latency / no DB writes per publish.
	QueuedMessagesEnabled bool `yaml:"QueuedMessagesEnabled"`
	// AllowRootWildcardSubscription permits subscribing to '#' (default
	// true, as in the Java broker). It also governs native WinCC OA
	// wildcard filters that cover every datapoint.
	AllowRootWildcardSubscription *bool `yaml:"AllowRootWildcardSubscription,omitempty"`
	MaxQueueMessages              *int  `yaml:"MaxQueueMessages"`
	QueueBatchSize                *int  `yaml:"QueueBatchSize"`
	QueueFlushIntervalMs          *int  `yaml:"QueueFlushIntervalMs"`

	// nodeIDOrigin tells where Validate took NodeID from; PeerLink refuses
	// the "edge" fallback, which is the same on every host.
	nodeIDOrigin nodeIDOrigin
}

type nodeIDOrigin int

const (
	nodeIDExplicit nodeIDOrigin = iota
	nodeIDHostname
	nodeIDFallback
)

// hostname is replaced in tests.
var hostname = os.Hostname

// resolvedNodeID returns the NodeId and its origin without changing the
// config, so configs built in code without Validate resolve the same way.
func (c *Config) resolvedNodeID() (string, nodeIDOrigin) {
	if c.NodeID != "" {
		return c.NodeID, c.nodeIDOrigin
	}
	if hn, err := hostname(); err == nil && hn != "" {
		return hn, nodeIDHostname
	}
	return "edge", nodeIDFallback
}

func Default() *Config {
	return &Config{
		NodeID:           "",
		TCP:              Listener{Enabled: true, Port: 1883},
		TCPS:             Listener{Enabled: false, Port: 8883},
		WS:               Listener{Enabled: false, Port: 1884},
		WSS:              Listener{Enabled: false, Port: 8884},
		MaxMessageSize:   1048576,
		DefaultStoreType: StoreSQLite,
		SQLite:           SQLiteConfig{Path: "./data/monstermq.db"},
		UserManagement:   UserManagementConfig{Enabled: false, PasswordAlgorithm: "BCRYPT", AnonymousEnabled: true, AclCacheEnabled: true, AllowAnonymousLocalhost: false},
		Metrics:          MetricsConfig{Enabled: true, CollectionIntervalSeconds: 1, RetentionHours: 168, MaxHistoryRows: 3600},
		Logging:          LoggingConfig{Level: "INFO", MqttSyslogEnabled: false, RingBufferSize: 1000},
		GraphQL: GraphQLConfig{
			Enabled:                 true,
			Port:                    4000,
			TLSPort:                 4443,
			RequireHTTPSFromOutside: false,
		},
		Dashboard: DashboardConfig{Enabled: true, Path: ""},
		RestApi:   RestApiConfig{Enabled: true},
		MCP:       MCPConfig{Enabled: false},
		Features:  FeaturesConfig{MqttClient: false, WinCCUa: false, WinCCOa: false, DeviceImportExport: false, Mcp: false, Hmi: false, Redfish: false, RtspCamera: false, PythonScripts: false},
		HostMonitoring: HostMonitoringConfig{
			Enabled:         false,
			BaseTopic:       "nodes/{NodeId}/host",
			IntervalSeconds: 10,
			QoS:             0,
		},
		HMI: HMIConfig{
			Enabled:       false,
			Path:          "./data/hmi",
			MountPath:     "/hmi",
			SyncEnabled:   true,
			SyncBaseTopic: defaultHMISyncBaseTopic,
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
		PythonScripts: PythonScriptsConfig{
			WorkerPoolSize:   4,
			QueueBufferSize:  1024,
			DefaultTimeoutMs: 200,
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

// QueueStore returns the offline-message queue store. WinCC OA does not
// hold queues: with DefaultStoreType WINCCOA it falls back to MEMORY.
func (c *Config) QueueStore() StoreType {
	if c.QueueStoreType != "" {
		return c.QueueStoreType
	}
	if c.DefaultStoreType == StoreWinCCOA {
		return StoreMemory
	}
	return c.DefaultStoreType
}

// MetricsStore returns the metrics store. Metrics are never written to
// WinCC OA: with DefaultStoreType WINCCOA it falls back to MEMORY.
func (c *Config) MetricsStore() StoreType {
	if c.Metrics.StoreType != "" {
		return c.Metrics.StoreType
	}
	if c.DefaultStoreType == StoreWinCCOA {
		return StoreMemory
	}
	return c.DefaultStoreType
}

// UserStore returns the store of users and ACL rules.
func (c *Config) UserStore() StoreType {
	if c.UserStoreType != "" {
		return c.UserStoreType
	}
	return c.DefaultStoreType
}

// Validate checks that all settings are recognised and self-consistent.
// Called after the YAML is parsed so the broker fails fast on bad config
// instead of silently falling back to a default.
func (c *Config) Validate() error {
	if c.NodeID == "" {
		c.NodeID, c.nodeIDOrigin = c.resolvedNodeID()
	}
	if c.MaxMessageSize < 0 {
		return fmt.Errorf("MaxMessageSize must be non-negative")
	}
	if err := c.WinCCOaNative.validate(); err != nil {
		return err
	}
	if c.DefaultStoreType == "" {
		return fmt.Errorf("DefaultStoreType is required")
	}
	if c.DefaultStoreType != StoreWinCCOA && !c.DefaultStoreType.isValidBackend() {
		return fmt.Errorf("invalid DefaultStoreType %q (must be one of SQLITE, POSTGRES, MONGODB, WINCCOA)", c.DefaultStoreType)
	}
	if c.UserStoreType != "" && c.UserStoreType != StoreWinCCOA && c.UserStoreType != c.DefaultStoreType {
		return fmt.Errorf("invalid UserStoreType %q (must be WINCCOA or DefaultStoreType %q)", c.UserStoreType, c.DefaultStoreType)
	}
	if c.DefaultStoreType == StoreWinCCOA {
		// Nothing else is opened: the other stores are WinCC OA datapoints
		// or kept in memory.
		for _, f := range []struct {
			name  string
			value StoreType
			mem   bool
		}{
			{"ConfigStoreType", c.ConfigStoreType, false},
			{"SessionStoreType", c.SessionStoreType, true},
			{"RetainedStoreType", c.RetainedStoreType, true},
			{"QueueStoreType", c.QueueStoreType, true},
		} {
			if f.value != "" && f.value != StoreWinCCOA && !(f.mem && f.value == StoreMemory) {
				return fmt.Errorf("%s %q is not supported with DefaultStoreType WINCCOA (use WINCCOA or MEMORY)", f.name, f.value)
			}
		}
		if c.Metrics.StoreType != "" && c.Metrics.StoreType != StoreMemory && c.Metrics.StoreType != StoreNone {
			return fmt.Errorf("Metrics.StoreType %q is not supported with DefaultStoreType WINCCOA (use MEMORY or NONE)", c.Metrics.StoreType)
		}
	}
	if c.ConfigStoreType != "" && c.ConfigStoreType != StoreWinCCOA && !c.ConfigStoreType.isValidBackend() {
		return fmt.Errorf("invalid ConfigStoreType %q (must be one of SQLITE, POSTGRES, MONGODB, WINCCOA)", c.ConfigStoreType)
	}
	if c.RetainedStoreType != "" && c.RetainedStoreType != StoreWinCCOA && !c.RetainedStoreType.isValidRetainedBackend() {
		return fmt.Errorf("invalid RetainedStoreType %q (must be one of SQLITE, POSTGRES, MONGODB, MEMORY, WINCCOA)", c.RetainedStoreType)
	}
	if c.SessionStoreType != "" && c.SessionStoreType != StoreWinCCOA && !c.SessionStoreType.isValidVolatileBackend() {
		return fmt.Errorf("invalid SessionStoreType %q (must be one of SQLITE, POSTGRES, MONGODB, MEMORY, WINCCOA)", c.SessionStoreType)
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
	if c.HMI.SyncBaseTopic == "" {
		c.HMI.SyncBaseTopic = defaultHMISyncBaseTopic
	}
	if c.Runtime.MemoryLimitMB < 0 {
		return fmt.Errorf("Runtime.MemoryLimitMB must be non-negative")
	}
	// A disabled PeerLink section is only decoded (strictly, in Load), so
	// the example block with placeholders stays valid.
	if c.PeerLink.Enabled {
		if _, err := c.ResolvePeerLink(); err != nil {
			return err
		}
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

func (c *Config) EffectiveGraphQLCertPath() string {
	if c.GraphQL.KeyStorePath != "" {
		return c.GraphQL.KeyStorePath
	}
	if c.SSL.KeyStorePath != "" {
		return c.SSL.KeyStorePath
	}
	return "data/server.crt"
}

func (c *Config) EffectiveGraphQLKeyPath() string {
	if c.GraphQL.KeyPath != "" {
		return c.GraphQL.KeyPath
	}
	if c.SSL.KeyPath != "" {
		return c.SSL.KeyPath
	}
	return "data/server.key"
}

func (c *Config) EffectiveGraphQLKeyPassword() string {
	if c.GraphQL.KeyStorePassword != "" {
		return c.GraphQL.KeyStorePassword
	}
	return c.SSL.KeyStorePassword
}

func (c *Config) EffectiveGraphQLTLSPort() int {
	return c.GraphQL.TLSPort
}

const defaultHMISyncBaseTopic = "monstermq/hmi/sync"

// RuntimeConfig holds settings of the Go runtime of the whole process.
type RuntimeConfig struct {
	// MemoryLimitMB is passed to debug.SetMemoryLimit; 0 keeps the Go default.
	MemoryLimitMB int `yaml:"MemoryLimitMB"`
}

// MemoryLimitBytes returns the soft memory limit in bytes, 0 when unset.
func (r RuntimeConfig) MemoryLimitBytes() int64 {
	return int64(r.MemoryLimitMB) << 20
}

// PeerLink defaults (plan-peerlink 18.1).
const (
	PeerLinkDefaultPort           = 1890
	peerLinkDefaultPreAuthPerIP   = 2
	peerLinkDefaultKeepAlive      = 10
	peerLinkDefaultMaxMessages    = 2000000
	peerLinkDefaultMaxBytes       = 256 << 20
	peerLinkRecordAllowance       = 64 << 10
	peerLinkDefaultDrainMs        = 2000
	peerLinkDefaultNeverConnected = 300
	peerLinkDefaultSnapshotTopics = 1000000
	peerLinkDefaultFetchRecords   = 4096
	peerLinkDefaultFetchBytes     = 1 << 20
	peerLinkDefaultFetchWaitMs    = 1000
	peerLinkDefaultPipeline       = 1
	peerLinkDefaultReconnectMaxMs = 30000
	peerLinkDefaultCatchUpFactor  = 3.0
	peerLinkDefaultMaxFrameBytes  = 16<<20 + 64<<10
	peerLinkDefaultInjectWorkers  = 1
	peerLinkDefaultCertPath       = "certs/peer-{NodeId}.pem"
	peerLinkDefaultKeyPath        = "certs/peer-{NodeId}.key"
)

const (
	PeerLinkTrustStorePEM    = "PEM"
	PeerLinkTrustStorePKCS12 = "PKCS12"

	PeerLinkIdentityNone = "NONE"
	PeerLinkIdentityDNS  = "DNS"
	PeerLinkIdentityCN   = "CN"

	PeerLinkSnapshotFill = "FILL"
	PeerLinkSnapshotOff  = "OFF"

	PeerLinkSharedSkip    = "SKIP"
	PeerLinkSharedDeliver = "DELIVER"
)

// PeerLinkConfig is the PeerLink section: pull-based in-memory forwarding of
// publishes between MonsterMQ Edge brokers. Load decodes it strictly, so an
// unknown key fails startup even while Enabled is false.
type PeerLinkConfig struct {
	Enabled                   bool             `yaml:"Enabled"`
	AllowUnauthenticatedPeers bool             `yaml:"AllowUnauthenticatedPeers"`
	Listener                  PeerLinkListener `yaml:"Listener"`
	Tls                       PeerLinkTLS      `yaml:"Tls"`
	SharedSecrets             []string         `yaml:"SharedSecrets"` // group secrets, base64, first = current
	KeepAliveSeconds          *int             `yaml:"KeepAliveSeconds"`
	Log                       PeerLinkLog      `yaml:"Log"`
	Capture                   PeerLinkCapture  `yaml:"Capture"`
	Snapshot                  PeerLinkSnapshot `yaml:"Snapshot"`
	Fetch                     PeerLinkFetch    `yaml:"Fetch"`
	Receive                   PeerLinkReceive  `yaml:"Receive"`
	Peers                     []PeerConfig     `yaml:"Peers"`
}

// PeerLinkListener is the peer port, bound when any peer has Serve.
type PeerLinkListener struct {
	Address         string   `yaml:"Address"`
	Port            int      `yaml:"Port"`            // 0 = 1890
	AllowedNetworks []string `yaml:"AllowedNetworks"` // CIDRs, checked before TLS
	MaxPreAuthPerIp *int     `yaml:"MaxPreAuthPerIp"`
	AllowPlaintext  bool     `yaml:"AllowPlaintext"` // TLS listener also accepts plaintext (migration)
}

// PeerLinkTLS is this node's identity and trust. Paths may contain {NodeId}.
type PeerLinkTLS struct {
	Enabled            bool           `yaml:"Enabled"` // listener TLS and the default for dialers
	CertPath           string         `yaml:"CertPath"`
	KeyPath            string         `yaml:"KeyPath"`
	TrustStorePath     string         `yaml:"TrustStorePath"`
	TrustStoreType     string         `yaml:"TrustStoreType"` // PEM | PKCS12
	TrustStorePassword string         `yaml:"TrustStorePassword"`
	ClientAuth         ClientAuthType `yaml:"ClientAuth"`
	IdentityFallback   string         `yaml:"IdentityFallback"` // NONE | DNS | CN
	AutoGenerate       bool           `yaml:"AutoGenerate"`
}

// PeerLinkLog bounds the in-memory log of captured publishes.
type PeerLinkLog struct {
	MaxMessages           *int   `yaml:"MaxMessages"`
	MaxBytes              *int64 `yaml:"MaxBytes"`
	MaxRecordBytes        int    `yaml:"MaxRecordBytes"` // 0 = MaxMessageSize + 64 KiB
	DrainOnShutdownMs     *int   `yaml:"DrainOnShutdownMs"`
	NeverConnectedWarnSec *int   `yaml:"NeverConnectedWarnSec"`
}

// PeerLinkCapture selects the publishes this node offers to its peers.
type PeerLinkCapture struct {
	Wills   *bool    `yaml:"Wills"`
	Include []string `yaml:"Include"` // empty = ["#"]
	// Exclude is a pointer so that YAML null (default) and [] (none) survive
	// a round trip; read it through GetExclude.
	Exclude        *[]string `yaml:"Exclude"`
	EchoSuppressMs int       `yaml:"EchoSuppressMs"`
}

// PeerLinkSnapshot controls the retained snapshot on first contact.
type PeerLinkSnapshot struct {
	Mode      string `yaml:"Mode"` // FILL | OFF
	MaxTopics *int   `yaml:"MaxTopics"`
}

// PeerLinkFetch controls how this node pulls from its sources.
type PeerLinkFetch struct {
	MaxRecords     *int `yaml:"MaxRecords"`
	MaxBytes       *int `yaml:"MaxBytes"`
	MaxWaitMs      *int `yaml:"MaxWaitMs"`
	LingerMs       int  `yaml:"LingerMs"`
	Pipeline       *int `yaml:"Pipeline"` // 1 | 2
	CrcOnTls       bool `yaml:"CrcOnTls"`
	ReconnectMaxMs *int `yaml:"ReconnectMaxMs"`
}

// PeerLinkReceive controls how replicas are applied on this node.
type PeerLinkReceive struct {
	Bus                 *bool    `yaml:"Bus"`
	BridgeOutbound      bool     `yaml:"BridgeOutbound"`
	Archive             *bool    `yaml:"Archive"`
	Queue               bool     `yaml:"Queue"`
	SharedSubscriptions string   `yaml:"SharedSubscriptions"` // SKIP | DELIVER
	MarkReplicas        bool     `yaml:"MarkReplicas"`
	CatchUpRateFactor   *float64 `yaml:"CatchUpRateFactor"` // 0 = no pacing
	MaxApplyRate        int      `yaml:"MaxApplyRate"`
	MaxRecordAgeMs      int      `yaml:"MaxRecordAgeMs"`
	MaxFrameBytes       *int     `yaml:"MaxFrameBytes"`
	InjectWorkers       *int     `yaml:"InjectWorkers"`
}

// PeerConfig is one entry of PeerLink.Peers. An Address means this node
// pulls from the peer; Serve (default true) lets the peer pull from this node.
type PeerConfig struct {
	NodeID        string      `yaml:"NodeId"`
	Address       string      `yaml:"Address"`
	Serve         *bool       `yaml:"Serve"`
	SharedSecrets []string    `yaml:"SharedSecrets"` // replaces the group secrets for this peer
	Tls           PeerTLS     `yaml:"Tls"`
	Receive       PeerReceive `yaml:"Receive"`
}

// PeerTLS holds the per-peer TLS overrides.
type PeerTLS struct {
	Enabled             *bool    `yaml:"Enabled"`      // dialer TLS; nil = PeerLink.Tls.Enabled
	PinnedSha256        []string `yaml:"PinnedSha256"` // hex SPKI or certificate SHA-256
	CertificateIdentity string   `yaml:"CertificateIdentity"`
	ServerName          string   `yaml:"ServerName"`
	RequireClientCert   bool     `yaml:"RequireClientCert"`
	InsecureSkipVerify  bool     `yaml:"InsecureSkipVerify"` // the dialer direction is then unauthenticated
}

// PeerReceive filters the records accepted from one peer.
type PeerReceive struct {
	Include []string `yaml:"Include"` // empty = ["#"]
	Exclude []string `yaml:"Exclude"`
}

func intOr(p *int, def int) int {
	if p != nil {
		return *p
	}
	return def
}

func boolOr(p *bool, def bool) bool {
	if p != nil {
		return *p
	}
	return def
}

func stringOr(s, def string) string {
	if s != "" {
		return s
	}
	return def
}

func (p *PeerLinkConfig) GetKeepAliveSeconds() int {
	return intOr(p.KeepAliveSeconds, peerLinkDefaultKeepAlive)
}

// DialerTLS reports whether this node dials the peer with TLS.
func (p *PeerLinkConfig) DialerTLS(peer PeerConfig) bool {
	return boolOr(peer.Tls.Enabled, p.Tls.Enabled)
}

// SecretsFor returns the shared secrets used with the peer: its own list, or
// the group secrets when it has none.
func (p *PeerLinkConfig) SecretsFor(peer PeerConfig) []string {
	if len(peer.SharedSecrets) > 0 {
		return peer.SharedSecrets
	}
	return p.SharedSecrets
}

func (l PeerLinkListener) ListenAddress() string { return stringOr(l.Address, "0.0.0.0") }

func (l PeerLinkListener) GetPort() int {
	if l.Port == 0 {
		return PeerLinkDefaultPort
	}
	return l.Port
}

func (l PeerLinkListener) GetMaxPreAuthPerIp() int {
	return intOr(l.MaxPreAuthPerIp, peerLinkDefaultPreAuthPerIP)
}

// GetCertPath returns CertPath; with AutoGenerate it defaults to
// certs/peer-{NodeId}.pem.
func (t PeerLinkTLS) GetCertPath() string {
	if t.CertPath == "" && t.AutoGenerate {
		return peerLinkDefaultCertPath
	}
	return t.CertPath
}

// GetKeyPath returns KeyPath; with AutoGenerate it defaults to
// certs/peer-{NodeId}.key.
func (t PeerLinkTLS) GetKeyPath() string {
	if t.KeyPath == "" && t.AutoGenerate {
		return peerLinkDefaultKeyPath
	}
	return t.KeyPath
}

func (t PeerLinkTLS) GetTrustStoreType() string {
	return stringOr(t.TrustStoreType, PeerLinkTrustStorePEM)
}

func (t PeerLinkTLS) GetClientAuth() ClientAuthType {
	if t.ClientAuth == "" {
		return ClientAuthNone
	}
	return t.ClientAuth
}

func (t PeerLinkTLS) GetIdentityFallback() string {
	return stringOr(t.IdentityFallback, PeerLinkIdentityNone)
}

func (l PeerLinkLog) GetMaxMessages() int { return intOr(l.MaxMessages, peerLinkDefaultMaxMessages) }

func (l PeerLinkLog) GetMaxBytes() int64 {
	if l.MaxBytes != nil {
		return *l.MaxBytes
	}
	return peerLinkDefaultMaxBytes
}

// GetMaxRecordBytes returns the largest record the log accepts. The default
// is the broker's MaxMessageSize (1 MiB when 0) plus 64 KiB for metadata.
func (l PeerLinkLog) GetMaxRecordBytes(maxMessageSize int) int {
	if l.MaxRecordBytes != 0 {
		return l.MaxRecordBytes
	}
	if maxMessageSize <= 0 {
		maxMessageSize = 1 << 20
	}
	return maxMessageSize + peerLinkRecordAllowance
}

func (l PeerLinkLog) GetDrainOnShutdownMs() int {
	return intOr(l.DrainOnShutdownMs, peerLinkDefaultDrainMs)
}

func (l PeerLinkLog) GetNeverConnectedWarnSec() int {
	return intOr(l.NeverConnectedWarnSec, peerLinkDefaultNeverConnected)
}

func (c PeerLinkCapture) GetWills() bool { return boolOr(c.Wills, true) }

func (c PeerLinkCapture) GetInclude() []string {
	if len(c.Include) == 0 {
		return []string{"#"}
	}
	return c.Include
}

// GetExclude returns the capture exclusions: nil Exclude means the HMI sync
// tree (<hmiBase>/#), an empty list means none.
func (c PeerLinkCapture) GetExclude(hmiBase string) []string {
	if c.Exclude != nil {
		return *c.Exclude
	}
	hmiBase = strings.TrimSuffix(stringOr(hmiBase, defaultHMISyncBaseTopic), "/")
	return []string{hmiBase + "/#"}
}

func (s PeerLinkSnapshot) GetMode() string { return stringOr(s.Mode, PeerLinkSnapshotFill) }

func (s PeerLinkSnapshot) GetMaxTopics() int {
	return intOr(s.MaxTopics, peerLinkDefaultSnapshotTopics)
}

func (f PeerLinkFetch) GetMaxRecords() int { return intOr(f.MaxRecords, peerLinkDefaultFetchRecords) }
func (f PeerLinkFetch) GetMaxBytes() int   { return intOr(f.MaxBytes, peerLinkDefaultFetchBytes) }
func (f PeerLinkFetch) GetMaxWaitMs() int  { return intOr(f.MaxWaitMs, peerLinkDefaultFetchWaitMs) }

// PeerLinkMinFetchWaitMs is the smallest Fetch.MaxWaitMs: an idle link answers EMPTY batches no
// faster than this.
const PeerLinkMinFetchWaitMs = 10

func (f PeerLinkFetch) GetPipeline() int { return intOr(f.Pipeline, peerLinkDefaultPipeline) }

func (f PeerLinkFetch) GetReconnectMaxMs() int {
	return intOr(f.ReconnectMaxMs, peerLinkDefaultReconnectMaxMs)
}

func (r PeerLinkReceive) GetBus() bool     { return boolOr(r.Bus, true) }
func (r PeerLinkReceive) GetArchive() bool { return boolOr(r.Archive, true) }

func (r PeerLinkReceive) GetSharedSubscriptions() string {
	return stringOr(r.SharedSubscriptions, PeerLinkSharedSkip)
}

func (r PeerLinkReceive) GetCatchUpRateFactor() float64 {
	if r.CatchUpRateFactor != nil {
		return *r.CatchUpRateFactor
	}
	return peerLinkDefaultCatchUpFactor
}

func (r PeerLinkReceive) GetMaxFrameBytes() int {
	return intOr(r.MaxFrameBytes, peerLinkDefaultMaxFrameBytes)
}

func (r PeerLinkReceive) GetInjectWorkers() int {
	return intOr(r.InjectWorkers, peerLinkDefaultInjectWorkers)
}

// GetServe reports whether the peer may pull from this node (default true).
func (p PeerConfig) GetServe() bool { return boolOr(p.Serve, true) }

// Pulls reports whether this node pulls from the peer.
func (p PeerConfig) Pulls() bool { return p.Address != "" }

func (r PeerReceive) GetInclude() []string {
	if len(r.Include) == 0 {
		return []string{"#"}
	}
	return r.Include
}

// CanonicalNodeID returns the form of a NodeId that PeerLink compares, sends
// in HELLO and binds to certificates: lower case, [a-z0-9._-], 1-64 bytes.
func CanonicalNodeID(id string) (string, error) {
	c := strings.ToLower(id)
	if len(c) == 0 || len(c) > 64 {
		return "", fmt.Errorf("NodeId %q must have 1 to 64 characters", id)
	}
	for i := 0; i < len(c); i++ {
		b := c[i]
		if !(b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '.' || b == '_' || b == '-') {
			return "", fmt.Errorf("NodeId %q may only contain letters, digits, '.', '_' and '-'", id)
		}
	}
	return c, nil
}

// PeerLinkSetup is the canonical form of a valid PeerLink section.
type PeerLinkSetup struct {
	NodeID   string       // canonical own NodeId
	Peers    []PeerConfig // without this node's own entry; NodeID canonical
	Infos    []string     // to log at INFO on start
	Warnings []string     // to log at WARN on start
}

// AnyServe reports whether any peer may pull from this node, i.e. whether
// the peer listener is needed.
func (s *PeerLinkSetup) AnyServe() bool {
	for _, peer := range s.Peers {
		if peer.GetServe() {
			return true
		}
	}
	return false
}

// ResolvePeerLink checks the PeerLink section against the rest of the
// configuration and returns its canonical form. Validate calls it when
// PeerLink is enabled; the broker calls it again to log the notices. It
// does not check that certificate files exist.
func (c *Config) ResolvePeerLink() (*PeerLinkSetup, error) {
	id, origin := c.resolvedNodeID()
	hn, err := hostname()
	if err != nil {
		hn = ""
	}
	return c.PeerLink.validate(peerLinkEnv{
		nodeID:         id,
		nodeIDOrigin:   origin,
		hostname:       hn,
		userMgmt:       c.UserManagement.Enabled,
		retainedStore:  c.RetainedStore(),
		tcpsTrustStore: c.EffectiveTCPSTrustStorePath(),
		maxMessageSize: c.MaxMessageSize,
		hmiBase:        c.HMI.SyncBaseTopic,
		memoryLimitMB:  c.Runtime.MemoryLimitMB,
	})
}

type peerLinkEnv struct {
	nodeID         string
	nodeIDOrigin   nodeIDOrigin
	hostname       string
	userMgmt       bool
	retainedStore  StoreType
	tcpsTrustStore string
	maxMessageSize int
	hmiBase        string
	memoryLimitMB  int
}

func (p *PeerLinkConfig) validate(env peerLinkEnv) (*PeerLinkSetup, error) {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("PeerLink."+format, args...))
	}
	s := &PeerLinkSetup{}

	if env.nodeIDOrigin == nodeIDFallback {
		errs = append(errs, fmt.Errorf("PeerLink needs a NodeId that differs per host: the hostname is unknown and the fallback %q is the same everywhere; set NodeId", env.nodeID))
	} else if own, err := CanonicalNodeID(env.nodeID); err != nil {
		errs = append(errs, fmt.Errorf("PeerLink: %w", err))
	} else {
		s.NodeID = own
	}

	tls := &p.Tls
	clientAuth := tls.GetClientAuth()
	switch clientAuth {
	case ClientAuthNone, ClientAuthRequest, ClientAuthRequired:
	default:
		fail("Tls.ClientAuth %q must be NONE, REQUEST or REQUIRED", tls.ClientAuth)
	}
	switch tls.GetTrustStoreType() {
	case PeerLinkTrustStorePEM, PeerLinkTrustStorePKCS12:
	default:
		fail("Tls.TrustStoreType %q must be PEM or PKCS12", tls.TrustStoreType)
	}
	switch tls.GetIdentityFallback() {
	case PeerLinkIdentityNone, PeerLinkIdentityDNS, PeerLinkIdentityCN:
	default:
		fail("Tls.IdentityFallback %q must be NONE, DNS or CN", tls.IdentityFallback)
	}
	if tls.Enabled && !tls.AutoGenerate && (tls.CertPath == "" || tls.KeyPath == "") {
		fail("Tls.Enabled needs Tls.CertPath and Tls.KeyPath, or Tls.AutoGenerate")
	}
	if clientAuth != ClientAuthNone && !tls.Enabled {
		fail("Tls.ClientAuth %s needs Tls.Enabled", clientAuth)
	}
	for i, sec := range p.SharedSecrets {
		if err := checkPeerSecret(sec); err != nil {
			fail("SharedSecrets[%d]: %v", i, err)
		}
	}
	if len(p.SharedSecrets) > 0 && !tls.Enabled {
		fail("SharedSecrets need Tls.Enabled: without TLS the secret cannot be bound to the connection")
	}

	l := &p.Listener
	if l.Port < 0 || l.Port > 65535 {
		fail("Listener.Port %d must be 1..65535 (0 = %d)", l.Port, PeerLinkDefaultPort)
	}
	for i, n := range l.AllowedNetworks {
		if _, _, err := net.ParseCIDR(n); err != nil {
			fail("Listener.AllowedNetworks[%d] %q is not a CIDR", i, n)
		}
	}
	if l.GetMaxPreAuthPerIp() < 1 {
		fail("Listener.MaxPreAuthPerIp must be at least 1")
	}

	waiver := p.AllowUnauthenticatedPeers
	if waiver {
		if len(l.AllowedNetworks) == 0 {
			fail("AllowUnauthenticatedPeers needs a non-empty Listener.AllowedNetworks")
		}
		if env.userMgmt {
			fail("AllowUnauthenticatedPeers is not allowed with UserManagement.Enabled: replicas are injected without ACL checks")
		}
	}

	var hostLabel string
	if env.nodeIDOrigin == nodeIDHostname {
		first, _, _ := strings.Cut(env.hostname, ".")
		hostLabel = strings.ToLower(first)
	}
	seen := make(map[string]int, len(p.Peers))
	others, ownMatched, exactOwn := 0, false, false
	adopted := ""
	groupSecretUsers := 0
	for i, peer := range p.Peers {
		id, err := CanonicalNodeID(peer.NodeID)
		if err != nil {
			fail("Peers[%d]: %v", i, err)
			others++
			continue
		}
		if j, dup := seen[id]; dup {
			fail("Peers[%d]: NodeId %q is already used by Peers[%d] (NodeIds are compared in lower case)", i, peer.NodeID, j)
			continue
		}
		seen[id] = i
		if s.NodeID != "" && (id == s.NodeID || (hostLabel != "" && id == hostLabel)) {
			ownMatched = true
			if id == s.NodeID {
				exactOwn = true
			} else {
				adopted = id
			}
			s.Infos = append(s.Infos, fmt.Sprintf("PeerLink: Peers[%d] %q is this node and is ignored", i, peer.NodeID))
			continue
		}
		others++
		peer.NodeID = id
		where := fmt.Sprintf("Peers[%d] (%s)", i, id)
		serve, pull := peer.GetServe(), peer.Pulls()
		if !serve && !pull {
			fail("%s needs an Address (this node pulls from it) or Serve: true (it pulls from this node)", where)
		}
		if pull {
			if err := checkPeerAddress(peer.Address); err != nil {
				fail("%s.Address: %v", where, err)
			}
		}
		for k, sec := range peer.SharedSecrets {
			if err := checkPeerSecret(sec); err != nil {
				fail("%s.SharedSecrets[%d]: %v", where, k, err)
			}
		}
		for k, pin := range peer.Tls.PinnedSha256 {
			if !validPeerPin(pin) {
				fail("%s.Tls.PinnedSha256[%d] %q must be 64 hex digits (SHA-256 of the SPKI or certificate)", where, k, pin)
			}
		}
		for k, f := range peer.Receive.GetInclude() {
			if !validPeerFilter(f) {
				fail("%s.Receive.Include[%d] %q is not a valid topic filter", where, k, f)
			}
		}
		for k, f := range peer.Receive.Exclude {
			if !validPeerFilter(f) {
				fail("%s.Receive.Exclude[%d] %q is not a valid topic filter", where, k, f)
			}
		}

		secrets := p.SecretsFor(peer)
		if len(peer.SharedSecrets) == 0 && len(p.SharedSecrets) > 0 {
			groupSecretUsers++
		}
		dialTLS := pull && p.DialerTLS(peer)
		listenTLS := serve && tls.Enabled
		pins := len(peer.Tls.PinnedSha256) > 0
		trust := tls.TrustStorePath != "" || pins

		if len(secrets) > 0 && ((serve && !tls.Enabled) || (pull && !dialTLS)) {
			fail("%s: SharedSecrets need TLS in every direction they are used (Tls.Enabled for Serve, the dialer Tls.Enabled for Address)", where)
		}
		if pins && !dialTLS && !listenTLS {
			fail("%s.Tls.PinnedSha256 needs TLS", where)
		}
		if peer.Tls.RequireClientCert && clientAuth == ClientAuthNone {
			fail("%s.Tls.RequireClientCert needs Tls.ClientAuth REQUEST or REQUIRED", where)
		}
		if serve && clientAuth != ClientAuthNone && !trust {
			fail("%s: Tls.ClientAuth %s needs Tls.TrustStorePath or PinnedSha256 for this peer", where, clientAuth)
		}
		if dialTLS && !peer.Tls.InsecureSkipVerify && !trust && len(secrets) == 0 {
			fail("%s: the dialer has no truststore, pin or shared secret to verify the peer; set Tls.InsecureSkipVerify: true to connect unauthenticated", where)
		}
		if !waiver {
			certRequired := clientAuth == ClientAuthRequired || (clientAuth == ClientAuthRequest && peer.Tls.RequireClientCert)
			if serve && !(listenTLS && (len(secrets) > 0 || (certRequired && trust))) {
				fail("%s is not authenticated when it pulls from this node: use TLS with a client certificate (Tls.ClientAuth REQUIRED, or REQUEST with RequireClientCert) or SharedSecrets, or set AllowUnauthenticatedPeers", where)
			}
			if pull && !(dialTLS && (len(secrets) > 0 || (!peer.Tls.InsecureSkipVerify && trust))) {
				fail("%s is not authenticated when this node pulls from it: use TLS with Tls.TrustStorePath or PinnedSha256, or SharedSecrets, or set AllowUnauthenticatedPeers", where)
			}
		}
		s.Peers = append(s.Peers, peer)
	}
	if others == 0 {
		fail("Peers: at least one peer other than this node is required")
	}
	if adopted != "" && !exactOwn {
		// The peers of a shared file know this node by its entry, so the link speaks that id
		// (HELLO, HELLO_OK, MAC, certificate URI SAN, injector ids), not the full hostname.
		s.Infos = append(s.Infos, fmt.Sprintf("PeerLink: NodeId %q (from the hostname) is used as %q, its Peers entry", s.NodeID, adopted))
		s.NodeID = adopted
	}
	if l.AllowPlaintext && tls.Enabled && s.AnyServe() && !waiver {
		fail("Listener.AllowPlaintext admits unauthenticated plaintext sessions; it needs AllowUnauthenticatedPeers")
	}
	if len(p.Peers) >= 2 && !ownMatched && s.NodeID != "" {
		s.Warnings = append(s.Warnings, fmt.Sprintf("PeerLink: no Peers entry matches this node (NodeId %q, hostname %q); if this file is shared, no entry matches this host", s.NodeID, env.hostname))
	}

	maxRecord := p.Log.GetMaxRecordBytes(env.maxMessageSize)
	maxBytes := p.Log.GetMaxBytes()
	fetchRecords := p.Fetch.GetMaxRecords()
	if p.Log.MaxRecordBytes < 0 {
		fail("Log.MaxRecordBytes must be non-negative (0 = MaxMessageSize + 64 KiB)")
	}
	if n := p.Log.GetMaxMessages(); n < max(100, fetchRecords) {
		fail("Log.MaxMessages %d must be at least 100 and at least Fetch.MaxRecords (%d)", n, fetchRecords)
	}
	if maxBytes < 1<<20 || maxBytes < 4*int64(maxRecord) {
		fail("Log.MaxBytes %d must be at least 1 MiB and at least 4 x MaxRecordBytes (%d)", maxBytes, maxRecord)
	}
	if p.Log.GetDrainOnShutdownMs() < 0 {
		fail("Log.DrainOnShutdownMs must be non-negative")
	}
	if p.Log.GetNeverConnectedWarnSec() < 0 {
		fail("Log.NeverConnectedWarnSec must be non-negative")
	}
	keepAlive := p.GetKeepAliveSeconds()
	if keepAlive < 1 {
		fail("KeepAliveSeconds must be at least 1")
	}

	for k, f := range p.Capture.GetInclude() {
		if !validPeerFilter(f) {
			fail("Capture.Include[%d] %q is not a valid topic filter", k, f)
		}
	}
	for k, f := range p.Capture.GetExclude(env.hmiBase) {
		if !validPeerFilter(f) {
			fail("Capture.Exclude[%d] %q is not a valid topic filter", k, f)
		}
	}
	if p.Capture.EchoSuppressMs < 0 {
		fail("Capture.EchoSuppressMs must be non-negative")
	}

	switch p.Snapshot.GetMode() {
	case PeerLinkSnapshotFill, PeerLinkSnapshotOff:
	default:
		fail("Snapshot.Mode %q must be FILL or OFF", p.Snapshot.Mode)
	}
	if p.Snapshot.GetMaxTopics() < 1 {
		fail("Snapshot.MaxTopics must be at least 1")
	}

	f := &p.Fetch
	if fetchRecords < 1 {
		fail("Fetch.MaxRecords must be at least 1")
	}
	if f.GetMaxBytes() < 1 {
		fail("Fetch.MaxBytes must be at least 1")
	}
	if w := f.GetMaxWaitMs(); w < PeerLinkMinFetchWaitMs || w >= keepAlive*1000 {
		fail("Fetch.MaxWaitMs %d must be at least %d and below KeepAliveSeconds*1000 (%d); a shorter long poll turns an idle link into a busy loop",
			w, PeerLinkMinFetchWaitMs, keepAlive*1000)
	}
	if f.LingerMs < 0 {
		fail("Fetch.LingerMs must be non-negative")
	}
	if pl := f.GetPipeline(); pl < 1 || pl > 2 {
		fail("Fetch.Pipeline %d must be 1 or 2", pl)
	}
	if f.GetReconnectMaxMs() < 1 {
		fail("Fetch.ReconnectMaxMs must be positive")
	}

	r := &p.Receive
	switch r.GetSharedSubscriptions() {
	case PeerLinkSharedSkip, PeerLinkSharedDeliver:
	default:
		fail("Receive.SharedSubscriptions %q must be SKIP or DELIVER", r.SharedSubscriptions)
	}
	if cf := r.GetCatchUpRateFactor(); cf != 0 && !(cf >= 1.5) {
		fail("Receive.CatchUpRateFactor %v must be 0 (no pacing) or at least 1.5", cf)
	}
	if r.MaxApplyRate < 0 {
		fail("Receive.MaxApplyRate must be non-negative")
	}
	if r.MaxRecordAgeMs < 0 {
		fail("Receive.MaxRecordAgeMs must be non-negative")
	}
	if mf, need := r.GetMaxFrameBytes(), f.GetMaxBytes()+peerLinkRecordAllowance; mf < need {
		fail("Receive.MaxFrameBytes %d must be at least Fetch.MaxBytes + 64 KiB (%d)", mf, need)
	}
	if w := r.GetInjectWorkers(); w < 1 || w > 16 {
		fail("Receive.InjectWorkers %d must be 1..16", w)
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	if waiver {
		s.Warnings = append(s.Warnings, fmt.Sprintf("PeerLink: AllowUnauthenticatedPeers is set; peers are admitted by network address only (%s)", strings.Join(l.AllowedNetworks, ", ")))
	}
	if groupSecretUsers > 0 && len(s.Peers)+1 > 2 {
		s.Warnings = append(s.Warnings, "PeerLink: the group SharedSecrets are used with more than two nodes; any holder can claim any NodeId of the group, prefer per-peer SharedSecrets")
	}
	if tls.TrustStorePath != "" && env.tcpsTrustStore != "" &&
		filepath.Clean(strings.ReplaceAll(tls.TrustStorePath, "{NodeId}", s.NodeID)) == filepath.Clean(env.tcpsTrustStore) {
		s.Warnings = append(s.Warnings, "PeerLink: Tls.TrustStorePath is the TCPS truststore; a certificate issued for an MQTT client could claim a NodeId, use a dedicated peer CA")
	}
	if env.retainedStore == StoreMemory && p.Snapshot.GetMode() == PeerLinkSnapshotOff {
		s.Warnings = append(s.Warnings, "PeerLink: RetainedStoreType MEMORY with Snapshot.Mode OFF: retained messages of peers are lost when this node restarts")
	}
	if env.memoryLimitMB > 0 {
		if needMB := 2.2*float64(maxBytes)/(1<<20) + 150; needMB > float64(env.memoryLimitMB) {
			s.Warnings = append(s.Warnings, fmt.Sprintf("PeerLink: Runtime.MemoryLimitMB %d is below 2.2 x Log.MaxBytes + 150 MiB (%.0f MiB); the GC may run continuously while the log is full", env.memoryLimitMB, needMB))
		}
	}
	return s, nil
}

func checkPeerAddress(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" {
		return fmt.Errorf("%q has no host", addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%q: port must be 1..65535", addr)
	}
	return nil
}

// checkPeerSecret validates a base64 shared secret. The error never
// contains the secret.
func checkPeerSecret(s string) error {
	_, err := tlsutil.DecodeSecret(s)
	return err
}

func validPeerPin(s string) bool {
	_, err := tlsutil.ParsePin(s)
	return err == nil
}

// validPeerFilter checks MQTT topic filter syntax: '+' and '#' occupy a
// whole level, '#' only the last one.
func validPeerFilter(f string) bool {
	if f == "" || !utf8.ValidString(f) || strings.IndexByte(f, 0) >= 0 || len(f) > 65535 {
		return false
	}
	levels := strings.Split(f, "/")
	for i, lv := range levels {
		if strings.ContainsAny(lv, "+#") && lv != "+" && lv != "#" {
			return false
		}
		if lv == "#" && i != len(levels)-1 {
			return false
		}
	}
	return true
}
