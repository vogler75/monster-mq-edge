# Plan: Kafka client bridge for the edge broker

**Status:** approved (2026-10-16): the owner signed off on 1:1 Core parity and the verbatim Core GraphQL schema.
Revised 2026-10-16 after checking the plan against Core (`main/broker`) and edge. Deliberate
differences from Core are marked **[edge-only]** and listed in one place under "Edge-only extensions".
**Scope:** port MonsterMQ Core's `KAFKA-Client` device (an MQTT <-> Kafka bridge) to the Go edge broker.
This is not the Kafka *server*; that is `dev/done/plan-kafka-server.md` (issue #22).

## Sign-off points (confirmed)

1. **New subsystem.** The owner approved adding the `KAFKA-Client` bridge (`Features.Kafka`) to edge with
   Core's behaviour. This overrides the "Kafka off-limits" note in `AGENTS.md:21-27`, which Phase 5 updates.
2. **GraphQL: Core's schema copied verbatim, no contract divergence.** Core defines the `kafkaClients` query,
   the `kafkaClient` mutation namespace (there is no singular `kafkaClient` query), the `KafkaClient*` types
   and `KafkaPayloadFormat`. The definitions are in `schema-queries.graphqls` (section docstring at ~907-921,
   definitions at 922-1009) and `schema-mutations.graphqls` (924-1007). Edge copies them verbatim into
   `internal/graphql/schema/kafkaclient.graphqls` and stays a strict subset of Core's schema, so the
   dashboard works unchanged. The copy is self-contained: it uses only the `Long` and `JSON` scalars, which
   edge already declares (`schema.graphqls:1-2`). Nothing is renamed or invented. `enabledFeatures` gains
   `"Kafka"`, matching Core (`Features.kt:7`).
3. **New production dependency.** A pure-Go Kafka client, `github.com/twmb/franz-go` (`kgo` plus
   `pkg/sasl/*`): no CGO, with TLS, SASL PLAIN/SCRAM/OAUTHBEARER, consumer groups and batching. Note:
   `plan-kafka-server.md:65-68` keeps franz-go out of the broker binary and uses it only in
   `test/integration`. This plan adds it to the **production** binary. The owner signed off on that, but
   `AGENTS.md:196` ("Add Hazelcast/Kafka-bus dependencies") must be reworded in Phase 5 so it no longer
   reads as forbidding it.

## Context

- Core (paths under `main/broker/src/main/kotlin/`):
  - `devices/kafkaclient/KafkaClientConnector.kt`
  - `devices/kafkaclient/KafkaClientExtension.kt`
  - `stores/devices/KafkaConfig.kt`
  - `graphql/KafkaClientConfig{Queries,Mutations}.kt`
  - `data/BrokerMessageCodec.kt`
  - `handlers/MetricsHandler.kt`
  - `stores/IMetricsStore.kt`
  - docs: `main/doc/kafka.md`

  Device type `KAFKA-Client` (`stores/DeviceConfig.kt:25`), feature flag `Features.Kafka`.
- Edge today has no Kafka client code. The only traces are the GraphQL broker-metrics fields
  `kafkaClientIn/Out` (`schema.graphqls:36-37`), which stay 0 because `snapshotToBrokerMetrics` never
  sets them. The existing bridges (`internal/bridge/mqttclient`, `winccua`, ...) show the pattern to follow.

## Parity rules

1. **Same device type string.** `"KAFKA-Client"` is stored verbatim. `deviceTypeToExternal/Internal`
   (`resolver.go:515-531`) pass unknown types through unchanged, so no translation is needed (unlike
   `MQTT_CLIENT`).
2. **Same config keys, defaults and normalisation as `KafkaConfig.kt`,** so configs export and import
   between Core and edge:
   - Keys are camelCase.
   - PascalCase is accepted **only** where Core accepts it (`KafkaConfig.kt:43-54`): `TopicKeyRegex`,
     `TopicKeyReplacement`, `OutboundTopicKeyRegex`, `OutboundTopicKeyReplacement`, `InboundEnabled`,
     `OutboundEnabled`, `OutboundTopicFilters`, `OutboundKafkaTopic`, `ExtraProducerConfig`.
   - Precedence is the same as Core: for the two `TopicKey*` fields the PascalCase key wins; for every
     other field the camelCase key wins.
   - `bootstrapServers`, `groupId`, `payloadFormat`, `extraConsumerConfig`, `reconnectDelayMs`,
     `destinationTopicPrefix` and `keyTopicPrefix` are camelCase only.
3. **Same GraphQL schema,** copied verbatim from Core.
4. **Same wire formats** for `DEFAULT`, `JSON`, `BINARY` and `TEXT`, so edge and Core can share a Kafka topic.
5. **Zero CGO.**

## Behaviour to port

### Config

Defaults are from `KafkaConfig.kt:12-26`:

| Key | Default and rules |
|---|---|
| `bootstrapServers` | `localhost:9092` |
| `groupId` | `monstermq-subscriber` |
| `payloadFormat` | `DEFAULT` |
| `extraConsumerConfig`, `extraProducerConfig` | Standard Kafka client properties (see below). |
| `reconnectDelayMs` | 5000, must be >= 500 |
| `destinationTopicPrefix` | Falls back to legacy `keyTopicPrefix`. Trimmed, blank becomes nil, trailing `/` appended, `+` or `#` rejected. |
| `topicKeyRegex` / `topicKeyReplacement` | Applied only when **both** are set (`KafkaClientConnector.kt:408-412`). |
| `inboundEnabled` | true |
| `outboundEnabled` | false |
| `outboundTopicFilters` | Must be non-empty when outbound is enabled. |
| `outboundKafkaTopic` | Default nil; blank is treated as nil, which means use the device namespace. |
| `outboundTopicKeyRegex` / `outboundTopicKeyReplacement` | Applied only when both are set. |

Where normalisation happens:
- **Parity:** Core stores `destinationTopicPrefix` as entered (trimmed, blank becomes null) and
  normalises it every time the config is read. Edge does the same: normalisation happens in
  `Config` unmarshaling, not when the config is saved.
- **Parity:** a wildcard in the prefix fails config parsing. In GraphQL the error surfaces as
  `"Invalid Kafka config: ..."`, and starting the connector fails.

Outer `"kafka"` nesting:
- **Parity:** the connector unwraps an optional outer `"kafka"` key (`KafkaClientConnector.kt:69`).
- **[edge-only]** Edge unwraps it in the GraphQL resolvers too. Core's resolvers call `fromJson`
  directly and silently fall back to defaults when parsing fails. Unwrapping there is harmless and
  avoids showing defaults for nested configs.

Validation beyond Core:
- **[edge-only]** On GraphQL create/update, edge also checks `topicKeyRegex` and `outboundTopicKeyRegex`
  with `regexp.Compile`, `outboundTopicFilters` with `bus.Validate`, and `payloadFormat` against the enum.
  Core validates none of these; an unknown `payloadFormat` just drops every record.
- When the connector starts (including after `importDevices`), an invalid regex or filter fails that
  connector with an error log. Other devices keep running.

### Inbound (Kafka -> MQTT)

Consumer setup:
- Consumes the Kafka topic named after `device.namespace` (`KafkaClientConnector.kt:256-257`).
- Consumer defaults (:124-135): client id `kafkaclient-<name>`, `enable.auto.commit=true`,
  `auto.offset.reset=latest`. `extraConsumerConfig` overrides these defaults (Core merges it with
  `putAll` after setting them).
- Keys are always decoded as UTF-8 strings. `record.Key == nil` is the only null case. An empty key is
  valid and becomes topic `""`.

Formats:
- **`BINARY` / `TEXT`.**
  - The MQTT topic is `applyDestinationPrefix(applyTopicKeyRegex(key))`.
  - A nil key is dropped and `messagesDropped` is incremented.
  - Published with QoS 0, no retain, client id `kafkaclient-<name>`.
- **`DEFAULT`.**
  - The value is a binary `BrokerMessageCodec` envelope.
  - If binary decoding fails, log a warning (the hint about the `payloadFormat` setting, as in Core)
    and try the JSON envelope instead.
  - A JSON record on a `DEFAULT` device therefore works, at the cost of one warning per record.
- **`JSON`.** A JSON envelope with these fields:

  | Field | Rule |
  |---|---|
  | `topicName`, else `topic` | Drop the record if both are missing. |
  | `payloadBase64` (base64), else `payload` (UTF-8 string) | Empty payload if neither is present. |
  | `messageUuid` | Random UUID if missing. |
  | `messageId` | 0 |
  | `qosLevel` | 0 |
  | `isRetain`, `isDup`, `isQueued` | false |
  | `clientId` | `kafkaclient-<name>` if missing. |
  | `time` | ISO-8601; current time if missing or invalid. |

Topic prefix:
- `destinationTopicPrefix` is applied as `prefix + strings.TrimLeft(topic, "/")` to **all four** formats,
  matching Core's `publishPlain` (:160) and `publishDecoded` (:429).

Local publish:
- Publish through `NewInlinePublisher(server).Publish("kafkaclient-"+name, msg)`
  (`internal/broker/inline_publisher.go:24-38`).
- **[edge-only] Client id.** Edge always publishes under `kafkaclient-<name>`. Core keeps the envelope's
  `clientId` for `DEFAULT`/`JSON` (:435). This difference is forced: `InlinePublisher` publishes under the
  ID of its inline client, and bus subscribers see `ClientID: cl.ID` (`hook_storage.go:357-366`).
  - The original client ID is not carried over.
  - In exchange, loop prevention works for every format (see Outbound).
- **[edge-only] MQTT v5 properties.** Edge keeps the v5 properties decoded from the `0x504C0001` trailer
  (content type, response topic, correlation data, user properties, message expiry, payload format
  indicator). They pass through `propsToPacket` (`internal/broker/mqtt5_props.go:21`).
  - Core's `publishDecoded` (:426-438) rebuilds the message from the basic fields only and drops all of
    these.
  - Edge never reapplies `originNodeId`, `peer`, `username` or `isWill` from the envelope.

Counters:
- **[edge-only] Counter semantics.** Edge increments `messagesIn` only for records that are actually
  published. A record that is dropped (missing key or topic, decode failure on both paths, unknown format)
  increments `messagesDropped`. A decode or parse failure also increments `errors`.
  - Core falls through to `messagesIn++` after most failure paths (:199-243), so its `messagesIn` counts
    every record consumed.
  - Document this difference in `doc/kafka-client.md`.

### Outbound (MQTT -> Kafka)

Subscription:
- Subscribe to `outboundTopicFilters` through `mqttclient.BusAdapter` with QoS 0.
- `BusAdapter` drops PeerLink replicas (`OriginNode != ""`) unless `BridgeOutbound` is set
  (`bus_adapter.go:22-41`). That matches Core's gate at `KafkaClientConnector.kt:329`.

Producer:
- Default client id `kafkaclient-producer-<name>`. `extraProducerConfig` overrides the defaults.
- Key = MQTT topic after `outboundTopicKeyRegex`.
- Destination Kafka topic = `outboundKafkaTopic` if non-blank, else `device.namespace`.

Formats:
- **`DEFAULT`:** binary `BrokerMessageCodec` envelope. The encoder **always** writes the `0x504C0001` trailer.
- **`JSON`:** a JSON object with `messageUuid`, `messageId`, `topic` (Core writes `"topic"`, not
  `"topicName"`), `payloadBase64`, `qosLevel`, `isRetain`, `isDup`, `isQueued`, `clientId` and `time`
  (ISO-8601).
- **`BINARY`:** the raw `msg.Payload` bytes.
- **`TEXT`:** `msg.Payload` as UTF-8 string bytes.

Loop prevention:
- Skip messages whose `ClientID` is `"kafkaclient-" + name`. This is the edge equivalent of Core's
  `senderId == internalClientId || clientId == internalClientId` (:326). Edge has no separate `senderId`
  for inline publishes.
- Because edge publishes inbound messages under that ID, loop prevention covers all four formats.
  Core only catches `BINARY`/`TEXT` round-trips reliably.
- A PeerLink replica keeps the source node's client ID (`hook_storage.go:397`), so a same-named Kafka
  device on a peer is also skipped when `BridgeOutbound` is on.

Anti-spoofing:
- **[edge-only]** Extend `bridgeIDHook.OnConnect` (`inline_publisher.go:63-71`) to reject network clients
  whose ID starts with `"kafkaclient-"`. `"bridge:"` is already rejected.
- Without this, a network client named `kafkaclient-<name>` would have its publishes silently skipped by
  outbound.
- Reject only when `cfg.Features.Kafka` is on, so existing clients are unaffected while the feature is off.
- Update the hook comment: the current rationale is MQTT No Local, whereas here it is bus loop prevention.
  Also update `inline_publisher_test.go:126`.

**[edge-only] Buffering and reconnect.** Core sends fire-and-forget and skips messages while no producer
exists (:331). It never uses `reconnectDelayMs`; Core's Kafka client reconnects internally.
- Edge puts a bounded in-memory channel between the bus and the producer. On overflow it drops the
  oldest message and counts it in `messagesDropped`.
- Producer send errors increment `errors`, as Core's `exceptionHandler` does (:281-284).
- franz-go reconnects to brokers by itself. `reconnectDelayMs` is used only as the backoff for
  recreating the `kgo.Client` after a fatal error (bad config, authentication failure).

### Kafka client property translation

`extraConsumerConfig` / `extraProducerConfig` hold standard Kafka client properties (the
configuration names Apache Kafka documents). Core passes them to its Kafka client with `putAll`
(`doc/kafka.md:17`) and accepts any key. **Both brokers expose the same interface:** edge accepts
every key Core accepts, never refuses one, and applies the standard properties to franz-go
(owner decision 2026-10-16):

| Kafka client property | franz-go |
|---|---|
| `security.protocol` (`PLAINTEXT`/`SSL`/`SASL_PLAINTEXT`/`SASL_SSL`) | `kgo.DialTLSConfig`, or none |
| `sasl.mechanism` + `sasl.jaas.config` (`PLAIN`, `SCRAM-SHA-256`, `SCRAM-SHA-512`, `OAUTHBEARER`) | `kgo.SASL(plain/scram/oauth)`, with username/password/token parsed from the JAAS string |
| `ssl.truststore/keystore.{location,password,type}`, `ssl.truststore.certificates`, `ssl.keystore.certificate.chain`, `ssl.keystore.key`, `ssl.key.password` (`PEM` incl. encrypted PKCS#8, `PKCS12`) | `tls.Config` `RootCAs` / `Certificates`. PKCS12 uses `software.sslmate.com/src/go-pkcs12`, encrypted PEM keys `github.com/youmark/pkcs8` |
| `ssl.endpoint.identification.algorithm` (empty = no hostname check) | `InsecureSkipVerify` plus own chain verification |
| `request.timeout.ms`, `connections.max.idle.ms`, `metadata.max.age.ms`, `socket.connection.setup.timeout.ms`, `retry.backoff.ms`/`retry.backoff.max.ms` | `RequestTimeoutOverhead`, `ConnIdleTimeout`, `MetadataMaxAge`, `DialTimeout`, `RetryBackoffFn` |
| `acks`, `enable.idempotence` | `RequiredAcks`, `DisableIdempotentWrite` |
| `compression.type`, `linger.ms`, `batch.size`, `retries`, `delivery.timeout.ms`, `max.request.size`, `buffer.memory`, `max.in.flight.requests.per.connection` | `ProducerBatchCompression`, `ProducerLinger`, `ProducerBatchMaxBytes`, `RecordRetries`, `RecordDeliveryTimeout`, `BrokerMaxWriteBytes`, `MaxBufferedBytes`, `MaxProduceRequestsInflightPerBroker`; producer `request.timeout.ms` also `ProduceRequestTimeout` |
| `group.id`, `group.instance.id`, `auto.offset.reset`, `enable.auto.commit`, `auto.commit.interval.ms`, `session.timeout.ms`, `heartbeat.interval.ms`, `max.poll.interval.ms` | `ConsumerGroup`, `InstanceID`, `ConsumeResetOffset`, `DisableAutoCommit`, `AutoCommitInterval`, `SessionTimeout`, `HeartbeatInterval`, `RebalanceTimeout` |
| `max.poll.records`, `fetch.min.bytes`, `fetch.max.bytes`, `fetch.max.wait.ms`, `max.partition.fetch.bytes`, `isolation.level`, `partition.assignment.strategy`, `allow.auto.create.topics`, `client.rack` | `PollRecords`, `FetchMinBytes`, `FetchMaxBytes`, `FetchMaxWait`, `FetchMaxPartitionBytes`, `FetchIsolationLevel`, `Balancers`, `AllowAutoTopicCreation`, `Rack` |
| `client.id`, `bootstrap.servers` | `kgo.ClientID`, `SeedBrokers` |
| `key/value.(de)serializer` | Accepted; edge picks the serialisation from `payloadFormat`, as Core does |

Both clients allow automatic topic creation by default, as the standard clients do.
- A key without an edge equivalent (e.g. `transactional.id`, a producer key in the consumer map) is
  logged when the device starts and otherwise ignored.
- A value a property does not allow (e.g. `sasl.mechanism=GSSAPI`, JKS stores, `acks=2`) fails the
  device start with an error naming the property, as it fails the Kafka client in Core. GraphQL
  accepts it, as Core does.

### Metrics

Connector counters:
- `messagesIn`, `messagesOut`, `messagesDropped`, `errors`, `lastRecordTs`.
- Per-interval rates through `SampleMetrics` / `LatestMetrics`, matching `mqttclient/connector.go:175,191`.

`metrics.Collector` (`internal/metrics/collector.go`):
- Add atomic counters `kafkaIn` / `kafkaOut` (next to :23-28), `IncKafkaIn(n int)` / `IncKafkaOut(n int)`,
  and the `Swap` plus rate computation in the tick loop (:142-164).
- Add `KafkaClientIn` / `KafkaClientOut` to `BrokerSnapshot` (:38-48) with tags `json:"kafkaClientIn"` and
  `json:"kafkaClientOut"`, matching Core's `MetricsHandler.kt:169-170`.
- Map them in `snapshotToBrokerMetrics` (`resolver.go:1859`). Broker `metricsHistory` decodes the stored
  snapshot JSON, so history picks them up automatically; older rows read as 0.

Metric store:
- Add `stores.MetricKafkaClient MetricKind = "kafka"` in `internal/stores/interfaces.go` (next to :217-221).
- This matches Core's `KAFKACLIENT -> "kafka"` (`IMetricsStore.kt:25`). It does not follow edge's own
  naming (`mqttclient`, `archivegroup`), but parity with Core wins.
- No store schema change is needed: `metric_type` is a text column in SQLite, Postgres and Mongo.
- The kind is persisted through `kafkaclient.Manager.StartMetrics`.

GraphQL:
- Add `KafkaClient.metrics` and `metricsHistory` resolvers, modelled on `mqttClientResolver.Metrics` /
  `MetricsHistory` (`resolver.go:2038,2049`).
- Core's `KafkaClientMetrics` type exposes only `messagesIn`, `messagesOut` and `timestamp`. The other
  counters are stored in the metrics JSON but are not exposed in GraphQL; exposing them would change the
  schema.

Not ported:
- Core publishes per-device metrics to `$SYS/sessions/kafka/<name>/metrics` (`MetricsHandler.kt:415`).
  Edge's `mqttclient` does not do the equivalent, so this is left out; it can be added later for all
  bridges together.

## Package layout

`internal/bridge/kafkaclient/`:
- `config.go`, `validate.go`: `DeviceTypeKafkaClient = "KAFKA-Client"`, `Config`, JSON unmarshaling
  (unwraps an optional `"kafka"` key; PascalCase aliases only for Core's set), defaults, prefix
  normalisation, regex, filter and payload-format validation.
- `kafkaprops.go`: translates Kafka client properties into kgo options, including PEM and PKCS12 TLS and JAAS
  parsing, and reports the keys without an edge equivalent.
- `codec.go`: Go port of `BrokerMessageCodec` (see below), plus JSON envelope encoding and decoding.
- `transport.go`: a thin interface over kgo (producer and consumer) so tests can use a fake.
- `connector.go`: inbound loop, outbound loop with a bounded buffer, client recreation, counters and rates.
- `manager.go`: modelled on `mqttclient.Manager`: `Start`, `Stop`, `Reload`, `Active`, `Snapshot`, `Get`,
  `StartMetrics(ctx, store, interval)`, `SetCounters(in, out func(int))`.
  - Core's `KafkaClientExtension` keeps a namespace-to-device registry (`kafkaclient.device.registry`).
    Edge doesn't need it, because the manager keys devices by name.

### `BrokerMessageCodec` layout

From `BrokerMessageCodec.kt`, big-endian.

**Strings:**
- A string is `int32 len` + UTF-8 bytes.
- A nullable string uses `len = -1` for null.
- Non-nullable strings (`uuid`, `topicName`, `clientId`, peer `sourceNode`, peer `clientId`) have no
  null handling.

**Header** (:26-38), in this order:
1. `uuid` string
2. `int32 messageId`
3. status byte: `((qos&3)<<3) | (dup<<2) | (retain<<1) | queued`
4. `topicName` string
5. `clientId` string
6. `int64 time` (epoch ms)
7. `int32 payloadLen` + bytes
8. nullable `originNodeId` string

**Trailer** (:41-79), starting with the magic `0x504C0001`. Fields in this order:
1. nullable `senderId`
2. `int64 messageExpiryInterval` (`-1` = null)
3. `int32 payloadFormatIndicator` (`-1` = null)
4. nullable `contentType`
5. nullable `responseTopic`
6. nullable `correlationData` (`int32 len`, `-1` = null)
7. nullable `userProperties` (`int32 count`, `-1` = null), followed by `count` key/value string pairs
8. `byte isWill`
9. nullable `username`
10. nullable `peerSource`
11. `byte hasPeer`, followed by the peer struct when set: `sourceNode`, `clientId`, nullable `username`,
    `int64 timeNs`, `int64 epoch`, `int64 offset`, `byte flags`. The flags byte is
    `(dup<<2) | (will<<1) | snapshot` (:75-77, decoded at :195-197).

Type mapping:
- Core's `messageExpiryInterval` is a `Long?` and `payloadFormatIndicator` is an `Int?`. Edge maps them to
  `*uint32` / `*byte`. Out-of-range values are clamped on decode and logged at debug level.
- Core's `userProperties` is a `Map<String,String>`, so duplicate keys collapse to the last value. Edge
  maps it to `[]stores.UserProperty` and keeps duplicates on encode. Byte-for-byte round-trips through
  Core are guaranteed only for messages without duplicate keys.

Decoder:
- Must tolerate a missing trailer (:136).
- Golden-byte tests use vectors derived byte by byte from `BrokerMessageCodec.kt`. Generating them
  with Core's own (JVM) encoder is not needed (owner decision 2026-10-17).

## Wiring

**`internal/config/config.go`**
- Add `Kafka bool \`yaml:"Kafka"\`` to `FeaturesConfig` (:232-243).
- Add `Kafka: false` to the default literal at :540.
- Update `yaml-json-schema.json` (Features at :262-277; it sets `additionalProperties: false`),
  `config.yaml.example` (:92-103) and `scripts/deb/config.yaml` (:82).

**`internal/broker/server.go`**
- Add a `kafkaClients *kafkaclient.Manager` field to the Server struct (:64) and to the struct literal (:735).
- Create the manager next to the mqttclient block (:573-591) when `cfg.Features.Kafka`:
  - publisher: `NewInlinePublisher(server).Publish`
  - bus: `&mqttclient.BusAdapter{Bus: bus, BridgeOutbound: cfg.PeerLink.Enabled && cfg.PeerLink.Receive.BridgeOutbound}`
  - counters: `kafkaClients.SetCounters(collector.IncKafkaIn, collector.IncKafkaOut)`, following the
    `bridges.SetCounters` pattern at :578
- In `Start()`:
  - Call `s.kafkaClients.Start(ctx)` next to the bridges start (:989-992).
  - Call `s.kafkaClients.StartMetrics(...)` next to the bridges `StartMetrics` call (:973-975), inside
    `if s.collector != nil`.
- In `stopPublishers()` (:1104), add `s.kafkaClients.Stop()` next to `bridges.Stop()` (:1109) for a clean
  shutdown and PeerLink drain.
- Optional: a `KafkaClients()` accessor like `Bridges()` (:1157).
- Pass the manager to `resolvers.New(...)`. That changes the signature (`resolver.go:78-108`) and its only
  caller (`server.go:688`).

**`internal/broker/inline_publisher.go`**
- Extend `bridgeIDHook` to reject the `"kafkaclient-"` prefix, but only when `Features.Kafka` is on.
  Update the comment and test (see Outbound).

**`internal/broker/peerlink.go`**
- `warnPeerLinkDevices` (:154) needs its own `case kafkaclient.DeviceTypeKafkaClient:`. Do **not** append
  it to the always-warn list at :209.
- Like the MQTT case (:188-208), the new case unmarshals the config and:
  - warns when any `bootstrapServers` host matches a PeerLink peer host (reuse `bridgeHost` / `peerHosts`);
  - continues to the shared-store duplicate-publish warning (`nodeId: "*"` or `"local"`) only when
    `inboundEnabled` is true.

**GraphQL**
- Add `internal/graphql/schema/kafkaclient.graphqls`, copied verbatim from Core. `gqlgen.yml` globs
  `schema/*.graphqls`, so it is picked up automatically.
- Run `make gen` (`Makefile:108-109`). gqlgen's single-file resolver layout puts the new stubs in
  `resolver.go`: `queryResolver.KafkaClients`, `mutationResolver.KafkaClient`, the root accessors
  `KafkaClient()` / `KafkaClientMutations()` and the resolver struct types (see the MqttClient ones at
  `resolver.go:172-213`). Move the bodies into `internal/graphql/resolvers/kafkaclient.go`, following the
  winccua/winccoa pattern.
- Resolvers:
  - Feature gate: mutations return `Errors: []string{"Kafka feature is not enabled on this node"}` inside
    `KafkaClientResult`, as `winccua.go:326` does and matching Core's text. The `kafkaClients` query
    returns an empty list when the feature is off (Core `KafkaClientConfigQueries.kt:27-28`).
  - `DeviceConfig` CRUD, start, stop, toggle and reassign; validation errors; `Metrics` and `MetricsHistory`.
- `internal/graphql/resolvers/resolver.go`:
  - Add `KafkaClients *kafkaclient.Manager` to `Resolver` (:44).
  - Add `"Kafka"` to `enabledFeatures()` (:114).
  - Call `KafkaClients.Reload(ctx)` in `reloadDeviceManagers` (:2633) so `importDevices` picks up imported
    devices. `ImportDevices` / `deviceInputToStore` have no type allow-list, so nothing else is needed.
  - Map `KafkaClientIn/Out` in `snapshotToBrokerMetrics` (:1859).

## Phases

1. **Foundation:** franz-go dependency, `Features.Kafka`, config and validation, GraphQL schema and
   resolvers, manager and server wiring (fields, `Start`, `Stop`, `SetCounters`, `resolvers.New`),
   collector and metric store wiring, stub transport.
2. **Codecs and outbound:** `BrokerMessageCodec` (`DEFAULT`) with golden tests, `JSON`/`BINARY`/`TEXT`
   encoding, franz-go producer, key regex, loop prevention and `bridgeIDHook` gating, bounded buffer,
   client recreation, metrics.
3. **Inbound:** franz-go consumer, all four formats (`DEFAULT` with JSON fallback, `JSON`, `BINARY`,
   `TEXT`), `topicKeyRegex`, `destinationTopicPrefix`, inline publish with MQTT v5 properties.
4. **Security and Kafka client properties:** TLS (PEM, and PKCS12 via `go-pkcs12`), SASL PLAIN/SCRAM/OAUTHBEARER,
   Kafka client property translation; every key accepted, keys without an equivalent logged.
5. **Docs and release:**
   - `doc/kafka-client.md`, mirroring Core's `kafka.md`, with an "Edge-only extensions" section.
   - README.
   - AGENTS.md:
     - Add the Kafka bridge to the supported list at :21-27.
     - Reword :196.
     - Fix the outdated `enabledFeatures` note at :59.
   - Config samples.

## Tests

**Unit**
- Config parsing:
  - nested `"kafka"`;
  - PascalCase only for Core's alias set, with Core's precedence;
  - prefix normalisation and wildcard rejection;
  - validation of regex, filter and payload format.
- Key regex: applied only when both regex and replacement are set.
- `BrokerMessageCodec` golden vectors:
  - with and without the `0x504C0001` trailer;
  - with peer flags;
  - with null and non-null v5 fields.
- Kafka client property translation (JAAS, PKCS12 and PEM), plus reporting keys without an equivalent.
- Connector against the fake transport:
  - all four formats, in both directions;
  - `DEFAULT` -> JSON fallback;
  - counter semantics on drops;
  - loop prevention for all formats;
  - PeerLink gate;
  - drop-oldest on overflow.
- `bridgeIDHook`: `kafkaclient-` is rejected only when `Features.Kafka` is on.

**Integration** (`test/integration`)
- Run against a real broker: a Redpanda container or Core's Kafka server.
- Cover outbound publish, inbound consume, a `DEFAULT` round-trip, reconnect after a broker restart,
  and SASL/TLS.
- Enable the feature with `cfg.Features.Kafka = true`, as `bridge_chain_test.go:54` does.
- `test/integration` has no build tags today. Gate these tests with `t.Skip` when no Kafka endpoint is
  reachable (for example via a `KAFKA_BOOTSTRAP` env var) rather than introducing build tags.

**Regression**
- New test: check that edge's SDL is a subset of Core's (`../main/broker/src/main/resources/*.graphqls`;
  skip if not present). No such test exists today.
- `MetricKafkaClient` (`"kafka"`) metrics are persisted on SQLite, Postgres and Mongo.
- Device import/export round-trip with a config exported from Core.

## Edge-only extensions

Deliberate differences from Core. Each one is documented in `doc/kafka-client.md`.

| # | Extension | Core behaviour | Reason |
|---|---|---|---|
| E1 | Inbound `DEFAULT`/`JSON` publish under `kafkaclient-<name>` | Keeps the envelope `clientId` | `InlinePublisher` publishes under the inline client's ID; this also makes loop prevention work for every format |
| E2 | MQTT v5 properties from the codec trailer are kept on inbound | Dropped in `publishDecoded` | Lossless Core <-> edge bridging |
| E3 | `messagesIn` counts only published records | Incremented after most failure paths | Correct metrics |
| E4 | Bounded outbound buffer with drop-oldest; `reconnectDelayMs` used as the backoff for recreating the client | Fire-and-forget; skip while no producer; `reconnectDelayMs` unused | Bounded memory; franz-go has a different reconnect model |
| E5 | GraphQL create/update checks regexes, topic filters and `payloadFormat` (never Kafka client property keys) | No checks | Fail early; the property interface stays identical to Core |
| E6 | Kafka client properties applied to franz-go; every key accepted, keys without an equivalent logged | Passed to its Kafka client | Same property interface, different client library |
| E7 | `"kafka"` nesting unwrapped in the GraphQL resolvers too | Connector only | Resolvers show the real config |
| E8 | `bridgeIDHook` rejects network clients with the `kafkaclient-` prefix (only when the feature is on) | No equivalent | Stops a network client from spoofing the loop-prevention ID |
| E9 | `$SYS/sessions/kafka/<name>/metrics` not published | Published | Edge bridges don't publish `$SYS` device metrics; deferred |
| E10 | `kafkaClient.update` refuses an input name different from the device name | Saves under the new name; the old device keeps running | Avoid an orphaned, still-running device |

Smaller, unavoidable differences (engine and library): no peer block / origin node in `DEFAULT`
records of PeerLink replicas; empty content type / response topic / correlation data encoded as
absent; `messagesOut` counts broker acks; QoS above 2 in an envelope published as 0; Go RE2 regex
syntax and empty-match handling (replacement strings follow Core exactly); WinCC OA native filters
deliver current values on subscribe; `nodeId: local` devices start at boot; GraphQL timestamps in
RFC 3339 nanoseconds. All are listed in `doc/kafka-client.md`.

## Implementation status (2026-10-17)

Implemented on `feature/kafka-client-bridge`. A line-by-line parity audit against Core found and
fixed: outbound envelopes now carry QoS 0 / no retain and the publisher as `senderId` (Core's QoS 0
internal subscription), user properties use Core's map semantics, `$` topics are not matched by
leading wildcards, TEXT and record keys replace invalid UTF-8, JSON output is not HTML-escaped;
inbound keeps the envelope UUID, time, message id, DUP and queued flags (`packets.Source`), drops
tombstones, and applies Core's JSON value rules to envelopes and configs; replacement strings use
Core's syntax and errors; GraphQL delete removes any device type, reassign checks the node
(own node and PeerLink peers), node filters include `*` devices, update stores the normalised
config; metrics come from the newest stored sample, rounded to two decimals, and history needs a
range and is newest first.

Verified live against the `scada` cluster: read-only consumption of topic `scada` (TEXT, key regex,
prefix), a round trip through topic `test` in all four formats, and recovery when the cluster is
unreachable at start (`TestKafkaClientClusterComesUpLater`: bootstrap through a local proxy that
opens later; messages published meanwhile wait in the buffer and arrive exactly once).

Full integration suite (with the live Kafka tests) passes as of b46d855, which also fixed two
unrelated tests that failed on Windows and under load.

Not needed (owner decision 2026-10-17): implementing or running Core's own (JVM) encoder to
generate the golden codec vectors. Edge's codec is checked against vectors derived byte by byte
from `BrokerMessageCodec.kt`, in both directions, and against records exchanged live with Kafka.

Not done, by owner decision (2026-10-17):
- Postgres and Mongo runs of the `kafka` metric-kind test: written
  (`internal/stores/{postgres,mongodb}/kafka_metrics_test.go`), skipped until
  `EDGE_TEST_POSTGRES_DSN` / `EDGE_TEST_MONGODB_URI` point at a test database; running them
  against a database was skipped by owner decision (2026-10-17).
- SASL/TLS and broker-restart-while-running integration tests: the test cluster is plaintext only
  and cannot be restarted. Possible with `github.com/twmb/franz-go/pkg/kfake` (in-memory Kafka with
  SASL/TLS) as a test-only dependency; declined by owner decision (2026-10-17). TLS/SASL option
  building is covered by unit tests.
- Push and pull request: not yet, by owner decision (2026-10-17).

## Risks / notes

- Binary size, measured on stripped Linux builds with the dashboard embedded: +5.0 MB amd64
  (63.5 -> 68.7 MB) and +4.6 MB arm64 (60.3 -> 65.1 MB), about +8%.
- Kafka client properties: every key is accepted; GSSAPI/Kerberos and JKS stores fail the device
  start with a clear error, as an unusable setting fails the client in Core.
- The `DEFAULT` format must stay bit-compatible with Core's `BrokerMessageCodec.kt`. The exceptions
  are the out-of-range expiry/PFI values noted under the codec layout.
