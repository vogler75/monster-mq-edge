package log

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestHandlerKeepsAttributesForEmptyMessage(t *testing.T) {
	bus := NewBus(10)
	var stderr bytes.Buffer
	logger := slog.New(NewHandler(bus, slog.NewTextHandler(&stderr, nil), "node-a"))

	logger.Warn("", "listener", "tcps", "error", errors.New("bad username or password"))

	entries := bus.Snapshot()
	if len(entries) != 1 {
		t.Fatalf("expected one log entry, got %d", len(entries))
	}
	entry := entries[0]
	if !strings.Contains(entry.Message, "listener=tcps") || !strings.Contains(entry.Message, "error=bad username or password") {
		t.Fatalf("message does not include attributes: %q", entry.Message)
	}
	if len(entry.Parameters) != 2 {
		t.Fatalf("expected two parameters, got %#v", entry.Parameters)
	}
	if entry.Exception == nil || entry.Exception.Message != "bad username or password" {
		t.Fatalf("expected error exception, got %#v", entry.Exception)
	}
}

func TestHandlerKeepsWithAttrsForBusEntries(t *testing.T) {
	bus := NewBus(10)
	var stderr bytes.Buffer
	handler := NewHandler(bus, slog.NewTextHandler(&stderr, nil), "node-a")
	logger := slog.New(handler.WithAttrs([]slog.Attr{slog.String("listener", "tcps")}))

	logger.LogAttrs(context.Background(), slog.LevelWarn, "auth failed", slog.Any("error", errors.New("bad username or password")))

	entries := bus.Snapshot()
	if len(entries) != 1 {
		t.Fatalf("expected one log entry, got %d", len(entries))
	}
	entry := entries[0]
	if entry.Message != "auth failed" {
		t.Fatalf("unexpected message: %q", entry.Message)
	}
	if len(entry.Parameters) != 2 {
		t.Fatalf("expected with-attrs and record attrs, got %#v", entry.Parameters)
	}
	if !contains(entry.Parameters, "listener=tcps") || !contains(entry.Parameters, "error=bad username or password") {
		t.Fatalf("missing parameters: %#v", entry.Parameters)
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
