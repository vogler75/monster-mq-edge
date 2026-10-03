# MonsterMQ Edge embedded in a WinCC OA API manager

`WCCOAmmq` is a WinCC OA C++ API manager that runs the MonsterMQ Edge
broker in-process. The manager thread executes all WinCC OA calls; the Go
broker (linked as a c-archive) owns MQTT, routing, storage and the native
namespace. Design and contracts:

- [Plan and acceptance criteria](../dev/plans/plan-winccoa-broker-embedded-manager.md)
- [Frozen specification (ABI, topics, limits)](../dev/plans/spec-winccoa-native.md)
- [Acceptance status](../dev/plans/acceptance-winccoa-native.md)

## Supported combination

Only this combination is claimed (spec section 2):

| Item | Value |
|---|---|
| WinCC OA | 3.21 (`/opt/WinCC_OA/3.21/api`) |
| OS / arch | Debian 12, aarch64 (Linux only; no Windows DLL) |
| Library | `libmonstermq.a` (Go `-buildmode=c-archive`) |
| Compiler | system gcc/g++, C++17 |

## Build

```bash
export API_ROOT=/opt/WinCC_OA/3.21/api
make embed-test                       # builds build/embed/libmonstermq.a and runs the C ABI harness
cd winccoa/manager && mkdir -p build && cd build
cmake .. && make                      # produces WCCOAmmq
```

Or in one step with `winccoa/build.sh` (`--test` runs the C ABI harness,
`--install <project>` copies the manager to `<project>/bin`, `--restart <index>`
stops and starts that PMON manager via `woa`):

```bash
./winccoa/build.sh --test --restart 11
```

The standalone broker is unaffected: `make build`, `build-arm64` and
`build-armv7` stay `CGO_ENABLED=0` and never link the embedding library.

## Project setup

1. Copy `WCCOAmmq` to `<project>/bin`.
2. The store datapoint types (`MMQConfigs`, `MMQSessions`, `MMQRetained`)
   are created by the manager at startup when a store type is `WINCCOA` and
   the type is missing. An existing type is never changed: with a different
   layout the broker refuses to start. `winccoa/scripts/mmqCreateTypes.ctl`
   creates the same types manually, e.g. to prepare a project. With
   `Namespace: true` the manager also creates `MMQTopic` (topics branch, see
   "Topics replicated through WinCC OA").
3. Copy `monstermq.yaml.example` to `<project>/config/monstermq.yaml` and
   adjust ports, users and stores. The manager runs in the project
   directory, so relative paths in it (`SQLite.Path`, key stores) resolve
   against `<project>/`.
4. Add to `<project>/config/config`:

   ```ini
   [monstermq]
   brokerConfig = "config/monstermq.yaml"   # relative to the project directory
   dispatchMs = 2                           # dispatch wait; latency floor for Go -> OA requests
   tickBudget = 256                         # requests per dispatch tick
   tickBudgetMs = 5
   queueCapacity = 4096
   stopTimeoutMs = 10000
   statsSeconds = 60                        # statistics line interval
   ```

5. Add the manager to the console (PMON) as `WCCOAmmq -num <n>`.
   Use a distinct manager number; `-dbg USR1` enables debug logging
   (see "Debug logging of WinCC OA calls" below). Every
   `statsSeconds` the manager logs a statistics line (`connects`, `queries`,
   `liveCallbacks`, `queued`, `queueHighWater`, `overloads`, `offThreadCalls`,
   `setMessages`, `setItems`, `hotlinkItems`); set `MMQ_STATS_SECONDS` in the
   manager environment for the broker-side `host client stats` line.

## Readiness and diagnostics

- `winccoa/systems/<System>` (retained JSON, `<System>` = the project's system
  name): `nodeId`, `ready`, `oa` connection, `system`, `redundant`, `role`,
  `timestamp`. `role` is `STANDALONE` in a non-redundant system; in a
  redundant one it is `ACTIVE` or `PASSIVE` for the host the broker runs on
  (`UNKNOWN` until known), with `host` (1 or 2), `hostName` and
  `activeHost` (1, 2, or 0 when unknown or in split mode). A retained empty
  publish clears a stale status of another system name; the own status
  cannot be written.
- Broker log lines go to the WinCC OA log (`PVSS_II.log` / log viewer)
  with the `MonsterMQ` catalog prefix. A start failure (bad config, occupied
  port, missing DPT, unreachable store) is logged as `broker failed: ...` and
  the manager exits with code 1.
- Namespace: `winccoa/systems/<System>/tags/<DP>/<element...>` and
  `winccoa/systems/<System>/types/<DPT>/<DP>/<element...>`, where `<System>` is the
  local system or a connected remote system (same form for both), optional
  read attribute suffix
  (`_online.._value`, `_online.._stime`, `_online.._status`,
  `_online.._invalid`), writes via `.../set` with `{"value": ..}`. See the
  spec for encoding, reason codes and command results.
  `winccoa/systems/<System>/topics/<topic>`: MQTT topics replicated through
  `MMQTopic` datapoints (see "Topics replicated through WinCC OA").

## Debug logging of WinCC OA calls

At level DEBUG the broker logs one line per WinCC OA call, after the answer,
with its arguments, duration and error:

```
oa dpQueryConnectSingle ref=12 query="SELECT '_online.._value', '_online.._stime' FROM '*.**'" answer=true duration=3.2ms
oa dpConnect ref=13 count=2 names="[System1:Pump1.speed:_online.._value ...]" answer=true duration=450µs
oa dpSetWait count=1 names="[System1:Pump1.speed:_original.._value]" values=[42.5] duration=1.1ms
oa dpQueryDisconnect ref=12 duration=210µs
```

Also logged: `dpConnectNoSource`, `dpDisconnect`, `dpGet`, `dpNames`,
`dpCreate`, `dpDelete`, `resolve`, `typeCheck`, `sysInfo`. Name and value
lists are cut after 20 entries (`(+N more)`). Hotlink and query-row events
are not logged.

Enable it either with `Logging.Level: DEBUG` in `monstermq.yaml` or with
the manager option `-dbg USR1`, which forces DEBUG regardless of the YAML.
Without `-dbg USR1`, `Logging.Level` applies (`INFO` by default). The lines
go to the WinCC OA log with the `MonsterMQ` prefix.

## Connection loss and restarts

- Remote systems: the manager follows `_DistManager.State.SystemNums`. A
  lost system makes its native subscriptions go quiet (no stale values, no
  fallback to the local system), new subscriptions and writes for it are
  rejected with `0x83`, and its registrations are re-created once it
  reconnects.
- Local Event/Data connection: a WinCC OA API manager terminates when it
  loses the Event manager. Run `WCCOAmmq` with PMON restart mode
  `always`; on restart the broker restores persistent sessions and their
  native subscriptions from the stores and revalidates them against the
  current datapoints.

## Topic names

Every system, the local one included, is addressed with its name below
`systems`; the local system is also reachable through a shortcut without
the system name, which is the easy form for a single (non-distributed)
system:

| | explicit form (always) | local shortcut (`LocalShortcut: true`, default) |
|---|---|---|
| local tag | `winccoa/systems/System1/tags/Pump1/speed` | `winccoa/tags/Pump1/speed` |
| local type | `winccoa/systems/System1/types/Pump/Pump1/speed` | `winccoa/types/Pump/Pump1/speed` |
| remote tag | `winccoa/systems/SubstationA/tags/Feeder1/voltage` | – |
| broker status | `winccoa/systems/System1` | `winccoa` |

Both forms of a local element are the same datapoint element: a change is
published to every form someone subscribed to, and writes work through
both. For ACLs the explicit form is the reference: a shortcut or type topic
is only allowed when the explicit tags form is allowed too.
`LocalShortcut: false` leaves only the explicit form.

`winccoa`, `systems`, `tags` and `types` are defaults and can be changed in
`monstermq.yaml`:

```yaml
WinCCOaNative:
  TopicRoot: plant/oa   # may have several levels
  TagsName: t
  TypesName: dpt
  SystemsName: sys
```

This gives `plant/oa/sys/System1/t/Pump1/speed` (shortcut
`plant/oa/t/Pump1/speed`), `plant/oa/sys/System1/dpt/Pump/Pump1/speed` and
the status topics `plant/oa/sys/System1` and `plant/oa`. The whole
`TopicRoot` branch is reserved for the
broker; the default `winccoa` becomes an ordinary topic. Clients' ACLs and
persisted subscriptions refer to topics, so they have to follow a rename.

## Wildcard subscriptions

Wildcards inside `winccoa/systems/<System>/tags/` and `winccoa/systems/<System>/types/` are
served by WinCC OA queries:

- `winccoa/systems/System1/tags/Pump1/#`: every value element of `Pump1`
- `winccoa/systems/System1/tags/Pump1/value/#`: `Pump1.value` and everything below
- `winccoa/systems/System1/tags/+/speed`: the `speed` element of every datapoint
- `winccoa/systems/System1/types/Pump/#`: every element of every datapoint of type `Pump`
- `winccoa/systems/System1/types/Pump/+/value/#`: the `value` subtree of all `Pump`s
- `winccoa/systems/System1/tags/#`: the whole system (internal and store datapoints excluded)

Each element is published to its own topic, so a client can mix wildcard and
exact subscriptions; a change is delivered once. Filters that cover every
datapoint (`tags/#`, `tags/+/...`, `types/#`) and the broker-wide `#` are
only accepted while `AllowRootWildcardSubscription` (top-level config key,
default `true`) is not set to `false`.

## SDK probe (AC-02)

Set `probeQuery` and/or `probeDpe` in `[monstermq]` and start the manager
once from a shell; it logs `PROBE:` lines (stderr and WinCC OA log) and
exits instead of starting the broker:

```ini
[monstermq]
probeQuery = "SELECT '_online.._value', '_online.._stime' FROM 'ExampleDP_*.'"
probeDpe = "ExampleDP_Arg1.:_online.._value"
probeSet = "ExampleDP_Arg1.:_original.._value"   # changed once per second by the probe
```

Change matching values while it listens. Record the output in
`dev/plans/acceptance-winccoa-native.md`: answer/hotlink table layout,
whether `values=false` suppresses the initial rows, and whether each
disconnect variant deletes the callback object.

## Backup and restore

The native stores keep configuration and session metadata in `MMQConfigs_*`
and `MMQSessions_*` datapoints, and with `RetainedStoreType: WINCCOA` the
retained messages in `MMQRetained_*` datapoints. Include them in the
project's ASCII export (`WCCOAascii -out ... -filterDp "MMQ*"`) or database
backup. They do not contain queued messages or MQTT inflight state; those
stay in the configured SQL store (`SQLite.Path` by default) and need their
own backup.

## Stores in WinCC OA

| Store | Key | Datapoint type |
|---|---|---|
| device and archive configs | `ConfigStoreType: WINCCOA` | `MMQConfigs` |
| sessions and subscriptions | `SessionStoreType: WINCCOA` | `MMQSessions` |
| retained messages | `RetainedStoreType: WINCCOA` | `MMQRetained` |
| MQTT users and ACL rules | `UserStoreType: WINCCOA` | `MMQUsers` |

`DefaultStoreType: WINCCOA` puts all four in WinCC OA. The offline message
queue and the metrics then stay in memory (`QueueStoreType` and
`Metrics.StoreType` may only be `MEMORY`, metrics also `NONE`), and no
database file is written. Archive groups with a `SQLITE` last value or
archive then need their own database connection.

### Users (`MMQUsers`)

One datapoint per user, `MMQUsers_k<hash of the user name>`:

| Element | Type | Content |
|---|---|---|
| `user` | string | user name |
| `passwordHash` | string | bcrypt hash |
| `enabled`, `canSubscribe`, `canPublish`, `isAdmin` | bool | permissions |
| `acl` | string | the user's ACL rules as JSON: `[{"id","topic","subscribe","publish","priority","created"}]` |
| `created`, `updated` | time | |

Deleting a user deletes its datapoint and with it its ACL rules. The
password hashes are readable by everyone who may read these datapoints in
WinCC OA; restrict read access to `MMQUsers_*` with WinCC OA's datapoint
permissions. They are never reachable through the MQTT namespace.

## Retained messages in WinCC OA

With `RetainedStoreType: WINCCOA` every retained topic is one datapoint of
type `MMQRetained` (created by the manager when missing):

| Element | Type | Content |
|---|---|---|
| `value` | blob | payload |
| `topic` | string | MQTT topic (the datapoint name is a hash of it) |
| `user` | string | MQTT user that published the value (empty for anonymous and broker-internal publishes) |
| `qos` | uint | QoS of the publish |
| `expiry` | uint | message expiry interval in seconds, 0 = none |
| `updated` | time | time of the publish |

Retained messages below `TopicRoot` (the broker's own status topics such as
`winccoa/systems/System1`) get no datapoint: they are kept in memory only and
republished by the broker at startup.

A retained publish with an empty payload deletes the datapoint, as do
message expiry and retention purges. The broker loads all retained messages
at startup and serves subscriptions from memory; edits made to these
datapoints in WinCC OA while it runs are not picked up. One payload is
limited to just under 1 MiB (the manager's message limit).

## Topics replicated through WinCC OA

Publishes to `winccoa/systems/<System>/topics/<topic>` (local shortcut
`winccoa/topics/<topic>`; the level is `TopicsName`) are not handled by the
broker itself but written to a datapoint of type `MMQTopic` on that system,
one per topic. Subscribers get the messages
through a connection to that datapoint, so every broker of a distributed
system that subscribes to the same topic receives them, whichever broker
published:

| Element | Type | Content |
|---|---|---|
| `topic` | string | MQTT topic below `topics/` (the datapoint name is a hash of it) |
| `value` | blob | payload of non-retained publishes; last value storage turned off |
| `retained` | blob | retained message (kept across restarts) |

- Datapoint names (`TopicDpNames`, the same on every broker of a
  distributed system):
  - `HASH` (default): `MMQTopic_k<24 hex digits of SHA-256 of the topic>`.
  - `NAME`: `MMQTopic_<topic>`, e.g. `plant/line-1/temp` ->
    `MMQTopic_plant/line-1/temp`. Characters WinCC OA forbids in datapoint
    names (blank `. : , ; * ? [ ] { } $ @`, control characters), the quotes
    `" ' \` and `%` are written as `%XX` (hex), so different topics never
    share a name; a name over 128 characters falls back to the hash.
  - In both modes a datapoint whose `topic` element holds another topic is
    never written, deleted or delivered from (publish rejected with `0x90`).
- The first publish to a topic creates the datapoint (also on a remote
  system) and turns off the last value storage of `value`; later publishes
  only write the element. The PUBACK is sent after WinCC OA confirmed the
  write; failures are rejected (`0x83` for an unavailable system or a missing
  `MMQTopic` type there). The manager creates the type on its own system;
  a remote system must have it (its own broker creates it).
- A retained publish writes `retained`, a retained publish with an empty
  payload clears it (subscribers get the empty message) and deletes the
  datapoint. Non-retained publishes never delete it.
- A subscription connects `value` and `retained`; a new subscriber gets the
  retained message (retain flag set), every later write of either element is
  delivered live (retain flag unset). Subscribing before the datapoint exists
  is allowed: the subscription connects as soon as any broker creates it.
- Wildcards work below `topics/` (`winccoa/systems/<System>/topics/#`,
  `winccoa/topics/plant/+/state`): one `dpQueryConnectSingle` per system on
  `MMQTopic_*.topic` lists the topics, including ones created later, and
  every matching datapoint is connected like an exact subscription.
  Messages arrive under the concrete topic in the subscribed form.
- A topic created by a broker on another system may lose its very first
  non-retained message for subscribers elsewhere (written before their
  connect); a retained message is never lost. On the publishing broker the
  subscriptions connect before the first write.
- The broker's own retained store is not used for these topics. Shared
  subscriptions are not supported below `topics/`.

## Redundant WinCC OA systems

In a redundant system `WCCOAmmq` runs on both hosts. By default a manager
connects only to the Event Manager of its own host. On the passive host the
Event Manager receives the manager's writes but does not execute them, so
`dpSet`, `dpCreate` and `dpDelete` of the broker there have no effect: store
writes time out (for example `subscriptions persist failed ... write
MMQSessions_k...: context deadline exceeded`) and native writes are lost.

Connect the manager to both Event Managers, like a UI:

```ini
[monstermq]
connectToRedundantHosts = 1
```

or start it with the option `-connectToRedundantHosts`. WinCC OA documents
the entry for all config sections except `[general]` ("all managers can
establish a connection to both Event Managers"). The passive Event Manager
holds back messages of a manager connected to both Event Managers until the
active one sends the same message, and the active Event Manager executes
them. The broker on the passive host then writes through the active server
like the one on the active host.

Give the two managers different manager numbers. Both connect to both Event
Managers, so they need distinct numbers in the system, for example
`WCCOAmmq -num 1` on the first host and `WCCOAmmq -num 2` on the second
(in PMON, Options `-num 1` / `-num 2`, both with `connectToRedundantHosts`
in `[monstermq]` or `-connectToRedundantHosts` in the options).

The broker status (`winccoa/systems/<System>` and `winccoa`) shows which
host the broker runs on and whether that host is active:

```json
{"system": "System1", "redundant": true, "host": 2, "hostName": "debian2",
 "role": "PASSIVE", "activeHost": 1, ...}
```

The broker finds its host by comparing the computer name with the event
hosts of the project (`event = "debian1$debian2"`), falling back to the
manager's replica number, and follows `_ReduManager.Status.Active` (host 1)
and `_ReduManager_2.Status.Active` (host 2). After a switchover the status is
republished with the new `role` and `activeHost`.

Still to verify on a redundant pair:

- whether answers to `dpSetWait`, `dpCreate` and `dpDelete` arrive once or
  once per Event Manager (the manager must ignore a second answer);
- whether `dpConnect` and `dpQueryConnect` registrations are restored after a
  switchover (the API calls `doRefresh`; the manager does not override it);
- whether hotlinks arrive twice, once per connection.

## PeerLink for redundant pairs

[PeerLink](../README.md#peerlink-forwarding-between-brokers) forwards MQTT
publishes between the brokers of the two hosts of a redundant pair, in memory
and in both directions. It is independent of the WinCC OA role: both brokers
accept clients and forward (active-active), and nothing switches on a
switchover.

> The WinCC OA specific parts (the `oaRetained` link mode, the `peerLink`
> status object and the device warnings) are implemented but not yet tested
> against a live WinCC OA project. The general
> [limitations](../README.md#limitations) apply; in particular, synchronise
> both hosts with NTP, and with `RetainedStoreType: WINCCOA` concurrent
> retained publishes of one topic are not serialized with their capture.

**The namespace is never forwarded** while native mode is active
(`WinCCOaNative.Enabled` and `Namespace`): WinCC OA mirrors `<TopicRoot>`
itself (tag values, the topics branch), and each host's status topic stays
local. Each broker also drops topics under the root the other host announces.
Every other topic is forwarded, except `$` topics and `Capture.Exclude`.

| | Topics branch (`winccoa/topics/...`) | PeerLink (all other topics) |
|---|---|---|
| Carried by | WinCC OA (`MMQTopic` datapoints) | Broker to broker, in memory |
| Survives a restart of both hosts | Yes | No |
| PUBACK | After WinCC OA confirmed the write | When the local broker accepted the message |
| Throughput and latency | Bound by WinCC OA | Bound by the brokers |

Use the topics branch for messages that must survive the loss of both hosts.

### Configuration

The same `PeerLink` block serves both hosts; only `NodeId` differs (see
`monstermq.yaml.example`):

```yaml
NodeId: edge-oa-1                   # edge-oa-2 on the other host
PeerLink:
  Enabled: true
  Tls: { Enabled: true, AutoGenerate: true, CertPath: "certs/peer-{NodeId}.pem", KeyPath: "certs/peer-{NodeId}.key" }
  SharedSecrets: ["<base64 of 32 random bytes, same on both hosts: openssl rand -base64 32>"]
  Log: { DrainOnShutdownMs: 5000 }  # planned stops: let the other host catch up
  Peers:
    - { NodeId: edge-oa-1, Address: "oa-host-a:1890" }
    - { NodeId: edge-oa-2, Address: "oa-host-b:1890" }
```

- The `NodeId` must differ per host. Devices run on the host their `NodeId`
  names, so changing a NodeId changes which devices run where: move device
  and archive-group assignments in the same maintenance step.
- The peers must authenticate each other: TLS with a shared secret as above,
  or mTLS (main README). With `UserManagement.Enabled: true` there is no
  unauthenticated option. Relative certificate paths resolve against the
  project directory.
- `DrainOnShutdownMs: 5000`: when `WCCOAmmq` stops, it waits up to 5 s for
  the other host to pull what is left.
- PeerLink is in memory. A planned stop of one host is lossless for
  everything published before it, as long as the other host is connected and
  catches up within `DrainOnShutdownMs`. A crash loses what the other host
  had not pulled yet. The full table is in the main README under "Delivery
  guarantees (RPO)".

### Stores

- **`RetainedStoreType: WINCCOA`** stays as it is: WinCC OA stores the
  `MMQRetained` datapoints and replicates them between the two hosts. Both
  hosts have the same system name, so their link runs in `oaRetained` mode: a
  forwarded retained message updates only the receiver's in-memory view and
  is not written a second time, and the retained snapshot on first contact is
  skipped. After a restart each host loads `MMQRetained`, which WinCC OA kept
  in sync. Between WinCC OA distributed systems (different system names) the
  receiver writes its own `MMQRetained`. Two independent projects linked by
  PeerLink need different system names: with the same name they are taken
  for one system, and forwarded retained messages are not stored.
- **`SessionStoreType: WINCCOA`** mirrors `MMQSessions` to both hosts. With
  PeerLink enabled, a broker restores offline queueing at startup only for
  persistent sessions that were last connected to it, not for clients that
  are online on the other host.
- **`ConfigStoreType: WINCCOA`** mirrors `MMQConfigs`: both hosts see the
  same device configurations, which is why devices are assigned to one host
  (below).

### Writes on the passive host

WinCC OA receives value changes made on the passive host but does not
execute them. Unless the manager connects to both Event Managers
(`connectToRedundantHosts = 1`, see "Redundant WinCC OA systems"), every
datapoint write of the broker on the passive host therefore has no effect:

- a retained message published to the passive broker is not stored in
  `MMQRetained`. It lives in the memory of both brokers (the active one gets
  it through PeerLink) and is lost when both restart;
- native writes (`.../set`) and topics-branch publishes
  (`winccoa/topics/...`) sent to the passive broker are not executed;
- writes of the other WINCCOA stores (`MMQSessions`, `MMQConfigs`,
  `MMQUsers`) on the passive host are lost as well.

With `connectToRedundantHosts = 1` these writes are executed by the active
server. Without it, clients that write into WinCC OA, and configuration
changes, have to use the broker on the active host.

### Devices and clients

- Assign every device that publishes into the broker (MQTT bridges with
  inbound subscriptions, WinCC UA/OA bridges, RTSP cameras, scripts) to one
  host's `NodeId`. With `ConfigStoreType: WINCCOA` (or a shared PostgreSQL or
  MongoDB database) a device with `local` or `*` runs on both hosts, and its
  output arrives twice. If that host fails, the device's output stops until
  the device is reassigned.
- Outbound-only MQTT bridges run on both hosts (`*`) with
  `Receive.BridgeOutbound: false` (the default): each host's bridge forwards
  that host's own publishes. A bridge with both directions on one host needs
  `BridgeOutbound: true` and a remote that is not the other host. That
  setting applies to every bridge of the host, so a host with
  `BridgeOutbound: true` must not also run `*` outbound-only bridges, or the
  other host's publishes reach the remote twice.
- Never bridge one host's broker to the other's.
- Redfish gateways ignore `NodeId`: enable Redfish on one host only. Host
  monitoring may run on both hosts as long as its `BaseTopic` contains
  `{NodeId}` (the default).
- An archive group that writes to a shared database belongs to one host, or
  set `Receive.Archive: false`.
- The broker logs a WARN at startup for devices that break these rules.
- Clients may connect to both brokers at the same time. Keep a persistent
  session on one host where possible, and give shared subscription groups
  members on both hosts (`Receive.SharedSubscriptions: SKIP`). Use MQTT 5: on
  a planned stop, clients get reason 0x8B (server shutting down) and can fail
  over at once. After a network partition, retained values can differ between
  the hosts (on each host the retained message applied last wins).

### Monitoring

The native status (`winccoa/systems/<System>`, retained JSON) gets a
`peerLink` object, updated at most every 5 s and on every state change:

```json
{ "enabled": true,
  "consumers": [{ "nodeId": "edge-oa-2", "state": "CONNECTED", "lag": 0, "lostTotal": 0 }],
  "sources":   [{ "nodeId": "edge-oa-2", "state": "STREAMING", "lagRecords": 0, "gapLostTotal": 0,
                 "sourceResets": 0, "retainedDiverged": 0, "lastError": "" }] }
```

`consumers` are the peers that pull from this host, `sources` the peers this
host pulls from. The status topic is not forwarded, so each host reports its
own view; it is the alarm surface for WinCC OA operators. The full counters
are at `curl -s http://127.0.0.1:1890/peerlink/v1/status` on each host.

### Rolling upgrade

Upgrade one host at a time. Versions with the same major protocol version
(`mmq-peer/1`) work together; every minor release is tested against the
previous one.

1. Check on both hosts that both links are up and the lag is 0 (`peerLink`
   status: consumers `CONNECTED`, sources `STREAMING`).
2. Stop `WCCOAmmq` on host A. The broker closes its MQTT listeners (clients
   fail over to host B), stops its bridges and scripts, waits up to
   `DrainOnShutdownMs` until host B has pulled the rest, and logs what it
   could not serve (`shutdownUnserved`).
3. Replace the manager on host A and start it. Host B pulls from A's new log;
   A pulls what B collected while A was down.
4. Wait until both links are up and the lag is 0 again, then repeat for
   host B.

While host A is down, host B keeps A's share in its log, bounded by
`Log.MaxBytes` (`capacitySeconds` in the status); older records are dropped
and counted. A new major protocol version must reach both hosts within that
window: until then the link is refused (`GOAWAY version`).

### Enabling TLS without losing the link

A pair that links in plaintext (`AllowUnauthenticatedPeers` with
`Listener.AllowedNetworks`, only possible with `UserManagement.Enabled:
false`) moves to TLS in three steps. Each step is applied on host A, then on
host B, with one restart at a time; each restart drains the connected peer,
and both directions are never down at once.

1. Enable listener TLS (`Tls.Enabled: true` with a certificate, or
   `AutoGenerate: true`) together with `Listener.AllowPlaintext: true`. Keep
   dialing in plaintext with `Tls: { Enabled: false }` on the peer entry.
2. Switch the dialer to TLS: drop the peer's `Tls.Enabled: false` and give it
   a way to verify the other host (`PinnedSha256` with the `spkiSha256` the
   other host logs at startup, or `Tls.TrustStorePath`).
3. Set `Listener.AllowPlaintext: false`.

A shared secret is introduced in one go: a host that has it refuses a peer
without it, in both directions, so the link is down until both hosts run with
the secret; meanwhile each host keeps the other's share in its log.
`SharedSecrets` and `PinnedSha256` are lists, so later rotations are lossless:
add the new value on both hosts, move it to the first position on both, then
remove the old one.

## Load and soak

- `go build -o build/mmqload ./cmd/mmqload`, create the load datapoints with
  `winccoa/scripts/mmqCreateLoadDps.ctl`, then e.g.
  `./build/mmqload -broker tcp://127.0.0.1:1883 -clients 50 -dpes 5000 -rate 2000 -duration 60s`
  (write -> WinCC OA -> hotlink -> subscriber round trip).
- `winccoa/scripts/mmq-soak.sh <broker-url> 24 soak.csv [pid-file]` runs the
  24 h soak in 10-minute rounds. A demo licence stops the project every few
  hours, so run it on a licensed installation.

## Durability and limits

- A store write is reported successful only after the OA event manager
  answered the `dpSet` without error; a queued `dpSet` is never success.
- Numeric limits (queue sizes, timeouts, batch size 100, payload and record
  sizes, interest limit) are in spec section 7.
- Commands: `confirmed` means OA accepted the value, not that the process
  acted on it. Commands without an `id` are not retry-safe.
- The native scope is single node: sessions, subscriptions, offline queues
  and inflight state are not shared between hosts, and there is no failover
  or write forwarding (M6, AC-27..AC-33, stays deferred). Publishes outside
  `<TopicRoot>` can be forwarded to the other host of a pair with PeerLink,
  in memory (see "PeerLink for redundant pairs").

## Rollback

Stop and remove `WCCOAmmq` from the console and run the standalone
broker again (or the previous embedded build). Stores that were not moved to
OA datapoints are untouched and are used by the standalone broker as
before. There is no automatic migration between the `MMQConfigs` /
`MMQSessions` datapoints and the SQL stores: export device and archive
configuration through the dashboard or GraphQL before switching, and import
it into the target broker. Session metadata is rebuilt by clients
reconnecting.
