package winccua

import (
	"context"
	"log/slog"
	"sync/atomic"
)

// Connector is the per-device bridge runner. The Manager creates one per
// enabled device and Stops it when the device is removed/disabled.
type Connector interface {
	Start(ctx context.Context) error
	Stop()
	IsConnected() bool
	MessagesIn() float64 // messages/sec since last sample (resets the counter)
}

// metrics is shared by both transports — counts of messages received from RT
// since the last sample, plus a timestamp and connected flag.
type metrics struct {
	count    atomic.Int64
	lastSamp atomic.Int64 // unix nanos
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

// New constructs the right Connector for the configured DataAccessMode.
func New(deviceName string, cfg *ConnectionConfig, namespace string, publish LocalPublisher, logger *slog.Logger) Connector {
	pub := newPublisher(namespace, cfg.TransformConfig, cfg.MessageFormat)
	if cfg.DataAccessMode == ModeOpenPipe {
		return newPipeConnector(deviceName, cfg, pub, publish, logger)
	}
	return newGraphQLConnector(deviceName, cfg, pub, publish, logger)
}

// nowNanos returns the wall-clock time in nanoseconds. Extracted so tests can
// stub it without going through time.Now().UnixNano() everywhere.
var nowNanos = defaultNowNanos
