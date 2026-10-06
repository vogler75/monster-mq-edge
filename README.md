# MonsterMQ Edge

A native, single-binary MQTT broker that ships the same GraphQL API and storage
schemas as the JVM-based MonsterMQ broker, slimmed down for edge deployments
on devices like the Raspberry Pi 4/5.

## Highlights

- **Single static binary** (~25 MB), zero CGO. Pure-Go SQLite via `modernc.org/sqlite`.
- **MQTT 3.1 / 3.1.1 / 5.0** native server engine ([internal/mqtt/](internal/mqtt)) — TCP, WebSocket, TLS, WSS.
- **Storage**: SQLite (default), PostgreSQL, MongoDB. Schemas are byte-compatible with the Kotlin broker, so the same DB can be opened by either implementation.
- **Archive groups** (last-value + history fanout, retention purging) — same model as the Kotlin broker.
- **GraphQL API** with subscriptions, schema-parity with the existing dashboard.
- **REST API** for publishing, bodyless current/retained/history reads, bulk and Influx writes, and SSE subscriptions.
- **MQTT bridge** — forward local topics to a remote broker and vice versa.
- **PeerLink** — pull-based, in-memory forwarding of publishes between MonsterMQ Edge brokers (pairs and full meshes), with QoS, retain, MQTT 5 properties, publisher and publish time preserved. See [PeerLink](#peerlink-forwarding-between-brokers).
- **Camera snapshots** — RTSP MJPEG and H.264 I/P/B streams to MQTT JPEG topics; [native Go decoder API and current limits](pkg/h264/README.md).
- **Users + ACL** with bcrypt password hashing.
- **Periodic metrics** surfaced via `Broker.metrics`/`metricsHistory`; history can be persisted or kept in memory.
- **WinCC Unified bridge** — GraphQL/WebSocket or Open Pipe transport into local MQTT topics.
- **WinCC Open Architecture bridge** — GraphQL/WebSocket subscriptions from `dpQueryConnectSingle` into local MQTT topics.
- No OPC UA / Kafka / Sparkplug / GenAI / flows.

## Getting Started

**Clone:**
```bash
git clone https://github.com/vogler75/monster-mq-edge
cd monster-mq-edge
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
- GraphQL HTTP/WS: `http://localhost:4000/graphql`
- GraphQL playground: `http://localhost:4000/playground`
- REST API and offline docs: `http://localhost:4000/api/v1/docs`

The REST API is enabled by default on the GraphQL listener and can be disabled
with `RestApi.Enabled: false`. For a bodyless current-value read:

```bash
curl 'http://localhost:4000/api/v1/topics/sensor/temperature'
```

Omitting `group` (or sending `?group=`) selects the `Default` archive group.
Adding `start` or `end` selects its history; a named group still uses
`?group=Name`.

Use URL-encoded MQTT wildcards to read multiple topics as a JSON `messages`
array, for example `/api/v1/topics/cameras/%23` or
`/api/v1/topics/cameras/%2B/snapshot`. Add `?raw` to an exact topic GET to
receive the original payload bytes, for example:

```bash
curl -o snapshot.jpg 'http://localhost:4000/api/v1/topics/cameras/front/snapshot?raw'
```

`?retained&raw` reads retained bytes. Raw mode requires one exact current or
retained topic; combining it with a wildcard or history range returns HTTP 400.

`POST /api/v1/login` issues an opaque edge session token accepted by both REST
and GraphQL on this broker. It is local to the edge process and is not a JWT
from the main broker. See [the OpenAPI contract](internal/restapi/openapi.yaml)
for publishing, retained and history reads, bulk writes, and SSE.
REST publishes appear in history with the edge inline MQTT client ID.

## Cross-compile

```bash
make build-arm64    # Linux ARM64 (Pi 4/5, generic 64-bit)
make build-armv7    # Linux ARMv7 (Pi 3 / older 32-bit)
make build-amd64    # Linux x86_64
```

## Docker

To build the Docker image, you can run the build script under `docker/` which automatically tags the image and embeds the version from `version.txt`:

```bash
./docker/build.sh -n
```

Or you can build manually with `docker buildx` from the repository root:

```bash
docker buildx build --platform linux/amd64,linux/arm64 -t monstermq-edge:latest .
docker run --rm -p 1883:1883 -p 8080:8080 monstermq-edge:latest
```

The version is automatically read from `version.txt` at the root of the repository and embedded in the binary's version metadata. To override the version during manual builds, pass `--build-arg VERSION=1.2.3`.

## Storage backends

Pick the backend in `config.yaml`:

```yaml
DefaultStoreType: SQLITE   # or POSTGRES, or MONGODB
SQLite: { Path: ./data/monstermq.db }
Postgres: { Url: "postgres://localhost:5432/monstermq", User: monstermq, Pass: monstermq }
MongoDB:  { Url: "mongodb://localhost:27017", Database: monstermq }
```

Archive groups can also write their history to CrateDB (`archiveType: CRATEDB`,
default connection `CrateDB: { Url: "postgres://localhost:5432/doc", User: crate }`)
or QuestDB (`archiveType: QUESTDB`). Both are history-only; the last-value
store of such a group uses one of the backends above.

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
  RtspCamera: true
```

- `MqttClient` enables the MQTT bridge manager for forwarding topics between
  the local broker and remote MQTT brokers.
- `WinCCUa` enables WinCC Unified clients. Each device config can use
  `GRAPHQL` for GraphQL/WebSocket subscriptions or `OPENPIPE` for local Open
  Pipe IPC.
- `WinCCOa` enables WinCC Open Architecture clients. Each client subscribes to
  WinCC OA GraphQL `dpQueryConnectSingle` updates and republishes datapoint
  changes into MQTT.
- `RtspCamera` enables the RTSP / HTTP / WebSocket video stream bridge for
  capturing frames and publishing JPEG snapshots into MQTT topics.

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

### Camera snapshot bridge behavior

Camera configurations are stored in the device config store (`RTSP_CAMERA`), so
they remain persistent across broker restarts and can be managed via the web
dashboard or GraphQL queries and mutations.

The bridge connects to video streams without external tools (no CGO, no ffmpeg,
no shared libraries). Supported sources:

- **RTSP / RTSPS**: `rtsp://...` or `rtsps://...` with `TCP` (interleaved,
  recommended for firewalls/NAT) or `UDP` transport.
- **HTTP / HTTPS**: `http://...` or `https://...` multipart MJPEG streams
  (`multipart/x-mixed-replace`).
- **WebSocket / WSS**: `ws://...` or `wss://...` streaming raw MJPEG frames.

Supported codecs and decoding modes:

- **Motion JPEG (MJPEG)**: RTP, HTTP, or WebSocket MJPEG payloads are assembled
  directly into JPEG snapshots with minimal processing.
- **H.264 / AVC**: Progressive 8-bit YCbCr 4:2:0 I/P/B streams decoded natively
  via `pkg/h264`. Configurable via `h264DecodeMode`:
  - `FULL` (default): Decodes all reference frames continuously to maintain
    unbroken inter-frame prediction, encoding JPEG snapshots on interval or
    trigger.
  - `KEYFRAMES_ONLY`: Skips P/B frames and decodes only independent IDR keyframes
    at most once per `intervalMs`. This dramatically reduces CPU usage on edge
    devices like the Raspberry Pi 4/5 while trading off capture latency.

#### Snapshot modes

- `CONTINUOUS`: Automatically captures and publishes the latest frame every
  `intervalMs` (default `1000`, min `50`).
- `TRIGGERED`: Captures on-demand when an MQTT message arrives on `triggerTopic`
  (default `<topicPrefix>/trigger`) or when requested via GraphQL mutation
  `rtspCamera.triggerSnapshot(name)`.
- `BOTH`: Publishes regular periodic snapshots and also responds to triggers.

#### Round-robin slot publishing and topic architecture

For a configured `topicPrefix` (e.g. `cameras/front_gate`) and `slots` count (e.g. `5`):

```text
cameras/front_gate/capture/frames/1        # Raw JPEG image binary
cameras/front_gate/capture/frames/1/meta   # JSON snapshot metadata
...
cameras/front_gate/capture/frames/5        # Round-robin advances 1..slots
cameras/front_gate/capture/frames/5/meta

cameras/front_gate/capture/latest/pic      # Always the most recent JPEG image
cameras/front_gate/capture/latest/meta     # Metadata for the most recent image
cameras/front_gate/capture/latest          # JSON pointer to current slot & topics
cameras/front_gate/capture/snapshot/pic    # Trigger-specific snapshot JPEG
cameras/front_gate/capture/snapshot/meta   # Trigger-specific metadata
cameras/front_gate/status                  # Retained connection and health status
```

Metadata payloads include:
```json
{
  "camera": "front_gate",
  "slot": 1,
  "timestamp": "2026-09-17T13:45:00.123456789Z",
  "timestampMs": 1789652700123,
  "bytes": 65432,
  "contentType": "image/jpeg",
  "topic": "cameras/front_gate/capture/frames/1",
  "trigger": "continuous"
}
```

The active slot pointer on `<topicPrefix>/capture/latest` allows clients to
discover the current frame and metadata topics:
```json
{
  "camera": "front_gate",
  "slot": 1,
  "picTopic": "cameras/front_gate/capture/frames/1",
  "metaTopic": "cameras/front_gate/capture/frames/1/meta",
  "timestamp": "2026-09-17T13:45:00.123456789Z",
  "timestampMs": 1789652700123,
  "bytes": 65432,
  "trigger": "continuous"
}
```

#### Reading snapshots via REST API

Snapshots can be fetched as raw binary images using the edge broker's REST API:

```bash
# Get the most recent snapshot as a JPEG file:
curl -o latest.jpg 'http://localhost:4000/api/v1/topics/cameras/front_gate/capture/latest/pic?raw'

# Get slot 1 snapshot:
curl -o frame1.jpg 'http://localhost:4000/api/v1/topics/cameras/front_gate/capture/frames/1?raw'
```

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
the in-process inflight handling instead.

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

### Localhost unauthenticated access

Setting `AllowAnonymousLocalhost: true` allows connections and requests originating
strictly from IPv4 `127.0.0.1` (localhost) to connect without authentication, even when
`UserManagement.Enabled: true` and `AnonymousEnabled: false`:

- **MQTT**: Clients connecting from `127.0.0.1` without credentials are authenticated as
  user `localhost` with full topic publish and subscribe permissions.
- **GraphQL**: Requests arriving from `127.0.0.1` without an `Authorization` header are
  granted administrator access with username `localhost`.
- **HMI (`/hmi`)**: Web dashboards and static assets are served directly to requests from
  `127.0.0.1` without requiring a session token.
- **REST API (`/api/v1`)**: Requests from `127.0.0.1` without credentials bypass authentication.

Connections or requests arriving from any other IP address continue to require valid
credentials or tokens.

## PeerLink: forwarding between brokers

PeerLink links MonsterMQ Edge brokers so that a message published on one of
them is also delivered by the others, with the same MQTT semantics. It is
generic: any two or more brokers can be linked, with or without WinCC OA. For
WinCC OA redundant pairs see
[winccoa/README.md](winccoa/README.md#peerlink-for-redundant-pairs).
The full guide with every configuration key is
[doc/peerlink.md](doc/peerlink.md).

- **Pull-based, in memory.** Every broker keeps the publishes it accepted in
  an in-memory log (nothing is written to disk). Each peer pulls from that log
  over one TCP connection (port 1890 by default, optional TLS), applies the
  records locally and commits its offset. A record is freed once every
  configured peer has applied it; when the log is full, the oldest records
  are dropped and counted.
- **Message fidelity.** QoS, retain (an empty retained payload deletes), the
  MQTT 5 publish properties (payload format, content type, response topic,
  correlation data, user properties), the remaining message expiry, the
  publisher's client id and username, and the publish time. Broker-internal
  publishes (bridges, scripts, publish APIs) carry the client id `inline`.
- **One hop.** A broker never forwards what it received from a peer (split
  horizon), so PeerLink alone cannot form a loop. The two directions of a pair
  are separate links. With more than two brokers configure a full mesh, in
  which every broker lists every other one; a chain or ring delivers one hop
  only.
- **Restart-safe within bounds.** A peer that restarts resumes where it
  stopped, and missing retained messages come back through a snapshot
  (fill-if-absent, `Snapshot.Mode: FILL`), as long as the source kept running
  and the outage fits into the source's log.

### What is forwarded

Every publish a broker accepts: from network clients, wills, and from
broker-internal publishers (REST/GraphQL publish APIs, MQTT and WinCC bridges,
scripts, host monitoring, cameras). Not forwarded:

- topics starting with `$`;
- topics outside `Capture.Include` (default `["#"]`) or inside
  `Capture.Exclude` (default: the HMI sync channel `<HMI.SyncBaseTopic>/#`,
  i.e. `monstermq/hmi/sync/#`; `[]` forwards it too);
- the WinCC OA namespace (`<TopicRoot>/...`), but only while the broker runs
  embedded in WinCC OA with native mode active (`WinCCOaNative.Enabled` and
  `Namespace`). A standalone broker forwards `winccoa/...` like any topic;
- wills fired because the broker itself shuts down.

`Peers[].Receive.Include` / `Exclude` filter what this node accepts from a
peer. Replicas always reach local subscribers and the retained store. The other
subsystems are set under `PeerLink.Receive`: the pubsub bus (GraphQL
`topicUpdates`, scripts, REST SSE) and archive groups get replicas (`Bus`,
`Archive`: true); MQTT bridges do not forward them (`BridgeOutbound: false`,
loop guard); offline queues of persistent sessions skip them (`Queue: false`);
shared subscription groups get each message once, on the broker it was
published on (`SharedSubscriptions: SKIP`, or `DELIVER`). Network clients may
not use the client ids `inline` or `peerlink:*`.

### Configuration

A link is configured on both sides: a broker pulls from a peer that has an
`Address`, and lets a peer pull from it when that peer has `Serve: true` (the
default). Every broker needs its own `NodeId` (default: the hostname; with
PeerLink enabled the `edge` fallback is rejected). One file can serve every
host, because the `Peers` entry whose `NodeId` equals the own one is ignored.
Unknown keys under `PeerLink` fail startup. `config.yaml.example` lists every
key with its default.

Peers must authenticate each other; otherwise startup fails. A pair with TLS
and a shared secret:

```yaml
NodeId: edge-a                      # edge-b on the other host; the rest is identical
PeerLink:
  Enabled: true
  Tls:
    Enabled: true
    AutoGenerate: true              # self-signed certs/peer-{NodeId}.pem and .key on first start
  SharedSecrets: ["<base64, at least 16 bytes: openssl rand -base64 32>"]   # same on both hosts
  Peers:
    - { NodeId: edge-a, Address: "edge-a.local:1890" }
    - { NodeId: edge-b, Address: "edge-b.local:1890" }
```

The secret is bound to the TLS session (TLS 1.3), so the self-signed
certificates need not be verified. With more than two brokers, prefer
per-peer `Peers[].SharedSecrets`: a group secret lets any holder claim any
NodeId of the group.

mTLS, recommended for production and for meshes. Each broker has a certificate
with the URI SAN `urn:monstermq:node:<NodeId>` and the extended key usages
serverAuth and clientAuth, issued by a dedicated peer CA:

```yaml
NodeId: edge-a
PeerLink:
  Enabled: true
  Tls:
    Enabled: true
    CertPath: certs/peer-{NodeId}.pem
    KeyPath: certs/peer-{NodeId}.key
    TrustStorePath: certs/peer-ca.pem   # dedicated peer CA; system roots are never used
    ClientAuth: REQUIRED
  Peers:
    - { NodeId: edge-a, Address: "edge-a.local:1890" }
    - { NodeId: edge-b, Address: "edge-b.local:1890" }
    - { NodeId: edge-c, Address: "edge-c.local:1890" }
```

Without a CA, pin the self-signed certificates instead: `AutoGenerate: true`
and `ClientAuth: REQUIRED` on every broker, and per peer
`Tls: { PinnedSha256: ["<SPKI SHA-256>"] }` with the `spkiSha256` value the
peer logs at startup. `SharedSecrets` and `PinnedSha256` are lists, so they
can be rotated without losing the link: add the new value on both sides, move
it to the first position on both, then remove the old one.
Plain TCP without authentication is accepted only on a trusted network:
`AllowUnauthenticatedPeers: true` together with `Listener.AllowedNetworks`
and `UserManagement.Enabled: false`; it logs a WARN on every start.

Memory: `Log.MaxBytes` (default 256 MiB) bounds the log. A 200-byte record
(topic, client id and payload) takes about 224 bytes, so the default holds
about 1.2 million records, about 60 s at 20,000 msg/s; the status reports the
remaining `capacitySeconds`. While the log is full the process can use about
twice `MaxBytes`; `Runtime.MemoryLimitMB` sets a soft Go memory limit.

### Delivery guarantees (RPO)

PeerLink is not synchronous replication: a PUBACK means that the local broker
accepted the message, not that a peer has it. With the link up on a LAN, the
data at risk is the replication lag.

| Event | Result |
|---|---|
| Connection drop, both brokers running | No loss and no duplicates (resume by offset), while the backlog fits into the source's log |
| Peer graceful restart | No loss and no duplicates; missing retained values come back through the snapshot |
| Peer crash | The records applied after the last commit, at most one batch, are delivered again (at least once) |
| Source graceful stop | The MQTT listeners close first (MQTT 5 clients get reason 0x8B and can fail over), bridges and scripts stop, then the source waits up to `Log.DrainOnShutdownMs` (default 2000) for connected peers to catch up. What it could not serve is logged exactly (`shutdownUnserved`, `uncapturedAtShutdown`). |
| Source crash | Records not yet pulled are lost; the peer counts `sourceResets` and a lower bound in `resetLostLowerBound` |
| Peer down longer than the log holds | The oldest records are dropped and counted on both sides (`lostTotal`, `gapLostTotal`) |

QoS 2 is not exactly-once across a peer crash. Clients connected to several
brokers at once can end up with swapped retained values after a network
partition: on each broker the retained message applied last wins. No loss is
silent; every drop is counted in the status.

### Loop guards

PeerLink never forwards a replica again, but other components can turn a
replica into a new publish, which is then forwarded like any other:

- Never point an MQTT bridge, inbound or outbound, at a peer broker.
- Assign every device that publishes into the broker (MQTT bridges with
  inbound subscriptions, WinCC UA/OA bridges, RTSP cameras, scripts) to one
  broker's `NodeId`. With a config store the brokers share (`ConfigStoreType:
  WINCCOA`, or one PostgreSQL/MongoDB database), a device with `local` or `*`
  runs on every broker and its output arrives twice.
- Outbound-only MQTT bridges are the exception: run them on every broker (`*`)
  with `Receive.BridgeOutbound: false` (the default), so that each bridge
  forwards exactly its own broker's publishes. A broker with
  `BridgeOutbound: true` must not also run such bridges.
- Redfish gateways ignore `NodeId`: enable Redfish on one broker only, or add
  its prefix (`redfish/#`) to `Capture.Exclude`.
- Keep `{NodeId}` in `HostMonitoring.BaseTopic` (the default).
- An archive group that writes into a database shared by several brokers
  belongs to one broker, or set `Receive.Archive: false`.
- For external clients that republish what they receive, set
  `Receive.MarkReplicas: true` (adds the user property `mmq-peer-src=<NodeId>`
  to replicas) or `Capture.EchoSuppressMs`.
- Shared subscription groups need members on every broker.

At startup the broker logs a WARN for devices that run on every broker under a
shared config store, Redfish gateways, bridges whose remote host is a peer, and
a host-monitoring topic without `{NodeId}`.

### Status

The peer port serves a JSON status to loopback clients, and over TLS to
mTLS-authenticated peers:

```bash
curl -s http://127.0.0.1:1890/peerlink/v1/status
```

It shows this broker's log (`lso`, `leo`, `records`, `bytes`,
`capacitySeconds`, evictions, capture drops), every peer that pulls from it
(`consumers`: `state`, `committed`, `lag`, `lostTotal`, `shutdownUnserved`) and
every peer it pulls from (`sources`: `state`, `lagRecords`, `injected`,
`dropped` per reason, `gapLostTotal`, `sourceResets`, `retainedDiverged`,
`lastError`, `applyDelayMs`). `POST /peerlink/v1/resync?source=<NodeId>`
(loopback only) fetches the retained messages of that source again and
overwrites local values that are more than 1 s older. The peer port is bound
on `Listener.Address` when a peer has `Serve: true`; a broker that only pulls
binds `127.0.0.1:<Listener.Port>` for these two endpoints. With
`Listener.AllowedNetworks`, include `127.0.0.1/32`. Requests that carry an
`Origin` header or a non-loopback `Host` are refused (browser guard), so call
the endpoints with `curl` and `127.0.0.1` or `localhost`. Link events are logged (connects at INFO, gaps and resets at
WARN, identity and configuration errors at ERROR), and `Broker.metrics`
reports the records injected and served as `messageBusIn` and
`messageBusOut`.

### Limitations

- The `$SYS` counters `messages/received` and `packets/received` include the
  replicas a broker applied.
- A snapshot (`FILL`) after a peer restart can bring back a retained value that
  was deleted on that peer while the source was unreachable.
- In a mesh, a snapshot also carries retained values the source itself received
  from other peers (harmless with `FILL`, which only fills absent topics).
- A resync snapshot has no tombstones: values the source deleted are not
  removed on the peer. After an outage longer than the log, run the resync,
  then clear or republish the remaining topics by hand.
- Retained values from snapshots and archive rows of replicas are dated with
  the source's clock. Synchronise the brokers with NTP; a clock difference
  above 1 s is logged as a WARN (`clockSkewMs` in the status).
- `Fetch.MaxWaitMs` is at least 10 ms. `Receive.InjectWorkers` is reserved:
  each source is applied by one injector in order, and values above 1 only log
  a WARN.
- A network client whose CONNECT username is not valid UTF-8 is forwarded
  without its username (`log.usernameStripped`).
- With a `WINCCOA` retained store, concurrent retained publishes of one topic
  from different clients are not serialized with their capture, so the two
  brokers can keep different values until the topic is published again.

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
  broker/                → MQTT server bootstrap, hook wiring, TLS, lifecycle
  stores/                → MessageStore / MessageArchive / SessionStore /
                           QueueStore / UserStore / ArchiveConfigStore /
                           DeviceConfigStore / MetricsStore interfaces
  stores/sqlite/         → byte-compatible SQLite implementations
  stores/postgres/       → byte-compatible PostgreSQL implementations
  stores/mongodb/        → MongoDB implementations
  archive/               → archive group orchestrator + retention
  bridge/mqttclient/     → MQTT-to-MQTT bridge (paho client)
  bridge/rtspcamera/     → RTSP/HTTP/WS camera stream & snapshot bridge (MJPEG & H.264)
  bridge/winccua/        → WinCC Unified bridge (GraphQL/Open Pipe)
  bridge/winccoa/        → WinCC Open Architecture bridge (GraphQL)
  auth/                  → user+ACL cache
  metrics/               → in-memory counters + periodic snapshot writer
  pubsub/                → in-process bus for GraphQL topicUpdates
  peerlink/              → PeerLink broker-to-broker forwarding (log, protocol, injection)
  tlsutil/               → TLS helpers for PeerLink (trust, pins, NodeId identity)
  graphql/               → gqlgen-generated server, resolvers, dashboard handler
pkg/
  h264/                  → pure-Go H.264/AVC depacketizer & picture decoder
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

Production-ready for edge use on Pi 4/5; PostgreSQL/MongoDB backends compile
but require a live DB to integration test. Brokers can forward publishes to
each other with [PeerLink](#peerlink-forwarding-between-brokers) (in memory);
sessions, subscriptions and offline queues are not shared between brokers.

## License

GNU General Public License v3.0.

See [THIRD_PARTY_NOTICES.txt](THIRD_PARTY_NOTICES.txt) for third-party open source licenses and notices, including the MIT license for code incorporated in `internal/mqtt/`.
