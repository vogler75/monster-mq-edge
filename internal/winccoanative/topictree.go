package winccoanative

import "strings"

// topicTree indexes the topics of one system by level, so a wildcard
// filter only walks the branches it can match.
type topicTree struct {
	root *topicNode
	size int
}

type topicNode struct {
	children map[string]*topicNode
	dp       string // Sys:MMQTopic_k... when a topic ends here
}

func newTopicTree() *topicTree { return &topicTree{root: &topicNode{}} }

// Put stores the datapoint of a topic and reports whether it is new.
func (t *topicTree) Put(topic, dp string) bool {
	n := t.root
	for _, l := range strings.Split(topic, "/") {
		c := n.children[l]
		if c == nil {
			if n.children == nil {
				n.children = map[string]*topicNode{}
			}
			c = &topicNode{}
			n.children[l] = c
		}
		n = c
	}
	added := n.dp == ""
	if added {
		t.size++
	}
	n.dp = dp
	return added
}

// Match calls fn for every stored topic that matches the MQTT filter.
func (t *topicTree) Match(filter string, fn func(topic, dp string)) {
	levels := strings.Split(filter, "/")
	path := make([]string, 0, len(levels))
	var walk func(n *topicNode, i int)
	walk = func(n *topicNode, i int) {
		if i == len(levels) {
			if n.dp != "" {
				fn(strings.Join(path, "/"), n.dp)
			}
			return
		}
		switch l := levels[i]; l {
		case "#":
			// '#' also matches the parent level itself.
			if n != t.root && n.dp != "" {
				fn(strings.Join(path, "/"), n.dp)
			}
			t.all(n, &path, fn)
		case "+":
			for name, c := range n.children {
				path = append(path, name)
				walk(c, i+1)
				path = path[:len(path)-1]
			}
		default:
			if c := n.children[l]; c != nil {
				path = append(path, l)
				walk(c, i+1)
				path = path[:len(path)-1]
			}
		}
	}
	walk(t.root, 0)
}

// all calls fn for every topic below n.
func (t *topicTree) all(n *topicNode, path *[]string, fn func(topic, dp string)) {
	for name, c := range n.children {
		*path = append(*path, name)
		if c.dp != "" {
			fn(strings.Join(*path, "/"), c.dp)
		}
		t.all(c, path, fn)
		*path = (*path)[:len(*path)-1]
	}
}
