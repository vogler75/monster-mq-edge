package winccoa

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

type publisher struct {
	namespace       string
	transformConfig TransformConfig
	messageFormat   string

	mu       sync.Mutex
	topicMap map[string]string
}

func newPublisher(namespace string, tc TransformConfig, format string) *publisher {
	return &publisher{
		namespace:       namespace,
		transformConfig: tc,
		messageFormat:   format,
		topicMap:        map[string]string{},
	}
}

func (p *publisher) resolveDatapointTopic(addressTopic, datapoint string) string {
	p.mu.Lock()
	if cached, ok := p.topicMap[datapoint]; ok {
		p.mu.Unlock()
		return joinTopic(p.namespace, addressTopic, cached)
	}
	transformed := p.transformConfig.TransformDatapointName(datapoint)
	p.topicMap[datapoint] = transformed
	p.mu.Unlock()
	return joinTopic(p.namespace, addressTopic, transformed)
}

func (p *publisher) formatDatapointPayload(row map[string]any, firstValue any) []byte {
	switch p.messageFormat {
	case FormatJSONMS:
		out := make(map[string]any, len(row))
		for k, v := range row {
			out[k] = convertTimeValueToMillis(k, v)
		}
		raw, _ := json.Marshal(out)
		return raw
	case FormatRawValue:
		return formatRawValue(firstValue)
	default:
		raw, _ := json.Marshal(row)
		return raw
	}
}

func formatRawValue(v any) []byte {
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		return x
	case string:
		return []byte(x)
	case map[string]any:
		if b, ok := bufferObjectBytes(x); ok {
			return b
		}
	}
	return []byte(fmt.Sprintf("%v", v))
}

func bufferObjectBytes(v map[string]any) ([]byte, bool) {
	typ, _ := v["type"].(string)
	if typ != "Buffer" {
		return nil, false
	}
	data, ok := v["data"].([]any)
	if !ok {
		return nil, false
	}
	out := make([]byte, 0, len(data))
	for _, item := range data {
		switch n := item.(type) {
		case float64:
			out = append(out, byte(n))
		case int:
			out = append(out, byte(n))
		case json.Number:
			i, err := n.Int64()
			if err != nil {
				return nil, false
			}
			out = append(out, byte(i))
		default:
			return nil, false
		}
	}
	return out, true
}

func convertTimeValueToMillis(key string, value any) any {
	lower := strings.ToLower(key)
	if !strings.Contains(lower, "time") && !strings.Contains(lower, "stime") {
		return value
	}
	s, ok := value.(string)
	if !ok || s == "" {
		return value
	}
	t, err := parseTimestamp(s)
	if err != nil {
		return value
	}
	return t.UnixMilli()
}

func joinTopic(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.Trim(p, "/")
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "/")
}

func parseTimestamp(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}
