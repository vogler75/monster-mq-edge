# Agent guide — monster-mq-edge

This file is for AI coding agents working in this repository. Read it
before making non-trivial changes.

## What this project is

`monster-mq-edge` is the **Go port** of [MonsterMQ Core](https://github.com/vogler75/monster-mq),
the JVM/Kotlin MQTT broker. The mission is a **single-binary**
broker for edge devices (Raspberry Pi 4/5 class) that talks to central
MonsterMQ Core instances over MQTT or PeerLink.

The original design rationale is archived in `dev/done/PLAN-broker-go.md`.
Active plans live under `dev/plans/`; completed plans move to `dev/done/`.
The short version:

- Native MQTT broker engine (inlined under `internal/mqtt/`).
- GraphQL API via [`99designs/gqlgen`](https://github.com/99designs/gqlgen).
- Storage backends: SQLite (`modernc.org/sqlite`, no CGO), PostgreSQL (`pgx/v5`),
  MongoDB (`mongo-driver/v2`).
- Lightweight subset of MonsterMQ Core's device subsystems. Currently:
  MQTT bridge (`bridge/mqttclient/`), Kafka client bridge
  (`bridge/kafkaclient/`, signed off 2026-10-16), Kafka protocol server
  (`internal/kafka/`, feature flag `KafkaServer`, GraphQL
  `schema/kafkaservers.graphqls` verbatim from Core, signed off 2026-11-06),
  WinCC Unified bridge
  (`bridge/winccua/`, GraphQL or Open Pipe transports), Redis protocol server
  (`internal/redisserver/`, signed off 2026-10-17) and NATS protocol
  server (`nats/`, signed off 2026-10-17; the outbound NATS client bridge
  remains off-limits), and MCP server (`internal/mcp/`). Heavyweight ones
  (Redis client bridge, Neo4j, Sparkplug, PLC4X, GenAI, flow engine,
  JDBC/InfluxDB/TimeBase loggers, OPC UA) remain off-limits unless the
  user signs off on adding them — see the parity rule below before
  introducing any new device type.
- No UI is hosted. An external dashboard talks to `/graphql`.

## The non-negotiable parity rules

The two rules below define why this project exists. Any change that
violates them needs explicit user sign-off — do not silently drift.

### 1. GraphQL schema parity with MonsterMQ Core

The schema (types, field names, argument names, enum values, nullability)
must remain a **strict subset** of MonsterMQ Core's schema at
`../main/broker/src/main/resources/schema-*.graphqls`. The same
external dashboard binary is supposed to work against either backend.

**CRITICAL: AVOID CHANGING THE GRAPHQL INTERFACE**:
- **Avoid changes at all costs**: The GraphQL interface is the central shared contract connecting MonsterMQ Core (`monster-mq`), Go edge broker (`monster-mq-edge`), web dashboard (`monster-mq-dashboard`), and external integrations. Modifying or extending the GraphQL interface (types, fields, queries, mutations, subscriptions, inputs, arguments, or enums) must be strictly avoided whenever possible.
- **Human commitment required**: Any change to the GraphQL interface must ONLY be done with explicit commitment from the human. Never modify the GraphQL schema or resolvers unilaterally.
- **Give a clear hint if the GraphQL interface must be changed**: If a task, feature, or bugfix appears to necessitate changing or extending the GraphQL interface:
  1. **Flag it clearly and prominently**: Immediately give a clear hint to the human up front before making any changes.
  2. **Explain the rationale**: Explain why the interface change is needed and what alternatives were evaluated to avoid altering the interface.
  3. **Detail the impact**: Specify the exact proposed schema modifications and detail the cross-ecosystem impact (including compatibility between both brokers and the web dashboard).
  4. **Wait for commitment**: Do not modify any SDL files (`internal/graphql/schema/*.graphqls`) or resolvers until the human has explicitly reviewed and committed to the change.

Concretely:

- **Never** rename a type or field that exists in the Java schema.
- **Never** change an argument name or enum value.
- New fields are allowed if the Java broker also has them (or will).
- Fields for absent subsystems (`opcUaDevices`, `kafkaClients`, …) stay
  *droppable* — entire schema files can be omitted, but individual fields
  inside a kept type must not be renamed away.
- `enabledFeatures` reports the active subset, using the `Features` flag names
  (`MqttClient`, `Kafka`, `KafkaServer`, `WinCCUa`, ...; see `Resolver.enabledFeatures`).
  The dashboard uses this to hide pages.

When in doubt, open the Kotlin schema file and copy verbatim. SDL files
under `internal/graphql/schema/` are the source of truth for `gqlgen`;
regenerate via `make gen` (or `go generate ./...`).

### 2. Storage-layout parity

Every backend (SQLite, Postgres, MongoDB) uses the **same DDL / collection
shape** as the Java broker. Same table names, same column names, same
types. The goal is that the same physical database can be opened by
either broker without migrations.

Reference (in the sibling checkout `../main/`):

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
  broker/                  # MQTT server bootstrap + hooks (storage, queue, auth)
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
  bridge/kafkaclient/      # MQTT <-> Kafka bridge (franz-go), KAFKA-Client devices
  kafka/                   # Kafka protocol server (Kafka-Server devices), hand-rolled codec
  bridge/winccua/          # WinCC Unified bridge (GraphQL ws + Open Pipe)
  nats/                    # NATS protocol server and JetStream KV facade
  auth/                    # bcrypt + ACL cache
  graphql/                 # gqlgen schema, generated, resolvers, server
  prometheus/              # Prometheus /metrics scrape endpoint and PromQL HTTP API
  pubsub/                  # in-process bus for `topicUpdates` subscription
  redisserver/             # Redis protocol server (RESP2 KV and Pub/Sub)
  metrics/, log/, version/
  oahost/                  # WinCC OA embedding contract (TLV, request client);
                           # oahost/simhost is an in-process OA stand-in for tests
  winccoanative/           # native winccoa/systems/<system> namespace, interests, writes
  stores/oastore/          # MMQConfigs / MMQSessions / MMQRetained / MMQUsers datapoint
                           # stores and WINCCOA last-value store & dpGetPeriod archive
  peerlink/                # PeerLink broker-to-broker forwarding: in-memory
                           # log, capture hook, source/consumer sessions;
                           # peerlink/wire/ is the mmq-peer/1 codec
  tlsutil/                 # TLS helpers (key pairs, truststores, pins, NodeId
                           # identity, self-signed peer certificates)
embed/cabi/                # cgo C ABI (c-archive) for the WinCC OA manager
embed/harness/             # plain C host exercising the ABI (make embed-test)
winccoa/manager/           # C++ WinCC OA API manager embedding the broker
test/integration/          # black-box tests (Go; drive the full broker)
config.yaml.example        # sample config — Features, no Bridges/Dashboard
yaml-json-schema.json      # draft-07 schema for config.yaml (root of repo)
dev/plans/                 # active design/implementation plans
dev/done/                  # completed or archived plans
```

## Conventions and rules

- **Go 1.26**. Generics are fine. Don't introduce CGO — pure Go is a
  shipping requirement for cross-compile to ARM. The single approved
  exception (2026-09-29) is the opt-in WinCC OA embedding library in
  `embed/cabi/` (build tag `winccoa_embed`, `make embed-lib`); the standalone
  binary must never import it. See `winccoa/doc/spec-winccoa-native.md`.
- Storage interfaces in `internal/stores/interfaces.go` are the contract.
  Backends implement them; consumers depend only on the interface.
- Hooks in `internal/broker/hook_*.go` are the bridge between the MQTT engine
  and our stores. Keep the hot path (`OnPublished`) cheap; per-publish
  database scans are not acceptable — use `topic.SubscriptionIndex`
  for resolution.
- New config fields land in `internal/config/config.go` **and** in
  `yaml-json-schema.json` **and** (if they affect default deployment) in
  `config.yaml.example` **and** `scripts/deb/config.yaml`.
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
make test               # unit tests, then the integration tests (below)
make test-integration   # integration tests only; ARGS="-test.run TestX -test.v"
make test-race          # go test ./... with -race
make lint               # go vet ./...
make gen                # regenerate gqlgen output

python make.py run                    # run bin/monstermq-edge with config.yaml
python make.py run -b -- -config test/smoke.yaml   # build, then run
python make.py build --binary         # same binary as `make build`, works without make
python make.py test-integration       # integration tests (see below)
python make.py help                   # all commands: build, docker, publish, winccoa, release, ...
```

**Integration tests on Windows.** On Windows, never run `go test ./test/integration` or `go test ./...`. Use:

- `python make.py test-integration` (PowerShell, cmd, or Git Bash, which has no `make`).

It builds `bin\integration.test.exe` and runs it from `test/integration`.

Why: the tests listen on all interfaces, and the Windows firewall asks once per program path. `go test` builds into a
new temp folder on every run, so the owner has to confirm a firewall dialog each time. The fixed path is allowed once
and stays allowed. A git worktree has its own path and asks once more.

- **Test flags.** Pass them in test-binary form, for example `python make.py test-integration -test.run
  TestPublishSubscribeQoS0 -test.v`.
- **Other packages.** Unit tests in the other packages (`go test ./internal/...`) can still use plain `go test`,
  because they bind only to the loopback interface (see below).

**Bind test listeners to 127.0.0.1.** Every listener a unit test opens binds to `127.0.0.1`: MQTT listeners, GraphQL,
Prometheus, peer link, `net.Listen`, test HTTP servers. Use port `0` or a free port taken from `127.0.0.1:0`. Never
bind to `0.0.0.0`, `""` or `:port`. The Windows firewall only asks about listeners on other interfaces, so a
loopback-only test binary such as `graphql.test.exe` runs without a dialog, even though `go test` puts it on a new
path every run. Where a server falls back to `0.0.0.0` when its address is unset (for example `GraphQL.Address`),
set the address to `127.0.0.1` in the test config. `httptest.NewServer` already binds to `127.0.0.1`.

The schema in `yaml-json-schema.json` validates `config.yaml` and
`config.yaml.example` — keep both as live examples.

## Cross-repo workflow

MonsterMQ Core lives at:

- **GitHub**: https://github.com/vogler75/monster-mq
- **Local sibling**: `../main/` (the `monster-mq` repo, checked out next to this one)

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

- Change, extend, or rename a GraphQL type/field/enum/argument without explicit human commitment.
- Change a column/table name in any backend store.
- Introduce CGO.
- Add a UI to this repo (an external dashboard is the consumer).
- Add Hazelcast/Kafka-bus dependencies (the Kafka client bridge's franz-go
  client, approved 2026-10-16, is the exception; Kafka must never become the
  internal message bus; the Kafka protocol server uses no Kafka library).
- Push to `main` directly, force-push shared branches, or skip git hooks.
