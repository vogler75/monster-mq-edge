# Acceptance record: WinCC OA native embedding (M0-M5)

Companion to [plan-winccoa-broker-embedded-manager.md](plan-winccoa-broker-embedded-manager.md)
section 10 and [spec-winccoa-native.md](spec-winccoa-native.md).

Tested tree: branch `winccoa-native` (base `c4887b6`) with the uncommitted
implementation of 2026-09-29. Platform: Debian 12 aarch64 (Parallels VM),
Go 1.27.1 (module language 1.25), gcc 12 (Debian), WinCC OA 3.21 API
headers at `/opt/WinCC_OA/3.21/api`.

Status values:

- **Met**: the criterion is demonstrated by the listed evidence, including
  its live WinCC OA part where it has one.
- **Pending**: evidence still being collected.
- **Deferred**: M6, not in the approved scope.

Live evidence: `WCCOAmmq` (built from this tree with the 3.21 SDK) running
in project `Test321` as manager 80 (Debian 12 aarch64 VM, 4 vCPUs, desktop
shell using 1-3.5 cores during measurements), live tests
`test/integration/winccoa_live_test.go`:

```bash
MMQ_LIVE_PORT=27200 MMQ_LIVE_GQL=27201 MMQ_LIVE_PROJECT=Test321 MMQ_LIVE_NODE=live \
MMQ_LIVE_LOG=$HOME/WinCC_OA_Proj/Test321/log/PVSS_II.log \
MMQ_LIVE_RESTART="mgr.sh stop && mgr.sh start 5" MMQ_LIVE_STOP="mgr.sh stop" MMQ_LIVE_START="mgr.sh start 5" \
MMQ_LIVE_ASCII=/opt/WinCC_OA/3.21/bin/WCCOAasciiSQLite MMQ_LIVE_OVL_PORT=27210 MMQ_LIVE_OVL_NUM=85 \
go test ./test/integration -run TestLive -v -count=1
```

Fixtures: `winccoa/scripts/mmqCreateTypes.ctl`, `mmqLiveFixture.ctl`,
`mmqLiveDelete.ctl`, `mmqDeleteStores.ctl`, `mmqCreateLoadDps.ctl`.

## 10.1 Scope and design gates

| AC | Status | Evidence |
|---|---|---|
| AC-01 | Met | Decisions recorded in spec section 1 (CGO exception, M6 not granted, OA store layout, GraphQL unchanged). `git diff c4887b6 -- internal/graphql` is empty; no SQL/Mongo DDL changed. |
| AC-02 | Met | Target confirmed by the owner (spec section 2). `WCCOAmmq` built with the 3.21 SDK (gcc, C++17, c-archive) and the probe run against `Test321` (output below). Findings applied: `del=true` for `dpConnect`/`dpQueryConnectSingle` (callbacks survive the answer and are deleted on disconnect; `del=false` leaks them), `values` flag = initial-answer switch, query answer/hotlink = `[uint queryId, dyn_dyn_anytype table]` with DpIdentifier header/name cells, name resolution, `_DistManager.State.SystemNums` present. |
| AC-03 | Met | Spec sections 3, 4, 5, 7, 8: ABI, topic encoding, wildcard/shared policy, store confirmation, command results, every numeric limit. RPO/RTO not applicable (single node). |

## 10.2 Embedding, threading and lifecycle

| AC | Status | Evidence |
|---|---|---|
| AC-04 | Met | `make build`, `build-arm64`, `build-armv7`: pass, `CGO_ENABLED=0`, statically linked. `go list -deps ./cmd/monstermq-edge` has no `runtime/cgo` or `embed/cabi`. `make lint`, `make test` and `make test-race` pass (see results). |
| AC-05 | Met (Linux aarch64, gcc, c-archive) | `make embed-test`: plain C host links `libmonstermq.a` through `monstermq.h`; checks ABI version, short structs, ABI mismatch, NULL pointers, missing submit, zero-length and binary payloads, oversized inputs, stale handle, second instance, calls after destroy. The ABI has no QoS parameter (QoS is decided by the broker), so "invalid QoS" does not apply. Only Linux aarch64 c-archive is claimed. |
| AC-06 | Met | Manager counts SDK work executed off the dispatch thread: `offThreadCalls=0` in every statistics line, including the 2000/s load (120k writes, 120k hotlinks) and store restarts. simhost and the C harness assert the same on the Go side. Startup store loads, runtime writes, publishing and shutdown disconnects complete while dispatch continues (TestLiveStoresRestart, TestLiveBackupRestore). |
| AC-07 | Met | Live `TestLiveOverload` (manager with `queueCapacity = 16`): 1000-command burst -> 932 refused at the full queue, 68 confirmed, every command reported an outcome, queue high-water 16, manager recovers. Simulated `TestNativeOverload` covers paused completion and proves expired requests are never executed late. |
| AC-08 | Met | Live: invalid config (exit 1 in 109 ms, `mmq_create failed`), occupied port (exit 1, bind error, first manager unaffected), unreachable Event manager (never ready, SIGTERM exit in 5 ms), SIGTERM during startup (2-255 ms), SIGTERM under 2000/s traffic (838 ms, port released), repeated stop (C harness). Missing/wrong DPT and failed store load: `TestNativeStoreMissingType`, `TestNativeStartupFailures` (the DPT check itself runs live at every start). Fixed during the live run: SIGTERM was ignored while `connectToEvent` retried (now a raw `SA_ONSTACK` handler). |
| AC-09 | Met except the soak part (deferred with AC-35) | Live `TestLiveNoGrowth`: 100 subscribe cycles and 10 device create/delete cycles return to `connects=0 queries=0 liveCallbacks=1`. `make embed-check`: cgocheck2 and AddressSanitizer clean. SIGINT/SIGTERM stop the manager cleanly. 40 min of soak rounds: RSS flat. |

## 10.3 Baseline query publishing

| AC | Status | Evidence |
|---|---|---|
| AC-10 | Superseded | 2026-09-30: the native query transport for `WinCCOA-Client` devices was removed; devices always use the WinCC OA GraphQL server (`TestWinCCOaClientGraphQL`, `TestWinCCOaClientReservedOutput`). Earlier evidence is in git history. |
| AC-11 | Superseded | 2026-09-30: the native query transport for `WinCCOA-Client` devices was removed; devices always use the WinCC OA GraphQL server (`TestWinCCOaClientGraphQL`, `TestWinCCOaClientReservedOutput`). Earlier evidence is in git history. |
| AC-12 | Superseded | 2026-09-30: the native query transport for `WinCCOA-Client` devices was removed; devices always use the WinCC OA GraphQL server (`TestWinCCOaClientGraphQL`, `TestWinCCOaClientReservedOutput`). Earlier evidence is in git history. |
| AC-13 | Superseded | 2026-09-30: the native query transport for `WinCCOA-Client` devices was removed; devices always use the WinCC OA GraphQL server (`TestWinCCOaClientGraphQL`, `TestWinCCOaClientReservedOutput`). Earlier evidence is in git history. |

## 10.4 Native storage

| AC | Status | Evidence |
|---|---|---|
| AC-14 | Met | Live `TestLiveStoresRestart`: devices created through GraphQL are stored in `MMQConfigs_*`, survive a manager restart; `TestNativeStoreCoverage` covers every store method. |
| AC-15 | Met | `TestNativeStoreIdentity` (collisions, Unicode, size, binary wills, corrupt/future records never overwritten); live datapoint names `MMQConfigs_k<24 hex>` created and read by WinCC OA. |
| AC-16 | Met | `TestNativeStoreCommitErrors`; live store writes are confirmed by the OA answer of a grouped `dpSet` (session element + metadata). |
| AC-17 | Met | Live `TestLiveStoresRestart`: MQTT 5 persistent session restored with session present, queued native change delivered after restart, ordinary subscription active; `TestNativeStoreSessions` covers 3.1.1 clean, zero expiry and takeover. |
| AC-18 | Met | Live `TestLiveBackupRestore`: `WCCOAasciiSQLite -out` of the MMQ datapoints, deletion, `-in` import, restart: device configuration identical (JSON_MS, retained, query), session present, native subscription active. Queue and retained data live in the SQL store and are not part of this restore (README). |

## 10.5 Namespace and subscriptions

| AC | Status | Evidence |
|---|---|---|
| AC-19 | Met | Parser tests plus live `TestLiveSubackMatrix` (scalar root, nested element, type path, attribute, struct root, wrong DPT, unknown remote, local-as-remote). Distributed systems: `TestNativeRemoteSystems` (the test project has none). |
| AC-20 | Met | Live `TestLiveSubackMatrix` (13 filters, MQTT 5 and 3.1.1, exact codes and order) and `TestNativeSubackPerFilter` (no rows or interests for rejected filters). |
| AC-21 | Met | Live `TestLiveNoGrowth`: two clients and three aliases share registrations, released exactly once; `TestNativeRestoreBatches`. Fixed during the live run: list `dpConnect` delivered every element on any change (now one registration per element). |
| AC-22 | Met | Live `TestLiveValues` (initial value without change, aliases, live value 12 ms after the write) and `TestLiveStoresRestart` (offline persistent subscriber receives the queued change). |
| AC-23 | Met | Live `TestLiveDeleteRecreate`: deleted DP rejected for subscribe (0x8F) and write (0x90), no stale value, interest restored after recreation. Dist-link interruption: `TestNativeRemoteSystems`. |

## 10.6 Commands and authorization

| AC | Status | Evidence |
|---|---|---|
| AC-24 | Met | Live `TestLiveWrites`: float, bool, string (UTF-8), int, uint, nested element written and read back with `woa`; wrong types, missing and struct elements rejected; `TestNativeTypedWrites` covers boundaries and malformed payloads. |
| AC-25 | Met | `TestNativeAccessIsolation` (broker-side ACL logic, independent of the SDK); live SUBACK matrix confirms internal and store datapoints are denied (0x87). |
| AC-26 | Met | Live `TestLiveWrites` (response topic result `confirmed`, retained rejected) and `TestNativeTypedWrites`/`TestNativeCommandQoS2` (QoS 0/1/2, both MQTT versions, duplicate ids, timeouts). |

## 10.7 Optional redundancy

AC-27 to AC-33: **Deferred** (dual-node exception not granted; spec section 1).

## 10.8 Release evidence

| AC | Status | Evidence |
|---|---|---|
| AC-34 | Met | Live `cmd/mmqload`, 5000 float DPEs, 50 subscribers, 4 writers, 2000 writes/s, 60 s: 119948 sent, 119948 received, 0 lost; write -> OA -> subscriber p50 2.1 ms, p95 8.0 ms, p99 18.2 ms, max 63 ms; manager CPU 22.6 s over the run (about 0.3 cores), RSS 138 MiB, host queue high-water 54, pending high-water 61, no overload or dropped event. Budgets (spec 7): p99 <= 150 ms round trip, no loss, RSS <= 256 MiB. GraphQL comparison removed from scope by the owner. |
| AC-35 | Deferred by the owner | The WinCC OA demo licence of `Test321` stops the project about every 4 h (21:31, "License timer expired"), so a continuous 24 h soak cannot run on this installation. Partial evidence: 4 rounds of 10 min at 2000 writes/s with fresh clients, 4.8 M values, 0 lost, p99 11-35 ms, RSS 132-145 MiB flat. On the licence stop the manager reported in-flight writes as failed and stopped cleanly. |
| AC-36 | Met | winccoa/README.md (build, setup, DPT scripts, bootstrap example, diagnostics and statistics, connection loss, backup/restore, limits, rollback); `TestExampleConfigsValidate`; GraphQL SDL unchanged. Deferred: AC-27..AC-33 (M6). |

## Results

### make test / make test-race

- `make test` and `make test-race`: pass (exit 0), no data race reported.
- Test timeouts were raised to 300 s / 600 s: the integration package needs
  about 312 s under `-race` on this VM after the added native tests.
- Two pre-existing data races surfaced by the race run were fixed:
  `broker.Server` stop functions written by `Serve` and read by `Close`
  (now guarded), and the MQTT bridge reading its queue pointer without the
  connector lock in the on-connect handler.
- `TestRestSSESlowReader` failed on the untouched base as well. Cause: the
  test shrinks the client receive buffer to 1 KiB, below half the loopback
  MSS (MTU 65536), so after the server correctly gives up (socket in
  FIN-WAIT-1 with queued data) sender-side silly window avoidance never
  delivers the rest. The test now restores a normal receive buffer before
  draining and still requires the server to end the stream. Product code is
  unchanged.

### Soak (AC-35, partial)

| Round (10 min) | Sent | Received | Lost | p50 | p99 | max | RSS |
|---|---|---|---|---|---|---|---|
| 1 | 1200003 | 1200003 | 0 | 1.96 ms | 11.3 ms | 119 ms | 132 MiB |
| 2 | 1200003 | 1200003 | 0 | 2.18 ms | 34.5 ms | 983 ms | 145 MiB |
| 3 | 1200003 | 1200003 | 0 | 2.26 ms | 18.7 ms | 193 ms | 141 MiB |
| 4 | 1199999 | 1199999 | 0 | 2.25 ms | 17.2 ms | 186 ms | 141 MiB |

Round 1 overlapped `make test`/`make test-race`. Round 5 was cut by the
demo-licence stop of the whole project at 21:31. The full 24 h run is
deferred by the owner.

### Simulated load (preliminary AC-34)

`MMQ_LOAD=1 go test ./test/integration -run TestNativeLoadSimulated -v`,
simulated OA host, 5000 float DPEs, 50 subscribers, 2000 changes/s, 30 s
per leg, Debian 12 aarch64 VM:

| Path | Sent | Received | Lost | p50 | p95 | p99 | max |
|---|---|---|---|---|---|---|---|
| OA change -> subscriber | 60003 | 60003 | 0 | 0.75 ms | 5.0 ms | 10.5 ms | 31.7 ms |
| MQTT write -> OA -> subscriber | 60004 | 60004 | 0 | 2.9 ms | 20.2 ms | 36.6 ms | 117 ms |

Host request pending high-water: 217 of 4096; no overload, timeout or
dropped event. These numbers exclude the real WinCC OA managers; the
acceptance run repeats the round trip with `cmd/mmqload` against
`WCCOAmmq` and records CPU/RSS (`ps -o %cpu,rss`) and the manager's
statistics line.

### AC-02 probe output

`WCCOAmmq -proj Test321 -num 77` with `probeQuery = "SELECT '_online.._value', '_online.._stime' FROM 'ExampleDP_Arg*.'"`, `probeDpe`/`probeSet` on `ExampleDP_Arg1.` (2026-09-29, log `WCCOAmmq77.log`):

```
local system System1 number 1; _DistManager.State.SystemNums exists: yes
query(values=true,  del=false): answer [uint queryId, dyn_dyn_anytype 3x3: header ["", ":_online.._value", ":_online.._stime"] as DpIdentifier cells, rows [DpIdentifier name, float, time]]
query(values=true,  del=false): hotlinks [uint queryId, dyn_dyn_anytype: header + changed rows only]; disconnect deleted callback: no
query(values=false, del=false): answer [uint queryId] only (no initial rows); hotlinks as above; disconnect deleted callback: no
query(values=true,  del=true):  callback destroyed on dpQueryDisconnect: yes
connect(list,   del=false): answer + hotlinks item dpe=System1:ExampleDP_Arg1.:_online.._value float; list disconnect deleted callback: no
connect(single, del=false): single disconnect deleted callback: no
connect(list,   del=true):  callback kept after the answer, destroyed on dpDisconnect
```

Note: `woa set` reports success but does not change `ExampleDP_Arg1` in this
project (value and timestamp unchanged); the probe therefore writes the
value itself with `dpSet`.
