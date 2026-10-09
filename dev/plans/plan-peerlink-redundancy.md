# Plan: PeerLink redundancy for the edge broker (WinCC OA and witness)

**Status: reviewed and clarified (2026-10-09); implementation planned. GraphQL remains gated; native-pair validation pending.**

This plan holds every work item needed to close the gaps of the deployment
cases in [use-cases-deployment.md](../../winccoa/dev/plans/use-cases-deployment.md). It replaces the
former plans for node redundancy status and session ownership; only the parts
that a use case needs were kept.

The edge broker gets its role either from WinCC OA (direct connectivity,
source `WINCCOA`, case 6) or, without WinCC OA, from a witness lease in an
external Postgres / MongoDB (source `WITNESS`, case 3). The Kotlin main broker
has the same design without native WinCC OA access:
`main/dev/plans/plan-peerlink-redundancy.md` (Part II). Wire format, role
rules, YAML keys, component modes, status, witness storage and GraphQL are
fixed in one shared contract, section 3.0 here and II.0 there, with the same
text in both plans. The only difference: the source `WINCCOA` exists only on
the edge.

Related documents:

- [use-cases-deployment.md](../../winccoa/dev/plans/use-cases-deployment.md): the six cases and their gaps (G…)
- [spec-peerlink-redundancy.md](../../winccoa/doc/spec-peerlink-redundancy.md): PeerLink as built, section 14 WinCC OA redundancy
- [spec-winccoa-native.md](../../winccoa/doc/spec-winccoa-native.md), section 5: store semantics
- `main/dev/plans/plan-peerlink-redundancy.md`: the same roles for the Kotlin broker (witness section II.5, split II.8)
- [winccoa/README.md](../../winccoa/README.md): redundant systems, `connectToRedundantHosts`

Estimates are marked **(est.)**.

---

## 1. Scope

### 1.1 Gaps covered

| Gap | Case | Work package |
|---|---|---|
| G2.2 archive group on a shared DB writes once per node | 2 | WP-R2 (placement with `NodeID`; with `Source: NONE` the component modes have no effect, see 4.4) |
| G3.1 no role manager | 3 | WP-R1 (source `WITNESS`, 3.4) |
| G3.2 no `Redundancy` setting for bridges, devices, archives, scripts | 3 | WP-R2 (per-component `Redundancy: ALWAYS \| HOT_STANDBY \| COLD_STANDBY`) |
| G3.3 no session failover without WinCC OA | 3 | not covered, decision D4 |
| G5.2 passive host without `connectToRedundantHosts` loses writes | 5 | WP-W1 |
| G5.3 cold-standby broker (case 5b) | 5 | WP-SO phase P0 only |
| G6.1 session ownership on shared `MMQSessions` | 6 | WP-SO |
| G6.2 role is status only | 6 | WP-R1 (source `WINCCOA`) |
| G6.3 bridges/devices/archives double-act | 6 | WP-R2 |
| G6.4 node status topics `node/1`, `node/2` | 6 | WP-NS |

Not covered, by decision of the use-case document: G2.1 (no session cluster in
a mesh), G4.1 (memory queues in case 4), G5.1 (case 5 has one broker by
definition).

### 1.2 Non-goals

- Session or queue replication between brokers over PeerLink.
- Exactly-once across nodes (WP-SO, section 7.6).
- Shared SQL or MongoDB session stores with ownership (D4).
- Changing the WinCC OA redundancy state from MQTT (no `Command.*` writes, no
  switchover from MQTT).
- Forwarding writes from the passive to the active WinCC OA host (PeerLink
  Q29); `connectToRedundantHosts` covers it.

## 2. What exists today

| # | Fact | Where |
|---|---|---|
| E1 | Broker status has `redundant`, `host`, `hostName`, `role` (`STANDALONE`, `UNKNOWN`, `ACTIVE`, `PASSIVE`), `activeHost`, updated at once on a switchover from `_ReduManager[_2].Status.Active`. Status only: nothing acts on it. | `winccoanative/redu.go`, spec-peerlink-redundancy 14.2 |
| E2 | `connectToRedundantHosts` is a standard WinCC OA option, documented; no broker code checks it. | `winccoa/README.md`, spec 14.1 |
| E3 | PeerLink pair with `RedundancyPartner` and `oaRetained`; WinCC OA namespace excluded from capture in native mode. | spec 14.3, 14.4 |
| E4 | Outbound `mqttclient` bridges forward replicas only with `PeerLink.Receive.BridgeOutbound`. | `bridge/mqttclient/bus_adapter.go:40`, `broker/server.go:529` |
| E5 | Device filters know only `NodeID` (`local`/`*` = every node). | `winccua/manager.go:155`, `winccoa/manager.go:203`, `oastore/stores.go:104`, `broker/peerlink.go:182` |
| E6 | Archive groups archive replicas with `PeerLink.Receive.Archive` (all groups or none). | `config/config.go` `PeerLinkReceive` |
| E7 | The WinCC OA session store is a cache loaded once at start; no dpConnect; writes go through. All brokers write the same `MMQSessions_<client>` datapoint; `nodeId`/`connected` are written, never read back. | `oastore/records.go:89,165,208`, `oastore/stores.go:290` |
| E8 | With `WINCCOA` the queue store falls back to `MEMORY` (per node). | `config/config.go:521` |
| E9 | With PeerLink, persistent sessions are marked offline at start only if their `nodeId` is this node; the subscription index and WinCC OA interests are filled from the store only at start; a resumed session does not call `OnSubscribed`. | `broker/hook_queue.go:104`, `broker/server.go:770`, `winccoanative/service.go:619`, `mqtt/server.go:631` |
| E10 | Session expiry deletes the datapoint (`DelClient`) without checking the owner. | `broker/hook_storage.go:264` |

## 3. WP-R1: role manager

### 3.0 Shared contract with the main broker

This contract is word for word the same in `edge/dev/plans/plan-peerlink-redundancy.md` (section 3.0) and `main/dev/plans/plan-peerlink-redundancy.md` (section II.0). Change both or neither. Where the rest of either plan disagrees with this contract, the contract wins. The only intended difference between the brokers: `Source: WINCCOA` exists on the edge broker only (it needs the native WinCC OA link); the main broker rejects it at startup.

**C1 Wire: `CapRole` (`mmq-peer/1`, spec section 3.6).** One wire change for both code bases; accepted for both brokers with the owner-authorized review recommendations (2026-10-09); requires one new golden-vector set (Kotlin and Go).

- Capability bit `CapRole = 1<<4` (after `CapTombstone`). `CapInterest = 1<<5` belongs to the interest-routing plans.
- Role block, 18 bytes, little-endian:

  | Offset | Field | Type | Meaning |
  |---|---|---|---|
  | 0 | `role` | u8 | `UNKNOWN=0`, `ACTIVE=1`, `STANDBY=2`; any other value reads as `UNKNOWN` |
  | 1 | `roleSeq` | u64 | sender's counter, starts at 1 with the process, +1 on each own role change |
  | 9 | `leaseEpoch` | u64 | witness lease epoch the sender acts on; 0 without `WITNESS` |
  | 17 | `flags` | u8 | bit 0 `witnessReachable`, bit 1 `leaseHolder`; other bits sent as 0, ignored on read |

- The block is appended after the last field of `HELLO` (when the consumer offers `CapRole`) and of `HELLO_OK`, `PING` and `PONG` (when `CapRole` is agreed). Decoders ignore trailing bytes, so a peer without `CapRole` reads all four frames unchanged; its role counts as `UNKNOWN`. Data replication is not affected.
- The field is called `leaseEpoch` to keep it apart from the log `epoch` in `HELLO_OK` and in the PeerLink status.
- With `CapRole` agreed:
  - the consumer sends `PING` every `keepAlive/2` also while a `FETCH` is outstanding, and at once after its own role changes;
  - the source answers every `PING` with a `PONG` carrying its role block, and sends an unsolicited `PONG` with token 0 at once after its own role changes. Token 0 is never used for RTT; consumers never send token 0.
- Every build that implements `CapRole` offers it in `SERVER_HELLO` and `HELLO` whatever `Redundancy.Source` is, so a `Source: NONE` node is never seen as `UNKNOWN`.
- `Source: NONE` is sent as `ACTIVE`. `STANDALONE` and the WinCC OA view `PASSIVE` never go on the wire.

**C2 Peer reachability and peer role.**

- A peer is reachable when a `HELLO`, `HELLO_OK`, `PING` or `PONG` from it arrived on any PeerLink connection, in either direction, within the last `PeerTimeoutMs`. Frames without a role block count for reachability too.
- The peer's role is taken from the role block with the highest `roleSeq` received from it; a new `HELLO` or `HELLO_OK` from the peer resets the stored `roleSeq` (the peer may have restarted). An unreachable peer has role `UNKNOWN`.
- `Partner` set: only that peer counts in the role rules (C4). `Partner` empty: every PeerLink peer counts; "peer reachable" means any peer is reachable, "peer `ACTIVE`" means any reachable peer reports `ACTIVE`. On edge with `WINCCOA`, an empty `Partner` defaults to the peer with `RedundancyPartner: true`.

**C3 Timing.**

- `STANDBY → ACTIVE`: at once when the rule says so (peer unreachable for `PeerTimeoutMs`, lease won, own WinCC OA host active, `STATIC` switch).
- `ACTIVE → STANDBY`: after `StandbyGraceMs`, and only if the rule still says `STANDBY` then.
- Fencing, at once without grace: local lease validity expires (except the explicit total-isolation `KEEP` case), lease lost to a higher `leaseEpoch`, a split resolved against this node, or voluntary handover. `StandbyGraceMs` never extends a valid lease.
- `WITNESS`: the preferred node acquires an expired lease at once, the others only after `Witness.TakeoverDelayMs` past expiry. A confirmed voluntary release bypasses that delay. There is no other hold-down.

**C4 Role rules.**

- `NONE` (default): always `ACTIVE`; component modes (C6) have no effect.
- `STATIC` and `WINCCOA` share one table. Own input: `STATIC` = `Static.Role` or its runtime override (C9); `WINCCOA` = own WinCC OA host active → `ACTIVE`, passive → `STANDBY`, unknown → `UNKNOWN`.

  | Own input | Peer reachable | Peer role | Role |
  |---|---|---|---|
  | `ACTIVE` | any | any | `ACTIVE` |
  | `STANDBY` / `UNKNOWN` | yes | `ACTIVE` | `STANDBY` |
  | `STANDBY` / `UNKNOWN` | yes | `STANDBY` / `UNKNOWN` | `ACTIVE` |
  | `STANDBY` / `UNKNOWN` | no | – | `ACTIVE` (fail open) |

  Both `ACTIVE` with the link up (e.g. both WinCC OA hosts active in split mode, or a misconfigured `STATIC` pair): both stay `ACTIVE`, WARN, `split: SPLIT_DUAL_ACTIVE`.
- `WITNESS`: the role table, isolation rule (`OnIsolation`), split cases and resolution are the same in both plans (main II.6.2, II.8; edge 3.4). Resolution order: lease holder, then higher `leaseEpoch`, then lower `Priority`, then lower `NodeId`; the loser goes `STANDBY` at once.
- `ELECTION` is dropped from v1 (owner authorized the review recommendations on 2026-10-09). `WITNESS` covers pairs and larger groups; `ELECTION` is a startup error in both brokers.

**C5 YAML.** Same key set and the same JSON schema in both brokers:

```yaml
Redundancy:
  Source: NONE             # NONE | STATIC | WITNESS | WINCCOA (edge only)
  Partner: ""              # PeerLink NodeId of the other half; empty = all peers
                           # (edge with WINCCOA: the peer with RedundancyPartner: true)
  Priority: 100            # lower = preferred (WITNESS); tie: lower NodeId
  StandbyGraceMs: 5000     # ACTIVE -> STANDBY delay (C3)
  PeerTimeoutMs: 10000     # peer unreachable after this (C2)
  Static:
    Role: ACTIVE           # ACTIVE | STANDBY; required with Source: STATIC
  Witness:
    Group: plant-a         # lease name, same on all nodes of the group; required with WITNESS
    Store: POSTGRES        # POSTGRES | MONGODB
    Connection: default    # default = the broker's Postgres / MongoDB section,
                           # or an own Url/User/Password block
    LeaseTtlMs: 10000
    RenewIntervalMs: 2500
    SafetyMarginMs: 2000
    TakeoverDelayMs: 3000
    Failback: false
    OnIsolation: STANDBY   # STANDBY | KEEP
```

- Keys outside this block are a schema error in both brokers. A key or value that a broker cannot act on is accepted with one WARN at start, except `Source: WINCCOA` on main, which is a startup error (otherwise the broker would run without a role source).
- Validation, same in both:
  - `Source != NONE` without PeerLink: WARN (no peer fallback, no split detection over the link);
  - `STATIC` without `Static.Role`: startup error;
  - `Partner` names no configured PeerLink peer: WARN;
  - `WITNESS` without `Witness.Group`, or with `SafetyMarginMs >= LeaseTtlMs` or `RenewIntervalMs > LeaseTtlMs / 2`: startup error;
  - `WITNESS` with `Connection: default` whose store host is this node: WARN (witness not independent);
  - `Priority` outside 0..2147483647, non-positive lease/renew/peer timeout, negative safety margin/takeover delay/standby grace: startup error;
  - `Source: ELECTION`: startup error (dropped from v1).
- `Witness.Connection` is either the string `default`, or `{Url: <connection URL>, User: <optional username>, Password: <optional password>}` for the selected store; unknown keys fail validation. Group is a non-empty, case-sensitive string used verbatim in both stores; NodeId follows PeerLink canonicalization. Do not apply hostname or lowercase normalization to Group.

**C6 Component setting `Redundancy`.** On every device / bridge config, every archive group and every script:

```text
Redundancy: ALWAYS | HOT_STANDBY | COLD_STANDBY      (default ALWAYS)
```

| Mode | On `ACTIVE` | On `STANDBY` |
|---|---|---|
| `ALWAYS` | runs (today's behaviour) | runs (today's behaviour) |
| `HOT_STANDBY` | runs; inbound is published; outbound sends **all** messages, local and replicas | runs and stays connected; inbound is discarded; nothing is sent outbound; device writes rejected with a log line; scripts run with outputs suppressed; archive groups stay connected and drop writes (the last-value store keeps being fed) |
| `COLD_STANDBY` | as `HOT_STANDBY` | not started, no connection (archive groups: writer and DB connection stopped, last-value store kept); the persisted `enabled` flag is not changed |

- Bidirectional bridges apply the mode to both directions together.
- Server-type components (listeners) offer only `ALWAYS` and `COLD_STANDBY`.
- With `Source: NONE` every mode behaves like `ALWAYS`.
- `HOT_STANDBY` / `COLD_STANDBY`: `PeerLink.Receive.BridgeOutbound` is ignored; the role decides.
- `ALWAYS`: the `BridgeOutbound` guard applies as today (deprecated, one WARN when set with `Source != NONE`). One WARN at start for every outbound bridge left at `ALWAYS` while `Source != NONE`.
- Interest routing (`CapInterest`): the filters of `HOT_STANDBY` and `COLD_STANDBY` components are announced to the peers whatever the role and `BridgeOutbound`, also while a `COLD_STANDBY` component is not running, so a switchover never waits for an interest update.

**C7 Status, metrics, log.**

- `$SYS/broker/redundancy/role`, retained, published on every change by both brokers (edge also in native mode):
  `{role, source, epoch, holder, witnessReachable, peers: [{nodeId, reachable, role}], split, since, reason}`.
  `role`: `ACTIVE` | `STANDBY` | `UNKNOWN` (`NONE` reports `ACTIVE`); `source`: the YAML value; `epoch`: `leaseEpoch`, 0 without `WITNESS`; `holder`: lease holder `NodeId` or `null`; `witnessReachable`: `null` without `WITNESS`; `split`: `NONE` | `SPLIT_LINK` | `SPLIT_DUAL_ACTIVE`; `since`: ISO-8601 UTC with milliseconds; `reason`: text of the last change.
- Metrics: `redundancy_role` (0 `UNKNOWN`, 1 `ACTIVE`, 2 `STANDBY`), `redundancy_epoch`, `redundancy_split` (0 / 1), `redundancy_lease_renew_failures_total`, `redundancy_role_changes_total`.
- PeerLink status JSON (spec section 12.1): every entry of `consumers[]` and `sources[]` gets `role`, `roleSeq`, `leaseEpoch`, `witnessReachable`, `leaseHolder` of that peer (`"UNKNOWN"`, 0, `false` without `CapRole`). The existing `epoch` there stays the log epoch.
- Log: one INFO line per role change with the reason; WARN on split entry and exit.

**C8 Witness storage.** One layout, so edge and main nodes can share one witness group.

- Postgres: tables `redundancylease` and `redundancynodes` exactly as main II.5.1, acquire / renew statement exactly as main II.5.2 (edge 3.4 repeats it). `role` holds `ACTIVE` | `STANDBY` | `UNKNOWN`. Lease `released` is Boolean, default false; heartbeat `priority` is integer, default 100.
- MongoDB, field names and types fixed:
  - `redundancylease`: `{_id: <group>, holder: <NodeId>, epoch: Int64, expiresAt: Date, updatedAt: Date, released: Boolean}`;
  - `redundancynodes`: `{_id: {group: <group>, nodeId: <NodeId>}, role: "ACTIVE" | "STANDBY" | "UNKNOWN", epoch: Int64, linkUp: Boolean, priority: Int32, version: String, heartbeatAt: Date}`.
- Expiry uses the store clock only (`now()` / `$$NOW`); nodes measure durations on their monotonic clock.
- Priority is learned from witness heartbeat rows, not from the 18-byte role block. Fresh means age no greater than `PeerTimeoutMs`, measured with the store clock. Cache the last known priorities for resolution during witness loss; if either priority is unknown, use lower NodeId for that tie-break. The preferred candidate is the lowest `(priority, NodeId)` among self and known group members that are configured PeerLink peers. Retain their last recorded priority even when their heartbeat becomes stale, so a dead preferred node does not remove the non-preferred takeover delay. Ignore rows for peers removed from configuration. Before a peer has ever reported its priority, first-winner startup applies; freshness is required for failback availability, not for priority ranking. A simultaneous first start may elect whichever node acquires first; `Failback: false` does not promise preferred ownership on a cold start.
- `released: true` distinguishes voluntary handover from timeout. Acquire/renew sets it false. Release matches group, holder and epoch, sets it true and expiry to store-now, and keeps the row/epoch. Quiesce component outputs and complete in-flight external operations before release; if quiescence cannot be confirmed, do not release early. A releasing node must not reacquire during shutdown or while a fresh preferred candidate is available for failback.
- Lease validity is checked before each external operation, including after a process pause, and again after every acquire/renew response. A response received after its local deadline cannot enable outputs. Timers alone are insufficient; cold components are also gated while stopping. Operations already sent cannot be fenced by a broker role change, so strict exactly-once side effects across failover are not promised.

**C9 GraphQL.** Same SDL in both brokers (both schemas already have `scalar Long`; timestamps are `String`). Needs human commitment per AGENTS.md in both repositories.

```graphql
enum RedundancyRole { UNKNOWN ACTIVE STANDBY }
enum RedundancySource { NONE STATIC WITNESS WINCCOA }
enum RedundancySplit { NONE SPLIT_LINK SPLIT_DUAL_ACTIVE }
enum ComponentRedundancy { ALWAYS HOT_STANDBY COLD_STANDBY }

type RedundancyPeer {
    nodeId: String!
    reachable: Boolean!
    role: RedundancyRole!
}

type BrokerRedundancy {
    role: RedundancyRole!
    source: RedundancySource!
    epoch: Long!
    holder: String
    witnessReachable: Boolean
    split: RedundancySplit!
    peers: [RedundancyPeer!]!
    since: String!
    reason: String
}

# added to type Query
brokerRedundancy: BrokerRedundancy!

# added to type Mutation (top level in both brokers)
setRedundancyRole(role: RedundancyRole!): BrokerRedundancy!
```

- `setRedundancyRole` is valid only with `Source: STATIC` and `role` `ACTIVE` or `STANDBY`; otherwise a GraphQL error. It overrides `Static.Role` at runtime, is not persisted, and is lost on restart.
- Per component: `redundancy: ComponentRedundancy` on every device / bridge, archive group and script input (optional, default `ALWAYS`) and output (non-null), next to `nodeId`.

Implementation order is coordinated with the interest plan's "Implementation readiness and cross-plan order" section. Role/component core and configured-filter providers are implemented before interest integration; GraphQL and all dashboard work ship only in main R7 / edge PG after the separate commitment gate. This plan revision accepts the review recommendations; it does not authorize GraphQL implementation or claim native-pair verification.

### 3.1 Role sources

`Redundancy.Source` in the broker config (all keys: C5):

| Source | Case | Own role input |
|---|---|---|
| `NONE` (default) | 1, 2, 4, 5 | none; always `ACTIVE`, component modes have no effect (C4, C6) |
| `STATIC` | 3 | `Static.Role: ACTIVE \| STANDBY`, runtime override with `setRedundancyRole` (C9) |
| `WINCCOA` | 6 | E1: own host active / passive / unknown (edge only) |
| `WITNESS` | 3 | lease in an external Postgres / MongoDB (3.4) |
| `ELECTION` | – | Dropped from v1; startup error; use `WITNESS` |

`Redundancy.Partner` (C2) names the PeerLink peer (`NodeId`) that is the other
half of the pair. With `WINCCOA` it defaults to the peer with
`RedundancyPartner: true`; with the other sources an empty `Partner` means all
peers.

Mixed meshes: when the mesh has more PeerLink peers than the two redundant
nodes (for example main brokers on `Source: NONE`, which report `ACTIVE`, C1),
`Partner` must be set. Otherwise the `NONE` node counts as an `ACTIVE` peer
(C2) and both nodes of the pair go `STANDBY`. Validation: one WARN at start
when `Source` is `STATIC` or `WINCCOA`, `Partner` is empty and more than one
PeerLink peer is configured. With `WINCCOA` the default from
`RedundancyPartner: true` counts as set.

### 3.2 Role rule (`STATIC`, `WINCCOA`)

The table of C4, evaluated on every change of the own input, of a peer's
reachability (C2) or of a peer's role. Timing as C3: a change to `ACTIVE`
applies at once (also when caused by the WinCC OA input), a change to
`STANDBY` after `StandbyGraceMs`.

For `WINCCOA` the own input comes from E1: `_ReduManager[_2].Status.Active` of
the own host → `ACTIVE`, the other host active → `STANDBY`, no information →
`UNKNOWN`. A WinCC OA split (both hosts active) gives two `ACTIVE` brokers and
`split: SPLIT_DUAL_ACTIVE` (C4); duplicates beat loss.

### 3.3 Interface

- New package `internal/redundancy`: `Manager` with `Role()`, `IsActive()`
  (one atomic load, called per message), `Subscribe(func(Role))` and
  `Acts(mode ComponentRedundancy) bool` (`false` for `HOT_STANDBY` /
  `COLD_STANDBY` whenever role is not `ACTIVE`, including startup `UNKNOWN`; always `true` with `Source: NONE`), used by
  WP-R2. est. 300 lines.
- Inputs: WinCC OA state from `winccoanative` (E1, through a small callback,
  no import cycle), per peer the C2 reachability and the `CapRole` block from
  `peerlink` (consumer and source side).
- Outputs:
  - `$SYS/broker/redundancy/role` (retained) and the metrics of C7, in every
    mode including native mode.
  - Native broker status (E1): `role` keeps the WinCC OA view (`STANDALONE`,
    `UNKNOWN`, `ACTIVE`, `PASSIVE`, unchanged for existing clients); new field
    `brokerRole` with the role of this manager (Q-R2 closed).
  - PeerLink status: per-peer fields of C7.
  - GraphQL: `brokerRedundancy` and `setRedundancyRole` of C9 in
    `internal/graphql`, only after the commitment gate (phase PG, section 8).
  - Log: one INFO line per change with the reason; WARN on split entry and
    exit.

Own role is UNKNOWN during initial role-source setup; HOT/COLD components must not act then. With WITNESS the first bounded attempt chooses ACTIVE only for a valid won lease, otherwise STANDBY (KEEP does not activate a node that has never held a lease). NONE/STATIC decide immediately. This mirrors main II.10.

### 3.4 Role source `WITNESS` (case 3, no WinCC OA)

Same design and same tables as the main broker (main plan II.5, II.6.2, II.8;
storage C8), so an edge and a Kotlin node can form one pair with one witness.
Keys: the `Witness` block of C5, plus the top-level `Priority`,
`StandbyGraceMs`, `PeerTimeoutMs`:

```yaml
Redundancy:
  Source: WITNESS
  Priority: 10             # lower = preferred; tie: lower NodeId
  StandbyGraceMs: 5000
  PeerTimeoutMs: 10000
  Witness:
    Group: plant-a         # lease name, same on all nodes of the pair
    Store: POSTGRES        # POSTGRES | MONGODB
    Connection: default    # default = the broker's Postgres / MongoDB section,
                           # or an own Url/User/Password block
    LeaseTtlMs: 10000
    RenewIntervalMs: 2500
    SafetyMarginMs: 2000
    TakeoverDelayMs: 3000
    Failback: false
    OnIsolation: STANDBY   # STANDBY | KEEP
```

**Lease.** Tables `redundancylease(groupname PK, holder, epoch, expiresat,
updatedat)` and `redundancynodes(groupname, nodeid, role, epoch, linkup,
version, heartbeatat, PK(groupname, nodeid))` with the DDL of main II.5.1,
created at start (`CREATE TABLE IF NOT EXISTS`). Acquire and renew in one
statement:

Acquire/renew uses the exact Postgres statement and atomic delay predicates of main II.5.2. MongoDB uses the same predicates with `$$NOW`. C8 is normative for `released`, priority discovery, heartbeat freshness and epoch increments, including reacquisition by the same NodeId.

- Local deadline: `tSend + LeaseTtlMs - SafetyMarginMs`. Check it before each external operation and after each response, including after process resume. Expiry fences at once without grace except explicit total-isolation `KEEP`; operations already sent cannot be fenced by this gate.
- First acquisition on an empty group is first-winner, not guaranteed preferred. Thereafter preference is lowest `(priority, NodeId)` among self and known configured group members (C8), retaining stale priorities for delay selection. Non-preferred acquisition waits the takeover delay unless the lease was voluntarily released.
- Shutdown/failback: fence new outputs and finish in-flight external operations before conditional holder/epoch release. Set `released: true` and expiry to store-now; preserve epoch. Keep PeerLink running during quiescence. Do not release early if quiescence cannot be confirmed, and do not reacquire during handover. Confirmed voluntary release bypasses takeover delay; the new acquisition increments epoch.
- Every renew cycle upserts role, epoch, `linkup`, priority, version and store-time heartbeat. During witness loss, use cached priorities for split resolution; if either is unknown, use NodeId.

**Role rule:**

| Witness | Lease | PeerLink to partner | Role |
|---|---|---|---|
| reachable | I hold it | any | `ACTIVE` |
| reachable | partner holds it | any | `STANDBY` |
| reachable | expired / none | any | try acquire; result decides |
| unreachable | I hold it, local validity left | up | `ACTIVE` |
| unreachable | – | up, partner `ACTIVE` | `STANDBY` |
| unreachable | no valid local lease | up, partner not `ACTIVE` | `STANDBY`; neither node may act without a valid lease |
| unreachable | – | down | `OnIsolation`: `STANDBY` (no dual-active, loss possible) or `KEEP` (duplicates, no loss) |

**Split mode:**

| Case | Seen by | Result |
|---|---|---|
| link down, both reach the witness | store: both heartbeats fresh, `linkup = false` | one lease holder, one `ACTIVE`; status `SPLIT_LINK` |
| link down, one node isolated | the node with the witness | witness side runs; isolated node per `OnIsolation` |
| two `ACTIVE` rows in the store, no link | store | worst case (`KEEP`, stale lease); status `SPLIT_DUAL_ACTIVE`, WARN |
| link up, both claim `ACTIVE` | `PING` / `PONG` role block (C1) | resolved at once |

Resolution: lease holder wins, then higher `leaseEpoch`, then lower
`Priority`, then lower `NodeId`; the loser goes `STANDBY` at once (fencing, no
grace). Duplicates while split are accepted; PeerLink resync fills the gaps
after the link returns.

**Code.** `internal/redundancy/witness.go` (interface `Lease`:
`AcquireOrRenew`, `Release`, `Read`, `Heartbeat`, `Nodes`),
`witness_postgres.go` and `witness_mongodb.go` on the existing drivers of
`internal/stores/postgres` and `internal/stores/mongodb`. est. 400 lines.
Validation: C5.

**Status:** `split`, `epoch`, `holder`, `witnessReachable` in
`$SYS/broker/redundancy/role` and the metrics of C7.

## 4. WP-R2: component setting `Redundancy`

### 4.1 Configuration

The per-component field of C6, `Redundancy: ALWAYS | HOT_STANDBY |
COLD_STANDBY` (default `ALWAYS`), next to `nodeId` in the stored config of each
component, and the `redundancy` field of C9 in its GraphQL input and output
(after the commitment gate, phase PG). No `ActiveOnly` lists and no
per-type defaults (D6 closed).

Example: an outbound bridge and an archive group on a shared DB that act only
on the `ACTIVE` node, an OPC-style reader that stays connected on the standby:

```yaml
# device / archive group configs (stored config; GraphQL and dashboard after PG)
to-cloud:     { type: MQTT_CLIENT, nodeId: "*", redundancy: HOT_STANDBY }
central:      { archiveGroup: true, nodeId: "*", redundancy: COLD_STANDBY }
plc-reader:   { type: WINCCUA, nodeId: "*", redundancy: HOT_STANDBY }
```

The example shows stored component configs, so the field is camelCase
`redundancy` (stored config and GraphQL, next to `nodeId`). In YAML component
blocks the key is `Redundancy` (C6).

### 4.2 One shared check

`redundancy.Manager.Acts(mode)` and the role subscription are the only
decision points:

| Component | Where the check goes | `HOT_STANDBY` on `STANDBY` | `COLD_STANDBY` on `STANDBY` |
|---|---|---|---|
| `mqttclient` (both directions) | `BusAdapter.forwards` (E4) and the inbound publish path | stays connected; forwards nothing (also no local publishes), discards what it reads; on `ACTIVE` forwards local publishes **and** replicas | disconnected; connects on `ACTIVE` |
| `winccua`, `winccoa` devices, cameras, redfish | publish path of the device | stays connected, discards what it reads (C6 `HOT_STANDBY`); the active node publishes, PeerLink carries it | not started |
| device that writes to a field system | device write path | write rejected with a log line | not started |
| archive group | `archive/group.go` write path | DB connected, no write; the in-memory last value is kept | writer and DB connection stopped; last value kept |
| script | `scripting/manager.go` | runs, outputs suppressed | not started; started on `ACTIVE`, stopped on `STANDBY` |
| server-type (listeners) | listener start | not offered | not listening |

A role change re-evaluates all components at once (subscription from
`Manager.Subscribe`). No reconnects for `ALWAYS` or `HOT_STANDBY` components.
`COLD_STANDBY` start / stop reuses the enable / disable path; the stored
`enabled` flag is not changed and the dashboard shows "Standby (cold)".

The filters of `HOT_STANDBY` and `COLD_STANDBY` components are announced to
the peers whatever the role (C6, interest-routing plan section 6).

### 4.3 Replaces `Receive.BridgeOutbound`

- `HOT_STANDBY` / `COLD_STANDBY`: `Receive.BridgeOutbound` is ignored; the
  role decides, and the `ACTIVE` node forwards replicas.
- `ALWAYS`: unchanged; `BridgeOutbound` stays as deprecated alias (D3), with
  one WARN when set together with `Source != NONE`.
- One WARN at start for each outbound `mqttclient` left at `ALWAYS` while
  `Source != NONE` (D2).

### 4.4 Without a role source (G2.2)

With `Source: NONE` every component acts (C4). An archive group on a shared DB
in a mesh is placed on one node with `nodeId` (that node's `NodeId`, not
`local` / `*`); the other nodes do not run it. This replaces the manual
`Receive.Archive: false` on all but one node and needs no role source.

### 4.5 Archive groups and `Receive.Archive`

Same rule as main II.11.2:

| Setup | Archive groups | Inbound bridges | `Receive.Archive` |
|---|---|---|---|
| Central HA database | `COLD_STANDBY` (or `HOT_STANDBY`) | `HOT_STANDBY` or `COLD_STANDBY` | `true` |
| DB per node (SQLite) | `ALWAYS` | `HOT_STANDBY` or `COLD_STANDBY` | `true` |

In both setups the active broker archives the replicas it got from its peer
(clients connected to the standby broker still publish there). So
`Receive.Archive: false` is only needed for unusual setups; the docs say so
(P7). AC-05 relies on this rule.

## 5. WP-W1: startup WARN for case 5 and 6

- When `SYS_INFO` reports `redundant: true` and the manager is not connected to
  both Event Managers (`connectToRedundantHosts` off), log one WARN at start:
  writes on the passive host are discarded by WinCC OA.
- Detection: C++ reports the option (`Resources` / command line) as new TLV
  tag `TagRedConn` = 19 in `OpSysInfo` (spec 14.2); older hosts omit it and no
  WARN is logged. est. 0.5 d.
- Also add the result to the broker status as `connectToRedundantHosts: bool`.
- Writes on the passive host without the option: no reject logic (would need
  verification on a pair, 10.2); the WARN and the README are the mitigation.

## 6. WP-NS: node status topics (case 6, G6.4)

### 6.1 Topics

```text
winccoa/systems/<System>/node/1    redundancy state of host 1 (retained JSON)
winccoa/systems/<System>/node/2    redundancy state of host 2 (retained JSON)
winccoa/node/1, winccoa/node/2     same, local-system shortcut
```

- `node` becomes a reserved level next to `tags`, `types`, `cns` (and below the
  root next to `systems` for the shortcut). Subscribe allowed with ACL;
  external publishes rejected; `node/+` is an ordinary MQTT filter, no OA
  registration.
- Keyed by host number (stable, matches `_ReduManager` / `_ReduManager_2`).
  Q-NS1: level name `node` or `redundancy`.
- Not redundant: no node topics (Q-NS2: or `node/1` with `active: true`).

### 6.2 Payload

```json
{
  "host": 1, "hostName": "scada-a", "active": true,
  "eventManagerActive": true, "errorStatus": 0, "peerErrorStatus": 0,
  "preferred": true, "manual": false, "splitMode": false, "splitActive": false,
  "recovering": false, "peerAlive": true,
  "peerLastAlive": "2026-09-30T12:00:00.000Z",
  "broker": true, "brokerRole": "ACTIVE",
  "timestamp": "2026-09-30T12:00:00.123Z"
}
```

- Values from `_ReduManager[_2]`: `Status.Active`, `EvStatus`, `MyErrorStatus`,
  `PeerErrorStatus`, `Status.Preferred`, `Status.Manual`, `SplitMode`,
  `SplitActive`, `IsRecovering`, `PeerAlive.Link0`, `PeerAlive.LastAliveTime`.
  Elements whose meaning is inferred from the name are published raw and
  confirmed on a pair (10.2).
- `broker` marks the host this broker runs on; `brokerRole` is the WP-R1 role
  of the broker on that host (own host: local; other host: partner role from
  3.2, omitted if unknown).
- Retained, QoS 1, on change only.
- Dropped from the old plan as not needed by a case: `maxErrorStatus`,
  `missingManagers`, `ErrorChangeReason`, `eventConnections` and the C++
  `handleManagerUpdate`/`doRefresh` events. The role manager uses PeerLink
  reachability, not the OA connection state.

### 6.3 Implementation

- Go only: `winccoanative/redu.go` connects the listed elements of both
  datapoints with the existing batched `dpConnect`, only when `redundant`.
  Internal datapoints stay hidden from `tags/`/`types/`.
- Simulator: model `_ReduManager`/`_ReduManager_2` and `redundant`/`replica`
  in `SYS_INFO`.

## 7. WP-SO: session ownership on shared `MMQSessions` (case 6, G6.1)

### 7.1 Problem

Client C has a persistent session, connects to A, disconnects, reconnects to B
(host failure in case 6):

1. B does not know C, or knows it as it was at B's start (E7): `SessionPresent=0`
   or stale subscriptions.
2. C's queued messages sit in A's memory queue (E8); B queued nothing (E9).
3. A never learns that C moved; it keeps queuing until expiry.
4. When A's copy expires, A deletes the shared datapoint although C is on B (E10).
5. When C returns to A, A uses its old queue and old subscriptions.
6. A crash leaves `connected=true`, `nodeId=A`.
7. A stale TCP connection on A writes `connected=false` over B's `connected=true`
   when its keep-alive expires.

### 7.2 Goal

The brokers sharing `MMQSessions` behave like one broker for ownership: every
broker sees each session's current subscriptions, owner and connected flag; at
most one broker holds it connected (a connect elsewhere takes it over with
`0x8E`); while it is offline everywhere, every broker with PeerLink to the
others queues for it; only the owner deletes it.

On automatically with `SessionStoreType: WINCCOA` embedded in WinCC OA (Q-SO1).
If the query connect fails: one WARN, P0 behaviour.

### 7.3 S1: session watch

- After `load`, one query connect
  `SELECT '_original.._value', '_original.._stime' FROM 'MMQSessions_*.{session,connected,nodeId}' WHERE _DPT = "MMQSessions"`
  (`oahost.API.QueryConnect`). No gap between load and connect: build the cache
  from the initial answer if it stays within the 1 MiB answer limit, otherwise
  connect first, enumerate, then apply buffered hotlinks.
- Created datapoints are reported by the query connect (Q-SO5). If deletes are
  not reported, the owner writes a tombstone (`data: null`, `connected=false`)
  before `dpDelete`.
- Per hotlink: decode (corrupt → `broken`, as `load`); order by `_stime` of
  `session`, older than the cache → ignored (own echoes change nothing, Q-SO6);
  replace the cache entry; hand the old/new pair to S2; apply the filter
  difference to the subscription index and to the WinCC OA namespace
  interests. The engine's topic index is not touched for clients not connected
  locally; resume reads the current cache.
- One goroutine per `recordSet`; `wmu` held only for the cache swap.
- On an OA reconnect (switchover with `connectToRedundantHosts`): re-establish
  the connect, reconcile the cache, re-evaluate all sessions.

### 7.4 S2: ownership tracker

| State on X | Record | Local engine | X queues |
|---|---|---|---|
| `LOCAL_ONLINE` | `nodeId=X`, `connected=true` | connected | – |
| `LOCAL_OFFLINE` | `nodeId=X`, `connected=false` | not connected | yes |
| `REMOTE_ONLINE` | `nodeId=Y`, `connected=true` | not connected | no |
| `REMOTE_OFFLINE` | `nodeId=Y`, `connected=false` | not connected | yes, with dual queuing (7.5) |
| `REMOTE_DEAD` | `nodeId=Y`, `connected=true`, Y down | not connected | yes; writes nothing to OA |

| Trigger on X | Action |
|---|---|
| Hotlink `nodeId=Y`, `connected=true`, client connected on X, hotlink `stime` later than X's own connect write | Takeover: disconnect with `0x8E` (v3.1.1: close), then next row |
| Hotlink `nodeId=Y`, `connected=true`, not connected on X | Purge queue, inflight, pending acks; remove from `offline`; drop the engine copy without `OnClientExpired` |
| Hotlink `nodeId=Y`, `connected=false` | `REMOTE_OFFLINE`; add to `offline` if dual queuing |
| Local connect | As today: write `nodeId=X`, `connected=true`, deliver X's queue |
| Local disconnect | Write `connected=false` only if the cache still has `nodeId=X` |
| Tombstone / deleted | Forget: `persistent`, `offline`, index, queue |

Clean sessions are tracked only for takeover. Code: `sessionOwnership` in
`internal/broker` (est. 300 lines), listener on the session watch, small queue
hook methods (`markRemoteOffline`, `markRemoteOnline`, `forget`) and new engine
hooks `Server.TakeOver(id)`, `Server.DropOffline(id)` (E9 in the engine-change
list, Q-SO7).

**Down node (`REMOTE_DEAD`):** Y is down when the WP-R1 manager reports the
partner unreachable **and** WinCC OA reports Y's host not active (Q-SO3).
This reuses the role manager's partner state instead of a separate detector.

### 7.5 S3: queuing on both nodes

- On for nodes with PeerLink to the owner and `Receive.Queue: true`. No dual queue without PeerLink (Q-SO2).
- B queues for a `REMOTE_OFFLINE` C: local publishes, PeerLink replicas
  (`hook_queue.go:153`), WinCC OA namespace values from B's own native service
  (not in the PeerLink log, so no doubles).
- E9 changes from "`nodeId` is this node" to "connected nowhere" while the
  watch is active; unchanged for SQL/Mongo stores.
- Max queue size per node, oldest dropped.

### 7.6 S4/S5: write and delete rules

- `SetConnected(false)`, `SetLastWill`, `Add/DelSubscriptions` for a client not
  connected locally are skipped when the cache has `nodeId≠X` (problem 7).
  `SetClient` on a local connect always writes.
- `OnClientExpired` and the clean-start purge delete only if `nodeId=X`, after
  a fresh `DpGet` of `nodeId`/`connected` (problem 4).
- At start, X sets every record with `nodeId=X`, `connected=true` to
  `connected=false` (problem 6; also what case 5b needs).

### 7.7 Delivery after a move

At-least-once for QoS 1/2 if the client resumes on a node that queued.
Duplicates and new packet IDs after a move; QoS 2 exactly-once not kept across
nodes (Q-SO4). Order kept per publisher, not across publishers on different
nodes. Retained unaffected.

Status counters in the broker status JSON (no GraphQL): `sessionWatch`,
`sessionTakeovers`, `sessionsDiscarded`, `sessionHotlinks`, `sessionWatchErrors`.

## 8. Phases

Ordered by what unblocks a case first.

| Phase | Content | Cases | Est. |
|---|---|---|---|
| P0 | WP-W1 startup WARN; WP-SO S4/S5 rules (delete with fresh `DpGet`, crash leftover reset, skip foreign `SetConnected(false)`) | 5, 5b, 6 | 2.5 d |
| P1 | WP-R1 role manager with `NONE`, `STATIC`, `WINCCOA`; `CapRole` over PeerLink (C1, golden vectors shared with main); `$SYS/broker/redundancy/role`, metrics, PeerLink status fields, native status `brokerRole` (C7) | 3, 6 | 1.5 w |
| P2 | WP-R2 `Redundancy` setting (C6) for `mqttclient` (both directions), archive groups, devices, scripts; YAML and stored config; `BridgeOutbound` alias | 2, 3, 6 | 1.5 w |
| PG | GraphQL (C9): `brokerRedundancy`, `setRedundancyRole`, per-component `redundancy` field and the dashboard that uses it. Gated: only after the human commitment (AGENTS.md) in both repositories; ships together with main (main R7) | 3 | 3 d (est.) |
| P3 | WP-NS node topics | 6 | 3 d |
| P4 | WP-SO S1 session watch | 6 | 1 w |
| P5 | WP-SO S2 tracker, engine hooks, S3 dual queuing, `REMOTE_DEAD` | 6 | 1.5 w |
| P6 | WP-R1 source `WITNESS` (Postgres, then MongoDB), split detection and resolution (3.4) | 3 | 1.5 w |
| P7 | Docs: spec-winccoa-native section 5, spec-peerlink-redundancy section 14, README, use-case document "works today" rows | all | 1 d |

P0 ships alone. P1 and P2 belong together (P1 alone is status only). P4 alone
already fixes problems 1 and 5 of 7.1. PG is not part of P1 or P2; until the
gate is passed, `STATIC` has no runtime override (`Static.Role` only) and the
component setting is set in the YAML / stored config only.

## 9. Tests and acceptance criteria

Unit and integration tests use `oahost/simhost` (two brokers on one simhost
for WP-SO; extend for query connect with deletes, Q-SO5) and two in-process
brokers with PeerLink for WP-R1/R2.

| Id | Criterion | Case |
|---|---|---|
| AC-01 | `Source: NONE`: behaviour unchanged, every `Redundancy` mode acts as `ALWAYS`; an archive group with `nodeId` = one node writes only there | 1, 2 |
| AC-02 | `STATIC` pair: one `ACTIVE`, one `STANDBY`; stop the active → standby becomes `ACTIVE` once the peer is unreachable (`PeerTimeoutMs`, C2); restart → the restarted node is `ACTIVE` (`Static.Role`), the other returns to `STANDBY` after `StandbyGraceMs` | 3 |
| AC-03 | Outbound `mqttclient` with `Redundancy: HOT_STANDBY` (and again with `COLD_STANDBY`) on both nodes: every message (local and replica) forwarded once in steady state; a controlled, drained switchover preserves this; abrupt failover has no exactly-once side-effect guarantee | 3, 6 |
| AC-04 | Inbound bridge on both nodes: standby connected, publishes nothing; after a switchover the new active publishes without reconnect | 3, 6 |
| AC-05 | Archive group with `Redundancy: COLD_STANDBY` on a shared DB: one row per message in steady state and a controlled, drained switchover; abrupt failover may lose/duplicate outstanding writes | 2, 3, 6 |
| AC-06a | `WINCCOA` source, fail open: role follows `_ReduManager[_2].Status.Active`; both WinCC OA hosts passive (own input `STANDBY`) and partner unreachable → both `ACTIVE`, logged | 6 |
| AC-06b | `WINCCOA` source, dual-active split: both WinCC OA hosts active with the link up → both stay `ACTIVE`, WARN, `split: SPLIT_DUAL_ACTIVE` in `$SYS/broker/redundancy/role` and `redundancy_split` = 1 (C4, C7) | 6 |
| AC-07 | WP-W1: redundant system without `connectToRedundantHosts` → one WARN, status `false` | 5, 6 |
| AC-08 | Non-redundant: no `node/...` topics, no connects to `_ReduManager*` | 4 |
| AC-09 | Redundant: `node/1`, `node/2` retained, current within 1 s, `broker` marks own host; not writable by clients, ACL respected | 6 |
| AC-10 | Session move with queue (v5 and v3.1.1): C reconnects on B with `SessionPresent=1`, same subscriptions, every message (duplicates allowed); A's queue empty afterwards | 6 |
| AC-11 | Takeover of a live connection: A closes with `0x8E`; record `nodeId=B`, `connected=true`; a late echo does not kick C | 6 |
| AC-12 | Expiry on the non-owner keeps the datapoint; crash leftover reset at start; down owner → other node queues | 5b, 6 |
| AC-13 | OA reconnect (switchover with `connectToRedundantHosts`): cache reconciled, states re-evaluated | 6 |
| AC-14 | Load: 10,000 sessions, cache build < 5 s, < 50 MiB (est.) | 6 |
| AC-15 | All of case 6 verified on a real redundant pair, including a switchover (10.2) | 6 |
| AC-16 | `WITNESS` pair: sequential start preferred node first → `ACTIVE` epoch 1; simultaneous cold start → one first-winner holder; kill it → partner `ACTIVE` after TTL + `TakeoverDelayMs`, epoch 2; graceful stop → takeover within one renew interval | 3 |
| AC-17 | `WITNESS`: block PeerLink only → roles unchanged, `SPLIT_LINK`; block witness only with link up → holder fences when local validity expires, both STANDBY until witness recovery | 3 |
| AC-18 | `WITNESS`: isolate the active node, `OnIsolation: STANDBY` → never two `ACTIVE`; `KEEP` → `SPLIT_DUAL_ACTIVE`, after reconnect the lower epoch steps down | 3 |
| AC-19 | `WITNESS`: pause the active node longer than the TTL → on resume no external operation starts before the local deadline check, even before renew/PONG; ±5 s clock skew → no overlap | 3 |
| AC-20 | Mixed pair edge + Kotlin main on one witness group: same roles and status as two edge nodes | 3 |
| AC-21 | `CapRole` golden vectors (C1): `HELLO`, `HELLO_OK`, `PING`, `PONG` with and without the role block decode the same in Go and Kotlin; a peer without `CapRole` keeps working | 3, 6 |
| AC-22 | Mixed pair edge + Kotlin main with `STATIC`: role change on one side seen by the other within one `PING` interval (also while a `FETCH` is outstanding) | 3 |
| AC-23 | Once the PG gate is passed, GraphQL (C9): `brokerRedundancy` shows the same values as `$SYS/broker/redundancy/role`; `setRedundancyRole` works with `STATIC`, errors with other sources, is lost on restart; the `redundancy` field round-trips on device, bridge, archive group and script configs; same schema text as main | 3 |
| AC-24 | C5 validation: a key outside the C5 block → schema error; `STATIC` without `Static.Role` → startup error; `WITNESS` without `Witness.Group`, with `SafetyMarginMs >= LeaseTtlMs` or with `RenewIntervalMs > LeaseTtlMs / 2` → startup error; one WARN each for `Source != NONE` without PeerLink, `Partner` not a configured peer, `WITNESS` with `Connection: default` on this node | 3, 6 |
| AC-25 | C6 WARNs: `Receive.BridgeOutbound` set with `Source != NONE` → one deprecation WARN; each outbound bridge at `ALWAYS` with `Source != NONE` → one WARN; none of these with `Source: NONE` | 3, 6 |
| AC-26 | C7 PeerLink status: each `consumers[]` and `sources[]` entry has `role`, `roleSeq`, `leaseEpoch`, `witnessReachable`, `leaseHolder` of that peer; a peer without `CapRole` shows `"UNKNOWN"`, 0, `false`; the log `epoch` is unchanged | 3, 6 |
| AC-27 | Device that writes to a field system with `Redundancy: HOT_STANDBY` on the `STANDBY` node: write rejected, one log line, nothing reaches the field system; after a switch to `ACTIVE` writes go through | 3, 6 |
| AC-28 | `Partner` WARN (3.1): `Source: STATIC` with `Partner` empty and two PeerLink peers → one WARN at start; with `Partner` set, or one peer only → no WARN | 3, 6 |
| AC-29 | Mixed mesh: edge pair (`STATIC` or `WINCCOA`, `Partner` set to each other) plus a main node on `Source: NONE` in the same PeerLink mesh: the pair keeps one `ACTIVE` and one `STANDBY`; the main node is listed in `peers` as `ACTIVE` and does not change the pair's roles | 3, 6 |
| AC-30 | `WITNESS` with `Failback: true`: the preferred node returns → the holder begins quiescence at the next renew once the preferred node's heartbeat row is fresh and its link up, then conditionally releases; the preferred node takes the lease with a higher epoch; never two `ACTIVE` | 3 |

## 10. Open points

### 10.1 Decisions (from the use-case document and the merged plans)

- **D1:** closed (owner authorized review recommendations, 2026-10-09): use `WITNESS`; keep `STATIC` for fail-open setups/tests; drop `ELECTION` from v1, configuration and planned GraphQL enum (same as main D-R3).
- **D2:** closed, shared rule (C6): outbound bridges keep the default
  `ALWAYS`; one WARN at start for each outbound bridge at `ALWAYS` while a
  role source is set.
- **D3:** closed: `Receive.BridgeOutbound` is a deprecated alias for one
  release, ignored for `HOT_STANDBY` / `COLD_STANDBY` (C6).
- **D4:** closed for v1: no session failover outside native WinCC OA; shared SQL/Mongo session ownership requires a separate later plan.
- **D5:** closed: document case 5b as a supported deployment variant; implementation scope P0.
- **D6:** closed: per-component `Redundancy: ALWAYS | HOT_STANDBY |
  COLD_STANDBY` (C6). The `ActiveOnly` lists are dropped. The `redundancy`
  field in GraphQL and the dashboard (C9) is held behind the human commitment
  gate per AGENTS.md in both repositories (shared with main D-R5); it ships in
  phase PG together with main R7, not in P1 or P2.
- **Q-R1:** closed: `CapRole` role block (`role`, `roleSeq`, `leaseEpoch`,
  `flags`) in `HELLO`, `HELLO_OK`, `PING`, `PONG` (C1); no fallback over the
  replicated status topic. Spec 3.6.1 (wire, marked planned) and 12.1
  (status fields) are updated; drop the "planned" marks when built.
- **Q-R3:** closed: `OnIsolation` default `STANDBY` (no dual-active), same in
  both brokers (C5). `KEEP` stays as option.
- **Q-R4:** closed: config key `Redundancy.Source`, key set of C5 in both
  brokers.
- **Q-R2:** closed: native status keeps `role` = OA view (`ACTIVE`/`PASSIVE`)
  and adds `brokerRole` (no change for existing clients).
- **Q-NS1/Q-NS2:** closed: level `node`; no node topics on non-redundant systems.
- **Q-SO1:** closed: session watch automatically on for embedded `SessionStoreType: WINCCOA`; no extra switch.
- **Q-SO2:** closed: dual queuing requires PeerLink and `Receive.Queue: true`.
- **Q-SO3:** closed: down owner means partner unreachable **and** its OA host known inactive; unknown OA state is insufficient.
- **Q-SO4:** closed: accept at-least-once duplicates after a move; no owner-written high-water mark in v1.
- **Q-SO7:** closed: implement `TakeOver` and `DropOffline` hooks. Native empirical checks Q-SO5/Q-SO6 remain validation gates, not assumed facts.

### 10.2 To verify on a real redundant pair

No redundant test system is available today; until then simulator tests and
the non-redundant live case (Test321) only.

- Q-SO5: does a query connect report created and deleted datapoints? Same in
  simhost?
- Q-SO6: does a confirmed `dpSet` without a time get an Event Manager `_stime`
  identical on both hosts? Fallback: envelope `Rev` + `updated` element.
- Meaning of the `_ReduManager` elements marked "name" (`PeerErrorStatus`,
  `IsRecovering`, `Status.Reason`, ...).
- What a `dpSet` from an API manager on the passive host does without
  `connectToRedundantHosts` (discarded, error, forwarded).
- spec-peerlink-redundancy 14.6: answers and hotlinks once or twice with
  `connectToRedundantHosts`; connects restored after a switchover.

## Sources

- WinCC OA 3.21 API headers: `Manager/Manager.hxx`, `Basics/Utilities/Resources.hxx`.
- Test321: structure of `_ReduManager`, `_ReduHost`, `_Connections`.
- [Redundancy: principle and functionality](https://www.winccoa.com/documentation/WinCCOA/latest/en_US/Redundancy/Redundancy-03.html)
- [getReduDp()](https://www.winccoa.com/documentation/WinCCOA/3.20/en_US/ControlE_R/getReduDp.html)
- [Knowledge base: internal datapoints _Ui, _ReduManager](https://www.winccoa.com/knowledge-base/detail/what-is-the-functionality-of-the-internal-datapoints-ui-redumanager.html)
