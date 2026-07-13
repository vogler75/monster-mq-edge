//go:build !cgo

package zenoh

import (
	"context"
	"log/slog"

	"monstermq.io/edge/internal/config"
	mqtt "monstermq.io/edge/internal/mqtt"
	"monstermq.io/edge/internal/stores"
)

type ArchiveFinder interface {
}

type Engine struct {
	logger *slog.Logger
}

func NewEngine(cfg *config.Config, nodeID string, mochiServer *mqtt.Server, storage *stores.Storage, archives ArchiveFinder, logger *slog.Logger) *Engine {
	return &Engine{
		logger: logger.With("system", "zenoh-federation-stub"),
	}
}

func (e *Engine) Start(ctx context.Context) error {
	e.logger.Warn("Zenoh federation is disabled in this build because it was compiled without CGo (CGO_ENABLED=0)")
	return nil
}

func (e *Engine) Stop() error {
	return nil
}

func (e *Engine) PublishMessageToBus(msg stores.BrokerMessage) {
}
