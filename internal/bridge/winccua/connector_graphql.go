package winccua

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// graphqlConnector is the WebSocket-based transport.
//
// Lifecycle (per Start invocation):
//   1. POST login mutation, capture bearer token.
//   2. Open ws/wss with the graphql-transport-ws subprotocol.
//   3. Send connection_init with {"Authorization":"Bearer ..."} payload.
//   4. After connection_ack, for each address run setupSubscription:
//      - TAG_VALUES: HTTP browse query → tag list → "subscribe" with that list.
//      - ACTIVE_ALARMS: "subscribe" directly.
//   5. Read loop dispatches "next" messages to handleSubscriptionData.
//   6. On disconnect or error, sleep ReconnectDelay and retry.
type graphqlConnector struct {
	name    string
	cfg     *ConnectionConfig
	pub     *publisher
	publish LocalPublisher
	logger  *slog.Logger
	metrics metrics

	ctx     context.Context
	cancel  context.CancelFunc
	stopped chan struct{}

	mu           sync.Mutex
	ws           *websocket.Conn
	subSeq       int64
	subscriptions map[string]Address // subscription id → address
	authToken    string

	httpClient *http.Client
}

func newGraphQLConnector(name string, cfg *ConnectionConfig, pub *publisher, publish LocalPublisher, logger *slog.Logger) *graphqlConnector {
	return &graphqlConnector{
		name:          name,
		cfg:           cfg,
		pub:           pub,
		publish:       publish,
		logger:        logger.With("device", name, "transport", "graphql"),
		subscriptions: map[string]Address{},
		stopped:       make(chan struct{}),
		httpClient: &http.Client{
			Timeout: time.Duration(cfg.ConnectionTimeout) * time.Millisecond,
		},
	}
}

func (g *graphqlConnector) MessagesIn() float64 { return g.metrics.sampleRate() }
func (g *graphqlConnector) IsConnected() bool   { return g.metrics.connFlag.Load() }

func (g *graphqlConnector) Start(ctx context.Context) error {
	g.ctx, g.cancel = context.WithCancel(ctx)
	go g.runLoop()
	return nil
}

func (g *graphqlConnector) Stop() {
	if g.cancel != nil {
		g.cancel()
	}
	<-g.stopped
}

// runLoop owns the connect/subscribe/read cycle and drives reconnect.
func (g *graphqlConnector) runLoop() {
	defer close(g.stopped)
	for {
		if err := g.runOnce(g.ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			g.logger.Warn("winccua graphql session ended", "err", err)
		}
		select {
		case <-g.ctx.Done():
			return
		case <-time.After(time.Duration(g.cfg.ReconnectDelay) * time.Millisecond):
		}
	}
}

func (g *graphqlConnector) runOnce(ctx context.Context) error {
	g.logger.Info("winccua graphql connecting",
		"endpoint", g.cfg.GraphqlEndpoint, "ws", g.cfg.WebSocketEndpoint(),
		"reconnect_delay_ms", g.cfg.ReconnectDelay)
	if err := g.authenticate(ctx); err != nil {
		return fmt.Errorf("authenticate: %w", err)
	}
	if err := g.openWebSocket(ctx); err != nil {
		return fmt.Errorf("ws connect: %w", err)
	}
	g.metrics.connFlag.Store(true)
	defer func() {
		g.metrics.connFlag.Store(false)
		g.mu.Lock()
		if g.ws != nil {
			_ = g.ws.Close()
			g.ws = nil
		}
		g.subscriptions = map[string]Address{}
		g.mu.Unlock()
	}()

	if err := g.setupSubscriptions(ctx); err != nil {
		return fmt.Errorf("setup subs: %w", err)
	}
	return g.readLoop(ctx)
}

// authenticate runs the login mutation and stores the bearer token.
func (g *graphqlConnector) authenticate(ctx context.Context) error {
	mut := fmt.Sprintf(`mutation { login(username: %q, password: %q) { token expires } }`,
		g.cfg.Username, g.cfg.Password)
	resp, err := g.postGraphQL(ctx, mut, "")
	if err != nil {
		return err
	}
	data, _ := resp["data"].(map[string]any)
	if data == nil {
		if errs, ok := resp["errors"]; ok {
			return fmt.Errorf("login failed: %v", errs)
		}
		return errors.New("login response missing data")
	}
	login, _ := data["login"].(map[string]any)
	if login == nil {
		return errors.New("login response missing login object")
	}
	token, _ := login["token"].(string)
	if token == "" {
		return errors.New("login response missing token")
	}
	g.authToken = token
	g.logger.Info("winccua graphql authenticated")
	return nil
}

func (g *graphqlConnector) postGraphQL(ctx context.Context, query string, authToken string) (map[string]any, error) {
	body, _ := json.Marshal(map[string]any{"query": query})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.cfg.GraphqlEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	res, err := g.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", res.StatusCode, string(raw))
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode: %w (body: %s)", err, string(raw))
	}
	return out, nil
}

// openWebSocket dials the graphql-transport-ws WebSocket and sends connection_init.
func (g *graphqlConnector) openWebSocket(ctx context.Context) error {
	wsURL := g.cfg.WebSocketEndpoint()
	u, err := url.Parse(wsURL)
	if err != nil {
		return fmt.Errorf("parse ws url: %w", err)
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: time.Duration(g.cfg.ConnectionTimeout) * time.Millisecond,
		Subprotocols:     []string{"graphql-transport-ws"},
	}
	headers := http.Header{}
	conn, _, err := dialer.DialContext(ctx, u.String(), headers)
	if err != nil {
		return err
	}
	g.mu.Lock()
	g.ws = conn
	g.mu.Unlock()
	g.logger.Info("winccua graphql ws connected", "url", wsURL, "subprotocol", conn.Subprotocol())

	initMsg := map[string]any{
		"type":    "connection_init",
		"payload": map[string]any{"Authorization": "Bearer " + g.authToken},
	}
	if err := conn.WriteJSON(initMsg); err != nil {
		return fmt.Errorf("send connection_init: %w", err)
	}
	// Wait for connection_ack (or hard error) before issuing subscriptions.
	conn.SetReadDeadline(time.Now().Add(time.Duration(g.cfg.ConnectionTimeout) * time.Millisecond))
	for {
		msg := map[string]any{}
		if err := conn.ReadJSON(&msg); err != nil {
			return fmt.Errorf("read connection_ack: %w", err)
		}
		switch msg["type"] {
		case "connection_ack":
			conn.SetReadDeadline(time.Time{})
			return nil
		case "ka", "ping":
			// keep waiting
		case "connection_error", "error":
			return fmt.Errorf("connection_error: %v", msg["payload"])
		}
	}
}

// setupSubscriptions creates a GraphQL subscription per configured address.
func (g *graphqlConnector) setupSubscriptions(ctx context.Context) error {
	for _, addr := range g.cfg.Addresses {
		switch addr.Type {
		case AddressTypeTagValues:
			if err := g.setupTagValuesSubscription(ctx, addr); err != nil {
				g.logger.Warn("winccua tag subscription failed", "topic", addr.Topic, "err", err)
			}
		case AddressTypeActiveAlarms:
			if err := g.setupActiveAlarmsSubscription(addr); err != nil {
				g.logger.Warn("winccua alarm subscription failed", "topic", addr.Topic, "err", err)
			}
		}
	}
	return nil
}

func (g *graphqlConnector) setupTagValuesSubscription(ctx context.Context, addr Address) error {
	tags, err := g.browseTags(ctx, addr)
	if err != nil {
		return fmt.Errorf("browse: %w", err)
	}
	if len(tags) == 0 {
		return errors.New("browse returned no tags")
	}
	g.logger.Info("winccua browse complete", "topic", addr.Topic, "tagCount", len(tags))

	tagsJSON, _ := json.Marshal(tags)
	qualityFields := ""
	if addr.IncludeQuality {
		qualityFields = `
                        quality {
                            quality
                            subStatus
                            limit
                            extendedSubStatus
                            sourceQuality
                            sourceTime
                            timeCorrected
                        }`
	}
	subQuery := fmt.Sprintf(`subscription {
        tagValues(names: %s) {
            name
            value {
                value
                timestamp%s
            }
            error {
                code
                description
            }
        }
    }`, string(tagsJSON), qualityFields)
	return g.sendSubscribe(addr, "tagvalues", subQuery)
}

func (g *graphqlConnector) browseTags(ctx context.Context, addr Address) ([]string, error) {
	if len(addr.NameFilters) == 0 {
		return nil, errors.New("no name filters")
	}
	filtersJSON, _ := json.Marshal(addr.NameFilters)
	q := fmt.Sprintf(`query { browse(nameFilters: %s) { name } }`, string(filtersJSON))
	resp, err := g.postGraphQL(ctx, q, g.authToken)
	if err != nil {
		return nil, err
	}
	if errs, ok := resp["errors"]; ok && errs != nil {
		return nil, fmt.Errorf("graphql browse errors: %v", errs)
	}
	data, _ := resp["data"].(map[string]any)
	if data == nil {
		return nil, errors.New("missing data")
	}
	browse, _ := data["browse"].([]any)
	tags := make([]string, 0, len(browse))
	for _, item := range browse {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		if name, ok := m["name"].(string); ok && name != "" {
			tags = append(tags, name)
		}
	}
	return tags, nil
}

func (g *graphqlConnector) setupActiveAlarmsSubscription(addr Address) error {
	filters := []string{}
	if len(addr.SystemNames) > 0 {
		sn, _ := json.Marshal(addr.SystemNames)
		filters = append(filters, "systemNames: "+string(sn))
	}
	if addr.FilterString != "" {
		fs, _ := json.Marshal(addr.FilterString)
		filters = append(filters, "filterString: "+string(fs))
	}
	args := ""
	if len(filters) > 0 {
		args = "(" + joinStr(filters, ", ") + ")"
	}
	subQuery := `subscription {
        activeAlarms` + args + ` {
            name instanceID alarmGroupID raiseTime acknowledgmentTime clearTime resetTime modificationTime
            state textColor backColor flashing languages alarmClassName alarmClassSymbol alarmClassID
            stateMachine priority alarmParameterValues alarmType eventText infoText
            alarmText1 alarmText2 alarmText3 alarmText4 alarmText5 alarmText6 alarmText7 alarmText8 alarmText9
            stateText origin area changeReason connectionName valueLimit sourceType suppressionState
            hostName userName value
            valueQuality { quality subStatus limit extendedSubStatus sourceQuality sourceTime timeCorrected }
            quality { quality subStatus limit extendedSubStatus sourceQuality sourceTime timeCorrected }
            invalidFlags { invalidConfiguration invalidTimestamp invalidAlarmParameter invalidEventText }
            deadBand producer duration durationIso sourceID systemSeverity loopInAlarm
            loopInAlarmParameterValues path userResponse notificationReason
        }
    }`
	return g.sendSubscribe(addr, "alarms", subQuery)
}

func (g *graphqlConnector) sendSubscribe(addr Address, kind, query string) error {
	id := fmt.Sprintf("%s-%s-%d", g.name, kind, atomic.AddInt64(&g.subSeq, 1))
	msg := map[string]any{
		"id":      id,
		"type":    "subscribe",
		"payload": map[string]any{"query": query},
	}
	g.mu.Lock()
	g.subscriptions[id] = addr
	conn := g.ws
	g.mu.Unlock()
	if conn == nil {
		return errors.New("ws not connected")
	}
	return conn.WriteJSON(msg)
}

// readLoop processes incoming WebSocket frames until the connection closes
// or the context is cancelled.
func (g *graphqlConnector) readLoop(ctx context.Context) error {
	g.mu.Lock()
	conn := g.ws
	g.mu.Unlock()
	if conn == nil {
		return errors.New("ws closed before read")
	}

	// Cancellation: closing the conn from a goroutine on ctx.Done unblocks
	// the synchronous Read.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	for {
		msg := map[string]any{}
		if err := conn.ReadJSON(&msg); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		switch t, _ := msg["type"].(string); t {
		case "connection_ack":
			// ignore
		case "next", "data":
			g.handleSubscriptionData(msg)
		case "error":
			g.logger.Warn("winccua graphql subscription error", "payload", msg["payload"])
		case "complete":
			id, _ := msg["id"].(string)
			g.mu.Lock()
			delete(g.subscriptions, id)
			g.mu.Unlock()
		case "ping":
			_ = conn.WriteJSON(map[string]any{"type": "pong"})
		case "ka", "pong":
			// noop
		}
	}
}

func (g *graphqlConnector) handleSubscriptionData(msg map[string]any) {
	id, _ := msg["id"].(string)
	g.mu.Lock()
	addr, ok := g.subscriptions[id]
	g.mu.Unlock()
	if !ok {
		return
	}
	payload, _ := msg["payload"].(map[string]any)
	if payload == nil {
		return
	}
	data, _ := payload["data"].(map[string]any)
	if data == nil {
		return
	}
	switch addr.Type {
	case AddressTypeTagValues:
		g.handleTagValuesData(data, addr)
	case AddressTypeActiveAlarms:
		g.handleActiveAlarmsData(data, addr)
	}
}

func (g *graphqlConnector) handleTagValuesData(data map[string]any, addr Address) {
	tv, _ := data["tagValues"].(map[string]any)
	if tv == nil {
		return
	}
	tagName, _ := tv["name"].(string)
	if tagName == "" {
		return
	}
	if errObj, ok := tv["error"].(map[string]any); ok && errObj != nil {
		code, _ := errObj["code"].(string)
		if code != "" && code != "0" {
			desc, _ := errObj["description"].(string)
			g.logger.Warn("winccua tag value error", "tag", tagName, "code", code, "desc", desc)
			return
		}
	}
	valueObj, _ := tv["value"].(map[string]any)
	if valueObj == nil {
		return
	}
	value := valueObj["value"]
	timestamp, _ := valueObj["timestamp"].(string)
	var quality map[string]any
	if q, ok := valueObj["quality"].(map[string]any); ok {
		quality = q
	}
	g.publishTagValue(addr, tagName, value, timestamp, quality)
}

func (g *graphqlConnector) handleActiveAlarmsData(data map[string]any, addr Address) {
	alarm, _ := data["activeAlarms"].(map[string]any)
	if alarm == nil {
		return
	}
	g.publishAlarm(addr, alarm)
}

func (g *graphqlConnector) publishTagValue(addr Address, tagName string, value any, timestamp string, quality map[string]any) {
	if !addr.IncludeQuality {
		quality = nil
	}
	topic := g.pub.resolveTagTopic(addr.Topic, tagName)
	payload := g.pub.formatTagPayload(value, timestamp, quality)
	if err := g.publish(topic, payload, addr.Retained, 0); err != nil {
		g.logger.Warn("winccua publish failed", "topic", topic, "err", err)
		return
	}
	g.metrics.inc()
}

func (g *graphqlConnector) publishAlarm(addr Address, alarm map[string]any) {
	name := resolveAlarmName(alarm)
	topic := g.pub.resolveAlarmTopic(addr.Topic, name)
	payload, _ := json.Marshal(alarm)
	if err := g.publish(topic, payload, addr.Retained, 0); err != nil {
		g.logger.Warn("winccua alarm publish failed", "topic", topic, "err", err)
		return
	}
	g.metrics.inc()
}

func joinStr(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}
