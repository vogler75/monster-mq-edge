package pubsub

import "testing"

func TestMatchTopicDollarSys(t *testing.T) {
	cases := []struct {
		filter, topic string
		want          bool
	}{
		{"#", "$SYS/broker/load", false},
		{"+/broker/load", "$SYS/broker/load", false},
		{"$SYS/#", "$SYS/broker/load", true},
		{"#", "sensor/temp", true},
	}
	for _, c := range cases {
		if got := matchTopic(c.filter, c.topic); got != c.want {
			t.Errorf("matchTopic(%q,%q)=%v want %v", c.filter, c.topic, got, c.want)
		}
	}
}
