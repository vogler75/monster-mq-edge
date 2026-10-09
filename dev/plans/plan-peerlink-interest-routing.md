# Plan: PeerLink interest routing (forward only what a peer subscribes to)

**Status: implemented in edge and main (2026-10-09), IR-M0–IR-M6.**

Edge implementation notes:
- Built: the `Receive.Queue: true` default (IR-P1), the `CapInterest` wire (0x20/0x21, sparse batches), the
  source interest table and capture mask, the consumer tracker with E8, bus, archive and inline interest, offline
  persistent sessions restored from the session store at startup, expiry sweeps, status, config and docs.
- HOT/COLD provider (C6): MQTT bridge devices carry `redundancy: ALWAYS|HOT_STANDBY|COLD_STANDBY` in their
  config JSON. The outbound filters of standby bridges feed `SetProvided("redundancy", ...)` and are announced
  also for a COLD bridge that is not running; a device change updates the announcement. The bridges have no
  role behaviour yet (they run as before); that comes with the redundancy plan.
- Generation rollover: before the delta generation would wrap, the tracker resets it to 0 and sends every
  cursor a FIRST/LAST snapshot, which the source accepts at any generation.
- G-IR1 (Apple M-series, not target hardware): capture is about 100 ns/1 alloc with routing off, 38 ns/0 allocs
  for a skipped publish, 53 ns at 10 % and 185 ns at 100 % interest. End to end (`PEERLINK_BENCH=1 go test
  ./internal/peerlink -run LinkInterestThroughput -v`), 10 % interest drains the link 4.6–5.5 times faster with
  10 times fewer bytes; 100 % interest has the same throughput and bytes (0.95–1.05). Passed; re-check on
  amd64/armv7 target hardware.
- Integration scenarios 1–17 run, including 6 (TCP gate proxy), 13 (standby bridges) and 17 (edge with the main
  broker, `MONSTERMQ_MAIN_BROKER=<main>/broker go test ./test/integration -run MainPair`), and per-direction
  `Interest: OFF` (source-only, consumer-only, both).
- Main finding from scenario 17: main's status printed `epoch` signed and reported source interest flat; main
  now writes unsigned epochs and edge's nested `interest` objects.

Proposed file: `dev/plans/plan-peerlink-interest-routing.md`. This plan extends PeerLink as described in
[spec-peerlink-redundancy.md](../../winccoa/doc/spec-peerlink-redundancy.md) and plan-peerlink.md (removed; last version at `2fe2282:dev/plans/plan-peerlink.md`). It borrows the
interest-propagation model of NATS routes (nats-server `server/route.go`: `updateRouteSubscriptionMap`,
`sendSubsToRoute`, `processRemoteSub`, `removeRemoteSubs`). It deliberately does not copy NATS's at-most-once
delivery: the PeerLink log, its offsets and its resume stay as they are.

Estimates are marked **(est.)**. None of them may be claimed until gate G-IR1 has measured them (section 12).

---

## Implementation readiness and cross-plan order (2026-10-09)

Owner authorized resolving the review recommendations on 2026-10-09. The observer, queue-default change, archive/bus coverage, `Unknown: ALL`, 64-consumer startup limit and status-only interest observability are accepted for both brokers. This authorizes the plan decisions; no code has been implemented by this revision.

1. Implement IR-M0 in both brokers (`Receive.Queue: true` is an intentional default change even with interest routing off).
2. Implement redundancy roles/wire and component configuration plus lifecycle (main R1/R2, edge P1/P2). Run shared role golden vectors and mixed-pair tests. No GraphQL/dashboard dependency.
3. Implement IR-M1–IR-M5; IR-M1's configured HOT/COLD provider depends on the component config from step 2. IR-M2 wire codec can be developed independently, reserving `CapRole` bit 4.
4. Complete IR-M6 benchmarks, expiry sweeps and mixed-pair acceptance tests before declaring routing ready. Main uses Maven/test-scoped benchmarks, not a Gradle build.
5. Implement witness phases with the revised shared C3/C8 rules and failure tests. GraphQL/main R7/edge PG and their dashboard work remain separately gated by explicit human commitment in both repositories.

For tracker synchronization, take snapshots at a fixed generation and retain subsequent absolute deltas per puller until delivered. Serialize snapshot frames and following deltas, never interleave a delta into an open snapshot. If a slow puller outruns the retained change history, send a new FIRST/LAST snapshot before further deltas. All chunks must use one generation; a mismatching chunk generation is a protocol error. Before u32 generation wrap, reconnect and reset with a snapshot; comparisons must not silently wrap.

## 1. Goal, scope, non-goals

### 1.1 Goal

Today a source captures every publish accepted by `Capture.Include`/`Exclude` and serves it to every configured
consumer, whether or not anything on that consumer wants it. With **interest routing** enabled, the flow changes:

1. Each consumer tells each source which topic filters it has local interest in. This is a refcounted set of
   filters, sent as a full snapshot after the handshake and as deltas afterwards.
2. The source appends a publish to its log only if at least one consumer has matching interest. Each log record is
   tagged with the set of consumers it is for.
3. The source serves each consumer only the records tagged for it.
4. Each remote subscription carries a **persistent** flag: it is persistent if the owning MQTT session survives a
   disconnect. When a consumer restarts, the source drops the consumer's **volatile** interest (sessions that died
   with it). It keeps the consumer's **persistent** interest until the session expiry the consumer's engine applies.

Interest routing is **optional**. It is off by default, and with it off routing stays dense; the separately approved IR-M0 queue-default change still applies.

### 1.2 In scope

- A consumer-side interest tracker. It is fed by an engine observer on the topic index (E8) and by the message bus
  and archive filter providers.
- Protocol extension `mmq-peer/1`, capability `CAP_INTEREST`:
  - `INTEREST_SNAPSHOT` and `INTEREST_DELTA` frames from consumer to source
  - restart detection via the existing `HELLO.InstanceID` (no `HELLO` layout change)
  - a sparse `BATCH` from source to consumer
- A source-side remote interest table, a capture-time consumer mask, per-record masks in the log, a sparse serve
  path, and an LWM that is not pinned by records a consumer never needs.
- A peer interest lifecycle (`UNKNOWN`, `LIVE`, `DISCONNECTED`), with restart detection and persistent expiry.
- Configuration, status and metrics, tests, and benchmarks.

### 1.3 Non-goals (v1)

- **Cluster-wide shared subscriptions** (one delivery per group across all nodes, NATS queue-group style with
  group names in the record). `SharedSubscriptions: SKIP|DELIVER` stays as it is. Section 9.3 describes how v1
  announces shared subscriptions. Phase 2 is sketched in section 16.
- **Multi-hop forwarding and gossip discovery.** The topology stays a one-hop full mesh.
- **Filtering retained publishes.** Retained publishes and the retained snapshot still go to every consumer
  (section 9.1).
- Persisting the remote interest table. It is soft state, rebuilt from snapshots, as in NATS.
- Subsumption, that is, collapsing `a/b` into `a/#` on the wire. Filters are announced verbatim, as in NATS.
- GraphQL SDL changes, storage DDL changes and ABI changes.

---

## 2. Governance and required sign-offs

| # | Item | Proposal |
|---|---|---|
| IR-S1 | Engine change E8 (section 10) | Add an optional interest observer to `TopicsIndex` in `internal/mqtt/topics.go`. It changes nothing when unset. **Accepted by the owner (2026-10-08).** |
| IR-S2 | Protocol extension | Capability-gated additions to `mmq-peer/1`. Mixed versions keep working, and a peer without `CAP_INTEREST` is served as today (section 5.1). |
| IR-S3 | Semantics change when enabled | A publish made before the consumer's interest reaches the source is not forwarded. That window is about `FlushMs` plus half the RTT after a SUBSCRIBE on the consumer, and the whole reconnect window if `Unknown: NONE` is set. NATS has the same window. When interest routing is off, routing stays dense; IR-M0 still changes the queue default. |
| IR-S4 | Receiver archive and bus coverage | **Decided by the owner (2026-10-08).** With `Receive.Archive: true`, the `TopicFilters` of **all** archive groups are announced, including the built-in `Default` group (`TopicFilters: ["#"]`, `internal/archive/manager.go:110-117`). A consumer with an archive group on `#` therefore gets everything, as today. There is no separate list of announced groups. The GraphQL message bus receives only forwarded topics unless its own filters are announced (`Receive.Bus`). |
| IR-P1 | Prerequisite: queue forwarded messages like local ones | **Decided by the owner (2026-10-08).** A message received from a peer is queued for offline persistent sessions exactly like a message published by a local client. `Receive.Queue` defaults to `true` (`internal/config/config.go`, spec section 10). The key stays accepted, because PeerLink rejects unknown keys at startup and existing configs may set it. Changes the existing PeerLink semantics in `plan-peerlink.md` 12.4 and the spec, independent of interest routing. The own-publisher exclusion stays (`hook_queue.go:174`). Consequence: a persistent client that moves between nodes may get messages again from its old node's queue when it returns (QoS 1 at-least-once; local publishes already behave this way today). |
| IR-S5 | GraphQL | None in v1. Interest status goes into the existing PeerLink status endpoint (`/peerlink/v1/status`, JSON, not GraphQL). **Accepted by the owner (2026-10-08): no GraphQL changes.** |

---

## 3. Model in one picture

```
consumer B (has subscribers)                     source A (has publishers)
──────────────────────────                       ─────────────────────────
TopicsIndex ──E8 observer──┐                     remote interest table
bus filters ───────────────┼─▶ interest tracker      peer B: {sensors/+/temp: P(exp 3600),
archive groups ────────────┘   refcount per filter            alarms/#: V}
                                    │                 peer C: {...}
                                    │ INTEREST_SNAPSHOT  │
                                    │ INTEREST_DELTA     ▼
                                    └──(puller conn)──▶ union trie (filter → peer bitmask)
                                                         │
                                       client publish ──▶ capture: mask = match(topic) & live peers
                                                         │   mask == 0 → not appended
                                                         ▼
                                                      log record {frame, mask}
                                                         │
                         sparse BATCH (only records ◀────┘ serve to B: skip records without bit B
                         with bit B, Span covers skips)
```

Interest travels on the connection the consumer already dials: the puller connection, which carries `FETCH`,
`COMMIT` and `PING` today. Data comes back on the same connection. On one TCP stream, an `INTEREST_DELTA` sent
before a `FETCH` is processed before it.

---

## 4. Consumer side: the interest tracker

### 4.1 Sources of interest

| Source | When announced | Class |
|---|---|---|
| Network client subscriptions (`TopicsIndex.Subscribe`/`Unsubscribe`, including subscriptions restored by `loadSubscriptions`, `internal/mqtt/server.go:1901`) | Always | Persistent if the session is persistent, otherwise volatile (4.3) |
| Inline subscriptions (`InlineSubscribe`, used by scripts and services) | Always, except owners that skip replicas (bridge outbound while `Receive.BridgeOutbound: false`). HOT_STANDBY/COLD_STANDBY components (redundancy contract C6) are always announced, whatever the broker role or `BridgeOutbound`, including COLD components that are not running, so a takeover finds the feed already in place. Their filters come from the redundancy component provider (below). | Volatile |
| Shared subscriptions `$share/g/f` | Only with `Receive.SharedSubscriptions: DELIVER`, announced as plain `f` (9.3) | As the session |
| Message bus filters (`pubsub.Bus.Subscribe(filters…)`, `internal/pubsub/bus.go:30`; GraphQL subscriptions, MCP) | Only with `Receive.Bus: true` | Volatile |
| Archive group `TopicFilters` (all groups, including `Default`) | Only with `Receive.Archive: true`. HOT/COLD archive groups follow the same rule (C6): announced on every node, active or not | Persistent, with no expiry while configured (`expirySec` `0xFFFFFFFF`) |
| Redundancy component provider: configured filters of `HOT_STANDBY`/`COLD_STANDBY` components (C6) | Always, from configuration, whether the component runs or not (archive groups: only with `Receive.Archive: true`) | Volatile (archive groups: persistent, as above) |

**Redundancy component provider (C6).** The tracker has a provider that reads the component configuration, not
the running components. It announces the configured topic filters of every component with `Redundancy:
HOT_STANDBY` or `COLD_STANDBY`:
- bridges: their outbound and subscription filters
- scripts: their subscriptions
- archive groups: their `TopicFilters`

The class is `VOL`; archive groups are `PER` with no expiry, as in the archive row. The filters are announced
whatever the broker role and `Receive.BridgeOutbound`, and whether or not the component is running. The
provider's counts are refcounted separately from runtime subscriptions: the runtime subscriptions of a running
component are counted through E8 as usual, and a component that stops withdraws only those, never the provider's
counts. On a configuration change (component added or removed, mode or filters changed) the provider recomputes
its set, and the tracker emits deltas for the filters whose announced class changes. Components in mode `ALWAYS`
are not covered by the provider; they follow the rows above.

The consumer's `Receive.Include`/`Exclude` stays in force on apply. Filters are announced verbatim, without
intersecting them with the receive filters, because intersecting wildcard filters is not worth the complexity.

### 4.2 Aggregation (the NATS refcount)

The tracker keeps `map[filter]*entry{vol, per uint32; maxExpiry uint32}`.

- `vol` and `per` count local subscriptions on the filter by class.
- `maxExpiry` is the largest session expiry among the persistent ones, in seconds. `0xFFFFFFFF` means "never".
  It is recomputed lazily when the holder of the maximum leaves.
- The **announced class** of a filter is:
  - `NONE` when both counts are 0
  - `VOL` when `per == 0`
  - `PER(maxExpiry)` otherwise

A delta is sent **only when the announced class changes**. A change of `maxExpiry` alone counts as a class change
only if it moves by more than 10 % or crosses "never". A thousand clients on `sensors/#` therefore produce one
announcement, as in NATS `acc.rm`.

Deltas are coalesced per filter in a pending map and flushed by the puller's writer goroutine, like `COMMIT`, at most
every `Interest.FlushMs` (default 5 ms), or immediately when the pending map reaches 1024 entries or the encoded
delta would exceed `MaxConsumerFrame` (64 KiB), whichever comes first. Each flushed `INTEREST_DELTA` frame takes the
next tracker generation (5.4), so no delta frame ever exceeds 64 KiB. Rapid
subscribe/unsubscribe churn on one filter collapses to its final state, which plays the role of NATS `acc.lws`.

The tracker is one per node, shared by all pullers. Each puller holds a cursor (a tracker generation) so that a
peer that reconnects gets a consistent snapshot followed by the deltas after it (5.3).

### 4.3 The persistent flag

A subscription is **persistent** if its session survives a disconnect:
- MQTT 5: `SessionExpiryInterval > 0`
- MQTT 3.1.1: `CleanSession == false`

This is the same predicate as `QueueHook.OnSessionEstablished` (`internal/broker/hook_queue.go:270`). Its expiry is
the session expiry. For MQTT 3.1.1 that is the broker's maximum session expiry; "never" if unlimited.

**Offline persistent sessions always count.** Forwarded messages are queued for offline persistent sessions
exactly like messages published on this node (prerequisite IR-P1, section 2). A persistent session therefore
counts as `PER` whether it is online or offline on the consumer, until it expires. The tracker reacts to
`OnClientExpired` (and to a session being replaced by a clean one) to withdraw its subscriptions, in addition
to E8.

### 4.4 Interest that must never be announced

- Subscriptions of the PeerLink injector clients themselves (the `peerlink:` prefix).
- `$`-topics. Filters that start with `$` are never sent, because they are never captured.
- A filter that is empty, longer than `MaxFilterBytes` (default 1024), not valid UTF-8, or outside the MQTT
  filter grammar (`peerlink/filter.go: validFilter`). It is **ignored**: not announced, counted as
  `interestRejected` and logged at WARN. An invalid filter matches nothing locally either, so nothing is lost
  (Q-IR4, decided). The source applies the same checks to received entries and ignores invalid ones (5.6).

---

## 5. Wire protocol additions (`mmq-peer/1`, capability `CAP_INTEREST`)

### 5.1 Negotiation

- `CAP_INTEREST` is a new bit in the capability bitmaps of `SERVER_HELLO` and `HELLO`: `1<<5` (`1<<4` is
  reserved for `CapRole` from the redundancy plan). The value is pinned identically in main
  (`main/dev/plans/plan-peerlink-interest-routing.md`, section 4).
**Final capability agreement.** `SERVER_HELLO` advertises node-wide `CapInterest` support when `Interest.Enabled` is true; it is sent before the source knows the consumer NodeId. The consumer offers the bit in `HELLO` only when its own interest setting is enabled and that source is not `Interest: OFF`. After authentication identifies the consumer, the source computes the intersection and clears `CapInterest` if its per-peer setting is OFF; `HELLO_OK.capabilities` is the final agreed set. The consumer uses that final set, never just the first two offers. Only then may interest frames and sparse batches be sent. On either node OFF therefore yields dense serving in both directions, including shared-secret/plain connections without a client certificate. Unexpected interest frames or sparse batches without final agreement are protocol errors.

Without final agreement the source always sets that consumer's mask bit, serves dense batches, and the consumer sends no interest frames (rolling-upgrade path).

### 5.2 Restart detection (existing `HELLO.InstanceID`)

`HELLO` already carries `InstanceID` (u64, drawn once per process start; `internal/peerlink/wire/frame.go:556`,
set in `puller.go:479`, read in `server.go:608`; main has the same field). No new TLV is needed. The
source compares it with the value last seen for that NodeId:

| Comparison | Meaning | Source action |
|---|---|---|
| Same | Network blip; the consumer kept running | Keep all interest. The snapshot that follows is reconciled (5.3). |
| Different | The consumer restarted; its clean sessions are gone | Drop **volatile** interest of that peer immediately. Keep persistent interest until the snapshot replaces it. |
| None seen yet (source restarted) | Unknown | Apply `Interest.Unknown` until the snapshot arrives (7.1, 7.2 case C) |

### 5.3 INTEREST_SNAPSHOT (consumer → source)

```
INTEREST_SNAPSHOT  0x20
  u32 generation        tracker generation the snapshot reflects
  u8  flags             FIRST=1, LAST=2 (a snapshot may span several frames); other bits sent as 0, ignored on read
  u32 count
  count × { u8 class (1=VOL, 2=PER); u32 expirySec; u16 len; filter bytes }
```

- The consumer sends it right after `HELLO_OK` and before its first `FETCH`. It is split into frames of at most
  `MaxConsumerFrame` (64 KiB, the existing consumer frame limit). All frames of one snapshot carry the same
  generation. A one-frame snapshot has `FIRST|LAST`.
- A snapshot always applies, whatever its generation, and at `LAST` sets the peer's last applied generation to
  its own.
- A `FIRST` frame while a snapshot is still open (no `LAST` yet) discards the open one, including its marks, and
  starts over.
- Order: after `HELLO_OK`, before the `SNAPSHOT` (resync) phase and before the first `FETCH`. The source accepts
  `INTEREST_SNAPSHOT`/`INTEREST_DELTA` frames at any time after `HELLO_OK`, including during the `SNAPSHOT` phase.
- The source applies it as **mark and sweep**. Entries of that peer not present in the snapshot are removed when
  `LAST` arrives. The new set replaces the old one atomically: the union trie is updated once, at `LAST`.
- Until `LAST`, the peer keeps its previous state, or the `Unknown` policy.

### 5.4 INTEREST_DELTA (consumer → source)

```
INTEREST_DELTA  0x21
  u32 generation        strictly increasing per frame
  u32 count
  count × { u8 class (0=NONE, 1=VOL, 2=PER); u32 expirySec; u16 len; filter bytes }
```

- Deltas are split like snapshots: no `INTEREST_DELTA` frame exceeds `MaxConsumerFrame` (64 KiB). The consumer
  flushes when the pending map reaches 1024 entries or the encoded delta would exceed 64 KiB, whichever comes
  first (4.2), and each delta frame takes the next generation.
- The source ignores a delta whose generation is ≤ the peer's last applied generation (of a snapshot or a delta).
  Otherwise it applies the delta and sets the last applied generation to the delta's.

`class` is the new **absolute** class of the filter, not an increment. A lost or duplicated delta therefore cannot
corrupt a count: the latest value wins. This is simpler than NATS `RS+`/`RS-`, which relies on exactly-once
delivery over the route.

### 5.5 Sparse BATCH (source → consumer)

There is a new batch flag, `BatchFlagSparse` = `1<<6` (after `Truncated` = `1<<5`; same value in main). When it is set, these fields follow the 68-byte header:

```
u32 span               offsets covered: BaseOffset .. BaseOffset+span-1
u32 deltas[Count]      offset of record i = BaseOffset + deltas[i]   (strictly increasing, < span)
```

- On apply, the consumer sets `appliedNext` to `BaseOffset + span`, not `+ Count`. It then commits as today.
  Duplicate suppression (`appliedNext`) and resume are unchanged.
- `Count == 0` with `span > 0` is valid. It is a pure "skip" batch that lets an idle consumer advance its committed
  offset (6.5).
- The flag is only sent to consumers that negotiated `CAP_INTEREST`, and only if at least one record in the span
  was skipped. Otherwise the batch is dense.
- The deltas table counts against the batch byte limit (`maxBytes` of the `FETCH`), like the records.
- The consumer treats each of these as a protocol error (5.6): a delta that is not strictly greater than the one
  before it, a delta ≥ `span`, `span < Count`, or a sparse batch with `span == 0`.
- Fuzz and vector tests in `internal/peerlink/wire` get the new frames.

### 5.6 Encoding, validation and errors

- **Encoding.** All integers are little-endian, like every `mmq-peer/1` field. Filter bytes are UTF-8, and
  `len > 0`.
- **`expirySec`.** `VOL` and `NONE` entries send 0; the value is ignored on read. `PER` entries that never expire
  (archive groups; on main, MQTT 3.1.1 persistent sessions; on edge, sessions with an unlimited
  `MaximumSessionExpiryInterval`) send `0xFFFFFFFF`.
- **Unknown flag bits** in `INTEREST_SNAPSHOT.flags` are sent as 0 and ignored on read.
- **Frame errors** (protocol error: the receiver sends `GOAWAY(protocol)` and closes the connection):
  - a truncated frame, or a frame whose `count` does not match its body (too few bytes for `count` entries, or
    bytes left over after them)
  - an `INTEREST_SNAPSHOT` frame without `FIRST` while no snapshot is open
  - a sparse `BATCH` that violates 5.5
- **Invalid entries** (the entry is ignored, counted in `interestRejected`, and the rest of the frame is applied):
  - `class` not 1 or 2 in a snapshot, or not 0, 1 or 2 in a delta
  - `len == 0`, `len > MaxFilterBytes` (the receiver's own setting), filter bytes that are not valid UTF-8, or a
    filter outside the MQTT filter grammar (`validFilter`)

  A bad `class` is always an invalid entry, never a frame error: the entry layout is fixed, so the decoder can
  skip it. An ignored snapshot entry is not marked, so mark and sweep removes a previous entry for that filter.
  An ignored delta entry leaves the previous state of that filter unchanged.
- **Restarting a snapshot.** A `FIRST` frame while a snapshot is open discards the open snapshot and its marks
  and starts over (5.3); it is not an error.

---

## 6. Source side

### 6.1 Remote interest table

Each peer has a `peerInterest` struct:
- `entries map[filter]{class, expirySec}`
- `state` (section 7)
- `instanceId`
- `disconnectedAt`
- the snapshot generation

All peers share one **union trie** (`filterNode`, extended from `internal/peerlink/filter.go`). Each node holds
`mask uint64` (a bit per consumer index, as in `Log.ConsumerIndex`) and is rebuilt incrementally per change. Lookups
on the capture path take an `RLock`. Writes come from interest frames only, which is rare compared with publishes.

Limit: 64 consumers per source, which is plenty for a full mesh. When more are configured, interest routing refuses
to start (Q-IR5).

**Received entries are validated** as in 5.6: invalid entries are ignored and counted in `interestRejected`.

**`MaxFiltersPerPeer`.** The limit is checked on the peer's filter set after each applied snapshot (at `LAST`) and
after each applied delta. If the set exceeds `MaxFiltersPerPeer`, the source serves that peer **ALL**: its mask
bit is always set and its batches are dense and unfiltered, as for a peer without `CAP_INTEREST`. While in ALL,
the source clears the peer's entries from the union trie and need not keep applying its deltas (only generations
are tracked). It goes back to filtered serving only when a later snapshot, at `LAST`, is within the limit (the consumer sends
the next snapshot after its next reconnect). Each transition (into ALL and back) logs one WARN; each transition into ALL increments
`interestOverLimit` (13).

### 6.2 Capture mask

In `Hook.captureAs` (`internal/peerlink/hook.go:165`), after the existing `accept`/include/exclude checks:

```
mask := alwaysMask                    // consumers without CAP_INTEREST, in state UNKNOWN with Unknown=ALL
if retained || snapshotRecord { mask = allConsumers }       // 9.1
else { mask |= union.match(topic) & activeInterestMask }    // one trie walk
if mask == 0 { countSkipped(); return }                     // not appended, not encoded
```

The check happens **before** the record frame is encoded, so an unwanted publish costs one trie walk and no
allocation.

### 6.3 Log storage

- `Log.Append(frame, kind)` becomes `Append(frame, kind, mask uint64)`.
- `logChunk` gets a parallel `masks [1024]uint64` array. That is 8 KiB per chunk, preallocated with the chunk.
- With interest routing off, all masks are `allConsumers`, and the read path takes the dense branch unchanged.

### 6.4 Serve path

`Log.ReadFor(c, from, …)` collects records whose mask has bit `c` until it reaches `maxRecords`/`maxBytes`. It also
stops after `Interest.MaxScanPerFetch` offsets (default 65536), so a consumer with sparse interest cannot make one
read walk the whole log. The read returns `Base`, `Span` and the offset deltas, and the batch is sparse if any record
was skipped. Mask scanning happens on the chunk snapshot outside the log mutex, like the existing read.

### 6.5 Committed offsets and the LWM

A record without bit `c` is never needed by consumer `c`. Two rules stop such records from pinning the log:

1. **Caught-up auto-advance.** On `Append`, any consumer with `C[c] == offset` whose bit is not set gets
   `C[c] = offset+1` in the same critical section. This is O(consumers) per append, and only for caught-up
   consumers.
2. **Lagging advance.** After a `COMMIT`, and at most every 100 ms for disconnected consumers, the source advances
   `C[c]` over the leading run of records without bit `c` (bounded scan). A disconnected consumer whose interest
   is gone therefore stops holding the log after at most one scan.

**Expiry backlog reclamation (shared rule).** Expiring a PER entry changes future capture and triggers a bounded sweep of that peer's outstanding log records, at most `MaxScanPerFetch` offsets per 100 ms tick. For each previously tagged non-retained record, clear only that peer's bit if the topic matches none of its remaining VOL/PER interests. Preserve retained publishes, clears, snapshot records and their tombstones, and preserve records still covered by remaining interests. Keep minimal immutable topic/kind metadata beside records where encoded tombstones do not contain it; allocate this only for records actually appended. Update masks under the log lock so fetch/chunk snapshots and LWM see a consistent result; never clear another consumer's bit. The normal skip/LWM rules then reclaim the abandoned backlog without `Lost`/`GAP`; expose `interestBacklogDiscarded`. Completion may take several ticks for a large log. Ordinary unsubscribe and restart VOL withdrawal preserve already captured backlog (restart scenario 5); only persistent expiry triggers abandonment. Tests cover partial overlap, retained/tombstones, concurrent fetch and multi-tick completion.

`S[c]` (served) and the loss counters keep their meaning. Records the consumer never needed are never counted as
lost (`Lost`/`GAP`).

---

## 7. Peer interest lifecycle (lost and restarted brokers)

### 7.1 States per (source, peer)

```
            first snapshot LAST
 UNKNOWN ─────────────────────────▶ LIVE ◀──── reconnect, same InstanceId: snapshot reconciles
   ▲                                  │   ◀──── reconnect, new InstanceId: drop VOL, then snapshot
   │ source restart                   │ link lost
   │                                  ▼
   └──────────────────────────── DISCONNECTED   VOL kept; each PER entry expires on its own
```

| State | Mask bit set for | Meaning |
|---|---|---|
| `UNKNOWN` | Everything (`Unknown: ALL`, default) or nothing (`Unknown: NONE`) | The source has no interest information for this peer, e.g. after the source itself restarted |
| `LIVE` | VOL and PER entries | Connected, snapshot complete |
| `DISCONNECTED` | VOL entries and PER entries that have not expired | Link lost. The source cannot tell a network partition from a dead peer, so it keeps capturing for volatile interest until the peer reconnects (Q-IR2, decided). |

There is no separate hold timer for volatile interest. What is captured for a disconnected peer is bounded by the
log limits (`MaxMessages`, `MaxBytes`): when the log is full, the oldest records are removed, as today.

### 7.2 A peer goes down and comes back

**Case A: the peer process restarts.** Its clean sessions are gone, and its persistent sessions are restored from
the session store (`loadSubscriptions`).

1. The link drops, and the source moves the peer to `DISCONNECTED`. It keeps capturing for all of the peer's
   interest, and the records stay in the log, as today. PER entries whose session expiry runs out stop matching.
2. The peer comes back with a **new InstanceId**. The source drops all VOL entries of that peer at once, then
   receives the peer's snapshot. The snapshot contains the restored persistent subscriptions and any subscriptions
   clients made since the restart.
3. Mark and sweep removes whatever is no longer there, and the state becomes `LIVE`.
4. The peer pulls the backlog from its committed offset. Records for persistent sessions that are still offline
   are queued (IR-P1). Records captured for the dead clean sessions find no subscriber and are dropped on apply.
   That is wasted transfer, bounded by the log limits.

**Case B: network partition or blip.** The peer kept running.

1. The state becomes `DISCONNECTED`. The peer reconnects with the **same InstanceId**.
2. The snapshot reconciles any subscription changes made while the link was down, and the state becomes `LIVE`.
3. Nothing is lost for any session as long as the log did not overflow. This matches today's lossless resume.

**Case C: the source restarts.** It has a new log epoch and no interest table.

1. All peers start in `UNKNOWN`. With `Unknown: ALL`, the source captures everything for them until each
   snapshot arrives, normally within one handshake RTT after it starts. This keeps today's guarantee that nothing
   published after the source starts is missed by a running consumer.
2. With `Unknown: NONE`, nothing is captured for a peer until its snapshot arrives. This trades that window for
   less memory on a source whose peers take long to connect.

### 7.3 Expiry of persistent interest

Each PER entry carries the session expiry the consumer's engine applies to that session, the same value
`Server.clearExpiredClients` uses (`internal/mqtt/server.go:1979`):

- MQTT 5: the session's `SessionExpiryInterval`, already capped by `MaximumSessionExpiryInterval` at CONNECT
  (`server.go:719`)
- MQTT 3.1.1 with `CleanSession == false`: the broker's `MaximumSessionExpiryInterval` (default: never)

The tracker announces the largest expiry among the sessions on a filter (4.2). A PER entry stops matching when
`now > disconnectedAt + expirySec`. The source checks this at most once a second, per peer, on the `background()`
tick (`internal/peerlink/manager.go:447`).

The expiry is measured from the moment **the link** was lost, not from when the client disconnected on the peer.
That only makes the source hold slightly longer than the peer's own expiry. The snapshot on reconnect corrects it.

### 7.4 Removing a peer from configuration

There is no hot reload. When a peer is removed from the YAML, its interest disappears at the next start.

---

## 8. Ordering and the "subscribe-before-data" window

- After a client subscribes on consumer B, publishes on A are forwarded only after B's delta reaches A. That is
  `FlushMs` plus half the RTT, the same window NATS has (IR-S3).
- The retained message the new subscriber receives comes from B's own replicated retained store, so the window
  never costs a retained value.
- Within one link, interest and fetch share the TCP stream: a delta written before a `FETCH` is applied before the
  source serves that fetch.

---

## 9. Special records

### 9.1 Retained publishes, retained clears and the snapshot

These always get `mask = allConsumers`. A consumer may gain a subscriber later and must already hold the retained
value, which plays the part of the replicated `$MQTT_rmsgs` stream in NATS. The `SNAPSHOT` fill is unchanged.

### 9.2 Wills

- Non-retained wills follow interest like any publish.
- Retained wills follow 9.1.
- The receiver-side will suppression is unchanged.

### 9.3 Shared subscriptions

| `Receive.SharedSubscriptions` | Announced | Effect |
|---|---|---|
| `SKIP` (default) | No | Replicas are not delivered to shared groups on the receiver anyway |
| `DELIVER` | `f` from `$share/g/f`, with the class of the member sessions | Each node delivers once per group locally, as today. Cluster-wide one-per-group is phase 2. |

### 9.4 Echo suppression and split horizon

Unchanged. Remote interest is never re-announced: the tracker ignores injector subscriptions (4.4), and replicas
are never captured. This is the interest equivalent of NATS's "routes never announce route subscriptions"
(`route.go:2560`).

---

## 10. Engine change E8: topic index interest observer

`internal/mqtt/topics.go` gets:

```go
// InterestObserver is notified after a subscription is added to or removed from the index.
// Calls happen after the index lock is released, in index order per client.
type InterestObserver interface {
    SubscriptionAdded(client string, filter string, shareGroup string, inline bool, inlineID int)
    SubscriptionRemoved(client string, filter string, shareGroup string, inline bool, inlineID int)
}
func (x *TopicsIndex) SetObserver(o InterestObserver)
```

- It is called from `Subscribe` (407), `Unsubscribe` (429), `InlineSubscribe` (374) and `InlineUnsubscribe` (388),
  only when the subscription is new or actually existed.
- It covers network clients, inline clients, subscriptions restored by `loadSubscriptions` and session takeover,
  with no hook gaps.
- The observer looks up the session class through the existing `ClientState` dependency
  (`internal/peerlink/manager.go:44`), extended with `SessionClass(clientID) (persistent bool, expirySec uint32)`.
- With no observer set, the cost is one nil check per subscribe.

---

## 11. Configuration

```yaml
PeerLink:
  Interest:
    Enabled: false              # off = CAP_INTEREST not offered: today's behaviour on this node (5.1)
    Unknown: ALL                # ALL | NONE  — mask for a peer until its first snapshot
    FlushMs: 5                  # delta coalescing on the consumer
    MaxScanPerFetch: 65536      # offsets scanned per FETCH for one consumer
    MaxFiltersPerPeer: 100000   # a peer over this is served ALL (dense) until a snapshot is within it (6.1)
    MaxFilterBytes: 1024        # longer filters are ignored, not announced or applied (4.4, 5.6)
  Peers:
    - NodeId: node-b
      Interest: INHERIT         # INHERIT | OFF  — OFF: no final CAP_INTEREST agreement on this link (5.1)
```

- `Enabled: true` advertises node-wide support. Per-peer OFF clears the consumer offer or the final `HELLO_OK` agreement (5.1); that link is dense in both directions. Use it for a peer that archives everything.
- Validation (the same list in main, section 7):
  - `Enabled: true` with more than 64 configured consumers: startup error (Q-IR5)
  - `FlushMs` ≤ 0: startup error
  - `MaxScanPerFetch` < 1024: startup error
  - `Unknown` not `ALL` or `NONE`: schema error
  - `MaxFilterBytes` outside 1..32768: startup error
  - `MaxFiltersPerPeer` < 1: startup error
- Every new key is documented in the README section on PeerLink and in the spec, as a new section.

---

## 12. Overhead, and when interest routing pays off

### 12.1 Costs

| Where | Cost | Estimate |
|---|---|---|
| Source, every publish | One trie walk over the union interest table, under an `RLock` | 0.2–1 µs per publish for ≤ 10k filters **(est.)**, about the cost of the existing `Subscribers()` match |
| Source, every record | An 8-byte mask, plus 8 KiB per 1024-slot chunk | Under 1 % of the log footprint for 200-byte records **(est.)** |
| Source, serve | Mask scan, and per-record deltas in sparse batches (4 bytes each) | Negligible when dense. Bounded by `MaxScanPerFetch` when sparse. |
| Source, memory | Interest table: about 100–200 bytes per (peer, filter) **(est.)** | 10k filters × 2 peers ≈ 4 MiB **(est.)** |
| Consumer, every subscribe/unsubscribe | One observer call, a map update, and possibly a pending delta | A few hundred ns **(est.)** |
| Link | Snapshot per reconnect (about 30 bytes + filter length per filter) and coalesced deltas | 10k filters ≈ 400 KiB per reconnect **(est.)** |
| Semantics | The subscribe-before-data window (8). A lost peer's volatile interest is kept until it reconnects, bounded by the log limits. | — |

### 12.2 Savings

For every publish no peer wants, the following are skipped:
- record encoding and log memory on the source
- network transfer
- TLS and CRC
- the whole apply path on the consumer: inject, topic match, bus, archive and retained check

The apply path is the measured throughput ceiling of PeerLink (plan-peerlink R2). This is where most of the
benefit lies.

### 12.3 When not to enable it

- **Peers that want almost everything.** Examples: a redundancy partner whose HMI subscribes `#`, or a
  peer with `Receive.Archive: true` and an archive group on `#` (such as `Default`). With a `#` filter the mask is always all ones, so you pay the
  costs and save nothing. For this case `Peers[].Interest: OFF` exists.
- **Peers that must have a complete last-value view of all topics.** For example, GraphQL topic browsing on the
  peer relies on the in-memory `Default` last-value store. Interest routing deliberately makes that view partial.

### 12.4 Gate G-IR1 (measured before the estimates above may be quoted)

The benchmark runs three source-side scenarios:
- 0 %, 10 % and 100 % of publishes with peer interest
- 1k and 10k remote filters, half of them wildcards
- 2 consumers

For each scenario it measures:
- capture ns/op
- allocs/op (must stay 0 for skipped publishes)
- log bytes
- end-to-end throughput on amd64 and armv7

It is compared with interest routing off. **Pass criteria:**
- at 100 % interest, throughput regresses by at most 5 %
- at 10 % interest, throughput is at least 3 times better

---

## 13. Status and metrics

`/peerlink/v1/status`: each peer gets an `interest` object with these fields:
- `state`
- `filters`
- `filtersPersistent`
- `snapshotGeneration`
- `lastSnapshotAt`
- `instanceId` (hex)

Counters:

| Counter | Side | Meaning |
|---|---|---|
| `interestSkipped` | Source | Publishes not appended because no peer was interested |
| `interestMatched` | Source | Publishes appended for at least one peer |
| `sparseBatches` | Source | Batches sent with `BatchFlagSparse` |
| `volatileDropped` | Source | Volatile filters dropped because the peer restarted (new InstanceId) |
| `persistentExpired` | Source | Persistent filters dropped after their announced expiry |
| `interestBacklogDiscarded` | Source | Consumer record bits cleared by persistent-expiry sweeps (not delivery loss) |
| `interestRejected` | Source and consumer | Invalid filters or entries ignored (4.4, 5.6) |
| `interestOverLimit` | Source | Transitions of a peer to ALL because its filter set exceeded `MaxFiltersPerPeer` (6.1) |
| `deltasSent` / `deltasReceived` | Consumer / source | Interest deltas on the link |

Every state change is logged at INFO with the peer NodeId and its filter count. `DISCONNECTED` is
logged at WARN.

---

## 14. Implementation steps and files

| Milestone | Content | Files |
|---|---|---|
| IR-M0 | IR-P1: `Receive.Queue` default `true`; update config test, spec section 10 and plan-peerlink 12.4; test that a forwarded QoS 1 message is queued for an offline persistent session and delivered on reconnect | `internal/config/config.go`, `internal/config/peerlink_test.go`, `internal/broker/hook_queue_test.go`, spec, plan-peerlink |
| IR-M1 | E8 observer; consumer interest tracker with refcount, classes, session expiry handling and provider hooks (bus, archive); redundancy component provider (4.1, C6): configured filters of all `HOT_STANDBY`/`COLD_STANDBY` bridges, scripts and archive groups, from configuration, running or not, refcounted separately from runtime subscriptions, updated on config change; unit tests | `internal/mqtt/topics.go`, `internal/peerlink/interest_tracker.go` (new), `internal/pubsub/bus.go` (filter change notification), `internal/archive/manager.go` (group filter listing), component configuration (bridges, scripts) for the provider |
| IR-M2 | Wire: `CAP_INTEREST`, `INTEREST_SNAPSHOT`/`DELTA`, `BatchFlagSparse`; fuzz and vector tests shared with main | `internal/peerlink/wire/frame.go`, `record.go`, `testdata/` |
| IR-M3 | Source: interest table, union trie with masks, capture mask, log masks, sparse read, LWM auto-advance | `internal/peerlink/interest_table.go` (new), `filter.go`, `hook.go`, `log.go`, `server.go` |
| IR-M4 | Lifecycle: InstanceId, `DISCONNECTED` state, persistent expiry, mark and sweep, `Unknown` policy | `server.go`, `manager.go`, `puller.go` (consumer sends the snapshot before the first `FETCH`; deltas via the writer goroutine) |
| IR-M5 | Config, validation, status, metrics, README and spec section | `internal/config/config.go`, `status.go`, `README.md`, `spec-peerlink-redundancy.md` |
| IR-M6 | Gate G-IR1 benchmarks; integration tests (section 15) | `internal/peerlink/bench_test.go`, `link_test.go`, `regress_test.go` |

---

## 15. Tests

Additional shared acceptance tests from the review:
- A source-only per-peer OFF setting on a shared-secret or plain connection clears the final HELLO_OK bit; consumer sends no interest frames and receives dense batches. Repeat with consumer-only OFF and both directions.
- Expire one of overlapping PER filters while other VOL/PER interests remain: abandon only uncovered non-retained backlog, preserve other peers' bits and retained/clear/tombstone obligations, and do not increment Lost/GAP. A log larger than MaxScanPerFetch is reclaimed over multiple ticks.
- Snapshot during concurrent subscription churn, slow puller exceeding retained change history, chunk generation mismatch and u32 generation rollover follow the readiness section; none silently loses a state update.


### 15.1 Unit tests

**Tracker**
- 0↔1 transitions per class
- VOL→PER upgrade and downgrade
- `maxExpiry` recomputation
- offline persistent sessions keep counting until they expire
- inline and bus sources
- `$share` handling for SKIP and DELIVER
- injector and `$` filters never announced
- redundancy component provider: a `COLD_STANDBY` component that is not running is announced; stopping a running
  `HOT_STANDBY` component does not withdraw its filters; a config change emits the matching deltas
- delta split: a pending map of 1024 entries, and a pending set whose encoding would exceed 64 KiB, each flush
  into several frames, none over 64 KiB, with strictly increasing generations

**Interest table (source)**
- a delta with generation ≤ the last applied generation (snapshot or delta) is ignored
- `FIRST` while a snapshot is open discards it and its marks
- invalid entries (bad class, `len 0`, over `MaxFilterBytes`, not UTF-8, bad grammar) are ignored and counted in
  `interestRejected`; the rest of the frame applies
- `MaxFiltersPerPeer` exceeded after a snapshot or a delta: the peer is served ALL, one WARN, `interestOverLimit`
  +1; a later snapshot within the limit restores filtering, one WARN

**Union trie**
- masks per peer, with overlapping and wildcard filters (`a/b`, `a/+`, `a/#`, `#`)
- `$` topics not matched by `#`/`+`

**Log**
- masked append and sparse read
- `MaxScanPerFetch`
- caught-up and lagging auto-advance; the LWM is not pinned by an uninterested disconnected consumer

**Wire**
- round trip, fuzz and golden vectors for the new frames, shared with main: little-endian integers, UTF-8 filters,
  `expirySec` 0 for `VOL`/`NONE`, `0xFFFFFFFF` for never-expiring `PER`, a finite `PER` expiry, a multi-frame
  snapshot (one generation) and split deltas (increasing generations)
- a sparse batch with `Count == 0`
- malformed frames → `GOAWAY(protocol)` and close: truncated frame, `count` not matching the body, a non-`FIRST`
  snapshot frame with no snapshot open
- sparse `BATCH` violations → protocol error: non-increasing delta, delta ≥ `span`, `span < Count`, sparse with
  `span == 0`; `BatchFlagSparse` is not set when no record in the span was skipped; the delta table counts
  against `maxBytes`
- unknown flag bits are ignored on read

### 15.2 Integration tests (two or three real brokers, `link_test.go` style)

1. B subscribes `x/1`. A publishes `x/1` and `x/2`. B receives only `x/1`, and A's log holds one record.
2. Three nodes. B subscribes `a/#`, C subscribes `a/b`. A publishes `a/b` and `a/c`. B gets both, C gets `a/b`.
   One record is appended for `a/c` (mask B) and one for `a/b` (mask B|C).
3. B has 100 clients on `s/#`: one delta is sent. 99 unsubscribe: no delta. The last one unsubscribes: a `NONE`
   delta, and publishes stop being appended.
4. Retained publishes on a topic B has no interest in still reach B's retained store. A later subscriber on B gets
   the retained value.
5. **Peer restart (the user's case).**
   - Setup: B has a clean session on `v/#` and a persistent session (expiry 3600) on `p/#`, offline at the
     time of the kill.
   - B is killed. A publishes `v/1` and `p/1` while B is down.
   - B restarts with a new InstanceId. Expected:
     - A drops B's `v/#` interest at once (`volatileDropped`); `v/1` is served but finds no subscriber on B
     - `p/1` is queued for the persistent session and delivered when it reconnects
     - after the snapshot, A's table for B holds only `p/#`, plus anything subscribed since the restart
6. **Network partition.**
   - The link is cut (proxy) while B keeps running, then restored with the same InstanceId.
   - All publishes for both sessions arrive after resume, and the state goes back to `LIVE`.
   - With a small `MaxMessages`, the oldest records are evicted and counted as lost, as today.
7. **Persistent expiry.**
   - B has only a persistent session (MQTT 5, `SessionExpiryInterval` 10 s) and is down for longer than that.
   - After expiry, new matching non-retained capture stops; the bounded sweep abandons uncovered backlog and frees it over successive ticks (retained records remain protected).
   - The same with MQTT 3.1.1 `CleanSession == false` and `MaximumSessionExpiryInterval` 10 s.
8. **Invalid filter.** An oversize filter on B is not announced, is counted as `interestRejected`, and does not
   cause `#` interest.
9. **Source restart with `Unknown: ALL`.** Publishes between A's start and B's snapshot reach B.
   **With `Unknown: NONE`.** They do not, and the absence is visible in the counters.
10. **Mixed versions.** B runs without `CAP_INTEREST`, so A serves B dense and unfiltered. C has `CAP_INTEREST` and
    is filtered on the same source.
11. **`Peers[].Interest: OFF`** for one peer gives a dense, full feed to that peer only.
12. **Archive.** With `Receive.Archive: true` and a group on `g1/#`, those topics are forwarded without any MQTT
    subscriber. With the `Default` group on `#`, everything is forwarded.
13. **HOT/COLD components.** A bridge outbound connector with `Redundancy: HOT_STANDBY` (or `COLD_STANDBY`, not
    running) on the STANDBY node, with `Receive.BridgeOutbound: false`: its filters are still announced, and after a
    takeover it receives the feed without a new snapshot. Same vectors as main M2b.
14. **Message bus.** A GraphQL subscription on `g/#` with `Receive.Bus: true` is announced. With
    `Receive.Bus: false` it is not, and `g/1` is not forwarded. (Main M1.)
15. **Bridge outbound, `ALWAYS`.** The inline subscriptions of a bridge outbound connector in mode `ALWAYS` with
    `Receive.BridgeOutbound: false` are not announced. (Main M2.)
16. **Session expiry.** A persistent session on B expires (`OnClientExpired`). A `NONE` delta follows, and A
    stops appending for it. (Main M3.)
17. **Mixed pair, edge ↔ main, in both directions.** Both nodes have `CAP_INTEREST` and are filtered; then one
    node runs without it and the other serves it dense and unfiltered. The shared golden vectors decode
    identically in both code bases. (Main M4 and M5.)

---

## 16. Phase 2 (not in this plan)

- **Cluster-wide shared subscriptions.**
  - Announce `$share/g/f` with a weight (the member count).
  - The source picks a group member, preferring local members. If it picks a remote member, it tags the record
    with the selected group names in a TLV, as NATS's RMSG queue list does (`msgHeaderForRouteOrLeaf`).
  - The receiver delivers only to those groups. This replaces `SKIP`/`DELIVER`.
- **Gossip discovery**, to grow the full mesh from one seed. This needs a trust model consistent with
  SharedSecrets and mTLS.
- **Persisting the interest table**, to shorten the `Unknown` window after a source restart.

---

## 17. Open questions (owner)

| # | Question | Recommendation |
|---|---|---|
| Q-IR1 | Should the archive groups of the consumer be announced? | **Decided:** all groups when `Receive.Archive: true`, including `Default`. A group on `#` forwards everything. No separate list (IR-S4). |
| Q-IR2 | How long to keep a lost peer's interest? | **Decided:** no hold timer. Volatile interest until the peer reconnects (dropped at once on restart); persistent interest until the session expiry the engine applies (7.3). The log limits bound memory, oldest records first. |
| Q-IR3 | Default for `Unknown`? | **Closed (2026-10-09):** `ALL`. It keeps today's "nothing missed after source start" property, and the window is one handshake. |
| Q-IR4 | An invalid or oversize filter on the consumer? | **Decided:** ignored, not announced, counted (4.4). |
| Q-IR5 | Over 64 consumers? | **Closed (2026-10-09):** startup error with interest routing on. Full meshes of that size are out of scope. |
| Q-IR6 | Should offline persistent sessions count? | **Closed:** always, because forwarded messages are always queued (IR-P1). |
| Q-IR7 | Expose interest status in GraphQL? | **Closed:** no GraphQL changes (IR-S5). |
| Q-IR8 | Restart detection: a new TLV or the existing `HELLO.InstanceID`? | **Closed:** restart detection uses `HELLO.InstanceID` (5.2); no TLV. |
