# MonsterMQ Edge

A native, single-binary MQTT broker that ships the same GraphQL API and storage
schemas as the JVM-based MonsterMQ broker, slimmed down for edge deployments
on devices like the Raspberry Pi 4/5.

## Highlights

- **Single static binary** (~25 MB), zero CGO. Pure-Go SQLite via `modernc.org/sqlite`.
- **MQTT 3.1 / 3.1.1 / 5.0** via [mochi-mqtt/server](https://github.com/mochi-mqtt/server) (locally vendored fork at [vogler75/mochi-mqtt-server](https://github.com/vogler75/mochi-mqtt-server)) — TCP, WebSocket, TLS, WSS.
- **Storage**: SQLite (default), PostgreSQL, MongoDB. Schemas are byte-compatible with the Kotlin broker, so the same DB can be opened by either implementation.
- **Archive groups** (last-value + history fanout, retention purging) — same model as the Kotlin broker.
- **GraphQL API** with subscriptions, schema-parity with the existing dashboard.
- **MQTT bridge** — forward local topics to a remote broker and vice versa.
- **Users + ACL** with bcrypt password hashing.
- **Periodic metrics** surfaced via `Broker.metrics`/`metricsHistory`; history can be persisted or kept in memory.
- **WinCC Unified bridge** — GraphQL/WebSocket or Open Pipe transport into local MQTT topics.
- **WinCC Open Architecture bridge** — GraphQL/WebSocket subscriptions from `dpQueryConnectSingle` into local MQTT topics.
- No clustering, no OPC UA / Kafka / Sparkplug / GenAI / flows.

## Getting Started

The mochi-mqtt fork is included as a git submodule. You must initialise it before compiling.

**Clone (first time):**
```bash
git clone --recurse-submodules https://github.com/vogler75/monster-mq-edge
cd monster-mq-edge
```

**If you already cloned without `--recurse-submodules`:**
```bash
git submodule update --init
```

**Install Go** (1.22+): https://go.dev/dl/

Then build and run:
```bash
# Linux / macOS
./run.sh -b

# Windows
run.bat -b
```

Or use Make directly:
```bash
make build
./bin/monstermq-edge -config config.yaml.example
```

## Quickstart

```bash
make build
./bin/monstermq-edge -config config.yaml.example
```

- MQTT: `mqtt://localhost:1883`
- WebSocket MQTT: `ws://localhost:1884/mqtt`
- GraphQL HTTP/WS: `http://localhost:8080/graphql`
- GraphQL playground: `http://localhost:8080/playground`

## Cross-compile

```bash
make build-arm64    # Linux ARM64 (Pi 4/5, generic 64-bit)
make build-armv7    # Linux ARMv7 (Pi 3 / older 32-bit)
make build-amd64    # Linux x86_64
```

## Docker

```bash
docker buildx build --platform linux/amd64,linux/arm64 -t monstermq-edge:latest .
docker run --rm -p 1883:1883 -p 8080:8080 monstermq-edge:latest
```

## Storage backends

Pick the backend in `config.yaml`:

```yaml
DefaultStoreType: SQLITE   # or POSTGRES, or MONGODB
SQLite: { Path: ./data/monstermq.db }
Postgres: { Url: "postgres://localhost:5432/monstermq", User: monstermq, Pass: monstermq }
MongoDB:  { Url: "mongodb://localhost:27017", Database: monstermq }
```

By default the stores live on the chosen backend. High-churn runtime stores can
also be moved to memory with `SessionStoreType`, `RetainedStoreType`,
`QueueStoreType`, and `Metrics.StoreType`.

`SessionStoreType: MEMORY` and `QueueStoreType: MEMORY` use in-memory SQLite
databases internally. Persistent cross-backend overrides are intentionally not
mixed: use `MEMORY` or the same backend as `DefaultStoreType`.

## Feature flags

Subsystems that should not always run are enabled under `Features`:

```yaml
Features:
  MqttClient: true
  WinCCUa: false
  WinCCOa: false
```

- `MqttClient` enables the MQTT bridge manager for forwarding topics between
  the local broker and remote MQTT brokers.
- `WinCCUa` enables WinCC Unified clients. Each device config can use
  `GRAPHQL` for GraphQL/WebSocket subscriptions or `OPENPIPE` for local Open
  Pipe IPC.
- `WinCCOa` enables WinCC Open Architecture clients. Each client subscribes to
  WinCC OA GraphQL `dpQueryConnectSingle` updates and republishes datapoint
  changes into MQTT.

Enabled features are also reported through GraphQL as `enabledFeatures`, using
the same names as the config flags.

### WinCC bridge behavior

WinCC client configurations are stored in the device config store, so they
remain persistent even when runtime stores such as metrics, sessions, retained
messages, and queues are configured as `MEMORY`.

WinCC Unified publishes tag values and alarms under the configured namespace and
address topic. WinCC OA publishes datapoint rows under:

```text
<namespace>/<address.topic>/<transformed datapoint name>
```

Both WinCC bridges expose live metrics through their GraphQL client `metrics`
field. WinCC OA also writes metrics history when `Metrics.StoreType` is backed
by a metrics store.

## Low-write edge devices

For flash-backed devices where runtime database writes should be avoided as much
as possible, keep the config/user/device stores on disk and move high-churn
runtime state to memory:

```yaml
DefaultStoreType: SQLITE
ConfigStoreType: SQLITE
SessionStoreType: MEMORY
RetainedStoreType: MEMORY
QueueStoreType: MEMORY

SQLite:
  Path: ./data/monstermq.db

Metrics:
  Enabled: true
  StoreType: MEMORY
  CollectionIntervalSeconds: 1
  MaxHistoryRows: 3600

Logging:
  RingBufferSize: 1000

QueuedMessagesEnabled: true
```

With this profile, normal metrics, logs, sessions, subscriptions, retained
messages, and queued messages do not write to the SQLite file. The broker still
writes when you change persistent configuration: users/ACLs, MQTT/WinCC device
configs, archive groups, database connections, and other config-store data.

`QueueStoreType: MEMORY` keeps the broker's queued-message behavior for offline
persistent sessions, but those queued messages are held in RAM and are lost on
broker restart. RAM is the limiting factor: if many clients are offline or large
payloads are queued, memory can grow quickly. For small devices, either size the
workload conservatively or set `QueuedMessagesEnabled: false` to rely on
mochi-mqtt's in-process inflight handling instead.

## User management

When `UserManagement.Enabled` is `true`, the broker ensures a default admin
user exists during startup:

```text
username: Admin
password: Admin
```

Change this password immediately after first login. If the user already exists,
startup leaves it unchanged.

With `AnonymousEnabled: true`, GraphQL login and MQTT clients can still use
anonymous access. Set `AnonymousEnabled: false` to require configured users.

## Dashboard

Set `Dashboard.Path` to a built `dashboard/dist` directory to serve the existing
MonsterMQ dashboard against this broker. With no path set, a placeholder index
page is served at `/` linking to the GraphQL playground.

```yaml
Dashboard:
  Enabled: true
  Path: /opt/monstermq-dashboard/dist
```

## Architecture

```
cmd/monstermq-edge/      → main, flag parsing, signal handling
internal/
  config/                → YAML schema + loader
  broker/                → mochi-mqtt bootstrap, hook wiring, TLS, lifecycle
  stores/                → MessageStore / MessageArchive / SessionStore /
                           QueueStore / UserStore / ArchiveConfigStore /
                           DeviceConfigStore / MetricsStore interfaces
  stores/sqlite/         → byte-compatible SQLite implementations
  stores/postgres/       → byte-compatible PostgreSQL implementations
  stores/mongodb/        → MongoDB implementations
  archive/               → archive group orchestrator + retention
  bridge/mqttclient/     → MQTT-to-MQTT bridge (paho client)
  bridge/winccua/        → WinCC Unified bridge (GraphQL/Open Pipe)
  bridge/winccoa/        → WinCC Open Architecture bridge (GraphQL)
  auth/                  → user+ACL cache
  metrics/               → in-memory counters + periodic snapshot writer
  pubsub/                → in-process bus for GraphQL topicUpdates
  graphql/               → gqlgen-generated server, resolvers, dashboard handler
```

## Running tests

```bash
make test
```

Integration tests cover MQTT pub/sub, retained survives-restart, bcrypt auth +
ACL, archive group fanout, GraphQL queries/mutations, metrics persistence,
memory-backed queue/session stores, and end-to-end MQTT bridging between two
brokers. Package tests cover WinCC OA config parsing, topic transforms, and
payload formatting.

## Status

Single-node. No clustering. Production-ready for edge use on Pi 4/5;
PostgreSQL/MongoDB backends compile but require a live DB to integration test.

## License

GNU General Public License v3.0.

The vendored `mochi-mqtt-server` subtree remains under its original MIT license;
see `mochi-mqtt-server/LICENSE.md`.
