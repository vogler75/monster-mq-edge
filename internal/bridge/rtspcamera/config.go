package rtspcamera

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

const (
	DeviceTypeRtspCamera = "RTSP_CAMERA"

	ModeContinuous = "CONTINUOUS"
	ModeTriggered  = "TRIGGERED"
	ModeBoth       = "BOTH"

	TransportTCP = "TCP"
	TransportUDP = "UDP"
)

// Config is the persisted JSON configuration for one RTSP Camera device.
type Config struct {
	URL             string `json:"url"`
	Transport       string `json:"transport"`
	TopicPrefix     string `json:"topicPrefix"`
	Mode            string `json:"mode"`
	IntervalMs      int    `json:"intervalMs"`
	Slots           int    `json:"slots"`
	TriggerTopic    string `json:"triggerTopic,omitempty"`
	Retain          bool   `json:"retain"`
	QoS             int    `json:"qos"`
	PublishMetadata bool   `json:"publishMetadata"`
}

// SnapshotMeta is the payload published to <topicPrefix>/capture/<slot>/meta.
type SnapshotMeta struct {
	Camera      string `json:"camera"`
	Slot        int    `json:"slot"`
	Timestamp   string `json:"timestamp"`
	TimestampMs int64  `json:"timestampMs"`
	Bytes       int    `json:"bytes"`
	ContentType string `json:"contentType"`
	Topic       string `json:"topic"`
	Trigger     string `json:"trigger"`
}

// LatestPointer is the companion state published to <topicPrefix>/capture/latest.
type LatestPointer struct {
	Camera      string `json:"camera"`
	Slot        int    `json:"slot"`
	PicTopic    string `json:"picTopic"`
	MetaTopic   string `json:"metaTopic"`
	Timestamp   string `json:"timestamp"`
	TimestampMs int64  `json:"timestampMs"`
	Bytes       int    `json:"bytes"`
	Trigger     string `json:"trigger"`
}

// Metrics captures runtime stats for an active camera connector.
type Metrics struct {
	Connected          bool    `json:"connected"`
	FramesReceived     float64 `json:"framesReceived"`
	SnapshotsPublished float64 `json:"snapshotsPublished"`
	CurrentSlot        int     `json:"currentSlot"`
	LastSnapshotAt     string  `json:"lastSnapshotAt"`
	LastError          string  `json:"lastError"`
	Timestamp          string  `json:"timestamp"`
}

// ParseConfig parses a raw JSON configuration string into Config.
func ParseConfig(raw string) (Config, error) {
	cfg := DefaultConfig()
	if strings.TrimSpace(raw) == "" {
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return cfg, err
	}
	cfg.ApplyDefaults()
	return cfg, nil
}

// DefaultConfig returns default camera bridge configuration values.
func DefaultConfig() Config {
	return Config{
		Transport:       TransportTCP,
		TopicPrefix:     "cameras/camera",
		Mode:            ModeContinuous,
		IntervalMs:      1000,
		Slots:           5,
		Retain:          true,
		QoS:             0,
		PublishMetadata: true,
	}
}

// ApplyDefaults fills in zero values with reasonable defaults.
func (c *Config) ApplyDefaults() {
	if c.Transport == "" {
		c.Transport = TransportTCP
	}
	if c.Mode == "" {
		c.Mode = ModeContinuous
	}
	if c.IntervalMs <= 0 {
		c.IntervalMs = 1000
	}
	if c.Slots <= 0 {
		c.Slots = 5
	}
	if c.TopicPrefix == "" {
		c.TopicPrefix = "cameras/camera"
	}
	if (c.Mode == ModeTriggered || c.Mode == ModeBoth) && c.TriggerTopic == "" {
		c.TriggerTopic = fmt.Sprintf("%s/trigger", strings.TrimSuffix(c.TopicPrefix, "/"))
	}
}

// Validate checks for configuration errors.
func (c *Config) Validate() []string {
	var errs []string
	if strings.TrimSpace(c.URL) == "" {
		errs = append(errs, "url is required")
	} else if parsed, err := url.Parse(c.URL); err != nil || parsed.Host == "" || (parsed.Scheme != "rtsp" && parsed.Scheme != "rtsps" && parsed.Scheme != "http" && parsed.Scheme != "https") {
		errs = append(errs, "url must be an rtsp://, rtsps://, http://, or https:// URL with a host")
	}

	mode := strings.ToUpper(c.Mode)
	if mode != ModeContinuous && mode != ModeTriggered && mode != ModeBoth {
		errs = append(errs, fmt.Sprintf("invalid mode %q, must be CONTINUOUS, TRIGGERED, or BOTH", c.Mode))
	}

	transport := strings.ToUpper(c.Transport)
	if transport != TransportTCP && transport != TransportUDP {
		errs = append(errs, fmt.Sprintf("invalid transport %q, must be TCP or UDP", c.Transport))
	}

	if c.Slots < 1 {
		errs = append(errs, "slots must be at least 1")
	}
	if c.IntervalMs < 50 {
		errs = append(errs, "intervalMs must be at least 50")
	}
	if strings.TrimSpace(c.TopicPrefix) == "" {
		errs = append(errs, "topicPrefix is required")
	}
	if (mode == ModeTriggered || mode == ModeBoth) && strings.TrimSpace(c.TriggerTopic) == "" {
		errs = append(errs, "triggerTopic is required when mode is TRIGGERED or BOTH")
	}
	if c.QoS < 0 || c.QoS > 2 {
		errs = append(errs, "qos must be 0, 1, or 2")
	}
	return errs
}
