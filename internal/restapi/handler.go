package restapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"monstermq.io/edge/internal/archive"
	"monstermq.io/edge/internal/auth"
	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/pubsub"
	"monstermq.io/edge/internal/stores"
	"monstermq.io/edge/internal/topic"
)

const maxBody = 4 << 20
const maxMessages = 10000
const maxSSEClients = 128

type Handler struct {
	cfg       *config.Config
	auth      *auth.Cache
	retained  stores.MessageStore
	archives  *archive.Manager
	bus       *pubsub.Bus
	publish   func(string, []byte, bool, byte) error
	activeSSE atomic.Int32
}

func New(cfg *config.Config, cache *auth.Cache, retained stores.MessageStore, archives *archive.Manager, bus *pubsub.Bus, publish func(string, []byte, bool, byte) error) *Handler {
	return &Handler{cfg: cfg, auth: cache, retained: retained, archives: archives, bus: bus, publish: publish}
}

func (h *Handler) Router() http.Handler {
	r := chi.NewRouter()
	r.Post("/login", h.login)
	r.Group(func(r chi.Router) {
		r.Use(h.authenticate)
		r.Get("/openapi.yaml", h.openapi)
		r.Get("/docs", h.docs)
		r.Post("/write", h.write)
		r.Post("/write/influx", h.writeInflux)
		r.Get("/subscribe", h.subscribe)
		r.Post("/topics/*", h.publishRaw)
		r.Put("/topics/*", h.publishInline)
		r.Get("/topics/*", h.read)
	})
	return r
}

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, message string) {
	respond(w, status, map[string]any{"error": message})
}

func (h *Handler) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.cfg.UserManagement.Enabled {
			next.ServeHTTP(w, r)
			return
		}
		if h.cfg.UserManagement.AllowAnonymousLocalhost && auth.IsLocalhostRequest(r) && r.Header.Get("Authorization") == "" {
			ctx := auth.WithPrincipal(r.Context(), auth.LocalhostUser)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		ctx, err := auth.AuthenticateHeader(r.Context(), h.auth, r.Header.Get("Authorization"))
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Basic realm="MonsterMQ REST API"`)
			fail(w, 401, err.Error())
			return
		}
		if _, ok := auth.Principal(ctx); !ok && !h.cfg.UserManagement.AnonymousEnabled {
			w.Header().Set("WWW-Authenticate", `Basic realm="MonsterMQ REST API"`)
			fail(w, 401, "Authentication required")
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.UserManagement.Enabled {
		respond(w, 200, map[string]any{"success": true, "token": nil, "message": "Authentication disabled", "username": "anonymous"})
		return
	}
	if h.cfg.UserManagement.AllowAnonymousLocalhost && auth.IsLocalhostRequest(r) {
		if p, ok := auth.Principal(r.Context()); ok && p.Username == "localhost" {
			respond(w, 200, map[string]any{"success": true, "token": nil, "message": "Authentication bypassed for localhost", "username": "localhost"})
			return
		}
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeBody(w, r, &input); err != nil {
		fail(w, bodyStatus(err), err.Error())
		return
	}
	if input.Username == "" || input.Password == "" {
		fail(w, 400, "Username and password are required")
		return
	}
	user, ok := h.auth.Authenticate(r.Context(), input.Username, input.Password)
	if !ok {
		fail(w, 401, "Invalid username or password")
		return
	}
	token, err := h.auth.CreateSession(user.Username)
	if err != nil {
		fail(w, 500, "Internal server error")
		return
	}
	respond(w, 200, map[string]any{"success": true, "token": token, "username": user.Username})
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return errors.New("only one JSON value is allowed")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
func bodyStatus(err error) int {
	var max *http.MaxBytesError
	if errors.As(err, &max) {
		return 413
	}
	return 400
}

func (h *Handler) allowed(r *http.Request, topic string, write bool) bool {
	if !h.cfg.UserManagement.Enabled {
		return true
	}
	user, ok := auth.Principal(r.Context())
	if !ok {
		return h.auth.Allow("", topic, write)
	}
	if h.cfg.UserManagement.AllowAnonymousLocalhost && user.Username == "localhost" && auth.IsLocalhostRequest(r) {
		return true
	}
	return h.auth.Allow(user.Username, topic, write)
}

func topicPath(r *http.Request) (string, error) {
	raw := strings.TrimPrefix(r.URL.EscapedPath(), "/api/v1/topics/")
	if raw == "" || raw == r.URL.EscapedPath() {
		return "", errors.New("Topic path is required")
	}
	topic, err := url.PathUnescape(raw)
	if err != nil || !utf8.ValidString(topic) {
		return "", errors.New("Invalid topic path encoding")
	}
	if !validFilter(topic) {
		return "", errors.New("Invalid topic filter")
	}
	return topic, nil
}
func validFilter(topic string) bool {
	if topic == "" || strings.ContainsRune(topic, 0) {
		return false
	}
	parts := strings.Split(topic, "/")
	for i, part := range parts {
		if strings.Contains(part, "#") && (part != "#" || i != len(parts)-1) {
			return false
		}
		if strings.Contains(part, "+") && part != "+" {
			return false
		}
	}
	return true
}
func validPublish(topic string) bool { return validFilter(topic) && !strings.ContainsAny(topic, "+#") }

func mqttOptions(r *http.Request) (byte, bool) {
	qos, err := strconv.Atoi(r.URL.Query().Get("qos"))
	if err != nil {
		qos = 0
	}
	if qos < 0 {
		qos = 0
	}
	if qos > 2 {
		qos = 2
	}
	return byte(qos), strings.EqualFold(r.URL.Query().Get("retain"), "true")
}

func (h *Handler) send(r *http.Request, topic string, payload []byte, qos byte, retain bool) (int, string) {
	if !validPublish(topic) {
		return 400, "Invalid publish topic"
	}
	if !h.allowed(r, topic, true) {
		return 403, "Publish not allowed on topic: " + topic
	}
	if len(payload) > h.cfg.MaxMessageSize && h.cfg.MaxMessageSize > 0 {
		return 413, "Payload exceeds broker message size"
	}
	if err := h.publish(topic, payload, retain, qos); err != nil {
		return 500, err.Error()
	}
	return 200, ""
}

func payloadValue(payload []byte) any {
	if !utf8.Valid(payload) {
		return base64.RawURLEncoding.EncodeToString(payload)
	}
	var value any
	if json.Unmarshal(payload, &value) == nil {
		return value
	}
	return string(payload)
}
func liveMessage(msg stores.BrokerMessage) map[string]any {
	return map[string]any{"topic": msg.TopicName, "value": payloadValue(msg.Payload), "timestamp": msg.Time.UTC().Format(time.RFC3339Nano), "qos": msg.QoS, "retain": msg.IsRetain}
}
func historyMessage(msg stores.ArchivedMessage) map[string]any {
	row := map[string]any{"topic": msg.Topic, "timestamp": msg.Timestamp.UnixMilli(), "qos": msg.QoS, "client_id": msg.ClientID}
	if len(msg.Payload) == 0 {
		return row
	}
	if utf8.Valid(msg.Payload) {
		text := string(msg.Payload)
		if strings.HasPrefix(strings.TrimSpace(text), "{") || strings.HasPrefix(strings.TrimSpace(text), "[") {
			row["payload"] = payloadValue(msg.Payload)
		} else {
			row["payload"] = text
		}
	} else {
		row["payload_base64"] = base64Payload(msg.Payload)
	}
	return row
}

func (h *Handler) read(w http.ResponseWriter, r *http.Request) {
	topic, err := topicPath(r)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	if !h.allowed(r, topic, false) {
		fail(w, 403, "Subscribe/read not allowed on topic: "+topic)
		return
	}
	q := r.URL.Query()
	_, raw := q["raw"]
	if raw && !validPublish(topic) {
		fail(w, 400, "Raw reads require one exact topic")
		return
	}
	if _, retained := q["retained"]; retained {
		if raw {
			h.readRaw(w, r, topic, h.retained, "Retained store is not available")
		} else {
			h.readMessages(w, r, topic, h.retained, "Retained store is not available")
		}
		return
	}
	name := q.Get("group")
	if name == "" {
		name = "Default"
	}
	group := h.archives.Get(name)
	if group == nil {
		fail(w, 404, "Archive group '"+name+"' not found")
		return
	}
	_, hasStart := q["start"]
	_, hasEnd := q["end"]
	if hasStart || hasEnd {
		if raw {
			fail(w, 400, "Raw reads do not support history; select one current or retained topic")
			return
		}
		h.readHistory(w, r, topic, name)
		return
	}
	if raw {
		h.readRaw(w, r, topic, group.LastValue(), "Archive group '"+name+"' has no last value store")
	} else {
		h.readMessages(w, r, topic, group.LastValue(), "Archive group '"+name+"' has no last value store")
	}
}

func (h *Handler) readRaw(w http.ResponseWriter, r *http.Request, topic string, store stores.MessageStore, unavailable string) {
	if store == nil {
		fail(w, 404, unavailable)
		return
	}
	var found *stores.BrokerMessage
	err := store.FindMatchingMessages(r.Context(), topic, func(msg stores.BrokerMessage) bool {
		if msg.TopicName == topic && h.allowed(r, msg.TopicName, false) {
			found = &msg
			return false
		}
		return true
	})
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if found == nil {
		fail(w, 404, "No value found for topic: "+topic)
		return
	}
	contentType := http.DetectContentType(found.Payload)
	switch contentType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
	default:
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(found.Payload)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(200)
	_, _ = w.Write(found.Payload)
}

func (h *Handler) readMessages(w http.ResponseWriter, r *http.Request, topic string, store stores.MessageStore, unavailable string) {
	if store == nil {
		fail(w, 404, unavailable)
		return
	}
	messages := make([]map[string]any, 0)
	err := store.FindMatchingMessages(r.Context(), topic, func(msg stores.BrokerMessage) bool {
		if h.allowed(r, msg.TopicName, false) {
			messages = append(messages, liveMessage(msg))
		}
		return len(messages) < maxMessages
	})
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	respond(w, 200, map[string]any{"messages": messages})
}
func parseTime(value string) (*time.Time, error) {
	if value == "" {
		return nil, errors.New("empty time")
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
func (h *Handler) readHistory(w http.ResponseWriter, r *http.Request, topic, name string) {
	archiveStore := h.archives.Get(name).Archive()
	if archiveStore == nil {
		fail(w, 404, "Archive group '"+name+"' has no queryable archive store")
		return
	}
	q := r.URL.Query()
	var start, end *time.Time
	for _, bound := range []struct {
		name string
		dst  **time.Time
	}{{"start", &start}, {"end", &end}} {
		if values, ok := q[bound.name]; ok {
			t, err := parseTime(values[0])
			if err != nil {
				fail(w, 400, "Invalid '"+bound.name+"' parameter: "+err.Error())
				return
			}
			*bound.dst = t
		}
	}
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil {
		limit = 1000
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 100000 {
		limit = 100000
	}
	rows, err := archiveStore.GetHistory(r.Context(), topic, start, end, limit)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	messages := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if topicMatch(topic, row.Topic) && h.allowed(r, row.Topic, false) {
			messages = append(messages, historyMessage(row))
		}
	}
	respond(w, 200, map[string]any{"messages": messages})
}

func (h *Handler) checkRead(r *http.Request, topic string) bool { return h.allowed(r, topic, false) }
func base64Payload(payload []byte) string                       { return base64.StdEncoding.EncodeToString(payload) }
func topicMatch(filter, name string) bool                       { return topic.MatchFilter(filter, name) }
