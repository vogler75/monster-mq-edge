// Package winccoa implements the WinCC Open Architecture GraphQL bridge.
package winccoa

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const (
	FormatJSONISO  = "JSON_ISO"
	FormatJSONMS   = "JSON_MS"
	FormatRawValue = "RAW_VALUE"

	DeviceTypeWinCCOaClient = "WinCCOA-Client"
)

type Address struct {
	Query       string `json:"query"`
	Topic       string `json:"topic"`
	Description string `json:"description,omitempty"`
	Answer      bool   `json:"answer,omitempty"`
	Retained    bool   `json:"retained,omitempty"`
}

func (a Address) Validate() []string {
	var errs []string
	if strings.TrimSpace(a.Query) == "" {
		errs = append(errs, "query cannot be blank")
	}
	if strings.TrimSpace(a.Topic) == "" {
		errs = append(errs, "topic cannot be blank")
	}
	return errs
}

type TransformConfig struct {
	RemoveSystemName         bool   `json:"removeSystemName"`
	ConvertDotToSlash        bool   `json:"convertDotToSlash"`
	ConvertUnderscoreToSlash bool   `json:"convertUnderscoreToSlash"`
	RegexPattern             string `json:"regexPattern,omitempty"`
	RegexReplacement         string `json:"regexReplacement,omitempty"`

	compiledRegex *regexp.Regexp
}

type transformConfigJSON struct {
	RemoveSystemName         *bool  `json:"removeSystemName"`
	ConvertDotToSlash        *bool  `json:"convertDotToSlash"`
	ConvertUnderscoreToSlash *bool  `json:"convertUnderscoreToSlash"`
	RegexPattern             string `json:"regexPattern,omitempty"`
	RegexReplacement         string `json:"regexReplacement,omitempty"`
}

func (t *TransformConfig) UnmarshalJSON(raw []byte) error {
	var in transformConfigJSON
	if err := json.Unmarshal(raw, &in); err != nil {
		return err
	}
	t.RemoveSystemName = true
	if in.RemoveSystemName != nil {
		t.RemoveSystemName = *in.RemoveSystemName
	}
	t.ConvertDotToSlash = true
	if in.ConvertDotToSlash != nil {
		t.ConvertDotToSlash = *in.ConvertDotToSlash
	}
	if in.ConvertUnderscoreToSlash != nil {
		t.ConvertUnderscoreToSlash = *in.ConvertUnderscoreToSlash
	}
	t.RegexPattern = in.RegexPattern
	t.RegexReplacement = in.RegexReplacement
	return nil
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

func (t *TransformConfig) TransformDatapointName(name string) string {
	out := strings.TrimRight(name, ".")
	if t.RemoveSystemName {
		if idx := strings.Index(out, ":"); idx >= 0 {
			out = out[idx+1:]
		}
	}
	if t.ConvertDotToSlash {
		out = strings.ReplaceAll(out, ".", "/")
	}
	if t.ConvertUnderscoreToSlash {
		out = strings.ReplaceAll(out, "_", "/")
	}
	if t.RegexPattern != "" {
		if t.compiledRegex == nil {
			if re, err := regexp.Compile(t.RegexPattern); err == nil {
				t.compiledRegex = re
			}
		}
		if t.compiledRegex != nil {
			out = t.compiledRegex.ReplaceAllString(out, t.RegexReplacement)
		}
	}
	return strings.Trim(out, "/")
}

type ConnectionConfig struct {
	GraphqlEndpoint   string          `json:"graphqlEndpoint,omitempty"`
	WebsocketEndpoint string          `json:"websocketEndpoint,omitempty"`
	Username          string          `json:"username,omitempty"`
	Password          string          `json:"password,omitempty"`
	Token             string          `json:"token,omitempty"`
	ReconnectDelay    int64           `json:"reconnectDelay,omitempty"`
	ConnectionTimeout int64           `json:"connectionTimeout,omitempty"`
	MessageFormat     string          `json:"messageFormat,omitempty"`
	TransformConfig   TransformConfig `json:"transformConfig"`
	Addresses         []Address       `json:"addresses,omitempty"`
}

func (c *ConnectionConfig) applyDefaults(defaultTransform bool) {
	if c.GraphqlEndpoint == "" {
		c.GraphqlEndpoint = "http://winccoa:4000/graphql"
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
	if defaultTransform && !c.TransformConfig.RemoveSystemName && !c.TransformConfig.ConvertDotToSlash &&
		!c.TransformConfig.ConvertUnderscoreToSlash && c.TransformConfig.RegexPattern == "" && c.TransformConfig.RegexReplacement == "" {
		c.TransformConfig.RemoveSystemName = true
		c.TransformConfig.ConvertDotToSlash = true
	}
}

func ParseConnectionConfig(raw string) (*ConnectionConfig, error) {
	cfg := &ConnectionConfig{}
	if strings.TrimSpace(raw) == "" {
		cfg.applyDefaults(true)
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(raw), cfg); err != nil {
		return nil, fmt.Errorf("winccoa config: %w", err)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal([]byte(raw), &fields)
	_, hasTransform := fields["transformConfig"]
	cfg.applyDefaults(!hasTransform)
	return cfg, nil
}

func (c *ConnectionConfig) Validate() []string {
	var errs []string
	if strings.TrimSpace(c.GraphqlEndpoint) == "" {
		errs = append(errs, "graphqlEndpoint cannot be blank")
	} else if !strings.HasPrefix(c.GraphqlEndpoint, "http://") && !strings.HasPrefix(c.GraphqlEndpoint, "https://") {
		errs = append(errs, "graphqlEndpoint must start with http:// or https://")
	}
	if (c.Username == "") != (c.Password == "") {
		errs = append(errs, "username and password must both be set or both be blank")
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
