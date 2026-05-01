package queue

import (
	"testing"
	"time"

	"monstermq.io/edge/internal/stores"
)

func TestDiskQueuePersistsCommittedMessages(t *testing.T) {
	dir := t.TempDir()
	q, err := NewDiskQueue("mqtt-bridge", "test", nil, 10, 10, 10*time.Millisecond, dir)
	if err != nil {
		t.Fatal(err)
	}
	q.Add(stores.BrokerMessage{TopicName: "a", Payload: []byte("1")})
	q.Add(stores.BrokerMessage{TopicName: "b", Payload: []byte("2")})
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}

	q, err = NewDiskQueue("mqtt-bridge", "test", nil, 10, 10, 10*time.Millisecond, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()

	var topics []string
	if got := q.PollBlock(func(message stores.BrokerMessage) {
		topics = append(topics, message.TopicName)
	}); got != 2 {
		t.Fatalf("PollBlock got %d messages, want 2", got)
	}
	if topics[0] != "a" || topics[1] != "b" {
		t.Fatalf("topics = %v, want [a b]", topics)
	}
	q.PollCommit()
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}

	q, err = NewDiskQueue("mqtt-bridge", "test", nil, 10, 10, 10*time.Millisecond, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if got := q.PollBlock(func(stores.BrokerMessage) {}); got != 0 {
		t.Fatalf("PollBlock after commit got %d messages, want 0", got)
	}
}

func TestDiskQueueRetriesUncommittedBlockAfterReopen(t *testing.T) {
	dir := t.TempDir()
	q, err := NewDiskQueue("mqtt-bridge", "test", nil, 10, 10, 10*time.Millisecond, dir)
	if err != nil {
		t.Fatal(err)
	}
	q.Add(stores.BrokerMessage{TopicName: "a", Payload: []byte("1")})
	if got := q.PollBlock(func(stores.BrokerMessage) {}); got != 1 {
		t.Fatalf("PollBlock got %d messages, want 1", got)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}

	q, err = NewDiskQueue("mqtt-bridge", "test", nil, 10, 10, 10*time.Millisecond, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()

	var topic string
	if got := q.PollBlock(func(message stores.BrokerMessage) {
		topic = message.TopicName
	}); got != 1 {
		t.Fatalf("PollBlock after reopen got %d messages, want 1", got)
	}
	if topic != "a" {
		t.Fatalf("topic = %q, want a", topic)
	}
}
