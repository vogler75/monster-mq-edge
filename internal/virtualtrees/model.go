// Package virtualtrees implements virtual MQTT topic trees with subtree mounts
// (issue #217): a tree reserves a root topic as a virtual namespace whose nodes
// present canonical topics (EXACT) or canonical subtrees (SUBTREE) under
// alternative names.
package virtualtrees

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const (
	SchemaVersion = 1
	DeviceType    = "VirtualTopicTree"
	DeviceNodeID  = "*"

	Exact     = "EXACT"
	Subtree   = "SUBTREE"
	ReadOnly  = "READ_ONLY"
	ReadWrite = "READ_WRITE"
)

const (
	CodeDuplicatePath    = "DUPLICATE_PATH"
	CodeDuplicateID      = "DUPLICATE_ID"
	CodeInvalidParent    = "INVALID_PARENT"
	CodeCycle            = "CYCLE"
	CodeInvalidSegment   = "INVALID_SEGMENT"
	CodeUnsupportedWild  = "UNSUPPORTED_WILDCARD"
	CodeTargetInRoot     = "TARGET_IN_VIRTUAL_ROOT"
	CodeRootConflict     = "ROOT_CONFLICT"
	CodeInvalidTopic     = "INVALID_TOPIC"
	CodeSchemaVersion    = "UNSUPPORTED_SCHEMA_VERSION"
	CodeReservedTopic    = "RESERVED_TOPIC"
	CodeInvalidBinding   = "INVALID_BINDING"
	CodeRevisionConflict = "REVISION_CONFLICT"
	CodePhysicalConflict = "PHYSICAL_CONFLICT"
	CodeNotFound         = "NOT_FOUND"
	CodeInvalidInput     = "INVALID_INPUT"
	CodeFeatureDisabled  = "FEATURE_DISABLED"
	CodeStoreError       = "STORE_ERROR"

	RejectReadOnly = "READ_ONLY"
	RejectUnbound  = "UNBOUND"
	RejectDisabled = "TREE_DISABLED"
)

type Binding struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
	Access string `json:"access"`
}

type Node struct {
	ID          string   `json:"id"`
	ParentID    *string  `json:"parentId"`
	Segment     string   `json:"segment"`
	Description string   `json:"description,omitempty"`
	Binding     *Binding `json:"binding,omitempty"`
}

type Tree struct {
	SchemaVersion int    `json:"schemaVersion"`
	ID            string `json:"id"`
	Name          string `json:"name"`
	RootPath      string `json:"rootPath"`
	Description   string `json:"description,omitempty"`
	Enabled       bool   `json:"enabled"`
	Nodes         []Node `json:"nodes"`
	Revision      string `json:"revision,omitempty"`
}

// Issue is a validation or mutation problem reported through GraphQL.
type Issue struct {
	Code    string
	NodeID  string
	Path    string
	Message string
}

// Document returns the portable JSON document (without the server revision).
func (t *Tree) Document() []byte {
	c := *t
	c.Revision = ""
	if c.Nodes == nil {
		c.Nodes = []Node{}
	}
	b, _ := json.Marshal(&c)
	return b
}

// Stored returns the stored representation: the document plus its revision.
func (t *Tree) Stored() []byte {
	c := *t
	if c.Nodes == nil {
		c.Nodes = []Node{}
	}
	b, _ := json.Marshal(&c)
	return b
}

func (t *Tree) Clone() *Tree {
	c := *t
	c.Nodes = make([]Node, len(t.Nodes))
	for i, n := range t.Nodes {
		if n.ParentID != nil {
			p := *n.ParentID
			n.ParentID = &p
		}
		if n.Binding != nil {
			b := *n.Binding
			n.Binding = &b
		}
		c.Nodes[i] = n
	}
	return &c
}

// Paths derives the virtual path per node id; nodes in a cycle or below an
// unknown parent are omitted.
func (t *Tree) Paths() map[string]string {
	byID := make(map[string]*Node, len(t.Nodes))
	for i := range t.Nodes {
		if _, ok := byID[t.Nodes[i].ID]; !ok {
			byID[t.Nodes[i].ID] = &t.Nodes[i]
		}
	}
	out := make(map[string]string, len(t.Nodes))
	var resolve func(id string, seen map[string]bool) (string, bool)
	resolve = func(id string, seen map[string]bool) (string, bool) {
		if p, ok := out[id]; ok {
			return p, true
		}
		n, ok := byID[id]
		if !ok || seen[id] {
			return "", false
		}
		seen[id] = true
		base := t.RootPath
		if n.ParentID != nil {
			pp, ok := resolve(*n.ParentID, seen)
			if !ok {
				return "", false
			}
			base = pp
		}
		p := base + "/" + n.Segment
		out[id] = p
		return p, true
	}
	for _, n := range t.Nodes {
		resolve(n.ID, map[string]bool{})
	}
	return out
}

// ParseJSON parses a stored or exported tree document.
func ParseJSON(data []byte) (*Tree, error) {
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("invalid tree document: %w", err)
	}
	if m == nil {
		return nil, fmt.Errorf("tree document must be an object")
	}
	return ParseMap(m, "")
}

func str(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case string:
		return x, true
	case json.Number:
		return x.String(), true
	case fmt.Stringer:
		return x.String(), true
	default:
		return fmt.Sprint(x), true
	}
}

// ParseMap parses a tree from a GraphQL input map or a decoded JSON object.
// A SUBTREE target written as "x/#" is normalized to "x".
func ParseMap(m map[string]any, revision string) (*Tree, error) {
	var list []any
	switch raw := m["nodes"].(type) {
	case nil:
	case []any:
		list = raw
	case []map[string]any:
		for _, x := range raw {
			list = append(list, x)
		}
	default:
		return nil, fmt.Errorf("nodes must be a list")
	}
	nodes := make([]Node, 0, len(list))
	for i, raw := range list {
		n, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("node %d must be an object", i)
		}
		id, ok := str(n["id"])
		if !ok {
			return nil, fmt.Errorf("node %d has no id", i)
		}
		node := Node{ID: id}
		if p, ok := str(n["parentId"]); ok {
			node.ParentID = &p
		}
		node.Segment, _ = str(n["segment"])
		node.Description, _ = str(n["description"])
		if rb, present := n["binding"]; present && rb != nil {
			b, ok := rb.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("binding of node %d must be an object", i)
			}
			kind, _ := str(b["kind"])
			target, _ := str(b["target"])
			access, ok := str(b["access"])
			if !ok {
				access = ReadOnly
			}
			if kind == Subtree && len(target) > 2 && strings.HasSuffix(target, "/#") {
				target = target[:len(target)-2]
			}
			node.Binding = &Binding{Kind: kind, Target: target, Access: access}
		}
		nodes = append(nodes, node)
	}
	t := &Tree{Nodes: nodes}
	switch sv := m["schemaVersion"].(type) {
	case nil:
		return nil, fmt.Errorf("schemaVersion is required")
	case int:
		t.SchemaVersion = sv
	case int64:
		t.SchemaVersion = int(sv)
	case float64:
		t.SchemaVersion = int(sv)
	default:
		s, _ := str(sv)
		v, err := strconv.Atoi(s)
		if err != nil {
			f, ferr := strconv.ParseFloat(s, 64)
			if ferr != nil {
				return nil, fmt.Errorf("schemaVersion is required")
			}
			v = int(f)
		}
		t.SchemaVersion = v
	}
	t.ID, _ = str(m["id"])
	t.Name, _ = str(m["name"])
	if t.Name == "" {
		t.Name = t.ID
	}
	t.RootPath, _ = str(m["rootPath"])
	t.Description, _ = str(m["description"])
	t.Enabled = true
	if e, ok := m["enabled"].(bool); ok {
		t.Enabled = e
	}
	t.Revision = revision
	if t.Revision == "" {
		t.Revision, _ = str(m["revision"])
	}
	return t, nil
}
