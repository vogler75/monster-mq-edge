package scripting

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"monstermq.io/edge/internal/pubsub"
	"monstermq.io/edge/internal/stores"
)

func TestConnectorTopicTrigger(t *testing.T) {
	bus := pubsub.NewBus()
	var published atomic.Int64

	publishFn := func(topic string, payload []byte, retain bool, qos byte) error {
		published.Add(1)
		return nil
	}

	script := `
if msg != None:
    mqtt.publish("output/" + msg["topic"], "ack: " + str(msg["payload"]))
`
	engine, err := NewEngine("topic_conn_test", script)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	cfg := ScriptConfig{
		Language:     "starlark",
		Script:       script,
		TriggerType:  TriggerTypeTopic,
		TopicFilters: []string{"sensors/+", "alerts/#"},
		InstanceMode: InstanceModeSingleton,
		TimeoutMs:    500,
	}

	conn := NewConnector(
		"topic_conn_test",
		cfg,
		engine,
		nil,
		NewGlobalStore(),
		nil,
		nil,
		nil,
		publishFn,
		bus,
		nil,
		"test-node",
		slog.New(slog.DiscardHandler),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := conn.Start(ctx); err != nil {
		t.Fatalf("connector start: %v", err)
	}
	defer conn.Stop()

	// 1. Publish matching sensor/+
	bus.Publish(stores.BrokerMessage{
		TopicName: "sensors/temperature",
		Payload:   []byte("22.5"),
		Time:      time.Now(),
	})

	// 2. Publish matching alerts/#
	bus.Publish(stores.BrokerMessage{
		TopicName: "alerts/fire/zone1",
		Payload:   []byte("CRITICAL"),
		Time:      time.Now(),
	})

	// 3. Publish non-matching topic
	bus.Publish(stores.BrokerMessage{
		TopicName: "unrelated/status",
		Payload:   []byte("OK"),
		Time:      time.Now(),
	})

	// Allow worker to process
	time.Sleep(100 * time.Millisecond)

	if got := published.Load(); got != 2 {
		t.Fatalf("expected 2 published messages from matching topics, got %d", got)
	}

	execCount, errCount, lastTime, lastStatus, _ := conn.Stats()
	if execCount != 2 {
		t.Fatalf("expected 2 executions, got %d", execCount)
	}
	if errCount != 0 {
		t.Fatalf("expected 0 errors, got %d", errCount)
	}
	if lastTime == "" || lastStatus != "SUCCESS" {
		t.Fatalf("expected lastStatus SUCCESS, got %s (time: %s)", lastStatus, lastTime)
	}
}

func TestConnectorTriggerOnChangeOnly(t *testing.T) {
	bus := pubsub.NewBus()
	var execTotal atomic.Int64

	publishFn := func(topic string, payload []byte, retain bool, qos byte) error {
		execTotal.Add(1)
		return nil
	}

	script := `
mqtt.publish("status", "changed")
`
	engine, err := NewEngine("change_conn_test", script)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	cfg := ScriptConfig{
		Language:            "starlark",
		Script:              script,
		TriggerType:         TriggerTypeTopic,
		TopicFilters:        []string{"device/value"},
		TriggerOnChangeOnly: true,
		InstanceMode:        InstanceModeSingleton,
		TimeoutMs:           500,
	}

	conn := NewConnector(
		"change_conn_test",
		cfg,
		engine,
		nil,
		NewGlobalStore(),
		nil,
		nil,
		nil,
		publishFn,
		bus,
		nil,
		"test-node",
		slog.New(slog.DiscardHandler),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := conn.Start(ctx); err != nil {
		t.Fatalf("connector start: %v", err)
	}
	defer conn.Stop()

	// 1. First publish "100" -> should trigger
	bus.Publish(stores.BrokerMessage{
		TopicName: "device/value",
		Payload:   []byte("100"),
		Time:      time.Now(),
	})
	time.Sleep(50 * time.Millisecond)

	// 2. Second publish identical "100" -> should NOT trigger
	bus.Publish(stores.BrokerMessage{
		TopicName: "device/value",
		Payload:   []byte("100"),
		Time:      time.Now(),
	})
	time.Sleep(50 * time.Millisecond)

	// 3. Third publish "200" -> should trigger
	bus.Publish(stores.BrokerMessage{
		TopicName: "device/value",
		Payload:   []byte("200"),
		Time:      time.Now(),
	})
	time.Sleep(50 * time.Millisecond)

	if got := execTotal.Load(); got != 2 {
		t.Fatalf("expected 2 executions with triggerOnChangeOnly, got %d", got)
	}
}

func TestConnectorTimerTrigger(t *testing.T) {
	var timerTicks atomic.Int64
	publishFn := func(topic string, payload []byte, retain bool, qos byte) error {
		timerTicks.Add(1)
		return nil
	}

	script := `
mqtt.publish("heartbeat", "tick")
`
	engine, err := NewEngine("timer_conn_test", script)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	cfg := ScriptConfig{
		Language:        "starlark",
		Script:          script,
		TriggerType:     TriggerTypeTimer,
		TimerIntervalMs: 50, // fast interval for test
		InstanceMode:    InstanceModeSingleton,
		TimeoutMs:       500,
	}

	conn := NewConnector(
		"timer_conn_test",
		cfg,
		engine,
		nil,
		NewGlobalStore(),
		nil,
		nil,
		nil,
		publishFn,
		nil,
		nil,
		"test-node",
		slog.New(slog.DiscardHandler),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := conn.Start(ctx); err != nil {
		t.Fatalf("connector start: %v", err)
	}

	// Let ticker fire ~3-4 times
	time.Sleep(180 * time.Millisecond)
	conn.Stop()

	ticks := timerTicks.Load()
	if ticks < 2 {
		t.Fatalf("expected at least 2 timer ticks, got %d", ticks)
	}
}

func TestConnectorDynamicSubscriptionCallback(t *testing.T) {
	bus := pubsub.NewBus()
	var callbackExecs atomic.Int64

	publishFn := func(topic string, payload []byte, retain bool, qos byte) error {
		if topic == "dyn/output" {
			callbackExecs.Add(1)
		}
		return nil
	}

	script := `
def on_message(msg):
    mqtt.publish("dyn/output", "received:" + str(msg["payload"]))

mqtt.subscribe("dyn/input/#", on_message)
`
	engine, err := NewEngine("dyn_conn_test", script)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	cfg := ScriptConfig{
		Language:     "starlark",
		Script:       script,
		TriggerType:  TriggerTypeCallable,
		InstanceMode: InstanceModeSingleton,
		TimeoutMs:    500,
	}

	conn := NewConnector(
		"dyn_conn_test",
		cfg,
		engine,
		nil,
		NewGlobalStore(),
		nil,
		nil,
		nil,
		publishFn,
		bus,
		nil,
		"test-node",
		slog.New(slog.DiscardHandler),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := conn.Start(ctx); err != nil {
		t.Fatalf("connector start: %v", err)
	}
	defer conn.Stop()

	// Initial script execution to register dynamic subscription
	_, err = conn.Call(nil)
	if err != nil {
		t.Fatalf("call error: %v", err)
	}

	// Publish message to matching dynamic filter
	bus.Publish(stores.BrokerMessage{
		TopicName: "dyn/input/sensor1",
		Payload:   []byte("test_payload"),
		Time:      time.Now(),
	})

	time.Sleep(100 * time.Millisecond)

	if got := callbackExecs.Load(); got != 1 {
		t.Fatalf("expected 1 callback execution, got %d", got)
	}
}
