package winccoanative

import (
	"sort"
	"strings"
	"testing"
)

func TestTopicTree(t *testing.T) {
	tr := newTopicTree()
	for _, topic := range []string{"1/2/3", "1/x/3", "1/2/3/4", "1/2", "1", "a//b", "2/2/3"} {
		if !tr.Put(topic, "dp:"+topic) {
			t.Fatalf("%s not new", topic)
		}
	}
	if tr.Put("1/2/3", "dp:1/2/3") || tr.size != 7 {
		t.Fatalf("repeated put, size %d", tr.size)
	}
	for filter, want := range map[string]string{
		"1/+/3":   "1/2/3 1/x/3",
		"1/2/3":   "1/2/3",
		"+/2/3":   "1/2/3 2/2/3",
		"1/#":     "1 1/2 1/2/3 1/2/3/4 1/x/3",
		"1/2/#":   "1/2 1/2/3 1/2/3/4",
		"#":       "1 1/2 1/2/3 1/2/3/4 1/x/3 2/2/3 a//b",
		"+":       "1",
		"a/+/b":   "a//b",
		"1/+/+/+": "1/2/3/4",
		"9/#":     "",
	} {
		var got []string
		tr.Match(filter, func(topic, dp string) {
			if dp != "dp:"+topic {
				t.Errorf("%s: dp %q for %q", filter, dp, topic)
			}
			got = append(got, topic)
		})
		sort.Strings(got)
		if g := strings.Join(got, " "); g != want {
			t.Errorf("%s: got %q want %q", filter, g, want)
		}
		for _, topic := range got {
			if !TopicMatches(filter, topic) {
				t.Errorf("%s: %s does not match", filter, topic)
			}
		}
	}
}

func TestTopicDPName(t *testing.T) {
	for topic, want := range map[string]string{
		"plant/line-1/temp_c": "MMQTopic_plant/line-1/temp_c",
		"/x//y/":              "MMQTopic_/x//y/",
		"ä $.:%":              "MMQTopic_ä%20%24%2E%3A%25",
		"a,b;c*d?e[f]g{h}i@j": "MMQTopic_a%2Cb%3Bc%2Ad%3Fe%5Bf%5Dg%7Bh%7Di%40j",
		"q\"r'\\\x01\x7F":     "MMQTopic_q%22r%27%5C%01%7F",
	} {
		if got := TopicDPName(topic); got != want {
			t.Errorf("%q: got %q want %q", topic, got, want)
		}
	}
	seen := map[string]string{}
	for _, topic := range []string{"a b", "a%20b", "a%b", "a%25b", "a/b", "a_b", "a.b", "a%2Eb"} {
		n := TopicDPName(topic)
		if o, dup := seen[n]; dup {
			t.Errorf("%q and %q both map to %q", o, topic, n)
		}
		seen[n] = topic
		if strings.ContainsAny(n, " .:,;*?[]{}$@\"'\\") {
			t.Errorf("%q: forbidden character in %q", topic, n)
		}
	}
	long := strings.Repeat("x", maxTopicDPName)
	if got := TopicDPName(long); got != TopicDP(long) {
		t.Errorf("long topic not hashed: %q", got)
	}
	if fit := strings.Repeat("x", maxTopicDPName-len("MMQTopic_")); TopicDPName(fit) != "MMQTopic_"+fit {
		t.Error("topic at the limit hashed")
	}
}
