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
   creates the same types manually, e.g. to prepare a project.
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
  name): `nodeId`, `ready`, `oa` connection, `system`, `role`
  (`STANDALONE`), `timestamp`. A retained empty publish clears a stale
  status of another system name; the own status cannot be written.
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
- Single node only; the redundancy milestone (M6) is not implemented.

## Rollback

Stop and remove `WCCOAmmq` from the console and run the standalone
broker again (or the previous embedded build). Stores that were not moved to
OA datapoints are untouched and are used by the standalone broker as
before. There is no automatic migration between the `MMQConfigs` /
`MMQSessions` datapoints and the SQL stores: export device and archive
configuration through the dashboard or GraphQL before switching, and import
it into the target broker. Session metadata is rebuilt by clients
reconnecting.
