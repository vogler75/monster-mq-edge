package kafka

import (
	"regexp"
	"strings"
	"sync"
	"time"

	"monstermq.io/edge/internal/stores"
	"monstermq.io/edge/internal/topic"
)

// stream is one configured Kafka topic: its MQTT filter, retention, ACL flag,
// queue store and the signal that wakes long-polling consumers.
type stream struct {
	cfg       StreamMapping
	suffix    string
	store     stores.KafkaQueueStore
	retention time.Duration

	mu     sync.Mutex
	notify chan struct{}
}

// wait returns a channel that is closed on the next signal. Take it BEFORE
// reading the store, so an append in between is never missed.
func (s *stream) wait() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.notify == nil {
		s.notify = make(chan struct{})
	}
	return s.notify
}

// signal wakes every consumer waiting on this stream (keyed by stream, i.e. by
// the Kafka topic the consumers fetch, not by the MQTT topic of the record).
func (s *stream) signal() {
	s.mu.Lock()
	if s.notify != nil {
		close(s.notify)
		s.notify = nil
	}
	s.mu.Unlock()
}

var (
	nonAlnum    = regexp.MustCompile(`[^a-z0-9]`)
	underscores = regexp.MustCompile(`_+`)
)

// TableSuffix is the table-name suffix of a stream: lowercase, non-alphanumeric
// characters become "_", runs collapse, ends are trimmed, empty becomes
// "default" (main broker's sanitizeTableNameSuffix).
func TableSuffix(name string) string {
	s := nonAlnum.ReplaceAllString(strings.ToLower(name), "_")
	s = underscores.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if s == "" {
		return "default"
	}
	return s
}

// byName returns the stream whose name is the Kafka topic.
func (s *Server) byName(kafkaTopic string) *stream {
	return s.streamsByName[kafkaTopic]
}

// routeFetch resolves the stream behind a Kafka topic (fetch, offsets,
// commits): the stream of that name, else the first stream (configuration
// order) whose filter matches the topic.
func (s *Server) routeFetch(kafkaTopic string) *stream {
	if st := s.byName(kafkaTopic); st != nil {
		return st
	}
	return s.firstMatching(kafkaTopic)
}

// routeEnqueue picks the stream an MQTT message belongs to: the first stream
// (configuration order, so the choice is deterministic) whose filter matches,
// else the stream named like the MQTT topic, else nil (message not stored).
func (s *Server) routeEnqueue(mqttTopic string) *stream {
	if st := s.firstMatching(mqttTopic); st != nil {
		return st
	}
	return s.byName(mqttTopic)
}

func (s *Server) firstMatching(t string) *stream {
	for _, st := range s.streams {
		if topic.MatchFilter(st.cfg.TopicFilter, t) {
			return st
		}
	}
	return nil
}

// groupByStream splits messages by routeEnqueue, preserving order per stream.
func (s *Server) groupByStream(msgs []stores.BrokerMessage) map[*stream][]stores.BrokerMessage {
	out := map[*stream][]stores.BrokerMessage{}
	for _, m := range msgs {
		if st := s.routeEnqueue(m.TopicName); st != nil {
			out[st] = append(out[st], m)
		}
	}
	return out
}

// isWriteAllowed is the stream rule for Produce: when a stream is named like the
// Kafka topic, it must allow writes and its filter must match the record's MQTT
// topic; a Kafka topic without a stream accepts any MQTT topic.
func (s *Server) isWriteAllowed(kafkaTopic, mqttTopic string) bool {
	st := s.byName(kafkaTopic)
	if st == nil {
		return true
	}
	return st.cfg.AllowWrite && topic.MatchFilter(st.cfg.TopicFilter, mqttTopic)
}
