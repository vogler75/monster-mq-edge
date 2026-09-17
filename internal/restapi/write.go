package restapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (h *Handler) publishRaw(w http.ResponseWriter, r *http.Request) {
	topic, err := topicPath(r)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		fail(w, bodyStatus(err), err.Error())
		return
	}
	qos, retain := mqttOptions(r)
	if status, reason := h.send(r, topic, payload, qos, retain); status != 200 {
		fail(w, status, reason)
		return
	}
	respond(w, 200, map[string]any{"success": true, "topic": topic})
}

func (h *Handler) publishInline(w http.ResponseWriter, r *http.Request) {
	topic, err := topicPath(r)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	values, ok := r.URL.Query()["payload"]
	if !ok {
		fail(w, 400, "Query parameter 'payload' is required")
		return
	}
	qos, retain := mqttOptions(r)
	if status, reason := h.send(r, topic, []byte(values[0]), qos, retain); status != 200 {
		fail(w, status, reason)
		return
	}
	respond(w, 200, map[string]any{"success": true, "topic": topic})
}

func valueText(value any) (string, bool) {
	if value == nil {
		return "", false
	}
	switch v := value.(type) {
	case string:
		return v, true
	case json.Number:
		return v.String(), true
	case bool:
		return strconv.FormatBool(v), true
	default:
		data, err := json.Marshal(v)
		return string(data), err == nil
	}
}
func numberOption(value any, fallback byte) byte {
	text, ok := valueText(value)
	if !ok {
		return fallback
	}
	n, err := strconv.Atoi(text)
	if err != nil {
		return fallback
	}
	if n < 0 {
		n = 0
	}
	if n > 2 {
		n = 2
	}
	return byte(n)
}
func boolOption(value any, fallback bool) bool {
	b, ok := value.(bool)
	if !ok {
		return fallback
	}
	return b
}

func (h *Handler) write(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []any `json:"messages"`
		Records  []any `json:"records"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		fail(w, bodyStatus(err), err.Error())
		return
	}
	if len(body.Messages)+len(body.Records) == 0 {
		fail(w, 400, "'messages' or 'records' array is required and must not be empty")
		return
	}
	count := 0
	errorsOut := make([]map[string]any, 0)
	add := func(index int, topic string, value any, qos byte, retain bool) {
		payload, ok := valueText(value)
		if topic == "" || !ok {
			errorsOut = append(errorsOut, map[string]any{"index": index, "error": "Missing topic or value"})
			return
		}
		if status, reason := h.send(r, topic, []byte(payload), qos, retain); status != 200 {
			errorsOut = append(errorsOut, map[string]any{"index": index, "error": reason})
			return
		}
		count++
	}
	for i, item := range body.Messages {
		message, ok := item.(map[string]any)
		if !ok {
			errorsOut = append(errorsOut, map[string]any{"index": i, "error": "Message must be an object"})
			continue
		}
		topic, _ := message["topic"].(string)
		add(i, topic, message["value"], numberOption(message["qos"], 0), boolOption(message["retain"], false))
	}
	for i, item := range body.Records {
		record, ok := item.([]any)
		if !ok {
			errorsOut = append(errorsOut, map[string]any{"index": i, "error": "Record must be an array"})
			continue
		}
		if len(record) < 2 {
			errorsOut = append(errorsOut, map[string]any{"index": i, "error": "Missing topic or value in record"})
			continue
		}
		topic, _ := record[0].(string)
		qos := byte(0)
		retain := false
		if len(record) > 2 {
			qos = numberOption(record[2], 0)
		}
		if len(record) > 3 {
			retain = boolOption(record[3], false)
		}
		add(i, topic, record[1], qos, retain)
	}
	result := map[string]any{"success": len(errorsOut) == 0, "count": count}
	if len(errorsOut) > 0 {
		result["errors"] = errorsOut
	}
	respond(w, 200, result)
}

// splitOutsideQuotes implements the main broker's simple line-protocol delimiter rule.
func splitOutsideQuotes(s string, sep byte) []string {
	var out []string
	quoted := false
	escaped := false
	start := 0
	for i := range s {
		c := s[i]
		if c == '\\' && !escaped {
			escaped = true
			continue
		}
		if c == '"' && !escaped {
			quoted = !quoted
		}
		if c == sep && !quoted {
			out = append(out, s[start:i])
			start = i + 1
		}
		escaped = false
	}
	return append(out, s[start:])
}
func influxValue(s string) any {
	if s == "true" || s == "false" {
		return s == "true"
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return n
	}
	return s
}
func influxTimestamp(s string) any {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return s
	}
	var t time.Time
	switch {
	case len(s) <= 10:
		t = time.Unix(n, 0)
	case len(s) <= 13:
		t = time.UnixMilli(n)
	case len(s) <= 16:
		t = time.UnixMicro(n)
	default:
		t = time.Unix(0, n)
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func (h *Handler) writeInflux(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		fail(w, bodyStatus(err), err.Error())
		return
	}
	if strings.TrimSpace(string(body)) == "" {
		fail(w, 400, "Empty body")
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "simple"
	}
	if format != "simple" && format != "json" {
		fail(w, 400, "Invalid format")
		return
	}
	base := strings.TrimSuffix(r.URL.Query().Get("base"), "/")
	if base != "" {
		base += "/"
	}
	qos, retain := mqttOptions(r)
	count := 0
	errorsOut := make([]map[string]any, 0)
	lineIndex := 0
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		index := lineIndex
		lineIndex++
		parts := splitOutsideQuotes(line, ' ')
		if len(parts) < 2 || parts[0] == "" {
			errorsOut = append(errorsOut, map[string]any{"index": index, "error": "Invalid line protocol format"})
			continue
		}
		mt := strings.Split(parts[0], ",")
		topic := base + mt[0]
		for _, tag := range mt[1:] {
			_, val, ok := strings.Cut(tag, "=")
			if ok && val != "" {
				topic += "/" + val
			}
		}
		fields := make(map[string]any)
		for _, field := range splitOutsideQuotes(parts[1], ',') {
			key, value, ok := strings.Cut(field, "=")
			if !ok || key == "" {
				continue
			}
			quoted := strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"")
			value = strings.Trim(value, "\"")
			if !quoted && strings.HasSuffix(value, "i") {
				if _, err := strconv.ParseInt(strings.TrimSuffix(value, "i"), 10, 64); err == nil {
					value = strings.TrimSuffix(value, "i")
				}
			}
			fields[key] = influxValue(value)
		}
		if len(fields) == 0 {
			errorsOut = append(errorsOut, map[string]any{"index": index, "error": "No valid fields found"})
			continue
		}
		publishOne := func(t string, payload []byte) {
			if status, reason := h.send(r, t, payload, qos, retain); status != 200 {
				errorsOut = append(errorsOut, map[string]any{"index": index, "error": reason})
				return
			}
			count++
		}
		if format == "json" {
			if len(parts) > 2 && parts[2] != "" {
				if n, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
					fields["timestamp_ns"] = n
				}
				fields["timestamp"] = influxTimestamp(parts[2])
			}
			payload, err := json.Marshal(fields)
			if err != nil {
				errorsOut = append(errorsOut, map[string]any{"index": index, "error": err.Error()})
				continue
			}
			publishOne(topic, payload)
		} else {
			for _, field := range splitOutsideQuotes(parts[1], ',') {
				key, _, ok := strings.Cut(field, "=")
				if !ok {
					continue
				}
				value, present := fields[key]
				if !present {
					continue
				}
				publishOne(topic+"/"+key, []byte(strings.TrimSpace(toText(value))))
			}
		}
	}
	if len(errorsOut) == 0 {
		w.WriteHeader(204)
		return
	}
	respond(w, 200, map[string]any{"success": false, "count": count, "errors": errorsOut})
}
func toText(v any) string {
	s, ok := valueText(v)
	if !ok {
		return ""
	}
	return s
}
