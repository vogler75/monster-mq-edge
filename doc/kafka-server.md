# Kafka Protocol Server

MonsterMQ Edge can act as a **Kafka-compatible server**: standard Kafka clients (franz-go, librdkafka,
`kcat`, the Java client, ...) produce into and consume from MQTT topics of the broker. It is a port of
MonsterMQ Core's `KafkaServer` feature, with the same device config, the same GraphQL schema
(`schema-kafka-servers.graphqls`, copied verbatim) and the same database tables, so the same dashboard pages
(`kafka-servers`, `kafka-server-detail`, `kafka-topics`, `kafka-groups`) and the same database work against
both brokers.

It is **not** a Kafka cluster: one broker node, one partition per topic, no real consumer-group coordination.
It is separate from the [Kafka client bridge](kafka-client.md), which connects the broker to an external
cluster.

---

## Enabling

```yaml
Features:
  KafkaServer: true   # default false
```

Servers are `Kafka-Server` devices managed over GraphQL (admin only). One device is one listener; several
servers on different ports are fine. `add`, `update`, `toggle` stop and redeploy the server, `delete` stops it.
With the feature off, the queries return empty results and the mutations answer
`KafkaServer feature is not enabled on this node`.

```graphql
mutation {
  kafkaServer {
    add(input: {
      name: "edge-kafka", namespace: "edge-kafka", nodeId: "*",
      host: "0.0.0.0", port: 9092,
      streams: [
        { streamName: "sensors", topicFilter: "sensors/#", retentionHours: 168, allowWrite: true }
      ]
    }) { success errors server { name status } }
  }
}
```

### Device config

| Field | Meaning |
|-------|---------|
| `name` | `^[a-zA-Z0-9_-]+$` |
| `namespace` | required, unused |
| `nodeId` | `*` or the node ID of this broker |
| `host`, `port` | listener, default `0.0.0.0:9092` (plain TCP, no TLS) |
| `advertisedHost`, `advertisedPort` | returned in Metadata; default: `host` (`0.0.0.0` resolves to the machine address) and the listener port |
| `storeType` | server-wide store type, see below |
| `streams[]` | `streamName` (Kafka topic), `topicFilter` (MQTT filter), `retentionHours` (default 168), `storeType`, `allowWrite` (default true, gates Produce) |

`status` is `RUNNING`, `STARTING`, `ERROR` (enabled but not running on this node, e.g. the port is taken) or
`STOPPED` (disabled).

### Store type

The queue of each stream is chosen in this order: the stream's `storeType`, the server's `storeType`,
`QueueStoreType`, `DefaultStoreType`, `SessionStoreType` (unless `MEMORY`), else `SQLITE`.

* `MEMORY` is an in-memory SQLite database (lost on restart).
* `SQLITE`, `POSTGRES` (also `POSTGRESQL`) and `MONGODB`, case-insensitive, use the broker's own database.
* Any other value is the name of a stored database connection (SQLite, PostgreSQL or MongoDB).

---

## Data flow

* **MQTT to Kafka:** every message matching a stream filter is copied into that stream's queue
  (`kafka_queue_<stream>`), batched every 100 ms. The Kafka record key is the MQTT topic, the value the payload.
* **Kafka to MQTT:** a Produce is stored in the stream queue and published to MQTT at QoS 1. The record key is
  used verbatim as MQTT topic (a missing key uses the Kafka topic). The publisher client ID is the Kafka client
  ID, or `kafka-producer`. Produced records are stored **once**: the orchestrator recognises the broker's own
  publish and skips it.
* **Stream tables:** the table suffix is the stream name lowercased with every non-alphanumeric character turned
  into `_` (runs collapsed, ends trimmed, empty becomes `default`), `kafka_queue_<suffix>` and
  `kafka_offsets_<suffix>`. DDL is identical to Core on SQLite, PostgreSQL and MongoDB.
* **Routing:** a message goes to the first stream (in configuration order) whose filter matches, else to the
  stream named like the MQTT topic, else it is dropped. Fetch and offsets pick the stream by Kafka topic.
* **Retention:** each stream prunes with its own `retentionHours`, 5 s after start and then hourly.
* **Authorization:** with user management on, SASL PLAIN is required; Produce needs publish permission and the
  stream's `allowWrite`, Fetch needs subscribe permission on the stream's topic filter.

## Supported APIs (15)

Produce (0..7), Fetch (0..10), ListOffsets (0..3), Metadata (0..4), OffsetCommit (0..7), OffsetFetch (0..1),
FindCoordinator (0), JoinGroup (0..2), Heartbeat (0..2), LeaveGroup (0..2), SyncGroup (0..2),
SaslHandshake (0..1), ApiVersions (0), SaslAuthenticate (0..1), DescribeConfigs (0..1).

* RecordBatch v2 and MessageSet v0/v1 are decoded; GZIP only. Fetch answers with MessageSet v0.
* Group coordination is stateless as in Core: every member leads its own one-member group, SyncGroup echoes the
  caller's assignment. Committed offsets are persisted in `kafka_offsets_<stream>`.
* Not supported (as in Core): TLS, flexible versions, more than one partition, snappy/lz4/zstd, transactions,
  record headers.

## GraphQL

Queries `kafkaServers`, `kafkaServer(name)`, `kafkaConsumerGroups`, `kafkaMessages(serverName, topic,
startOffset, limit)`, `kafkaTopicOffsets(serverName, topic)`; mutation group
`kafkaServer { add, update, delete, toggle, reassign, deleteConsumerGroup }`. `reassign` accepts only this node
or a PeerLink peer. `kafkaMessages`, `kafkaTopicOffsets` and `kafkaConsumerGroups` read the stores of the
**running** servers; a stopped server shows no messages. Consumer-group `lag` covers all streams of all running
servers.

---

## Differences from the main broker

The main broker had several defects that this port fixes (tracked for Core in
[vogler75/monster-mq#237](https://github.com/vogler75/monster-mq/issues/237)), plus a few protocol deviations.

Fixed defects:

* Produce no longer stores a record twice when the topic also matches a stream filter.
* Long-poll Fetch is woken by new data of its stream (Core waits out `max_wait_ms` unless the Kafka topic and
  MQTT topic names are equal).
* Retention is per stream (Core uses the maximum over all streams).
* Routing follows the configuration order (Core: hash-map order).
* A failed queue write returns error 56 (`KAFKA_STORAGE_ERROR`), nothing is published (Core answered success).
* Unsupported compression returns 76, a rejected write 29 (Core: 13 and 1).
* Multi-partition Fetch, ListOffsets and OffsetCommit requests are parsed correctly; the v1 layouts of
  SaslAuthenticate, OffsetCommit and FindCoordinator are complete.
* `kafkaServers` returns `advertisedHost`/`advertisedPort`; consumer-group lag covers all stores.
* Store type `PostgreSQL` is recognised (Core falls back to in-memory SQLite).
* `Kafka-Stream` devices and the YAML `KafkaServer:` block are not ported.

Protocol deviations:

* Fetch high watermark is `latest + 1` (the next offset), consistent with ListOffsets (Core: `MAX(offset_id)`).
* ApiVersions with a version above 0 answers error 35 plus the v0 key list (KIP-511), so clients fall back.
* `acks=0` Produce sends no response (Core answers, which desynchronises clients).
* SaslHandshake with an unsupported mechanism returns error 33; SASL PLAIN only. A failed SaslAuthenticate
  returns 58 and closes the connection.
* A partition other than 0 returns error 3 (unknown topic or partition) on Produce, Fetch, ListOffsets and
  OffsetCommit.
* The Fetch ACL check uses the stream's topic filter instead of the Kafka topic name; a denied fetch returns an
  empty result with error 0, as Core.
* Produce stores into the stream named like the Kafka topic and the stream the orchestrator would pick for the
  MQTT topic (deduplicated).

## Footprint

Measured on this development machine with `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"` against the
commit before the port (dashboard assets included in both builds):

| Target | Before | With Kafka server | Delta |
|--------|-------:|------------------:|------:|
| windows/amd64 | 71,485,952 B | 72,148,480 B | +662,528 B (+0.9 %) |
| linux/arm64 | 66,257,056 B | 66,781,344 B | +524,288 B (+0.8 %) |

No new dependency: the codec is hand-written and `franz-go` is only linked for the Kafka client bridge. With the
feature off nothing runs and no listener or goroutine is started. Runtime memory on Pi-class hardware has not
been measured yet.

## Tests

`go test ./internal/kafka/...` (codec, protocol, orchestrator against `franz-go`), `go test
./internal/stores/...` (queue stores; PostgreSQL and MongoDB with `EDGE_TEST_POSTGRES_DSN` /
`EDGE_TEST_MONGODB_URI`) and `go test ./test/integration -run TestKafkaServer` (full broker, GraphQL,
produce/consume, offsets across a restart).
