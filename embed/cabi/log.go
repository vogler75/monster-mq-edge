//go:build cgo && winccoa_embed

package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
)

func levelName(l int32) string {
	switch l {
	case 0:
		return "DEBUG"
	case 2:
		return "WARN"
	case 3:
		return "ERROR"
	}
	return "INFO"
}

// hostHandler formats records as text and hands each line to the host log
// callback, which queues it for the WinCC OA log on the manager thread.
type hostHandler struct {
	host  *cHost
	level slog.Level
	mu    *sync.Mutex
	buf   *bytes.Buffer
	inner slog.Handler
}

func newHostLogger(host *cHost, level string) *slog.Logger {
	var lv slog.Level
	_ = lv.UnmarshalText([]byte(strings.ToUpper(level)))
	buf := &bytes.Buffer{}
	h := &hostHandler{host: host, level: lv, mu: &sync.Mutex{}, buf: buf}
	h.inner = slog.NewTextHandler(buf, &slog.HandlerOptions{
		Level: lv,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && (a.Key == slog.TimeKey || a.Key == slog.LevelKey) {
				return slog.Attr{}
			}
			return a
		},
	})
	return slog.New(h)
}

func (h *hostHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *hostHandler) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	h.buf.Reset()
	err := h.inner.Handle(ctx, r)
	line := strings.TrimRight(h.buf.String(), "\n")
	h.mu.Unlock()
	if err != nil {
		return err
	}
	lvl := int32(1)
	switch {
	case r.Level >= slog.LevelError:
		lvl = 3
	case r.Level >= slog.LevelWarn:
		lvl = 2
	case r.Level < slog.LevelInfo:
		lvl = 0
	}
	h.host.log(lvl, line)
	return nil
}

func (h *hostHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.inner = h.inner.WithAttrs(attrs)
	return &c
}

func (h *hostHandler) WithGroup(name string) slog.Handler {
	c := *h
	c.inner = h.inner.WithGroup(name)
	return &c
}
