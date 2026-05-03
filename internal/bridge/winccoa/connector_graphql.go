package winccoa

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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

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

	mu            sync.Mutex
	ws            *websocket.Conn
	subSeq        int64
	subscriptions map[string]Address
	authToken     string

	httpClient *http.Client
}

func newGraphQLConnector(name string, cfg *ConnectionConfig, pub *publisher, publish LocalPublisher, logger *slog.Logger) *graphqlConnector {
	return &graphqlConnector{
		name:          name,
		cfg:           cfg,
		pub:           pub,
		publish:       publish,
		logger:        logger.With("device", name, "transport", "graphql"),
		stopped:       make(chan struct{}),
		subscriptions: map[string]Address{},
		httpClient: &http.Client{
			Timeout: time.Duration(cfg.ConnectionTimeout) * time.Millisecond,
		},
	}
}

func (g *graphqlConnector) MessagesIn() float64 { return g.metrics.sampleRate() }
func (g *graphqlConnector) IsConnected() bool   { return g.metrics.connFlag.Load() }

func (g *graphqlConnector) SampleMetrics(now time.Time) MetricsSnapshot {
	return MetricsSnapshot{
		MessagesIn: g.MessagesIn(),
		Connected:  g.IsConnected(),
		Timestamp:  now.UTC(),
	}
}

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

func (g *graphqlConnector) runLoop() {
	defer close(g.stopped)
	for {
		if err := g.runOnce(g.ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			g.logger.Warn("winccoa graphql session ended", "err", err)
		}
		select {
		case <-g.ctx.Done():
			return
		case <-time.After(time.Duration(g.cfg.ReconnectDelay) * time.Millisecond):
		}
	}
}

func (g *graphqlConnector) runOnce(ctx context.Context) error {
	g.logger.Info("winccoa graphql connecting",
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

	if err := g.setupSubscriptions(); err != nil {
		return fmt.Errorf("setup subs: %w", err)
	}
	return g.readLoop(ctx)
}

func (g *graphqlConnector) authenticate(ctx context.Context) error {
	if g.cfg.Token != "" {
		g.authToken = g.cfg.Token
		return nil
	}
	if g.cfg.Username == "" && g.cfg.Password == "" {
		g.authToken = ""
		return nil
	}
	mut := fmt.Sprintf(`mutation { login(username: %q, password: %q) { token expiresAt } }`,
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
	g.logger.Info("winccoa graphql authenticated")
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
	conn, _, err := dialer.DialContext(ctx, u.String(), http.Header{})
	if err != nil {
		return err
	}
	g.mu.Lock()
	g.ws = conn
	g.mu.Unlock()
	g.logger.Info("winccoa graphql ws connected", "url", wsURL, "subprotocol", conn.Subprotocol())

	payload := map[string]any{}
	if g.authToken != "" {
		payload["Authorization"] = "Bearer " + g.authToken
	}
	if err := conn.WriteJSON(map[string]any{"type": "connection_init", "payload": payload}); err != nil {
		return fmt.Errorf("send connection_init: %w", err)
	}
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
		case "connection_error", "error":
			return fmt.Errorf("connection_error: %v", msg["payload"])
		}
	}
}

func (g *graphqlConnector) setupSubscriptions() error {
	for _, addr := range g.cfg.Addresses {
		if err := g.sendSubscribe(addr); err != nil {
			g.logger.Warn("winccoa subscription failed", "topic", addr.Topic, "query", addr.Query, "err", err)
		}
	}
	return nil
}

func (g *graphqlConnector) sendSubscribe(addr Address) error {
	id := fmt.Sprintf("%s-dpquery-%d", g.name, atomic.AddInt64(&g.subSeq, 1))
	queryArg, _ := json.Marshal(addr.Query)
	subQuery := fmt.Sprintf(`subscription {
        dpQueryConnectSingle(query: %s, answer: %t) {
            values
            type
            error
        }
    }`, string(queryArg), addr.Answer)
	msg := map[string]any{
		"id":      id,
		"type":    "subscribe",
		"payload": map[string]any{"query": subQuery},
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

func (g *graphqlConnector) readLoop(ctx context.Context) error {
	g.mu.Lock()
	conn := g.ws
	g.mu.Unlock()
	if conn == nil {
		return errors.New("ws closed before read")
	}

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
		case "next", "data":
			g.handleSubscriptionData(msg)
		case "error", "connection_error":
			g.logger.Warn("winccoa graphql subscription error", "payload", msg["payload"])
		case "complete":
			id, _ := msg["id"].(string)
			g.mu.Lock()
			delete(g.subscriptions, id)
			g.mu.Unlock()
		case "ping":
			_ = conn.WriteJSON(map[string]any{"type": "pong"})
		case "ka", "pong":
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
	result, _ := data["dpQueryConnectSingle"].(map[string]any)
	if result == nil {
		return
	}
	if errText, _ := result["error"].(string); strings.TrimSpace(errText) != "" {
		g.logger.Warn("winccoa dpQueryConnectSingle error", "topic", addr.Topic, "query", addr.Query, "err", errText)
		return
	}
	values, _ := result["values"].([]any)
	g.publishRows(addr, values)
}

func (g *graphqlConnector) publishRows(addr Address, values []any) {
	if len(values) < 2 {
		return
	}
	headers, ok := stringSlice(values[0])
	if !ok || len(headers) < 2 {
		return
	}
	for _, item := range values[1:] {
		rowValues, ok := anySlice(item)
		if !ok || len(rowValues) == 0 {
			continue
		}
		datapoint, _ := rowValues[0].(string)
		if datapoint == "" {
			continue
		}
		row := map[string]any{}
		var firstValue any
		for i := 1; i < len(headers) && i < len(rowValues); i++ {
			key := strings.TrimPrefix(headers[i], ":")
			if key == "" {
				continue
			}
			if i == 1 {
				firstValue = rowValues[i]
			}
			row[key] = rowValues[i]
		}
		topic := g.pub.resolveDatapointTopic(addr.Topic, datapoint)
		payload := g.pub.formatDatapointPayload(row, firstValue)
		if err := g.publish(topic, payload, addr.Retained, 0); err != nil {
			g.logger.Warn("winccoa publish failed", "topic", topic, "err", err)
			continue
		}
		g.metrics.inc()
	}
}

func stringSlice(v any) ([]string, bool) {
	items, ok := anySlice(v)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

func anySlice(v any) ([]any, bool) {
	switch x := v.(type) {
	case []any:
		return x, true
	default:
		return nil, false
	}
}
