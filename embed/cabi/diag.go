//go:build cgo && winccoa_embed

package main

import (
	"log/slog"
	"os"
	"runtime/pprof"
	"strconv"
	"time"
)

// startDiagnostics logs the host client counters periodically
// (MMQ_STATS_SECONDS, default 60) and, when MMQ_CPUPROFILE names a file,
// records a CPU profile until the broker stops.
func (in *instance) startDiagnostics(logger *slog.Logger) {
	done := make(chan struct{})
	var profile *os.File
	if path := os.Getenv("MMQ_CPUPROFILE"); path != "" {
		if f, err := os.Create(path); err == nil && pprof.StartCPUProfile(f) == nil {
			profile = f
			logger.Info("cpu profile started", "path", path)
		}
	}
	every := 60 * time.Second
	if s, err := strconv.Atoi(os.Getenv("MMQ_STATS_SECONDS")); err == nil && s > 0 {
		every = time.Duration(s) * time.Second
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				st := in.client.Stats()
				logger.Info("host client stats", "submitted", st.Submitted, "completed", st.Completed,
					"timedOut", st.TimedOut, "overloaded", st.Overloaded, "late", st.LateCompletions,
					"eventsDelivered", st.EventsDelivered, "eventsDropped", st.EventsDropped,
					"eventsUnrouted", st.EventsUnrouted, "pendingHighWater", st.PendingHighWater)
			}
		}
	}()
	in.diagStop = func() {
		close(done)
		if profile != nil {
			pprof.StopCPUProfile()
			_ = profile.Close()
		}
	}
}

func (in *instance) stopDiagnostics() {
	if in.diagStop != nil {
		in.diagStop()
		in.diagStop = nil
	}
}
