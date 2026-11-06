package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/stores"
)

const kafkaServerAddMutation = `mutation($input: KafkaServerInput!) {
  kafkaServer { add(input: $input) { success errors server { name status port advertisedHost advertisedPort streams { streamName topicFilter retentionHours allowWrite } } } }
}`

func kafkaServerInput(port int) map[string]any {
	return map[string]any{
		"name": "ks", "namespace": "ks", "nodeId": "*", "enabled": true,
		"host": "127.0.0.1", "port": port, "advertisedHost": "127.0.0.1", "advertisedPort": port,
		"streams": []map[string]any{
			{"streamName": "sensors", "topicFilter": "sensors/#", "retentionHours": 168, "allowWrite": true},
			{"streamName": "readonly", "topicFilter": "ro/#", "allowWrite": false},
		},
	}
}

func kafkaServerStatus(t *testing.T, url string) string {
	t.Helper()
	data := gqlQuery(t, url, `{ kafkaServer(name:"ks") { status } }`, nil)
	srv, _ := data["kafkaServer"].(map[string]any)
	if srv == nil {
		return ""
	}
	return srv["status"].(string)
}

func waitKafkaStatus(t *testing.T, url, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for kafkaServerStatus(t, url) != want {
		if time.Now().After(deadline) {
			t.Fatalf("kafka server status %q, want %q", kafkaServerStatus(t, url), want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestKafkaServerEndToEnd(t *testing.T) {
	kafkaPort := getFreePort(t)
	srv, url := startWithGraphQL(t, getFreePort(t), getFreePort(t), func(c *config.Config) {
		c.Features.KafkaServer = true
	})
	t.Cleanup(func() { _ = srv.Close() })

	if feats := gqlQuery(t, url, `{ broker { enabledFeatures } }`, nil); fmt.Sprint(feats) == "" {
		t.Fatal("no features")
	}

	// Create through GraphQL; the server starts right away.
	data := gqlQuery(t, url, kafkaServerAddMutation, map[string]any{"input": kafkaServerInput(kafkaPort)})
	res := data["kafkaServer"].(map[string]any)["add"].(map[string]any)
	if res["success"] != true {
		t.Fatalf("add failed: %v", res["errors"])
	}
	info := res["server"].(map[string]any)
	if info["status"] != "RUNNING" || info["advertisedHost"] != "127.0.0.1" {
		t.Fatalf("unexpected server info: %v", info)
	}
	waitForPort(t, kafkaPort)

	// A duplicate is rejected like Core does.
	dup := gqlQuery(t, url, kafkaServerAddMutation, map[string]any{"input": kafkaServerInput(kafkaPort)})
	if dup["kafkaServer"].(map[string]any)["add"].(map[string]any)["success"] != false {
		t.Fatal("duplicate add must fail")
	}

	obs := observe(t, srv, "sensors/#")
	broker := fmt.Sprintf("127.0.0.1:%d", kafkaPort)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Kafka producer -> MQTT subscriber (record key = MQTT topic).
	prod, err := kgo.NewClient(kgo.SeedBrokers(broker), kgo.ClientID("it-producer"))
	if err != nil {
		t.Fatal(err)
	}
	defer prod.Close()
	if err := prod.ProduceSync(ctx, &kgo.Record{Topic: "sensors", Key: []byte("sensors/room1/temp"), Value: []byte("21.5")}).FirstErr(); err != nil {
		t.Fatalf("produce: %v", err)
	}
	got := obs.waitFor(t, "sensors/room1/temp", 1, 300*time.Millisecond)
	if len(got) != 1 || string(got[0].Payload) != "21.5" {
		t.Fatalf("mqtt side saw %d messages (first %q)", len(got), got[0].Payload)
	}

	// A stream with allowWrite=false rejects produce.
	if err := prod.ProduceSync(ctx, &kgo.Record{Topic: "readonly", Key: []byte("ro/x"), Value: []byte("1")}).FirstErr(); err == nil {
		t.Fatal("produce to a read-only stream must fail")
	}

	// MQTT publish -> Kafka consumer, key = MQTT topic.
	publishAt(t, srv, "it-mqtt", stores.BrokerMessage{TopicName: "sensors/room2/temp", Payload: []byte("19"), QoS: 1})
	cons, err := kgo.NewClient(kgo.SeedBrokers(broker), kgo.ConsumeTopics("sensors"), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer cons.Close()
	seen := map[string]string{}
	deadline := time.Now().Add(20 * time.Second)
	for len(seen) < 2 && time.Now().Before(deadline) {
		pctx, pcancel := context.WithTimeout(ctx, 3*time.Second)
		fetches := cons.PollFetches(pctx)
		pcancel()
		fetches.EachRecord(func(r *kgo.Record) { seen[string(r.Key)] = string(r.Value) })
	}
	if seen["sensors/room1/temp"] != "21.5" || seen["sensors/room2/temp"] != "19" {
		t.Fatalf("consumer saw %v", seen)
	}

	// Stored exactly once although the produced record was also published to MQTT.
	q := gqlQuery(t, url, `{ kafkaTopicOffsets(serverName:"ks", topic:"sensors") { earliestOffset latestOffset }
	  kafkaMessages(serverName:"ks", topic:"sensors", limit: 10) { offset topic key value } }`, nil)
	msgs := q["kafkaMessages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("kafkaMessages returned %d, want 2: %v", len(msgs), msgs)
	}
	if q["kafkaTopicOffsets"] == nil {
		t.Fatal("kafkaTopicOffsets is null")
	}

	// Commit an offset, restart the server through toggle, offset survives.
	commit := kmsg.NewPtrOffsetCommitRequest()
	commit.Group = "it-group"
	commit.Version = 2
	ct := kmsg.NewOffsetCommitRequestTopic()
	ct.Topic = "sensors"
	cp := kmsg.NewOffsetCommitRequestTopicPartition()
	cp.Partition = 0
	cp.Offset = 2
	ct.Partitions = append(ct.Partitions, cp)
	commit.Topics = append(commit.Topics, ct)
	if _, err := commit.RequestWith(ctx, prod); err != nil {
		t.Fatalf("offset commit: %v", err)
	}
	groups := gqlQuery(t, url, `{ kafkaConsumerGroups { groupId topics lag } }`, nil)["kafkaConsumerGroups"].([]any)
	if len(groups) != 1 || groups[0].(map[string]any)["groupId"] != "it-group" {
		t.Fatalf("consumer groups: %v", groups)
	}

	toggle := func(enabled bool) {
		r := gqlQuery(t, url, `mutation($e: Boolean!) { kafkaServer { toggle(name:"ks", enabled:$e) { success errors } } }`, map[string]any{"e": enabled})
		if r["kafkaServer"].(map[string]any)["toggle"].(map[string]any)["success"] != true {
			t.Fatalf("toggle(%v) failed: %v", enabled, r)
		}
	}
	toggle(false)
	waitKafkaStatus(t, url, "STOPPED")
	toggle(true)
	waitKafkaStatus(t, url, "RUNNING")
	waitForPort(t, kafkaPort)

	prod2, err := kgo.NewClient(kgo.SeedBrokers(broker))
	if err != nil {
		t.Fatal(err)
	}
	defer prod2.Close()
	fetch := kmsg.NewPtrOffsetFetchRequest()
	fetch.Version = 1
	fetch.Group = "it-group"
	ft := kmsg.NewOffsetFetchRequestTopic()
	ft.Topic = "sensors"
	ft.Partitions = []int32{0}
	fetch.Topics = append(fetch.Topics, ft)
	resp, err := fetch.RequestWith(ctx, prod2)
	if err != nil {
		t.Fatalf("offset fetch: %v", err)
	}
	if len(resp.Topics) != 1 || len(resp.Topics[0].Partitions) != 1 || resp.Topics[0].Partitions[0].Offset != 2 {
		t.Fatalf("committed offset after restart: %+v", resp.Topics)
	}

	// Delete group and server.
	del := gqlQuery(t, url, `mutation { kafkaServer { deleteConsumerGroup(groupId:"it-group") } }`, nil)
	if del["kafkaServer"].(map[string]any)["deleteConsumerGroup"] != true {
		t.Fatal("deleteConsumerGroup returned false")
	}
	rm := gqlQuery(t, url, `mutation { kafkaServer { delete(name:"ks") } }`, nil)
	if rm["kafkaServer"].(map[string]any)["delete"] != true {
		t.Fatal("delete returned false")
	}
	if s := gqlQuery(t, url, `{ kafkaServers { name } }`, nil)["kafkaServers"].([]any); len(s) != 0 {
		t.Fatalf("servers after delete: %v", s)
	}
}

// With the feature off the resolvers answer with empty results and the
// documented error, and the feature is not reported.
func TestKafkaServerFeatureDisabled(t *testing.T) {
	srv, url := startWithGraphQL(t, getFreePort(t), getFreePort(t))
	t.Cleanup(func() { _ = srv.Close() })
	data := gqlQuery(t, url, `{ kafkaServers { name } kafkaConsumerGroups { groupId } }`, nil)
	if len(data["kafkaServers"].([]any)) != 0 || len(data["kafkaConsumerGroups"].([]any)) != 0 {
		t.Fatalf("expected empty results: %v", data)
	}
	data = gqlQuery(t, url, kafkaServerAddMutation, map[string]any{"input": kafkaServerInput(getFreePort(t))})
	res := data["kafkaServer"].(map[string]any)["add"].(map[string]any)
	errs, _ := res["errors"].([]any)
	if res["success"] != false || len(errs) != 1 || errs[0] != "KafkaServer feature is not enabled on this node" {
		t.Fatalf("unexpected result: %v", res)
	}
}
