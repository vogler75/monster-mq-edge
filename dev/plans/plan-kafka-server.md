# Kafka Protocol Server Implementation Plan

Status: proposed, awaiting owner sign-off and GraphQL commitment (2026-10-03).

Edge gets the main broker's Kafka server (`KafkaServer` feature) as a 1:1 port:
same config keys, same wire behaviour, same GraphQL schema, same storage
tables. The source of truth is the main broker's code (`../main/broker`), not
the upstream Kafka specification; every deviation the edge needs is listed
below.

Companion plan: [plan-nats-server.md](plan-nats-server.md). NATS goes first.

## Context & current state

The main broker ships a Kafka protocol server; the edge broker has none. It is
separate from the Kafka client bridge and the Kafka message bus, which this plan
does not port.

### Main broker (JVM)

| | Kafka server |
| --- | --- |
| Source | `extensions/KafkaProtocolServer.kt` (1,782 lines), `devices/kafkaserver/`, `handlers/KafkaStreamOrchestrator.kt`, `stores/*KafkaQueueStore*` |
| Switched on by | Features flag `KafkaServer` plus a Kafka server device config |
| Configured via | GraphQL, `schema-kafka-servers.graphqls` |
| Storage | `kafka_queue` and `kafka_offsets` (SQLite, Postgres, MongoDB) |

The Features name `Kafka` belongs to the client bridge, not to this server.

### monster-mq-edge (Go) today

- Single static binary, zero CGO (only exception: the opt-in WinCC OA
  embedding library); MQTT via the in-house MQTT engine in `internal/mqtt/`.
- SQLite, PostgreSQL and MongoDB with the main broker's schemas; device configs
  with one manager per device type (`bridge/mqttclient`, `bridge/winccua`);
  PeerLink broker-to-broker forwarding.
- AGENTS.md lists Kafka as off-limits without owner sign-off. This plan is that
  sign-off request; AGENTS.md gets updated with the ported server.

### Main broker building blocks and their edge counterparts

| Main broker | Edge |
| --- | --- |
| `SessionHandler.subscribeInternalClient` / `unsubscribeInternalClient` | `mqtt.Server.Subscribe` / `Unsubscribe` (inline subscription) |
| `SessionHandler.publishMessage` | `mqtt.Server.InjectPacket` with an inline client of the same ID, so storage hooks and archives see the same publisher |
| `UserManager.authenticate`, `canPublish`, `canSubscribe`, enabled `Anonymous` user | `auth.Cache` (bcrypt, ACL) and `UserManagement.AnonymousEnabled` |
| `IDeviceConfigStore` + extension verticle | `stores.DeviceConfigStore` + a manager like `bridge/winccua.Manager` |
| `IKafkaQueueStore` + SQLite/Postgres/MongoDB stores | new `KafkaQueueStore` interface in `stores/interfaces.go` + three backends |

## Parity rules for the port

The server is an in-process adapter over the local MQTT core, as in the main
broker, and behaves exactly like the main broker's code.

1. **Behaviour comes from the main broker's code.** Where it differs from
   upstream Kafka (record key used verbatim as MQTT topic), edge differs the
   same way.
2. **Same config keys.** Features flag `KafkaServer` plus device configs. No new
   `Kafka:` YAML block; in the main broker that name configures the client
   bridge and the Kafka message bus.
3. **GraphQL copied verbatim.** `schema-kafka-servers.graphqls` unchanged;
   adding it to edge still needs the owner's commitment under AGENTS.md.
   `enabledFeatures` gains `KafkaServer`.
4. **Same storage.** `kafka_queue` and `kafka_offsets` get the main broker's DDL
   and MongoDB shapes for all three backends.
5. **Zero CGO, no protocol library in the broker.** The main broker hand-rolls
   the codec (`KafkaBufferReader/Writer`); edge does the same. `franz-go`
   appears only in `test/integration`.

The main broker has no shared topic-mapping module and Kafka keys need none.
Edge keeps it that way; the earlier `internal/protocolmap/` idea is dropped.

## Issue — Kafka server, port of the KafkaServer feature

**Title:** Port the main broker's Kafka server (`KafkaServer` feature) to the
edge broker

**Summary:** Kafka servers are `Kafka-Server` device configs managed over
GraphQL. Each runs a plain-TCP listener for the Kafka wire protocol, a stream
orchestrator that copies matching MQTT messages into per-stream
`kafka_queue_<stream>` tables, and a stateless group coordinator with offsets
in `kafka_offsets_<stream>`. Implemented in `internal/kafka/`, following
`KafkaProtocolServer.kt`, `KafkaServerExtension.kt`,
`KafkaStreamOrchestrator.kt` and `KafkaMultiplexingQueueStore.kt`.

**Motivation:** Standard Kafka clients and the dashboard's Kafka pages
(`kafka-servers`, `kafka-server-detail`, `kafka-topics`, `kafka-groups`) work
against edge unchanged, on the same database.

### Scope

One phase: a server cannot even be created without the GraphQL CRUD, and the
protocol part is a fixed set of 15 APIs with stub group coordination.

- Not ported, because the main broker has none of it: TLS, flexible API
  versions, more than one partition, real consumer-group rebalancing,
  snappy/lz4/zstd, transactions, record headers, Kafka-specific metrics.
- Not ported, because edge has no cluster: the Hazelcast advertised-host lookup,
  `"*"` deploying on every node, reassigning to other cluster nodes.

### Configuration model

| Item | Main broker |
| --- | --- |
| Device type | `Kafka-Server`; one device per listener, several servers on different ports |
| Device fields | `name` (`^[a-zA-Z0-9_-]+$`), `namespace` (required, unused), `nodeId`, `enabled` |
| `config` JSON | `{"host":"0.0.0.0","port":9092,"advertisedHost"?,"advertisedPort"?,"storeType":null,"streams":[{"streamName","topicFilter","retentionHours":168,"storeType":null,"allowWrite":true}]}` |
| Stream | Kafka topic `streamName` maps to MQTT filter `topicFilter`; `allowWrite` gates Produce; `retentionHours` drives pruning |
| Store type | stream, then server, then `QueueStoreType`, `DefaultStoreType`, `SessionStoreType` (unless MEMORY), `StoreType`, else SQLITE. `MEMORY` is in-memory SQLite; any other value is the name of a stored database connection |
| Lifecycle | `add`/`update`/`toggle` stop and redeploy the server, `delete` stops it; done by the edge manager's `Reload`, as for WinCC UA |
| Feature flag | `KafkaServer`; when off, the GraphQL resolvers return empty results or `KafkaServer feature is not enabled on this node` |

### GraphQL

`schema-kafka-servers.graphqls` is copied verbatim:

- Queries `kafkaServers`, `kafkaServer(name)`, `kafkaConsumerGroups`,
  `kafkaMessages(serverName, topic, startOffset, limit)`,
  `kafkaTopicOffsets(serverName, topic)`.
- Mutation group
  `kafkaServer { add, update, delete, toggle, reassign, deleteConsumerGroup }`,
  admin-only.
- `status` is `RUNNING`, `STARTING`, `ERROR` or `STOPPED`, taken from the
  running manager.

**This extends the edge GraphQL interface.** Under AGENTS.md it needs the
owner's explicit commitment before any SDL file or resolver is touched.

### Streams and storage

- **Orchestrator**, one per server: an inline subscription on every stream
  filter; a buffer flushed every 100 ms into the queue store, followed by a
  stream-updated signal per MQTT topic; retention pruning 5 s after start, then
  hourly.
- **Multiplexer**, one store per stream. Table suffix = stream name lowercased,
  every non-alphanumeric character turned into `_`, runs collapsed, ends
  trimmed, empty becomes `default`; `sensors/temp` becomes
  `kafka_queue_sensors_temp`.
- **Routing:** enqueue picks the store by MQTT topic (first matching filter,
  else the stream named like the topic, else drop). Fetch and offsets pick it by
  Kafka topic (stream name, else filter match) and return the whole stream
  table.

| Backend | Queue table / collection | Offsets table / collection |
| --- | --- | --- |
| SQLite | `offset_id INTEGER PRIMARY KEY AUTOINCREMENT`, `topic TEXT`, `payload BLOB`, `qos INTEGER DEFAULT 1`, `publisher_id TEXT`, `creation_time INTEGER` (ms), `message_uuid TEXT`; indexes `_topic_offset_idx`, `_creation_idx` | `group_id`, `topic`, `partition_id DEFAULT 0`, `committed_offset`, `last_commit_time` (epoch s); PK `(group_id, topic, partition_id)` |
| PostgreSQL | same columns with `BIGINT GENERATED ALWAYS AS IDENTITY`, `BYTEA`, `VARCHAR(36)` UUID; indexes `_topic_offset_idx`, `_creation_time_idx` | same, `last_commit_time TIMESTAMPTZ DEFAULT now()` |
| MongoDB | same fields; `offset_id` from collection `counters` `{_id: <queue>, seq}`; index names as SQLite | `_id` = `<group>:<topic>:<partition>`, `last_commit_time` as Date |

The DDL and MongoDB shapes are copied character for character from the main
broker's `stores/dbs/*/KafkaQueueStore*.kt`.

### Protocol behaviour to copy

| Area | Main broker behaviour (`KafkaProtocolServer.kt`) |
| --- | --- |
| Listener | Device `host:port`, `TCP_NODELAY`, plaintext. Advertised host = `advertisedHost`, else `host`; `0.0.0.0` resolves to the local host address, `127.0.0.1` becomes `localhost` |
| Framing | int32 size + request header v1 (`apiKey`, `apiVersion`, `correlationId`, `clientId`); size 0 or above 10 MiB closes; no flexible versions; request versions not validated; responses sent in request order; a parse error or unknown API key closes without a response |
| ApiVersions (18) | 15 keys, all min 0, max: Produce 7, Fetch 10, ListOffsets 3, Metadata 4, OffsetCommit 7, OffsetFetch 1, FindCoordinator 0, JoinGroup 2, Heartbeat 2, LeaveGroup 2, SyncGroup 2, SaslHandshake 1, ApiVersions 0, SaslAuthenticate 1, DescribeConfigs 1 |
| Metadata (3) | One broker, node 0, advertised host and port, `cluster_id` `monstermq-cluster`, controller 0; topics = requested ones, else all stream names; one partition each; unknown topics returned with error 0; no ACL |
| Produce (0) | RecordBatch v2 and MessageSet v0/v1; GZIP only, other codecs error 13; CRC, headers and timestamps ignored; null or empty values skipped. MQTT topic = record key verbatim, null key gives the Kafka topic. `canPublish` plus the stream's `allowWrite` and filter check; a failure sets error 29 for the whole partition. Accepted records go straight into the queue store, then to MQTT at QoS 1 with client ID = Kafka client ID or `kafka-producer`. `acks` ignored, `base_offset` always 0 |
| Fetch (1) | `canSubscribe` on the Kafka topic (denied gives an empty result, error 0); up to 100 rows from `fetch_offset`; high watermark = MAX(`offset_id`); records encoded as MessageSet v0 with key = MQTT topic; long-poll up to `max_wait_ms`, woken by stream updates; size limits ignored |
| ListOffsets (2) | `-1` gives MAX(`offset_id`) + 1; any other value gives MIN(`offset_id`); no timestamp lookup |
| Groups (10 to 14) | FindCoordinator returns node 0. JoinGroup makes each member leader of its own one-member group, generation 1. SyncGroup echoes the caller's own assignment. Heartbeat and LeaveGroup always succeed. Nothing is kept in memory |
| OffsetCommit / OffsetFetch (8, 9) | Persisted in `kafka_offsets_<stream>`; missing group id becomes `default-group`; nothing committed gives -1 |
| SASL (17, 36) | PLAIN only. With user management on, only keys 17, 18 and 36 are served before auth, anything else closes. A failed SaslAuthenticate returns error 58 and closes; a raw PLAIN token after the handshake gets a bare int32 0 |
| DescribeConfigs (32) | Topic: `cleanup.policy=delete`, `retention.ms=604800000`, `segment.bytes=1073741824`. Broker: `advertised.listeners=PLAINTEXT://<host>:<port>` |

Edge-only adjustments, because edge is a single node with different building
blocks:

- `nodeId` is `"*"` or the edge node ID; `reassign` accepts only the local node
  ID, like the existing edge device resolvers.
- The `KafkaServer` feature defaults to off, like every edge feature; the main
  broker defaults all features to on.
- An in-process stream-updated signal replaces the Vert.x event bus.
- ACL goes through `auth.Cache` and the auth hook's check, including the WinCC
  OA alias rule.

### Known defects in the main broker

Recommendation for each: fix in both repos at once instead of copying.

| Defect | Effect |
| --- | --- |
| Produce writes to the queue and publishes to MQTT; the orchestrator captures that publish again | Every produced record whose topic matches a stream filter is stored twice |
| Long-poll listens on the Kafka topic, producers signal the MQTT topic | Consumers wait out `max_wait_ms` unless both names are equal |
| Retention uses the maximum of all streams | Short-retention streams keep data too long |
| Multiplexer takes the first matching filter in hash-map order | Which table a message lands in is not deterministic |
| A failed queue write still answers error 0 and publishes nothing | Silent data loss |
| Error code 1 for failures, 13 for unsupported codecs | Misleading client errors |
| Fetch topic count wrong when one topic lists several partitions; v1 layouts of SaslAuthenticate, OffsetCommit and FindCoordinator incomplete | Malformed frames for those request shapes |
| `kafkaServers` query drops `advertisedHost`/`advertisedPort`; consumer-group lag looks at one store only | Wrong values in the dashboard |
| Dashboard store type `PostgreSQL` is not recognised and falls back to in-memory SQLite | Data silently not persisted |
| `Kafka-Stream` devices and the YAML `KafkaServer:` block are only half-wired | Captured messages dropped, config without effect; not ported, remove in main |

### Implementation plan / tasks

1. Storage: `KafkaQueueStore` interface plus SQLite, PostgreSQL and MongoDB
   implementations with the main DDL; store-type resolution including named
   database connections.
2. `internal/kafka/wire/`: framing, primitives (ints, zigzag varints, strings,
   arrays), RecordBatch v2 and MessageSet v0/v1 decoding, GZIP, MessageSet v0
   encoding with CRC32.
3. `internal/kafka/server.go`: per-connection loop with ordered responses, auth
   gating, SASL PLAIN.
4. API handlers: ApiVersions, Metadata, Produce, Fetch with long-poll,
   ListOffsets, group stubs, OffsetCommit/OffsetFetch, DescribeConfigs.
5. Orchestrator and multiplexer: inline subscriptions, 100 ms batch flush,
   hourly retention pruning, stream-updated signals.
6. Manager for `Kafka-Server` devices: start, stop, reload, status.
7. GraphQL: `schema-kafka-servers.graphqls` verbatim plus resolvers;
   `enabledFeatures` adds `KafkaServer`. Only after the owner's commitment.
8. Config: `Features.KafkaServer` in `config.go`, `yaml-json-schema.json`,
   `config.yaml.example`, `scripts/deb/config.yaml`.
9. Integration tests with `franz-go`, plus ports of the cases in the main
   broker's `tests/pytest_tests/kafka/test_kafka_protocol.py`: ApiVersions,
   Metadata v0 and v4 layout, MQTT-to-Kafka fetch with magic 0, device
   lifecycle, write authorization, SASL, DescribeConfigs.
10. Docs: README, a page mirroring the main broker's `doc/kafka.md`, AGENTS.md
    allowing the Kafka server, measured footprint on Pi-class hardware.

### Acceptance criteria

- The main broker's `test_kafka_protocol.py` passes against edge with the same
  provisioning, only the URLs changed.
- The dashboard's Kafka pages create, toggle and delete servers on edge and show
  topics, messages and consumer groups.
- A database written by edge's Kafka server opens in the main broker and the
  other way round.
- Records produced over Kafka reach MQTT subscribers; MQTT publishes on stream
  filters reach Kafka consumers with key = MQTT topic.
- Committed offsets survive a restart; the standalone binary stays CGO-free.

## Sequencing and decisions

**After the NATS server.** NATS proves the inline-client and session pattern
with no GraphQL change and no storage. Kafka needs the GraphQL commitment, three
store backends and about 1,800 lines of protocol code. It stays off by default:
`Features.KafkaServer: false`.

### Decisions needed

- [ ] Sign-off: allow the Kafka server on edge; AGENTS.md lists it as
      off-limits today.
- [ ] Commitment to add `schema-kafka-servers.graphqls` to the edge GraphQL
      interface.
- [ ] For the defects listed above: fix them in both repos (recommended) or copy
      them into edge.
- [ ] Footprint budget on Pi 3 / ARMv7 with the Kafka server on, measured after
      implementation.

Settled by copying the main broker: hand-rolled codec, no protocol library;
storage tables `kafka_queue` and `kafka_offsets`; Kafka keys used verbatim as
MQTT topics.

---

*Source: the main broker at `../main/broker` as read on 2026-10-03:
`extensions/KafkaProtocolServer.kt`, `devices/kafkaserver/`,
`handlers/KafkaStreamOrchestrator.kt`,
`stores/KafkaMultiplexingQueueStore.kt`, `stores/KafkaQueueStoreFactory.kt`,
`stores/dbs/*/KafkaQueueStore*.kt`, `schema-kafka-servers.graphqls`,
`tests/pytest_tests/kafka/test_kafka_protocol.py`, `doc/kafka.md`.*
