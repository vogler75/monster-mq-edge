package winccua

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// LocalPublisher is the function the bridge calls to inject an MQTT message
// into the local broker. mochi-mqtt's *Server.Publish satisfies this shape.
type LocalPublisher func(topic string, payload []byte, retain bool, qos byte) error

// publisher holds a topic-transformation cache shared between transports for
// one device. Tag → MQTT topic resolution is done once per tag and reused.
type publisher struct {
	namespace       string
	transformConfig TransformConfig
	messageFormat   string

	mu        sync.Mutex
	topicMap  map[string]string
}

func newPublisher(namespace string, tc TransformConfig, format string) *publisher {
	return &publisher{
		namespace:       namespace,
		transformConfig: tc,
		messageFormat:   format,
		topicMap:        make(map[string]string),
	}
}

// resolveTagTopic returns "<namespace>/<addressTopic>/<transformed-tag-name>".
// Cached per tagName.
func (p *publisher) resolveTagTopic(addressTopic, tagName string) string {
	p.mu.Lock()
	if cached, ok := p.topicMap[tagName]; ok {
		p.mu.Unlock()
		return joinTopic(p.namespace, addressTopic, cached)
	}
	transformed := p.transformConfig.TransformTagName(tagName)
	p.topicMap[tagName] = transformed
	p.mu.Unlock()
	return joinTopic(p.namespace, addressTopic, transformed)
}

func (p *publisher) resolveAlarmTopic(addressTopic, alarmName string) string {
	return joinTopic(p.namespace, addressTopic, alarmName)
}

// formatTagPayload builds the published MQTT payload for one tag value.
//
//   FORMAT_JSON_ISO  → {"value":<v>,"time":"<ISO>",[ "quality":<q> ]}
//   FORMAT_JSON_MS   → {"value":<v>,"time":<ms>,[ "quality":<q> ]}
//   FORMAT_RAW_VALUE → string(value)
func (p *publisher) formatTagPayload(value any, timestamp string, quality map[string]any) []byte {
	switch p.messageFormat {
	case FormatJSONMS:
		ms := time.Now().UnixMilli()
		if timestamp != "" {
			if t, err := parseTimestamp(timestamp); err == nil {
				ms = t.UnixMilli()
			}
		}
		obj := map[string]any{"value": value, "time": ms}
		if quality != nil {
			obj["quality"] = quality
		}
		out, _ := json.Marshal(obj)
		return out
	case FormatRawValue:
		return []byte(fmt.Sprintf("%v", value))
	default:
		// FORMAT_JSON_ISO and any unknown value
		obj := map[string]any{"value": value}
		if timestamp != "" {
			obj["time"] = timestamp
		}
		if quality != nil {
			obj["quality"] = quality
		}
		out, _ := json.Marshal(obj)
		return out
	}
}

// resolveAlarmName extracts a stable identifier from an alarm payload, trying
// the GraphQL "name", the OpenPipe "Name", and finally "path"/"Path".
func resolveAlarmName(alarm map[string]any) string {
	for _, k := range []string{"name", "Name", "path", "Path"} {
		if v, ok := alarm[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return "unknown"
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

// parseTimestamp parses an ISO 8601 timestamp. The Java broker uses
// Instant.parse, which accepts RFC3339 with optional fractional seconds and Z.
func parseTimestamp(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}
