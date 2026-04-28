package topic

import "strings"

// Subscriber is the result type of a publish-side lookup.
type Subscriber struct {
	ClientID string
	QoS      byte
}

// SubscriptionIndex is the dual-index subscription manager used by the edge
// broker. It mirrors the Kotlin SubscriptionManager: an exact-match hash for
// wildcard-free topics plus a Tree for wildcard patterns.
//
// What this index intentionally does NOT track (in contrast to the Kotlin
// version): noLocal and retainAsPublished. On the edge those are handled by
// mochi-mqtt on the live MQTT publish path. This index exists for paths that
// run alongside mochi — currently the offline-queue hook — where the cost of
// scanning the persisted subscription table per published message is the
// dominating factor.
//
// All methods are safe for concurrent use.
type SubscriptionIndex struct {
	exact    *ExactIndex[byte]
	wildcard *WildcardIndex[byte]
}

func NewSubscriptionIndex() *SubscriptionIndex {
	return &SubscriptionIndex{
		exact:    NewExactIndex[byte](),
		wildcard: NewWildcardIndex[byte](),
	}
}

// Subscribe registers a (clientID, filter) subscription with the given QoS.
// The filter is routed to the exact or wildcard index based on whether it
// contains '+' or '#'.
func (s *SubscriptionIndex) Subscribe(clientID, filter string, qos byte) {
	if hasWildcard(filter) {
		s.wildcard.Add(filter, clientID, qos)
	} else {
		s.exact.Add(filter, clientID, qos)
	}
}

// Unsubscribe removes the (clientID, filter) subscription from whichever
// index it lives in.
func (s *SubscriptionIndex) Unsubscribe(clientID, filter string) bool {
	if hasWildcard(filter) {
		return s.wildcard.Remove(filter, clientID)
	}
	return s.exact.Remove(filter, clientID)
}

// FindSubscribers resolves all subscribers for a published topic across both
// indexes. Duplicates (a client subscribed via both an exact and a wildcard
// filter) are collapsed, keeping the highest QoS.
//
// Hot path. Allocates once for the result; returns a freshly owned slice.
func (s *SubscriptionIndex) FindSubscribers(topic string) []Subscriber {
	exact := s.exact.Find(topic)
	wild := s.wildcard.FindMatching(topic)
	if len(exact) == 0 && len(wild) == 0 {
		return nil
	}
	// Dedupe by clientID, keeping highest QoS.
	merged := make(map[string]byte, len(exact)+len(wild))
	for _, e := range exact {
		merged[e.Key] = e.Value
	}
	for _, e := range wild {
		if cur, ok := merged[e.Key]; !ok || e.Value > cur {
			merged[e.Key] = e.Value
		}
	}
	out := make([]Subscriber, 0, len(merged))
	for cid, q := range merged {
		out = append(out, Subscriber{ClientID: cid, QoS: q})
	}
	return out
}

// HasSubscribers is the cheap "any match?" probe used to skip expensive work
// when nobody cares about a topic.
func (s *SubscriptionIndex) HasSubscribers(topic string) bool {
	return s.exact.Has(topic) || s.wildcard.HasMatching(topic)
}

// HasSubscription returns whether the specific (clientID, filter) is present
// (used for MQTT v5 retain-handling option 1).
func (s *SubscriptionIndex) HasSubscription(clientID, filter string) bool {
	if hasWildcard(filter) {
		// The wildcard index has no direct (clientId, pattern) probe; iterate
		// the patterns that match the same depth — but in practice this method
		// is only called by retain-handling code, which already has the exact
		// filter the client subscribed to, so we can do the cheap thing and
		// look up the entries at that pattern.
		for _, e := range s.wildcard.tree.FindData(filter) {
			if e.Key == clientID {
				return true
			}
		}
		return false
	}
	return s.exact.HasKey(filter, clientID)
}

// DisconnectClient purges every subscription owned by clientID from both
// indexes. Returns the filters that were removed.
func (s *SubscriptionIndex) DisconnectClient(clientID string) []string {
	out := s.exact.RemoveKey(clientID)
	out = append(out, s.wildcard.RemoveKey(clientID)...)
	return out
}

// Stats describes the current shape of the index. Useful for /metrics and
// debugging.
type Stats struct {
	ExactTopics        int
	ExactSubscriptions int
	WildcardEntries    int
}

func (s *SubscriptionIndex) Stats() Stats {
	return Stats{
		ExactTopics:        s.exact.TopicCount(),
		ExactSubscriptions: s.exact.EntryCount(),
		WildcardEntries:    s.wildcard.PatternCount(),
	}
}

func hasWildcard(filter string) bool {
	return strings.ContainsAny(filter, "+#")
}
