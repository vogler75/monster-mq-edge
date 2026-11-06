// Package kafka implements the Kafka wire-protocol server (device type
// Kafka-Server), a port of the main broker's KafkaServer feature.
package kafka

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// StreamMapping maps a Kafka topic (stream) to an MQTT topic filter.
type StreamMapping struct {
	StreamName     string  `json:"streamName"`
	TopicFilter    string  `json:"topicFilter"`
	RetentionHours int     `json:"retentionHours"`
	StoreType      *string `json:"storeType"`
	AllowWrite     bool    `json:"allowWrite"`
}

// ServerConfig is the JSON config of one Kafka-Server device.
type ServerConfig struct {
	Name           string          `json:"name"`
	Namespace      string          `json:"namespace"`
	NodeID         string          `json:"nodeId"`
	Enabled        bool            `json:"enabled"`
	Host           string          `json:"host"`
	Port           int             `json:"port"`
	AdvertisedHost *string         `json:"advertisedHost,omitempty"`
	AdvertisedPort *int            `json:"advertisedPort,omitempty"`
	StoreType      *string         `json:"storeType"`
	Streams        []StreamMapping `json:"streams"`
}

var nameRE = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ParseConfig decodes the device config JSON applying the main broker's
// defaults (host 0.0.0.0, port 9092, retentionHours 168, allowWrite true).
func ParseConfig(raw []byte) (ServerConfig, error) {
	cfg := ServerConfig{Enabled: true, Host: "0.0.0.0", Port: 9092, NodeID: "*"}
	var generic struct {
		Streams []json.RawMessage `json:"streams"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("kafka server config: %w", err)
		}
		if err := json.Unmarshal(raw, &generic); err != nil {
			return cfg, fmt.Errorf("kafka server config: %w", err)
		}
	}
	// Decode streams one by one so absent fields get their defaults.
	cfg.Streams = cfg.Streams[:0]
	for _, s := range generic.Streams {
		m := StreamMapping{RetentionHours: 168, AllowWrite: true}
		if err := json.Unmarshal(s, &m); err != nil {
			return cfg, fmt.Errorf("kafka server stream: %w", err)
		}
		cfg.Streams = append(cfg.Streams, m)
	}
	return cfg, nil
}

// Validate checks name, port and stream mappings.
func (c ServerConfig) Validate() []string {
	var errs []string
	if !nameRE.MatchString(c.Name) {
		errs = append(errs, "name must match ^[a-zA-Z0-9_-]+$")
	}
	if c.Port < 1 || c.Port > 65535 {
		errs = append(errs, "port must be 1-65535")
	}
	seen := map[string]bool{}
	for _, s := range c.Streams {
		if s.StreamName == "" || s.TopicFilter == "" {
			errs = append(errs, "stream needs streamName and topicFilter")
			continue
		}
		if seen[s.StreamName] {
			errs = append(errs, "duplicate streamName "+s.StreamName)
		}
		seen[s.StreamName] = true
		if s.RetentionHours < 1 {
			errs = append(errs, "retentionHours must be >= 1 for "+s.StreamName)
		}
	}
	return errs
}
