# Plan: WinCC OA Node and Redundancy Status over MQTT

**Status: partly implemented (2026-10-04): broker status fields `redundant`, `host`, `hostName`, `role` and `activeHost`, with switchover tracking ([spec-peerlink-redundancy.md](spec-peerlink-redundancy.md) section 14); the `node/1` and `node/2` topics and `eventConnections` are not implemented; `connectToRedundantHosts` is documented in `winccoa/README.md`.**

## 1. Goal and scope

MQTT clients of the embedded broker (`WCCOAmmq`) need to see the state of the
WinCC OA system they are connected to, in particular in a redundant pair:
which host is active, the error status of each host, whether the peer is
reachable, and on which host this broker runs.

In scope: reading and publishing WinCC OA redundancy state for both hosts,
and extending the broker status with this broker's host and role.

Out of scope (unchanged, still deferred as M6 in
[plan-winccoa-broker-embedded-manager.md](plan-winccoa-broker-embedded-manager.md)):
broker-to-broker replication, session/queue failover, write forwarding from
the passive to the active broker, and any change of the WinCC OA
redundancy state (no `Command.*` writes, no switchover from MQTT).

## 2. What WinCC OA offers a connected API manager

### 2.1 C++ API (verified in the 3.21 headers)

| Call | Meaning |
|---|---|
| `Resources::isRedundant()` | project runs as a redundant pair |
| `Resources::getReplica()` | replica number of this manager (1 or 2) |
| `Resources::getMyReduHostNum()` | 1 or 2: which event manager this manager is connected to (`event = "host1$host2"`); 1 when not redundant |
| `Resources::getMyReduHost()` / `getOtherReduHost()` | host names of the own and the other event manager |
| `Resources::getPrimaryEMHost()` / `getSecondaryEMHost()` | configured event hosts |
| `Resources::isRedActive()` | this system (host) is the active one |
| `Manager::isRedConnOpen(eventId)` | connection to the redundant partner's event manager is open |
| `handleManagerUpdate(manId, disconnect)` (virtual) | called when a redundant connection to one of the two event/data managers opens or closes |
| `doRefresh(mId)` (virtual) | called on a redundancy switchover; the manager must refresh its connections |

`isRedundantConnection(manId)` only says whether a connection goes to a
redundant pair; it is not a role or health query (already noted in the
embedded-manager plan, section 1.3).

### 2.2 Internal datapoints (structure verified in Test321)

`_ReduManager` belongs to host 1 (left replica), `_ReduManager_2` to host 2
(right replica); both are kept in sync by the redundancy manager and are
readable from either host. Relevant elements; "doc" = meaning stated in the
WinCC OA documentation, "name" = inferred from the element name and to be
confirmed on a redundant pair (open point 3):

| Element | Type | Meaning | Source |
|---|---|---|---|
| `Status.Active` | bool | this host is active (1) or passive (0) | doc |
| `EvStatus` | bool | the event manager of this host is active (1) or passive (0) | doc |
| `MyErrorStatus` | uint | error status of this host; the host with the lower value becomes active automatically (weights 0..999) | doc |
| `PeerAlive.Link0` | bool | LAN connection 0 to the partner is alive | doc |
| `PeerAlive.LastAliveTime` | time | last alive sign received from the partner | doc |
| `PeerErrorStatus` | uint | error status of the partner as seen by this host | name |
| `MaxMyErrorStatus` | uint | maximum error status | name |
| `Status.Preferred` | bool | this host is the preferred one | name |
| `Status.Manual` | bool | manual mode (no automatic switchover) | name |
| `Status.Reason` | uint | reason of the last status change | name |
| `SplitMode` / `SplitActive` | bool | split mode is on / this host is active in split mode | name |
| `IsRecovering` | int | recovery (resynchronization) in progress | name |
| `MissingMonitoredManagers` | dyn | monitored managers that are missing on this host | name |
| `ErrorChangeReason` | text | reason of the last error status change | name |

`_Connections` / `_Connections_2` list the managers connected to the event
manager of host 1 / host 2 (`ManNums`, `StartTimes`, `HostNames` per manager
type), so the broker can also report which hosts its own manager number is
running on.

Behavior documented for redundant systems: the same managers run on both
hosts; the passive event manager only synchronizes with the active one and
discards changes coming from its side (UI, drivers). Consequence for the
broker: reads work on both hosts, **writes (`.../set`) through the broker on
the passive host are discarded** (confirmed by the owner 2026-10-03: the
passive host receives value changes but does not execute them). Forwarding
them to the active host is a follow-up idea (`plan-peerlink.md`, Q29).

In Test321 (not redundant) all these values are unset or false; that is the
expected non-redundant state.

## 3. Proposed MQTT topics

The native namespace uses `<TopicRoot>/<SystemsName>/<system>/...` (default `winccoa`).
A top-level `winccoa/node/<node>` would collide with a WinCC OA system named
`node` (the reason `winccoa/node/...` was removed), so the node topics go
below the system:

```text
winccoa/systems/<System>                 broker status (exists), extended (3.2)
winccoa/systems/<System>/node/1          redundancy state of host 1 (retained JSON)
winccoa/systems/<System>/node/2          redundancy state of host 2 (retained JSON)
```

`node` becomes a reserved level next to `tags`, `types` and `cns` (not
configurable). `winccoa/systems/<System>/node/+` subscribes to both hosts. With
the local shortcut the node topics of the local system are also
`winccoa/node/1` and `winccoa/node/2` (broker status on `winccoa`), so
`node` is then also reserved directly below the root, next to `systems`.
(The owner suggested `redundancy` instead of `node` on 2026-09-30; to be
decided in review.)

**Decision for review:** key the node topics by host number (`1`, `2`,
recommended: stable, matches `_ReduManager`/`_ReduManager_2`, independent
of host renames) or by host name (`winccoa/systems/System1/node/scada-a`, readable,
but changes with the host name and needs encoding).

### 3.1 Node payload

```json
{
  "host": 1,
  "hostName": "scada-a",
  "active": true,
  "eventManagerActive": true,
  "errorStatus": 0,
  "peerErrorStatus": 0,
  "maxErrorStatus": 100,
  "preferred": true,
  "manual": false,
  "splitMode": false,
  "splitActive": false,
  "recovering": false,
  "peerAlive": true,
  "peerLastAlive": "2026-09-30T12:00:00.000Z",
  "missingManagers": [],
  "broker": true,
  "timestamp": "2026-09-30T12:00:00.123Z"
}
```

- Values come from `_ReduManager[_2]`; `hostName` from the configured event
  hosts; `broker` is true for the host this broker runs on
  (`Resources::getReplica()`).
- Published retained, QoS 1, on every change of a watched element and when
  this broker's connection to an event manager changes; unchanged values are
  not republished.
- Not redundant: no `node/...` topics are published (only the broker status
  says `redundant: false`). **Decision for review:** alternatively publish
  `node/1` with `active: true` so clients need no special case.

### 3.2 Broker status extension (`winccoa/systems/<System>`)

Existing fields stay. New fields:

```json
{
  "redundant": true,
  "host": 2,
  "hostName": "scada-b",
  "role": "PASSIVE",
  "eventConnections": { "1": true, "2": true }
}
```

- `role` changes from the fixed `STANDALONE` to `ACTIVE` / `PASSIVE` when
  redundant (from `Status.Active` of the own host). `STANDALONE` stays for
  non-redundant systems.
- `eventConnections`: which event managers this manager is connected to
  (`isConnOpen(eventId)`, `isRedConnOpen(eventId)`, kept current by
  `handleManagerUpdate`).
- **Known limitation:** both brokers (one per host) publish their own status
  on the same topic `winccoa/systems/<System>`. That is fine for clients of one
  broker, but conflicts when the two brokers are bridged. **Decision for
  review:** keep it (bridging is not supported without M6), or move the
  per-broker part into `node/<host>` (`"broker": {...}` only on the
  broker's own host) and keep `winccoa/systems/<System>` for system-wide facts.

## 4. Implementation

1. **Static facts via `SYS_INFO`** (C++ + Go): extend the `OpSysInfo` answer
   with `redundant`, `replica`, `myReduHostNum`, host 1/2 names. No new
   operation; old fields unchanged.
2. **Redundancy datapoints via existing `DpConnect`** (Go only): the native
   service connects the listed `_ReduManager` and `_ReduManager_2` elements
   (about 30 DPEs) with the existing batched `dpConnect`, only when
   `redundant` is true. Internal datapoints stay hidden from the tag
   namespace (`Protected()` is unchanged; this is an internal consumer).
3. **Connection changes** (C++): override `handleManagerUpdate` and
   `doRefresh`, and report "event connection to host N open/closed" and
   "switchover" as events on reference 0, like the `_DistManager` system
   events today. The Go side updates `eventConnections` and republishes.
4. **Publishing** (Go): a small `reduState` component in
   `internal/winccoanative` builds the node JSON per host, compares with the
   last published payload and publishes retained on change; the broker
   status includes the new fields.
5. **Namespace**: reserve `node` below the system (subscribe allowed, with
   ACL; external publishes rejected; wildcards `node/+` handled as ordinary
   MQTT filters, no OA registration); SUBACK/Classify rules and docs.
6. **Simulator**: model `_ReduManager`/`_ReduManager_2`, `redundant` and
   `replica` in `SYS_INFO`, and connection events, so all of it is testable
   without a redundant project.
7. **Docs**: spec section 4 (topics, payloads), README, config/schema only
   if a setting is added (none planned).

## 5. Tests

- Simulator: non-redundant (no node topics, status `STANDALONE`,
  `redundant: false`); redundant host 1 active / host 2 passive; switchover
  (both node topics and `role` change, one publish per change); peer loss
  (`peerAlive` false, `eventConnections` updates); error status change;
  broker restart (retained node topics current again); ACL on `node/...`;
  external publish to `node/...` rejected.
- Live, non-redundant (Test321): status fields and absence of node topics.
- Live, redundant: needs a redundant pair (two hosts or VMs with one
  project). Not available today, see 7.

## 6. Acceptance criteria

- [ ] **RS-01** Non-redundant: broker status has `redundant: false`,
  `role: STANDALONE`; no `node/...` publications; no connects to
  `_ReduManager*`.
- [ ] **RS-02** Redundant: `node/1` and `node/2` are retained and match
  `_ReduManager` / `_ReduManager_2` within 1 s of a change; `broker` marks
  the own host.
- [ ] **RS-03** Switchover: both node topics and the broker `role` reflect the
  new active host; no stale `ACTIVE` on both.
- [ ] **RS-04** Peer loss and recovery: `peerAlive`, error status and
  `eventConnections` follow; after recovery all values are current again.
- [ ] **RS-05** Node topics cannot be written by clients and respect ACLs;
  internal datapoints are not reachable through `tags/`/`types/`.
- [ ] **RS-06** Verified on a real redundant pair (both hosts, including a
  switchover), with the observed passive-side write behavior documented.

## 7. Open points and risks

1. **No redundant test system.** RS-06 needs a redundant pair; until then
   only simulator tests and the non-redundant live case are possible.
2. **Writes on the passive host.** The documentation says the passive event
   manager discards changes from its side. Verify what a `dpSet` from an API
   manager on the passive host does (discarded, error, or forwarded), and
   decide whether the broker should reject `.../set` on the passive host
   with a clear result (proposed: reject with `0x83` "passive redundancy
   host" once verified) instead of confirming a write that has no effect.
3. **Element semantics.** The elements marked "name" in 2.2 (for example
   `Status.Reason`, `IsRecovering`, `PeerErrorStatus` vs. the partner's own
   `MyErrorStatus`) need confirmation on a redundant pair or from the
   "Internal datapoint types" reference of the installed version; the
   payload keeps the raw values.
4. **Event connections from API managers.** Whether an API manager on host 2
   connects to both event managers (and `isRedConnOpen` is meaningful)
   depends on the project's `event = "host1$host2"` configuration; verify.

## Sources

- WinCC OA 3.21 API headers: `Manager/Manager.hxx` (`isRedConnOpen`,
  `handleManagerUpdate`, `doRefresh`, `isRedundantConnection`),
  `Basics/Utilities/Resources.hxx` (`isRedundant`, `isRedActive`,
  `getReplica`, `getMyReduHostNum`, `getMyReduHost`, `getOtherReduHost`).
- Test321: structure of `_ReduManager`, `_ReduHost`, `_Connections`.
- [Redundancy: principle and functionality](https://www.winccoa.com/documentation/WinCCOA/latest/en_US/Redundancy/Redundancy-03.html)
- [getReduDp() (left/right replica datapoints)](https://www.winccoa.com/documentation/WinCCOA/3.20/en_US/ControlE_R/getReduDp.html)
- [Knowledge base: internal datapoints _Ui, _ReduManager](https://www.winccoa.com/knowledge-base/detail/what-is-the-functionality-of-the-internal-datapoints-ui-redumanager.html)
