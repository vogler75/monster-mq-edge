package redfish

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ExtractSensorRecords parses a raw MQTT message payload and maps it into
// one or more NormalizedSensorRecord instances based on GatewayConfig.
func ExtractSensorRecords(payload []byte, topic string, gw *GatewayConfig) ([]NormalizedSensorRecord, error) {
	if len(payload) == 0 {
		return nil, fmt.Errorf("empty payload")
	}

	var root any
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil, fmt.Errorf("invalid json payload: %w", err)
	}

	schemaProps := getMap(gw.JSONSchema, "properties")
	mapping := getStringMap(gw.JSONSchema, "mapping")
	arrayPath := getString(gw.JSONSchema, "arrayPath")

	var items []map[string]any
	rootMap, isMap := root.(map[string]any)

	if arrayPath != "" {
		arrayVal, ok := EvaluateJSONPath(root, arrayPath)
		if ok && arrayVal != nil {
			if list, isList := arrayVal.([]any); isList {
				for _, elem := range list {
					if elemMap, ok := elem.(map[string]any); ok {
						merged := make(map[string]any)
						if isMap {
							for k, v := range rootMap {
								merged[k] = v
							}
						}
						for k, v := range elemMap {
							merged[k] = v
						}
						items = append(items, merged)
					}
				}
			}
		}
	} else if isMap {
		items = append(items, rootMap)
	} else {
		return nil, fmt.Errorf("payload must be a json object or contain arrayPath")
	}

	var results []NormalizedSensorRecord
	now := time.Now().UTC()

	topicPrefix := gw.TopicPrefix
	if topicPrefix == "" {
		topicPrefix = "redfish"
	}
	defaultChassisID := gw.ChassisID
	if defaultChassisID == "" {
		defaultChassisID = "EdgeNode"
	}

	for _, item := range items {
		record, ok := mapSingleRecord(item, topic, topicPrefix, defaultChassisID, gw, schemaProps, mapping, now)
		if ok {
			results = append(results, record)
		}
	}

	return results, nil
}

func mapSingleRecord(
	data map[string]any,
	topic string,
	topicPrefix string,
	defaultChassisID string,
	gw *GatewayConfig,
	props map[string]any,
	mapping map[string]string,
	fallbackTime time.Time,
) (NormalizedSensorRecord, bool) {
	// 1. Reading (required)
	readingVal, ok := extractFieldValue("reading", data, props, mapping)
	if !ok || readingVal == nil {
		return NormalizedSensorRecord{}, false
	}
	readingFloat, ok := toFloat64(readingVal)
	if !ok {
		return NormalizedSensorRecord{}, false
	}

	// 2. Sensor ID
	sensorID := ""
	if v, ok := extractFieldValue("sensorId", data, props, mapping); ok && v != nil {
		sensorID = toString(v)
	}
	if sensorID == "" {
		// Fallback: extract last segment of topic (e.g. sensors/rack1/temp -> temp)
		parts := strings.Split(strings.Trim(topic, "/"), "/")
		if len(parts) > 0 {
			sensorID = parts[len(parts)-1]
		} else {
			sensorID = "sensor"
		}
	}

	// 3. Chassis ID
	chassisID := ""
	if v, ok := extractFieldValue("chassisId", data, props, mapping); ok && v != nil {
		chassisID = toString(v)
	}
	if chassisID == "" {
		chassisID = defaultChassisID
	}

	// 4. Name
	name := ""
	if v, ok := extractFieldValue("name", data, props, mapping); ok && v != nil {
		name = toString(v)
	}
	if name == "" {
		name = sensorID
	}

	// 5. ReadingType
	readingType := ""
	if v, ok := extractFieldValue("readingType", data, props, mapping); ok && v != nil {
		readingType = toString(v)
	}
	if readingType == "" {
		readingType = gw.DefaultReadingType
		if readingType == "" {
			readingType = "Temperature"
		}
	}

	// 6. ReadingUnits
	readingUnits := ""
	if v, ok := extractFieldValue("readingUnits", data, props, mapping); ok && v != nil {
		readingUnits = toString(v)
	}
	if readingUnits == "" {
		readingUnits = gw.DefaultReadingUnits
		if readingUnits == "" {
			if strings.EqualFold(readingType, "Temperature") {
				readingUnits = "Cel"
			} else if strings.EqualFold(readingType, "Voltage") {
				readingUnits = "V"
			} else if strings.EqualFold(readingType, "Power") {
				readingUnits = "W"
			} else if strings.EqualFold(readingType, "Humidity") {
				readingUnits = "%"
			} else if strings.EqualFold(readingType, "Pressure") {
				readingUnits = "Pa"
			}
		}
	}

	// 7. Timestamp
	timestampStr := ""
	if v, ok := extractFieldValue("ts", data, props, mapping); ok && v != nil {
		timestampStr = parseTimestamp(v)
	}
	if timestampStr == "" {
		timestampStr = FormatTimeRFC3339(fallbackTime)
	}

	// 8. Range Min / Max
	var rangeMin, rangeMax *float64
	if v, ok := extractFieldValue("rangeMin", data, props, mapping); ok && v != nil {
		if f, ok := toFloat64(v); ok {
			rangeMin = &f
		}
	}
	if v, ok := extractFieldValue("rangeMax", data, props, mapping); ok && v != nil {
		if f, ok := toFloat64(v); ok {
			rangeMax = &f
		}
	}

	// 9. State & Health
	state := "Enabled"
	if v, ok := extractFieldValue("state", data, props, mapping); ok && v != nil {
		state = toString(v)
	}

	health := ""
	if v, ok := extractFieldValue("health", data, props, mapping); ok && v != nil {
		health = toString(v)
	}
	if health == "" {
		health = CalculateHealth(readingFloat, gw.Thresholds)
	}

	return NormalizedSensorRecord{
		ChassisID:    chassisID,
		SensorID:     sensorID,
		Name:         name,
		Reading:      readingFloat,
		ReadingType:  readingType,
		ReadingUnits: readingUnits,
		RangeMin:     rangeMin,
		RangeMax:     rangeMax,
		State:        state,
		Health:       health,
		Thresholds:   gw.Thresholds,
		SourceTopic:  topic,
		Timestamp:    timestampStr,
		GatewayName:  "",
		TopicPrefix:  topicPrefix,
	}, true
}

func extractFieldValue(fieldName string, data map[string]any, props map[string]any, mapping map[string]string) (any, bool) {
	if mapping != nil {
		if jsonPath, hasPath := mapping[fieldName]; hasPath && jsonPath != "" {
			return EvaluateJSONPath(data, jsonPath)
		}
	}
	// Fallback to direct field name lookup
	val, ok := data[fieldName]
	return val, ok
}

// EvaluateJSONPath extracts a value from a nested JSON structure using dot/bracket paths.
// Examples:
//   "$.temperature" -> data["temperature"]
//   "$.metrics.temp" -> data["metrics"]["temp"]
//   "$.sensors[*]" -> all elements of data["sensors"] slice
//   "$.items[0].value" -> data["items"][0]["value"]
func EvaluateJSONPath(root any, path string) (any, bool) {
	cleanPath := strings.TrimSpace(path)
	cleanPath = strings.TrimPrefix(cleanPath, "$")
	cleanPath = strings.TrimPrefix(cleanPath, ".")

	if cleanPath == "" {
		return root, true
	}

	tokens := tokenizePath(cleanPath)
	current := root

	for i, token := range tokens {
		if current == nil {
			return nil, false
		}

		if token.isArrayWildcard {
			// e.g. [*]
			slice, ok := current.([]any)
			if !ok {
				return nil, false
			}
			if i == len(tokens)-1 {
				return slice, true
			}
			// sub-evaluate each element
			remainingTokens := tokens[i+1:]
			var subResults []any
			for _, elem := range slice {
				res, ok := evaluateTokens(elem, remainingTokens)
				if ok && res != nil {
					subResults = append(subResults, res)
				}
			}
			return subResults, true
		}

		if token.isArrayIndex {
			slice, ok := current.([]any)
			if !ok || token.index < 0 || token.index >= len(slice) {
				return nil, false
			}
			current = slice[token.index]
			continue
		}

		// Object property lookup
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		val, exists := m[token.key]
		if !exists {
			return nil, false
		}
		current = val
	}

	return current, true
}

func evaluateTokens(current any, tokens []pathToken) (any, bool) {
	for i, token := range tokens {
		if current == nil {
			return nil, false
		}
		if token.isArrayWildcard {
			slice, ok := current.([]any)
			if !ok {
				return nil, false
			}
			if i == len(tokens)-1 {
				return slice, true
			}
			remaining := tokens[i+1:]
			var subResults []any
			for _, elem := range slice {
				res, ok := evaluateTokens(elem, remaining)
				if ok && res != nil {
					subResults = append(subResults, res)
				}
			}
			return subResults, true
		}
		if token.isArrayIndex {
			slice, ok := current.([]any)
			if !ok || token.index < 0 || token.index >= len(slice) {
				return nil, false
			}
			current = slice[token.index]
			continue
		}
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		val, exists := m[token.key]
		if !exists {
			return nil, false
		}
		current = val
	}
	return current, true
}

type pathToken struct {
	key             string
	isArrayIndex    bool
	index           int
	isArrayWildcard bool
}

func tokenizePath(path string) []pathToken {
	var tokens []pathToken
	parts := strings.Split(path, ".")

	for _, part := range parts {
		if part == "" {
			continue
		}
		// Handle array notation like "sensors[0]" or "sensors[*]" or "[0]"
		for len(part) > 0 {
			openBracket := strings.Index(part, "[")
			if openBracket == -1 {
				tokens = append(tokens, pathToken{key: part})
				break
			}
			if openBracket > 0 {
				tokens = append(tokens, pathToken{key: part[:openBracket]})
				part = part[openBracket:]
			}
			closeBracket := strings.Index(part, "]")
			if closeBracket == -1 {
				// Malformed, treat remainder as key
				tokens = append(tokens, pathToken{key: part})
				break
			}
			bracketContent := part[1:closeBracket]
			if bracketContent == "*" {
				tokens = append(tokens, pathToken{isArrayWildcard: true})
			} else if idx, err := strconv.Atoi(bracketContent); err == nil {
				tokens = append(tokens, pathToken{isArrayIndex: true, index: idx})
			} else {
				tokens = append(tokens, pathToken{key: bracketContent})
			}
			part = part[closeBracket+1:]
		}
	}

	return tokens
}

func parseTimestamp(v any) string {
	switch val := v.(type) {
	case string:
		if t, err := time.Parse(time.RFC3339, val); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
		if t, err := time.Parse("2006-01-02 15:04:05", val); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
		if ms, err := strconv.ParseInt(val, 10, 64); err == nil {
			return parseEpoch(ms)
		}
		return val
	case float64:
		return parseEpoch(int64(val))
	case int64:
		return parseEpoch(val)
	case int:
		return parseEpoch(int64(val))
	}
	return ""
}

func parseEpoch(n int64) string {
	if n > 1e11 { // Milliseconds
		return time.UnixMilli(n).UTC().Format(time.RFC3339)
	}
	// Seconds
	return time.Unix(n, 0).UTC().Format(time.RFC3339)
}

func toFloat64(v any) (float64, bool) {
	switch val := v.(type) {
	case float64:
		return val, true
	case float32:
		return float64(val), true
	case int:
		return float64(val), true
	case int64:
		return float64(val), true
	case string:
		f, err := strconv.ParseFloat(val, 64)
		return f, err == nil
	case json.Number:
		f, err := val.Float64()
		return f, err == nil
	}
	return 0, false
}

func toString(v any) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case fmt.Stringer:
		return val.String()
	default:
		return fmt.Sprintf("%v", val)
	}
}

func getMap(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if sub, ok := m[key].(map[string]any); ok {
		return sub
	}
	return nil
}

func getStringMap(m map[string]any, key string) map[string]string {
	if m == nil {
		return nil
	}
	sub, ok := m[key].(map[string]any)
	if !ok {
		return nil
	}
	res := make(map[string]string)
	for k, v := range sub {
		if s, ok := v.(string); ok {
			res[k] = s
		}
	}
	return res
}

func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}
