package scripting

import (
	"encoding/json"
	"strings"
)

type ScriptTriggerType string

const (
	TriggerTypeTopic    ScriptTriggerType = "TOPIC"
	TriggerTypeTimer    ScriptTriggerType = "TIMER"
	TriggerTypeBoth     ScriptTriggerType = "BOTH"
	TriggerTypeCallable ScriptTriggerType = "CALLABLE"
)

type ScriptInstanceMode string

const (
	InstanceModeSingleton     ScriptInstanceMode = "SINGLETON"
	InstanceModeMultiInstance ScriptInstanceMode = "MULTI_INSTANCE"
)

const (
	DefaultLanguage    = "starlark"
	DefaultTimeoutMs   = 200
	DefaultTriggerType = TriggerTypeTopic
	DefaultInstanceMode = InstanceModeSingleton
)

// ScriptConfig is the configuration structure decoded from DeviceConfig.Config JSON.
type ScriptConfig struct {
	Language            string             `json:"language"`
	Script              string             `json:"script"`
	TriggerType         ScriptTriggerType  `json:"triggerType"`
	TopicFilters        []string           `json:"topicFilters"`
	TriggerOnChangeOnly bool               `json:"triggerOnChangeOnly"`
	TimerIntervalMs     int64              `json:"timerIntervalMs"`
	InstanceMode        ScriptInstanceMode `json:"instanceMode"`
	TimeoutMs           int64              `json:"timeoutMs"`
	Description         string             `json:"description"`
}

// ParseConfig unmarshals and normalizes a ScriptConfig JSON string.
func ParseConfig(raw string) (*ScriptConfig, error) {
	cfg := &ScriptConfig{
		Language:     DefaultLanguage,
		TriggerType:  DefaultTriggerType,
		TopicFilters: []string{},
		InstanceMode: DefaultInstanceMode,
		TimeoutMs:    DefaultTimeoutMs,
	}

	if raw == "" || raw == "{}" {
		return cfg, nil
	}

	// Support parsing both direct ScriptConfig and potential singular topicFilter
	var rawMap map[string]any
	if err := json.Unmarshal([]byte(raw), &rawMap); err != nil {
		return nil, err
	}

	// Normalise singular "topicFilter" if present and topicFilters is missing
	if tf, ok := rawMap["topicFilter"].(string); ok && tf != "" {
		if _, hasFilters := rawMap["topicFilters"]; !hasFilters {
			rawMap["topicFilters"] = []string{tf}
		}
	}

	// Re-marshal to struct
	normBytes, err := json.Marshal(rawMap)
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(normBytes, cfg); err != nil {
		return nil, err
	}

	if cfg.Language == "" {
		cfg.Language = DefaultLanguage
	} else {
		cfg.Language = strings.ToLower(cfg.Language)
	}

	if cfg.TriggerType == "" {
		cfg.TriggerType = DefaultTriggerType
	}

	if cfg.InstanceMode == "" {
		cfg.InstanceMode = DefaultInstanceMode
	}

	if cfg.TimeoutMs <= 0 {
		cfg.TimeoutMs = DefaultTimeoutMs
	}

	if cfg.TopicFilters == nil {
		cfg.TopicFilters = []string{}
	}

	return cfg, nil
}
