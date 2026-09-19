# MonsterMQ Edge Starlark Script Reference

The Broker Script Engine on MonsterMQ Edge executes deterministic, lightweight scripts written in **Starlark** (a Python dialect implemented in pure Go). Scripts execute standalone directly inside the broker process with zero overhead.

---

## 1. Runtime & Execution Model

- **Dialect**: Google Starlark (Python 3 syntax subset). Safe, deterministic, hermetic execution with instant startup.
- **Data Types**: `int`, `float`, `string`, `bool`, `list`, `dict`, `set`, `None`.
- **Language Constraints**: No classes, no recursion, and no unbounded `while` loops (to guarantee termination). Pre-compiled bytecode with configurable execution step limits.
- **Triggers**:
  - `TOPIC`: Triggered when an incoming MQTT message matches the configured topic filter(s). Supports MQTT wildcards (`+`, `#`). Can optionally enable *Trigger on Change Only* to skip identical consecutive payloads.
  - `TIMER`: Triggered periodically at a fixed millisecond interval (e.g. `5000` ms). In timer ticks, `msg` is `None`.
  - `BOTH`: Responds to both incoming MQTT messages and periodic timer intervals.
  - `CALLABLE`: Invoked only when explicitly called by another script via `scripts.call(name, args)` or via the test sandbox.
- **Concurrency Modes**:
  - `SINGLETON` (Default): Invocations execute sequentially in a dedicated FIFO queue for this script instance. Guarantees that in-memory `state` is thread-safe without race conditions.
  - `MULTI_INSTANCE`: Invocations run concurrently across the broker worker pool for high-throughput, stateless workloads.

---

## 2. Global Bindings & API Reference

### 2.1 `msg` (Incoming MQTT Message)
Available during `TOPIC` or `BOTH` triggers. `None` during timer ticks or callable invocations without a message payload.

```python
if msg != None:
    topic = msg["topic"]             # string: e.g. "sensors/chiller/temp"
    payload = msg["payload"]         # parsed JSON dict/list, or string if not JSON
    raw = msg["raw_payload"]         # raw string payload
    qos = msg["qos"]                 # integer: 0, 1, or 2
    retain = msg["retain"]           # boolean: True if retained
    ts = msg["timestamp"]            # string: ISO 8601 UTC timestamp
```

### 2.2 `mqtt` (Broker MQTT Operations)
Publish messages directly into the broker or subscribe dynamically to topics.

```python
# Publish message
# mqtt.publish(topic, payload, qos=0, retain=False)
mqtt.publish("alerts/temperature", json.encode({"alarm": True, "value": 85.4}), qos=1, retain=True)

# Subscribe dynamically to topics with a callback
def on_message(sub_msg):
    log.info("Received message on: " + sub_msg["topic"])

mqtt.subscribe("cmd/reset/+", on_message)
```

### 2.3 `archive` (Time-Series & Last Value Store)
Query retained or historical time-series data from archive groups.

```python
# Get last recorded message for a specific topic
last_val = archive.get_last_value("sensors/ambient/temperature", archive_group="Default")
if last_val != None:
    log.info("Previous value: " + str(last_val["payload"]))

# Get historical messages (from_time, to_time in ISO-8601 strings)
records = archive.get_history("sensors/ambient/temperature", limit=50)

# Get aggregated history
# archive.get_aggregated_history(topics, interval, from_time, to_time, functions, fields, archive_group)
agg = archive.get_aggregated_history(["sensors/temp1"], "5m", "2026-01-01T00:00:00Z", "2026-01-01T01:00:00Z", ["AVG", "MAX"])
```

### 2.4 `db` (Configured Database Connections)
Execute queries or statements against configured SQLite, PostgreSQL, or other database connections.

```python
# Query returning a list of row dictionaries
rows = db.query("ProductionDb", "SELECT id, setpoint, status FROM equipment WHERE active = $1", [True])
for row in rows:
    log.info("Equipment setpoint: " + str(row["setpoint"]))

# Execute INSERT / UPDATE / DELETE statements
# Returns dict: {"affected_rows": int, "success": bool}
res = db.execute("ProductionDb", "UPDATE equipment SET last_seen = NOW() WHERE id = $1", [42])
```

### 2.5 Scoped Storage (`state`, `global`, `storage`)

- **`state`**: Mutable dictionary private to this script instance, preserved in memory across invocations. Ideal for counters, moving averages, debounce flags, or state machines.
- **`global`**: Shared in-memory key-value store accessible across all scripts on the local broker node.
- **`storage`**: Persistent key-value store saved on disk, surviving broker restarts and script reloads.

```python
# Local in-memory state (singleton scripts)
state["count"] = state.get("count", 0) + 1

# Node-wide shared memory
global.set("active_shift", "Shift-A")
current_shift = global.get("active_shift", "Default")

# Persistent on-disk storage
last_run = storage.get("last_calibration_time", None)
storage.set("last_calibration_time", "2026-09-19T18:00:00Z")
storage.delete("temporary_flag")
```

### 2.6 `scripts.call` (Inter-Script Calls)
Invoke another registered script configured with trigger type `CALLABLE`.

```python
result = scripts.call("CalculateFlowRate", {"pressure": 4.2, "diameter": 0.05})
log.info("Calculated flow rate: " + str(result))
```

### 2.7 `log` (System Logging)
Emit structured log entries captured in the script's recent execution buffer and broker system logs.

```python
log.info("Processing started for device")
log.warn("Value exceeded normal threshold: " + str(val))
log.error("Failed to process message: " + str(err))
log.debug("Debug payload details: " + str(payload))
```

### 2.8 `json` (Serialization & Deserialization)
Encode and decode JSON data.

```python
json_str = json.encode({"temperature": 23.5, "alarm": False})
data = json.decode(json_str)
```

---

## 3. Practical Recipes

### 3.1 Threshold Alert & Retained Alarm
```python
# Trigger: TOPIC (sensors/+/temp)
if msg != None and type(msg["payload"]) == "dict":
    temp = msg["payload"].get("temperature", 0)
    parts = msg["topic"].split("/")
    dev_id = parts[1] if len(parts) > 1 else "unknown"

    if temp > 80.0:
        alarm = {
            "device": dev_id,
            "temperature": temp,
            "status": "CRITICAL_HIGH",
            "timestamp": msg["timestamp"]
        }
        mqtt.publish("alarms/" + dev_id, json.encode(alarm), qos=1, retain=True)
        log.warn("High temperature alert on " + dev_id + ": " + str(temp))
    elif temp < 70.0:
        mqtt.publish("alarms/" + dev_id, json.encode({"device": dev_id, "status": "OK"}), qos=1, retain=True)
```

### 3.2 Moving Average Filter
```python
# Trigger: TOPIC (meters/+/power)
if msg != None:
    try:
        val = float(msg["raw_payload"])
        window = state.get("window", [])
        window.append(val)
        if len(window) > 10:
            window.pop(0)
        state["window"] = window

        avg = sum(window) / len(window)
        mqtt.publish(msg["topic"] + "/avg", str(round(avg, 2)), retain=True)
    except Exception as e:
        log.error("Error calculating average: " + str(e))
```

### 3.3 Periodic Database Poll & MQTT Publish
```python
# Trigger: TIMER (Interval: 10000 ms)
rows = db.query("PlantDb", "SELECT tag, value FROM telemetry WHERE reported = false LIMIT 50", [])
for r in rows:
    mqtt.publish("plant/telemetry/" + r["tag"], str(r["value"]), qos=0, retain=False)
log.info("Published " + str(len(rows)) + " telemetry points")
```
