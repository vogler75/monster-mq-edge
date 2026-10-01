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
