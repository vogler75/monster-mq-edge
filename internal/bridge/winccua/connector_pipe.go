package winccua

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// pipeConnector talks WinCC Unified Open Pipe (expert/JSON syntax).
//
// All requests/responses are correlated by ClientCookie. Subscriptions are
// kept in activeSubscriptions; paged BrowseTags state lives in pendingBrowses
// until the server returns an empty page or ErrorBrowseTags ("browse expired"
// = browse complete).
//
// Tag publish modes (per address):
//   SINGLE — one SubscribeTag per tag; topic "<ns>/<addr.topic>/<tag>".
//   BULK   — one SubscribeTag with all tags; on every change the full Tags
//            array is dropped on "<ns>/<addr.topic>" as a JSON payload.
type pipeConnector struct {
	name    string
	cfg     *ConnectionConfig
	pub     *publisher
	publish LocalPublisher
	logger  *slog.Logger
	metrics metrics

	ctx     context.Context
	cancel  context.CancelFunc
	stopped chan struct{}

	mu               sync.Mutex
	conn             io.ReadWriteCloser
	cookieSeq        int64
	activeSubs       map[string]Address // cookie → address
	pendingBrowses   map[string]*browseState

	writeMu        sync.Mutex // serialize writes on the pipe handle
	firstTagLogged atomic.Bool
}

type browseState struct {
	address Address
	pageDone chan []string
	pageErr  chan error
	accum    []string
}

func newPipeConnector(name string, cfg *ConnectionConfig, pub *publisher, publish LocalPublisher, logger *slog.Logger) *pipeConnector {
	return &pipeConnector{
		name:           name,
		cfg:            cfg,
		pub:            pub,
		publish:        publish,
		logger:         logger.With("device", name, "transport", "openpipe"),
		activeSubs:     map[string]Address{},
		pendingBrowses: map[string]*browseState{},
		stopped:        make(chan struct{}),
	}
}

func (p *pipeConnector) MessagesIn() float64 { return p.metrics.sampleRate() }
func (p *pipeConnector) IsConnected() bool   { return p.metrics.connFlag.Load() }

func (p *pipeConnector) Start(ctx context.Context) error {
	p.ctx, p.cancel = context.WithCancel(ctx)
	go p.runLoop()
	return nil
}

func (p *pipeConnector) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
	<-p.stopped
}

func (p *pipeConnector) runLoop() {
	defer close(p.stopped)
	for {
		if err := p.runOnce(p.ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			p.logger.Warn("winccua pipe session ended, will retry",
				"err", err, "retry_in_ms", p.cfg.ReconnectDelay)
		}
		select {
		case <-p.ctx.Done():
			return
		case <-time.After(time.Duration(p.cfg.ReconnectDelay) * time.Millisecond):
		}
	}
}

func (p *pipeConnector) runOnce(ctx context.Context) error {
	path := p.cfg.ResolvePipePath()
	p.logger.Info("winccua pipe dialing",
		"path", path, "timeout_ms", p.cfg.ConnectionTimeout,
		"reconnect_delay_ms", p.cfg.ReconnectDelay)
	dialStart := time.Now()
	conn, err := dialPipe(ctx, path, time.Duration(p.cfg.ConnectionTimeout)*time.Millisecond)
	if err != nil {
		return fmt.Errorf("dial pipe %s after %s: %w", path, time.Since(dialStart).Round(time.Millisecond), err)
	}
	p.mu.Lock()
	p.conn = conn
	p.activeSubs = map[string]Address{}
	p.pendingBrowses = map[string]*browseState{}
	p.firstTagLogged.Store(false)
	p.mu.Unlock()
	p.metrics.connFlag.Store(true)
	p.logger.Info("winccua pipe connected",
		"path", path, "dial_duration", time.Since(dialStart).Round(time.Millisecond))

	// readLoop processes incoming lines until ctx done or read error.
	errCh := make(chan error, 1)
	go func() { errCh <- p.readLoop(conn) }()

	// Setup subscriptions in parallel with the read loop being ready.
	go p.setupSubscriptions(ctx)

	// Cleanup must run BEFORE the function returns so the pipe handle is
	// fully released and the readLoop goroutine has exited — otherwise the
	// kernel-side handle lingers, WinCC RT keeps the previous pipe instance
	// busy, and the next dial sits in WaitNamedPipe until our 10s timeout.
	gracefulShutdown := func(reason string) {
		// Send UnsubscribeTag / UnsubscribeAlarm for every active subscription
		// BEFORE closing the conn. Otherwise RT considers the previous client
		// session "logically alive" and rejects new dials with ERROR_PIPE_BUSY
		// until it tries to push a notification and finds the pipe dead —
		// which can take 10s+ if no values change in the meantime.
		p.sendUnsubscribesBeforeClose()
		_ = conn.Close() // unblocks readLoop's scanner
		<-errCh          // wait for readLoop to exit
		p.metrics.connFlag.Store(false)
		p.mu.Lock()
		p.conn = nil
		for _, b := range p.pendingBrowses {
			select {
			case b.pageErr <- errors.New("pipe disconnected: " + reason):
			default:
			}
		}
		p.pendingBrowses = map[string]*browseState{}
		p.activeSubs = map[string]Address{}
		p.mu.Unlock()
		p.logger.Info("winccua pipe disconnected", "reason", reason)
	}

	select {
	case <-ctx.Done():
		gracefulShutdown("context cancelled")
		return ctx.Err()
	case err := <-errCh:
		// readLoop already returned; conn is already broken — no point in
		// trying to send unsubscribes, just clean up.
		_ = conn.Close()
		p.metrics.connFlag.Store(false)
		p.mu.Lock()
		p.conn = nil
		for _, b := range p.pendingBrowses {
			select {
			case b.pageErr <- errors.New("pipe disconnected: read failed"):
			default:
			}
		}
		p.pendingBrowses = map[string]*browseState{}
		p.activeSubs = map[string]Address{}
		p.mu.Unlock()
		p.logger.Info("winccua pipe disconnected", "reason", "read failed")
		return err
	}
}

// sendUnsubscribesBeforeClose walks the active subscription table and emits
// one UnsubscribeTag / UnsubscribeAlarm per cookie. Best-effort: errors are
// swallowed because we are about to close the pipe anyway. The grace sleep
// gives RT's OpennessManager a moment to process the unsubscribes before our
// pipe close races against its event loop.
func (p *pipeConnector) sendUnsubscribesBeforeClose() {
	p.mu.Lock()
	type entry struct{ cookie, msg string }
	entries := make([]entry, 0, len(p.activeSubs))
	for cookie, addr := range p.activeSubs {
		msg := "UnsubscribeTag"
		if addr.Type == AddressTypeActiveAlarms {
			msg = "UnsubscribeAlarm"
		}
		entries = append(entries, entry{cookie, msg})
	}
	p.mu.Unlock()
	if len(entries) == 0 {
		return
	}
	for _, e := range entries {
		_ = p.sendCommand(map[string]any{
			"Message":      e.msg,
			"ClientCookie": e.cookie,
		})
	}
	p.logger.Info("winccua pipe unsubscribed before close", "count", len(entries))
	// Without this RT can still be mid-flight on the unsubscribes when we
	// yank the pipe handle, leaving its session state in limbo.
	time.Sleep(200 * time.Millisecond)
}

func (p *pipeConnector) readLoop(conn io.Reader) error {
	scanner := bufio.NewScanner(conn)
	// Allow large lines — alarm payloads can be sizable.
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		p.logger.Debug("winccua pipe RX", "line", line)
		p.handleMessage(line)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return io.EOF
}

func (p *pipeConnector) sendCommand(req map[string]any) error {
	enc, err := json.Marshal(req)
	if err != nil {
		return err
	}
	p.mu.Lock()
	conn := p.conn
	p.mu.Unlock()
	if conn == nil {
		return errors.New("pipe not connected")
	}
	p.logger.Debug("winccua pipe TX", "line", string(enc))
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_, err = conn.Write(append(enc, '\n'))
	return err
}

func (p *pipeConnector) nextCookie(prefix string) string {
	return fmt.Sprintf("%s-%s-%d", p.name, prefix, atomic.AddInt64(&p.cookieSeq, 1))
}

// ---- subscription setup --------------------------------------------------

func (p *pipeConnector) setupSubscriptions(ctx context.Context) {
	for _, addr := range p.cfg.Addresses {
		if ctx.Err() != nil {
			return
		}
		switch addr.Type {
		case AddressTypeTagValues:
			if err := p.setupTagValuesSubscription(ctx, addr); err != nil {
				p.logger.Warn("winccua pipe tag setup failed", "topic", addr.Topic, "err", err)
			}
		case AddressTypeActiveAlarms:
			if err := p.setupActiveAlarmsSubscription(addr); err != nil {
				p.logger.Warn("winccua pipe alarm setup failed", "topic", addr.Topic, "err", err)
			}
		}
	}
}

func (p *pipeConnector) setupTagValuesSubscription(ctx context.Context, addr Address) error {
	tags, err := p.executeBrowse(ctx, addr)
	if err != nil {
		return err
	}
	if len(tags) == 0 {
		p.logger.Warn("winccua pipe browse returned no tags", "topic", addr.Topic)
		return nil
	}
	p.logger.Info("winccua pipe browse complete", "topic", addr.Topic, "tagCount", len(tags))
	return p.subscribeToTags(addr, tags)
}

// executeBrowse runs a paged BrowseTags. The expert-syntax server closes the
// browse session when the last page contains fewer entries than the page
// size; a subsequent Next returns ErrorBrowseTags ("Your browse request has
// been expired") which we treat as browse complete.
func (p *pipeConnector) executeBrowse(ctx context.Context, addr Address) ([]string, error) {
	if len(addr.NameFilters) == 0 {
		return nil, errors.New("no name filters")
	}
	cookie := p.nextCookie("browse")

	state := &browseState{
		address:  addr,
		pageDone: make(chan []string, 1),
		pageErr:  make(chan error, 1),
	}
	p.mu.Lock()
	p.pendingBrowses[cookie] = state
	p.mu.Unlock()

	// BrowseTags accepts a single Filter; use the first name filter and warn
	// for the rest, mirroring the Kotlin connector.
	filter := addr.NameFilters[0]
	if len(addr.NameFilters) > 1 {
		p.logger.Warn("winccua pipe browseTags accepts a single filter; using first",
			"filter", filter, "ignored", addr.NameFilters[1:])
	}

	params := map[string]any{"Filter": filter}
	if len(addr.SystemNames) > 0 {
		params["SystemNames"] = addr.SystemNames
	}
	p.logger.Info("winccua pipe BrowseTags",
		"topic", addr.Topic, "filter", filter, "systemNames", addr.SystemNames,
		"cookie", cookie)
	if err := p.sendCommand(map[string]any{
		"Message":      "BrowseTags",
		"Params":       params,
		"ClientCookie": cookie,
	}); err != nil {
		p.mu.Lock()
		delete(p.pendingBrowses, cookie)
		p.mu.Unlock()
		return nil, err
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case res := <-state.pageDone:
			return res, nil
		case err := <-state.pageErr:
			return nil, err
		}
	}
}

func (p *pipeConnector) subscribeToTags(addr Address, tags []string) error {
	mode := addr.PipeTagMode
	if mode == "" {
		mode = PipeTagModeSingle
	}
	if mode == PipeTagModeSingle {
		for _, tag := range tags {
			cookie := p.nextCookie("tagvalues")
			p.mu.Lock()
			p.activeSubs[cookie] = addr
			p.mu.Unlock()
			if err := p.sendCommand(map[string]any{
				"Message":      "SubscribeTag",
				"Params":       map[string]any{"Tags": []string{tag}},
				"ClientCookie": cookie,
			}); err != nil {
				p.mu.Lock()
				delete(p.activeSubs, cookie)
				p.mu.Unlock()
			}
		}
		p.logger.Info("winccua pipe SubscribeTag SINGLE", "topic", addr.Topic, "tagCount", len(tags))
		return nil
	}

	cookie := p.nextCookie("tagvalues")
	p.mu.Lock()
	p.activeSubs[cookie] = addr
	p.mu.Unlock()
	if err := p.sendCommand(map[string]any{
		"Message":      "SubscribeTag",
		"Params":       map[string]any{"Tags": tags},
		"ClientCookie": cookie,
	}); err != nil {
		p.mu.Lock()
		delete(p.activeSubs, cookie)
		p.mu.Unlock()
		return err
	}
	p.logger.Info("winccua pipe SubscribeTag BULK", "topic", addr.Topic, "tagCount", len(tags), "cookie", cookie)
	return nil
}

func (p *pipeConnector) setupActiveAlarmsSubscription(addr Address) error {
	cookie := p.nextCookie("alarms")
	params := map[string]any{}
	if len(addr.SystemNames) > 0 {
		params["SystemNames"] = addr.SystemNames
	}
	if addr.FilterString != "" {
		params["Filter"] = addr.FilterString
	}
	p.mu.Lock()
	p.activeSubs[cookie] = addr
	p.mu.Unlock()
	if err := p.sendCommand(map[string]any{
		"Message":      "SubscribeAlarm",
		"Params":       params,
		"ClientCookie": cookie,
	}); err != nil {
		p.mu.Lock()
		delete(p.activeSubs, cookie)
		p.mu.Unlock()
		return err
	}
	p.logger.Info("winccua pipe SubscribeAlarm", "topic", addr.Topic, "cookie", cookie)
	return nil
}

// ---- incoming dispatch --------------------------------------------------

func (p *pipeConnector) handleMessage(line string) {
	msg := map[string]any{}
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		p.logger.Warn("winccua pipe parse failed", "err", err, "line", line)
		return
	}
	t, _ := msg["Message"].(string)
	switch t {
	case "":
		p.logger.Warn("winccua pipe message without Message field", "line", line)
	case "NotifyBrowseTags":
		p.handleBrowseTagsResponse(msg)
	case "NotifySubscribeTag", "NotifyReadTag":
		p.handleTagNotification(msg)
	case "NotifySubscribeAlarm", "NotifyReadAlarm":
		p.handleAlarmNotification(msg)
	case "NotifyUnsubscribeTag", "NotifyUnsubscribeAlarm":
		cookie, _ := msg["ClientCookie"].(string)
		p.mu.Lock()
		delete(p.activeSubs, cookie)
		p.mu.Unlock()
	default:
		if len(t) >= 5 && t[:5] == "Error" {
			p.handleErrorMessage(msg, t)
		}
	}
}

func (p *pipeConnector) handleBrowseTagsResponse(msg map[string]any) {
	cookie, _ := msg["ClientCookie"].(string)
	p.mu.Lock()
	state := p.pendingBrowses[cookie]
	p.mu.Unlock()
	if state == nil {
		p.logger.Warn("winccua pipe NotifyBrowseTags for unknown cookie", "cookie", cookie)
		return
	}

	params, _ := msg["Params"].(map[string]any)
	tags, _ := params["Tags"].([]any)
	if len(tags) == 0 {
		p.mu.Lock()
		delete(p.pendingBrowses, cookie)
		p.mu.Unlock()
		p.logger.Info("winccua pipe BrowseTags empty page (browse done)",
			"cookie", cookie, "totalTags", len(state.accum))
		state.pageDone <- append([]string(nil), state.accum...)
		return
	}
	for _, item := range tags {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		name, _ := m["Name"].(string)
		if name == "" {
			name, _ = m["name"].(string)
		}
		if name != "" {
			state.accum = append(state.accum, name)
		}
	}
	p.logger.Info("winccua pipe BrowseTags page",
		"cookie", cookie, "pageTags", len(tags), "totalSoFar", len(state.accum))
	// Request next page
	if err := p.sendCommand(map[string]any{
		"Message":      "BrowseTags",
		"Params":       "Next",
		"ClientCookie": cookie,
	}); err != nil {
		p.mu.Lock()
		delete(p.pendingBrowses, cookie)
		p.mu.Unlock()
		state.pageErr <- fmt.Errorf("send Next: %w", err)
	}
}

func (p *pipeConnector) handleTagNotification(msg map[string]any) {
	cookie, _ := msg["ClientCookie"].(string)
	p.mu.Lock()
	addr, ok := p.activeSubs[cookie]
	p.mu.Unlock()
	if !ok {
		p.logger.Warn("winccua pipe tag notification for unknown cookie", "cookie", cookie)
		return
	}
	params, _ := msg["Params"].(map[string]any)
	if params == nil {
		return
	}
	tagsArr, _ := params["Tags"].([]any)
	if !p.firstTagLogged.Swap(true) {
		p.logger.Info("winccua pipe first tag notification arrived",
			"topic", addr.Topic, "tagsInPayload", len(tagsArr))
	}
	mode := addr.PipeTagMode
	if mode == "" {
		mode = PipeTagModeSingle
	}
	if mode == PipeTagModeBulk {
		topic := joinTopic(p.pub.namespace, addr.Topic)
		payload, _ := json.Marshal(tagsArr)
		if err := p.publish(topic, payload, addr.Retained, 0); err != nil {
			p.logger.Warn("winccua pipe BULK publish failed", "topic", topic, "err", err)
			return
		}
		p.metrics.inc()
		return
	}
	for _, item := range tagsArr {
		tag, _ := item.(map[string]any)
		if tag == nil {
			continue
		}
		if !tagErrorIsZero(tag["ErrorCode"]) {
			desc, _ := tag["ErrorDescription"].(string)
			name, _ := tag["Name"].(string)
			p.logger.Warn("winccua pipe tag error", "tag", name, "code", tag["ErrorCode"], "desc", desc)
			continue
		}
		name, _ := tag["Name"].(string)
		if name == "" {
			continue
		}
		value := tag["Value"]
		ts, _ := tag["TimeStamp"].(string)
		quality := pipeQualityFromTag(tag)
		p.publishTagValue(addr, name, value, ts, quality)
	}
}

func (p *pipeConnector) handleAlarmNotification(msg map[string]any) {
	cookie, _ := msg["ClientCookie"].(string)
	p.mu.Lock()
	addr, ok := p.activeSubs[cookie]
	p.mu.Unlock()
	if !ok {
		p.logger.Warn("winccua pipe alarm notification for unknown cookie", "cookie", cookie)
		return
	}
	params, _ := msg["Params"].(map[string]any)
	if params == nil {
		// Some servers spell the field "params" (lowercase) in alarm replies.
		params, _ = msg["params"].(map[string]any)
	}
	if params == nil {
		return
	}
	alarms, _ := params["Alarms"].([]any)
	for _, a := range alarms {
		alarm, _ := a.(map[string]any)
		if alarm == nil {
			continue
		}
		p.publishAlarm(addr, alarm)
	}
}

// handleErrorMessage handles Error<Command>. The browse session expiry is
// expected — when the last BrowseTags page contained fewer items than the
// page size, the server closes the browse and a subsequent Next returns
// ErrorBrowseTags ("Your browse request has been expired"). Treat as
// browse complete.
func (p *pipeConnector) handleErrorMessage(msg map[string]any, t string) {
	cookie, _ := msg["ClientCookie"].(string)
	desc, _ := msg["ErrorDescription"].(string)
	if t == "ErrorBrowseTags" && cookie != "" {
		p.mu.Lock()
		state := p.pendingBrowses[cookie]
		delete(p.pendingBrowses, cookie)
		p.mu.Unlock()
		if state != nil {
			p.logger.Info("winccua pipe browse expired (treated as complete)",
				"cookie", cookie, "desc", desc, "accumulated", len(state.accum))
			state.pageDone <- append([]string(nil), state.accum...)
			return
		}
	}
	p.logger.Warn("winccua pipe error message", "type", t, "cookie", cookie, "desc", desc)
	if cookie != "" {
		p.mu.Lock()
		state := p.pendingBrowses[cookie]
		delete(p.pendingBrowses, cookie)
		p.mu.Unlock()
		if state != nil {
			state.pageErr <- fmt.Errorf("%s: %s", t, desc)
		}
	}
}

func (p *pipeConnector) publishTagValue(addr Address, tagName string, value any, timestamp string, quality map[string]any) {
	topic := p.pub.resolveTagTopic(addr.Topic, tagName)
	payload := p.pub.formatTagPayload(value, timestamp, quality)
	if err := p.publish(topic, payload, addr.Retained, 0); err != nil {
		p.logger.Warn("winccua pipe publish failed", "topic", topic, "err", err)
		return
	}
	p.metrics.inc()
}

func (p *pipeConnector) publishAlarm(addr Address, alarm map[string]any) {
	name := resolveAlarmName(alarm)
	topic := p.pub.resolveAlarmTopic(addr.Topic, name)
	payload, _ := json.Marshal(alarm)
	if err := p.publish(topic, payload, addr.Retained, 0); err != nil {
		p.logger.Warn("winccua pipe alarm publish failed", "topic", topic, "err", err)
		return
	}
	p.metrics.inc()
}

// tagErrorIsZero treats nil, the integer 0, the string "0", and an empty
// string as success — matching the WinCCUaPipeConnector.kt logic.
func tagErrorIsZero(v any) bool {
	switch n := v.(type) {
	case nil:
		return true
	case float64:
		return n == 0
	case int:
		return n == 0
	case int64:
		return n == 0
	case string:
		if n == "" || n == "0" {
			return true
		}
		i, err := strconv.ParseInt(n, 10, 64)
		return err == nil && i == 0
	}
	return false
}

func pipeQualityFromTag(tag map[string]any) map[string]any {
	q, qok := tag["Quality"].(string)
	c, cok := tag["QualityCode"].(string)
	if !qok && !cok {
		return nil
	}
	out := map[string]any{}
	if qok {
		out["quality"] = q
	}
	if cok {
		out["qualityCode"] = c
	}
	return out
}
