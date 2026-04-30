// Package winccua implements the WinCC Unified bridge — a port of
// monster-mq's devices/winccua/{WinCCUaConnector,WinCCUaPipeConnector,
// WinCCUaPublisher,WinCCUaExtension}.kt.
//
// Two transports share the downstream publish path (topic transform, payload
// formatting, MQTT envelope): GraphQL (HTTP login + WebSocket subscriptions)
// and OpenPipe (local named pipe to WinCC Unified Runtime, expert/JSON syntax).
//
// The persisted JSON config shape (DeviceConfig.Config column) is identical to
// the Java broker's WinCCUaConnectionConfig.toJsonObject(), so the same
// physical SQLite/Postgres/Mongo row opens cleanly in either broker.
package winccua

import (
	"encoding/json"
	"fmt"
	"regexp"
	"runtime"
	"strings"
)

const (
	AddressTypeTagValues    = "TAG_VALUES"
	AddressTypeActiveAlarms = "ACTIVE_ALARMS"

	FormatJSONISO  = "JSON_ISO"
	FormatJSONMS   = "JSON_MS"
	FormatRawValue = "RAW_VALUE"

	ModeGraphQL  = "GRAPHQL"
	ModeOpenPipe = "OPENPIPE"

	PipeTagModeSingle = "SINGLE"
	PipeTagModeBulk   = "BULK"

	DefaultPipePathWindows = `\\.\pipe\HmiRuntime`
	DefaultPipePathLinux   = "/tmp/HmiRuntime"

	DeviceTypeWinCCUaClient = "WinCCUA-Client"
)

// Address mirrors WinCCUaAddress (stores/devices/WinCCUaConfig.kt).
type Address struct {
	Type           string   `json:"type"`
	Topic          string   `json:"topic"`
	Description    string   `json:"description,omitempty"`
	Retained       bool     `json:"retained,omitempty"`
	NameFilters    []string `json:"nameFilters,omitempty"`
	IncludeQuality bool     `json:"includeQuality,omitempty"`
	SystemNames    []string `json:"systemNames,omitempty"`
	FilterString   string   `json:"filterString,omitempty"`
	PipeTagMode    string   `json:"pipeTagMode,omitempty"`
}

func (a Address) Validate() []string {
	var errs []string
	if strings.TrimSpace(a.Topic) == "" {
		errs = append(errs, "topic cannot be blank")
	}
	switch a.Type {
	case AddressTypeTagValues:
		if len(a.NameFilters) == 0 {
			errs = append(errs, "nameFilters is required for TAG_VALUES address type")
		}
	case AddressTypeActiveAlarms:
		// systemNames and filterString optional
	default:
		errs = append(errs, fmt.Sprintf("unknown address type %q", a.Type))
	}
	return errs
}

// TransformConfig mirrors WinCCUaTransformConfig.
type TransformConfig struct {
	ConvertDotToSlash        bool   `json:"convertDotToSlash"`
	ConvertUnderscoreToSlash bool   `json:"convertUnderscoreToSlash"`
	RegexPattern             string `json:"regexPattern,omitempty"`
	RegexReplacement         string `json:"regexReplacement,omitempty"`

	compiledRegex *regexp.Regexp
}

func (t TransformConfig) Validate() []string {
	var errs []string
	if t.RegexPattern != "" {
		if _, err := regexp.Compile(t.RegexPattern); err != nil {
			errs = append(errs, fmt.Sprintf("invalid regex pattern: %v", err))
		}
	}
	if t.RegexReplacement != "" && t.RegexPattern == "" {
		errs = append(errs, "regexReplacement requires regexPattern to be set")
	}
	return errs
}

// TransformTagName converts a tag name to the corresponding MQTT topic
// fragment using the configured rules. Rules apply in this order:
//   1. trim trailing dots,
//   2. dot → slash (if enabled),
//   3. underscore → slash (if enabled),
//   4. regex replace (if pattern set),
//   5. trim leading/trailing slashes.
func (t *TransformConfig) TransformTagName(tagName string) string {
	r := strings.TrimRight(tagName, ".")
	if t.ConvertDotToSlash {
		r = strings.ReplaceAll(r, ".", "/")
	}
	if t.ConvertUnderscoreToSlash {
		r = strings.ReplaceAll(r, "_", "/")
	}
	if t.RegexPattern != "" {
		if t.compiledRegex == nil {
			re, err := regexp.Compile(t.RegexPattern)
			if err == nil {
				t.compiledRegex = re
			}
		}
		if t.compiledRegex != nil {
			r = t.compiledRegex.ReplaceAllString(r, t.RegexReplacement)
		}
	}
	return strings.Trim(r, "/")
}

// ConnectionConfig mirrors WinCCUaConnectionConfig — the JSON payload stored
// in DeviceConfig.Config.
type ConnectionConfig struct {
	GraphqlEndpoint   string          `json:"graphqlEndpoint,omitempty"`
	WebsocketEndpoint string          `json:"websocketEndpoint,omitempty"`
	Username          string          `json:"username,omitempty"`
	Password          string          `json:"password,omitempty"`
	ReconnectDelay    int64           `json:"reconnectDelay,omitempty"`
	ConnectionTimeout int64           `json:"connectionTimeout,omitempty"`
	Addresses         []Address       `json:"addresses,omitempty"`
	TransformConfig   TransformConfig `json:"transformConfig"`
	MessageFormat     string          `json:"messageFormat,omitempty"`
	DataAccessMode    string          `json:"dataAccessMode,omitempty"`
	PipePath          string          `json:"pipePath,omitempty"`
}

// applyDefaults fills any zero-valued fields with the same defaults the Java
// broker uses in WinCCUaConnectionConfig.fromJsonObject.
func (c *ConnectionConfig) applyDefaults() {
	if c.GraphqlEndpoint == "" {
		c.GraphqlEndpoint = "http://winccua:4000/graphql"
	}
	if c.ReconnectDelay == 0 {
		c.ReconnectDelay = 5000
	}
	if c.ConnectionTimeout == 0 {
		c.ConnectionTimeout = 10000
	}
	if c.MessageFormat == "" {
		c.MessageFormat = FormatJSONISO
	}
	if c.DataAccessMode == "" {
		c.DataAccessMode = ModeGraphQL
	}
	for i := range c.Addresses {
		if c.Addresses[i].PipeTagMode == "" && c.Addresses[i].Type == AddressTypeTagValues {
			c.Addresses[i].PipeTagMode = PipeTagModeSingle
		}
	}
}

func ParseConnectionConfig(raw string) (*ConnectionConfig, error) {
	cfg := &ConnectionConfig{}
	if strings.TrimSpace(raw) == "" {
		cfg.applyDefaults()
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(raw), cfg); err != nil {
		return nil, fmt.Errorf("winccua config: %w", err)
	}
	cfg.applyDefaults()
	return cfg, nil
}

func (c *ConnectionConfig) Validate() []string {
	var errs []string

	if c.DataAccessMode != ModeGraphQL && c.DataAccessMode != ModeOpenPipe {
		errs = append(errs, fmt.Sprintf("dataAccessMode must be one of: %s, %s", ModeGraphQL, ModeOpenPipe))
	}

	switch c.DataAccessMode {
	case ModeGraphQL:
		if strings.TrimSpace(c.GraphqlEndpoint) == "" {
			errs = append(errs, "graphqlEndpoint cannot be blank")
		} else if !strings.HasPrefix(c.GraphqlEndpoint, "http://") && !strings.HasPrefix(c.GraphqlEndpoint, "https://") {
			errs = append(errs, "graphqlEndpoint must start with http:// or https://")
		}
		if strings.TrimSpace(c.Username) == "" {
			errs = append(errs, "username cannot be blank")
		}
		if strings.TrimSpace(c.Password) == "" {
			errs = append(errs, "password cannot be blank")
		}
	case ModeOpenPipe:
		// PipePath is optional; OS default applies when blank. Explicit blank
		// (non-nil but empty) is rejected to mirror the Kotlin behavior.
	}

	if c.ReconnectDelay < 1000 {
		errs = append(errs, "reconnectDelay should be at least 1000ms")
	}
	if c.ConnectionTimeout < 1000 {
		errs = append(errs, "connectionTimeout should be at least 1000ms")
	}

	switch c.MessageFormat {
	case FormatJSONISO, FormatJSONMS, FormatRawValue:
	default:
		errs = append(errs, fmt.Sprintf("messageFormat must be one of: %s, %s, %s", FormatJSONISO, FormatJSONMS, FormatRawValue))
	}

	for i, a := range c.Addresses {
		for _, e := range a.Validate() {
			errs = append(errs, fmt.Sprintf("Address %d: %s", i, e))
		}
	}
	for _, e := range c.TransformConfig.Validate() {
		errs = append(errs, fmt.Sprintf("Transform config: %s", e))
	}
	return errs
}

// WebSocketEndpoint returns the configured ws/wss endpoint, deriving it from
// graphqlEndpoint if not set.
func (c *ConnectionConfig) WebSocketEndpoint() string {
	if c.WebsocketEndpoint != "" {
		return c.WebsocketEndpoint
	}
	switch {
	case strings.HasPrefix(c.GraphqlEndpoint, "https://"):
		return "wss://" + strings.TrimPrefix(c.GraphqlEndpoint, "https://")
	case strings.HasPrefix(c.GraphqlEndpoint, "http://"):
		return "ws://" + strings.TrimPrefix(c.GraphqlEndpoint, "http://")
	}
	return c.GraphqlEndpoint
}

// ResolvePipePath returns the configured pipe path or the OS default.
func (c *ConnectionConfig) ResolvePipePath() string {
	if strings.TrimSpace(c.PipePath) != "" {
		return c.PipePath
	}
	if runtime.GOOS == "windows" {
		return DefaultPipePathWindows
	}
	return DefaultPipePathLinux
}
