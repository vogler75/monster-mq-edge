package queue

import (
	"testing"
	"time"

	"monstermq.io/edge/internal/stores"
)

func TestMemoryQueueRetriesUntilCommit(t *testing.T) {
	q := NewMemoryQueue(nil, 10, 2, 10*time.Millisecond)
	q.Add(stores.BrokerMessage{TopicName: "a", Payload: []byte("1")})
	q.Add(stores.BrokerMessage{TopicName: "b", Payload: []byte("2")})

	var first []stores.BrokerMessage
	if got := q.PollBlock(func(message stores.BrokerMessage) {
		first = append(first, message)
	}); got != 2 {
		t.Fatalf("PollBlock got %d messages, want 2", got)
	}

	var retry []stores.BrokerMessage
	if got := q.PollBlock(func(message stores.BrokerMessage) {
		retry = append(retry, message)
	}); got != 2 {
		t.Fatalf("retry PollBlock got %d messages, want 2", got)
	}
	if retry[0].TopicName != first[0].TopicName || retry[1].TopicName != first[1].TopicName {
		t.Fatalf("retry block = %#v, want %#v", retry, first)
	}

	q.PollCommit()
	if got := q.PollBlock(func(stores.BrokerMessage) {}); got != 0 {
		t.Fatalf("PollBlock after commit got %d messages, want 0", got)
	}
}

func TestMemoryQueueDropsWhenFull(t *testing.T) {
	q := NewMemoryQueue(nil, 1, 1, 10*time.Millisecond)
	q.Add(stores.BrokerMessage{TopicName: "a"})
	q.Add(stores.BrokerMessage{TopicName: "b"})

	if !q.IsQueueFull() {
		t.Fatal("queue should be marked full after dropping a message")
	}

	var topics []string
	q.PollBlock(func(message stores.BrokerMessage) {
		topics = append(topics, message.TopicName)
	})
	if len(topics) != 1 || topics[0] != "a" {
		t.Fatalf("topics = %v, want [a]", topics)
	}
}
