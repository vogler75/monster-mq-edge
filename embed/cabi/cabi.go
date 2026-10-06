//go:build cgo && winccoa_embed

// Command cabi builds the embeddable broker library (c-archive) for the
// WinCC OA API manager. It is the only package allowed to use cgo; the
// standalone broker never imports it.
package main

/*
#include <stdlib.h>
#include <string.h>
#include "monstermq_types.h"

static int32_t mmq_call_submit(const mmq_host *h, uint64_t id, int64_t deadline, const uint8_t *data, uint32_t len) {
	return h->submit(h->user, id, deadline, data, len);
}
static void mmq_call_wake(const mmq_host *h) {
	if (h->wake) h->wake(h->user);
}
static void mmq_call_log(const mmq_host *h, int32_t level, const char *msg, uint32_t len) {
	if (h->log) h->log(h->user, level, msg, len);
}
*/
import "C"

import (
	"log/slog"
	"fmt"
	"runtime/debug"
	"sync"
	"time"
	"unsafe"

	"monstermq.io/edge/internal/broker"
	"monstermq.io/edge/internal/config"
	mlog "monstermq.io/edge/internal/log"
	"monstermq.io/edge/internal/oahost"
)

func main() {}

const (
	stateCreated  = 1
	stateStarting = 2
	stateRunning  = 3
	stateStopping = 4
	stateStopped  = 5
	stateFailed   = 6
)

// cHost adapts the C host callbacks to oahost.Host. The host struct is a
// C-allocated copy owned by the instance.
type cHost struct {
	h *C.mmq_host
}

func (c *cHost) Submit(id uint64, deadline time.Time, msg []byte) error {
	var p *C.uint8_t
	if len(msg) > 0 {
		p = (*C.uint8_t)(unsafe.Pointer(&msg[0]))
	}
	rc := C.mmq_call_submit(c.h, C.uint64_t(id), C.int64_t(deadline.UnixMilli()), p, C.uint32_t(len(msg)))
	switch int32(rc) {
	case oahost.StatusOK:
		C.mmq_call_wake(c.h)
		return nil
	case oahost.StatusOverload:
		return oahost.ErrOverload
	}
	return fmt.Errorf("%w: host submit returned %d", oahost.ErrState, int32(rc))
}

func (c *cHost) log(level int32, msg string) {
	if len(msg) == 0 {
		return
	}
	b := []byte(msg)
	C.mmq_call_log(c.h, C.int32_t(level), (*C.char)(unsafe.Pointer(&b[0])), C.uint32_t(len(b)))
}

type instance struct {
	handle uint64
	host   *cHost
	chost  *C.mmq_host
	cfg    *config.Config
	client *oahost.Client

	mu      sync.Mutex
	state   int
	lastErr string
	srv     *broker.Server
	started chan struct{} // closed when the start attempt finished
	diagStop func()
	stopped chan struct{}
}

var (
	instMu     sync.Mutex
	inst       *instance
	nextHandle uint64
)

func current(h C.uint64_t) (*instance, int32) {
	instMu.Lock()
	defer instMu.Unlock()
	if inst == nil || inst.handle != uint64(h) {
		return nil, oahost.StatusState
	}
	return inst, oahost.StatusOK
}

// guard converts a panic into MMQ_E_STATE so nothing escapes into C.
func guard(rc *C.int32_t) {
	if r := recover(); r != nil {
		*rc = C.int32_t(oahost.StatusState)
		instMu.Lock()
		if inst != nil {
			inst.mu.Lock()
			inst.lastErr = fmt.Sprintf("panic: %v\n%s", r, debug.Stack())
			inst.mu.Unlock()
		}
		instMu.Unlock()
	}
}

//export mmq_abi_version
func mmq_abi_version() C.int32_t { return C.MMQ_ABI_VERSION }

//export mmq_create
func mmq_create(cfg *C.mmq_config, host *C.mmq_host, out *C.uint64_t) (rc C.int32_t) {
	defer guard(&rc)
	if cfg == nil || host == nil || out == nil {
		return C.int32_t(oahost.StatusInvalid)
	}
	if uint32(cfg.struct_size) < uint32(C.sizeof_mmq_config) || uint32(host.struct_size) < uint32(C.sizeof_mmq_host) {
		return C.int32_t(oahost.StatusABI)
	}
	if uint32(cfg.abi_version) != C.MMQ_ABI_VERSION || uint32(host.abi_version) != C.MMQ_ABI_VERSION {
		return C.int32_t(oahost.StatusABI)
	}
	if host.submit == nil {
		return C.int32_t(oahost.StatusInvalid)
	}
	if cfg.config_path_len > 4096 || (cfg.config_path == nil && cfg.config_path_len > 0) {
		return C.int32_t(oahost.StatusInvalid)
	}
	path := ""
	if cfg.config_path != nil && cfg.config_path_len > 0 {
		path = C.GoStringN(cfg.config_path, C.int(cfg.config_path_len))
	}

	instMu.Lock()
	defer instMu.Unlock()
	if inst != nil {
		return C.int32_t(oahost.StatusState)
	}
	chost := (*C.mmq_host)(C.malloc(C.sizeof_mmq_host))
	C.memcpy(unsafe.Pointer(chost), unsafe.Pointer(host), C.sizeof_mmq_host)
	h := &cHost{h: chost}

	bc, err := config.Load(path)
	if err != nil {
		h.log(3, fmt.Sprintf("monstermq config error: %v", err))
		C.free(unsafe.Pointer(chost))
		return C.int32_t(oahost.StatusInvalid)
	}
	if !bc.WinCCOaNative.Enabled {
		bc.WinCCOaNative.Enabled = true
	}
	limits := oahost.Limits{
		MaxPending:     int(cfg.max_pending),
		EventQueue:     int(cfg.event_queue),
		DefaultTimeout: time.Duration(cfg.default_timeout_ms) * time.Millisecond,
	}
	nextHandle++
	inst = &instance{
		handle:  nextHandle,
		host:    h,
		chost:   chost,
		cfg:     bc,
		client:  oahost.NewClient(h, limits),
		state:   stateCreated,
		started: make(chan struct{}),
		stopped: make(chan struct{}),
	}
	// The host's DEBUG (WCCOAmmq -dbg USR1) wins; otherwise Logging.Level
	// from the broker YAML applies.
	if int32(cfg.log_level) == 0 || inst.cfg.Logging.Level == "" {
		inst.cfg.Logging.Level = levelName(int32(cfg.log_level))
	}
	*out = C.uint64_t(inst.handle)
	return C.int32_t(oahost.StatusOK)
}

//export mmq_start
func mmq_start(h C.uint64_t) (rc C.int32_t) {
	defer guard(&rc)
	in, st := current(h)
	if in == nil {
		return C.int32_t(st)
	}
	in.mu.Lock()
	if in.state != stateCreated {
		in.mu.Unlock()
		return C.int32_t(oahost.StatusState)
	}
	in.state = stateStarting
	in.mu.Unlock()
	go in.start()
	return C.int32_t(oahost.StatusOK)
}

func (in *instance) start() {
	defer close(in.started)
	defer func() {
		if r := recover(); r != nil {
			in.fail(fmt.Sprintf("panic during start: %v", r))
		}
	}()
	// Log lines go to the WinCC OA log and to the log bus that feeds the
	// dashboard's log viewer (GraphQL systemLogs).
	logBus := mlog.NewBus(in.cfg.Logging.RingBufferSize)
	logger := slog.New(mlog.NewHandler(logBus, newHostLogger(in.host, in.cfg.Logging.Level).Handler(), in.cfg.NodeID))
	in.client.SetLogger(logger)
	in.startDiagnostics(logger)
	srv, err := broker.NewWithOptions(in.cfg, logger, logBus, broker.Options{OA: in.client})
	if err != nil {
		in.fail(err.Error())
		return
	}
	if err := srv.Serve(); err != nil {
		_ = srv.Close()
		in.fail(err.Error())
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	in.srv = srv
	if in.state == stateStarting {
		in.state = stateRunning
	}
}

func (in *instance) fail(msg string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.lastErr = msg
	if in.state == stateStarting {
		in.state = stateFailed
	}
	in.host.log(3, "monstermq start failed: "+msg)
}

//export mmq_state
func mmq_state(h C.uint64_t, errBuf *C.char, errCap C.uint32_t, errLen *C.uint32_t) (rc C.int32_t) {
	defer guard(&rc)
	in, st := current(h)
	if in == nil {
		return C.int32_t(st)
	}
	in.mu.Lock()
	state, msg := in.state, in.lastErr
	in.mu.Unlock()
	if errLen != nil {
		*errLen = C.uint32_t(len(msg))
	}
	if errBuf != nil && errCap > 0 && len(msg) > 0 {
		n := min(int(errCap), len(msg))
		b := []byte(msg[:n])
		C.memcpy(unsafe.Pointer(errBuf), unsafe.Pointer(&b[0]), C.size_t(n))
	}
	return C.int32_t(state)
}

//export mmq_stop
func mmq_stop(h C.uint64_t, timeoutMs C.uint32_t) (rc C.int32_t) {
	defer guard(&rc)
	in, st := current(h)
	if in == nil {
		return C.int32_t(st)
	}
	in.mu.Lock()
	switch in.state {
	case stateStopping, stateStopped:
		in.mu.Unlock()
		return C.int32_t(oahost.StatusOK)
	case stateCreated:
		in.state = stateStopped
		in.mu.Unlock()
		in.client.Close()
		close(in.stopped)
		return C.int32_t(oahost.StatusOK)
	}
	in.state = stateStopping
	in.mu.Unlock()
	timeout := time.Duration(timeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	go in.stop(timeout)
	return C.int32_t(oahost.StatusOK)
}

func (in *instance) stop(timeout time.Duration) {
	defer close(in.stopped)
	deadline := time.Now().Add(timeout)
	select {
	case <-in.started:
	case <-time.After(time.Until(deadline)):
	}
	in.mu.Lock()
	srv := in.srv
	in.mu.Unlock()
	done := make(chan error, 1)
	if srv != nil {
		go func() { done <- srv.Close() }()
	} else {
		done <- nil
	}
	var msg string
	select {
	case err := <-done:
		if err != nil {
			msg = err.Error()
		}
	case <-time.After(time.Until(deadline)):
		msg = "stop exceeded its timeout; remaining work abandoned"
	}
	in.stopDiagnostics()
	// Pending OA requests fail with ErrStopped; late host completions are
	// then rejected with MMQ_E_NOT_FOUND.
	in.client.Close()
	in.mu.Lock()
	in.state = stateStopped
	if msg != "" {
		in.lastErr = msg
	}
	in.mu.Unlock()
}

//export mmq_destroy
func mmq_destroy(h C.uint64_t) (rc C.int32_t) {
	defer guard(&rc)
	instMu.Lock()
	defer instMu.Unlock()
	if inst == nil || inst.handle != uint64(h) {
		return C.int32_t(oahost.StatusState)
	}
	inst.mu.Lock()
	state := inst.state
	inst.mu.Unlock()
	switch state {
	case stateCreated:
		inst.client.Close()
	case stateStopped, stateFailed:
		if state == stateFailed {
			inst.client.Close()
		}
	default:
		return C.int32_t(oahost.StatusState)
	}
	C.free(unsafe.Pointer(inst.chost))
	inst = nil
	return C.int32_t(oahost.StatusOK)
}

//export mmq_complete
func mmq_complete(h C.uint64_t, id C.uint64_t, status C.int32_t, data *C.uint8_t, n C.uint32_t) (rc C.int32_t) {
	defer guard(&rc)
	in, st := current(h)
	if in == nil {
		return C.int32_t(st)
	}
	b, code := copyIn(data, n)
	if code != oahost.StatusOK {
		// Still resolve the waiter so it does not hang until its deadline.
		_ = in.client.Complete(uint64(id), code, nil)
		return C.int32_t(code)
	}
	if err := in.client.Complete(uint64(id), int32(status), b); err != nil {
		return C.int32_t(oahost.ErrorStatus(err))
	}
	return C.int32_t(oahost.StatusOK)
}

//export mmq_event
func mmq_event(h C.uint64_t, ref C.uint64_t, data *C.uint8_t, n C.uint32_t) (rc C.int32_t) {
	defer guard(&rc)
	in, st := current(h)
	if in == nil {
		return C.int32_t(st)
	}
	b, code := copyIn(data, n)
	if code != oahost.StatusOK {
		return C.int32_t(code)
	}
	if err := in.client.Event(uint64(ref), b); err != nil {
		return C.int32_t(oahost.ErrorStatus(err))
	}
	return C.int32_t(oahost.StatusOK)
}

//export mmq_stats
func mmq_stats(h C.uint64_t, data *C.uint8_t, n C.uint32_t) (rc C.int32_t) {
	defer guard(&rc)
	in, st := current(h)
	if in == nil {
		return C.int32_t(st)
	}
	b, code := copyIn(data, n)
	if code != oahost.StatusOK {
		return C.int32_t(code)
	}
	if err := in.client.SetHostStats(b); err != nil {
		return C.int32_t(oahost.ErrorStatus(err))
	}
	return C.int32_t(oahost.StatusOK)
}

func copyIn(data *C.uint8_t, n C.uint32_t) ([]byte, int32) {
	if n == 0 {
		return nil, oahost.StatusOK
	}
	if data == nil {
		return nil, oahost.StatusInvalid
	}
	if uint32(n) > oahost.MaxMessage {
		return nil, oahost.StatusTooLarge
	}
	return C.GoBytes(unsafe.Pointer(data), C.int(n)), oahost.StatusOK
}
