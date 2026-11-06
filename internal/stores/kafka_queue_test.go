package stores

import "testing"

func TestSanitizeKafkaSuffix(t *testing.T) {
	cases := map[string]string{
		"sensors/temp": "sensors_temp",
		"Sensors.Temp": "sensors_temp",
		"a--b__c":      "a_b_c",
		"_x_":          "x",
		"":             "default",
		"///":          "default",
		"plain":        "plain",
		"MixedCase123": "mixedcase123",
	}
	for in, want := range cases {
		if got := SanitizeKafkaSuffix(in); got != want {
			t.Errorf("SanitizeKafkaSuffix(%q) = %q, want %q", in, got, want)
		}
	}
	q, o := KafkaQueueNames("a/b")
	if q != "kafka_queue_a_b" || o != "kafka_offsets_a_b" {
		t.Errorf("names = %s %s", q, o)
	}
	if q, o := KafkaQueueNames(""); q != "kafka_queue" || o != "kafka_offsets" {
		t.Errorf("unsuffixed names = %s %s", q, o)
	}
}
