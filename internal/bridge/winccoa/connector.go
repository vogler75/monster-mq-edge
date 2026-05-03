package winccoa

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// LocalPublisher is the function the bridge calls to inject an MQTT message
// into the local broker. mochi-mqtt's *Server.Publish satisfies this shape.
type LocalPublisher func(topic string, payload []byte, retain bool, qos byte) error

// Connector is the per-device bridge runner. The Manager creates one per
// enabled device and Stops it when the device is removed/disabled.
type Connector interface {
	Start(ctx context.Context) error
	Stop()
	IsConnected() bool
	MessagesIn() float64
	SampleMetrics(now time.Time) MetricsSnapshot
}

type MetricsSnapshot struct {
	MessagesIn float64   `json:"messagesIn"`
	Connected  bool      `json:"connected"`
	Timestamp  time.Time `json:"timestamp"`
}

type metrics struct {
	count    atomic.Int64
	lastSamp atomic.Int64
	connFlag atomic.Bool
}

func (m *metrics) inc() { m.count.Add(1) }

func (m *metrics) sampleRate() float64 {
	now := nowNanos()
	prev := m.lastSamp.Swap(now)
	if prev == 0 {
		m.count.Store(0)
		return 0
	}
	count := m.count.Swap(0)
	elapsedNs := now - prev
	if elapsedNs <= 0 {
		return 0
	}
	return float64(count) * 1e9 / float64(elapsedNs)
}

func New(deviceName string, cfg *ConnectionConfig, namespace string, publish LocalPublisher, logger *slog.Logger) Connector {
	pub := newPublisher(namespace, cfg.TransformConfig, cfg.MessageFormat)
	return newGraphQLConnector(deviceName, cfg, pub, publish, logger)
}

var nowNanos = func() int64 { return time.Now().UnixNano() }
