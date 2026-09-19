# Plan: Standalone Script Device with Python/Starlark for MonsterMQ Edge

This plan details the implementation of a dedicated **Script DeviceConfig type** (`"Script"`) for **MonsterMQ Edge** (`monster-mq-edge`), using **Starlark** (Python dialect) for pure-Go, sandboxed execution.

Per requirements, this replaces visual workflows with a **clean, flat script-only design**, featuring:
- **Multiple topic subscriptions** with full MQTT wildcard support (`+`, `#`).
- **Interval timer** support.
- **Rich broker APIs**: Database execution/queries, Archive history & last values, dynamic topic subscriptions with callbacks, structured logging.
- **Scoped memory & persistent storage** (surviving restarts, Node-RED style).
- **Inter-script calls** (`scripts.call`) with argument passing and return values.
- **Instance concurrency flag** (`instanceMode`: `"SINGLETON"` vs `"MULTI_INSTANCE"`).

---

## Architecture & Data Model

### 1. New DeviceConfig Type: `"Script"`

Stored directly in the existing `deviceconfigs` table (`stores.DeviceConfig`) across SQLite, PostgreSQL, and MongoDB without database schema migrations:

* **`name`**: Unique script identifier (e.g. `"UPSMonitor"`, `"TemperatureEnricher"`).
* **`namespace`**: Logical namespace/grouping (e.g. `"default"`, `"ups"`).
* **`nodeId`**: Target cluster/edge node (e.g. `"local"` or `cfg.NodeID`).
* **`type`**: **`"Script"`** (constant `DeviceTypeScript = "Script"` in Go, `DEVICE_TYPE_SCRIPT = "Script"` in Kotlin).
* **`enabled`**: `true` / `false`.
* **`config`**: Raw JSON representing `ScriptConfig`.

### 2. The `ScriptConfig` JSON Structure

```json
{
  "language": "starlark",
  "triggerType": "TOPIC",
  "topicFilters": [
    "sensors/+/temperature",
    "ups/+/status"
  ],
  "triggerOnChangeOnly": false,
  "timerIntervalMs": 0,
  "instanceMode": "SINGLETON",
  "timeoutMs": 200,
  "script": "...",
  "description": "Monitors voltage and alerts on power loss"
}
```

#### Trigger & Concurrency Configuration:
* **`triggerType`**: String enum:
  - `"TOPIC"`: Triggered by incoming MQTT messages matching `topicFilters`.
  - `"TIMER"`: Triggered periodically by interval timer.
  - `"BOTH"`: Triggered by both topic messages and timer ticks.
  - `"CALLABLE"`: Not triggered by topics or timers; invoked explicitly by other scripts via `scripts.call()`.
* **`topicFilters`**: List of MQTT topic patterns with **full wildcard support** (`+` and `#`).
* **`triggerOnChangeOnly`**: Boolean (default `false`). If `true`, skips execution when the new payload matches the previous payload for that concrete topic.
* **`timerIntervalMs`**: Periodic interval in milliseconds (`0` = disabled).
* **`instanceMode`**: String enum (`"SINGLETON"` [default] | `"MULTI_INSTANCE"`):
  - `"SINGLETON"`: Only one instance runs at a time. Inbound executions are serialized to prevent race conditions on `state`.
  - `"MULTI_INSTANCE"`: Allows concurrent executions across the worker pool for stateless/parallel tasks.
* **`timeoutMs`**: Per-execution timeout limit (default `200` ms).
* **`description`**: Human-readable description.

---

## Broker Functions Available in Scripts

Scripts have access to a rich set of sandboxed modules and global variables:

### 1. Message & MQTT Operations (`msg`, `mqtt`)
* **`msg`**: Dictionary injected on topic trigger:
  ```python
  topic = msg["topic"]
  payload = msg["payload"]       # Auto-decoded if valid JSON, otherwise string
  raw_payload = msg["raw_payload"] # Always raw string
  timestamp = msg["timestamp"]   # Epoch milliseconds
  qos = msg["qos"]
  retain = msg["retain"]
  ```
  *(In timer runs or callable scripts, `msg` is `None`)*.
* **`mqtt.publish(topic, payload, qos=0, retain=False)`**: Publishes a message to the broker.
* **`mqtt.subscribe(filter, callback_fn)`**: Dynamically subscribes to an MQTT filter within the script and delegates incoming messages to a specified Python callback function.

### 2. Archive & History Operations (`archive`)
* **`archive.get_last_value(topic, archive_group="Default")`**:
  Returns dict `{"topic": str, "value": Any, "timestamp": int, "qos": int}` or `None`.
* **`archive.get_last_values(pattern, limit=100, archive_group="Default")`**:
  Returns a list of matching last value dicts.
* **`archive.get_history(topic, from_time=None, to_time=None, limit=100, archive_group="Default")`**:
  Returns historical message points.
* **`archive.get_aggregated_history(topics, interval, from_time, to_time, functions, fields)`**:
  Queries time-bucketed aggregations (`AVG`, `MIN`, `MAX`, `COUNT`, `SUM`).

### 3. Database Access (`db`)
Hooks into configured database connections (`stores.DatabaseConnectionConfig`):
* **`db.query(conn_name, sql, args=[])`**: Executes SQL query and returns rows as a list of dictionaries:
  ```python
  rows = db.query("pg_site", "SELECT id, threshold FROM limits WHERE site = $1", ["PlantA"])
  for row in rows:
      log.info(row["threshold"])
  ```
* **`db.execute(conn_name, sql, args=[])`**: Executes SQL statement (INSERT, UPDATE, DELETE) and returns `{"affected_rows": N, "success": True}`.

### 4. Variables & Scoped Storage (Node-RED Style)
* **`state`**: Persistent mutable dictionary local to **this script instance** across invocations (in-memory; resets on script reload/broker restart):
  ```python
  state["counter"] = state.get("counter", 0) + 1
  ```
* **`global`**: In-memory dictionary shared across **all scripts** in the broker:
  ```python
  global.set("active_shift", "Morning")
  current_shift = global.get("active_shift")
  ```
* **`storage`**: **Persistent Key-Value Store** (backed by SQLite/database; **survives script reloads and broker restarts**):
  ```python
  storage.set("ups_calibration_factor", 1.045)
  factor = storage.get("ups_calibration_factor", default=1.0)
  storage.delete("temporary_flag")
  ```

### 5. Inter-Script Execution (`scripts`)
Enables modular scripting by calling library/helper scripts:
* **`scripts.call(script_name, args={})`**:
  Invokes another registered script synchronously, passing `args` into the callee's execution context, and returning the callee's return value:
  ```python
  # Call a shared checksum calculation script
  result = scripts.call("CalculateCRC", {"data": msg["payload"]})
  if result.get("valid"):
      mqtt.publish("valid/data", msg["payload"])
  ```

### 6. Logging, Console & JSON
* **`log.info(...)`**, **`log.warn(...)`**, **`log.error(...)`**, **`log.debug(...)`** (and **`console.log(...)`**):
  Routes to structured logger with script name prefix, and appends to the script's recent circular log buffer.
* **`json.encode(obj)`** & **`json.decode(str)`**: Native JSON serialization helpers.

---

## Proposed Changes

### Component 1: Edge Broker Subsystem (`monster-mq-edge`)

#### [MODIFY] `go.mod`
- Add `go.starlark.net` dependency.

#### [MODIFY] `internal/config/config.go`
- Add `PythonScripts bool` to `FeaturesConfig` (with future room for `JavaScripts bool`).
- Add `PythonScripts` configuration settings (`WorkerPoolSize`, `QueueBufferSize`, `DefaultTimeoutMs`).

#### [MODIFY] `yaml-json-schema.json`
- Add `PythonScripts` under `Features.properties`.

#### [NEW] `internal/scripting/config.go`
- `ScriptConfig` struct with `triggerType`, `topicFilters: []string`, `instanceMode`, `timerIntervalMs`, etc.

#### [NEW] `internal/scripting/storage.go`
- Implement persistent Key-Value store (`ScriptKVStore`) backed by SQLite/PostgreSQL to support `storage.get/set/delete` across restarts.

#### [NEW] `internal/scripting/engine.go`
- Sandbox engine wrapping `go.starlark.net/starlark`.
- Built-in modules:
  - `msg`, `mqtt` (with dynamic callbacks), `archive`, `db`, `state`, `global`, `storage`, `scripts.call`, `log`, `json`.
- Guards: Step limits (`SetMaxExecutionSteps(50000)`), execution timeout.

#### [NEW] `internal/scripting/connector.go`
- Script instance controller:
  - Subscribes to `pubsub.Bus` for each topic filter in `topicFilters`.
  - Runs timer ticker if `timerIntervalMs > 0`.
  - Manages `instanceMode` serialization (mutex for `SINGLETON`, worker pool for `MULTI_INSTANCE`).
  - Maintains circular in-memory buffer of recent logs for live inspection.

#### [NEW] `internal/scripting/manager.go`
- Coordinates all enabled `"Script"` devices.
- Handles `scripts.call(name, args)`.
- Handles dynamic `Start`, `Stop`, `Reload`.

#### [MODIFY] `internal/broker/server.go`
- Initialize `scripting.NewManager` when `cfg.Features.PythonScripts` is enabled, and wire into GraphQL resolvers.

---

### Component 2: GraphQL Schema & API (`monster-mq-edge`)

#### [NEW] `internal/graphql/schema/scripts.graphqls`
- Dedicated SDL matching Main broker:
  ```graphql
  enum ScriptTriggerType {
      TOPIC
      TIMER
      BOTH
      CALLABLE
  }

  enum ScriptInstanceMode {
      SINGLETON
      MULTI_INSTANCE
  }

  type ScriptConfig {
      language: String!
      script: String!
      triggerType: ScriptTriggerType!
      topicFilters: [String!]!
      triggerOnChangeOnly: Boolean
      timerIntervalMs: Int
      instanceMode: ScriptInstanceMode!
      timeoutMs: Int
      description: String
  }

  input ScriptConfigInput {
      language: String! = "starlark"
      script: String!
      triggerType: ScriptTriggerType! = TOPIC
      topicFilters: [String!]! = []
      triggerOnChangeOnly: Boolean = false
      timerIntervalMs: Int = 0
      instanceMode: ScriptInstanceMode! = SINGLETON
      timeoutMs: Int = 200
      description: String
  }

  input ScriptInput {
      name: String!
      namespace: String! = "script"
      nodeId: String! = "local"
      enabled: Boolean = true
      config: ScriptConfigInput!
  }

  type Script {
      name: String!
      namespace: String!
      nodeId: String!
      enabled: Boolean!
      config: ScriptConfig!
      createdAt: String!
      updatedAt: String!
      isOnCurrentNode: Boolean!
      executionCount: Long
      errorCount: Long
      lastExecutionTime: String
      lastExecutionStatus: String
      recentLogs: [String!]!
  }

  type ScriptResult {
      script: Script
      success: Boolean!
      errors: [String!]!
  }

  type ScriptTestResult {
      success: Boolean!
      returnValue: String
      outputMessages: [ScriptPublishedMessage!]!
      logs: [String!]!
      errors: [String!]!
      executionTimeMs: Float!
  }

  type ScriptPublishedMessage {
      topic: String!
      payload: String!
      qos: Int!
      retain: Boolean!
  }

  extend type Query {
      scripts(name: String, nodeId: String): [Script!]!
      script(name: String!): Script
  }

  extend type Mutation {
      script: ScriptMutations!
  }

  type ScriptMutations {
      create(input: ScriptInput!): ScriptResult!
      update(name: String!, input: ScriptInput!): ScriptResult!
      delete(name: String!): Boolean!
      toggle(name: String!, enabled: Boolean!): ScriptResult!
      start(name: String!): ScriptResult!
      stop(name: String!): ScriptResult!
      test(input: ScriptInput!, testTopic: String, testPayload: String, testArgs: String): ScriptTestResult!
  }
  ```

#### [NEW] `internal/graphql/resolvers/scripts.go`
- Implement query & mutation handlers.

---

### Component 3: Main Java Broker Parity (`monster-mq`)

1. `stores/DeviceConfig.kt`: Add `const val DEVICE_TYPE_SCRIPT = "Script"`.
2. `stores/devices/ScriptConfig.kt`: Add `ScriptConfig` data class matching the schema.
3. `Features.kt`: Add `const val PythonScripts = "PythonScripts"` (and in future `const val JavaScripts = "JavaScripts"`).
4. `broker/src/main/resources/schema-scripts.graphqls`: Add matching SDL.

---

## Verification Plan

### Automated Tests
1. **Engine & Builtin Tests (`internal/scripting/engine_test.go`)**:
   - `msg` and `mqtt.publish`.
   - `mqtt.subscribe` dynamic callback.
   - `archive.get_last_value` & `archive.get_history`.
   - `db.query` & `db.execute`.
   - Memory scopes: `state` (instance), `global` (broker-wide), `storage` (persistent across restarts).
   - `scripts.call(name, args)` with return values.
   - Execution guards: step limit and timeout enforcement.
2. **Trigger & Concurrency Tests (`internal/scripting/connector_test.go`)**:
   - Multiple `topicFilters` wildcard matching (`+`, `#`).
   - `timerIntervalMs` ticks.
   - `instanceMode`: Serialized `SINGLETON` vs parallel `MULTI_INSTANCE`.
3. **Integration Tests (`test/integration/script_test.go`)**:
   - Create script via GraphQL mutation.
   - Test `test()` mutation endpoint with mock payload and args.
   - Verify live message processing end-to-end.

### Build Verification
- `make build` (`CGO_ENABLED=0`)
- `make build-arm64`
- `make test` & `make lint`
