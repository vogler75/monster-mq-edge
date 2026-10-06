package packets

import (
	"reflect"
	"testing"
)

func TestSubscriptionIdentifiers(t *testing.T) {
	cases := []struct {
		name string
		sub  Subscription
		want []int
	}{
		{"none", Subscription{Filter: "a"}, nil},
		{"own identifier, not merged", Subscription{Filter: "a", Identifier: 7}, []int{7}},
		{"merged, zero skipped, sorted", Subscription{Filter: "a/+", Identifier: 9, Identifiers: map[string]int{"a/+": 9, "a/#": 0, "a/b": 3}}, []int{3, 9}},
		{"merged without identifiers", Subscription{Filter: "a", Identifiers: map[string]int{"a": 0}}, nil},
	}
	for _, c := range cases {
		if got := c.sub.SubscriptionIdentifiers(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
