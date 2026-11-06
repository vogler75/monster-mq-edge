package stores

import (
	"context"
	"strings"
)

// KafkaQueueFactory creates the queue store of one Kafka server stream.
// storeType is the resolved store type (SQLITE, MEMORY, POSTGRES, MONGODB or
// the name of a stored database connection) and suffix the value of
// SanitizeKafkaSuffix(streamName). The broker wiring supplies the factory.
type KafkaQueueFactory func(ctx context.Context, storeType string, suffix string) (KafkaQueueStore, error)

// SanitizeKafkaSuffix turns a stream name into the table/collection suffix:
// lower case, every non-alphanumeric character becomes '_', runs of '_' are
// collapsed, leading and trailing '_' are trimmed, empty becomes "default".
func SanitizeKafkaSuffix(name string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	s := strings.Trim(b.String(), "_")
	if s == "" {
		return "default"
	}
	return s
}

// KafkaQueueNames returns the queue and offsets table (collection) names for
// a suffix; an empty suffix gives the unsuffixed main-broker names.
func KafkaQueueNames(suffix string) (queue, offsets string) {
	if suffix == "" {
		return "kafka_queue", "kafka_offsets"
	}
	suffix = SanitizeKafkaSuffix(suffix)
	return "kafka_queue_" + suffix, "kafka_offsets_" + suffix
}
