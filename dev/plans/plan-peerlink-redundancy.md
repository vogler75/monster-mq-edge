# Plan: PeerLink redundancy for the edge broker (WinCC OA and witness)

**Status: draft (2026-10-09). Not reviewed, not committed by the owner.**

This plan holds every work item needed to close the gaps of the deployment
cases in [use-cases-deployment.md](../../winccoa/dev/plans/use-cases-deployment.md). It replaces the
former plans for node redundancy status and session ownership; only the parts
that a use case needs were kept.

The edge broker gets its role either from WinCC OA (direct connectivity,
source `WINCCOA`, case 6) or, without WinCC OA, from a witness lease in an
external Postgres / MongoDB (source `WITNESS`, case 3). The Kotlin main broker
has the same design without native WinCC OA access:
`main/dev/plans/plan-peerlink-redundancy.md` (Part II). Wire format and the
`Redundancy` / `Witness` YAML keys are shared between both brokers.

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
| G2.2 archive group on a shared DB writes once per node | 2 | WP-R2 (`ACTIVE_ONLY` without a role source = "only on the node named in `NodeID`", see 4.4) |
| G3.1 no role manager | 3 | WP-R1 (source `WITNESS`, 3.4) |
| G3.2 no `Redundancy` switch for bridges, devices, archives, scripts | 3 | WP-R2 |
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
- GraphQL changes (the switch of WP-R2 is YAML only, see D6).

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

### 3.1 Role sources

`Redundancy.Source` in the broker config:

| Source | Case | Own role input |
|---|---|---|
| `NONE` (default) | 1, 2, 4, 5 | none; role is `STANDALONE`, every `ACTIVE_ONLY` component acts (4.4) |
| `STATIC` | 3 | `Redundancy.Role: ACTIVE \| STANDBY` |
| `WINCCOA` | 6 | E1: own host active / passive / unknown |
| `WITNESS` | 3 | lease in an external Postgres / MongoDB (3.4) |
| `ELECTION` | 3, later | lowest `NodeId` among reachable peers (D1; may be dropped for `WITNESS`) |

`Redundancy.Partner` names the PeerLink peer (`NodeId`) that is the other
half of the pair. With `WINCCOA` it defaults to the peer with
`RedundancyPartner: true`.

### 3.2 Role rule (`STATIC`, `WINCCOA`)

Evaluated on every change of the own input or of the partner's state:

| Own input | Partner | Role |
|---|---|---|
| active | any | `ACTIVE` |
| passive | reachable and reports `ACTIVE` | `STANDBY` |
| passive or unknown | unreachable, or reachable and not `ACTIVE` | `ACTIVE` (split brain accepted: duplicates beat loss) |

- **Partner reachable** = PeerLink link to the partner is `CONNECTED` in at
  least one direction.
- **Partner reports `ACTIVE`**: the role is carried in the PeerLink status
  that already flows over the link: capability `CapRole` (`1<<4`, after
  `CapTombstone`), same as the main broker (main plan II.4). `HelloOK` and
  `PONG` get `role` (u8: `UNKNOWN=0`, `ACTIVE=1`, `STANDBY=2`), `roleSeq`
  (u64), `epoch` (u64, 0 without witness) and `flags` (u8: bit 0
  `witnessReachable`, bit 1 `leaseHolder`). Wire change in `mmq-peer/1`, spec
  section 3.6; needs owner sign-off, Q-R1. A peer without `CapRole` counts as
  `UNKNOWN`. Fallback without a wire change: read the partner's retained broker
  status topic replicated by PeerLink (slower, only with native mode).
- **Hold-down:** a role change from `STANDBY` to `ACTIVE` because the partner
  became unreachable waits `Redundancy.TakeoverDelayMs` (default 3000) so a
  short PeerLink reconnect does not flip the role. A change caused by the own
  WinCC OA input applies at once.

### 3.3 Interface

- New package `internal/redundancy`: `Manager` with `Role()`, `Subscribe(func(Role))`
  and `ActiveOnlyAllowed(component) bool` (used by WP-R2). est. 250 lines.
- Inputs: WinCC OA state from `winccoanative` (E1, through a small callback,
  no import cycle), PeerLink link state and partner role from `peerlink`.
- Output: the role in the broker status (`role` field already exists for
  native mode; values `ACTIVE`, `STANDBY` (new for non-OA), `STANDALONE`;
  native keeps `PASSIVE` as its OA view in a new field `oaRole`, Q-R2) and in
  the log (one INFO line per change with the reason).
- Without native mode the status is published on `$SYS/broker/redundancy/role`
  (retained). No GraphQL field (D6).

### 3.4 Role source `WITNESS` (case 3, no WinCC OA)

Same design and same tables as the main broker (main plan II.5, II.6.2, II.8),
so an edge and a Kotlin node can form one pair with one witness.

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
version, heartbeatat, PK(groupname, nodeid))`, created at start
(`CREATE TABLE IF NOT EXISTS`). Acquire and renew in one statement:

```sql
INSERT INTO redundancylease (groupname, holder, epoch, expiresat, updatedat)
VALUES ($1, $2, 1, now() + $3 * interval '1 millisecond', now())
ON CONFLICT (groupname) DO UPDATE SET
  holder    = EXCLUDED.holder,
  epoch     = CASE WHEN redundancylease.holder = EXCLUDED.holder
                   THEN redundancylease.epoch ELSE redundancylease.epoch + 1 END,
  expiresat = EXCLUDED.expiresat,
  updatedat = now()
WHERE redundancylease.holder = EXCLUDED.holder OR redundancylease.expiresat < now()
RETURNING holder, epoch;
```

MongoDB: `FindOneAndUpdate` on `_id = group` with filter
`$or: [{holder: me}, {$expr: {$lt: ["$expiresAt", "$$NOW"]}}]`, update
pipeline with `$$NOW`, upsert; a duplicate-key error means "not won".

- Expiry uses the store clock only; the node keeps the lease locally until
  `tSend + LeaseTtlMs - SafetyMarginMs` on its monotonic clock, so two holders
  never overlap.
- The preferred node acquires at once, others after `TakeoverDelayMs` past
  expiry. `Failback: false`: a returning preferred node does not take the
  lease back. Graceful shutdown releases (`expiresat = now()`) before PeerLink
  stops.
- Each renew upserts the own `redundancynodes` row (role, epoch, `linkup`).

**Role rule:**

| Witness | Lease | PeerLink to partner | Role |
|---|---|---|---|
| reachable | I hold it | any | `ACTIVE` |
| reachable | partner holds it | any | `STANDBY` |
| reachable | expired / none | any | try acquire; result decides |
| unreachable | I hold it, local validity left | up | `ACTIVE` |
| unreachable | – | up, partner `ACTIVE` | `STANDBY` |
| unreachable | – | up, partner not `ACTIVE` | keep current role |
| unreachable | – | down | `OnIsolation`: `STANDBY` (no dual-active, loss possible) or `KEEP` (duplicates, no loss) |

**Split mode:**

| Case | Seen by | Result |
|---|---|---|
| link down, both reach the witness | store: both heartbeats fresh, `linkup = false` | one lease holder, one `ACTIVE`; status `SPLIT_LINK` |
| link down, one node isolated | the node with the witness | witness side runs; isolated node per `OnIsolation` |
| two `ACTIVE` rows in the store, no link | store | worst case (`KEEP`, stale lease); status `SPLIT_DUAL_ACTIVE`, WARN |
| link up, both claim `ACTIVE` | `PONG` | resolved at once |

Resolution: lease holder wins, then higher epoch, then lower `Priority`, then
lower `NodeId`; the loser goes `STANDBY` at once (fencing, no grace).
Duplicates while split are accepted; PeerLink resync fills the gaps after the
link returns.

**Code.** `internal/redundancy/witness.go` (interface `Lease`:
`AcquireOrRenew`, `Release`, `Read`, `Heartbeat`, `Nodes`),
`witness_postgres.go` and `witness_mongodb.go` on the existing drivers of
`internal/stores/postgres` and `internal/stores/mongodb`. est. 400 lines.
Validation: `WITNESS` with `Connection: default` whose host is this node →
WARN (witness not independent); `SafetyMarginMs >= LeaseTtlMs` or
`RenewIntervalMs > LeaseTtlMs / 2` → startup error.

**Status:** `split` (`NONE`, `SPLIT_LINK`, `SPLIT_DUAL_ACTIVE`), `epoch`,
`holder`, `witnessReachable` in the broker status (3.3) and as metrics
`redundancy_role`, `redundancy_epoch`, `redundancy_split`,
`redundancy_lease_renew_failures_total`.

## 4. WP-R2: `ALWAYS | ACTIVE_ONLY` switch

### 4.1 Configuration (YAML only, D6)

```yaml
Redundancy:
  Source: WINCCOA              # NONE | STATIC | WINCCOA | WITNESS | ELECTION
  Partner: oa-host-2           # PeerLink NodeId, optional with RedundancyPartner
  TakeoverDelayMs: 3000
  ActiveOnly:                  # components that act only on the ACTIVE node
    Bridges: [ "to-cloud" ]    # mqttclient connection names (outbound part)
    Devices: [ "plc-writer" ]  # winccua / winccoa / camera / redfish device names
    ArchiveGroups: [ "central" ]
    Scripts: [ "alarm-mailer" ]
  Defaults:
    MqttClientOutbound: ALWAYS # D2: ACTIVE_ONLY when a Source is set?
```

Everything not listed is `ALWAYS` (today's behaviour). Names that match no
component log one WARN at start.

### 4.2 One shared check

`redundancy.Manager.ActiveOnlyAllowed(kind, name)` is the single decision point:

| Component | Where the check goes | `ACTIVE_ONLY` on `STANDBY` |
|---|---|---|
| `mqttclient` outbound | `BusAdapter.forwards` (E4) | forward nothing, **including** local publishes; on `ACTIVE` forward local publishes **and** replicas |
| `mqttclient` inbound, `winccua`, cameras | publish path of the bridge | stays connected, discards what it reads (R3); the active node publishes, PeerLink carries it |
| device that writes to a field system | device write path | write rejected with a log line; reads as inbound |
| archive group | `archive/group.go` write path | no write; the in-memory last value is kept so a switchover starts current |
| script | `scripting/manager.go` run loop | not started; started on `ACTIVE`, stopped on `STANDBY` |

A role change re-evaluates all components at once (subscription from
`Manager.Subscribe`). No reconnects for `ALWAYS` components.

### 4.3 Replaces `Receive.BridgeOutbound`

- With a role source set, `Receive.BridgeOutbound` is ignored for bridges
  listed in `ActiveOnly.Bridges`; they forward replicas on `ACTIVE`.
- D3: keep `BridgeOutbound` as deprecated alias (recommended: keep for one
  release, WARN when set together with `Source`).

### 4.4 Without a role source (G2.2)

With `Source: NONE`, `ActiveOnly` components act only where `NodeID`
names this node explicitly (not `local`/`*`). For an archive group on a
shared DB in a mesh: list it in `ActiveOnly.ArchiveGroups` and set it on one
node; the others do not write. This replaces the manual
`Receive.Archive: false` on all but one node.

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
| P1 | WP-R1 role manager with `NONE`, `STATIC`, `WINCCOA`; `CapRole` over PeerLink | 3, 6 | 1 w |
| P2 | WP-R2 switch for `mqttclient` (both directions), archive groups, devices, scripts; `BridgeOutbound` alias | 2, 3, 6 | 1 w |
| P3 | WP-NS node topics | 6 | 3 d |
| P4 | WP-SO S1 session watch | 6 | 1 w |
| P5 | WP-SO S2 tracker, engine hooks, S3 dual queuing, `REMOTE_DEAD` | 6 | 1.5 w |
| P6 | WP-R1 source `WITNESS` (Postgres, then MongoDB), split detection and resolution (3.4) | 3 | 1.5 w |
| P7 | Docs: spec-winccoa-native section 5, spec-peerlink-redundancy section 14, README, use-case document "works today" rows | all | 1 d |

P0 ships alone. P1 and P2 belong together (P1 alone is status only). P4 alone
already fixes problems 1 and 5 of 7.1.

## 9. Tests and acceptance criteria

Unit and integration tests use `oahost/simhost` (two brokers on one simhost
for WP-SO; extend for query connect with deletes, Q-SO5) and two in-process
brokers with PeerLink for WP-R1/R2.

| Id | Criterion | Case |
|---|---|---|
| AC-01 | `Source: NONE`: behaviour unchanged; `ActiveOnly` components act only where `NodeID` names this node | 1, 2 |
| AC-02 | `STATIC` pair: one `ACTIVE`, one `STANDBY`; stop the active → standby becomes `ACTIVE` after `TakeoverDelayMs`; restart → back to `STANDBY` | 3 |
| AC-03 | Outbound `mqttclient` in `ActiveOnly.Bridges` on both nodes: every message (local and replica) forwarded exactly once while both run, and still once after a failover | 3, 6 |
| AC-04 | Inbound bridge on both nodes: standby connected, publishes nothing; after a switchover the new active publishes without reconnect | 3, 6 |
| AC-05 | Archive group in `ActiveOnly.ArchiveGroups` on a shared DB: one row per message, before and after failover | 2, 3, 6 |
| AC-06 | `WINCCOA` source: role follows `_ReduManager[_2].Status.Active`; split (both passive, partner unreachable) → both `ACTIVE`, logged | 6 |
| AC-07 | WP-W1: redundant system without `connectToRedundantHosts` → one WARN, status `false` | 5, 6 |
| AC-08 | Non-redundant: no `node/...` topics, no connects to `_ReduManager*` | 4 |
| AC-09 | Redundant: `node/1`, `node/2` retained, current within 1 s, `broker` marks own host; not writable by clients, ACL respected | 6 |
| AC-10 | Session move with queue (v5 and v3.1.1): C reconnects on B with `SessionPresent=1`, same subscriptions, every message (duplicates allowed); A's queue empty afterwards | 6 |
| AC-11 | Takeover of a live connection: A closes with `0x8E`; record `nodeId=B`, `connected=true`; a late echo does not kick C | 6 |
| AC-12 | Expiry on the non-owner keeps the datapoint; crash leftover reset at start; down owner → other node queues | 5b, 6 |
| AC-13 | OA reconnect (switchover with `connectToRedundantHosts`): cache reconciled, states re-evaluated | 6 |
| AC-14 | Load: 10,000 sessions, cache build < 5 s, < 50 MiB (est.) | 6 |
| AC-15 | All of case 6 verified on a real redundant pair, including a switchover (10.2) | 6 |
| AC-16 | `WITNESS` pair: preferred node `ACTIVE` epoch 1; kill it → partner `ACTIVE` after TTL + `TakeoverDelayMs`, epoch 2; graceful stop → takeover within one renew interval | 3 |
| AC-17 | `WITNESS`: block PeerLink only → roles unchanged, `SPLIT_LINK`; block witness only → roles unchanged | 3 |
| AC-18 | `WITNESS`: isolate the active node, `OnIsolation: STANDBY` → never two `ACTIVE`; `KEEP` → `SPLIT_DUAL_ACTIVE`, after reconnect the lower epoch steps down | 3 |
| AC-19 | `WITNESS`: pause the active node longer than the TTL → on resume it sees the higher epoch and steps down at once; ±5 s clock skew → no overlap | 3 |
| AC-20 | Mixed pair edge + Kotlin main on one witness group: same roles and status as two edge nodes | 3 |

## 10. Open points

### 10.1 Decisions (from the use-case document and the merged plans)

- **D1:** case 3 with `WITNESS` (recommended; `STATIC` stays for tests).
  Drop `ELECTION` (same as main D-R3)?
- **D2:** outbound `mqttclient` bridges default to `ACTIVE_ONLY` when a role
  source is set? Recommended: no, keep `ALWAYS` and list them; a WARN at start
  for outbound bridges not listed while a role source is set.
- **D3:** `Receive.BridgeOutbound` as deprecated alias for one release
  (recommended) or removed.
- **D4:** case 3 session failover: accept "no session failover"
  (recommended now) or a later plan for a shared Postgres session store reusing
  WP-SO.
- **D5:** case 5b (cold standby started by WinCC OA) as documented variant
  (recommended: yes, needs only P0).
- **D6:** `ActiveOnly` as YAML lists (recommended, no GraphQL change) or a
  `redundancy` field on each device / archive group input (GraphQL change in
  both brokers and the dashboard; needs owner commitment per AGENTS.md).
- **Q-R1:** `CapRole` with `role`, `roleSeq`, `epoch`, `flags` in
  `HelloOK`/`PONG` (wire change shared with main, recommended) or the
  replicated status topic.
- **Q-R3:** `OnIsolation` default `STANDBY` (no dual-active) or `KEEP` (no loss)?
  Same default in both brokers.
- **Q-R4:** config key `Redundancy.Source` (as main) instead of the earlier
  `RoleSource`. This plan uses `Source`.
- **Q-R2:** native status: keep `role` = OA view (`ACTIVE`/`PASSIVE`) and add
  `brokerRole`, or switch `role` to the broker role and add `oaRole`.
  Recommended: keep `role`, add `brokerRole` (no change for existing clients).
- **Q-NS1:** level name `node` or `redundancy`. **Q-NS2:** publish `node/1`
  on non-redundant systems?
- **Q-SO1:** session watch always on with `WINCCOA` (recommended) or a switch.
- **Q-SO2:** no dual queuing without PeerLink (recommended).
- **Q-SO3:** down node = partner unreachable **and** OA host not active
  (recommended), PeerLink only, or not at all.
- **Q-SO4:** accept duplicates after a move (recommended) or an owner-written
  high-water mark (est. +1 week).
- **Q-SO7:** accept engine hooks `TakeOver`, `DropOffline`.

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
