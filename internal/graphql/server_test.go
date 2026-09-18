package graphql

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"monstermq.io/edge/internal/config"
)

func getFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("getFreePort: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func TestServer_StartStopConcurrentRace(t *testing.T) {
	// Exercise rapid concurrent Start and Stop across multiple iterations
	for i := 0; i < 20; i++ {
		port := getFreePort(t)
		cfg := config.Default()
		cfg.GraphQL.Enabled = true
		cfg.GraphQL.Port = port
		cfg.UserManagement.Enabled = false

		srv := NewServer(cfg, nil, nil, nil, nil, nil, nil, slog.New(slog.DiscardHandler))

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			_ = srv.Start()
		}()

		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Stop(ctx)
		}()

		wg.Wait()
	}
}

func TestServer_StopBeforeStart(t *testing.T) {
	port := getFreePort(t)
	cfg := config.Default()
	cfg.GraphQL.Enabled = true
	cfg.GraphQL.Port = port
	cfg.UserManagement.Enabled = false

	srv := NewServer(cfg, nil, nil, nil, nil, nil, nil, slog.New(slog.DiscardHandler))

	// Stop is called before Start
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("Stop error: %v", err)
	}

	// Start should now be a no-op returning nil
	if err := srv.Start(); err != nil {
		t.Fatalf("Start after Stop should return nil, got: %v", err)
	}

	// Verify port was never bound or is closed
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
	if err == nil {
		resp.Body.Close()
		t.Fatalf("server should not be listening after stop-before-start")
	}
}

func TestServer_StopIdempotent(t *testing.T) {
	port := getFreePort(t)
	cfg := config.Default()
	cfg.GraphQL.Enabled = true
	cfg.GraphQL.Port = port
	cfg.UserManagement.Enabled = false

	srv := NewServer(cfg, nil, nil, nil, nil, nil, nil, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("first Stop error: %v", err)
	}
	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("second Stop error: %v", err)
	}
}

func TestServer_StartTwiceFails(t *testing.T) {
	port := getFreePort(t)
	cfg := config.Default()
	cfg.GraphQL.Enabled = true
	cfg.GraphQL.Port = port
	cfg.UserManagement.Enabled = false

	srv := NewServer(cfg, nil, nil, nil, nil, nil, nil, slog.New(slog.DiscardHandler))

	go func() {
		_ = srv.Start()
	}()

	// Give Start a brief moment to transition state
	time.Sleep(50 * time.Millisecond)

	err := srv.Start()
	if err == nil {
		t.Fatalf("expected error on duplicate Start, got nil")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = srv.Stop(ctx)
}
