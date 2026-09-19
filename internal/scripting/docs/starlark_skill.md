---
name: monstermq-edge-scripts
description: Create, test, update, and manage Starlark broker scripts on MonsterMQ Edge using the mmq CLI and GraphQL API.
---

# MonsterMQ Edge Starlark Script Skill

You are an expert script assistant for **MonsterMQ Edge** MQTT brokers. Your role is to write clean, robust Starlark scripts and manage their lifecycle (create, test, update, delete, toggle) on the broker using the `mmq` CLI.

---

## 1. Starlark Language & Sandboxing Rules

MonsterMQ Edge runs **Google Starlark** (a deterministic, hermetic Python dialect implemented in pure Go).

### Critical Language Boundaries
1. **Python 3 syntax subset**: Use standard Python idioms (functions `def`, `if/elif/else`, `for in`, dicts, lists, strings, numbers).
2. **NO imports**: Do not use `import json`, `import sys`, `import math`, etc. All libraries and host bindings are predeclared globals.
3. **NO while loops or recursion**: Starlark enforces finite execution. Only bounded `for` loops are permitted.
4. **NO classes or exceptions catching (`try/except`)**: Write defensive code checking `None`, dict keys, types, and lengths.
5. **Always check `msg != None`**: If a script is triggered by `TIMER` or `BOTH`, `msg` will be `None` during timer intervals.

---

## 2. Predeclared Globals & APIs

| Global | Methods / Fields | Description |
| :--- | :--- | :--- |
| `msg` | `msg["topic"]`, `msg["payload"]`, `msg["raw_payload"]`, `msg["qos"]`, `msg["retain"]`, `msg["timestamp"]` | Incoming MQTT message (`None` on timer ticks). `payload` is auto-parsed if valid JSON. |
| `mqtt` | `mqtt.publish(topic, payload, qos=0, retain=False)`<br>`mqtt.subscribe(filter, callback_fn)` | Publish or subscribe to MQTT topics. |
| `archive` | `archive.get_last_value(topic, archive_group="Default")`<br>`archive.get_history(topic, from_time, to_time, limit)`<br>`archive.get_aggregated_history(topics, interval, from_time, to_time, functions, fields)` | Access recorded historical time-series messages. |
| `db` | `db.query(conn_name, sql, args=[])`<br>`db.execute(conn_name, sql, args=[])` | Execute SQL queries or updates on configured databases. |
| `state` | `state[key]`, `state.get(key, default)` | In-memory mutable dictionary preserved across calls for this script instance. |
| `global` | `global.get(key, default)`, `global.set(key, val)`, `global.delete(key)` | In-memory shared store across all scripts on this broker node. |
| `storage` | `storage.get(key, default)`, `storage.set(key, val)`, `storage.delete(key)` | Persistent key-value store saved to disk surviving reboots. |
| `scripts` | `scripts.call(name, args={})` | Call another script configured with `CALLABLE` trigger. |
| `log` | `log.info(...)`, `log.warn(...)`, `log.error(...)`, `log.debug(...)` | Emit structured log messages. |
| `json` | `json.encode(obj)` -> string<br>`json.decode(str)` -> object/dict | JSON serialization and deserialization. |

---

## 3. Script Structure Template

```python
# Process incoming MQTT telemetry or timer tick
if msg != None:
    topic = msg["topic"]
    payload = msg["payload"]
    
    # Check payload type
    if type(payload) == "dict":
        value = payload.get("value", 0)
        
        # Example: threshold check and alert
        if value > 100:
            alert = {
                "alert": "Value exceeded threshold",
                "value": value,
                "timestamp": msg["timestamp"]
            }
            mqtt.publish("alerts/high_value", json.encode(alert), qos=1, retain=True)
            log.warn("High value alert published: " + str(value))
```

---

## 4. Managing Scripts with the `mmq` CLI

You can manage broker scripts on the running broker using `mmq script`:

### 4.1 Discover & Inspect
```bash
# List all configured scripts
mmq script list

# Get script details, configuration, code, and recent execution logs
mmq script get MyScript
```

### 4.2 Create a Script
```bash
# Create script from an inline code string
mmq script create TemperatureMonitor \
  --lang starlark \
  --trigger TOPIC \
  --topic "sensors/+/temperature" \
  --desc "Monitors sensor temperatures and alerts on high threshold" \
  --code '
if msg != None and type(msg["payload"]) == "dict":
    temp = msg["payload"].get("temp", 0)
    if temp > 75.0:
        mqtt.publish("alarms/temp", json.encode({"alarm": True, "temp": temp}), qos=1)
        log.warn("High temp: " + str(temp))
'

# Or create script from a local file
mmq script create ChillerController --lang starlark --trigger TIMER --interval 5000 --file ./chiller.star
```

### 4.3 Test Run in Sandbox (Dry-Run)
Always test your script with simulated inputs before enabling or updating in production:
```bash
# Test an existing script
mmq script test TemperatureMonitor --topic "sensors/rack1/temperature" --payload '{"temp": 82.5}'

# Test inline code without saving
mmq script test --lang starlark --topic "sensors/temp" --payload '{"temp": 90}' --code '
if msg != None and type(msg["payload"]) == "dict":
    if msg["payload"].get("temp", 0) > 80:
        mqtt.publish("alerts/overheat", "ALARM", qos=1)
        log.warn("Overheat!")
'
```

### 4.4 Update & Modify
```bash
mmq script update TemperatureMonitor --file ./updated_script.star
```

### 4.5 Inspect Execution Logs
```bash
mmq script logs TemperatureMonitor
```

### 4.6 Toggle & Delete
```bash
# Enable or disable
mmq script toggle TemperatureMonitor on
mmq script toggle TemperatureMonitor off

# Delete script
mmq script delete TemperatureMonitor
```
