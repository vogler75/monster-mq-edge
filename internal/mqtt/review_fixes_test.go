package mqtt

import (
	"testing"

	"monstermq.io/edge/internal/mqtt/packets"
)

// Regression tests for dev/plans/plan-mqtt-code-review-findings.md.

func TestInlineHashSubscriptionMatchesNestedTopic(t *testing.T) {
	x := NewTopicsIndex()
	x.InlineSubscribe(InlineSubscription{
		Subscription: packets.Subscription{Filter: "foo/#", Identifier: 1},
		Handler:      func(cl *Client, sub packets.Subscription, pk packets.Packet) {},
	})

	for _, topic := range []string{"foo", "foo/bar", "foo/bar/baz"} {
		if subs := x.Subscribers(topic); len(subs.InlineSubscriptions) != 1 {
			t.Errorf("%s: got %d inline subscriptions, want 1", topic, len(subs.InlineSubscriptions))
		}
	}
	if subs := x.Subscribers("other/bar"); len(subs.InlineSubscriptions) != 0 {
		t.Errorf("other/bar: got %d inline subscriptions, want 0", len(subs.InlineSubscriptions))
	}
}

func TestIsValidFilterRejectsPartialWildcardLevels(t *testing.T) {
	invalid := []string{"sport/tennis#", "foo+bar", "foo/+bar/baz", "foo/#/bar", "#/foo", "$share//foo", "$share/group/"}
	for _, f := range invalid {
		if IsValidFilter(f, false) {
			t.Errorf("IsValidFilter(%q) = true, want false", f)
		}
	}

	valid := []string{"#", "+", "foo/#", "foo/+/bar", "+/+", "/foo", "foo//bar", "$share/group/foo/#", "$share/group/+"}
	for _, f := range valid {
		if !IsValidFilter(f, false) {
			t.Errorf("IsValidFilter(%q) = false, want true", f)
		}
	}
}

func TestNextImmediateReturnsDeferredPacket(t *testing.T) {
	i := NewInflights()
	i.Set(packets.Packet{PacketID: 1, Expiry: 100})
	i.Set(packets.Packet{PacketID: 2, Expiry: -1})

	pk, ok := i.NextImmediate()
	if !ok || pk.PacketID != 2 {
		t.Fatalf("NextImmediate() = %d, %v; want 2, true", pk.PacketID, ok)
	}
}

func TestQuotaGetters(t *testing.T) {
	i := NewInflights()
	i.ResetReceiveQuota(3)
	i.ResetSendQuota(2)
	i.DecreaseReceiveQuota()
	i.DecreaseSendQuota()

	if got := i.ReceiveQuota(); got != 2 {
		t.Errorf("ReceiveQuota() = %d, want 2", got)
	}
	if got := i.SendQuota(); got != 1 {
		t.Errorf("SendQuota() = %d, want 1", got)
	}
	if got := i.MaximumSendQuota(); got != 2 {
		t.Errorf("MaximumSendQuota() = %d, want 2", got)
	}
}
