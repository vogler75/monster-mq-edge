package restapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

func (h *Handler) subscribe(w http.ResponseWriter, r *http.Request) {
	filters := r.URL.Query()["topic"]
	if len(filters) == 0 {
		fail(w, 400, "At least one 'topic' query parameter is required")
		return
	}
	for _, filter := range filters {
		if !validFilter(filter) {
			fail(w, 400, "Invalid topic filter")
			return
		}
		if !h.allowed(r, filter, false) {
			fail(w, 403, "Subscribe not allowed on topic: "+filter)
			return
		}
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, "Streaming is unavailable")
		return
	}
	for {
		active := h.activeSSE.Load()
		if active >= maxSSEClients {
			fail(w, http.StatusServiceUnavailable, "Too many SSE clients")
			return
		}
		if h.activeSSE.CompareAndSwap(active, active+1) {
			break
		}
	}
	defer h.activeSSE.Add(-1)
	id, messages, overflow := h.bus.SubscribeWithOverflow(filters, 64)
	defer h.bus.Unsubscribe(id)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-overflow:
			return
		case <-ticker.C:
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case msg, open := <-messages:
			if !open {
				return
			}
			select {
			case <-overflow:
				return
			default:
			}
			if !h.checkRead(r, msg.TopicName) {
				continue
			}
			data, _ := json.Marshal(map[string]any{"topic": msg.TopicName, "value": payloadValue(msg.Payload), "timestamp": msg.Time.UTC().Format(time.RFC3339Nano)})
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
