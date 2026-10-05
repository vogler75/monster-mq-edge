//go:build cgo && winccoa_embed

package main

import (
	"log/slog"
	"os"
	"runtime/pprof"
)

// startDiagnostics records a CPU profile until the broker stops when
// MMQ_CPUPROFILE names a file. Counters are published as $SYS/winccoa/...
func (in *instance) startDiagnostics(logger *slog.Logger) {
	path := os.Getenv("MMQ_CPUPROFILE")
	if path == "" {
		return
	}
	f, err := os.Create(path)
	if err != nil {
		return
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		_ = f.Close()
		return
	}
	logger.Info("cpu profile started", "path", path)
	in.diagStop = func() {
		pprof.StopCPUProfile()
		_ = f.Close()
	}
}

func (in *instance) stopDiagnostics() {
	if in.diagStop != nil {
		in.diagStop()
		in.diagStop = nil
	}
}
