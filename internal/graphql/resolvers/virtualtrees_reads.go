package resolvers

import (
	"context"
	"sort"
	"strings"

	"monstermq.io/edge/internal/graphql/generated"
	"monstermq.io/edge/internal/stores"
	"monstermq.io/edge/internal/virtualtrees"
)

// vtReads is the routing snapshot for read paths; nil when no tree is active.
func (r *Resolver) vtReads() *virtualtrees.Resolver {
	vr := r.vtResolver()
	if vr == nil || vr.IsEmpty() {
		return nil
	}
	return vr
}

// vtResolveRead maps a concrete topic to the canonical topic backing it. ok is
// false for unbound virtual topics or when the canonical topic is not readable.
func (r *Resolver) vtResolveRead(ctx context.Context, topic string) (string, bool) {
	vr := r.vtReads()
	if vr == nil {
		return topic, true
	}
	c, ok := vr.ReadResolve(topic)
	if !ok {
		return "", false
	}
	if c != topic && !r.allowTopic(ctx, c, false) {
		return "", false
	}
	return c, true
}

// vtFind runs a store lookup for a (possibly virtual) filter and emits every
// item under the name a subscriber of the filter would see.
func vtFind[T any](r *Resolver, ctx context.Context, filter string, find func(string, func(T) bool) error, topicOf func(T) string, emit func(string, T) bool) error {
	vr := r.vtReads()
	if vr == nil {
		return find(filter, func(item T) bool { return emit(topicOf(item), item) })
	}
	var ferr error
	virtualtrees.FindPresented(vr, filter, func(f string, fn func(T) bool) {
		if err := find(f, fn); err != nil && ferr == nil {
			ferr = err
		}
	}, topicOf, func(name string, item T) bool {
		if c := topicOf(item); c != name && !r.allowTopic(ctx, c, false) {
			return true
		}
		return emit(name, item)
	})
	return ferr
}

func (r *Resolver) vtFindMessages(ctx context.Context, filter string, store stores.MessageStore, emit func(stores.BrokerMessage) bool) error {
	return vtFind(r, ctx, filter, func(f string, fn func(stores.BrokerMessage) bool) error {
		return store.FindMatchingMessages(ctx, f, fn)
	}, func(m stores.BrokerMessage) string { return m.TopicName }, func(name string, m stores.BrokerMessage) bool {
		m.TopicName = name
		return emit(m)
	})
}

// vtBrowse adds virtual names (tree structure and observed mount descendants)
// to a browseTopics result for a wildcard pattern.
func (r *Resolver) vtBrowse(ctx context.Context, pattern string, store stores.MessageStore, out []*generated.Topic) ([]*generated.Topic, error) {
	vr := r.vtReads()
	if vr == nil {
		return out, nil
	}
	levels := strings.Split(pattern, "/")
	depth := len(levels)
	byName := map[string]*generated.Topic{}
	kept := out[:0]
	for _, t := range out {
		if vr.InVirtualNamespace(t.Name) {
			continue
		}
		byName[t.Name] = t
		kept = append(kept, t)
	}
	out = kept
	add := func(name string, leaf bool) {
		parts := strings.Split(name, "/")
		if len(parts) < depth {
			return
		}
		for i, l := range levels {
			if l != "+" && l != "#" && l != parts[i] {
				return
			}
		}
		p := strings.Join(parts[:depth], "/")
		leaf = leaf && len(parts) == depth
		if t, ok := byName[p]; ok {
			t.IsLeaf = t.IsLeaf || leaf
			return
		}
		if !r.allowTopic(ctx, p, false) {
			return
		}
		t := &generated.Topic{Name: p, IsLeaf: leaf}
		byName[p] = t
		out = append(out, t)
	}
	for _, p := range vr.StructuralPaths() {
		add(p, false)
	}
	if store != nil {
		filter := pattern
		if levels[depth-1] != "#" {
			filter += "/#"
		}
		for _, q := range vr.CanonicalQueries(filter) {
			err := store.FindMatchingTopics(ctx, q, func(c string) bool {
				for _, p := range vr.Project(c) {
					if virtualtrees.Match(filter, p.Virtual) && (p.Virtual == c || r.allowTopic(ctx, c, false)) {
						add(p.Virtual, true)
					}
				}
				return true
			})
			if err != nil {
				return nil, err
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// vtStreamFilters returns the bus filters for a topicUpdates subscription and
// whether updates must be projected to virtual names. Filters reaching into a
// tree root subscribe to all canonical traffic so mapping edits apply live.
func (r *Resolver) vtStreamFilters(filters []string) ([]string, bool) {
	vr := r.vtReads()
	if vr == nil {
		return filters, false
	}
	for _, f := range filters {
		for _, t := range vr.Trees() {
			if virtualtrees.Intersects(f, t.RootPath) || virtualtrees.Under(f, t.RootPath) {
				return []string{"#"}, true
			}
		}
	}
	return filters, false
}

// vtPresent returns the presentations of a bus message matching the
// subscription filters, using the current mapping.
func (r *Resolver) vtPresent(ctx context.Context, filters []string, m stores.BrokerMessage) []stores.BrokerMessage {
	vr := r.vtResolver()
	names := []string{m.TopicName}
	if vr != nil && !vr.IsEmpty() {
		names = vr.Presentations(m.TopicName)
	}
	var out []stores.BrokerMessage
	for _, n := range names {
		matched := false
		for _, f := range filters {
			if virtualtrees.Match(f, n) {
				matched = true
				break
			}
		}
		if !matched || !r.allowTopic(ctx, n, false) {
			continue
		}
		if n != m.TopicName && !r.allowTopic(ctx, m.TopicName, false) {
			continue
		}
		c := m
		c.TopicName = n
		out = append(out, c)
	}
	return out
}
