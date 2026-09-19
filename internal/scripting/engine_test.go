package scripting

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.starlark.net/starlark"
	_ "modernc.org/sqlite"
	"monstermq.io/edge/internal/stores"
	storesqlite "monstermq.io/edge/internal/stores/sqlite"
)

func TestEngineBasicExecution(t *testing.T) {
	script := `
def run():
    return 42
result = run()
`
	engine, err := NewEngine("test1", script)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	ctx := context.Background()
	execCtx := &ExecutionContext{
		ScriptName: "test1",
		Trigger:    "CALLABLE",
		State:      starlark.NewDict(0),
		Global:     NewGlobalStore(),
		LogBuffer:  NewCircularLogBuffer(10),
	}

	res := engine.Execute(ctx, execCtx, 1000)
	if !res.Success {
		t.Fatalf("execution failed: %v", res.Error)
	}
	if res.ReturnValue != int64(42) && res.ReturnValue != 42 {
		t.Fatalf("expected 42, got %v (%T)", res.ReturnValue, res.ReturnValue)
	}
}

func TestEngineMsgHandling(t *testing.T) {
	script := `
if msg != None:
    temp = msg["payload"]["temperature"]
    log.info("Temperature received: " + str(temp))
    if temp > 30:
        mqtt.publish("alert/temp", json.encode({"alert": True, "val": temp}), retain=True, qos=1)
`
	engine, err := NewEngine("msg_test", script)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	logBuf := NewCircularLogBuffer(10)
	published := []PublishedMessage{}
	publishFn := func(topic string, payload []byte, retain bool, qos byte) error {
		published = append(published, PublishedMessage{
			Topic:   topic,
			Payload: string(payload),
			Retain:  retain,
			QoS:     int(qos),
		})
		return nil
	}

	msg := &stores.BrokerMessage{
		TopicName: "sensors/temp",
		Payload:   []byte(`{"temperature": 35.5, "sensor": "A1"}`),
		Time:      time.Now(),
		QoS:       1,
		IsRetain:  false,
	}

	execCtx := &ExecutionContext{
		ScriptName: "msg_test",
		Trigger:    "TOPIC",
		Msg:        msg,
		State:      starlark.NewDict(0),
		Global:     NewGlobalStore(),
		PublishFn:  publishFn,
		LogBuffer:  logBuf,
	}

	res := engine.Execute(context.Background(), execCtx, 1000)
	if !res.Success {
		t.Fatalf("execution failed: %v", res.Error)
	}

	if len(published) != 1 {
		t.Fatalf("expected 1 published message, got %d", len(published))
	}
	if published[0].Topic != "alert/temp" {
		t.Errorf("expected topic alert/temp, got %s", published[0].Topic)
	}
	if !published[0].Retain || published[0].QoS != 1 {
		t.Errorf("expected retain=true, qos=1, got retain=%v, qos=%d", published[0].Retain, published[0].QoS)
	}
	if !strings.Contains(published[0].Payload, `"alert":true`) {
		t.Errorf("unexpected payload: %s", published[0].Payload)
	}

	logs := logBuf.Snapshot()
	if len(logs) != 1 || !strings.Contains(logs[0], "Temperature received: 35.5") {
		t.Errorf("unexpected logs: %v", logs)
	}
}

func TestEngineStateAndGlobal(t *testing.T) {
	script := `
count = state.get("count", 0) + 1
state["count"] = count

g_val = globals.get("counter", 100) + 10
globals.set("counter", g_val)

result = {"count": count, "global": g_val}
`
	engine, err := NewEngine("state_test", script)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	state := starlark.NewDict(0)
	global := NewGlobalStore()

	execCtx1 := &ExecutionContext{
		ScriptName: "state_test",
		Trigger:    "CALLABLE",
		State:      state,
		Global:     global,
		LogBuffer:  NewCircularLogBuffer(10),
	}
	res1 := engine.Execute(context.Background(), execCtx1, 1000)
	if !res1.Success {
		t.Fatalf("exec 1 failed: %v", res1.Error)
	}
	m1 := res1.ReturnValue.(map[string]any)
	if m1["count"] != int64(1) || m1["global"] != int64(110) {
		t.Fatalf("unexpected res1: %v", m1)
	}

	// Second execution preserves state & global
	execCtx2 := &ExecutionContext{
		ScriptName: "state_test",
		Trigger:    "CALLABLE",
		State:      state,
		Global:     global,
		LogBuffer:  NewCircularLogBuffer(10),
	}
	res2 := engine.Execute(context.Background(), execCtx2, 1000)
	if !res2.Success {
		t.Fatalf("exec 2 failed: %v", res2.Error)
	}
	m2 := res2.ReturnValue.(map[string]any)
	if m2["count"] != int64(2) || m2["global"] != int64(120) {
		t.Fatalf("unexpected res2: %v", m2)
	}
}

func TestEnginePersistentStorage(t *testing.T) {
	ctx := context.Background()
	sdb, err := storesqlite.OpenMemory("test-kv")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	defer sdb.Close()

	devStore := storesqlite.NewDeviceConfigStore(sdb)
	if err := devStore.EnsureTable(ctx); err != nil {
		t.Fatalf("ensure table: %v", err)
	}

	kvStore := NewScriptKVStore("storage_test", devStore, "local")

	script := `
val = storage.get("last_pos", 0)
storage.set("last_pos", val + 5)
result = val
`
	engine, err := NewEngine("storage_test", script)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	execCtx1 := &ExecutionContext{
		ScriptName: "storage_test",
		Trigger:    "CALLABLE",
		State:      starlark.NewDict(0),
		Global:     NewGlobalStore(),
		Storage:    kvStore,
		LogBuffer:  NewCircularLogBuffer(10),
	}

	res1 := engine.Execute(ctx, execCtx1, 1000)
	if !res1.Success {
		t.Fatalf("exec 1 failed: %v", res1.Error)
	}
	if res1.ReturnValue != int64(0) && res1.ReturnValue != 0 {
		t.Fatalf("expected 0, got %v", res1.ReturnValue)
	}

	// Verify persistence in KV store
	v := kvStore.Get("last_pos", nil)
	if v != float64(5) && v != int64(5) {
		t.Fatalf("expected 5 in KV store, got %v (%T)", v, v)
	}

	// Reload a new KVStore instance from the same DB backend to verify persistence
	kvStore2 := NewScriptKVStore("storage_test", devStore, "local")
	if err := kvStore2.Load(ctx); err != nil {
		t.Fatalf("reload KV store: %v", err)
	}

	execCtx2 := &ExecutionContext{
		ScriptName: "storage_test",
		Trigger:    "CALLABLE",
		State:      starlark.NewDict(0),
		Global:     NewGlobalStore(),
		Storage:    kvStore2,
		LogBuffer:  NewCircularLogBuffer(10),
	}
	res2 := engine.Execute(ctx, execCtx2, 1000)
	if !res2.Success {
		t.Fatalf("exec 2 failed: %v", res2.Error)
	}
	if res2.ReturnValue != float64(5) && res2.ReturnValue != int64(5) {
		t.Fatalf("expected 5, got %v (%T)", res2.ReturnValue, res2.ReturnValue)
	}
}

func TestEngineScriptCalling(t *testing.T) {
	script := `
out = scripts.call("HelperScript", {"factor": 3, "val": 10})
result = out
`
	engine, err := NewEngine("caller_test", script)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	callFn := func(name string, args map[string]any) (any, error) {
		if name != "HelperScript" {
			t.Fatalf("expected HelperScript, got %s", name)
		}
		factor := int64(args["factor"].(int64))
		val := int64(args["val"].(int64))
		return factor * val, nil
	}

	execCtx := &ExecutionContext{
		ScriptName: "caller_test",
		Trigger:    "CALLABLE",
		State:      starlark.NewDict(0),
		Global:     NewGlobalStore(),
		CallFn:     callFn,
		LogBuffer:  NewCircularLogBuffer(10),
	}

	res := engine.Execute(context.Background(), execCtx, 1000)
	if !res.Success {
		t.Fatalf("execution failed: %v", res.Error)
	}
	if res.ReturnValue != int64(30) {
		t.Fatalf("expected 30, got %v", res.ReturnValue)
	}
}

func TestEngineJSONHelpers(t *testing.T) {
	script := `
raw = '{"name": "Alice", "tags": [1, 2, 3]}'
obj = json.decode(raw)
obj["tags"].append(4)
encoded = json.encode(obj)
result = encoded
`
	engine, err := NewEngine("json_test", script)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	execCtx := &ExecutionContext{
		ScriptName: "json_test",
		Trigger:    "CALLABLE",
		State:      starlark.NewDict(0),
		Global:     NewGlobalStore(),
		LogBuffer:  NewCircularLogBuffer(10),
	}

	res := engine.Execute(context.Background(), execCtx, 1000)
	if !res.Success {
		t.Fatalf("execution failed: %v", res.Error)
	}
	resStr := res.ReturnValue.(string)
	if !strings.Contains(resStr, `"name":"Alice"`) || !strings.Contains(resStr, `[1,2,3,4]`) {
		t.Fatalf("unexpected JSON: %s", resStr)
	}
}

func TestEngineInfiniteLoopProtection(t *testing.T) {
	// Starlark does not have unbounded 'while True' by default, but recursion / large iteration step limits are guarded
	script := `
def run():
    x = 0
    for i in range(1000000):
        x += 1
    return x
run()
`
	engine, err := NewEngine("loop_test", script)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	execCtx := &ExecutionContext{
		ScriptName: "loop_test",
		Trigger:    "CALLABLE",
		State:      starlark.NewDict(0),
		Global:     NewGlobalStore(),
		LogBuffer:  NewCircularLogBuffer(10),
	}

	// Limit steps to 1000
	engine.SetMaxExecutionSteps(1000)
	res := engine.Execute(context.Background(), execCtx, 500)
	if res.Success {
		t.Fatalf("expected step limit error, but succeeded")
	}
	if !strings.Contains(res.Error.Error(), "too many steps") {
		t.Fatalf("expected 'too many steps' error, got: %v", res.Error)
	}
}

func TestEngineDynamicSubscription(t *testing.T) {
	script := `
def on_press(m):
    log.info("Pressure: " + str(m["payload"]["bar"]))

mqtt.subscribe("factory/pressure", on_press)
`
	engine, err := NewEngine("sub_test", script)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	registeredFilter := ""
	var registeredCb starlark.Callable

	subRegFn := func(filter string, cb starlark.Callable) {
		registeredFilter = filter
		registeredCb = cb
	}

	execCtx := &ExecutionContext{
		ScriptName: "sub_test",
		Trigger:    "CALLABLE",
		State:      starlark.NewDict(0),
		Global:     NewGlobalStore(),
		SubRegFn:   subRegFn,
		LogBuffer:  NewCircularLogBuffer(10),
	}

	res := engine.Execute(context.Background(), execCtx, 1000)
	if !res.Success {
		t.Fatalf("execution failed: %v", res.Error)
	}

	if registeredFilter != "factory/pressure" {
		t.Fatalf("expected filter factory/pressure, got %s", registeredFilter)
	}
	if registeredCb == nil {
		t.Fatalf("expected registered callback callable")
	}
}

func TestEngineDatabase(t *testing.T) {
	ctx := context.Background()
	sdb, err := storesqlite.OpenMemory("test-db-engine")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	defer sdb.Close()

	dbMgr := NewDatabaseManager(nil, sdb, nil)
	defer dbMgr.Close()

	// 1. Create table and insert data via db.execute
	scriptSetup := `
db.execute("CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, age INTEGER)")
db.execute("INSERT INTO users (name, age) VALUES (?, ?)", ["Bob", 30])
db.execute("INSERT INTO users (name, age) VALUES (?, ?)", ["Carol", 25])
`
	engSetup, err := NewEngine("db_setup", scriptSetup)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	execCtx1 := &ExecutionContext{
		ScriptName: "db_setup",
		Trigger:    "CALLABLE",
		State:      starlark.NewDict(0),
		Global:     NewGlobalStore(),
		DB:         dbMgr,
		LogBuffer:  NewCircularLogBuffer(10),
	}
	res1 := engSetup.Execute(ctx, execCtx1, 1000)
	if !res1.Success {
		t.Fatalf("setup failed: %v", res1.Error)
	}

	// 2. Query data via db.query
	scriptQuery := `
rows = db.query("SELECT name, age FROM users ORDER BY age DESC")
result = rows
`
	engQuery, err := NewEngine("db_query", scriptQuery)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	execCtx2 := &ExecutionContext{
		ScriptName: "db_query",
		Trigger:    "CALLABLE",
		State:      starlark.NewDict(0),
		Global:     NewGlobalStore(),
		DB:         dbMgr,
		LogBuffer:  NewCircularLogBuffer(10),
	}
	res2 := engQuery.Execute(ctx, execCtx2, 1000)
	if !res2.Success {
		t.Fatalf("query failed: %v", res2.Error)
	}

	rows, ok := res2.ReturnValue.([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %v (%T)", res2.ReturnValue, res2.ReturnValue)
	}
	r0 := rows[0].(map[string]any)
	if r0["name"] != "Bob" || r0["age"] != int64(30) {
		t.Fatalf("unexpected row 0: %v", r0)
	}
}

func TestEngineArchiveLastValue(t *testing.T) {
	ctx := context.Background()
	sdb, err := storesqlite.OpenMemory("test-archive-engine")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	defer sdb.Close()

	msgStore := storesqlite.NewMessageStore("lastval_test", sdb)
	if err := msgStore.EnsureTable(ctx); err != nil {
		t.Fatalf("ensure table: %v", err)
	}

	_ = msgStore.AddAll(ctx, []stores.BrokerMessage{
		{
			TopicName: "sensors/boiler",
			Payload:   []byte(`{"status": "OK", "bar": 4.2}`),
			Time:      time.Now(),
			QoS:       0,
			IsRetain:  true,
		},
	})

	script := `
val = archive.get_last_value("sensors/boiler")
result = val
`
	engine, err := NewEngine("archive_test", script)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	execCtx := &ExecutionContext{
		ScriptName: "archive_test",
		Trigger:    "CALLABLE",
		State:      starlark.NewDict(0),
		Global:     NewGlobalStore(),
		Messages:   msgStore,
		LogBuffer:  NewCircularLogBuffer(10),
	}

	res := engine.Execute(ctx, execCtx, 1000)
	if !res.Success {
		t.Fatalf("archive read failed: %v", res.Error)
	}

	ret, ok := res.ReturnValue.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T: %v", res.ReturnValue, res.ReturnValue)
	}
	if ret["topic"] != "sensors/boiler" {
		t.Fatalf("unexpected topic: %v", ret["topic"])
	}
	valObj := ret["value"].(map[string]any)
	if valObj["status"] != "OK" || valObj["bar"] != 4.2 {
		t.Fatalf("unexpected parsed value: %v", valObj)
	}
}

func TestEngineIsInstance(t *testing.T) {
	script := `
d = {"a": 1}
s = "hello"
n = 123
res1 = isinstance(d, dict)
res2 = isinstance(d, "dict")
res3 = isinstance(s, str)
res4 = isinstance(s, "string")
res5 = isinstance(n, (int, float))
res6 = isinstance(n, dict)
res7 = isinstance(None, dict)

result = [res1, res2, res3, res4, res5, res6, res7]
`
	engine, err := NewEngine("isinstance_test", script)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	ctx := context.Background()
	execCtx := &ExecutionContext{
		ScriptName: "isinstance_test",
		Trigger:    "CALLABLE",
		State:      starlark.NewDict(0),
		Global:     NewGlobalStore(),
		LogBuffer:  NewCircularLogBuffer(10),
	}

	res := engine.Execute(ctx, execCtx, 1000)
	if !res.Success {
		t.Fatalf("execution failed: %v", res.Error)
	}

	results, ok := res.ReturnValue.([]any)
	if !ok {
		t.Fatalf("expected []any, got %T: %v", res.ReturnValue, res.ReturnValue)
	}

	expected := []bool{true, true, true, true, true, false, false}
	for i, exp := range expected {
		if results[i] != exp {
			t.Errorf("res%d: expected %v, got %v", i+1, exp, results[i])
		}
	}
}

