package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"go.yaml.in/yaml/v3"
)

func Load(path string) (*Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := decodePeerLinkStrict(data); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// decodePeerLinkStrict decodes the PeerLink section again and rejects unknown
// keys, also while it is disabled: a misspelled security key must not leave
// the link silently unauthenticated. The rest of the file stays lenient.
func decodePeerLinkStrict(data []byte) error {
	var doc struct {
		PeerLink PeerLinkConfig `yaml:"PeerLink"`
		Rest     map[string]any `yaml:",inline"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("PeerLink: %w", err)
	}
	return nil
}
