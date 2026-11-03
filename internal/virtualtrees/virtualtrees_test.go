package virtualtrees

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"

	"monstermq.io/edge/internal/stores"
)

type fixture struct {
	Name    string            `json:"name"`
	Trees   []json.RawMessage `json:"trees"`
	Valid   *bool             `json:"valid"`
	Errors  []string          `json:"errors"`
	Publish []struct {
		Topic     string `json:"topic"`
		Canonical string `json:"canonical"`
		Reject    string `json:"reject"`
	} `json:"publish"`
	Project []struct {
		Canonical string   `json:"canonical"`
		Virtual   []string `json:"virtual"`
	} `json:"project"`
	Deliver []struct {
		Filter    string   `json:"filter"`
		Canonical string   `json:"canonical"`
		Presented []string `json:"presented"`
	} `json:"deliver"`
}

func loadFixtures(t *testing.T) []fixture {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	sort.Strings(files)
	var out []fixture
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var list []fixture
		if err := json.Unmarshal(b, &list); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out = append(out, list...)
	}
	return out
}

func sameSet(a, b []string) bool {
	x := append([]string{}, a...)
	y := append([]string{}, b...)
	sort.Strings(x)
	sort.Strings(y)
	if len(x) == 0 && len(y) == 0 {
		return true
	}
	return reflect.DeepEqual(x, y)
}

func TestFixtures(t *testing.T) {
	for _, f := range loadFixtures(t) {
		t.Run(f.Name, func(t *testing.T) {
			var trees []*Tree
			for _, raw := range f.Trees {
				tr, err := ParseJSON(raw)
				if err != nil {
					t.Fatalf("parse: %v", err)
				}
				trees = append(trees, tr)
			}
			var issues []Issue
			for i, tr := range trees {
				var rest []*Tree
				rest = append(rest, trees[:i]...)
				rest = append(rest, trees[i+1:]...)
				issues = append(issues, Validate(tr, rest)...)
			}
			if f.Valid != nil && !*f.Valid {
				if len(issues) == 0 {
					t.Fatal("expected validation issues, got none")
				}
				got := map[string]bool{}
				for _, is := range issues {
					got[is.Code] = true
				}
				for _, code := range f.Errors {
					if !got[code] {
						t.Errorf("expected issue %s, got %v", code, issues)
					}
				}
				return
			}
			if len(issues) > 0 {
				t.Fatalf("expected valid configuration, got %v", issues)
			}
			r := NewResolver(trees)
			for _, p := range f.Publish {
				c, rej := r.Publish(p.Topic)
				if c != p.Canonical || rej != p.Reject {
					t.Errorf("publish %q: got (%q,%q) want (%q,%q)", p.Topic, c, rej, p.Canonical, p.Reject)
				}
			}
			for _, p := range f.Project {
				var got []string
				for _, v := range r.Project(p.Canonical) {
					got = append(got, v.Virtual)
				}
				if !sameSet(got, p.Virtual) {
					t.Errorf("project %q: got %v want %v", p.Canonical, got, p.Virtual)
				}
			}
			for _, d := range f.Deliver {
				got := r.Presented(d.Filter, d.Canonical)
				if !sameSet(got, d.Presented) {
					t.Errorf("deliver %q <- %q: got %v want %v", d.Filter, d.Canonical, got, d.Presented)
				}
				// Delivery through canonical filters and projection must agree.
				var viaQueries []string
				FindPresented(r, d.Filter, func(q string, fn func(string) bool) {
					if Match(q, d.Canonical) {
						fn(d.Canonical)
					}
				}, func(s string) string { return s }, func(v string, _ string) bool {
					viaQueries = append(viaQueries, v)
					return true
				})
				if !sameSet(viaQueries, d.Presented) {
					t.Errorf("findPresented %q <- %q: got %v want %v", d.Filter, d.Canonical, viaQueries, d.Presented)
				}
			}
		})
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		filter, topic string
		want          bool
	}{
		{"a/#", "a", true},
		{"a/#", "a/b/c", true},
		{"a/+", "a/b", true},
		{"a/+", "a/b/c", false},
		{"a/+", "a/", true},
		{"+/+", "/a", true},
		{"#", "$SYS/x", false},
		{"+/x", "$SYS/x", false},
		{"$SYS/#", "$SYS/x", true},
		{"a/b", "a/B", false},
		{"area/#", "area2/x", false},
		{"a/+/#", "a/b", true},
		{"a/b/#", "a", false},
		{"a", "a/b", false},
		{"#", "a/b", true},
	}
	for _, c := range cases {
		if got := Match(c.filter, c.topic); got != c.want {
			t.Errorf("Match(%q,%q)=%v want %v", c.filter, c.topic, got, c.want)
		}
	}
}

func TestParseNormalizesAndRejects(t *testing.T) {
	tr, err := ParseJSON([]byte(`{"schemaVersion":1,"id":"t","rootPath":"v","nodes":[{"id":"a","parentId":null,"segment":"x","binding":{"kind":"SUBTREE","target":"p/#"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Name != "t" || !tr.Enabled || tr.Nodes[0].Binding.Target != "p" || tr.Nodes[0].Binding.Access != ReadOnly {
		t.Fatalf("unexpected parse result %+v %+v", tr, tr.Nodes[0].Binding)
	}
	if _, err := ParseJSON([]byte(`{"schemaVersion":1,"id":"t","rootPath":"v","nodes":"not-a-list"}`)); err == nil {
		t.Fatal("expected error for nodes not a list")
	}
	if _, err := ParseJSON([]byte(`{"id":"t","rootPath":"v","nodes":[]}`)); err == nil {
		t.Fatal("expected error for missing schemaVersion")
	}
	back, err := ParseJSON(tr.Stored())
	if err != nil || !reflect.DeepEqual(back, tr) {
		t.Fatalf("round trip mismatch: %v %+v", err, back)
	}
}

type memStore struct {
	mu   sync.Mutex
	devs map[string]stores.DeviceConfig
}

func (s *memStore) GetAll(ctx context.Context) ([]stores.DeviceConfig, error) { return s.GetByType(ctx, "") }
func (s *memStore) GetByType(_ context.Context, typ string) ([]stores.DeviceConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []stores.DeviceConfig
	for _, d := range s.devs {
		if typ == "" || d.Type == typ {
			out = append(out, d)
		}
	}
	return out, nil
}
func (s *memStore) GetByNode(ctx context.Context, _ string) ([]stores.DeviceConfig, error) {
	return s.GetAll(ctx)
}
func (s *memStore) GetEnabledByNode(ctx context.Context, _ string) ([]stores.DeviceConfig, error) {
	return s.GetAll(ctx)
}
func (s *memStore) Get(_ context.Context, name string) (*stores.DeviceConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.devs[name]; ok {
		return &d, nil
	}
	return nil, nil
}
func (s *memStore) Save(_ context.Context, d stores.DeviceConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devs[d.Name] = d
	return nil
}
func (s *memStore) Delete(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.devs, name)
	return nil
}
func (s *memStore) Toggle(context.Context, string, bool) (*stores.DeviceConfig, error) { return nil, nil }
func (s *memStore) Reassign(context.Context, string, string) (*stores.DeviceConfig, error) {
	return nil, nil
}
func (s *memStore) Close() error { return nil }

func sample(id, root string) *Tree {
	p := "f"
	return &Tree{SchemaVersion: 1, ID: id, Name: id, RootPath: root, Enabled: true, Nodes: []Node{
		{ID: "f", Segment: "folder"},
		{ID: "m", ParentID: &p, Segment: "mount", Binding: &Binding{Kind: Subtree, Target: "plant/" + id, Access: ReadWrite}},
	}}
}

func TestManagerRevisionsAndPersistence(t *testing.T) {
	ctx := context.Background()
	st := &memStore{devs: map[string]stores.DeviceConfig{}}
	m := NewManager(st, nil)
	r := m.Save(ctx, sample("a", "va"), "")
	if !r.Success || r.Tree.Revision == "" {
		t.Fatalf("create failed: %+v", r)
	}
	if r2 := m.Save(ctx, sample("a", "va"), ""); r2.Success || r2.Errors[0].Code != CodeRevisionConflict {
		t.Fatalf("expected revision conflict, got %+v", r2)
	}
	if r2 := m.Save(ctx, sample("b", "vb"), "x"); r2.Success || r2.Errors[0].Code != CodeNotFound {
		t.Fatalf("expected not found, got %+v", r2)
	}
	if r2 := m.Save(ctx, sample("c", "va/sub"), ""); r2.Success || r2.Errors[0].Code != CodeRootConflict {
		t.Fatalf("expected root conflict, got %+v", r2)
	}
	if c, rej := m.Resolver().Publish("va/folder/mount/x"); c != "plant/a/x" || rej != "" {
		t.Fatalf("publish routing: %q %q", c, rej)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if m.Save(ctx, sample("a", "va"), r.Tree.Revision).Success {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("parallel saves: %d winners", wins)
	}

	m2 := NewManager(st, nil)
	if err := m2.Load(ctx); err != nil {
		t.Fatal(err)
	}
	cur := m2.Tree("a")
	if cur == nil || cur.Revision != m.Tree("a").Revision {
		t.Fatalf("reload mismatch: %+v", cur)
	}
	if d := m2.Delete(ctx, "a", "wrong"); d.Success || d.Errors[0].Code != CodeRevisionConflict {
		t.Fatalf("expected delete conflict, got %+v", d)
	}
	if d := m2.Delete(ctx, "a", cur.Revision); !d.Success {
		t.Fatalf("delete failed: %+v", d)
	}
	if d := m2.Delete(ctx, "a", cur.Revision); d.Success || d.Errors[0].Code != CodeNotFound {
		t.Fatalf("expected not found, got %+v", d)
	}
	if !m2.Resolver().IsEmpty() {
		t.Fatal("resolver should be empty")
	}
}

func TestManagerImport(t *testing.T) {
	ctx := context.Background()
	st := &memStore{devs: map[string]stores.DeviceConfig{}}
	m := NewManager(st, nil)
	doc := string(sample("a", "va").Document())
	res := m.Import(ctx, []ImportEntry{{Name: "a", Config: doc}})
	if res.Imported != 1 {
		t.Fatalf("import failed: %+v", res)
	}
	if tr := m.Tree("a"); tr == nil || tr.Enabled {
		t.Fatalf("imported tree must be disabled: %+v", tr)
	}
	if _, rej := m.Resolver().Publish("va/folder/mount/x"); rej != RejectDisabled {
		t.Fatalf("expected disabled reject, got %q", rej)
	}
	bad := []ImportEntry{
		{Name: "b", Config: `{"schemaVersion":2,"id":"b","rootPath":"vb","nodes":[]}`},
		{Name: "c", Config: `{"schemaVersion":1,"id":"c","rootPath":"vc","nodes":"not-a-list"}`},
	}
	if res := m.Import(ctx, bad); res.Imported != 0 || res.Failed != 2 {
		t.Fatalf("expected both rejected: %+v", res)
	}
	if m.Tree("b") != nil || m.Tree("c") != nil {
		t.Fatal("rejected trees must not be stored")
	}
	if r := m.SetEnabled(ctx, "a", true); !r.Success || !m.Tree("a").Enabled {
		t.Fatalf("enable failed: %+v", r)
	}
}
