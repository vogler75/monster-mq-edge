package integration

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"monstermq.io/edge/internal/broker"
	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/stores"
)

func portFree(t *testing.T, port int) bool {
	t.Helper()
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// AC-08: an unreachable OA during store load or native start prevents
// readiness and releases the listener and storage it had acquired.
func TestNativeStartupFailures(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	sim.SetSystemAvailable("System1", false)

	cfg := config.Default()
	cfg.NodeID = "sf"
	cfg.TCP.Port = 27160
	cfg.GraphQL.Enabled = false
	cfg.Metrics.Enabled = false
	cfg.SQLite.Path = filepath.Join(t.TempDir(), "n.db")
	cfg.WinCCOaNative = config.WinCCOaNativeConfig{Enabled: true, Namespace: true, Stores: []string{config.WinCCOaStoreDevice}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := broker.NewWithOptions(cfg, slog.New(slog.DiscardHandler), nil, broker.Options{OA: client}); err == nil {
		t.Fatal("store load against an unreachable OA must fail")
	}
	if !portFree(t, 27160) {
		t.Fatal("listener still bound after a failed start")
	}

	// Native namespace start (local system lookup) fails: Serve reports it.
	sim2, client2 := newSim(0)
	defer sim2.Close()
	sim2.Pause()
	cfg.WinCCOaNative.Stores = nil
	srv, err := broker.NewWithOptions(cfg, slog.New(slog.DiscardHandler), nil, broker.Options{OA: client2})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := srv.Serve(); err == nil || !strings.Contains(err.Error(), "winccoa native start") {
		t.Fatalf("Serve with a blocked OA: %v", err)
	}
	if time.Since(start) > 8*time.Second {
		t.Fatalf("start failure took %s", time.Since(start))
	}
	_ = srv.Close()
	if !portFree(t, 27160) {
		t.Fatal("listener still bound after Close")
	}
}

// AC-09: repeated subscription and reload cycles leave no registrations,
// callbacks, routes or goroutines behind.
func TestNativeNoGrowth(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	env := startNative(t, 27161, filepath.Join(t.TempDir(), "n.db"), sim, client, func(c *config.Config) {
		c.Features.WinCCOa = true
		c.WinCCOaNative.Transport = config.WinCCOaTransportNative
	}, broker.Options{})
	defer env.srv.Close()
	ctx := context.Background()

	cycle := func(i int) {
		c, _ := dialRaw(t, env.port, rawConnect{ClientID: fmt.Sprintf("g%d", i%5), Version: 5, Clean: true})
		c.Subscribe(sub("winccoa/this/tags/Pump1/speed", 1), sub("winccoa/this/types/AnalogDrive/Pump101/count", 0))
		c.Unsubscribe("winccoa/this/tags/Pump1/speed")
		c.Close() // clean session: remaining interest released on disconnect
	}
	reload := func(i int) {
		cfg := fmt.Sprintf(`{"addresses":[{"query":"SELECT '_online.._value' FROM 'Pump*.speed'","topic":"q%d","answer":true}]}`, i)
		if err := env.srv.Storage().DeviceConfig.Save(ctx, stores.DeviceConfig{Name: "oa", Namespace: "ns", NodeID: "n-27161", Type: "WinCCOA-Client", Enabled: true, Config: cfg}); err != nil {
			t.Fatal(err)
		}
		if err := env.srv.WinCCOa().Reload(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 20; i++ {
		cycle(i)
		reload(i)
	}
	time.Sleep(500 * time.Millisecond)
	runtime.GC()
	baseGoroutines := runtime.NumGoroutine()
	baseRefs := client.Refs()

	for i := 20; i < 220; i++ {
		cycle(i)
		if i%10 == 0 {
			reload(i)
		}
	}
	time.Sleep(800 * time.Millisecond)
	runtime.GC()

	if n := sim.Connections(); n != 0 {
		t.Errorf("dpConnect registrations left: %d", n)
	}
	if n := sim.Queries(); n != 1 {
		t.Errorf("query registrations: %d, want the one live device query", n)
	}
	if st := env.srv.Native().Stats(); st.Interests != 0 || st.DPEs != 0 || st.Batches != 0 {
		t.Errorf("native state left: %+v", st)
	}
	if r := client.Refs(); r > baseRefs {
		t.Errorf("event routes grew from %d to %d", baseRefs, r)
	}
	if p := client.Pending(); p != 0 {
		t.Errorf("pending requests: %d", p)
	}
	if g := runtime.NumGoroutine(); g > baseGoroutines+10 {
		t.Errorf("goroutines grew from %d to %d", baseGoroutines, g)
	}
	if o := sim.Orphans(); o != 0 {
		t.Logf("orphaned registrations released: %d", o)
	}
}
