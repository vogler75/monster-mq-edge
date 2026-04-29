# Agent guide — monster-mq-edge

This file is for AI coding agents working in this repository. Read it
before making non-trivial changes.

## What this project is

`monster-mq-edge` is the **Go port** of [MonsterMQ](https://github.com/vogler75/monster-mq),
the JVM/Kotlin MQTT broker. The mission is a **single-binary, single-node**
broker for edge devices (Raspberry Pi 4/5 class) that talks to the central
MonsterMQ over MQTT.

The full design rationale is in `dev/plans/PLAN-broker-go.md` (gitignored;
read it if you have access). The short version:

- Native MQTT broker via [`mochi-mqtt/server`](https://github.com/mochi-mqtt/server).
- GraphQL API via [`99designs/gqlgen`](https://github.com/99designs/gqlgen).
- Storage backends: SQLite (`modernc.org/sqlite`, no CGO), PostgreSQL (`pgx/v5`),
  MongoDB (`mongo-driver/v2`).
- MQTT bridge only — no OPC UA, Kafka, WinCC, NATS, Redis, Neo4j, Sparkplug,
  PLC4X, GenAI, flow engine, MCP, JDBC/InfluxDB/TimeBase loggers.
- No clustering — single node only.
- No UI is hosted. An external dashboard talks to `/graphql`.

## The non-negotiable parity rules

The two rules below define why this project exists. Any change that
violates them needs explicit user sign-off — do not silently drift.

### 1. GraphQL schema parity with the Java broker

The schema (types, field names, argument names, enum values, nullability)
must remain a **strict subset** of the Java broker's schema at
`../monster-mq/broker/src/main/resources/schema-*.graphqls`. The same
external dashboard binary is supposed to work against either backend.

Concretely:

- **Never** rename a type or field that exists in the Java schema.
- **Never** change an argument name or enum value.
- New fields are allowed if the Java broker also has them (or will).
- Fields for absent subsystems (`opcUaDevices`, `kafkaClients`, …) stay
  *droppable* — entire schema files can be omitted, but individual fields
  inside a kept type must not be renamed away.
- `enabledFeatures` reports the active subset (currently `["MqttClient"]`).
  The dashboard uses this to hide pages.

When in doubt, open the Kotlin schema file and copy verbatim. SDL files
under `internal/graphql/schema/` are the source of truth for `gqlgen`;
regenerate via `make gen` (or `go generate ./...`).

### 2. Storage-layout parity

Every backend (SQLite, Postgres, MongoDB) uses the **same DDL / collection
shape** as the Java broker. Same table names, same column names, same
types. The goal is that the same physical database can be opened by
either broker without migrations.

Reference (in the sibling repo `../monster-mq/`):

- `broker/src/main/kotlin/stores/dbs/sqlite/*.kt` — SQLite DDL is
  authoritative.
- `broker/src/main/kotlin/stores/dbs/postgres/*.kt` — Postgres DDL.
- `broker/src/main/kotlin/stores/dbs/mongodb/*.kt` — Mongo collection shape.

Use the **V2 (PGMQ-style single-table) queue store** schema. V1 is legacy
and not implemented here.

When you add a new column or table, do it in **both repos** in lockstep,
or coordinate with the user before merging.

## Repository layout

```
cmd/monstermq-edge/        # main entrypoint
internal/
  broker/                  # mochi-mqtt server + hooks (storage, queue, auth)
  config/                  # YAML config parsing
  stores/                  # MessageStore / SessionStore / RetainedStore /
                           # QueueStore / UserStore / ArchiveConfigStore /
                           # DeviceConfigStore / MetricsStore — and {sqlite,
                           # postgres,mongodb,memory}/ implementations.
  topic/                   # dual-index (Exact + Wildcard Tree) subscription
                           # resolver, ported from data/SubscriptionManager
                           # in the JVM broker. Used by the queue hook so it
                           # doesn't scan the subscription table per publish.
  archive/                 # ArchiveGroup orchestrator + retention loop
  bridge/mqttclient/       # paho-based outbound MQTT bridge
  auth/                    # bcrypt + ACL cache
  graphql/                 # gqlgen schema, generated, resolvers, server
  pubsub/                  # in-process bus for `topicUpdates` subscription
  metrics/, log/, version/
test/integration/          # black-box tests (Go; drive the full broker)
config.yaml.example        # sample config — Features, no Bridges/Dashboard
yaml-json-schema.json      # draft-07 schema for config.yaml (root of repo)
dev/plans/                 # gitignored design docs (PLAN-broker-go.md,
                           # PLAN-inline-mochi.md)
```

## Conventions and rules

- **Go 1.25**. Generics are fine. Don't introduce CGO — pure Go is a
  shipping requirement for cross-compile to ARM.
- Storage interfaces in `internal/stores/interfaces.go` are the contract.
  Backends implement them; consumers depend only on the interface.
- Hooks in `internal/broker/hook_*.go` are the bridge between mochi-mqtt
  and our stores. Keep the hot path (`OnPublished`) cheap; per-publish
  database scans are not acceptable — use `topic.SubscriptionIndex`
  for resolution.
- New config fields land in `internal/config/config.go` **and** in
  `yaml-json-schema.json` **and** (if they affect default deployment) in
  `config.yaml.example`.
- Tests live in `test/integration/` and drive the broker through its real
  network listeners. Don't add unit-test-only abstractions that mock the
  broker out of the loop.
- No emojis in code or comments. No comments that just narrate what the
  code obviously does. Save comments for non-obvious *why*.

## Build / test / run

```bash
make build              # CGO_ENABLED=0 native binary at bin/monstermq-edge
make build-arm64        # cross-compile for Raspberry Pi 4/5
make build-armv7        # cross-compile for older Pi
make test               # go test ./... (black-box integration tests)
make test-race          # with -race
make lint               # go vet ./...
make gen                # regenerate gqlgen output

./run.sh                # build + run with config.yaml
./run.sh -b -- -config test/smoke.yaml
```

The schema in `yaml-json-schema.json` validates `config.yaml` and
`config.yaml.example` — keep both as live examples.

## Cross-repo workflow

The Java broker lives at:

- **GitHub**: https://github.com/vogler75/monster-mq
- **Local sibling**: `../monster-mq/` (when checked out alongside this repo)

Useful pointers in that tree:

- `broker/src/main/resources/schema-*.graphqls` — GraphQL SDL (parity source)
- `broker/src/main/kotlin/data/SubscriptionManager.kt` — the subscription
  index this repo's `internal/topic/` is ported from
- `broker/src/main/kotlin/stores/dbs/{sqlite,postgres,mongodb}/` — DDL
  reference for storage parity
- `broker/src/main/kotlin/handlers/SessionHandler.kt` — session/subscription
  lifecycle reference
- `broker/src/main/kotlin/handlers/ArchiveGroup.kt` — archive-group
  orchestration reference
- `broker/yaml-json-schema.json` — the original (richer) config schema

When porting a feature: read the Kotlin original first, mirror behavior,
**then** decide what to slim. Don't reinvent.

## Things to never do without asking

- Rename a GraphQL type/field/enum/argument that exists in the Java schema.
- Change a column/table name in any backend store.
- Introduce CGO.
- Add a UI to this repo (an external dashboard is the consumer).
- Add clustering/Hazelcast/Kafka-bus dependencies — single-node is a
  product decision.
- Push to `main` directly, force-push shared branches, or skip git hooks.
