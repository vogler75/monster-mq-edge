package mqtt

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

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

type aclRecHook struct {
	HookBase
	mu     sync.Mutex
	topics []string
}

func (h *aclRecHook) ID() string { return "acl-rec" }

func (h *aclRecHook) Provides(b byte) bool { return b == OnACLCheck }

func (h *aclRecHook) OnACLCheck(_ *Client, topic string, _ bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.topics = append(h.topics, topic)
	return true
}

func newAliasClient(t *testing.T) (*Server, *Client, *aclRecHook) {
	t.Helper()
	s := New(&Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	h := new(aclRecHook)
	if err := s.AddHook(h, nil); err != nil {
		t.Fatal(err)
	}
	c1, c2 := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, c2) }()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
	cl := s.NewClient(c1, "t1", "alias-client", false)
	cl.Properties.ProtocolVersion = 5
	return s, cl, h
}

func aliasPk(topic string, alias uint16) packets.Packet {
	pk := packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Publish}, TopicName: topic, Payload: []byte("x")}
	pk.Properties.TopicAlias = alias
	pk.Properties.TopicAliasFlag = true
	return pk
}

func TestTopicAliasResolvedBeforeACL(t *testing.T) {
	s, cl, h := newAliasClient(t)
	if err := s.processPublish(cl, aliasPk("a/b", 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.processPublish(cl, aliasPk("", 1)); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.topics) != 2 || h.topics[0] != "a/b" || h.topics[1] != "a/b" {
		t.Fatalf("ACL saw topics %q, want [a/b a/b]", h.topics)
	}
}

func TestUnknownTopicAliasDisconnects(t *testing.T) {
	s, cl, h := newAliasClient(t)
	err := s.processPublish(cl, aliasPk("", 7))
	if !errors.Is(err, packets.ErrTopicAliasInvalid) {
		t.Fatalf("err = %v, want ErrTopicAliasInvalid", err)
	}
	if len(h.topics) != 0 {
		t.Fatalf("ACL checked %q for an unknown alias", h.topics)
	}
	if got := cl.State.TopicAliases.Inbound.Set(7, ""); got != "" {
		t.Fatalf("unknown alias was stored as %q", got)
	}
}

func TestConnectTimeoutClosesSilentConnection(t *testing.T) {
	s := New(&Options{ConnectTimeout: 50 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	c1, c2 := net.Pipe()
	defer c2.Close()

	done := make(chan error, 1)
	go func() { done <- s.EstablishConnection("t1", c1) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("EstablishConnection returned nil for a connection that never sent CONNECT")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connection without CONNECT was not closed")
	}
}
