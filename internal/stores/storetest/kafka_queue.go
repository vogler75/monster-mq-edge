package storetest

import (
	"context"
	"testing"
	"time"

	"monstermq.io/edge/internal/stores"
)

// CheckKafkaQueue exercises a stores.KafkaQueueStore on an empty queue
// (EnsureTable already called).
func CheckKafkaQueue(t *testing.T, s stores.KafkaQueueStore) {
	t.Helper()
	ctx := context.Background()

	if off, err := s.GetLatestOffset(ctx, "t"); err != nil || off != 0 {
		t.Fatalf("empty latest = %d, %v", off, err)
	}
	if off, err := s.GetEarliestOffset(ctx, "t"); err != nil || off != 0 {
		t.Fatalf("empty earliest = %d, %v", off, err)
	}
	if err := s.Enqueue(ctx, nil); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	old := now.Add(-48 * time.Hour)
	msgs := []stores.BrokerMessage{
		{MessageUUID: "u1", TopicName: "a/b", Payload: []byte("one"), QoS: 1, ClientID: "c1", Time: old},
		{MessageUUID: "u2", TopicName: "a/c", Payload: []byte{0, 1, 2}, QoS: 0, ClientID: "c2", Time: now},
		{MessageUUID: "u3", TopicName: "a/b", Payload: []byte("three"), QoS: 2, ClientID: "c1", Time: now},
	}
	if err := s.Enqueue(ctx, msgs); err != nil {
		t.Fatal(err)
	}

	got, err := s.Fetch(ctx, "t", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("fetch returned %d entries, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Offset <= got[i-1].Offset {
			t.Fatalf("offsets not ascending: %+v", got)
		}
	}
	if got[0].Message.TopicName != "a/b" || string(got[0].Message.Payload) != "one" || got[0].Message.ClientID != "c1" ||
		got[0].Message.MessageUUID != "u1" || got[0].Message.QoS != 1 || !got[0].Message.IsQueued {
		t.Fatalf("entry 0 wrong: %+v", got[0])
	}
	if got[0].Message.Time.UnixMilli() != old.UnixMilli() {
		t.Fatalf("entry 0 time %v want %v", got[0].Message.Time, old)
	}
	if len(got[1].Message.Payload) != 3 || got[1].Message.Payload[2] != 2 {
		t.Fatalf("binary payload lost: %+v", got[1].Message.Payload)
	}

	first, last := got[0].Offset, got[2].Offset
	if v, _ := s.GetLatestOffset(ctx, "t"); v != last {
		t.Fatalf("latest = %d want %d", v, last)
	}
	if v, _ := s.GetEarliestOffset(ctx, "t"); v != first {
		t.Fatalf("earliest = %d want %d", v, first)
	}

	part, err := s.Fetch(ctx, "t", got[1].Offset, 1)
	if err != nil || len(part) != 1 || part[0].Offset != got[1].Offset {
		t.Fatalf("fetch from offset/limit: %+v, %v", part, err)
	}

	// Offsets and consumer groups.
	if _, ok, err := s.GetOffset(ctx, "g1", "t", 0); err != nil || ok {
		t.Fatalf("uncommitted offset: ok=%v err=%v", ok, err)
	}
	if err := s.CommitOffset(ctx, "g1", "t", 0, 5); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitOffset(ctx, "g1", "t", 0, 7); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitOffset(ctx, "g2", "t", 0, 1); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := s.GetOffset(ctx, "g1", "t", 0); err != nil || !ok || v != 7 {
		t.Fatalf("committed offset = %d ok=%v err=%v", v, ok, err)
	}
	groups, err := s.GetConsumerGroups(ctx)
	if err != nil || len(groups) != 2 || groups[0].GroupID != "g1" || len(groups[0].Topics) != 1 || groups[0].Topics[0] != "t" {
		t.Fatalf("groups = %+v err=%v", groups, err)
	}
	if time.Since(groups[0].LastCommitTime) > time.Minute {
		t.Fatalf("last commit time %v too old", groups[0].LastCommitTime)
	}
	if ok, err := s.DeleteConsumerGroup(ctx, "g1"); err != nil || !ok {
		t.Fatalf("delete group: %v %v", ok, err)
	}
	if ok, _ := s.DeleteConsumerGroup(ctx, "g1"); ok {
		t.Fatal("second delete should report false")
	}
	if _, ok, _ := s.GetOffset(ctx, "g1", "t", 0); ok {
		t.Fatal("offset survived group delete")
	}

	// Pruning by creation time (ms).
	n, err := s.PruneExpired(ctx, now.Add(-24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("prune = %d, %v", n, err)
	}
	if v, _ := s.GetEarliestOffset(ctx, "t"); v != got[1].Offset {
		t.Fatalf("earliest after prune = %d want %d", v, got[1].Offset)
	}

	// Offsets keep growing after pruning.
	if err := s.Enqueue(ctx, msgs[:1]); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.GetLatestOffset(ctx, "t"); v <= last {
		t.Fatalf("latest %d did not grow past %d", v, last)
	}
}
