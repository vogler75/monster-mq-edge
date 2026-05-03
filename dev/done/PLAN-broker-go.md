# Edge MonsterMQ — Native MQTT Broker for Raspberry Pi

## Status (2026-04-28)

Implementation has landed on `develop`. Milestones 1–10 are complete; M11
(dashboard packaging) was dropped — the edge does not host a UI. The repo
lives standalone at `monster-mq-edge/` (sibling of `monster-mq/`), not under
`broker.go/`. Notable deltas from the original plan:

- `Bridges: { Mqtt: { Enabled } }` replaced by flat `Features: { MqttClient: true }`
  (mirrors the Java broker's `Features` map).
- `Dashboard:` config + the embedded placeholder removed entirely; an
  external dashboard talks to `/graphql`.
- `internal/topic/` added — dual-index subscription resolver (ExactIndex +
  WildcardIndex) ported from `monster-mq` Kotlin. Used by the queue hook so
  it doesn't scan the whole subscription table per published message. Not
  in the original plan; called out in the Storage / Hooks sections below.
- `internal/stores/memory/` exists for tests; not all interfaces are
  memory-backed but the basic ones are.
- `yaml-json-schema.json` lives at the repo root and is enforced against
  `config.yaml{,.example}`. The plan used to point at the Java broker's
  schema; we have our own now.
- `cmd/monstermq-edge/` and the run/Makefile flags (`-build`, `-norun`,
  `-dashboard`) match the plan's intent.

Open work captured in this plan but not yet done:
- `enabledFeatures` reports `["MqttClient"]`. Plan said `["MqttBroker",
  "MqttBridge"]`. Decide which set the dashboard expects and align.
- Paho v5 (`paho.golang/paho`) is not yet pulled in — bridge currently
  uses `paho.mqtt.golang` v3.x only.
- `scripts/sync-mochi.sh` for re-syncing upstream — ties in with
  `PLAN-inline-mochi.md`, which has not been executed yet.

## Context

MonsterMQ today is a Kotlin/Vert.x JVM broker (~86k LOC, ~258 files) with rich features: clustering via Hazelcast, eight bridge protocols (OPC UA, Kafka, NATS, Redis, WinCC OA/UA, PLC4X, Neo4j, MQTT), GenAI integration, a flow engine, MCP servers, and an extensive GraphQL API. JVM startup, RAM footprint, and packaging make it a poor fit for small edge deployments.

We want a **second, slimmer broker** that:
- Is a single statically-linked native binary, easy to drop on a Raspberry Pi 4/5.
- Keeps **schema parity** with the existing GraphQL API for the core MQTT/storage surface so the existing (external, JVM-broker-side) dashboard works unchanged against the edge. Pages for absent features (OPC UA, Kafka, etc.) just return empty data.
- Supports the same three production storage backends: **SQLite**, **PostgreSQL**, **MongoDB**.
- Implements the same **archive group / last-value / message archive / metrics / users+ACL / device config** persistence model.
- Includes only the **MQTT bridge** (no OPC UA, Kafka, WinCC, PLC4X, NATS, Redis, Neo4j, GenAI, flows, MCP, Sparkplug, JDBC/InfluxDB/TimeBase loggers).
- No clustering — single node only.

The target is a **field-deploy MQTT node** that talks to the central MonsterMQ via MQTT bridge and exposes the same API surface for local management.

---

## Language Decision: **Go** (not Rust)

### Library evaluation

| Concern | Go pick | Rust pick | Verdict |
|---|---|---|---|
| Embeddable MQTT broker | `github.com/mochi-mqtt/server/v2` — purpose-built for embedding, **first-class hook system** (Auth, OnPublished, OnSubscribed, OnRetained, OnSelectClient, OnSession*, etc.), MQTT 3.0/3.1.1/5.0 fully compliant, MIT, with bundled storage hooks (Pebble, Badger, Bolt, Redis) as reference implementations. | `rumqttd` — excellent broker but exposes **only an auth handler closure and a "local link"** observer API. Custom retained/session/queue stores are not first-class extension points; you'd fork or shim heavily. | **Go wins decisively for our use case.** Mochi's hook contract maps almost 1:1 onto MonsterMQ's `IMessageStore` / `ISessionStoreAsync` / `IRetainedStore` / `IQueueStoreAsync` interfaces. |
| GraphQL server with subscriptions | `github.com/99designs/gqlgen` — **schema-first**, multi-file `.graphqls` globs, codegen produces strongly-typed resolver stubs, WebSocket + SSE transports, GraphQL-WS protocol supported. | `async-graphql` — code-first (SDL is exported, not consumed). | **Go wins** because our requirement is *schema parity* with the existing `.graphqls` files. gqlgen consumes those files directly; async-graphql would mean reverse-mapping the schema into Rust types by hand. |
| SQL drivers | `database/sql` + `mattn/go-sqlite3` (or `modernc.org/sqlite` for pure-Go), `jackc/pgx/v5`. | `sqlx` (excellent). | Both fine. Pure-Go SQLite (`modernc.org/sqlite`) gives a true zero-CGO static binary. |
| MongoDB | `go.mongodb.org/mongo-driver/v2` (official). | `mongodb` crate (official). | Both fine. |
| Cross-compile to ARM | `GOOS=linux GOARCH=arm64 go build` — trivial. | `cross` / target installs — works but more friction. | Go wins on ergonomics. |
| Memory on Pi 4/5 (1–8 GB) | ~25–50 MB RSS. | ~5–15 MB RSS. | **Irrelevant on Pi 4/5.** Would matter on Pi Zero. User confirmed Pi 4/5 is the target. |
| Dev velocity | High. | Medium. | Go wins. |

Both languages are native-compiled and meet the binary-size/no-GC-pause-vs-acceptable trade-off on Pi-class hardware. The deciding factors are mochi-mqtt's hook architecture and gqlgen's schema-first flow — those two together remove most of the design risk from this project.

### Why not Rust
On Pi Zero / sub-512 MB devices we would revisit. For Pi 4/5 the Go ecosystem fit is materially better and the implementation will land faster.

---

## Repository Layout

New top-level sibling directory: **`monster-mq-edge/`**

```
monster-mq-edge/
├── go.mod
├── go.sum
├── cmd/
│   └── monstermq-edge/
│       └── main.go                    # entry point, flag parsing, bootstrap
├── internal/
│   ├── config/
│   │   ├── config.go                  # YAML schema (compatible subset of broker/yaml-json-schema.json)
│   │   └── load.go
│   ├── broker/
│   │   ├── server.go                  # mochi-mqtt server bootstrap, listeners, lifecycle
│   │   ├── hook_storage.go            # mochi Hook -> session/retained/queue stores
│   │   ├── hook_auth.go               # mochi Hook -> IUserStore-backed auth + ACL
│   │   ├── hook_archive.go            # mochi Hook -> archive groups (last-value + history fanout)
│   │   └── hook_metrics.go            # in-memory rate counters, drained by metrics writer
│   ├── stores/
│   │   ├── types.go                   # BrokerMessage, MqttSubscription, ArchiveGroupConfig, User, AclRule, MetricKind, etc.
│   │   ├── interfaces.go              # IMessageStore, IMessageArchive, ISessionStore, IQueueStore,
│   │   │                              # IUserStore, IArchiveConfigStore, IDeviceConfigStore, IMetricsStore
│   │   ├── sqlite/                    # SQLite implementations of all interfaces
│   │   ├── postgres/                  # PostgreSQL implementations
│   │   ├── mongodb/                   # MongoDB implementations
│   │   └── memory/                    # in-memory backends used by integration tests
│   ├── topic/                         # dual-index subscription resolver (ported from
│   │                                  # monster-mq's data/SubscriptionManager): ExactIndex
│   │                                  # + Tree-backed WildcardIndex with MQTT 3.1.1 $SYS rule.
│   ├── archive/
│   │   ├── group.go                   # ArchiveGroup orchestrator (filter match + buffered write)
│   │   ├── manager.go                 # load/reload from IArchiveConfigStore
│   │   └── retention.go               # purge loop honoring lastValRetentionMs / archiveRetentionMs
│   ├── bridge/
│   │   └── mqttclient/
│   │       ├── connector.go           # paho.mqtt.golang client per device
│   │       ├── manager.go             # load from IDeviceConfigStore, deploy/undeploy
│   │       └── transform.go           # topic remap / removePath
│   ├── graphql/
│   │   ├── schema/                    # symlinked OR copied .graphqls files (slimmed subset)
│   │   ├── gqlgen.yml
│   │   ├── generated/                 # gqlgen-emitted code (committed)
│   │   ├── resolvers/                 # query/mutation/subscription resolvers
│   │   └── server.go                  # HTTP + WebSocket transport (no UI hosted)
│   ├── pubsub/                        # in-process bus for `topicUpdates` subscription
│   ├── auth/
│   │   ├── bcrypt.go
│   │   ├── acl.go                     # priority-ordered topic-pattern matching
│   │   └── cache.go                   # in-memory user+ACL cache, reload on mutation
│   ├── metrics/
│   │   ├── collector.go               # 1s tick: drain hook counters into ring buffer
│   │   └── writer.go                  # periodic flush to IMetricsStore + retention purge
│   ├── log/
│   │   └── log.go                     # slog-based, optional MQTT $SYS/syslogs publish + ring buffer
│   └── version/
│       └── version.go
├── config.yaml.example
├── yaml-json-schema.json              # draft-07 schema for config.yaml
├── Dockerfile                         # multi-arch buildx (amd64 + arm64)
├── Makefile                           # build, build-arm64, test, gen, lint
└── README.md
```

A separate Go module keeps Kotlin/Maven and Go toolchains independent. Top-level CI can be extended later.

---

## Library Picks (locked)

| Concern | Library | Notes |
|---|---|---|
| MQTT broker | `github.com/mochi-mqtt/server/v2` | Hook-based extensibility. Use built-in TCP/TLS/WS listeners. |
| MQTT client (bridge) | `github.com/eclipse/paho.mqtt.golang` for v3.1.1, `github.com/eclipse/paho.golang/paho` for v5 | Match what the existing Kotlin bridge supports. Pick at runtime by config. |
| GraphQL | `github.com/99designs/gqlgen` | Schema-first, codegen, WebSocket subscriptions via `transport.Websocket` (graphql-ws + graphql-transport-ws). |
| HTTP router | `github.com/go-chi/chi/v5` | Lightweight, idiomatic; gqlgen examples assume it. |
| YAML config | `gopkg.in/yaml.v3` | |
| SQLite | `modernc.org/sqlite` | Pure-Go translation of SQLite — **no CGO**, true static binary. CGO `mattn/go-sqlite3` as fallback if perf demands. |
| PostgreSQL | `github.com/jackc/pgx/v5` | Modern, fast, no `database/sql` overhead. |
| MongoDB | `go.mongodb.org/mongo-driver/v2` | Official driver. |
| bcrypt | `golang.org/x/crypto/bcrypt` | Match algorithm used by existing broker. |
| Logging | `log/slog` (stdlib) | |
| Validation/JSON Schema | `github.com/santhosh-tekuri/jsonschema/v6` | Validate `config.yaml` against the existing `broker/yaml-json-schema.json` (subset). |
| Metrics ring buffer | hand-rolled, no external dep | |

---

## Storage Interface Design

Direct port of the existing Kotlin interfaces (`broker/src/main/kotlin/stores/I*.kt`). Rename to idiomatic Go (`MessageStore` not `IMessageStore`), use `context.Context` and channels/iterators in place of Vert.x `Future` and callbacks.

```go
// internal/stores/interfaces.go (excerpt)

type MessageStore interface {
    Name() string
    Type() MessageStoreType
    Get(ctx context.Context, topic string) (*BrokerMessage, error)
    AddAll(ctx context.Context, msgs []BrokerMessage) error
    DelAll(ctx context.Context, topics []string) error
    FindMatchingMessages(ctx context.Context, pattern string, yield func(BrokerMessage) bool) error
    FindMatchingTopics(ctx context.Context, pattern string, yield func(string) bool) error
    PurgeOlderThan(ctx context.Context, t time.Time) (PurgeResult, error)
    Drop(ctx context.Context) error
    Connected() bool
    EnsureTable(ctx context.Context) error
}

type MessageArchive interface {
    Name() string
    Type() MessageArchiveType
    AddHistory(ctx context.Context, msgs []BrokerMessage) error
    GetHistory(ctx context.Context, topic string, from, to time.Time, limit int) ([]ArchivedMessage, error)
    GetAggregatedHistory(ctx context.Context, topics []string, from, to time.Time, intervalMin int,
        funcs []AggFunc, fields []string) (AggregatedResult, error)
    PurgeOlderThan(ctx context.Context, t time.Time) (PurgeResult, error)
    EnsureTable(ctx context.Context) error
}

type SessionStore interface { /* setClient, setLastWill, addSubscriptions, delClient, iterate*, ... */ }
type QueueStore   interface { /* enqueue, dequeue, purgeForClient, count, ... */ }
type RetainedStore interface { MessageStore }   // same shape; separate type for clarity
type UserStore    interface { /* CRUD users, CRUD AclRules, ValidateCredentials, LoadAll */ }
type ArchiveConfigStore interface { /* GetAll, Get, Save, Delete, Update */ }
type DeviceConfigStore  interface { /* GetAll, GetByNode, Save, Delete, Toggle, Reassign, Import, Export */ }
type MetricsStore interface { /* StoreMetrics, GetLatestMetrics, GetHistory, PurgeOlderThan, Of(MetricKind) */ }
```

Per-backend implementations live under `internal/stores/{sqlite,postgres,mongodb}/`. Schema (DDL/collections) is **byte-compatible** with the existing Kotlin implementations — same table names, same column names — so the same physical database can be opened by either broker. Critical files to read before porting each backend:

- `broker/src/main/kotlin/stores/dbs/sqlite/MessageStoreSQLite.kt`
- `broker/src/main/kotlin/stores/dbs/sqlite/MessageArchiveSQLite.kt`
- `broker/src/main/kotlin/stores/dbs/sqlite/SessionStoreSQLite.kt`
- `broker/src/main/kotlin/stores/dbs/sqlite/UserStoreSqlite.kt`
- `broker/src/main/kotlin/stores/dbs/sqlite/ArchiveConfigStoreSQLite.kt`
- `broker/src/main/kotlin/stores/dbs/sqlite/DeviceConfigStoreSQLite.kt`
- `broker/src/main/kotlin/stores/dbs/sqlite/MetricsStoreSQLite.kt`
- (and the matching `postgres/` and `mongodb/` siblings)

We use the **V2 (PGMQ-style single-table) queue store** schema only — V1 is legacy.

---

## How mochi-mqtt Hooks Wire to Our Stores

mochi exposes a `Hook` interface; we register one composite hook (or several) that fan out to our store interfaces:

| mochi hook callback | Routed to |
|---|---|
| `OnConnectAuthenticate` | `auth.Cache.Validate(username, password)` |
| `OnACLCheck` | `auth.Cache.CheckTopic(username, topic, write)` |
| `OnSessionEstablished` | `SessionStore.SetClient(...)` + `RetainedStore.FindMatchingMessages(...)` for matching subs |
| `OnDisconnect` | `SessionStore.SetConnected(clientId, false)`; if last-will, schedule + publish |
| `OnSubscribed` / `OnUnsubscribed` | `SessionStore.AddSubscriptions / DelSubscriptions` **and** mirror into `topic.SubscriptionIndex` |
| `OnPublished` | `archive.Manager.Dispatch(msg)` → fan-out to matching ArchiveGroups (last-value + history buffer) + `pubsub.Publish` for GraphQL `topicUpdates`. The queue hook resolves offline persistent subscribers via `topic.SubscriptionIndex.FindSubscribers` (O(1) + O(depth)) instead of scanning the whole subscription table. |
| `OnRetainMessage` | `RetainedStore.AddAll([msg])` (or delete on empty payload) |
| `OnQosPublish` / `OnQosComplete` | in-flight bookkeeping (stored in-memory; persisted via `QueueStore` for offline persistent sessions) |

Mochi already implements MQTT 5 topic aliases, flow control, will delay, session expiry, retained-handling — we don't reimplement them.

---

## GraphQL Surface (subset, schema-parity with existing broker)

Strategy: **copy the relevant `.graphqls` files** from `broker/src/main/resources/` into `monster-mq-edge/internal/graphql/schema/`, then **delete or stub bridge-specific blocks**. Keep type definitions and field names byte-identical so existing dashboard queries continue to work.

### Keep (intact)
- `schema-types.graphqls` — almost all types reused: `BrokerMetrics`, `Session`, `SessionMetrics`, `MqttSubscription`, `MqttClientMetrics`, `ArchiveGroupMetrics`, `BrokerConfig`, `Broker`, `CurrentUser`, `MessageStoreType`, `MessageArchiveType`, `PayloadFormat`, `DataFormat`, `OrderDirection`. Drop only the `OpcUaDeviceMetrics` etc. bridge-specific metric types.
- From `schema-queries.graphqls`: `currentUser`, `currentValue(s)`, `retainedMessage(s)`, `archivedMessages`, `aggregatedMessages`, `systemLogs`, `searchTopics`, `browseTopics`, `brokerConfig`, `broker(s)`, `sessions`, `session`, `users`, `archiveGroups`, `archiveGroup`, `mqttClients`. **Drop**: `opcUaDevices`, `opcUaServers`, `kafkaClients`, `natsClients`, `redisClients`, `winCCOaClients`, `winCCUaClients`, `plc4xClients`, `neo4jClients`, `getDevices`.
- From `schema-mutations.graphqls`: `login`, `publish`, `publishBatch`, `purgeQueuedMessages`, `user.*`, `session.removeSessions`, `archiveGroup.*`, `mqttClient.*`. **Drop** every other `*Device`/`*Client` mutation group.
- From `schema-subscriptions.graphqls`: `topicUpdates`, `topicUpdatesBulk`, `systemLogs`. (All three, intact.)
- The MQTT bridge subschema — read existing resolvers under `broker/src/main/kotlin/devices/mqttclient/MqttClientConfigQueries.kt` and `MqttClientConfigMutations.kt` to mirror exactly.

### Stub (return constants)
- `Broker.enabledFeatures` returns e.g. `["MqttBroker", "MqttBridge"]`. The dashboard uses this to hide pages, so stubbing is cleaner than dropping.
- `Broker.isLeader` returns `true` (single node).
- `BrokerConfig.clustered` returns `false`; `mcpEnabled`, `prometheusEnabled`, `genAiEnabled`, `i3xEnabled`, `kafkaServers`, `crateDbUrl` etc. return their disabled defaults.

### Drop schemas entirely
`schema-agents.graphqls`, `schema-flows.graphqls`, `schema-genai*.graphqls`, `schema-influxdb-logger.graphqls`, `schema-mcp-servers.graphqls`, `schema-sparkplugb-decoder.graphqls`, `schema-timebase-logger.graphqls`, `schema-topic-schema.graphqls`.

### Resolver wiring
gqlgen generates strongly-typed resolver interfaces from the SDL. Each resolver method calls into the appropriate store or in-process bus. The `topicUpdates` subscription uses an in-memory pub/sub fed from the `OnPublished` hook (no message-bus abstraction needed — single node).

---

## MQTT Bridge

Files to mirror:
- `broker/src/main/kotlin/devices/mqttclient/MqttClientConnector.kt` — per-remote-broker client.
- `broker/src/main/kotlin/devices/mqttclient/MqttClientExtension.kt` — coordinator.
- `broker/src/main/kotlin/devices/mqttclient/MqttTopicTransformer.kt` — topic remap.

Implementation in `monster-mq-edge/internal/bridge/mqttclient/`:
- `manager.go` — on startup: `DeviceConfigStore.GetEnabledDevicesByNode(nodeId)`, deploy a connector per device. On GraphQL mutation: reload diff.
- `connector.go` — paho client. Outbound (subscribe locally via mochi, publish to remote). Inbound (subscribe on remote, inject into mochi via `server.Publish`). TLS, WS, MQTT v3.1.1 + v5.
- Buffering when remote is down: optional disk-backed buffer using a small SQLite file (matches existing behavior).

---

## Configuration (`config.yaml`)

The edge ships its own draft-07 schema at `yaml-json-schema.json` (root of
this repo); editors with yaml-language-server can validate against it
directly. Field set is a compatible subset of the Java broker's schema —
shared keys (NodeId, listeners, store-types, UserManagement, Metrics,
GraphQL) keep the same names so configs are mostly portable.

```yaml
NodeId: edge-rpi-01
TCP:  { Enabled: true,  Port: 1883 }
TCPS: { Enabled: false, Port: 8883, KeyStorePath: ..., KeyStorePassword: ... }
WS:   { Enabled: true,  Port: 1884 }
WSS:  { Enabled: false, Port: 8884 }
MaxMessageSize: 1048576

DefaultStoreType: SQLITE         # SQLITE | POSTGRES | MONGODB
SessionStoreType: SQLITE
RetainedStoreType: SQLITE
ConfigStoreType: SQLITE

SQLite:   { Path: "./data/monstermq.db" }
Postgres: { Url: "jdbc:postgresql://...", User: ..., Pass: ... }
MongoDB:  { Url: "mongodb://...", Database: "monstermq" }

UserManagement:
  Enabled: true
  PasswordAlgorithm: BCRYPT
  AnonymousEnabled: false
  AclCacheEnabled: true

Metrics: { Enabled: true, CollectionIntervalSeconds: 1, RetentionHours: 168 }

Logging:
  Level: INFO
  MqttSyslogEnabled: false
  RingBufferSize: 1000

GraphQL: { Enabled: true, Port: 8080 }

Features:
  MqttClient: true                      # the only subsystem feature flag today

# ArchiveGroups + per-device MqttClient configs are loaded from the database
# (IArchiveConfigStore + IDeviceConfigStore), not from YAML — same as the JVM broker.
```

CLI flags mirror `run.sh`: `-config`, `-log-level`, `-version`. No `-cluster` flag.

---

## Build / Cross-Compile / Package

- `make build` → host-arch binary in `bin/`.
- `make build-arm64` → `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/monstermq-edge-arm64`.
- `make build-armv7` → `GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 ...`.
- `Dockerfile` uses `--platform=$BUILDPLATFORM` and `buildx` for `linux/amd64,linux/arm64` images.
- `modernc.org/sqlite` is pure-Go → `CGO_ENABLED=0` works for all backends.

Expected binary size: 20–30 MB. Idle RSS: 25–50 MB. (No UI is bundled
with the binary — the GraphQL endpoint is the only HTTP surface.)

---

## Phased Milestones

Each milestone is independently shippable / testable.

1. **Skeleton + config + logging** — module bootstrap, YAML loader against JSON Schema, slog wiring, `--version` flag, smoke `go run`.
2. **Mochi-mqtt with in-memory stores + anonymous auth** — TCP/WS listeners, retained/sessions/queue/subscriptions all in-memory. Use `mosquitto_pub`/`mosquitto_sub` for end-to-end check, including QoS 1/2 and retained.
3. **SQLite backend** — implement all seven store interfaces, ensure tables are byte-compatible with the Kotlin SQLite implementations (open the same file with both brokers). Restart-survives test for retained + persistent sessions + queued messages.
4. **Users + ACL** — IUserStore + bcrypt + ACL cache + mochi auth/ACL hooks. CLI/SQL bootstrap of an admin user. Verify with authenticated `mosquitto_pub`/`_sub`.
5. **Archive groups** — IArchiveConfigStore + ArchiveGroup orchestrator + last-value store + message archive (SQLite). Verify by publishing and reading rows from SQLite directly.
6. **GraphQL read surface** — gqlgen integration, copy/slim `.graphqls` files, implement queries (`currentValue(s)`, `retainedMessage(s)`, `archivedMessages`, `aggregatedMessages`, `searchTopics`, `browseTopics`, `sessions`, `broker(s)`, `brokerConfig`, `users`, `archiveGroups`). Test against the existing dashboard pages that use these queries.
7. **GraphQL write surface + subscriptions** — `publish`, `publishBatch`, `purgeQueuedMessages`, `user.*`, `archiveGroup.*`, `session.removeSessions`, `topicUpdates`, `topicUpdatesBulk`, `systemLogs`. Test the dashboard "Publish" page and live subscription panes.
8. **Metrics** — in-memory counters drained on a 1s tick, persisted via IMetricsStore. Implement `Broker.metrics`, `metricsHistory`, `Session.metrics`. Verify dashboard graphs.
9. **PostgreSQL + MongoDB backends** — port the SQLite implementations, share the same DDL/collection conventions as the Kotlin code. Run an integration matrix.
10. **MQTT bridge** — IDeviceConfigStore + manager + paho-based connector with TLS/WS, topic transform, optional disk buffer. Implement the `mqttClient.*` GraphQL queries/mutations and the dashboard's MQTT-bridge page.
11. ~~**Dashboard packaging**~~ — **dropped.** The edge does not host a UI;
    an external dashboard consumes the GraphQL API. Schema parity with
    the Java broker preserved so the same dashboard works against either
    backend.
12. **TLS + WSS listeners**, system-log MQTT publishing, retention/purge background loops.
13. **Docker multi-arch image + systemd unit + README**.

---

## Critical Files to Read Before Implementation

All Kotlin source paths below live in the sibling repo `../monster-mq/`
(the JVM broker). For every milestone, read the **Kotlin original first**
and mirror behavior. Key entry points:

- Mochi hooks ↔ MonsterMQ behavior:
  - `broker/src/main/kotlin/MqttClient.kt` (per-connection state machine)
  - `broker/src/main/kotlin/handlers/SessionHandler.kt` (session/subscription lifecycle)
- Storage interfaces:
  - `broker/src/main/kotlin/stores/IMessageStore.kt`, `IMessageArchive.kt`, `ISessionStoreAsync.kt`, `IUserStore.kt`, `IArchiveConfigStore.kt`, `IDeviceConfigStore.kt`, `IMetricsStore.kt`
- Backend reference (SQLite):
  - `broker/src/main/kotlin/stores/dbs/sqlite/*.kt` — copy DDL exactly
- Archive group orchestration:
  - `broker/src/main/kotlin/handlers/ArchiveGroup.kt`
  - `broker/src/main/kotlin/handlers/ArchiveHandler.kt`
- MQTT bridge:
  - `broker/src/main/kotlin/devices/mqttclient/MqttClientExtension.kt`
  - `broker/src/main/kotlin/devices/mqttclient/MqttClientConnector.kt`
  - `broker/src/main/kotlin/devices/mqttclient/MqttTopicTransformer.kt`
- GraphQL parity:
  - `broker/src/main/resources/schema-*.graphqls` — keep type/field names byte-identical
  - `broker/src/main/kotlin/graphql/QueryResolver.kt`, `MutationResolver.kt`, `SubscriptionResolver.kt`
- Config:
  - `broker/yaml-json-schema.json` (in `../monster-mq/`) — original
    reference. The edge ships its own slimmer schema at
    `yaml-json-schema.json` in this repo's root.

---

## Verification (end-to-end)

A new test directory `monster-mq-edge/test/integration/` hosts Go tests that drive the broker as a black box. Reuse where useful the existing Python tests in `tests/` (they already exercise MQTT 3/5, GraphQL, SQLite-backed scenarios) by pointing them at the new broker — schema parity is the explicit goal that makes this possible.

Required green checks before declaring a milestone done:

- **MQTT compliance**: paho test suite (`paho.mqtt.testing`), QoS 0/1/2 round-trip, retained, will, persistent sessions, MQTT 5 properties (topic alias, session expiry, user properties).
- **Storage compatibility**: open the same SQLite file with the Kotlin broker and the Go broker alternately; verify retained messages, sessions, archive group rows are mutually readable.
- **GraphQL parity**: run the existing dashboard against the new broker on `localhost:8080`. Sessions, retained, archive explorer, MQTT-bridge, users, metrics pages must function unchanged.
- **Bridge**: run the new broker as an edge node, point its MQTT bridge at a Kotlin MonsterMQ; publish on edge → arrives on central; subscribe on edge → receives messages from central.
- **Memory / size**: idle RSS < 60 MB, binary < 35 MB, 10k connected clients with 100 msg/s/client sustainable on a Pi 4 (4 GB).
