package zenoh

import (
	"strings"
)

// MapToZenohKey maps a local MQTT topic to a Zenoh key
func MapToZenohKey(topic, localPrefix, remotePrefix string) string {
	trimmedLocal := strings.Trim(localPrefix, "/")
	trimmedRemote := strings.Trim(remotePrefix, "/")
	topicNormalized := strings.Trim(topic, "/")

	var relativeTopic string
	if trimmedLocal == "" {
		relativeTopic = topicNormalized
	} else {
		if !strings.HasPrefix(topicNormalized, trimmedLocal) {
			return ""
		}
		relativeTopic = strings.TrimPrefix(topicNormalized, trimmedLocal)
		relativeTopic = strings.TrimPrefix(relativeTopic, "/")
	}

	if relativeTopic == "" {
		return ""
	}
	if trimmedRemote == "" {
		return relativeTopic
	}
	return trimmedRemote + "/" + relativeTopic
}

// MapToMqttTopic maps a Zenoh key back to a local MQTT topic
func MapToMqttTopic(zenohKey, localPrefix, remotePrefix string) string {
	trimmedLocal := strings.Trim(localPrefix, "/")
	trimmedRemote := strings.Trim(remotePrefix, "/")
	keyNormalized := strings.Trim(zenohKey, "/")

	var relativeTopic string
	if trimmedRemote == "" {
		relativeTopic = keyNormalized
	} else {
		if !strings.HasPrefix(keyNormalized, trimmedRemote) {
			return ""
		}
		relativeTopic = strings.TrimPrefix(keyNormalized, trimmedRemote)
		relativeTopic = strings.TrimPrefix(relativeTopic, "/")
	}

	if relativeTopic == "" {
		return ""
	}

	// Map wildcards back
	var mqttTopic string
	if strings.ContainsAny(relativeTopic, "*") {
		parts := strings.Split(relativeTopic, "/")
		for i, part := range parts {
			if part == "**" {
				parts[i] = "#"
			} else if part == "*" {
				parts[i] = "+"
			}
		}
		mqttTopic = strings.Join(parts, "/")
	} else {
		mqttTopic = relativeTopic
	}

	if trimmedLocal == "" {
		return mqttTopic
	}
	return trimmedLocal + "/" + mqttTopic
}

// SubscriptionKey maps a local MQTT filter pattern to a Zenoh key expression
func SubscriptionKey(localFilter, localPrefix, remotePrefix string) string {
	trimmedLocal := strings.Trim(localPrefix, "/")
	trimmedRemote := strings.Trim(remotePrefix, "/")
	filterNormalized := strings.Trim(localFilter, "/")

	var relativeFilter string
	if trimmedLocal == "" {
		relativeFilter = filterNormalized
	} else {
		if filterNormalized == "#" || filterNormalized == "+" {
			relativeFilter = "#"
		} else {
			if !strings.HasPrefix(filterNormalized, trimmedLocal) {
				return ""
			}
			relativeFilter = strings.TrimPrefix(filterNormalized, trimmedLocal)
			relativeFilter = strings.TrimPrefix(relativeFilter, "/")
		}
	}

	if relativeFilter == "" {
		return ""
	}

	return SubscriptionKeyWithPrefix(trimmedRemote, relativeFilter)
}

// SubscriptionKeyWithPrefix maps relative filter to Zenoh key expression
func SubscriptionKeyWithPrefix(prefix, mqttFilter string) string {
	if mqttFilter == "" {
		return ""
	}
	parts := strings.Split(mqttFilter, "/")
	for i, part := range parts {
		if part == "#" {
			parts[i] = "**"
		} else if part == "+" {
			parts[i] = "*"
		}
	}
	zenohFilter := strings.Join(parts, "/")
	trimmedPrefix := strings.Trim(prefix, "/")
	if trimmedPrefix == "" {
		return zenohFilter
	}
	return trimmedPrefix + "/" + zenohFilter
}

// MinimalFilters removes redundant filters
func MinimalFilters(filters []string) []string {
	var distinct []string
	seen := make(map[string]bool)
	for _, f := range filters {
		if !seen[f] {
			seen[f] = true
			distinct = append(distinct, f)
		}
	}

	var result []string
	for _, candidate := range distinct {
		redundant := false
		for _, other := range distinct {
			if other != candidate && includes(other, candidate) {
				redundant = true
				break
			}
		}
		if !redundant {
			result = append(result, candidate)
		}
	}
	return result
}

func includes(covering, candidate string) bool {
	a := strings.Split(covering, "/")
	b := strings.Split(candidate, "/")
	index := 0
	for index < len(a) {
		coveringLevel := a[index]
		if coveringLevel == "#" {
			return true
		}
		if index >= len(b) {
			return false
		}
		candidateLevel := b[index]
		if candidateLevel == "#" {
			return false
		}
		if coveringLevel != "+" && coveringLevel != candidateLevel {
			return false
		}
		index++
	}
	return index == len(b)
}
