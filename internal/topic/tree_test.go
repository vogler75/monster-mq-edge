package topic

import "testing"

func TestTrailingHashMatchesParentLevel(t *testing.T) {
	tr := NewTree[int]()
	tr.AddData("a/+/#", "k1", 1)
	tr.AddData("a/#", "k2", 1)
	tr.AddData("a/+", "k3", 1)
	if got := len(tr.FindData("a/b")); got != 3 {
		t.Fatalf("FindData(a/b) = %d entries, want 3", got)
	}
	if got := len(tr.FindData("a")); got != 1 {
		t.Fatalf("FindData(a) = %d entries, want 1", got)
	}
	only := NewTree[int]()
	only.AddData("a/+/#", "k", 1)
	if !only.IsMatching("a/b") {
		t.Fatal("IsMatching(a/b) with a/+/# should be true")
	}
	if only.IsMatching("a") {
		t.Fatal("IsMatching(a) with a/+/# should be false")
	}
	s := NewSubscriptionIndex()
	s.Subscribe("c", "a/+/#", 1)
	if len(s.FindSubscribers("a/b")) != 1 {
		t.Fatal("subscription index should match a/b with a/+/#")
	}
}
