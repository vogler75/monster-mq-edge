package virtualtrees

import (
	"sort"
	"strings"
)

// Route is a binding node compiled for routing.
type Route struct {
	NodeID  string
	Path    string
	Binding Binding
	tree    *compiled
}

// TreeID returns the id of the tree owning the route.
func (r *Route) TreeID() string { return r.tree.tree.ID }

// Presentation is one virtual name of a canonical topic.
type Presentation struct {
	Virtual string
	NodeID  string
	TreeID  string
	Access  string
}

type compiled struct {
	tree    *Tree
	paths   map[string]string
	exact   map[string]*Route
	mounts  map[string]*Route
	folders map[string]bool
}

func (c *compiled) winner(v string) *Route {
	if r, ok := c.exact[v]; ok {
		return r
	}
	if len(c.mounts) == 0 {
		return nil
	}
	p := v
	for {
		if r, ok := c.mounts[p]; ok {
			return r
		}
		i := strings.LastIndexByte(p, '/')
		if i < 0 {
			return nil
		}
		p = p[:i]
	}
}

// Resolver is an immutable, compiled routing table for a set of valid trees.
// Lookups walk topic levels over hash maps; cost is independent of the number
// of bindings.
type Resolver struct {
	trees          []*Tree
	compiled       []*compiled
	byRoot         map[string]*compiled
	exactByTarget  map[string][]*Route
	mountsByTarget map[string][]*Route
}

// Empty is a resolver without trees.
var Empty = NewResolver(nil)

func NewResolver(trees []*Tree) *Resolver {
	r := &Resolver{
		trees:          trees,
		byRoot:         map[string]*compiled{},
		exactByTarget:  map[string][]*Route{},
		mountsByTarget: map[string][]*Route{},
	}
	for _, t := range trees {
		c := &compiled{tree: t, paths: t.Paths(), exact: map[string]*Route{}, mounts: map[string]*Route{}, folders: map[string]bool{}}
		for _, n := range t.Nodes {
			p, ok := c.paths[n.ID]
			if !ok {
				continue
			}
			switch {
			case n.Binding == nil:
				c.folders[p] = true
			case n.Binding.Kind == Exact:
				c.exact[p] = &Route{NodeID: n.ID, Path: p, Binding: *n.Binding, tree: c}
			default:
				c.mounts[p] = &Route{NodeID: n.ID, Path: p, Binding: *n.Binding, tree: c}
			}
		}
		r.compiled = append(r.compiled, c)
		if _, ok := r.byRoot[t.RootPath]; !ok {
			r.byRoot[t.RootPath] = c
		}
		if !t.Enabled {
			continue
		}
		for _, e := range c.exact {
			r.exactByTarget[e.Binding.Target] = append(r.exactByTarget[e.Binding.Target], e)
		}
		for _, m := range c.mounts {
			r.mountsByTarget[m.Binding.Target] = append(r.mountsByTarget[m.Binding.Target], m)
		}
	}
	return r
}

func (r *Resolver) IsEmpty() bool { return len(r.compiled) == 0 }

func (r *Resolver) Trees() []*Tree { return r.trees }

func (r *Resolver) Tree(id string) *Tree {
	for _, t := range r.trees {
		if t.ID == id {
			return t
		}
	}
	return nil
}

func (r *Resolver) owner(topic string) *compiled {
	if len(r.byRoot) == 0 {
		return nil
	}
	p := topic
	for {
		if c, ok := r.byRoot[p]; ok {
			return c
		}
		i := strings.LastIndexByte(p, '/')
		if i < 0 {
			return nil
		}
		p = p[:i]
	}
}

// InVirtualNamespace reports whether the topic lies in a reserved root
// (enabled or disabled tree).
func (r *Resolver) InVirtualNamespace(topic string) bool { return r.owner(topic) != nil }

func canonical(v string, rt *Route) string {
	if rt.Binding.Kind == Exact {
		return rt.Binding.Target
	}
	return rt.Binding.Target + v[len(rt.Path):]
}

// Publish resolves a concrete publish topic. Physical topics resolve to
// themselves; a non-empty reject means the publish must be refused.
func (r *Resolver) Publish(topic string) (canonicalTopic, reject string) {
	c := r.owner(topic)
	if c == nil {
		return topic, ""
	}
	if !c.tree.Enabled {
		return "", RejectDisabled
	}
	rt := c.winner(topic)
	if rt == nil {
		return "", RejectUnbound
	}
	if rt.Binding.Access != ReadWrite {
		return "", RejectReadOnly
	}
	return canonical(topic, rt), ""
}

// WinnerRoute is the winning binding of a virtual topic regardless of access.
func (r *Resolver) WinnerRoute(topic string) *Route {
	c := r.owner(topic)
	if c == nil || !c.tree.Enabled {
		return nil
	}
	return c.winner(topic)
}

// Canonical returns the canonical topic of a virtual topic through a route.
func Canonical(topic string, rt *Route) string { return canonical(topic, rt) }

// ReadResolve returns the canonical topic backing a concrete topic for reads.
// Physical topics resolve to themselves; ok is false for virtual topics
// without a backing source (unbound, folder, disabled tree).
func (r *Resolver) ReadResolve(topic string) (string, bool) {
	c := r.owner(topic)
	if c == nil {
		return topic, true
	}
	if !c.tree.Enabled {
		return "", false
	}
	rt := c.winner(topic)
	if rt == nil {
		return "", false
	}
	return canonical(topic, rt), true
}

// Project returns every virtual presentation of a canonical topic (enabled
// trees only), sorted by virtual topic.
func (r *Resolver) Project(canonicalTopic string) []Presentation {
	if len(r.exactByTarget) == 0 && len(r.mountsByTarget) == 0 {
		return nil
	}
	var out []Presentation
	for _, e := range r.exactByTarget[canonicalTopic] {
		out = append(out, Presentation{Virtual: e.Path, NodeID: e.NodeID, TreeID: e.tree.tree.ID, Access: e.Binding.Access})
	}
	if len(r.mountsByTarget) > 0 {
		target := canonicalTopic
		for {
			for _, m := range r.mountsByTarget[target] {
				v := m.Path + canonicalTopic[len(target):]
				if m.tree.winner(v) == m {
					out = append(out, Presentation{Virtual: v, NodeID: m.NodeID, TreeID: m.tree.tree.ID, Access: m.Binding.Access})
				}
			}
			i := strings.LastIndexByte(target, '/')
			if i < 0 {
				break
			}
			target = target[:i]
		}
	}
	if len(out) > 1 {
		sort.Slice(out, func(i, j int) bool { return out[i].Virtual < out[j].Virtual })
	}
	return out
}

// Presentations returns the topic names under which a message on the
// canonical topic is presented: the topic itself (unless it lies in a reserved
// root) plus all virtual presentations.
func (r *Resolver) Presentations(canonicalTopic string) []string {
	proj := r.Project(canonicalTopic)
	physical := !r.InVirtualNamespace(canonicalTopic)
	if len(proj) == 0 {
		if physical {
			return []string{canonicalTopic}
		}
		return nil
	}
	out := make([]string, 0, len(proj)+1)
	if physical {
		out = append(out, canonicalTopic)
	}
	for _, p := range proj {
		dup := false
		for _, o := range out {
			if o == p.Virtual {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, p.Virtual)
		}
	}
	return out
}

// Presented returns the names one subscription with the filter receives for a
// message on the canonical topic (deduplicated, sorted).
func (r *Resolver) Presented(filter, canonicalTopic string) []string {
	var out []string
	for _, p := range r.Presentations(canonicalTopic) {
		if Match(filter, p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// SubscribeRoutes returns the routes whose binding can contribute deliveries
// to a virtual filter (all trees, before precedence), sorted by node id.
func (r *Resolver) SubscribeRoutes(filter string) []*Route {
	var out []*Route
	for _, c := range r.compiled {
		for _, e := range c.exact {
			if Match(filter, e.Path) {
				out = append(out, e)
			}
		}
		for _, m := range c.mounts {
			if Intersects(filter, m.Path) {
				out = append(out, m)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// CanonicalFilters returns the canonical filters a virtual filter maps to
// through one route (empty if none).
func CanonicalFilters(filter string, rt *Route) []string {
	if rt.Binding.Kind == Exact {
		if Match(filter, rt.Path) {
			return []string{rt.Binding.Target}
		}
		return nil
	}
	f := strings.Split(filter, "/")
	p := strings.Split(rt.Path, "/")
	target := rt.Binding.Target
	for i := range p {
		if i >= len(f) {
			return nil
		}
		if f[i] == "#" {
			return []string{target + "/#", target}
		}
		if f[i] != "+" && f[i] != p[i] {
			return nil
		}
	}
	if len(f) == len(p) {
		return []string{target}
	}
	rest := strings.Join(f[len(p):], "/")
	if rest == "#" {
		return []string{target + "/#", target}
	}
	return []string{target + "/" + rest}
}

// CanonicalQueries returns the distinct canonical filters needed to look up
// stored data for a (possibly virtual) filter.
func (r *Resolver) CanonicalQueries(filter string) []string {
	var out []string
	seen := map[string]bool{}
	addAll := func(fs []string) {
		for _, f := range fs {
			if !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	for _, c := range r.compiled {
		if !c.tree.Enabled {
			continue
		}
		for _, rt := range sortedRoutes(c.exact) {
			addAll(CanonicalFilters(filter, rt))
		}
		for _, rt := range sortedRoutes(c.mounts) {
			addAll(CanonicalFilters(filter, rt))
		}
	}
	return out
}

func sortedRoutes(m map[string]*Route) []*Route {
	out := make([]*Route, 0, len(m))
	for _, r := range m {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// FindPresented looks up stored items for a subscription filter as a subscriber
// sees them: physical items outside reserved roots plus virtual presentations
// of canonical items matching the filter. finder runs a store query and calls
// its callback per item until it returns false; emit receives the presented
// topic and the item and returns false to stop.
func FindPresented[T any](r *Resolver, filter string, finder func(string, func(T) bool), topicOf func(T) string, emit func(string, T) bool) {
	seen := map[string]bool{}
	stop := false
	finder(filter, func(item T) bool {
		t := topicOf(item)
		if !r.InVirtualNamespace(t) && !seen[t] {
			seen[t] = true
			if !emit(t, item) {
				stop = true
			}
		}
		return !stop
	})
	if stop || r.IsEmpty() {
		return
	}
	for _, q := range r.CanonicalQueries(filter) {
		finder(q, func(item T) bool {
			c := topicOf(item)
			for _, p := range r.Project(c) {
				if Match(filter, p.Virtual) && !seen[p.Virtual] {
					seen[p.Virtual] = true
					if !emit(p.Virtual, item) {
						stop = true
						break
					}
				}
			}
			return !stop
		})
		if stop {
			return
		}
	}
}

// StructuralPaths returns the roots and node paths of enabled trees.
func (r *Resolver) StructuralPaths() []string {
	var out []string
	for _, c := range r.compiled {
		if !c.tree.Enabled {
			continue
		}
		out = append(out, c.tree.RootPath)
		for _, p := range c.paths {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}
