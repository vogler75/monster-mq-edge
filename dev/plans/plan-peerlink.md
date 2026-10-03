# Plan: PeerLink, pull-based in-memory forwarding between MonsterMQ Edge brokers

**Status: draft for review (2026-10-03). Nothing implemented.**

Proposed file: `dev/plans/plan-peerlink.md`. Branch at drafting time: `winccoa-native`.

This plan merges three competing designs (throughput-first, failure-first, simplicity-first). The failure-first design is the backbone, and the strongest ideas of the other two are added to it. Revision 2 addresses the review findings: poison-record liveness, shutdown ordering, will semantics, retained bootstrap, external relay loops, string retention, shared subscriptions, offline queues, fail-closed security, format evolution, apply-side throughput and reproducible gates. Section 5 records each design decision and the alternative it rejected. Cost figures marked **(est.)** are estimates, not measurements. Section 21 explains how to measure them before anyone may claim them, following the AC-34 rule "an unmeasured low-latency claim does not pass" (`dev/plans/plan-winccoa-broker-embedded-manager.md:336`).

---

## 1. Goal, scope, non-goals

### 1.1 Goal

Take a WinCC OA redundant pair, or any small set of MonsterMQ Edge brokers. An MQTT client publishes to one broker on a topic **outside** the WinCC OA namespace (`<TopicRoot>`, default `winccoa`). The other brokers must then deliver that message with the same MQTT semantics.

The source broker keeps captured publishes in memory until every configured peer has pulled and applied them. A peer that restarts therefore loses nothing, as long as two conditions hold: the source keeps running, and the outage fits inside the source's memory bound. That holds for both live messages and retained state (section 16.5).

### 1.2 In scope

- **Capture hook** on the source. It sees every accepted client publish, every publish of the broker-internal client `inline` (publish APIs, MQTT/WinCC UA/WinCC OA bridges, scripts, host monitoring, Redfish, RTSP, native command replies; 7.4), and every will not caused by the broker's own shutdown.
- **Per-source in-memory log** with absolute offsets.
  - Each configured consumer has its own committed offset (its high-water mark).
  - A record is freed once every consumer has applied it.
  - The log is bounded by message count and by bytes; the oldest records are dropped first, and every drop is counted.
- **Pull protocol `mmq-peer/1`** over TCP, optionally with TLS. It provides long-poll fetch, batched records, commits, epochs, a retained snapshot on first contact, and forward-compatible framing.
- **Injection on the receiver** through a dedicated inline client per source, with full message fidelity: QoS, retain (including deletes), MQTT 5 publish properties, publisher client id and username, publish timestamp, and remaining expiry.
- **Topology:** bidirectional links, configured on both sides and identified by a canonical `NodeId`. Loop prevention is by split horizon.
- **Security:** authentication that fails closed (mTLS bound to the NodeId, or a shared secret over TLS), optional encryption-only TLS, and a CIDR allow-list.
- **Operations:** status (log events, metrics snapshot, a loopback status endpoint, an optional native status object), black-box integration tests with two or three real brokers, and measured performance.

### 1.3 Non-goals

These are listed so that reviewers do not measure this plan against M6 AC-27..AC-33:

- Replication or takeover of sessions, subscriptions, offline queues (`QueueStore`) or inflight state (AC-27).
- PUBACK/PUBREC/PUBCOMP gated on peer durability (AC-28); a crash matrix with RPO 0 (AC-29); forced full recovery on overflow (AC-30); fencing or role ownership (AC-31); forwarded writes to the active OA host (AC-32).
- Status isolation (AC-33) is met by construction, because `<TopicRoot>` is never forwarded.
- A durable log, or spilling to disk.
- Transitive multi-hop forwarding. More than two nodes need a full mesh (section 14).
- Retained resync that **overwrites** existing values automatically. v1 provides automatic fill-if-absent and an operator-triggered newer-wins resync (16.5).
- Forwarding `$` topics or the WinCC OA namespace. The HMI sync channel is excluded by the default `Capture.Exclude` (7.2), which can be changed.
- Configuration through GraphQL or the dashboard, and hot reload. Configuration is static YAML.
- GraphQL SDL changes, storage DDL changes, `embed/cabi` ABI changes, C++ manager changes, Kotlin broker interoperability.
- Any Kafka, Zenoh, Hazelcast or gRPC dependency, and any CGO.

---

## 2. Governance and required sign-offs (milestone M0, blocking)

The single-node rule ("No clustering — single node only") was removed from `AGENTS.md` by the owner on 2026-10-03, together with "single-node is a product decision" in its never-do-without-asking list (Hazelcast/Kafka-bus dependencies still need sign-off). PeerLink therefore needs no exception from the project rules (S1).

`dev/plans/spec-winccoa-native.md:15` still says "Dual-node exception: Not granted. M6 (AC-27..AC-33) is deferred", and `:19` says "Single node. No failover RPO/RTO applies". Those lines describe the WinCC OA native scope; S2 amends both of them and adds a new row. The remaining decisions are recorded before any code is written.

| # | Sign-off | Proposed text / outcome |
|---|---|---|
| S1 | Single-node rule | **Done (2026-10-03):** the rule was removed from `AGENTS.md`. PeerLink is pure Go, in memory and pull based, between explicitly configured, independent brokers; it shares no sessions, subscriptions, ownership or fencing. |
| S2 | Spec decision entry | Amend two rows of `spec-winccoa-native.md` §1: `:15` becomes "Dual-node exception: not required since 2026-10-03 (AGENTS.md rule removed); PeerLink is M6a; AC-27..AC-33 stay deferred", and `:19` (Availability/durability) becomes "Native scope single node; non-OA publishes may be forwarded by PeerLink (M6a, RPO per plan-peerlink 15.4)". Add a new row to §1. PeerLink is M6a. It supersedes only the "non-OA publishes" paragraph of `plan-winccoa-broker-embedded-manager.md` §7.2 (`:220`), and only for in-memory forwarding. AC-27..AC-33 stay deferred. AC-28 (PUBACK barrier, `:327`) and AC-30 (full recovery on overflow, `:329`) are explicitly replaced by sections 15 and 8.5. The same entry covers the optional `peerLink` object in the native status JSON (20.2). Add a cross-reference in §7.2. |
| S3 | RPO / durability statement | Section 15.4: PUBACK means "accepted locally"; records not yet pulled are lost if the source process crashes; a graceful source stop closes the MQTT listeners, stops the internal publishers, drains to a fixed `drainTarget` (15.6), and logs exactly what it could not serve (`shutdownUnserved`) or no longer captured (`uncapturedAtShutdown`); overflow drops the oldest records and counts them on both sides; across a consumer crash delivery is at-least-once, with at most one batch duplicated; otherwise exactly-once. |
| S4 | GraphQL | No SDL change. Optionally fill the existing `BrokerMetrics.messageBusIn/messageBusOut` (`internal/graphql/schema/schema.graphqls:32-33`, never set by `resolver.go:1832-1842`). That is a resolver-only mapping, which AGENTS.md:43-50 still asks to commit explicitly. |
| S5 | Engine changes | E1, E2, E4, E5 and E6 in the inlined mochi engine (section 13): preset `Origin`/`Created` for inline injections, `Packet.Forward`, `Server.RetainOnly`, `Server.CloseListeners`, and `Packet.Will`. E3 and E7 are optional. |
| S6 | Topology | One hop only. More than two nodes need a full mesh. |
| S7 | Default policies and partial requirements | The defaults in section 25, in particular: every publish is captured like a client publish, including those of bridges, scripts, host monitoring and the publish APIs (decided 2026-10-03); `$` topics are never captured, and the HMI sync tree is excluded by the default `Capture.Exclude`; wills are captured, but the receiver suppresses a will while the client is connected locally; shared subscriptions on the receiver skip replicas; offline queues skip replicas. The requirements marked "partial" in section 3 are accepted with these limitations. |
| S8 | Security posture | PeerLink fails closed. An unauthenticated peer is refused unless `AllowUnauthenticatedPeers: true` is set together with `AllowedNetworks`, and never when `UserManagement.Enabled` is true (17.3). |

Naming: the feature is **PeerLink** (package `internal/peerlink`, config section `PeerLink`). "Replicated" and "replication" already refer to the WinCC OA topics branch (`internal/broker/hook_storage.go:36-39,282`; `winccoa/README.md:273`), so this plan does not reuse them.

---

## 3. Requirements traceability

| # | User requirement (2026-10-03) | Status | Satisfied by | Limitations accepted by |
|---|---|---|---|---|
| R1 | Forward topics that are not in the winccoa namespace to another MonsterMQ broker | Yes | 7.2 (capture filter), 7.4 (sources), 12.2 (receiver filter), 18 | All publishes are forwarded, including bridges, scripts and the publish APIs (7.4). Not forwarded: any `$` topic, wills fired by the source's own shutdown, and by default `<HMI.SyncBaseTopic>/#` (`Capture.Exclude`, changeable). Devices that run on both nodes produce their output twice (19); Redfish gateways ignore NodeId and run on every node (7.4). S7. |
| R2 | "Super high performant" | Yes, gated | 8 (chunked log), 9.7 (batched long-poll fetch, writev), 11.1 (split reader/injector), 13.4 (batched retained writes), 21 (budget, gates G0-G7) | Throughput ceiling is the receiver's apply path; measured per GOARCH (21.2). armv7 has a documented lower rate. |
| R3 | Pull-driven: the source collects value changes until another node pulls them | Yes | 8, 9 | — |
| R4 | A restart of another node does not lose values | Yes, within bounds | 8.3 (consumers pin from epoch start), 9.6 (resume rules), 15.5, 16.5 (retained snapshot on first contact), 12.6 (catch-up pacing) | Lossless only while the source keeps running and the outage fits the log (`capacitySeconds`, 8.7). S3. |
| R5 | Kafka-like, but in memory only (no disk) | Yes | 8 (offsets, epochs, committed offsets), 15.4 | — |
| R6 | Multiple brokers can connect; the source queue serves multiple clients | **Partial** | 8.3, 14 | Only a full mesh gives every node every message; a chain delivers one hop only. S6. |
| R7 | A high-water mark per connected broker; free the queue when the last consumer has read | Yes | 8.4 | — |
| R8 | Configure the connection on both parties | Yes | 18.2 | — |
| R9 | Max queue length; when reached, remove older values | Yes | 8.5 (count and byte bounds, counted eviction) | In practice the byte bound governs for records above about 130 B (8.7). |
| R10 | Replicate the full MQTT message: QoS, retain, MQTT 5 values, publisher, publish timestamp | **Partial** | 7.3, 10, 12.3, 13 | Pre-existing engine limits: will properties other than User are lost (`server.go:1685-1687`); DB-retained rows and offline queues store no MQTT 5 properties; user properties are suppressed for subscribers with RequestProblemInfo=0. On the receiver, the retained row time is the backdated capture time in whole seconds (bus and archive rows carry the source ns time). Broker-internal publishes (bridges, scripts, publish APIs) carry client id `inline` and no username. S7. |
| R11 | Bidirectional: both brokers can connect to each other | Yes | 14.3, 18 | — |
| R12 | The receiver must not send received values back to the source | Yes for PeerLink injections; partial for internal publishers | 14.1, 14.2, 14.6 (bridge outbound skips replicas) | Scripts/bridges that republish replicas and MQTT bridges subscribed to a peer are captured like any publish (7.4) and can loop, as can external republishing clients. Guards (14.6): one-node device assignment, bridge-outbound skip, peer-host WARN, `Capture.Exclude`, `Capture.EchoSuppressMs` (network and internal publishes), `Receive.MarkReplicas`. |
| R13 | Identify connections by broker NodeId | Yes | 9.5, 17.2, 18.4 (canonical NodeId) | — |
| R14 | Optional TLS encryption | Yes | 17.1 | — |
| R15 | Optional certificate authentication | Yes | 17.2 (URI SAN identity, pins) | At least one authentication method is mandatory unless explicitly waived (S8). |

---

## 4. Terms

| Term | Meaning |
|---|---|
| Source | The broker whose local publishes are captured into its log and served. |
| Consumer | The broker that pulls from a source and injects the records locally. In a pair, every node is both. |
| Link | One direction, source to consumer. "Bidirectional" means two links on two TCP connections. |
| Epoch | A random non-zero 64-bit id from `crypto/rand`, drawn when the source process creates its log. It is new on every start. |
| Offset | An absolute, gap-free `uint64`. It starts at 1 in every epoch; 0 means "none". `(sourceNodeId, epoch, offset)` identifies a record globally. |
| LSO / LEO | Log start offset (the oldest offset still held) and log end offset (the offset the next append gets). The log is empty when `LSO == LEO`. |
| Committed offset `C[c]` | For consumer `c`, the next offset it needs. Every record below it has been **applied** on `c`, that is, injected or dropped by policy. This is the user's "high water mark per connected broker". The initial value is 1 in each epoch. |
| Served mark `S[c]` | One past the highest offset ever sent to `c` in this epoch. Used only for loss accounting (8.5). |
| Low-water mark `LWM` | `min(C[c])` over all configured consumers. Records with offset < LWM are freed. |
| Canonical NodeId | The NodeId lower-cased and checked against `^[a-z0-9._-]{1,64}$`. Every comparison uses this form (18.4). |
| Client publish | A publish from a network MQTT client. |
| Internal publish | A publish through the shared inline client `inline` (`internal/mqtt/server.go:34,202-205,808-832`): GraphQL/REST/MCP publish APIs, bridges, scripts, host monitoring, HMI sync, Redfish, RTSP and native command replies. Captured like a client publish (7.4). |
| Injector client | The dedicated inline client on the consumer for one source peer (listener id `peerlink`). |
| Replica | A packet injected by an injector client (`pk.Forward != nil`). |

---

## 5. Design basis: decisions and rejected alternatives

| Topic | Decision | From | Rejected alternative and reason |
|---|---|---|---|
| Transport | Custom binary framing over one TCP/TLS connection per link (`mmq-peer/1`) | throughput, failure | HTTP/1.1 long-poll (simplicity) cannot pipeline, costs a request per batch, and has no commit channel separate from the fetch. MQTT has no offsets or epochs, and the Go bridge speaks MQTT 3.1.1 only (`go.mod:10`). |
| Status endpoint | `GET /peerlink/v1/status` on the peer port: plaintext from loopback, or over TLS for mTLS-authenticated peers only | simplicity | A separate port; GraphQL fields (SDL rule); HMAC-signed remote status (it would need a replay-safe scheme that v1 does not need). |
| Handshake | Minimal pre-auth `SERVER_HELLO` (nonce and capabilities only). All identity and log details move to `HELLO_OK`, which is sent after authentication. | failure, review | Disclosing NodeId, TopicRoot and epoch to any scanner. |
| Log structure | Chunked index of immutable pre-encoded frames. Fixed 1024-slot chunks are never moved and are preallocated outside the lock. Trim clears slots outside the lock. Readers snapshot in O(1). | failure + review | A power-of-two ring that grows ×2: it re-slots every entry under the publisher mutex during a burst. The segmented lock-free arena (throughput) stays an M6 option behind gates G1/G1b/G6. |
| Serve path | `net.Buffers` (writev) on the raw `*net.TCPConn`; a 64 KiB `bufio.Writer` on TLS | throughput, failure | Wrapping the conn for writes: `net.Buffers.WriteTo` falls back to one `Write` per record (`/opt/go/src/net/net.go:853-866`). |
| Record header | 44-byte little-endian header with `recVersion` and `hdrLen`, plus a skippable TLV (`id u8, len u32`) for properties | throughput + review | Reusing `packets.Properties.Encode`: it allocates and depends on `Mods` quirks (`properties.go:216-227,326-338`). A fixed header without a length: it cannot evolve. |
| Format evolution | Unknown TLVs, trailing header bytes and trailing frame-body bytes are skipped; capability bitmaps in the handshake | review | Treating every unknown element as a protocol error: that breaks rolling upgrades of a redundant pair. |
| Poison handling | Only transport-level faults close a session. Every per-record failure is a counted policy drop that advances the offset. Oversize records are replaced by a counted tombstone. | review | "Structural failure = protocol error": one bad record would stall the link until it is evicted. |
| Time and expiry | Wall-clock publish time (ns) as metadata. Expiry and age are computed from monotonic capture time plus elapsed time on the consumer, at injection time. | failure + review | Absolute expiry on the source clock (second resolution, skew), or age measured at batch build time (ignores apply delay). |
| Commit | `COMMIT` after every applied batch, plus every 100 ms during a long apply. `FETCH` also carries it. | review | Commit only piggybacked on FETCH: it ties the duplicate window to the pipeline depth. |
| Consumer threading | Reader/decoder goroutine → handoff (1-2 batches) → inject goroutine; one writer goroutine for FETCH, COMMIT and PING | review | A single loop: TLS decryption sits serially on the apply critical path, and nothing can send PING during a long apply. |
| Duplicate suppression | O(1) per `(source, epoch)` `appliedNext`; HELLO carries the resume offset | all three | A UUID LRU as in Kotlin Zenoh (`MessageBusZenoh.kt:241-251`, O(n) per message). |
| Loop prevention | Split horizon (injector clients and `pk.Forward` never captured). Replicas are marked on the bus, and the MQTT bridge outbound skips them by default. | failure + review | Transitive forwarding with an origin path (deferred; can be added as a TLV). |
| Retained capture point | Retained publishes are captured in `OnRetainMessage` (before the PUBACK write); everything else in `OnPublished` | review | Capturing everything in `OnPublished`: a retained race across a blocking ack write can diverge the nodes permanently. |
| Retained after a consumer restart | Automatic `SNAPSHOT` (fill-if-absent) on first contact and after a source reset; operator-triggered newer-wins resync | review | Leaving MEMORY-retained receivers empty after a restart (it violates R4). |
| Retained record expired before pull | Drop and count (`retainedDiverged`). M6: conditional silent clear. | failure | "Inject with 1 s" violates MQTT 5; an unconditional clear can wipe a newer local value. |
| Wills | Captured, except during the source's own shutdown. Dropped on the receiver while the client is or became connected there. Kept off bus, archive and queue there, matching local behaviour. | review | Applying wills as ordinary publishes: a stale "offline" would overwrite a failed-over client's live state. |
| Inline capture | Every internal publish is captured like a client publish | owner decision 2026-10-03 | Capturing only some internal publishers (the earlier API/service split): a device's output would never reach the peer unless the device also ran there. Duplicates are avoided by assigning each device to one node (19). |
| Receiver gating | Defaults: bus on (bridge outbound off), archive on, offline queue off, shared subscriptions skip replicas | failure + review | Delivering replicas to every subsystem: it causes duplicate floods for persistent sessions and duplicate shared-group processing. |
| Retained writes on the receiver (DB modes) | Coalesced per batch into one `AddAll`/`DelAll` before the commit | review | One synchronous transaction per retained replica on the inject goroutine. |
| Shutdown | Close MQTT listeners first (clients fail over), stop the internal publishers (bridges, scripts, host monitoring, Redfish, RTSP, HMI sync, publish APIs), drain to the fixed `drainTarget`, then stop the other subsystems; shutdown wills are not captured (decided 2026-10-03) | review | Draining before `native.Stop` while clients keep publishing for seconds into a closed link; draining while internal publishers keep publishing (a moving target, and their later publishes would be lost uncounted). |
| Security default | Fail closed (S8); a shared secret requires TLS and is bound to the TLS exporter; identity is the URI SAN by default | review | A WARN only; plaintext HMAC (relayable); CN fallback by default. |
| TLS helpers | New leaf package `internal/tlsutil`; `broker.LoadTLS` untouched in v1 | simplicity | Refactoring `LoadTLS` now touches the TCPS path for no v1 benefit. |
| Integrity | Per-batch CRC32C on plaintext links; off by default under TLS, whose AEAD already covers integrity | failure + review | Mandatory CRC on TLS, which costs software CRC on armv7 for no gain. |
| NodeId collisions | Canonical NodeIds; duplicate detection only on sustained flapping; then refusal | failure + review | A 30 s heuristic that fires on ordinary crash-restarts. |
| Receiver scaling | `InjectWorkers` (topic-hash sharding) as a v1 contingency if gate G3 fails | throughput + review | Deferring it to M6 regardless of measurements. |
| Config shape | Symmetric `Peers` list; one file can serve both hosts | failure, simplicity | — |

---

## 6. Architecture

```
          Node A (NodeId oa-a)                                      Node B (NodeId oa-b)
 MQTT client --PUBLISH--> engine processPublish --> local subscribers
                          | OnRetainMessage (retained) / OnPublished (other) / OnWillSent
                          v
                 peerlink.Hook --filter--> Log(A): chunked index, LSO..LEO, C[oa-b], S[oa-b]
                                                    ^         |
                                                    | FETCH   | BATCH / SNAPSHOT (writev or TLS)
 peer port :1890 <-- peerlink.Server session(oa-b) -+---------+----------> Puller(oa-a) on B
                                                                             reader: read, CRC, decode
                                                                             injector: filter, expiry,
                                                                             will check, pacing
                                                                                v
                                                              injector client "peerlink:oa-a"
                                                              (inline, ProtocolVersion 5, not in s.Clients)
                                                              InjectPacket -> retain (batched DB write),
                                                              subscribers, bus (flagged), archives
                                                              peerlink.Hook on B skips it (split horizon)

 Mirror image: B has Log(B) and a listener; A has Puller(oa-b). Each direction is its own TCP connection.
```

### 6.1 Packages and files

| File | Responsibility |
|---|---|
| `internal/peerlink/manager.go` | `Manager`: `New`, `Hook`, `Start`, `StopPullers`, `BeginDrain`, `Drain(ctx)`, `Close`, injector clients, `Status()` |
| `internal/peerlink/hook.go` | `Hook`. It provides `OnConnect`, `OnPublished`, `OnRetainMessage`, `OnWillSent`, `OnSessionEstablished` and `OnSelectSubscribers`, and never `OnPublish`. `OnConnect` refuses the reserved network client ids (12.1). |
| `internal/peerlink/filter.go` | Built-in exclusions; precompiled Include/Exclude matcher (allocation-free level walker); capture-time topic validation |
| `internal/peerlink/log.go` | Chunked log, epoch, consumer cursors, trim, eviction, waiters. This is the only file using `unsafe` (frame pointers). |
| `internal/peerlink/wire/record.go` | Record size/encode/decode/validate, TLV, tombstones (exported for the test peer) |
| `internal/peerlink/wire/frame.go` | Preamble and frame codec, capability bits |
| `internal/peerlink/server.go` | Peer listener, admission, sniffing, handshake, per-consumer session, serve loop, snapshot streaming |
| `internal/peerlink/puller.go` | Dial, handshake, reader/injector/writer goroutines, dedup, backoff, pacing |
| `internal/peerlink/inject.go` | Receiver validation, packet build, will suppression, `InjectPacket` / `RetainOnly` |
| `internal/peerlink/status.go` | Counters (striped), status JSON, loopback HTTP status and resync handlers |
| `internal/tlsutil/tlsutil.go` (new leaf) | `LoadKeyPair`, `LoadCertPool` (PEM, legacy-PKCS12 truststore, never system roots), `ServerConfig`, `ClientConfig`, `VerifyConnection` NodeId binding, pins, `EnsurePeerCertificate`. No `cert:key` split. |
| `internal/broker/server.go` | Manager field; wiring in `build()` (including the device WARN step, 6.2), `Serve()`, `Close()` (including the internal-publisher stop before the drain, 6.2); the `RetainedAccess` adapter for snapshots |
| `internal/broker/hook_storage.go` | `Forward`-aware publisher and time, derived message UUID, receive gating, will skip, `IncBusIn`, batched retained writer for replicas |
| `internal/broker/hook_queue.go` | Skip replicas unless `Receive.Queue`; skip replicated wills; hydrate only own-node sessions when PeerLink is enabled |
| `internal/bridge/mqttclient/bus_adapter.go` | Skip peer-origin bus messages unless `Receive.BridgeOutbound` |
| `internal/stores/types.go` | `BrokerMessage.OriginNode string` with `json:"-" bson:"-"`. It is in memory only and never persisted; M1 verifies that no store marshals the struct generically. |
| `internal/mqtt/packets/packets.go`, `internal/mqtt/server.go` | E1, E2, E4, E5, E6 (section 13) |
| `internal/metrics` | `IncBusIn`/`IncBusOut`; snapshot fields `messageBusIn`/`messageBusOut` |
| `internal/config/config.go`, `load.go`, `yaml-json-schema.json`, `config.yaml.example`, `winccoa/monstermq.yaml.example` | Configuration (section 18) |

`peerlink` must not import `internal/broker`, because `broker` constructs it and that would be an import cycle. This is why the TLS helpers go into `internal/tlsutil`. Callbacks that need broker state go through small interfaces passed into `peerlink.New`:

- `RetainedAccess{ Snapshot(func(packets.Packet) bool); Has(topic string) bool; FlushReplicas(source string) error }`
- `ClientState{ Connected(id string) bool }`

### 6.2 Lifecycle wiring (`internal/broker/server.go`)

**`build()`**, right after the queue hook (`server.go:325-332`):

1. If `cfg.PeerLink.Enabled`, construct the manager:
   ```go
   peerlink.New(cfg.PeerLink, cfg.NodeID, names, server, retainedAccess, clientState, collector, logger)
   ```
2. If `cfg.PeerLink.Enabled`, call `server.AddHook(pl.Hook(), nil)`. Do this whenever any `Pull` or `Serve` peer exists, so the split-horizon counters also work on pull-only nodes. **Register the hook before StorageHook and QueueHook.** The hook only reads `pk.Ignore` (set by the `OnPublish` chain) and identity fields, so the order does not affect correctness. Running first keeps the capture append ahead of the potentially blocking QueueHook in `OnPublished`.
3. Create one injector client per pull peer:
   ```go
   server.NewClient(nil, "peerlink", "peerlink:"+canonicalPeerNodeId, true)
   ```
   with `Properties.ProtocolVersion = 5` (`internal/mqtt/server.go:244-263`). It is **not** added to `s.Clients`.
4. Bind the peer `net.Listener` next to the MQTT listeners (`server.go:345-394`) and append its close to `*undo` (`server.go:100-113`), so a port conflict fails startup cleanly. Use a plain `net.Listener`, **not** `server.AddListener`: mochi listeners hand connections to MQTT `EstablishConnection`.
5. **Device WARNs** (PeerLink enabled). `Config.Validate` cannot see device configs, which live in the DeviceConfigStore, so these checks run here, where the stores are open (`server.go:128-199`, including `useOAStores` at `:181-186`). A helper in `server.go` reads `storage.DeviceConfig.GetAll` once (MQTT bridge configs decoded as `mqttclient.Config`, `internal/bridge/mqttclient/connector.go:21-48`) and logs a WARN, not an error, when:
   - an enabled device that publishes into the broker (MQTT bridge with inbound subscriptions, i.e. an address with `mode: SUBSCRIBE`; WinCC UA/OA bridge; RTSP camera; script) has NodeId `local` or `*` while `cfg.ConfigStore()` (`internal/config/config.go:467-472`) is WINCCOA, POSTGRES or MONGODB (a config store both nodes can share); it runs on every node and its output arrives twice (7.4);
   - an enabled Redfish gateway exists: gateways ignore NodeId and run on every node (7.4, 14.6);
   - `HostMonitoring.Enabled` and `HostMonitoring.BaseTopic` lacks `{NodeId}` (7.4);
   - an enabled MQTT bridge, inbound or outbound, has a `brokerUrl` host equal to a configured peer's `Address` host (14.6).

**`Serve()`**, after `startNative`: `s.peer.Start()` starts the accept loop and the puller goroutines without blocking, because the embedded host calls `Serve` synchronously (`embed/cabi/cabi.go:223-229`).

**`Close()`**: when PeerLink is enabled, the new order is:

1. `s.peer.StopPullers(ctx)`. Each puller finishes its current batch, flushes the batched retained writer, sends `COMMIT` and then `GOAWAY(shutdown)`.
2. `s.peer.BeginDrain()`. From here on, wills are not captured: they would only be the broker's own shutdown wills (7.5).
3. `s.mqtt.CloseListeners()` (E5). This closes every MQTT listener and disconnects its clients with reason 0x8B "server shutting down" (`Listeners.CloseAll(s.closeListenerClients)`, `server.go:1654,1663-1668`). It does **not** call `OnStopped` or `hooks.Stop`. No network client can publish after it returns, and v5 clients get an explicit reason to fail over to the peer.
4. **Stop the internal publishers** (decided 2026-10-03): `bridges`, `winCCUa`, `winCCOa`, `rtspCameras`, `scripts`, `hostMonitor`, `hmiSync`, `redfishMgr`, and the GraphQL/HTTP server `gqlSrv`, which also serves the REST and MCP publish APIs (`server.go:502`). Today they stop later in `Close()`, after `native.Stop` (`server.go:764-789` and `:799-803`). After this step the only remaining `inline` publisher is the native service: no client is left to send it commands, and a reply still in flight after the drain is counted, not lost silently (15.6). It stops in step 7.
5. `s.peer.Drain(ctx)`. It fixes `drainTarget = leo` and waits at most `Log.DrainOnShutdownMs`, then switches capture off. Details are in 15.6.
6. `GOAWAY(shutdown)` to all sessions; close the peer server. The source logs `shutdownUnserved` per consumer and `uncapturedAtShutdown` (15.6).
7. The existing order runs unchanged, without the parts already stopped in step 4: `native.Stop` … `metricsStop`, `collector.Stop` … `archives.Stop` … `mqtt.Close` … `storage.Close` (`server.go:758-820`). M1 verifies that `mqtt.Close` → `Listeners.CloseAll` is a no-op on listeners that are already closed (`listeners.go:115-130`), and that the step-4 `Stop` calls are not repeated.

When PeerLink is disabled, `Close()` is unchanged. The standalone binary and the embedded `WCCOAmmq` share this path. There is no ABI or C++ change.

---

## 7. Capture (source side)

### 7.1 Tap points (verified)

- **`OnRetainMessage(cl, pk, r)`** fires inside `retainMessage` (`server.go:1082-1112`). It runs after the `OnPublish` chain has accepted the publish and **before** the PUBACK/PUBREC write and before subscriber fan-out (`server.go:1037-1039` vs `:1061`, `:1074-1075`). It does not fire for `pk.Ignore` packets or when `RetainAvailable == 0` (`server.go:1083`). It fires for both memory and DB modes (`:1098-1109`). It also fires for retained wills, through `sendLWT` → `retainMessage`.
- **`OnPublished(cl, pk)`** (`hooks.go:453`) fires for every accepted publish after local delivery. For QoS 0 and inline publishes see `server.go:1041-1048`; for QoS 1/2 from network clients it fires after the ack write (`:1050-1077`). It also fires for `pk.Ignore` packets.
- **`OnWillSent(cl, pk)`** (`hooks.go:555`). Wills bypass `OnPublished` (`server.go:1671-1707`).
- All three run synchronously on the publisher's goroutine (`internal/mqtt/clients.go:373-397`), which gives per-publisher FIFO. Retained and non-retained publishes of one client are captured in the order they were processed, because each hook call finishes before the next packet is read.
- **Rule:** retained publishes are captured in `OnRetainMessage` when `Capabilities.RetainAvailable == 1` (the default, `server.go:82`); everything else in `OnPublished`. This removes the window between a retained-store update and capture, and closes the old gap where the ack write fails after the retained store was updated.
- `processPublish` overwrites `pk.Origin` and `pk.Created` (`server.go:980-981`), so the hook takes its own timestamp.

### 7.2 Filter chain (cheapest first, allocation-free)

```go
func (h *Hook) Provides(b byte) bool {
    switch b {
    case mqtt.OnConnect, mqtt.OnPublished, mqtt.OnRetainMessage, mqtt.OnWillSent,
         mqtt.OnSessionEstablished, mqtt.OnSelectSubscribers:
        return true
    }
    return false
}

func (h *Hook) OnConnect(cl *mqtt.Client, pk packets.Packet) error {             // reserved client ids (12.1)
    if cl.ID == mqtt.InlineClientId || strings.HasPrefix(cl.ID, "peerlink:") {
        _ = h.srv.SendConnack(cl, packets.ErrClientIdentifierNotValid, false, nil)
        return packets.ErrClientIdentifierNotValid
    }
    return nil
}

func isReplica(cl *mqtt.Client, pk *packets.Packet) bool {
    return cl.Net.Listener == peerlink.ListenerID || pk.Forward != nil
}

func (h *Hook) OnRetainMessage(cl *mqtt.Client, pk packets.Packet, r int64) {
    if pk.Will || isReplica(cl, &pk) || !h.retainViaHook { return } // wills: OnWillSent; replicas: counted in OnPublished
    h.capture(cl, &pk, false)
}

func (h *Hook) OnPublished(cl *mqtt.Client, pk packets.Packet) {
    if isReplica(cl, &pk) { h.skipPeer.Add(1); return }             // split horizon (R12); exact count, 14.2
    if pk.FixedHeader.Retain && h.retainViaHook && !pk.Ignore { return } // already captured in OnRetainMessage
    h.capture(cl, &pk, false)
}

func (h *Hook) OnWillSent(cl *mqtt.Client, pk packets.Packet) {
    if !h.captureWills || h.draining.Load() { h.skipWill.Add(1); return } // no shutdown wills (7.5)
    h.capture(cl, &pk, true)
}

func (h *Hook) capture(cl *mqtt.Client, pk *packets.Packet, will bool) {
    if !h.active.Load() {                                           // no Serve peers, or switched off by Drain (15.6)
        if h.log.Sealed() && !pk.Ignore && h.filter.Accept(pk.TopicName) { h.cnt.inc(uncapturedAtShutdown) }
        return
    }
    if pk.Ignore { return }                                         // native commands, topics branch
    if !h.filter.Accept(pk.TopicName) { h.cnt.inc(filtered); return }
    if cl.Net.Inline && !wire.ValidTopic(pk.TopicName) { h.cnt.inc(invalid); return } // 7.3
    if h.echo != nil && !will && h.echo.Match(pk) { h.cnt.inc(echoSuppressed); return } // 14.6, network and inline
    h.log.Append(cl, pk, will)                                      // encode + append, 8.2
}
```

- `Provides` is a direct comparison, because hook dispatch calls `Provides` on every hook for every event (`hooks.go:146-156`). `OnSelectSubscribers` is only invoked when shared subscriptions match (`server.go:1132-1138`).
- The replica test runs before the `active` and `Ignore` tests, so `skipPeer` counts every replica that reaches `OnPublished` (14.2).
- `OnConnect` runs for every hook before `OnConnectAuthenticate` (`internal/mqtt/server.go:448-451` vs `:454`), so the refusal holds whatever the auth configuration (12.1).
- Counters on publisher goroutines (`filtered`, `invalid`, `echoSuppressed`) are striped: 16 cache-line-padded stripes. Each increment picks its stripe with `rand.Uint32()` from `math/rand/v2` (the runtime's per-thread generator, no shared state). The client pointer is not used as the index: every internal publisher shares the single `inline` client and would land on one stripe. This avoids a shared contended cache line.

**Always excluded (built in, not configurable):**

1. Replicas (split horizon).
2. `pk.Ignore`: WinCC OA native commands and topics-branch publishes (`internal/broker/hook_winccoa.go:112-160`). QueueHook uses the same rule (`hook_queue.go:106`).
3. The own namespace: `t == root || strings.HasPrefix(t, rootSlash)`, with `rootSlash` precomputed. For topic names this equals `Names.Classify(t) != KindOther` (`internal/winccoanative/namespace.go:132-171`). `TopicRoot` is defaulted even when native mode is off (`internal/config/config.go:262-268`).
4. Topics starting with `$`.

**Configurable:** `Capture.Include` (default `["#"]`) and `Capture.Exclude` (default `[<HMI.SyncBaseTopic>/#]`, i.e. `monstermq/hmi/sync/#`), where Exclude wins. The HMI sync channel is a request/response protocol between the `mmq hmi sync` tool and one broker; forwarded, the sync service of every node would answer each command and write the files (`internal/hmi/sync.go:110-130`). `Exclude: []` forwards it anyway. Filters are compiled once into a level trie and matched with an index-walking iterator, with no `strings.Split` (unlike `internal/topic/tree.go:332`). `Include: ["#"]` needs no test. Exclude entries of the form `<prefix>/#` (the default) compile to precomputed prefix tests like the namespace check (`t == prefix || strings.HasPrefix(t, prefixSlash)`); the trie runs only for other filters. `Capture.EchoSuppressMs` is in 14.6.

### 7.3 What is captured

| Field | Source | Note |
|---|---|---|
| QoS, Retain, Dup | `pk.FixedHeader` | QoS is already capped to `MaximumQos` (`server.go:1003-1005`). Dup is metadata only. |
| Topic | `pk.TopicName` | The inbound alias is already resolved (`server.go:999-1001`). Inline topics are validated at capture with the receiver's own rules: valid UTF-8, no NUL, no `+`/`#`, at most 65535 bytes. Network topics were already validated by decode (`internal/mqtt/packets/codec.go:46-56`). Failures are counted as `captureDropped{invalid}` and never logged. |
| Payload | `pk.Payload` | Copied into the frame. |
| PayloadFormat (+flag), ContentType, ResponseTopic, CorrelationData, User properties | `pk.Properties` | User properties keep their order and duplicates. Inline-origin strings are validated like topics. |
| Message expiry | `min(pk.Properties.MessageExpiryInterval, MaximumMessageExpiryInterval)` s, 0 = none | `pk.Expiry` is not used: it has second resolution, and for delayed wills it holds the send time. For wills it is always 0, because `ParseConnect` drops will expiry. |
| Not captured | `TopicAlias`/`TopicAliasFlag`, `SubscriptionIdentifier` | Both are per hop. |
| Publisher | `cl.ID`, `cl.Properties.Username` | For a will, `cl` is the disconnected client. Strings above 65535 bytes are counted as `invalid`. |
| Publish timestamp | `t := time.Now()`; `publishWallNs = t.UnixNano()`; `captureMonoMs = t.Sub(log.startMono).Milliseconds()` | One `time.Now()` call yields both readings. |
| Kind flags | `will`, `inline` (published by a broker-internal client) | |

### 7.4 Inline sources

Every publish of a broker-internal client is captured exactly like a client publish (owner decision 2026-10-03): the GraphQL/REST/MCP publish APIs, the MQTT, WinCC UA and WinCC OA bridges, scripts, host monitoring, Redfish, RTSP and native command replies. They all use the shared inline client `inline` (`internal/broker/server.go:397-483`); the record carries client id `inline`, no username and the `inline` kind flag. The same filters apply as for client publishes (7.2): the own namespace and `$` topics are never captured, Include/Exclude apply.

- **Devices on both nodes.** Bridges, RTSP cameras and scripts run where their config's `NodeId` matches: the own NodeId, `local` or `*` (`GetEnabledByNode`, `internal/bridge/mqttclient/manager.go:93`, `internal/bridge/winccua/manager.go:42`, `internal/bridge/winccoa/manager.go:64`, `internal/bridge/rtspcamera/manager.go:45`, `internal/scripting/manager.go:78`). With a config store shared by both hosts (`ConfigStoreType: WINCCOA` mirrors MMQConfigs; a shared POSTGRES or MONGODB database does the same), a device with `local` or `*` runs on both nodes. Each node then publishes the device's output itself and also receives the peer's copy, so subscribers get every message twice. Assign each device that publishes into the broker to one node's NodeId; that node forwards its output to the peers. **Exception:** outbound-only MQTT bridges run on every node (`*`) with `Receive.BridgeOutbound: false`, because each node's bridge then forwards exactly that node's own publishes (19). A startup WARN names devices that break the rule (6.2).
- **Redfish ignores NodeId.** Gateways are loaded by type (`internal/redfish/manager.go:142`, `GetByType`), so an enabled gateway runs on every node where Redfish is on. Each node derives its retained `<topicPrefix>/<chassis>/sensors/<id>` values (default prefix `redfish`) from the bus, replicas included (`internal/redfish/subscriber.go:69,142-147,168`), so every derived value arrives twice. Enable Redfish on one node only, or add its prefix to `Capture.Exclude` (14.6). Startup WARN (6.2).
- **Host monitoring** runs on every node that enables it. That is harmless with the default `BaseTopic` `nodes/{NodeId}/host` (`internal/config/config.go:420`; `internal/hostinfo/collector.go:34`), because each node publishes under its own NodeId. A `BaseTopic` without `{NodeId}` makes both nodes write the same topics; startup WARN (6.2).
- **RTSP frames.** Inline publishes bypass `MaxMessageSize` locally, but a frame above `MaxRecordBytes` is dropped at capture (`captureDropped{size}`, 8.2). Frames that fit dominate the log: each frame is published twice, to `<topicPrefix>/capture/frames/<slot>` and `<topicPrefix>/capture/latest/pic` (plus `capture/snapshot/pic` on a trigger; `internal/bridge/rtspcamera/connector.go:554-620`, default prefix `cameras/camera`, `internal/bridge/rtspcamera/config.go:101,129`). At 10 fps × 300 KB that is about 6 MB/s, which fills the default 256 MiB in about 45 s. Recommendation: `Capture.Exclude: ["<topicPrefix>/capture/#"]` for camera topics that the peer does not need.
- **Derived publishes.** A script or bridge that publishes in reaction to a forwarded message is captured too, and its output goes to the peers. Identity republishing on the same topic loops between the nodes. An MQTT bridge subscribed to a peer re-publishes the peer's messages locally, and they are forwarded back. The guards are in 14.6.
- **Native command replies** go to the requester's `ResponseTopic` (`hook_winccoa.go:184-197`). Forwarding them is harmless: subscribers of that reply topic on the peer receive them as well.
- In v1, internal publishes carry client id `inline` and an empty username (R10 limitation). Carrying the real API user is an M6 item.

### 7.5 Known capture gaps and shutdown rules

1. **Shutdown wills are not captured.** From `BeginDrain` on, every will comes from the broker's own shutdown (`mqtt.CloseListeners` disconnects clients, and `attachClient` then calls `sendLWT`, `server.go:485-494`). Those clients are expected to reconnect to the peer, and forwarding their wills would mark them offline there. Trade-off: a client that never reconnects anywhere keeps its last state on the peer. This is documented.
2. **Delayed wills.** `sendDelayedLWT` publishes to subscribers but fires `OnWillSent` only if the client is still in `s.Clients` (`server.go:1908-1922`). Optional fix E3.
3. **Will properties.** Wills carry only User properties (`server.go:1685-1687`).
4. **QoS 1 retransmits with DUP** are processed and captured again, just as local subscribers receive them again.
5. **A QoS>0 retained publish whose PUBACK write fails** is now captured (in `OnRetainMessage`) but was never delivered to local subscribers (`server.go:1061-1064`). Retained state on the two nodes stays consistent.

---

## 8. The in-memory log

### 8.1 Structure

```go
const chunkSlots = 1024

type chunk struct {
    base  uint64                          // first offset in this chunk
    slots [chunkSlots]unsafe.Pointer      // *byte of an immutable frame; length from its recLen prefix; nil = gone
}

type Log struct {
    mu         sync.Mutex
    epoch      uint64                      // crypto/rand, non-zero
    startMono  time.Time
    chunks     []*chunk                    // ascending; chunks[0] contains lso
    spare      atomic.Pointer[chunk]       // preallocated outside the lock by a refill goroutine
    lso, leo   uint64                      // offsets start at 1; empty when lso == leo
    bytes      int64                       // accounted footprint (8.7)
    lwm        uint64                      // cached min(C[c])
    consumers  []*consumer                 // fixed at startup (Serve peers); read-only slice
    waiters    []*waiter                   // sessions in long poll; few
    minWakeAt  uint64                      // min(waiter.wakeAt); math.MaxUint64 if none
    sealed     bool                        // set by Drain under mu (15.6); Append then discards and counts
    // atomics: appended{client,inline,will}, trimmed, evictedUnread, evictedBy{count,bytes},
    //          captureDropped{reason}, uncapturedAtShutdown
}

type consumer struct {
    nodeID     string
    committed  uint64        // C[c]
    served     uint64        // S[c]
    acctNext   uint64        // loss accounted below this offset (8.5)
    lostTotal  uint64
    session    *session
    instance   uint64
    state      ConsumerState // NEVER_CONNECTED | CONNECTED | DISCONNECTED
    lastFetch  time.Time
}

type waiter struct { wakeAt uint64; notify chan struct{} } // cap 1, allocated once per session
```

- **Frames are immutable and never reused.** Each frame is one exact-size heap allocation, and its length is in its own `recLen` prefix. A slot holds only a pointer, which costs one allocation per record. Slots are read and written with `atomic.LoadPointer`/`StorePointer`.
- **Chunks never move.** Growth takes the preallocated `spare` chunk, so it costs O(1) under the lock. If `spare` is empty, which is rare, the chunk is allocated under the lock and the event is counted. The `chunks` slice holds at most about `MaxMessages/1024` pointers, so appending to it is negligible.
- **Readers** snapshot `(lso, leo, chunk pointers for the range)` under `mu` in O(1). They load slot pointers outside the lock. A nil slot means the record was evicted after the snapshot: the batch is truncated there (`TRUNCATED` flag), and the next fetch reports the gap.
- **The interface is stable:** `Append`, `Read(from, maxRecords, maxBytes, out *[][]byte)`, `Commit(c, off)`, `Wait(w)`, `Snapshot()`, `Seal()`/`Sealed()`. The M6 arena can replace the implementation without a protocol change.

### 8.2 Append (hot path, publisher goroutine)

1. **Outside the lock:**
   - `size := wire.RecordSize(cl, pk)` (pure arithmetic);
   - `buf := make([]byte, size)`;
   - `wire.EncodeRecord(buf, ...)`.

   That is one exact-size allocation and one payload `memcpy`.
2. **Size guard.** A record larger than `MaxRecordBytes` (default: the broker `MaxMessageSize` + 64 KiB metadata allowance, or 1 MiB + 64 KiB if `MaxMessageSize` is 0) or larger than `MaxBytes/4` is not captured. It is counted as `captureDropped{size}`.
3. **Under `mu`:**
   - if `sealed` (15.6): count `uncapturedAtShutdown`, unlock and discard the frame;
   - `off := leo; leo++`; count `appended{client|inline|will}` (will for wills, inline for `cl.Net.Inline`, client otherwise);
   - store the pointer, `bytes += accounted(size)`;
   - evict while `leo-lso > MaxMessages || bytes > MaxBytes` (8.5);
   - `if leo > minWakeAt`, non-blocking send to each due waiter's `notify` (no allocation).
4. **Unlock.** If the last chunk is more than 50 % full and `spare` is nil, signal the refill goroutine with a non-blocking send.

The lock is never held across I/O or a blocking send, which is the BatchingQueueStore rule (`dev/done/plan-queue-performance.md:13-15`).

### 8.3 Multiple consumers (R4, R6)

The consumer set is the static list of `Serve` peers. Every consumer exists from the start of the epoch with `C[c] = 1`, **even before it first connects**. A consumer that connects late, or that restarts, therefore receives everything captured since the source started, within the bounds. Retained state from before the epoch is covered by the snapshot (16.5).

A peer that is configured but never connects pins the log at its maximum. That is by design (Q20). The source logs a WARN after `NeverConnectedWarnSec` (default 300 s).

### 8.4 Commit and trim (R7)

- On every `FETCH.commit != 0` or `COMMIT`, set `C[c] = max(C[c], commit)`. A commit greater than `leo` is a protocol error.
- After a change:
  1. Recompute `lwm = min(C[c])` in O(#peers).
  2. Under the lock, compute the trim range `[lso, lwm)`, advance `lso`, update `bytes` and `trimmed`, and detach any chunks that are now fully below `lso`.
  3. Release the lock, then clear the trimmed slots that remain in the partially trimmed chunk with atomic stores.

  No reader can be serving those offsets: each session reads only offsets at or above its consumer's `C[c]`, and `C[c] ≥ lwm`.
- "Read" means **applied** by the consumer, that is, injected or dropped by policy. Fetched alone is not enough.

### 8.5 Maximum length, eviction and loss accounting (R9)

**Limits.** `Log.MaxMessages` (default 2,000,000) and `Log.MaxBytes` (default 256 MiB, provisional; final value from the M1 restart measurement, 8.7). When either limit is hit, the oldest record is evicted (`lso++`, slot cleared under the lock, O(1) per evicted record), whether or not it has been read. `evictedBy{count|bytes}` records which limit caused the eviction.

**One loss formula, applied once.**

- **Source side, per consumer.** `acctNext_c = max(acctNext_c, C[c], S[c])`. Whenever the consumer is observed (HELLO, FETCH, status snapshot) and `lso > acctNext_c`:
  ```
  lostTotal_c += lso - acctNext_c
  acctNext_c = lso
  ```
  Because `acctNext_c` only increases, each record is counted at most once. Only records **never served** to `c` are counted, so the source count is a lower bound.
- **Source global.** `evictedUnread++` when the evicted offset is ≥ `lwm`. This is O(1).
- **Consumer side.** `gapLostTotal += deliveredBase - requestedOffset` for every `GAP` batch and every `lostOnResume`. Here `requestedOffset` is the consumer's `appliedNext`, or `C[c]` after a consumer restart. This count is an upper bound: after a consumer crash it can include up to one batch the consumer had applied but not committed.
- **Relationship.** Without a consumer crash the two counts are equal. Both are exposed. Drops are never silent (`plan-winccoa-broker-embedded-manager.md:70`). This replaces AC-30 (S2).

### 8.6 Long poll

A fetch is valid only if `lso ≤ offset ≤ leo`. Otherwise:

- `offset < lso` takes the GAP path;
- `offset > leo` gets `GOAWAY(offset_out_of_range)`.

With `offset ≤ leo` guaranteed, the wait condition is `offset + minRecords > leo`. In that case the session:

1. registers `waiter{wakeAt: offset+minRecords}` under `mu` and updates `minWakeAt`;
2. unlocks;
3. waits in `select` on `notify`, a reusable timer (`maxWaitMs`), the session context, or `stopping`;
4. deregisters and re-reads.

If records are available but fewer than `maxRecords`, and the FETCH carries `lingerMs > 0`, the session waits up to `lingerMs` more for the batch to fill. The default is 0; M5 decides the default from gate G7.

After `maxWaitMs` an empty `BATCH` (flag `EMPTY`) is sent. It doubles as the heartbeat.

### 8.7 Sizing and accounting

- **Record size** = 44 B header + topic + clientId + username + props + payload. Example: topic 40 B, clientId 16 B, payload 100 B gives 200 B.
- **Accounted size** = Go size class of the frame (a table copied from `runtime/sizeclasses.go` for ≤ 32 KiB; 8 KiB page rounding above) + 8 B slot. The example accounts 208 + 8 = 216 B. Chunk headers are counted when a chunk is attached. With this, `MaxBytes` bounds the real heap footprint of the log.
- **Capacity.**
  ```
  capacityRecords = min(MaxMessages, MaxBytes / accountedRecordBytes)
  capacitySeconds = capacityRecords / appendRate
  ```
  A live gauge `capacitySeconds = (MaxBytes - bytes) / smoothedAppendBytesPerSec` is exported. With 256 MiB and 216 B records: about 1.24 M records, which is about 62 s at 20,000 msg/s and about 12 s at 100,000 msg/s. The count bound only governs records below about 134 B, so the "max queue length" is in practice a byte bound. The docs say so.
- **Restart window.** M1 measures `T_restart` (p95) for the standalone broker and for the embedded WCCOAmmq. The default `MaxBytes` is then set so that `referenceRate × accountedBytes × 2 × T_restart` fits. Reference workload: 20,000 msg/s with 200 B records (10× AC-34). If the default would not fit on a 4 GB Pi 4, the docs instead state the lossless outage window per rate.
- **RSS.** Up to about 2 × `MaxBytes` + baseline under GOGC=100 while the log is full (est.). A new top-level `Runtime.MemoryLimitMB` (Q23) calls `debug.SetMemoryLimit`. The repo has no GC tuning today. A startup WARN fires when `2.2 × MaxBytes + 150 MiB` exceeds the limit. The embedded manager inherits its environment, so the config knob is the supported path. Gate G6.

---

## 9. Wire protocol `mmq-peer/1`

### 9.1 Transport

- One long-lived TCP connection per link. The consumer dials the source's peer listener, default port **1890**. That port does not collide with 1883/1884/8883/8884/4000/4443/8000 (`config.go:398-433`).
- `TCP_NODELAY` stays on (the Go default); batching is done by the protocol. TCP keepalive 15 s.
- **Writes** go to the raw `*net.TCPConn` (plaintext) or the `*tls.Conn`. Only the read side is wrapped, with a 64 KiB `bufio.Reader` that also serves the sniff peek. At session start the server asserts that the plaintext writer is a `*net.TCPConn`; otherwise it uses a 64 KiB `bufio.Writer`. A unit test with a counting conn asserts at most `ceil(records/1024)+1` write calls per plaintext batch (`internal/poll` splits writes at 1024 iovecs).
- **Optional TLS** (≥ 1.2; 1.3 is required when a `SharedSecret` is used, 17.3) with ALPN `mmq-peer/1` and `http/1.1`. `HandshakeContext` runs with a 10 s deadline immediately after admission.
- **Byte order:** little-endian throughout. **Strings:** `str8` = `u8 len` + UTF-8 (NodeIds, ≤ 64); `str16` = `u16 len` + bytes.

### 9.2 Admission and sniffing (one port, three uses)

Order of checks on every accepted connection:

1. **`Listener.AllowedNetworks`** (if set), right after `Accept`. A non-matching IP is closed before any read or TLS.
2. **Pre-auth limits:** at most 16 unauthenticated connections in total, and at most `Listener.MaxPreAuthPerIp` (default 2) per remote IP. Excess connections are closed. A configured peer's IP is never starved by another IP.
3. **Sniff** the first byte, with a 10 s deadline:

| First byte | Meaning | Handling |
|---|---|---|
| `0x16` | TLS ClientHello (only when TLS is enabled) | Handshake. Then ALPN `mmq-peer/1` → peer protocol; ALPN `http/1.1` → status endpoint, for **mTLS-authenticated peers only**. |
| `M` | Plaintext peer protocol preamble | Accepted only when TLS is off, or when `Listener.AllowPlaintext: true` (migration, 17.5) |
| `G`, `P` | Plaintext `GET`/`POST` | Status and resync endpoints, **loopback only**. A one-shot `net/http` handler serves `GET /peerlink/v1/status` and `POST /peerlink/v1/resync`. |
| other | — | Close |

### 9.3 Preamble and framing

```
PREAMBLE (C→S, once, not a frame):  magic "MMQP" [4] | u16 versionMajor = 1 | u16 versionMinor = 0      (8 bytes)
FRAME:                              u32 frameLen (bytes after this field) | u8 type | body
```

- The server checks the major version. If it differs, it sends `GOAWAY(version)` and closes.
- **Frame caps.** Before `HELLO_OK`, the cap is 4 KiB. After it, the consumer accepts frames up to `max(Fetch.MaxBytes, sourceMaxRecordBytes) + 64 KiB`, where `sourceMaxRecordBytes` comes from `HELLO_OK`. That is bounded by `Receive.MaxFrameBytes` (default 16 MiB + 64 KiB). If the source announces a larger record size than that, the consumer logs an ERROR at the handshake and keeps the link up, because the source substitutes tombstones (9.7). The source accepts frames up to 64 KiB after authentication: consumers send only small frames. A larger frame is a framing error.
- **Forward compatibility.** Every decoder ignores trailing bytes in a frame body beyond the fields it knows. Unknown frame types are ignored within the same major version. A minor version can therefore append fields and add frame types.
- No per-consumer state is touched before authentication completes.

### 9.4 Frame catalogue

| Type | Dir | Body (field: type) |
|---|---|---|
| `SERVER_HELLO` 0x01 | S→C | `versionMajor u16`, `versionMinor u16`, `capabilities u64`, `authModes u8` (b0 client cert requested, b1 shared secret configured), `nonceS [32]` |
| `HELLO` 0x02 | C→S | `flags u16` (b0 MAC present), `capabilities u64`, `instanceId u64`, `lastEpoch u64`, `resumeOffset u64` (consumer `appliedNext`, 0 = none), `lastSeenLeo u64`, `maxRecordBytes u32` (largest record it accepts), `retainedClass u8` (0 MEMORY, 1 DB, 2 WINCCOA), `nonceC [32]`, `mac [32]` (zero if no MAC), `consumerNodeId str8`, `expectedSourceNodeId str8`, `topicRoot str16` |
| `HELLO_OK` 0x03 | S→C | `flags u16` (b0 SOURCE_RESET, b1 CONSUMER_STATE_USED, b2 SNAPSHOT_AVAILABLE), `capabilities u64` (agreed set = intersection), `epoch u64`, `resumeAt u64`, `logStart u64`, `leo u64`, `committed u64`, `lostOnResume u64`, `wallNowMs i64`, `monoNowMs u64`, `maxRecordBytes u32` (source capture cap), `retainedClass u8`, `macS [32]` (zero if no MAC), `sourceNodeId str8`, `topicRoot str16` |
| `GOAWAY` 0x04 | both | `code u16`, `reason str16` (empty before authentication when auth is configured, 9.5). The sender closes afterwards. |
| `FETCH` 0x10 | C→S | `fetchId u32`, `flags u16` (b0 SNAPSHOT), `lingerMs u16`, `offset u64`, `commit u64` (0 = unchanged), `maxRecords u32`, `maxBytes u32`, `minRecords u32`, `maxWaitMs u32` |
| `BATCH` 0x11 | S→C | `fetchId u32`, `flags u16` (b0 GAP, b1 EMPTY, b2 CRC, b3 SNAPSHOT, b4 SNAPSHOT_END, b5 TRUNCATED), `reserved u16`, `baseOffset u64`, `count u32`, `recordsBytes u32`, `logStart u64`, `leo u64`, `lost u64`, `sourceMonoMs u64`, `sourceWallMs i64`, `crc32c u32` (valid if b2) (68 bytes), then `recordsBytes` of records |
| `COMMIT` 0x12 | C→S | `commit u64` |
| `PING` 0x13 / `PONG` 0x14 | C→S / S→C | `token u64` |

**Capability bits (v1.0):**

| Bit | Name | Meaning |
|---|---|---|
| b0 | `BATCH_CRC` | The sender computes the CRC on plaintext links. Off under TLS unless `Fetch.CrcOnTls`. |
| b1 | `SNAPSHOT_FILL` | Automatic retained snapshot (16.5) |
| b2 | `RESYNC_NEWER` | Operator-triggered resync (16.5) |
| b3 | `TOMBSTONE` | Mandatory in 1.0 |

Bits that are not understood are ignored. The agreed set is the intersection of both sides.

**`GOAWAY` codes:**

| Code | Name | Meaning / client backoff |
|---|---|---|
| 1 | `version` | Major version mismatch (max backoff) |
| 2 | `unknown_peer` | Consumer NodeId is not a configured peer (max backoff, ERROR) |
| 3 | `not_allowed` | The peer has `Serve: false` (max backoff, ERROR) |
| 4 | `auth_failed` | MAC or certificate check failed. Also the only code any pre-auth rejection returns when authentication is configured (9.5). (max backoff, ERROR) |
| 5 | `identity_mismatch` | Certificate identity ≠ HELLO NodeId (max backoff, ERROR) |
| 6 | `self_connection` | Consumer NodeId == source NodeId (max backoff, ERROR) |
| 7 | `wrong_node` | `expectedSourceNodeId` ≠ own NodeId; crossed address config (max backoff, ERROR) |
| 8 | `superseded` | A newer authenticated session for the same NodeId took over (normal backoff) |
| 9 | `shutdown` | The source or consumer is stopping (normal backoff, INFO) |
| 10 | `protocol` | Framing violation, commit > LEO, transport CRC failure (normal backoff, WARN) |
| 11 | `offset_out_of_range` | FETCH or resume offset > leo in the same epoch. The consumer resets to `lastEpoch=0` and reconnects. |
| 12 | `busy` | Pre-auth limits exceeded |
| 13 | `duplicate_node` | A second process uses this NodeId (9.5). Max backoff, and the source refuses that instance for 5 min. |

### 9.5 Handshake (R13)

```
     (admission 9.2, optional TLS handshake with VerifyConnection 17.2)
C→S  PREAMBLE
S→C  SERVER_HELLO(capabilities, authModes, nonceS)
C→S  HELLO(consumerNodeId, expectedSourceNodeId, instanceId, lastEpoch, resumeOffset, maxRecordBytes, nonceC, mac)
S:   checks in order; the first failure ends the handshake:
       1 consumerNodeId != own NodeId                          self_connection
       2 consumerNodeId is a configured peer                    unknown_peer
       3 that peer has Serve: true                              not_allowed
       4 expectedSourceNodeId == own NodeId                     wrong_node
       5 TLS client cert present → identity == consumerNodeId   identity_mismatch
         peer requires a cert and none was given                identity_mismatch
       6 peer's SharedSecrets configured → mac valid            auth_failed
       7 peer authenticated by 5 or 6, or AllowUnauthenticatedPeers
     If any authentication method is configured on this listener, every failure in 1-7 is sent as
     GOAWAY(auth_failed) with an empty reason; the precise reason is logged locally only.
S→C  HELLO_OK(epoch, resumeAt, ..., macS, sourceNodeId, topicRoot)
C:   checks sourceNodeId == configured NodeId (and != own), == certificate identity (TLS), macS valid;
     on failure: GOAWAY(identity_mismatch | self_connection | wrong_node | auth_failed), close, back off
```

**MAC (shared secret, TLS only, 17.3):**

- `lp(x)` = `u16 len(x)` + `x`. Every field is length-prefixed.
- `exporter` = `ConnectionState.ExportKeyingMaterial("monstermq-peer/1", nil, 32)`.
- `mac  = HMAC-SHA256(secret, lp("mmq-peer/1 C") | lp(nonceS) | lp(nonceC) | lp(consumerId) | lp(sourceId) | lp(exporter))`
- `macS = HMAC-SHA256(secret, lp("mmq-peer/1 S") | lp(nonceC) | lp(nonceS) | lp(sourceId) | lp(consumerId) | lp(exporter))`
- The consumer computes `mac` with its first configured secret. The source verifies against each secret in the peer's list (rotation, 17.5) and computes `macS` with the one that matched. The consumer verifies `macS` against its list.
- Comparisons use `hmac.Equal`. A relay that terminates TLS separately on each side yields different exporters, so the MAC fails.

**TopicRoot mismatch.** Both sides log a WARN once and set `topicRootMismatch=1`. The receiver filters both its own root and the announced one (12.2).

**Retained store class mismatch.** If `retainedClass` differs between the two sides, both log a WARN once. The 24 h in-memory purge applies only to MEMORY (12.3, K10).

**Session takeover and duplicate NodeIds.**

- There is one active session per consumer NodeId. The newest authenticated session wins, and the old one gets `GOAWAY(superseded)`.
- A crash-restart is the normal case: a new `instanceId` arrives while the dead session is still open. It is not reported.
- A **duplicate** is declared only when one of these happens:
  - at least 3 takeovers alternate between two distinct `instanceId`s within 60 s;
  - a superseded session keeps sending frames after its `GOAWAY`.
- The source then:
  - increments `duplicateConsumer` and logs an ERROR with both remote addresses;
  - keeps the earlier-established instance;
  - answers the newer instance with `GOAWAY(duplicate_node)` and refuses it for 5 min.
- Cursor corruption by flapping is therefore bounded.

### 9.6 Resume rules (decided by the source)

| Consumer state in HELLO | Source decision | `lostOnResume` |
|---|---|---|
| `lastEpoch == epoch`, `resumeOffset ∈ [lso, leo]` | `resumeAt = resumeOffset`; `C[c] = max(C[c], resumeOffset)`; flag `CONSUMER_STATE_USED`. This is a connection drop with both processes up: no loss, no duplicates. | 0 |
| `lastEpoch == epoch`, `resumeOffset < lso` | `resumeAt = lso`; `C[c] = max(C[c], resumeOffset)`; source accounting per 8.5 | `lso - resumeOffset` |
| `lastEpoch == epoch`, `resumeOffset > leo` | `GOAWAY(offset_out_of_range)`. Only a bug can cause this; the consumer resets its state. | — |
| `lastEpoch == 0` (consumer restarted, or first contact) | `resumeAt = max(C[c], lso)`; flag `SNAPSHOT_AVAILABLE` if agreed (16.5). This is the source-held high-water mark that makes a consumer restart lossless (R4). Up to one batch may be duplicated (15.3). | `max(0, lso - C[c])` |
| `lastEpoch != 0 && != epoch` (source restarted) | Flags `SOURCE_RESET` and `SNAPSHOT_AVAILABLE`; `resumeAt = max(C[c], lso)`, normally 1. The consumer increments `sourceResets` and records `resetLostLowerBound = lastSeenLeo - resumeOffset` of the old epoch. It then drops its old dedup state. | `max(0, lso - C[c])` |

### 9.7 Fetch and batch

- **Kafka semantics.** A batch returns at least one record when any is available, even if that record is larger than `maxBytes`. Otherwise the source waits for `minRecords` or `maxWaitMs` (8.6).
- **Contiguous offsets.** A batch always holds `count` records at offsets `baseOffset .. baseOffset+count-1`. The source never filters inside a batch. The consumer sees a gap only through `lost`/`GAP`: `baseOffset = requestedOffset + lost`.
- **Oversize tombstone.** If a stored record is larger than the consumer's announced `HELLO.maxRecordBytes`, the source sends a 44-byte tombstone in its place (record flag `SKIPPED`, no variable part) and counts `servedSkipped{size}`. The consumer counts `dropped{size_source}` and advances. This makes asymmetric `MaxMessageSize` configurations safe.
- **Serve path.**
  1. Under `mu`, the session snapshots the range and chunk pointers in O(1).
  2. Outside the lock, it loads the frame pointers into its reusable `[][]byte`. A nil frame truncates the batch (`TRUNCATED`).
  3. It computes the CRC if agreed, then writes:
     - plaintext: header + frames as one `net.Buffers.WriteTo` on the raw `*net.TCPConn`;
     - TLS: through a 64 KiB `bufio.Writer`.
  4. Afterwards it clears the reusable slice (`clear(buf[:n])`), so it never pins evicted frames.
- **Natural batching.** While batch N is being injected, new records accumulate. The next fetch takes up to `Fetch.MaxRecords` (default 4096) or `Fetch.MaxBytes` (1 MiB). Idle defaults (`minRecords=1`, `maxWaitMs=1000`) give minimal latency. `lingerMs` trades latency for CPU at moderate rates (gate G7).
- **Pipelining** (`Fetch.Pipeline`, default 1, maximum 2). With 2, the reader sends FETCH N+1 as soon as it has read the header of BATCH N. The duplicate window stays at one batch, because COMMIT follows every applied batch (9.8). The default is chosen by measurement in M5 (Q14).
- **Failure classes (liveness rule).** No deterministic failure can stall a link for more than three retries.

| Class | Examples | Action |
|---|---|---|
| Transport | `frameLen` above the cap; CRC mismatch; truncated frame; deadline | Apply nothing from this batch; `GOAWAY(protocol)`; reconnect; resume at `appliedNext`. On the 3rd consecutive CRC failure at the same `baseOffset`, treat the batch as poison (next row). |
| Batch-structural (CRC valid or absent) | Records overrun `recordsBytes`; record count ≠ `count` | Apply the records before the fault. Count the rest of `baseOffset..baseOffset+count-1` as `dropped{malformed}`. Advance to `baseOffset+count`. ERROR log. |
| Record content | `recLen` invariant broken; QoS > 2; invalid topic/UTF-8; NUL; wildcard; malformed TLV | `dropped{malformed}`; the record counts as applied; continue |
| Policy | Namespace, filter, size, expiry, stale, will superseded | `dropped{reason}`; continue (12.2) |

### 9.8 Commit semantics

- `commit = c` means: every record with offset < c has been applied locally. The `InjectPacket`/`RetainOnly` call returned, and the batched retained writes for those records have been flushed (13.4).
- The injector sends `COMMIT` through the writer goroutine:
  - after every applied batch;
  - every 100 ms during a long apply (flushing the retained writer first);
  - on graceful shutdown.

  FETCH also carries the latest commit.
- The consumer keeps `(epoch, appliedNext, lastSeenLeo)` per source **in memory across reconnects**. Records with `offset < appliedNext` are skipped in O(1).

### 9.9 Keepalive and timeouts

| Timer | Default | Action |
|---|---|---|
| `Fetch.MaxWaitMs` | 1000 | The source returns an empty batch, so there is traffic at least once per second |
| Consumer progress deadline | `MaxWaitMs + KeepAliveSeconds*1000` until the batch header arrives; then reset after every 64 KiB read | Close and reconnect |
| Consumer `PING` | every `KeepAliveSeconds/2` while no FETCH is outstanding (the writer goroutine sends it independently of injection) | — |
| Source read deadline | `3 × KeepAliveSeconds` | Close the session. `C[c]` is kept. |
| Source write progress deadline | `KeepAliveSeconds` per 64 KiB written | Detects a dead peer with a full socket buffer, without killing slow links |
| Dial / TLS handshake / HELLO | 5 s / 10 s / 10 s | Close and back off |

Progress-based deadlines let a 1 MiB batch cross a 1 Mbit/s link (about 8 s) without livelock. Optionally, `Fetch.MaxBytes` adapts to the measured link throughput (M6).

### 9.10 Reconnect and backoff

- Exponential backoff with ±20 % jitter: 200 ms initial, ×2, capped at `ReconnectMaxMs` (default 30 s). The backoff resets after a session has lived 30 s or has applied at least one batch.
- `GOAWAY` codes that indicate configuration errors (`unknown_peer`, `not_allowed`, `auth_failed`, `identity_mismatch`, `self_connection`, `wrong_node`, `version`, `duplicate_node`) go straight to the cap. They are logged as ERROR once per distinct (peer, code) and then every 5 min. Fixing the peer's configuration heals the link without a restart on this side.

### 9.11 Versioning and rolling upgrades

- The preamble carries major.minor. Minor versions may:
  - append fields to frame bodies;
  - add frame types, capability bits, record header fields (`hdrLen`) and TLV ids.

  Older decoders skip all of these. Semantics-changing record changes need a new major.
- A rolling upgrade of a redundant pair (one host at a time) is supported across minor versions. The procedure is documented in `winccoa/README.md`.
- An N-1 interoperability test is part of every minor release (PL-27). A major bump requires both nodes to be upgraded inside the log window. The docs say so.

---

## 10. Record format (identical in the log and on the wire)

```
off size field
  0   4  recLen          u32  bytes after this field
  4   1  recVersion      u8   1
  5   1  hdrLen          u8   44 in v1.0 = offset of the variable part; decoders accept >= 44 and skip extra
  6   2  flags           u16  b0-1 QoS | b2 retain | b3 dup (source) | b4 will | b5 inline | b6 reserved
                              | b7 payloadFormat present | b8 snapshot | b9 SKIPPED (tombstone) | b10-15 reserved, ignored
  8   8  publishWallNs   i64  source wall clock at capture, unix ns ("timestamp of publish")
 16   8  captureMonoMs   u64  ms since source epoch start (monotonic); snapshot: source mono "now - age"
 24   4  expirySec       u32  original MessageExpiryInterval, capped by MaximumMessageExpiryInterval; 0 = none
 28   1  payloadFormat   u8   valid if b7
 29   1  reserved        u8   0
 30   2  topicLen        u16
 32   2  clientIdLen     u16
 34   2  usernameLen     u16
 36   4  propsLen        u32
 40   4  payloadLen      u32
 44   …  (from hdrLen) topic | clientId | username | props | payload
```

**Invariant:** `4 + recLen == hdrLen + topicLen + clientIdLen + usernameLen + propsLen + payloadLen`. A tombstone has all lengths 0 and flag b9.

**Props block.** Uniform TLV: `id u8 | len u32 | value`. Order is preserved and duplicates are allowed. Unknown ids are skipped and counted (`unknownProps`). The block is empty in the common case.

```
0x03 ContentType      value = UTF-8 bytes
0x08 ResponseTopic    value = UTF-8 bytes
0x09 CorrelationData  value = bytes
0x26 UserProperty     value = u16 keyLen | key | val      (repeated, original order)
```

`MessageExpiryInterval` and `PayloadFormat` live in the fixed header. `TopicAlias` and `SubscriptionIdentifier` are never carried. A future origin or hop extension (14.5) would be a new TLV, which a v1 consumer safely ignores, because v1 never re-forwards.

**Implicit fields.** The source NodeId and epoch come from the session, and each record's offset is `baseOffset + i`. `stores.BrokerMessage` (`internal/stores/types.go:7-27`, a lossy user-property map) and the Kotlin `BrokerMessageCodec` (which drops `senderId` and every MQTT 5 property, `BrokerMessageCodec.kt:9-39`) are deliberately **not** used.

The encoder cannot fail: the exact size is precomputed and every input was validated at capture (7.3). Both the record decoder and the frame codec are fuzzed (`FuzzDecodeRecord`, `FuzzFrame`).

---

## 11. State machines

### 11.1 Puller (consumer side, one per `Pull` peer)

```
          +---------+  Start()   +---------+ dial ok +-----------+ tls ok/none +-----------+ HELLO_OK +-----------+
STOPPED<--| any     |----------->| BACKOFF |-------->| DIALING   |------------>| HANDSHAKE |--------->| [SNAPSHOT]|
  ^ Stop()+---------+            +---------+         +-----------+             +-----------+          +-----+-----+
                                    ^   ^  dial/tls error |   identity/GOAWAY(config) |   SNAPSHOT_END       |
                                    |   +-----------------+   -> backoff = cap, ERROR |                      v
                                    +--------------------------- transport error / deadline --------- +-----------+
                                                                                                      | STREAMING |
                                                                                                      +-----------+
STREAMING runs three goroutines:
  reader:   read BATCH → CRC (if agreed) → decode → handoff channel (cap = Pipeline)
  injector: for each record in offset order: skip offset < appliedNext → validate/filter → pace (12.6)
            → InjectPacket | RetainOnly | drop; then FlushReplicas → appliedNext = base+count → COMMIT
  writer:   FETCH(offset=next, commit=appliedNext), COMMIT, PING; the only writer of the connection
Stop(): finish current batch → FlushReplicas → COMMIT(appliedNext) → GOAWAY(shutdown) → STOPPED
```

State kept across reconnects, in memory only: `epoch`, `appliedNext`, `lastSeenLeo`, the counters.

### 11.2 Source session (per accepted connection)

```
ACCEPTED → ADMISSION (AllowedNetworks, pre-auth limits) → SNIFF → [TLS_HANDSHAKE] → PREAMBLE → SERVER_HELLO sent
        → AUTH (HELLO checks) → HELLO_OK → [SNAPSHOT streaming on FETCH(SNAPSHOT)] → STREAMING → CLOSED
                 └→ STATUS_HTTP (loopback, or mTLS peer for GET) → CLOSED
Any timeout or GOAWAY → CLOSED. Entering STREAMING supersedes an older session of the same consumer (9.5).
```

### 11.3 Consumer as seen by the source

```
NEVER_CONNECTED --first STREAMING--> CONNECTED <--session ends / reconnect--> DISCONNECTED
(NEVER_CONNECTED after NeverConnectedWarnSec → WARN; C[c] keeps pinning the log in every state)
```

### 11.4 Record lifecycle on the source

```
captured (offset assigned) → served (S[c] advanced) → committed by all (offset < LWM) → trimmed
                         \→ evicted (count/byte bound) → lost for c if never served to c (8.5)
```

---

## 12. Receiver-side injection

### 12.1 Injector client

```go
inj := server.NewClient(nil, peerlink.ListenerID /* "peerlink" */, "peerlink:"+sourceNodeID, true)
inj.Properties.ProtocolVersion = 5
```

- **Not added to `s.Clients`.** It is not a session, is never expired and cannot be taken over. It still increments the broker-wide `$SYS` counters `messages/received` and `packets/received`, because `InjectPacket` adds to the shared `Info` (`server.go:944-947`). This is documented. A private `Info` per injector client is optional (E7).
- **ProtocolVersion 5.** `InjectPacket` copies it into `pk.ProtocolVersion` (`server.go:937`). The per-message expiry purge requires `== 5` (`server.go:1881`). An `OnPublish` rejection of a v<5 QoS>0 publish would call `DisconnectClient` (`server.go:1031-1032`).
- **Inline bypasses.** Inline clients skip ACL, topic validity, the native hook and the receive quota (`server.go:954,962,255-260`; `hook_winccoa.go:112-115`). The receiver therefore validates every record itself (12.2).
- **Reserved names.** No MQTT listener may use the id `peerlink` (config validation). Network client ids equal to `inline` or starting with `peerlink:` are rejected in the PeerLink hook's `OnConnect` (7.2): it sends CONNACK 0x85 (client identifier not valid) and returns the code, and `attachClient` ends the connection before authentication (`internal/mqtt/server.go:448-451`). An error returned there refuses the connection whatever the auth configuration: `AuthHook` is only registered with UserManagement, otherwise `auth.AllowHook` runs (`internal/broker/server.go:247-264`), and neither provides `OnConnect`. `inline` is the id of the shared inline client, which sits in `s.Clients` (`internal/mqtt/server.go:34,202-205`); `peerlink:` is the injector prefix. This closes the `Origin`/NoLocal confusion with internal publishes and replicas.

### 12.2 Validation and filtering per record (in this order)

All failures here are per-record: they increment `dropped{reason}`, the record counts as applied, and the link continues (9.7).

1. **Structure** (`malformed`): the `recLen` invariant; QoS ≤ 2; `topicLen > 0`; topic valid UTF-8 with no NUL, `+` or `#`; strings valid UTF-8; TLV well-formed; `captureMonoMs ≤ batch.sourceMonoMs`.
2. **Tombstone** (`size_source`): flag `SKIPPED`.
3. **Namespace** (`namespace`): the topic is under the receiver's **own** `TopicRoot` or under the source's announced root, or it starts with `$`. This check is mandatory, because inline injection bypasses ACL, `IsValidFilter` and the native hook.
4. **Per-peer filters** (`filtered`): `Receive.Include/Exclude`.
5. **Size** (`size`): the payload exceeds the receiver's `MaxMessageSize` (inline injection skips the engine's size check).
6. **Age at injection.** This is computed immediately before injection, not at batch build time:
   ```
   ageNow = (batch.sourceMonoMs - rec.captureMonoMs)            // on the source, monotonic
          + (consumerMonoNow - batchRecvMono)                    // waiting and applying on the consumer
          + rttHalfMs                                            // smoothed from PING/PONG and FETCH→BATCH
   ```
   - **Expiry** (`expired`): `expirySec > 0 && ageNow >= expirySec*1000`.
   - **Staleness** (`stale`, optional): `Receive.MaxRecordAgeMs > 0 && ageNow > MaxRecordAgeMs`.
7. **Will supersession** (`will_superseded`). For a record with `will=1`, the will is dropped if either:
   - a local client with that client id is connected now (`ClientState.Connected`, i.e. `s.Clients.Get(id)` and not `Closed()`); or
   - such a client established a session on this node at a time ≥ `consumerMonoNow - ageNow`.

   Connect times come from the hook's `OnSessionEstablished`: a sharded map from client id to mono time, pruned of entries older than 24 h. This comparison is skew-free.

**Retained records that fail a policy step (12.2 steps 2-6):**

| Reason | Retained action |
|---|---|
| `stale` | Apply the value **silently** through `Server.RetainOnly` (E4): the retained store is updated, and there is no live delivery. Deletes are applied the same way. |
| `size`, `size_source`, `expired` | Not applied. Counted in `retainedDiverged{reason}`, with a rate-limited WARN naming the topic. |
| `filtered`, `namespace` | Intentional; counted as `dropped` only |

### 12.3 Building and injecting the packet

```go
nowMs := time.Now().UnixMilli()
pk := packets.Packet{
    FixedHeader: packets.FixedHeader{Type: packets.Publish, Qos: q, Retain: r}, // Dup=false: DUP is per hop [MQTT-3.3.1-3]
    TopicName:   topic,                    // interned string (two-generation cache, 12.3)
    Payload:     payload,                  // aliases the per-batch buffer; the engine deep-copies wherever it retains
    PacketID:    pid(q),                   // 1 iff q > 0: PublishValidate (internal/mqtt/packets/packets.go:670-700)
    Properties:  props,                    // heap-copied strings; MessageExpiryInterval = expirySec;
                                           // TopicAlias=0; SubscriptionIdentifier=nil
    Origin:      clientId,                 // E1: original publisher (interned), so NoLocal behaves per logical client
    Created:     (nowMs - ageNow) / 1000,  // E1: backdated to the capture instant in the receiver's clock frame
    Forward:     &fwds[i],                 // E2: {SourceNode, ClientID, Username, TimeNs, Epoch, Offset, Dup, Will, Snapshot}
}
err := server.InjectPacket(inj, pk)        // one injector goroutine per source: strict source order
```

**Memory ownership.** These rules prevent a batch buffer from being pinned by long-lived engine state.

- `Packet.Copy` deep-copies only `Payload`, `CorrelationData` and `SubscriptionIdentifier`. It copies `TopicName`, `Origin`, `ContentType`, `ResponseTopic` and User key/value by reference (`packets.go:212-217`; `properties.go:131-132,176-184`). The in-memory retained map, inflight entries and the Default archive group's last-value map keep those references (`server.go:1104-1105`; `archive/manager.go:108-118`).
- **Rule:** every string handed to the engine or to `Forward` is a fresh heap string or an interned string. That covers `TopicName`, `Origin`/`ClientID`, `Username`, `ContentType`, `ResponseTopic`, User key/value and `SourceNode`. None of them may alias the batch buffer.
- **`Payload` and `CorrelationData`** may alias the batch buffer, because every retaining path deep-copies them. The only inline subscription (`hmi/sync.go:127`) is on the HMI sync tree, which is excluded by default; when it is forwarded, a request payload pins its batch buffer only until the sync worker has processed it (the buffer is allocated per batch and never reused).
- **Interning.** Topics, client ids and usernames are interned in per-source two-generation caches (64k entries per generation; when the current generation is full it becomes the old one). There is no reset storm above 64k topics.
- **Allocation budget:** at most 1 allocation per record without properties (amortised), plus 2 per batch (the body buffer and the `[]packets.Forward`). Verified by `BenchmarkDecodeBatch -benchmem`.
- **Retention test (PL-34):** 100 batches touch 10k retained topics, then GC is forced. `HeapInuse` growth must stay ≤ 2 × (retained topic + payload bytes).

**Expiry.** The engine computes `Expiry = Created + min(86400, expirySec)` (`server.go:982-986`), so the remaining interval is `expirySec - ageNow`, accurate to ±1 s. `WritePacket` hands each subscriber `Expiry - now` (`clients.go:543-549`).

**24 h purge alignment.** The force-purge of in-memory retained and inflight messages (`now - Created > MaximumMessageExpiryInterval`, `server.go:1886-1888`) applies only to the MEMORY retained store. Backdating `Created` aligns the purge across nodes **only when both use MEMORY**. DB stores purge only rows with an expiry (`internal/stores/sqlite/message_store.go:287-293`). The handshake WARNs on mixed classes (9.5); risk K10.

**Errors.** An error from `InjectPacket` is counted as `rejected` and logged as a rate-limited WARN. The stream continues.

### 12.4 Resulting semantics on the receiver

| Aspect | Behaviour | Evidence |
|---|---|---|
| Path | `InjectPacket` → `PublishValidate` → `processPublish`; no PUBACK or inflight for the inline injector | `server.go:936-950,733-737,1041-1048` |
| QoS | Each subscriber gets `min(QoS, subQoS, MaximumQos)` | `server.go:1155-1249` |
| Retain | Normal `retainMessage`. An empty retained payload **deletes**. Subscribers see retain=false unless RetainAsPublished is set. DB modes: writes are coalesced per batch (13.4). | `server.go:1037-1039,1082-1112`; `topics.go:463-486` |
| Shared subscriptions | Default `Receive.SharedSubscriptions: SKIP`: the hook's `OnSelectSubscribers` clears `Shared`/`SharedSelected` for replicas, so a group gets each message once, from the node it was published on. `DELIVER`: every node's group gets it, i.e. one delivery per node. | `server.go:1132-1138`; `topics.go:322-327`; `hooks.go:379-383` |
| Offline sessions | Default `Receive.Queue: false`: QueueHook skips replicas. A persistent client that fails over gets peer-origin messages live on the node it is connected to, and no duplicate backlog on its old node. | `hook_queue.go:105-145` |
| NoLocal | `Origin = original clientId`, so a client using the same id on both nodes does not receive its own replayed messages | `server.go:1156` |
| Read ACL | Still checked per subscriber at delivery | `server.go:1160-1162` |
| Publish ACL | Enforced on the source. The receiver applies peer-level authorization (authentication, `Receive.Include/Exclude`, namespace filter). | — |
| Wills | After 12.2 step 7: delivered to subscribers and the retained store only, **not** to bus, archives or offline queues. This matches local wills, which never reach StorageHook or QueueHook, and for which offline QoS>0 subscribers get nothing (`server.go:1197-1201,1704-1706`). | `hook_storage.go:63-79`; `hook_queue.go:91-100` |
| `$SYS` counters | Include replicas (12.1) | `server.go:944-947` |
| MQTT 5 limits (pre-existing) | User properties suppressed for RequestProblemInfo=0; DB-retained rows and offline queues store no MQTT 5 properties | `properties.go:326-338`; `hook_storage.go:314-378` |

### 12.5 Receiver subsystems

| Subsystem | Default for replicas | Switch |
|---|---|---|
| Live subscribers | Delivered | — |
| Shared subscription groups | Skipped | `Receive.SharedSubscriptions: SKIP \| DELIVER` |
| Retained store (MEMORY, SQLite, Postgres, Mongo, WINCCOA) | Always applied; the row carries the original client id, username and the backdated time in seconds. There is no switch: in every DB mode, including WINCCOA, the store is the only retained source (`server.go:316-317`; `hook_storage.go:63-67`), so skipping it would mean no retained copy at all. | — |
| Offline queues | Skipped | `Receive.Queue` |
| pubsub bus (GraphQL `topicUpdates`, scripts, Redfish, REST SSE) | Dispatched, flagged `OriginNode` | `Receive.Bus` |
| MQTT bridge outbound (reads the bus) | **Skipped** (loop guard, 14.6) | `Receive.BridgeOutbound` |
| Archive groups | Dispatched with the original publisher and ns time | `Receive.Archive` |
| `messagesIn` metric | Not incremented; counted as `messageBusIn` | — |
| WinCC OA native | Never reached (namespace dropped on both sides) | — |

The gate is a `pk.Forward != nil` check in StorageHook and QueueHook. It follows the existing `replicated` predicate (`hook_storage.go:36-39,282-284`; `server.go:318-320`).

### 12.6 Catch-up pacing

A backlog replayed at the full inject rate can overflow local subscriber channels. Overflow drops messages at 8192 pending (`server.go:72,77,1234-1244`) and on the pubsub bus (`pubsub/bus.go:67-80`). The link would still report zero loss.

- While `lag > Fetch.MaxRecords`, the injector paces itself with a token bucket at `Receive.CatchUpRateFactor × smoothed source append rate`. The default factor is 3, 0 disables pacing, and the floor is 1,000 msg/s. The append rate comes from `leo` deltas over `sourceMonoMs`.
- `Receive.MaxApplyRate` (0 = off) is an absolute cap.
- A factor above 1 always drains the backlog. At 20k msg/s and factor 3, a 400k backlog drains in about 10 s.
- Optional (M6): an engine accessor exposing the maximum subscriber outbound fill, so the injector can back off above a watermark. Trade-off: one slow subscriber then stalls replication.
- Gate G5 (21.2) asserts zero subscriber drops during the replay.

---

## 13. Engine and hook changes (sign-off S5)

### 13.1 E1: keep a preset `Origin` and `Created` for inline injections

`internal/mqtt/server.go:980-981` today:

```go
pk.Origin = cl.ID
pk.Created = time.Now().Unix()
```

Proposed:

```go
now := time.Now().Unix()
if !cl.Net.Inline || pk.Origin == "" {
    pk.Origin = cl.ID
}
if !cl.Net.Inline || pk.Created <= 0 || pk.Created > now {
    pk.Created = now
}
```

- **Backward compatible.** No publish path presets either field before `processPublish`. `Server.Publish` and `PublishPacket` callers leave them zero (`server.go:808-832`; `hook_winccoa.go:184-197`).
- The presets at `server.go:858,886,919` belong to `PublishCurrentValue` and the inline subscribe/unsubscribe paths, which do not go through `processPublish`.
- Network clients cannot set either field.

### 13.2 E2: `Packet.Forward`; E6: `Packet.Will`

```go
// internal/mqtt/packets/packets.go
// Forward describes a publish injected by the peer link. OnPublish, OnRetainMessage, OnSelectSubscribers
// and OnPublished receive the same pk, so they see it. Copy does not copy it.
type Forward struct {
    SourceNode, ClientID, Username string
    TimeNs                         int64  // publish time on the source, unix ns
    Epoch, Offset                  uint64
    Dup, Will, Snapshot            bool
}
// in type Packet:
    Forward *Forward
    Will    bool     // E6: set by sendLWT before retainMessage and before willDelayed.Add; not encoded, not copied
```

- E6 is needed because a retained will reaches `OnRetainMessage` through `sendLWT` (`server.go:1671-1707`), and nothing else distinguishes it from a client's retained publish.
- Cost: 9 bytes per `Packet` value (est.).
- Unit tests assert that `Packet.Copy` (`packets.go:185-249`) copies neither field.

### 13.3 Other engine items

- **E4 (v1). `Server.RetainOnly(cl *Client, pk packets.Packet) error`.**
  - It validates the packet like `PublishValidate`, sets `Origin`/`Created`/`Expiry` with the E1 rules, and calls `retainMessage(cl, pk)`.
  - There is no delivery, no `OnPublished` and no counter increment.
  - Used for stale retained replicas (12.2). In M6 it is also used for the conditional silent clear.
- **E5 (v1). `Server.CloseListeners()`.** It calls `s.Listeners.CloseAll(s.closeListenerClients)` (`server.go:1654,1663-1668`) without `close(s.done)`, `OnStopped` or `hooks.Stop`. Used in the PeerLink shutdown order (6.2).
- **E3 (optional, M6).** In `sendDelayedLWT` (`server.go:1908-1922`), fire `OnWillSent` with a detached stub client when the client has already left `s.Clients`.
- **E7 (optional).** `NewClient` option for a private `Info`, so injector traffic does not count in the `$SYS` received counters.

### 13.4 Broker hooks

**StorageHook (`internal/broker/hook_storage.go`)**

- **`OnPublished` (`:278-311`)**, when `pk.Forward != nil`:
  1. Skip `IncIn()` and call `IncBusIn()`.
  2. If `Forward.Will` is set, or both `Receive.Bus` and `Receive.Archive` are off, return.
  3. Otherwise build the message with:
     - `MessageUUID` derived from `(SourceNode, Epoch, Offset)`: 16 bytes = `fnv64(SourceNode) ^ Epoch` ‖ `Offset`, formatted as a UUID with a custom version nibble. This avoids `uuid.NewString()`, which reads `crypto/rand` per message.
     - `ClientID = Forward.ClientID`;
     - `Time = time.Unix(0, Forward.TimeNs).UTC()`;
     - `IsDup = Forward.Dup`;
     - `OriginNode = Forward.SourceNode`.
  4. Gate the bus by `Receive.Bus` and archives by `Receive.Archive`.
- **`OnRetainMessage` (`:314-378`)**, when `pk.Forward != nil`:
  - `ClientID` and `Username` come from `Forward`.
  - `Time = time.Unix(pk.Created, 0)`, the backdated receiver-frame time in seconds. `OnSelectRetainedMessages` rebuilds `Expiry` from the row time (`:381-404`), so the source wall time would make expiry skew-dependent. PL-03 asserts exactly this.
  - In DB modes (SQLite, Postgres, Mongo, WINCCOA) the write is not executed immediately. It goes into a per-source **pending map** (topic → last value or delete) that the injector flushes with one `AddAll` plus one `DelAll` before every COMMIT (`RetainedAccess.FlushReplicas`, 9.8).
  - A **local** retained write to a topic that has a pending replica entry removes that entry, so the local (newest arrival) value wins. The check is one atomic read when nothing is pending.
  - Trade-off: for up to one batch, a new subscriber on the receiver can see the previous retained value in DB mode.
- **WINCCOA retained.** Every replicated retained message becomes a confirmed `dpSet` on MMQRetained (`internal/stores/oastore/retained.go:40-42,228-279`), batched per flush. The cost is measured in gate G3. Whether passive-host writes are confirmed is an M0 exit criterion (Q9).

**QueueHook (`internal/broker/hook_queue.go`)**

- In `OnPublished` (`:105`), return early when `pk.Forward != nil && (!Receive.Queue || pk.Forward.Will)`.
- In `hydratePersistentClients` (`:65-85`), when PeerLink is enabled, mark only sessions with `SessionInfo.NodeID == own NodeId` (`internal/stores/interfaces.go:57-62`; populated at `hook_storage.go:106`) as offline-persistent.
  - With `SessionStoreType: WINCCOA`, MMQSessions is mirrored, and `IterateSessions` does not filter by node (`internal/stores/oastore/stores.go:397-399`).
  - Without this filter, B would queue every message for clients that are connected to A.

**MQTT bridge bus adapter (`internal/bridge/mqttclient/bus_adapter.go`)**

- Drop bus messages with `OriginNode != ""` unless `PeerLink.Receive.BridgeOutbound` is true. The setting is passed in by broker wiring, so the bridge device config has no new field and there is no parity change.

---

## 14. Loop prevention and topologies (R11, R12)

### 14.1 Rule: split horizon (one hop)

> A node's log contains only publishes that originated on that node. A record applied from a peer is never appended to any log.

1. **Structural.** Every injection uses a per-source client with `Net.Inline=true, Net.Listener="peerlink"` and sets `pk.Forward`. The hook drops both before any other test (7.2).
2. **Consequence.** PeerLink itself never re-forwards: every PeerLink path has length 1, so PeerLink alone cannot form a cycle in any topology (A↔B, the ring A→B→C→A, or any mesh).
3. **Defence in depth.** Self-connections are refused in both directions, and `wrong_node` catches crossed addresses.
4. **Limit of the claim.** Split horizon only recognises PeerLink's own injections. A script, bridge or Redfish gateway that turns a replica into a new internal publish, or an external client that turns it into a network publish, can close a loop (14.6).

### 14.2 Testable invariants

| Invariant | Note |
|---|---|
| `hook.skipPeer == Σ_links injectedViaInjectPacket` | `injectedViaInjectPacket` counts `InjectPacket` calls that returned nil. Policy drops, `rejected` records and `RetainOnly` applications never reach `OnPublished`, so they are excluded. |
| `log.appended{client}` and `log.appended{will}` do not change when only peer traffic flows; `log.appended{inline}` changes only through local internal publishers (for example a script reacting to a replica, 7.4) | Holds on every node, including pull-only nodes, because the hook is registered whenever PeerLink is enabled |

Both are integration-test assertions and live gauges.

### 14.3 Bidirectional

A↔B is two independent links: A pulls from B and B pulls from A, on two TCP connections. Each side dials, so either side can restart on its own, and TLS roles, backpressure and failures stay independent.

### 14.4 More than two nodes

- **A full mesh is required:** n·(n-1) links. Each source log is captured once and shared by its n-1 consumers.
- **A chain or ring** never loops but does not deliver beyond one hop. Documented and tested (PL-05).
- The status endpoint lists the configured and connected links.

### 14.5 Why transitive forwarding is deferred

Re-exporting received records would need all of the following:

- an origin `(node, epoch, offset)` per record;
- per-origin dedup at every hop;
- a hop limit;
- path merging.

A per-origin high-water-mark dedup is **incorrect** when multiple paths exist. If B receives A#5 via C before A#4 via the direct link, a high-water check drops A#4 silently. A correct design needs a sliding-window bitmap per origin and epoch. The format is open for it: it would arrive as a new TLV (section 10).

### 14.6 Loops the link cannot see, and their guards

| Relay | Loop | Guard |
|---|---|---|
| MQTT bridge outbound on B whose remote is A, or a broker bridged back to A. The Go bridge has no loop prevention, unlike the Kotlin `loopPrevention` default true (`MqttClientConnector.kt:541-580`). | Replica on B → bus → bridge → network publish on A → captured → B → … (unbounded with `Receive.Bus: true`) | Bus messages carry `OriginNode`; the bridge outbound skips them unless `Receive.BridgeOutbound: true` (13.4). Startup WARN when an enabled MQTT bridge's `brokerUrl` host equals a configured peer's address host (6.2). PL-31. |
| MQTT bridge on B that subscribes to A (inbound) | The bridge re-publishes A's messages on B as internal publishes; they are captured and forwarded back to A, where they are delivered to the bridge's subscription again → … Unbounded with topic remapping, and independent of `Receive.Bus`. | `BridgeOutbound` does not help (the loop does not pass through the bus); `EchoSuppressMs` only helps for identity mappings. Rule: never bridge to a peer. The startup WARN covers inbound and outbound bridges whose remote host is a peer address (6.2). PL-31 inbound variant. |
| Script or bridge on B deriving a new publish from a replica (captured like every internal publish, 7.4) | Duplicates when the same device also runs on A; an application loop with cyclic mappings or identity republishing on the same topic | Assign each device to one node (7.4, 19, startup WARN); `Capture.Exclude` for the derived topics; `Capture.EchoSuppressMs` for identity republishing |
| Redfish gateways on both nodes. They are loaded by type and ignore NodeId (`internal/redfish/manager.go:142`, `GetByType`). | Each node derives retained `redfish/<chassis>/sensors/<id>` values from the bus, replicas included (`internal/redfish/subscriber.go:69,142-147,168`), and the derived publishes are captured, so every derived value arrives twice | Enable Redfish on one node only, or add its topic prefix to `Capture.Exclude` (`redfish/#` with the default prefix). Startup WARN (6.2). |
| External MQTT client connected to both nodes that republishes what it receives on the same topic | Replica → client → network publish → captured → … | (a) `Receive.MarkReplicas: true` (default false, to keep fidelity) adds the user property `mmq-peer-src=<nodeId>` to injected replicas, so external clients and bridges can filter. (b) `Capture.EchoSuppressMs > 0` (default 0): the hook keeps a sharded map `topic → (fnv64(payload), retain, appliedAtMono)` of replicas applied on this node, and skips capturing a publish, from a network client or an internal publisher, that exactly echoes one within the window, counted as `echoSuppressed` (7.2). Wills are never suppressed. Documented as a deployment rule. PL-04 variant. |

---

## 15. Delivery semantics and failure behaviour

### 15.1 Ordering

| Scope | Guarantee |
|---|---|
| One publisher on the source | FIFO end to end. Hooks run on the publisher goroutine, offsets increase, and the consumer applies in offset order on one goroutine. |
| Different publishers on one source | Log order is a valid linearisation of the capture calls. It may differ from what a local subscriber saw, which MQTT allows. |
| Same retained topic, two publishers on one source | Retained publishes are captured inside `retainMessage` (7.1), so log order matches retained-store order. The remaining window is the few instructions between `Topics.RetainMessage` and the `OnRetainMessage` call (est. < 1 µs). This is a known limitation (K5). |
| Different sources at one receiver | Arbitrary interleaving |
| `InjectWorkers > 1` (contingency, 21.3) | Per-topic order is kept; cross-topic order is relaxed |

### 15.2 QoS across the link

The link itself is reliable. QoS applies to the publisher→source hop and to the receiver→subscriber hop.

| Situation | End-to-end guarantee |
|---|---|
| Steady state, connection drops | Exactly once (dedup by `(epoch, offset)`; HELLO resume) |
| Consumer process crash | At least once: at most the records applied after the last COMMIT are duplicated. That is at most one batch, or 100 ms of apply time, independent of `Pipeline`. |
| Consumer graceful restart | Exactly once (`COMMIT` before `GOAWAY`) |
| Source graceful stop | Drained to connected consumers up to `drainTarget` (15.6). The rest is logged exactly: `shutdownUnserved` (captured, not committed) and `uncapturedAtShutdown` (reached the hook after capture was switched off). |
| Source crash | At most once: the unpulled backlog is lost |
| Overflow | At most once, counted (8.5) |

QoS 2 is **not** exactly-once across a consumer crash.

### 15.3 Duplicates

Within an epoch the consumer skips `offset < appliedNext` in O(1). After a consumer **crash**, the records applied after the last commit the source saw are replayed. Avoiding this would need persistent consumer state, which the in-memory requirement excludes.

### 15.4 RPO statement (sign-off S3)

Data is lost only in these cases, and each is observable:

1. **Source crash** with an unpulled backlog: `SOURCE_RESET` plus `resetLostLowerBound`, which is a lower bound because records captured after the consumer's last contact cannot be known.
2. **Source graceful stop** with consumers disconnected, or not drained within `DrainOnShutdownMs`: the source logs `shutdownUnserved` per consumer exactly at stop. A publish that reaches the hook after capture was switched off (with the order of 6.2, which stops the listeners and the other internal publishers before the drain, this is limited to late publishes such as a native command reply still in flight) is counted as `uncapturedAtShutdown` and logged with it (15.6).
3. **Overflow** while a consumer lags or is down: counted on both sides (8.5).
4. **Policy drops**: per reason, plus `retainedDiverged`.
5. **Capture gaps** (7.5).
6. **Crash between PUBACK and capture** for non-retained QoS>0 publishes. The engine acks before `OnPublished` (`server.go:1050-1075`). The window is microseconds.
7. **Local subscriber overflow on the receiver** (`OnPublishDropped`). Catch-up pacing (12.6) keeps it out of replays.

With the link up on a LAN, the RPO equals the replication lag (expected in ms; measured, G4). PUBACK means "accepted locally". The WinCC OA topics branch (`winccoa/topics/...`, PUBACK after OA confirmation, `winccoa/README.md:273-322`) remains the durable alternative.

### 15.5 Restart and failure matrix

| # | Event | Source | Consumer | Loss / duplicates | Observable as |
|---|---|---|---|---|---|
| 1 | Network drop, both up | Session closes; `C[c]` kept; log fills | Reconnects with `(epoch, appliedNext)` | None within bounds | `reconnects`, `lag` |
| 2 | Consumer graceful restart | `COMMIT` received; `C[c]` exact | `lastEpoch=0`; snapshot FILL; resumes at `C[c]` | None within bounds, retained included (16.5) | `sessions`, `snapshotFilled` |
| 3 | Consumer crash | `C[c]` = last commit | Snapshot FILL; resumes at `C[c]` | ≤ 1 commit window duplicated | documented |
| 4 | Source graceful restart | Listeners closed first, internal publishers stopped, drain to `drainTarget` (15.6), new epoch | `SOURCE_RESET`; snapshot FILL; resumes at 1 | Only `shutdownUnserved` and `uncapturedAtShutdown` | `sourceResets`, `shutdownUnserved`, `uncapturedAtShutdown` (source log) |
| 5 | Source crash | New epoch; backlog gone | As #4 | Unpulled backlog (RPO) | `sourceResets`, `resetLostLowerBound` |
| 6 | Consumer down longer than the log holds | Evicts the oldest | `lostOnResume`; continues at LSO; snapshot FILL restores absent retained topics only | Counted. Stale present retained values remain (`retainedDiverged`); operator resync (16.5). | `gapLostTotal` / `lostTotal` |
| 7 | Both crash | — | — | Everything in flight; not countable | `sourceResets` |
| 8 | Configured consumer never connects | Log stays full; FIFO eviction | — | Counted when it connects | WARN after 300 s |
| 9 | Wall-clock step on the source | Monotonic times unaffected; publish timestamps jump | Skew gauge jumps | None | `clockSkewMs` |
| 10 | NodeId collision (copied config) | `self_connection`, or `duplicate_node` after flapping | Same | Link refused for the second instance, loudly | ERROR logs |
| 11 | Network partition | Both sides buffer up to their bounds | Catch up on heal, paced | Per #6 if exceeded | `lag`, `lost` |
| 12 | Client fails over A→B before A detects the dead connection | A fires the will after keepalive expiry | Will dropped (`will_superseded`) | None; B's state stays "online" | `dropped{will_superseded}` |
| 13 | Config change | Static; read at startup. A removed peer stops pinning after restart. Rolling changes per 17.5. | — | — | — |

### 15.6 Graceful source shutdown (drain)

The sequence is described in 6.2.

1. After `CloseListeners`, no network client can publish. The next step stops the internal publishers (bridges, WinCC UA/OA bridges, RTSP cameras, scripts, host monitoring, HMI sync, Redfish, and the GraphQL/HTTP server with the REST and MCP publish APIs). Everything they published before they stopped has been captured (subject to the filters of 7.2). The only internal publisher left is the native service, which stops after the drain; it can still publish a command reply that was in flight.
2. `Drain` records `drainTarget = leo` when it starts. This is a fixed target, not a moving one. It waits until every **connected** consumer has committed `drainTarget`, or until `Log.DrainOnShutdownMs` expires (default 2000; recommendation for WinCC OA hosts: 5000; 0 disables). Records captured after `drainTarget` are still served while the sessions are open, but the drain does not wait for them.
3. Capture is then switched off: `Log.Seal()` sets `sealed` under `mu`, then `active=false`. A publish that reaches the hook after that is not captured and counts as `uncapturedAtShutdown`; an `Append` that passed the `active` test just before the switch finds `sealed` under `mu` and is counted the same way (8.2). From the seal on, `leo` is final.
4. The source logs per consumer `shutdownUnserved = leo - C[c]` (the final `leo`, so records captured after `drainTarget` are included) as WARN, or INFO when 0, together with `uncapturedAtShutdown`. Disconnected consumers are not waited for. Together the two counts are exactly what the source captured but could not serve, plus what it no longer captured.

This makes planned WinCC OA switchovers and updates lossless for everything captured before `drainTarget`, whenever the peer is connected and commits within `DrainOnShutdownMs`. With this order that is every publish of a network client before `CloseListeners` and every internal publish before `Close()` step 4 of 6.2 completed. Anything later is counted, never lost silently.

---

## 16. Retained messages

### 16.1 Set and delete

A retained record with a payload sets the value; a retained record with an empty payload deletes it, in memory and in DB mode (`topics.go:463-486`; `hook_storage.go:316-319`). Deletes are ordinary records and need no extra mechanism.

### 16.2 Expiry while held

The receiver stores a still-valid retained record with its remaining interval and a backdated `Created` (12.3), so it expires on both nodes within about ±1 s. When the source's copy expires, `clearExpiredRetainedMessages` runs without a publish and without capture (`server.go:1879-1894`). That is correct, because the receiver's copy expires on the same schedule. In DB mode, `PurgeExpired` covers rows with an expiry.

### 16.3 Retained record that expired before it was pulled (v1: drop)

The record is dropped and counted in `retainedDiverged{expired}`.

- **Consequence.** The receiver keeps its previous value until that value's own expiry (K4).
- **Rejected alternatives.** Delivering with 1 s violates MQTT 5. An unconditional clear can wipe a newer local value.
- **M6 option.** A silent conditional clear through E4: delete the receiver's value without delivery only if it is older than the expired record's capture instant.

### 16.4 Active-active conflicts

A partition, or a device failing over from A to B while A's backlog is still being replayed, can make the nodes **swap** retained values.

- **v1 (`RetainedConflict: ARRIVAL`).** Documented, and visible through the lag and gap counters. Will supersession (12.2 step 7) removes the most common failover case.
- **M6 option (`RetainedConflict: NEWEST`).** A per-topic map `topic → (publishWallNs, nodeId)` fed by an `OnRetainMessage` tap. An older incoming retained record is injected with `Retain=false`; ties are broken by NodeId. Costs: est. 50-100 ns per retained publish, and dependence on NTP.

### 16.5 Retained snapshot and resync (R4)

**Problem.** A restarted consumer resumes at `C[c]`, and records it applied before the restart are never sent again. A receiver with `RetainedStoreType: MEMORY`, which is the setting in `config.yaml.example:26` and `scripts/deb/config.yaml:24`, starts with an empty retained map (`server.go:316`). Retained values that a source loaded from its persistent store at startup are not in its log at all.

**Automatic snapshot (FILL), v1.**

- **When.** On `HELLO_OK` with `SNAPSHOT_AVAILABLE`, that is, after a consumer restart or a source reset, if both sides agree `SNAPSHOT_FILL` and `Snapshot.Mode: FILL` (default).
- **Source side.**
  1. The consumer sends `FETCH(flags=SNAPSHOT)`.
  2. The source materialises the **topic list** of its current retained set through `RetainedAccess.Snapshot`: in-memory `Topics.Retained`, or `store.Retained.FindMatchingMessages("#")` in DB modes.
  3. It filters the list exactly like capture: namespace, `$`, Include and the resolved Exclude (`GetExclude`, by default the HMI sync tree, 18.3).
  4. It streams the current values as `BATCH(SNAPSHOT)` records (record flag `snapshot`; `captureMonoMs` = source mono now minus the value's age; remaining expiry), one batch per FETCH, ending with `SNAPSHOT_END`.
  5. `Snapshot.MaxTopics` (default 1,000,000) bounds the set. Anything beyond it is counted as `snapshotTruncated` and logged as a WARN.
- **Consumer side.**
  1. At snapshot start, the consumer takes the set of topics that currently have a retained value (`RetainedAccess.Has`: an in-memory map lookup; DB modes preload a topic set once per snapshot).
  2. It injects a snapshot record **only if the topic is absent**. It injects through the normal path, so live subscribers on the receiver get the value with retain=false, and the retained store keeps it. `Forward.Snapshot` keeps it off bus, archives and queues.
  3. Present topics are skipped (`snapshotSkippedPresent`).
  4. Snapshot records carry no offsets and are not committed. The log tail starts after `SNAPSHOT_END`, at `resumeAt`, so newer log records overwrite snapshot values in order.
- **Properties.**
  - There is no echo: injected snapshot values are replicas and never captured.
  - Values that originated on the consumer itself and were lost with its MEMORY store come back. That is the intended recovery.
  - FILL can resurrect a value that the consumer deleted while the source was not reachable. This needs prior divergence and is documented.
  - In a mesh, the snapshot can contain retained values that the source itself received from other peers, because its retained store does not distinguish replicas. This is harmless: FILL only fills absent topics, and in a full mesh those values equal what the consumer receives from the originating peer. Documented.
- **Cost.** One transfer of the retained set per consumer restart (est. 20 MB for 100k topics × 200 B). `Snapshot.Mode: OFF` disables it.

**Operator resync (NEWER), v1, M5.**

- Endpoint: `POST /peerlink/v1/resync?source=<nodeId>` on the consumer's loopback.
- It requests a snapshot in `NEWER` mode. The consumer overwrites a present value only if the source value's `Created` is newer than the local one by more than 1 s, after correcting for `clockSkewMs`.
- The application is logged with counts.
- Limitation: a snapshot has no tombstones, so it cannot remove values that the source deleted. The documented procedure for divergence after an outage longer than the log is: run the resync, then republish or clear the remaining topics with existing tools.

---

## 17. Security

### 17.1 TLS (optional, R14)

- **One identity per node.** Each node is both listener and dialer, so one certificate with `ExtKeyUsage = ServerAuth + ClientAuth` serves both directions. Paths may contain `{NodeId}`, so one config file fits both hosts.
- **Listener:** `tlsutil.ServerConfig` with a PEM key pair and a PEM or legacy-PKCS12 truststore (parsing ported from `internal/broker/tls.go:135-204`). MinVersion TLS 1.2, or 1.3 when a `SharedSecret` exists. ALPN `mmq-peer/1` and `http/1.1`.
- **Dialer:** `tlsutil.ClientConfig` with `RootCAs` from the truststore, an optional client certificate, optional pins and an optional `ServerName` (SNI only). Nothing in the repo provides this today: the only outbound TLS is `&tls.Config{MinVersion: tls.VersionTLS12}` (`internal/bridge/mqttclient/connector.go:181`).
- **Never system roots.** When no truststore is configured, `tlsutil` uses an explicit **empty** `x509.CertPool`, so chain verification fails unless a pin or a shared secret applies. This differs from `LoadTLS`, whose nil `ClientCAs` falls back to the OS roots (`internal/broker/tls.go:174`; `/opt/go/src/crypto/tls/handshake_server.go:970-976`).
- **`AutoGenerate: true`** calls `tlsutil.EnsurePeerCertificate(NodeId)` when the files are missing. It creates a self-signed ECDSA P-256 certificate with:
  - CN = NodeId;
  - URI SAN `urn:monstermq:node:<NodeId>`;
  - both EKUs;
  - 10 years of validity.

  The SPKI SHA-256 is logged at startup. A dialer refuses to connect to a peer that has neither a pin nor a truststore nor a secret (17.4). The existing `EnsureCertificate` is unsuitable: CN=hostname and ServerAuth only (`tls.go:87-101`).
- **Formats:** PEM key pair only, with an explicit `KeyPath`; no `cert:key` split (`tls.go:141-146` breaks Windows paths). JKS, OpenSSL 3 default PKCS12 and encrypted keys are not supported; each would need a new module and sign-off. `broker.LoadTLS` and TCPS stay unchanged.
- **Certificate reload** without restart (`GetCertificate`/`GetClientCertificate` with file watching) is M6. Rotation in v1 uses pin and secret lists (17.5).

### 17.2 Certificate identity bound to the NodeId (optional, R15)

- **All checks run in `tls.Config.VerifyConnection`.** It runs on every handshake, including resumptions (`/opt/go/src/crypto/tls/common.go:670-692`). The dialer sets `InsecureSkipVerify = true`, which only turns off hostname verification. `VerifyConnection` must never be nil, and a unit test asserts it.
- **Steps inside `VerifyConnection`:**
  1. **Chain check.** `x509.Verify` against the configured pool (possibly empty), with `KeyUsages = [ServerAuth]` on the dialer and `[ClientAuth]` on the listener, and no DNSName. If the peer has `PinnedSha256`, a pin match replaces chain verification. A pin is a SHA-256 over SubjectPublicKeyInfo or over the certificate DER. A self-signed peer certificate in the truststore verifies as a chain of one, and the EKU check still applies.
  2. **Identity.** By default the **only** accepted identity is the URI SAN `urn:monstermq:node:<canonical NodeId>`, or a per-peer `CertificateIdentity` override (an exact URI SAN or DNS SAN value).
     - `Tls.IdentityFallback: NONE | DNS | CN` (default NONE) is an explicit opt-in for PKIs that cannot issue URI SANs.
     - A startup WARN fires when the peer truststore path equals the TCPS truststore path (shared CA). The docs recommend a dedicated peer CA.
  3. **Binding.** Dialer: identity == configured source NodeId. Listener: identity is a configured `Serve` peer, and after HELLO it must equal `consumerNodeId`.
- **Listener `ClientAuth`.**
  - `REQUIRED` maps to `RequireAndVerifyClientCert`, or to `RequireAnyClientCert` when pins are used.
  - `REQUEST` maps to `VerifyClientCertIfGiven`.
  - A per-peer `RequireClientCert` is enforced after HELLO. Combining it with `ClientAuth: NONE` is a validation error.
- **Separate from MQTT user auth.** Peer authentication does not reuse `AuthHook`/`UseIdentityAsUsername`, which reads only the CN and may auto-create users (`hook_auth.go:58-150`).

### 17.3 Authentication modes and the fail-closed rule (S8)

A peer is **authenticated** for a direction when at least one of these holds:

| Mode | Serve peer (it pulls from us) | Pull peer (we pull from it) |
|---|---|---|
| mTLS | Its client certificate passes 17.2 with identity == its NodeId | Its server certificate passes 17.2 (truststore or pin) with identity == its NodeId |
| Shared secret over TLS ("PSK over TLS") | Valid exporter-bound `mac` (9.5); TLS server certificate may be unverified | Valid exporter-bound `macS`; TLS server certificate may be unverified |

Rules:

- **A `SharedSecret` requires TLS.** Without TLS the exporter is empty, and an on-path relay could forward the handshake and then rewrite or drop records (CRC32C is not keyed). TLS with an unverified certificate plus the exporter-bound MAC gives a mutually authenticated, encrypted channel. MinVersion is TLS 1.3, because `ExportKeyingMaterial` needs TLS 1.3 or TLS 1.2 with Extended Master Secret.
- **Fail closed.** If any configured peer is not authenticated in some direction it is used in, startup fails. The only exception is `AllowUnauthenticatedPeers: true`, which is accepted only if:
  - `Listener.AllowedNetworks` is non-empty; and
  - `UserManagement.Enabled` is false. When users and ACLs are enforced, an unauthenticated peer could bypass them through inline injection.

  It then logs a WARN on every start.
- **Group versus pairwise secrets.** A node-wide `SharedSecrets` is a group secret: any holder can claim any NodeId in the group. With more than two nodes, a WARN recommends per-peer `SharedSecrets`.

| Configuration | Protects | Recommendation |
|---|---|---|
| Plain TCP + `AllowUnauthenticatedPeers` + `AllowedNetworks` | Network position only | Lab or dedicated redundancy LAN without UserManagement |
| TLS with no truststore, no pin, no secret | — | Not allowed (needs per-peer `InsecureSkipVerify: true`, which counts as unauthenticated) |
| TLS + shared secret (per peer) | Encryption, mutual authentication, channel binding | Simple production setup for a pair |
| mTLS (URI SAN, dedicated CA or pins) | Encryption, mutual NodeId-bound identity | Recommended for production and meshes |

### 17.4 Threats

| Threat | Impact | Mitigation |
|---|---|---|
| Rogue consumer claims a configured NodeId | Reads data. Advances `C[c]`, so records are trimmed before the real consumer reads them. | Fail-closed authentication (17.3) |
| Rogue source, or MITM | Injects arbitrary non-namespace topics, **bypassing ACLs**; drops records while forwarding commits | Dialer verifies identity or pin, or `macS` over TLS with exporter binding. Plaintext only via the explicit waiver. |
| Forged WinCC OA namespace or `$` topics | Forged native values or status | The receiver drops these regardless of auth state, using both roots (12.2) |
| Shared-CA impersonation (machine or MQTT client certificate whose CN equals a NodeId) | Peer impersonation | URI SAN-only identity by default; WARN on a shared truststore |
| Misspelled security key (non-strict YAML, `internal/config/load.go:19`) | Silently unauthenticated | Strict decoding of the `PeerLink` subtree (18.4); fail closed |
| DoS on the peer port | Goroutines, memory | Admission before TLS (9.2); per-IP and global pre-auth limits; 4 KiB pre-auth frame cap; handshake deadlines; no state before auth; one session per NodeId |
| NodeId enumeration | Learns the peer list | Generic `auth_failed` before authentication; minimal `SERVER_HELLO` |
| Slowloris on TLS | Goroutines | `HandshakeContext` deadline (unlike `listeners/tcp.go:64`) |

### 17.5 Rotation and migration without losing the link

- **Per-peer `Tls.Enabled`** controls the dialer, so the two directions can be switched independently.
- **`Listener.AllowPlaintext: true`** (default false) lets a TLS-enabled listener also accept the plaintext preamble during migration. Plaintext sessions must still satisfy 17.3, i.e. they need `AllowUnauthenticatedPeers`.
- **Lossless order for enabling TLS in a pair:**
  1. Node A, then node B: enable listener TLS with `AllowPlaintext: true`. One restart at a time; each restart drains its connected consumer.
  2. Node A, then node B: set the per-peer dialer `Tls.Enabled: true`.
  3. Node A, then node B: set `AllowPlaintext: false`.

  At no step are both directions down at once.
- **Secret rotation:** `SharedSecrets` is a list. Add the new secret on both nodes, move it to first position on both, then remove the old one. **Pin rotation:** `PinnedSha256` is a list, rotated the same way.

---

## 18. Configuration

### 18.1 YAML (one file can be shared by both hosts of a pair)

```yaml
NodeId: ""                       # must differ per host; empty = hostname (canonicalised, 18.4).
                                 # With PeerLink enabled, the 'edge' fallback (no hostname) is rejected.
Runtime:
  MemoryLimitMB: 0               # 0 = Go default; otherwise debug.SetMemoryLimit (8.7)
PeerLink:
  Enabled: false
  AllowUnauthenticatedPeers: false   # only with AllowedNetworks and UserManagement disabled (17.3)
  Listener:                      # bound when any peer has Serve: true
    Address: 0.0.0.0
    Port: 1890
    AllowedNetworks: []          # CIDR allow-list, checked before TLS, e.g. ["10.10.0.0/24"]
    MaxPreAuthPerIp: 2
    AllowPlaintext: false        # migration only (17.5)
  Tls:                           # this node's identity and trust
    Enabled: false               # listener TLS and the default for dialers
    CertPath: certs/peer-{NodeId}.pem
    KeyPath:  certs/peer-{NodeId}.key
    TrustStorePath: certs/peer-ca.pem
    TrustStoreType: PEM          # PEM | PKCS12 (legacy ciphers only)
    TrustStorePassword: ""
    ClientAuth: NONE             # NONE | REQUEST | REQUIRED
    IdentityFallback: NONE       # NONE | DNS | CN
    AutoGenerate: false
  SharedSecrets: []              # group secrets, first = current; requires TLS
  KeepAliveSeconds: 10
  Log:
    MaxMessages: 2000000
    MaxBytes: 268435456          # 256 MiB, provisional (8.7)
    MaxRecordBytes: 0            # 0 = MaxMessageSize + 64 KiB (1 MiB + 64 KiB if MaxMessageSize is 0)
    DrainOnShutdownMs: 2000
    NeverConnectedWarnSec: 300
  Capture:
    Wills: true                  # never during own shutdown; superseded wills dropped on the receiver
    Include: ["#"]               # always excluded too: <TopicRoot>/#, $...
    Exclude: null                # null = [<HMI.SyncBaseTopic>/#]; [] forwards the HMI sync channel too
    EchoSuppressMs: 0
  Snapshot:
    Mode: FILL                   # FILL | OFF
    MaxTopics: 1000000
  Fetch:
    MaxRecords: 4096
    MaxBytes: 1048576
    MaxWaitMs: 1000
    LingerMs: 0
    Pipeline: 1                  # 1 | 2
    CrcOnTls: false
    ReconnectMaxMs: 30000
  Receive:
    Bus: true                    # scripts, REST SSE, GraphQL subscriptions, Redfish
    BridgeOutbound: false        # MQTT bridge outbound forwards replicas (loop risk, 14.6)
    Archive: true                # set false if both nodes archive into one shared database
    Queue: false                 # offline persistent sessions get replicas
    SharedSubscriptions: SKIP    # SKIP | DELIVER
    MarkReplicas: false          # add user property mmq-peer-src=<nodeId>
    CatchUpRateFactor: 3
    MaxApplyRate: 0
    MaxRecordAgeMs: 0
    MaxFrameBytes: 16842752      # 16 MiB + 64 KiB
    InjectWorkers: 1             # contingency (21.3)
  Peers:                         # the entry whose canonical NodeId equals the own one is ignored
    - NodeId: oa-host-a
      Address: oa-host-a.plant.local:1890   # present => we pull from it
      Serve: true                            # it may pull from us (default true)
      SharedSecrets: []                      # per-peer secrets (recommended for > 2 nodes)
      Tls:
        Enabled: null                        # dialer override; null = PeerLink.Tls.Enabled
        PinnedSha256: []                     # hex SPKI or certificate SHA-256; replaces chain verification
        CertificateIdentity: ""
        ServerName: ""                       # SNI only
        RequireClientCert: false
        InsecureSkipVerify: false            # counts as unauthenticated for the dialer direction
      Receive:
        Include: ["#"]
        Exclude: []
    - NodeId: oa-host-b
      Address: oa-host-b.plant.local:1890
```

**Minimal production pair** (TLS with a per-peer secret; each host lists the other):

```yaml
NodeId: oa-host-a
PeerLink:
  Enabled: true
  Tls: { Enabled: true, AutoGenerate: true, CertPath: certs/peer.pem, KeyPath: certs/peer.key }
  Peers:
    - { NodeId: oa-host-b, Address: "192.168.10.12:1890", SharedSecrets: ["<32+ random bytes, base64>"] }
```

### 18.2 "Configured on both parties" (R8)

A link works only if both sides agree:

- The consumer lists the source with an `Address` (Pull).
- The source lists the consumer with `Serve: true`.
- Authentication material matches on both sides.

A one-sided configuration is refused at the handshake, or the consumer simply never dials. The never-connected WARN flags the source side.

### 18.3 Go types (`internal/config/config.go`)

The section is a top-level `PeerLink PeerLinkConfig` with its own `Enabled`, like `HostMonitoring`. `enabledFeatures` is unchanged (`internal/graphql/resolvers/resolver.go:99-129`).

- Optional default-true booleans use `*bool` with accessors; optional ints use `*int` with `Get…()` defaults (`config.go:691-717`).
- `Capture.Exclude` is `*[]string` (nil = default `[<HMI.SyncBaseTopic>/#]`, empty = none), read through `GetExclude(hmiBase)`. A plain `[]string` cannot keep the difference: `go.yaml.in/yaml/v3` writes a nil slice as `[]` and reads it back as empty, whereas a nil `*[]string` is written as `null` and stays nil.
- Sub-structs: `PeerLinkListener`, `PeerLinkTLS`, `PeerLinkLog`, `PeerLinkCapture`, `PeerLinkSnapshot`, `PeerLinkFetch`, `PeerLinkReceive`, and `PeerConfig{NodeID, Address, Serve *bool, SharedSecrets, Tls PeerTLS, Receive PeerReceive}`.
- `Runtime{MemoryLimitMB}` is a new small top-level section.

### 18.4 Loading and validation

- **Strict decoding.** `load.go` keeps the non-strict `yaml.Unmarshal` for the whole file (`load.go:19`). It then re-decodes the `PeerLink` subtree with a `yaml.v3` Decoder (`go.yaml.in/yaml/v3`, `go.mod:22`) with `KnownFields(true)`. An unknown or misspelled key under `PeerLink` fails startup, even when `Enabled` is false.
- **`PeerLinkConfig.validate(nodeID, nodeIDFromFallback, hostname, userMgmtEnabled, retainedStore)`**, called from `Config.Validate`:
  1. If `Enabled` is false, only key names and types are checked, so the example block stays valid.
  2. `Config.Validate` records whether `NodeId` came from the `edge` fallback **before** filling it (`config.go:510-516`). With PeerLink enabled, that fallback is rejected.
  3. **Canonical NodeIds.** The own NodeId and every peer NodeId are lower-cased and must match `^[a-z0-9._-]{1,64}$`. Two peer entries that are equal after lower-casing are an error. The canonical form is used for config lookup, HELLO fields, URI SANs and injector client ids.
  4. **Own-entry detection.** An entry matches the own node when its canonical NodeId equals the own one. When `NodeId` came from the hostname, an entry also matches when it equals the hostname's first label. The match is dropped (INFO). If the list has two or more entries and none matches, a WARN names the resolved hostname ("if this file is shared, no entry matches this host"). It is not an error, because per-host mesh files list only the other nodes.
  5. **Peers:** at least one remains. Each entry has an `Address` or `Serve: true`. An `Address` parses with `net.SplitHostPort`.
  6. **Listener:** if any peer has `Serve`, `Listener.Port` must be 1..65535 (0 → 1890). `AllowedNetworks` entries parse as CIDR. No MQTT listener may have the id `peerlink`.
  7. **Authentication (17.3):**
     - every peer is authenticated in each direction it is used, or `AllowUnauthenticatedPeers` holds with its conditions;
     - `SharedSecrets` without TLS (node-level or per-peer dialer) is an error;
     - each secret is ≥ 16 bytes after base64 decoding;
     - `ClientAuth != NONE` requires `TrustStorePath`, or pins on every `Serve` peer;
     - `RequireClientCert: true` with `ClientAuth: NONE` is an error;
     - pins without TLS are an error;
     - `Tls.Enabled` requires `CertPath` + `KeyPath`, or `AutoGenerate`.
  8. **Bounds:**
     - `MaxMessages ≥ max(100, Fetch.MaxRecords)`;
     - `MaxBytes ≥ 1 MiB` and `≥ 4 × MaxRecordBytes`;
     - `1 ≤ Pipeline ≤ 2`;
     - `Fetch.MaxWaitMs < KeepAliveSeconds*1000`;
     - `CatchUpRateFactor` is 0 or ≥ 1.5;
     - `1 ≤ InjectWorkers ≤ 16`;
     - every Include/Exclude entry is a valid filter.
  9. **Startup WARNs (not errors):**
     - `AllowUnauthenticatedPeers` in use;
     - a group secret with more than two nodes;
     - the peer truststore equals the TCPS truststore;
     - `RetainedStoreType: WINCCOA` (Q9);
     - `RetainedStoreType: MEMORY` with `Snapshot.Mode: OFF` ("peer retained state is lost on restart");
     - `2.2 × MaxBytes + 150 MiB > Runtime.MemoryLimitMB`.

     Device WARNs (devices assigned to both nodes, Redfish gateways, a bridge targeting a peer host) and the `HostMonitoring.BaseTopic` WARN are not part of `validate`: `Config.Validate` cannot see device configs, which live in the DeviceConfigStore. They run in `build()` after the stores are opened (6.2 `build()` step 5).

### 18.5 Files

| File | Change |
|---|---|
| `internal/config/config.go`, `load.go`, `config_test.go` | Section, defaults, strict subtree decode, validation; round-trip test (YAML → struct → YAML → struct). The round trip must keep `Capture.Exclude` nil vs empty: the test covers `Exclude: null`, an omitted key and `Exclude: []`, and compares through `GetExclude` (18.3). |
| `yaml-json-schema.json` | `PeerLink` and `Runtime` objects, `additionalProperties:false` on each sub-object, enums; `Capture.Exclude` has type `["array","null"]`. Fix the documented `NodeId` default (`:9-13` says `edge`; the real default is the hostname). |
| `config.yaml.example` | **Uncommented** `PeerLink:` block containing every key, with `Enabled: false`, so the schema test exercises it (`test/integration/config_schema_test.go:33-48`) |
| `winccoa/monstermq.yaml.example` | Uncommented `PeerLink: { Enabled: false, ... }` pair example. A comment at `:4` says the hard-coded `NodeId: edge-oa-1` must differ per host, and that changing a NodeId also changes which devices run (`internal/bridge/mqttclient/manager.go:93`). |
| `test/integration/testdata/peerlink-full.yaml` | Fully populated with `Enabled: true` (TLS, pins, secrets, per-peer overrides). Schema-validated and passed through `Validate` (no file existence checks at validation time). |
| `scripts/deb/config.yaml` | Unchanged (disabled by default; `AGENTS.md:136-138`) |

---

## 19. Guidance for WinCC OA redundant pairs

- **Independent of the OA role.** Both brokers accept clients and forward, active-active. Nothing switches on an OA switchover. Role reporting stays in `plan-winccoa-node-redundancy-status.md`.
- **NodeId must differ per host.** Changing a NodeId changes device assignment (`internal/bridge/mqttclient/manager.go:93`). Move device and archive-group assignments to the new NodeId in the same maintenance step.
- **The namespace is never forwarded.** WinCC OA mirrors `<TopicRoot>` itself, and each node's status topic stays node-local. This resolves `plan-winccoa-node-redundancy-status.md:160-165` and matches `plan-winccoa-broker-cns-mqtt-namespace.md:140`.

| | Topics branch (`winccoa/topics/...`) | PeerLink (all other topics) |
|---|---|---|
| Carried by | WinCC OA (MMQTopic datapoints) | Broker to broker, in memory |
| Survives restart of both hosts | Yes | No |
| PUBACK | After OA confirmation | Local only |
| Throughput / latency | OA-bound | Broker-bound (measured, G3/G4) |

- **Stores.**
  - **`RetainedStoreType: WINCCOA`** writes every replicated retained value as a confirmed `dpSet` to the MMQRetained datapoint that OA already mirrors. Writes on the passive host may be discarded (`plan-winccoa-node-redundancy-status.md:71-75`), and the oastore cache is loaded only at startup (`retained.go:40-42`). Q9 is an M0 exit criterion. Until it passes, the recommendation is `RetainedStoreType: MEMORY` (with snapshot FILL) or `SQLITE` together with PeerLink.
  - **`SessionStoreType: WINCCOA`** mirrors MMQSessions across hosts. With PeerLink enabled, QueueHook hydrates only own-node sessions (13.4).
  - **`UserStoreType` and `ConfigStoreType: WINCCOA`** are unaffected as stores; a shared config store matters only for device assignment (next bullet).
- **Devices.** Assign devices that publish into the broker (inbound MQTT bridges, WinCC UA/OA bridges, RTSP cameras, scripts) to one node's NodeId. Outbound-only MQTT bridges run on every node (`*`) with `BridgeOutbound: false`. A bridge with both directions on one node needs `BridgeOutbound: true` and a remote that is not a peer (14.6).
  - Why: the output of a device that publishes into the broker is forwarded to the peer like any publish (7.4), so a device running on both nodes (with `local` or `*` under a shared config store: `ConfigStoreType` WINCCOA, or a shared POSTGRES or MONGODB database) delivers everything twice. With `BridgeOutbound: false`, an outbound bridge on A never forwards B's publishes, so each node needs its own instance.
  - `Receive.BridgeOutbound` applies to every bridge of the node. A node with `BridgeOutbound: true` therefore must not also run `*` outbound-only bridges, or the peer's publishes reach the remote twice.
  - Redfish ignores NodeId: enable it on one node only (7.4). Host monitoring may run on both nodes as long as its `BaseTopic` contains `{NodeId}`.
  - If a device's node fails, its output stops until the device is reassigned. The startup WARNs are listed in 6.2 `build()` step 5.
- **Clients.** Keep a persistent session on one node where possible. Shared subscription groups should have members on every node (`SharedSubscriptions: SKIP`). Use MQTT 5, so clients see reason 0x8B on a planned stop and fail over immediately.
- **Bridges and archives.** The MQTT bridge outbound does not forward replicas by default. Assign an archive group that writes to a **shared** database to one node only, or set `Receive.Archive: false`.

---

## 20. Observability (no SDL change)

### 20.1 Counters and gauges (`Manager.Status()`)

- **Log (source):**
  - `epoch`, `lso`, `leo`, `records`, `bytes`, `capacitySeconds`;
  - `appended{client,inline,will}`, `trimmed`, `evictedUnread`, `evictedBy{count,bytes}`;
  - `captureDropped{size,invalid}`, `skipPeer`, `skipWill`, `filtered`, `echoSuppressed`;
  - `spareMisses`, `uncapturedAtShutdown`.
- **Per consumer (on the source):**
  - `state`, `remote`, `committed`, `served`, `lag = leo - committed`, `lostTotal`;
  - `servedRecords`, `servedBytes`, `servedSkipped{size}`, `sessions`;
  - `duplicateConsumer`, `authFailures{reason}`, `lastFetch`, `shutdownUnserved`.
- **Per source (on the consumer):**
  - `state`, `epoch`, `appliedNext`, `sourceLeo`, `batches`;
  - `injected`, `retainOnly`, `appliedBytes`, `dupSkipped`;
  - `dropped{malformed|size_source|namespace|filtered|size|expired|stale|will_superseded}`, `retainedDiverged{reason}`, `rejected`, `unknownProps`;
  - `gapLostTotal`, `sourceResets`, `resetLostLowerBound`, `reconnects`, `crcErrors`;
  - `snapshotFilled`, `snapshotSkippedPresent`, `snapshotTruncated`;
  - `paced`, `lastError`, `clockSkewMs`, `topicRootMismatch`, `retainedClassMismatch`;
  - `applyDelayMs p50/p99/p99.9`.
- **Measurement details.**
  - Latency uses fixed allocation-free buckets.
  - `applyDelayMs = ageNow` at injection, which is skew-free.
  - `clockSkewMs = sourceWallMs - (tSend + tRecv)/2` per batch, smoothed with an EWMA; the error is ≤ RTT/2.

### 20.2 Surfaces

1. **slog** (reaches the dashboard log viewer via the log bus):
   - INFO: connect, disconnect, state changes.
   - WARN, rate-limited per peer to 1 per 10 s with aggregate counts: gaps, source resets, never connected, skew, retained divergence, `shutdownUnserved > 0`, `uncapturedAtShutdown > 0`.
   - ERROR: identity mismatch, self connection, wrong node, duplicate NodeId, poison batch.
2. **Status endpoint** `GET /peerlink/v1/status`: plaintext from loopback, or over TLS to mTLS-authenticated peers only. There is no HMAC-signed remote status in v1. It returns JSON with `nodeId`, `epoch`, `listen`, `tls`, `log{...}`, `consumers[...]` and `sources[...]`, e.g. `curl -s http://127.0.0.1:1890/peerlink/v1/status`. The integration tests assert counters through it. `POST /peerlink/v1/resync` is loopback only (16.5).
3. **Metrics collector.** `messageBusIn` (records injected) and `messageBusOut` (records served) are added to the persisted `BrokerSnapshot` JSON under the Java keys (`MetricsStoreSQLite.kt:351-352`; meaning per `SessionHandler.kt:58-60`). The `snapshotToBrokerMetrics` mapping (`resolver.go:1832-1842`) follows only after S4.
4. **Native status JSON (M5, covered by S2).** When native mode is on, the retained status on `<TopicRoot>/...` (`internal/winccoanative/service.go:1149-1173`) gets a `peerLink` object:

   ```json
   { "enabled": true,
     "consumers": [{ "nodeId": "...", "state": "...", "lag": 0, "lostTotal": 0 }],
     "sources":   [{ "nodeId": "...", "state": "...", "lagRecords": 0, "gapLostTotal": 0,
                     "sourceResets": 0, "retainedDiverged": 0, "lastError": "" }] }
   ```

   It is updated at most once per 5 s, and on every state change. That topic is never forwarded, so each node reports its own view. This is the supported alarm surface for WinCC OA operators.
5. **No MQTT status topics outside `<TopicRoot>`.** Inline publishes would reach `OnPublished`, the bus and archives, and would be captured and forwarded to every peer (7.4).

---

## 21. Performance budget and gates

### 21.1 Per-record cost model (all est., to be replaced by measurements)

| Stage | arm64 (Pi 4, A72) | armv7 (Pi 4, 32-bit OS) | amd64 | Note |
|---|---|---|---|---|
| Capture (filters, `time.Now`, encode ~200 B, 1 alloc, lock section) | 200-350 ns | 300-600 ns | 80-150 ns | Chunked log |
| Serve (amortised over a 4096-record batch, writev) | ≤ 60 ns | ≤ 100 ns | ≤ 20 ns | No per-record encode |
| CRC32C (plaintext only) | ~20 ns | ~150 ns (table-driven) | ~5 ns | `hash/crc32` has no arm assembly |
| TLS encrypt + decrypt (~220 B) | 1-2 µs | 4-10 µs | 0.1-0.4 µs | ChaCha20-Poly1305 on Pi 4: assembly ChaCha20 on arm64 only, generic Poly1305 on arm |
| Receive decode + validate | 50-100 ns | 100-200 ns | 20-40 ns | Interning |
| **Inject (engine publish, default hooks)** | **2-5 µs** | **4-10 µs** | **0.5-1.5 µs** | Includes StorageHook bus/archive dispatch (Default archive group `#` with in-memory last value, `archive/manager.go:108-118`, `archive/group.go:152-167`) and derived UUID; plus ~0.3-0.5 µs per matching subscriber |
| Retained replica, DB mode | + batched share of one transaction per batch | same | same | SQLite single mutex (`sqlite/db.go:66`); WINCCOA: confirmed dpSet per value, batched |

The ceiling is the receiver's apply path on one goroutine. Section 9.1 no longer claims single-instruction loads on armv7.

### 21.2 Gates

All gates are measured per GOARCH (arm64, armv7, amd64). The load generator runs on a **separate host**. Results are recorded in this plan before acceptance and stored as baselines in `dev/bench/peerlink/`.

| Gate | Measurement | Pass |
|---|---|---|
| G0 | Baseline QoS 0 ingress of the current broker, PeerLink off; `T_restart` p95 of the standalone and the embedded broker | Recorded |
| G1 | `BenchmarkLogAppendParallel` (1/4/16 goroutines, 200 B), `-benchmem`, CPU ns/op | ≤ 1 alloc/op; arm64 ≤ 350 ns, amd64 ≤ 150 ns CPU per op |
| G1b | 16 appenders + 2 readers fetching 4096 with commits, continuous eviction, filling from empty to `MaxBytes` | Append latency p99 ≤ 1 µs, p99.9 ≤ 10 µs, max ≤ 100 µs (arm64) |
| G2 | Ingress with PeerLink on and one consumer pulling, vs G0 | ≥ 90 % (≥ 95 % with no consumer connected) |
| G2b | Publish→local-subscriber latency p99 with PeerLink on (log filling, consumer pulling) | ≤ 1.1 × PeerLink off |
| G3 | **Apply matrix** on the default config (SQLite retained, Default archive group present): retained 0/50/100 %; receiver fan-out 0/1/10 (one `#`); payload 100 B / 1 KiB; TLS on/off | Reference row (retained 50 %, fan-out 1, 200 B, TLS on): sustained apply ≥ 1.5 × reference rate (30,000 msg/s) on arm64 and amd64. Every row reported. armv7: the measured rate is published as its supported rate. Informative stretch: plaintext, fan-out 0, retained 0 ≥ 100k msg/s arm64. |
| G4 | Added latency (remote minus local subscriber), LAN | At 20k msg/s: p50 ≤ 1 ms, p99 ≤ 5 ms. At 80 % of the G3 reference rate: p99 ≤ 50 ms. Idle single message p50 ≤ 0.5 ms. |
| G5 | At the reference rate, kill the consumer for `T_restart`, then restart it, with a reference `#` subscriber on B consuming at 1.2 × reference rate | Link loss 0; lag < 1 batch within 3 × `T_restart`; B's `Info.MessagesDropped` delta 0 and bus overflow 0 |
| G6 | RSS and GC with a full log, **both** source and receiver, payloads 16 B / 200 B / 1.5 KB / 40 KB, with and without `Runtime.MemoryLimitMB` | RSS ≤ 2.2 × `MaxBytes` + baseline (≤ 1.3 × with a limit); GC CPU ≤ 5 %; publisher p99 during GC ≤ 2 × idle |
| G7 | CPU µs per forwarded message (utime+stime delta, both nodes, excluding inject) at 2k/20k/100k msg/s, TLS on/off | arm64: ≤ 15 µs at 2k, ≤ 5 µs at 20k, ≤ 2 µs at 100k. armv7 is reported. Decides the defaults of `LingerMs` and `Pipeline`. |

Regression: once reference hosts exist, the PL-24 matrix runs nightly and fails on > 10 % regression against the stored baseline. Until then it runs per release (Q24).

### 21.3 Contingencies when a gate fails

| Failing gate | Action |
|---|---|
| G3 (apply) | v1 contingency: `Receive.InjectWorkers > 1` (sharding by `fnv1a(topic) % N`, per-topic order kept, a completion ring computes the contiguous commit). Then the remaining StorageHook costs. |
| G1 / G1b / G2 / G6 | Segmented arena log (M6). Same `Log` interface. |
| G3 on high-RTT links, G7 | `Pipeline: 2` and/or `LingerMs` defaults |
| G6 receiver | Review interning generation sizes; per-batch buffer size cap |

---

## 22. Test plan

All black-box tests live in `test/integration/peerlink_*_test.go` and drive real listeners (`AGENTS.md:139-141`).

### 22.1 Harness

- **`startPeerNode(t, nodeID, mods...)`.** Starts an in-process broker with `broker.New` + `Serve` on free MQTT and peer ports, with `t.TempDir()` SQLite (pattern: `test/integration/max_message_size_test.go:22-52`, `retained_expiry_test.go:19-43`). A subprocess variant exists for crash tests.
- **Clients.** paho for QoS/retain basics. The raw MQTT 5 client (`test/integration/rawclient_test.go`) for properties, NoLocal, shared subscriptions and RequestProblemInfo.
- **Test peer.** A scripted source or consumer built on the exported `internal/peerlink/wire` codec. It talks to the broker's **real** listener or dialer and can speak older minor versions.
- **Certificates.** Extend `createTestCertificates` (`test/integration/mtls_test.go:44-165`) with:
  - per-node certificates with a URI SAN and both EKUs;
  - a certificate whose CN equals a NodeId but that has no URI SAN;
  - a certificate without the ClientAuth EKU;
  - a foreign CA.
- **Network faults.** A TCP proxy for drops and relays. tc-netem cases are env-gated (Linux, root).
- **Status.** Counters are read through `GET /peerlink/v1/status`.

### 22.2 Cases

| ID | Scenario | Asserts |
|---|---|---|
| PL-01 | A→B, QoS 0/1/2, retain, empty retained (delete) | Exact payload; QoS = min(pub, sub); retained set and delete visible to a late subscriber on B |
| PL-02 | MQTT 5 fidelity | ContentType, ResponseTopic, CorrelationData, PayloadFormat, **user properties in order with duplicates**, remaining expiry at a v5 subscriber on B (RequestProblemInfo=1) |
| PL-03 | Publisher and time | Bus and archive rows on B carry the A client id and the A capture time in ns. The DB-retained row on B carries the A client id and username, and `Created` equal to the A capture second (±1). |
| PL-04 | Bidirectional, no echo | One publish on A gives exactly one delivery on A and one on B. `A.log.appended{client}` unchanged by B's injections. `skipPeer == injectedViaInjectPacket` on both nodes, including a pull-only node. Stable after 10 s idle. **Variant:** an external client on B republishes every received message on the same topic: with `EchoSuppressMs: 2000` the message count stays bounded; with `MarkReplicas: true` the user property is present. |
| PL-05 | Three nodes: full mesh, chain, ring | Mesh: exactly once per node. Chain A–B–C: C gets nothing from A. Ring: no loop. |
| PL-06 | Namespace and `$` exclusion | `winccoa/...` and `$x/...` on A are not captured, nor is `monstermq/hmi/sync/...` with the default Exclude. The test peer sends root, `$` and announced-root topics, and B drops them with `reason=namespace`. A TopicRoot mismatch sets `topicRootMismatch`. |
| PL-07 | Inline sources | A GraphQL/REST publish, a script publish and an MQTT bridge inbound publish on A are forwarded with client id `inline`. The HMI sync tree is not forwarded by default and is forwarded with `Exclude: []`. Replicas are never re-forwarded. Startup WARNs (6.2 `build()` step 5): a script with NodeId `*` under a shared config store, tested with `ConfigStoreType: WINCCOA` through the simulated WinCC OA host (`internal/oahost/simhost`, set up like `withOAStores` and `startNative` in `test/integration/winccoa_native_store_test.go:25-35`); an enabled Redfish gateway; an MQTT bridge whose `brokerUrl` host is a peer address. An outbound-only bridge with `*` raises no WARN. |
| PL-08 | Consumer restart | Stop B, publish 10k on A, start B: all 10k arrive in order. Crash B (subprocess kill) during apply: duplicates ≤ one commit window. |
| PL-09 | Connection drop | The proxy kills the connection mid-stream: zero loss, zero duplicates |
| PL-10 | Source restart | `SOURCE_RESET`, correct `resetLostLowerBound`, resume at offset 1, snapshot FILL performed |
| PL-11 | Overflow | `MaxMessages=1000`, `Fetch.MaxRecords=500`. B never connected, 2500 published: B gets the newest 1000; `gapLostTotal=1500` on B and `lostTotal=1500` on A. Variant: B connected, then down: B ≥ A count, equal without a crash. |
| PL-12 | Trim with two consumers | LSO pinned by the laggard; after both commit, `lso == leo` and `bytes` returns to the baseline |
| PL-13 | Expiry | 2 s expiry with B paused 3 s: dropped (`expired`). 10 s expiry with a 3 s pause: delivered with 7 s remaining ±1. Retained copy expires on B at the source's time ±1 s. **Variant:** slow retained persistence (WINCCOA-like stub with a 50 ms write delay) still drops records whose `ageNow` has passed. |
| PL-14 | Offline queue and NoLocal | Default: a persistent session offline on B gets no replicas queued. With `Receive.Queue: true` it does. A NoLocal client with the same id on A and B does not receive its own message; other subscribers do. |
| PL-15 | Wills, happy path | A will on A (TCP killed, client not reconnecting) is delivered on B with `Forward.Will`. It does not appear on B's bus, archive or queue. |
| PL-16 | TLS and auth | Covers: <br>- mTLS with URI SAN; <br>- wrong-NodeId certificate rejected in both directions; <br>- missing ClientAuth EKU rejected; <br>- pin match and mismatch; <br>- self-signed certificate in the truststore; <br>- HELLO NodeId ≠ certificate identity; <br>- resumed session still identity-checked; <br>- shared secret over TLS right, wrong and rotated (two-entry list); <br>- channel binding: a TLS-terminating relay with a valid secret is rejected; <br>- CN-only certificate rejected by default and accepted with `IdentityFallback: CN`. |
| PL-17 | Handshake guards | Self-connection, unknown peer, `Serve: false`, crossed addresses (`wrong_node`), `AllowedNetworks` (no TLS handshake from a disallowed IP). Generic `auth_failed` with an empty reason when auth is configured. |
| PL-18 | Duplicate consumer | A crash-restart within 30 s raises no ERROR. Two live processes with one NodeId: detected after flapping, the newer one gets `duplicate_node`, no commit regression. |
| PL-19 | Graceful drain | Publish burst, then `Close()` A: B has everything captured before `drainTarget`, i.e. before the listeners closed and the internal publishers stopped; `shutdownUnserved` 0 for the connected consumer |
| PL-20 | Protocol robustness | The test peer sends: an oversize frame; a bad CRC (3× at the same offset, then poison skip); commit > LEO; FETCH offset > LEO (`offset_out_of_range`); FETCH offset < LSO (GAP); a batch-structural fault with a valid CRC (link continues, `malformed` counted); unknown record flags (ignored). Counters correct. |
| PL-21 | Status endpoint | Loopback plaintext works; non-loopback plaintext refused; over TLS only for an mTLS peer; resync endpoint loopback only |
| PL-22 | Config | Both examples and the full fixture pass the schema. Validation errors for: duplicate NodeIds (also case-only differences); the `edge` fallback; a missing Address and Serve; certificate auth without trust; `SharedSecrets` without TLS; `RequireClientCert` with `ClientAuth: NONE`; `AllowUnauthenticatedPeers` without `AllowedNetworks` or with UserManagement enabled; an unauthenticated peer without the waiver; a misspelled key under `PeerLink`. |
| PL-23 | Race | `make test-race` with 16 publishers, two consumers, continuous eviction, snapshot during load |
| PL-24 | Load (env-gated) | G0-G7 matrix, generator on a separate host |
| PL-25 | Soak (env-gated, AC-35 style, `plan-winccoa-broker-embedded-manager.md:337`) | 1 h at the reference rate in both directions, restarting each node alternately every 5 min. Every loss appears in a counter, RSS is bounded on both sides, no goroutine leak. |
| PL-26 | Poison records | Inline (script) topic with invalid UTF-8 or a NUL: not captured (`captureDropped{invalid}`), link healthy. Asymmetric `MaxMessageSize` (A 8 MiB, B 1 MiB) with a 2 MiB record: tombstone, `servedSkipped` on A, `dropped{size_source}` on B, following records delivered. |
| PL-27 | Rolling upgrade N-1 | The test peer speaks v1.0 against a v1.1 broker and vice versa: extra header bytes, unknown TLV, appended frame fields, unknown capability bits. No errors; `unknownProps` counted. |
| PL-28 | Shutdown under load | Clients keep publishing QoS 1 during `Close()` of A. v5 clients receive 0x8B. Every message PUBACKed before the listeners closed arrives on B, or is included in `shutdownUnserved`. No will of A's clients reaches B. A script publishing during `Close()`: everything captured before `drainTarget` arrives on B; nothing after `CloseListeners` and the internal-publisher stop is lost silently (counted in `shutdownUnserved` or `uncapturedAtShutdown`). |
| PL-29 | Will after failover | The client loses its path to A (proxy blackhole), reconnects to B and publishes a retained "online" birth. A's keepalive expires and the will fires. B's retained value stays "online" (`dropped{will_superseded}`), and A converges to "online" via B's record. |
| PL-30 | MEMORY retained restart | B (MEMORY) restarts. A late subscriber on B sees A's retained values (snapshot FILL). B's own retained values published after the restart are not overwritten. |
| PL-31 | Bridge loop | An MQTT bridge on B forwards `#` to A's MQTT port: the message count on both is bounded (one extra copy via the bridge, no amplification). With `BridgeOutbound: true` the documented WARN fires and the test asserts amplification is detected by a counter limit (negative test, time-boxed). **Inbound variant:** a bridge on B subscribes to `#` on A's MQTT port. The peer-host WARN fires for the inbound bridge. With an identity mapping and `EchoSuppressMs: 2000` the count stays bounded (`echoSuppressed` > 0); with a topic remapping the loop is unbounded regardless of `Receive.Bus` and `BridgeOutbound`, which the test detects by a counter limit (negative test, time-boxed). |
| PL-32 | Shared subscriptions | Group `$share/g/t` with members on A and B. `SKIP`: each message is processed once in total. `DELIVER`: once per node. |
| PL-33 | Persistent session failover | Client A→B→A with clean=false: no duplicate backlog on return to A. Hydration filter: sessions with another NodeID are not marked offline (unit test with a session store fixture). |
| PL-34 | String retention | Inject 100 batches touching 10k retained topics, force GC: `HeapInuse` growth ≤ 2 × retained bytes |
| PL-35 | Catch-up flood | B down 20 s at the reference rate with a `#` subscriber on B: B's `MessagesDropped` delta 0 and bus overflow 0 (G5) |
| PL-36 | Slow link (env-gated netem: 1 Mbit/s, 50 ms RTT) | Steady progress; no deadline livelock with 1 MiB batches |
| PL-37 | Write path | A counting conn shows ≤ `ceil(records/1024)+1` writes per plaintext batch; the reusable slice is cleared after each write |
| PL-38 | Retained policy drops | A stale retained record (`MaxRecordAgeMs`) is applied silently (retained present, no live delivery). Expired retained: `retainedDiverged{expired}`. |
| PL-39 | NodeId canonicalisation | Config `OA-Host-A` vs hostname `oa-host-a`: own entry dropped, link up. Shared file with an FQDN hostname matches the short entry. A WARN when no entry matches. |
| PL-40 | Operator resync | After an overflow, `POST /peerlink/v1/resync` restores newer values on B; present newer local values are kept |
| PL-41 | TLS migration | Steps of 17.5 executed with one node restart at a time: no interval in which both directions are down; zero loss for connected consumers |
| PL-42 | Admission | 3 parallel pre-auth connections from one IP: the third is refused (`busy`), and the configured peer from another IP still connects |
| PL-43 | Retained capture order | A retained publish whose PUBACK write blocks (client with a tiny receive window) and a concurrent same-topic retained publish: final retained value identical on A and B |

### 22.3 Package-level tests (pure codec and log, no broker mocks)

- **Fuzz:** `FuzzDecodeRecord`, `FuzzFrame`, `FuzzTLV`.
- **Benchmarks:**
  - `BenchmarkLogAppendParallel` and `BenchmarkLogAppendWithReaders` (G1/G1b);
  - `BenchmarkLogRead`;
  - `BenchmarkDecodeBatch -benchmem`;
  - `BenchmarkInjectPublish`. It drives `server.InjectPacket` with the real broker hook set and anchors 21.1. The repo has no publish or inject benchmark today.
- **Unit tests:**
  - `Packet.Copy` copies neither `Forward` nor `Will`;
  - E1 is unchanged for network and plain inline publishes;
  - E4 and E5 semantics;
  - `VerifyConnection` is never nil;
  - the empty-pool rule (no system roots);
  - the loss formula (8.5) table-driven.

---

## 23. Milestones

| M | Scope | Exit criteria |
|---|---|---|
| **M0** Decisions | S2-S8 recorded (spec §1 `:15` and `:19` amended, M6a row added, §7.2 cross-reference; S1 done). Q1-Q28 answered. This plan merged. **Q9 verified on the live project:** are passive-host MMQRetained writes confirmed, and how long do they take? **Verify** that `SessionInfo.NodeID` is populated by every session store. | Owner sign-off recorded; Q9 result written into section 19 |
| **M1** Core, no network | E1, E2, E4, E5, E6 with tests; `wire` codec (header, TLV, tombstone); chunked `Log`; `Hook` (capture paths, striped counters); `BenchmarkInjectPublish`; verify `BrokerMessage.OriginNode` is not persisted by any store; verify `mqtt.Close` after `CloseListeners` | G0 (incl. `T_restart`) and G1/G1b recorded and met; default `MaxBytes` fixed from `T_restart`; fuzz clean |
| **M2** One-way link, plain TCP | Protocol and handshake, server, puller (three goroutines), injector, receiver filters and failure classes, StorageHook/QueueHook/bus-adapter changes, batched retained writer, config + strict decode + schema + examples + fixture, lifecycle wiring incl. shutdown order | PL-01, 02, 03, 06, 07, 12, 14, 22, 26, 33, 34, 37 green |
| **M3** Failure semantics | Epochs and resume, commit and trim, loss accounting, overflow/GAP, keepalive and progress deadlines, backoff, takeover and duplicate detection, drain, wills with supersession, snapshot FILL, catch-up pacing, shared-subscription skip, bidirectional links and meshes, versioning | PL-04, 05, 08-11, 13, 15, 17-20, 23, 27-32, 35, 38, 39, 43 green; G5 |
| **M4** Security | `internal/tlsutil`, TLS listener and dialer, NodeId binding (URI SAN), pins, `EnsurePeerCertificate`, shared secrets over TLS with channel binding, admission, fail-closed validation, migration switches | PL-16, 21, 41, 42 green |
| **M5** Performance, observability, docs | Status endpoint and resync, metrics snapshot fields, native status `peerLink` object, log events, `Runtime.MemoryLimitMB`, load harness and stored baselines, Pi 4 (arm64 and armv7) and x86 runs, soak. README sections: "PeerLink for redundant pairs" in `winccoa/README.md` (incl. rolling upgrade and TLS migration procedures, RPO, device assignment rules of 19) with the "Single node only" line (`winccoa/README.md:342`) rewritten; in `README.md` a PeerLink feature entry, "No clustering" removed from `:21` and "Single-node. No clustering." from `:399`; `AGENTS.md` repository layout lists `internal/peerlink/` and `internal/tlsutil/` (26). | G2-G7 measured, recorded and met (or contingency applied); PL-24, 25, 36, 40 passed |
| **M6** Optional | Arena log, `RetainedConflict: NEWEST`, conditional retained clear, E3, E7, API username in `Forward`, Redfish NodeId assignment (Q28), certificate hot reload, adaptive `Fetch.MaxBytes`, subscriber-fill backpressure, `messageBusIn/Out` resolver mapping (after S4) | Separate owner decisions |

---

## 24. Risks

| ID | Risk | Mitigation |
|---|---|---|
| K1 | Governance: reviewers hold the plan to AC-27..AC-33 | S2 recorded; explicit non-goals (1.3) |
| K2 | RPO misunderstood as zero loss ("redundancy") | S3 statement in docs and README; counters for every loss path; listener-first drain with internal publishers stopped before it; `shutdownUnserved`, `uncapturedAtShutdown` |
| K3 | Hot-path regression: every publisher pays for capture | Gates G1/G1b/G2/G2b; chunked log; prefix fast path for the default filters; arena fallback |
| K4 | Retained divergence (overflow, source crash, expired-before-pull, size drops, active-active) | `retainedDiverged`, snapshot FILL, will supersession, operator resync; NEWEST and conditional clear in M6 |
| K5 | Same-topic retained race between two publishers on one source, inside `retainMessage` (sub-µs window) | Documented; NEWEST only helps if the timestamps differ |
| K6 | NodeId collision or mismatch from copied configs or hostname case/FQDN | Canonical NodeIds, `edge` fallback rejected, own-entry WARN, `duplicate_node`, example comment |
| K7 | Duplicate production: a device that publishes into the broker (inbound bridge, WinCC UA/OA bridge, RTSP camera, script) with NodeId `local`/`*` runs on both nodes under a shared config store, so its output arrives twice; Redfish gateways ignore NodeId, so the NodeId rule does not apply to them; both nodes archive into one shared database | Assign such devices to one node (7.4, 19); exception: outbound-only MQTT bridges run on every node with `BridgeOutbound: false`; Redfish on one node only or its prefix in `Capture.Exclude`; startup WARNs (6.2 `build()` step 5); `Receive.Archive`; documentation |
| K8 | Receiver apply is the throughput ceiling; DB-retained and WINCCOA writes | G3 matrix; batched retained writer; `InjectWorkers` contingency |
| K9 | Memory: capacity depends on rate and record size; RSS up to 2× without a limit | Size-class accounting, `capacitySeconds`, `Runtime.MemoryLimitMB`, G6 on both sides |
| K10 | Pre-existing: in-memory retained messages are force-purged 24 h after `Created`, even without expiry (`server.go:1885-1888`); DB stores do not do this | Backdated `Created` aligns MEMORY↔MEMORY only; handshake WARN on mixed store classes; noted for a separate engine review |
| K11 | Pre-existing MQTT 5 gaps (RequestProblemInfo=0 suppression; DB-retained and queue rows without properties) | Documented (R10 partial) |
| K12 | E1 makes `Origin` the original client id; a different device reusing that id with NoLocal on the peer misses those messages | Accepted (Q4); network client ids equal to `inline` or starting with `peerlink:` are refused in the PeerLink hook's `OnConnect` (12.1) |
| K13 | Clock skew affects archived publish times and the NEWER resync | All mechanics use monotonic deltas; `clockSkewMs` WARN; NTP recommended |
| K14 | New binary codec: parsing bugs | Fuzzing, liveness rule (9.7), strict invariants, test peer, N-1 tests |
| K15 | TLS on Pi 4 costs a significant share of a core, much more on armv7 | Measured in G3/G7 per GOARCH; armv7 supported rate published; CRC off under TLS |
| K16 | Operators waive authentication on a routed network | `AllowUnauthenticatedPeers` needs `AllowedNetworks`, is refused with UserManagement enabled, and logs a WARN on every start |
| K17 | No redundant WinCC OA test pair exists (`plan-winccoa-node-redundancy-status.md:225-226`) | Acceptance rests on two- and three-broker integration tests plus the non-redundant live project |
| K18 | External clients, scripts/bridges that republish replicas, or MQTT bridges subscribed to a peer create loops that split horizon cannot see (14.6) | Documented deployment rule; one-node assignment, peer-host WARN, `EchoSuppressMs` for network and internal publishes, `MarkReplicas` |
| K19 | `Receive.SharedSubscriptions: SKIP` loses messages for groups whose members exist only on other nodes | Deployment rule (members on every node); `DELIVER` option |
| K20 | Snapshot FILL resurrects a value deleted on the consumer while the source was unreachable | Requires prior divergence; documented; `Snapshot.Mode: OFF` |
| K21 | Will supersession suppresses a legitimate will when a different device reuses the same client id on the receiver | Same accepted trade-off as K12 |
| K22 | `$SYS` received counters include replicas | Documented; E7 optional |

---

## 25. Open questions (each with a recommended answer)

| ID | Question | Recommendation |
|---|---|---|
| Q1 | Remove the single-node rule from AGENTS.md? | Done 2026-10-03 |
| Q2 | Accept the in-memory RPO (15.4) replacing plan §7.2 durability, AC-28 and AC-30? | Yes |
| Q3 | Which inline publishes are captured? | Decided 2026-10-03: all of them, like client publishes (7.4); duplicates are avoided by assigning devices to one node |
| Q4 | Preserve the original client id as `Origin` on the receiver (NoLocal per logical client, Kotlin `senderId` parity)? | Yes (E1) |
| Q5 | Replicas feed the receiver's bus and archives by default; MQTT bridge outbound? | Bus and archives yes, with switches; bridge outbound no (`Receive.BridgeOutbound: false`) |
| Q6 | Exclude replicas from `messagesIn` and count them as `messageBusIn`? | Yes. `$SYS` counters keep including them (documented). |
| Q7 | Retained record expired before pull | v1: drop and count in `retainedDiverged`. M6: conditional silent clear. |
| Q8 | Retained conflict policy for active-active | ARRIVAL in v1 (with will supersession); NEWEST opt-in in M6 |
| Q9 | Write replicated retained messages into MMQRetained (`RetainedStoreType: WINCCOA`)? | Yes, always (there is no correct alternative, 12.5), batched per flush. Verify in M0 that passive-host writes are confirmed and fast. Until verified, recommend MEMORY (with snapshot) or SQLITE with PeerLink. |
| Q10 | Topology | One hop, full mesh required; transitive forwarding deferred |
| Q11 | Replicate wills? | Yes (`Capture.Wills: true`), never during the source's own shutdown, and with receiver-side supersession. E3 only on demand. |
| Q12 | Default peer port | 1890 |
| Q13 | Default log limits | 2,000,000 messages / 256 MiB provisional; final `MaxBytes` from the M1 `T_restart` measurement |
| Q14 | Default fetch pipeline depth and linger | 1 and 0 until G3/G7 are measured; then whichever wins (the duplicate window no longer depends on it) |
| Q15 | Fill `BrokerMetrics.messageBusIn/Out` via the resolver? | Yes, after S4 |
| Q16 | Is the HMI sync base topic excluded? | By default (`Capture.Exclude` = `<HMI.SyncBaseTopic>/#`): the sync service on every node would otherwise answer and write the files. Changeable. |
| Q17 | Status endpoint scope | Loopback plaintext, and mTLS peers over TLS; no HMAC remote status in v1 |
| Q18 | Refactor `broker.LoadTLS` onto `tlsutil` now? | No; refactor later |
| Q19 | Support JKS or modern PKCS12 keystores or encrypted keys? | No in v1 (PEM only; a new dependency needs sign-off) |
| Q20 | Does a configured but never-connected consumer pin the log? | Yes (required for R4), with a WARN after 300 s |
| Q21 | Drop stale replicas by age by default? | No (`MaxRecordAgeMs: 0`). Stale retained records are applied silently when enabled. |
| Q22 | Filter the source's announced TopicRoot in addition to the own one? | Yes |
| Q23 | Add a top-level `Runtime.MemoryLimitMB` (affects the whole broker)? | Yes; default 0 (unchanged behaviour) |
| Q24 | Nightly performance regression runs | Yes, once reference Pi 4 and x86 hosts are available; until then per release with stored baselines |
| Q25 | Default for shared subscriptions on the receiver | `SKIP`, with the deployment rule "members on every node" |
| Q26 | Default for offline queues on the receiver, and the own-node session hydration filter | `Receive.Queue: false`; hydration filter on whenever PeerLink is enabled |
| Q27 | Stop the internal publishers (bridges, scripts, host monitoring, HMI sync, Redfish, RTSP, publish APIs) before the drain, which moves their stop ahead of `native.Stop` when PeerLink is enabled? | Yes (decided 2026-10-03, 6.2 `Close()` step 4). Otherwise the drain chases a moving target and their later publishes are lost uncounted. With PeerLink disabled, `Close()` is unchanged. |
| Q28 | Make Redfish gateways honour NodeId like the other devices? | Not in v1: it changes Redfish behaviour outside PeerLink. Enable Redfish on one node, or exclude its prefix; startup WARN (6.2). M6 candidate. |

---

## 26. Change list

- **New:**
  - `internal/peerlink/*` (incl. `wire/`; `hook.go` also refuses the reserved client ids `inline` and `peerlink:*` in `OnConnect`, 12.1), `internal/tlsutil/*`;
  - `test/integration/peerlink_*_test.go`, `test/integration/testdata/peerlink-full.yaml`;
  - `dev/plans/plan-peerlink.md`, `dev/bench/peerlink/` (baselines).
- **Modified:**
  - **Engine:** `internal/mqtt/packets/packets.go` (E2, E6), `internal/mqtt/server.go` (E1, E4, E5, E6).
  - **Broker:**
    - `internal/broker/server.go`: build (incl. the device WARN step, 6.2 `build()` step 5)/Serve/Close (incl. the internal-publisher stop before the drain, 6.2), `RetainedAccess`/`ClientState` adapters.
    - `internal/broker/hook_storage.go`: Forward handling, gates, derived UUID, `IncIn`, batched replica retained writer.
    - `internal/broker/hook_queue.go`: replica skip, own-node hydration.
  - **Bridge:** `internal/bridge/mqttclient/bus_adapter.go` (skip peer-origin).
  - **Stores:** `internal/stores/types.go` (`BrokerMessage.OriginNode`, not persisted).
  - **Metrics:** `internal/metrics` (bus in/out).
  - **Native status:** `internal/winccoanative/service.go` (`peerLink` status object, M5, under S2).
  - **Config:** `internal/config/config.go`, `load.go` (+ tests), `yaml-json-schema.json`, `config.yaml.example`, `winccoa/monstermq.yaml.example`.
  - **Docs and governance:**
    - `AGENTS.md`: single-node rule already removed 2026-10-03; add `internal/peerlink/` and `internal/tlsutil/` to the repository layout (`AGENTS.md:88-119`).
    - `dev/plans/spec-winccoa-native.md`: amend `:15` and `:19`, add the M6a row (S2).
    - `dev/plans/plan-winccoa-broker-embedded-manager.md` (§7.2 cross-reference).
    - `README.md`: remove "No clustering" from `:21` and "Single-node. No clustering." from `:399`; add a PeerLink feature entry.
    - `winccoa/README.md`: rewrite the "Single node only" line (`:342`) likewise; add "PeerLink for redundant pairs" (M5).
  - **Tests:** `test/integration/mtls_test.go` (certificate helpers).
- **Untouched:**
  - GraphQL SDL (and resolvers unless S4), storage DDL, `embed/cabi`, the C++ manager, `scripts/deb/config.yaml`;
  - `go.mod`: stdlib plus the existing `golang.org/x/crypto/pkcs12` and `go.yaml.in/yaml/v3` only;
  - `internal/broker/tls.go`.

## Sources

- **Engine:** `internal/mqtt/server.go`:
  - processPublish 953-1078; InjectPacket 936-950; NewClient 244-263;
  - attachClient and will on disconnect 465-494; DisconnectClient 1567-1587; Close 1650-1668;
  - sendLWT 1671-1707; delayed LWT 1908-1922; retainMessage 1082-1112; shared selection 1132-1138; offline/quota 1197-1205; retained purge 1879-1894.

  Also `internal/mqtt/hooks.go`, `internal/mqtt/packets/packets.go` (Copy 185-249), `internal/mqtt/packets/properties.go` (Copy 127-188), `internal/mqtt/packets/codec.go:46-56`, `internal/mqtt/clients.go`, `internal/mqtt/listeners/listeners.go:115-130`, `internal/mqtt/topics.go:322-327`, `/opt/go/src/net/net.go:853-866`.
- **Broker:**
  - `internal/broker/server.go` (build 115-512, Close 758-820);
  - `hook_storage.go` (OnPublished 278-311, OnRetainMessage 314-378, retention 381-404);
  - `hook_queue.go` (hydrate 65-85, OnPublished 105-145), `hook_winccoa.go`, `tls.go`;
  - `internal/archive/manager.go:100-120`, `internal/archive/group.go:152-167`;
  - `internal/stores/interfaces.go:57-62`, `internal/stores/oastore/retained.go:40-42`, `internal/stores/oastore/stores.go:397-399`;
  - `internal/winccoanative/namespace.go`, `internal/config/config.go`, `internal/config/load.go:19`, `Makefile:39-47`.
- **Internal publishers:** `internal/broker/server.go` (publishFn and managers 396-502, auth hooks 247-264); `internal/bridge/mqttclient/connector.go:21-48`; `internal/redfish/manager.go:142`, `internal/redfish/subscriber.go:69,142-168`; `internal/bridge/rtspcamera/connector.go:554-620`, `config.go:101,129`; `internal/hostinfo/collector.go:34`; `test/integration/winccoa_native_store_test.go:25-35`.
- **Plans:** `dev/plans/plan-winccoa-broker-embedded-manager.md` §7 and AC-25..AC-35, `dev/plans/plan-winccoa-node-redundancy-status.md`, `dev/plans/spec-winccoa-native.md`, `dev/done/plan-queue-performance.md`.
- **Kotlin precedent:** `main/broker/src/main/kotlin/bus/MessageBusZenoh.kt`, `bus/ZenohMessageEnvelope.kt`, `data/BrokerMessage.kt`, `data/BrokerMessageCodec.kt`, `handlers/SessionHandler.kt`, `extensions/KafkaProtocolServer.kt`, `devices/mqttclient/MqttClientConnector.kt`.
