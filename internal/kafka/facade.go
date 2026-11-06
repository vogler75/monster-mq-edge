package kafka

import (
	"context"
	"sort"
	"time"

	"monstermq.io/edge/internal/stores"
)

// GroupInfo is a consumer group as shown by the dashboard.
type GroupInfo struct {
	GroupID    string
	Topics     []string
	LastCommit time.Time
	// Lag is the number of queued messages the group has not yet committed past,
	// summed over all of its topics and across every stream store.
	Lag int64
}

// MessageInfo is one queued message as shown by the dashboard.
type MessageInfo struct {
	Offset    int64
	Topic     string // MQTT topic of the message
	Partition int
	Timestamp time.Time
	Key       *string // MQTT topic, as on the wire
	Value     *string
	Size      int
}

// ConsumerGroups lists the groups with committed offsets in any stream of this
// server, merging a group's topics and taking the newest commit time. The lag
// is computed per stream store and summed, so it covers all streams.
func (s *Server) ConsumerGroups(ctx context.Context) ([]GroupInfo, error) {
	merged := map[string]*GroupInfo{}
	var firstErr error
	for _, st := range s.streams {
		groups, err := st.store.GetConsumerGroups(ctx)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, g := range groups {
			gi := merged[g.GroupID]
			if gi == nil {
				gi = &GroupInfo{GroupID: g.GroupID}
				merged[g.GroupID] = gi
			}
			for _, t := range g.Topics {
				if !contains(gi.Topics, t) {
					gi.Topics = append(gi.Topics, t)
				}
			}
			if g.LastCommitTime.After(gi.LastCommit) {
				gi.LastCommit = g.LastCommitTime
			}
			gi.Lag += s.groupLag(ctx, st, g)
		}
	}
	out := make([]GroupInfo, 0, len(merged))
	for _, g := range merged {
		sort.Strings(g.Topics)
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GroupID < out[j].GroupID })
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// groupLag sums max(0, nextOffset - committed) over the group's topics held by st.
func (s *Server) groupLag(ctx context.Context, st *stream, g stores.KafkaConsumerGroup) int64 {
	var lag int64
	for _, topic := range g.Topics {
		committed, ok, err := st.store.GetOffset(ctx, g.GroupID, topic, 0)
		if err != nil || !ok {
			continue
		}
		latest, err := st.store.GetLatestOffset(ctx, topic)
		if err != nil {
			continue
		}
		if d := latest + 1 - committed; d > 0 {
			lag += d
		}
	}
	return lag
}

func contains(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

// Messages returns up to limit queued messages of the stream behind topic. A
// negative startOffset returns the newest limit messages.
func (s *Server) Messages(ctx context.Context, topic string, startOffset int64, limit int) ([]MessageInfo, error) {
	st := s.routeFetch(topic)
	if st == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 100
	}
	if startOffset < 0 {
		earliest, err := st.store.GetEarliestOffset(ctx, topic)
		if err != nil {
			return nil, err
		}
		latest, err := st.store.GetLatestOffset(ctx, topic)
		if err != nil {
			return nil, err
		}
		startOffset = latest - int64(limit) + 1
		if startOffset < earliest {
			startOffset = earliest
		}
	}
	entries, err := st.store.Fetch(ctx, topic, startOffset, limit)
	if err != nil {
		return nil, err
	}
	out := make([]MessageInfo, 0, len(entries))
	for _, e := range entries {
		key := e.Message.TopicName
		val := string(e.Message.Payload)
		out = append(out, MessageInfo{
			Offset: e.Offset, Topic: e.Message.TopicName, Timestamp: e.Message.Time,
			Key: &key, Value: &val, Size: len(e.Message.Payload),
		})
	}
	return out, nil
}

// TopicOffsets returns the earliest and latest stored offsets of a topic's
// stream; ok is false when no stream serves the topic.
func (s *Server) TopicOffsets(ctx context.Context, topic string) (earliest, latest int64, ok bool, err error) {
	st := s.routeFetch(topic)
	if st == nil {
		return 0, 0, false, nil
	}
	if earliest, err = st.store.GetEarliestOffset(ctx, topic); err != nil {
		return 0, 0, false, err
	}
	if latest, err = st.store.GetLatestOffset(ctx, topic); err != nil {
		return 0, 0, false, err
	}
	return earliest, latest, true, nil
}

// DeleteConsumerGroup removes the group's committed offsets from every stream
// and reports whether any existed.
func (s *Server) DeleteConsumerGroup(ctx context.Context, groupID string) (bool, error) {
	any := false
	var firstErr error
	for _, st := range s.streams {
		ok, err := st.store.DeleteConsumerGroup(ctx, groupID)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		any = any || ok
	}
	if !any && firstErr != nil {
		return false, firstErr
	}
	return any, nil
}
