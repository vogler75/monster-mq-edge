# Plan: In-Process C/C++ Embedding for Go Edge Broker (WinCC OA Manager Integration)

## 1. Executive Summary and Review Status

Embed `monster-mq-edge` in a WinCC OA C++ API Manager through an isolated C ABI. The manager owns WinCC OA API calls; Go owns MQTT connections, routing, and broker services. Deliver single-node query publishing first, then native storage and dynamic subscriptions/writes. Dual-node replication is a separate, conditional milestone.

**Review date: 2026-09-24. Status: feasible direction, not yet ready for implementation approval.** This review checks repository code, the API-manager skill and its bundled API references, the Kotlin bridge, and official documentation. No manager was compiled or live WinCC OA project tested. The WinCC OA version, patch, target OS/architecture, and C++ toolchain remain unconfirmed. Version-dependent API signatures, ownership rules, redundancy signals, and build compatibility require validation against that selected SDK.

### 1.1 Objectives

1. Remove the GraphQL/WebSocket transport between the broker and the local WinCC OA manager. C ABI calls still incur scheduling, copying, conversion, and potentially JSON serialization; latency and throughput improvements must be measured.
2. Preserve the existing WinCC OA query bridge's query, initial-answer, topic transformation, retained-message, and payload-format behavior using a native transport. *(Removed 2026-09-30: `WinCCOA-Client` devices always use the WinCC OA GraphQL server; `WinCCOaNative.Transport` no longer exists.)*
3. Optionally persist device/archive/database-connection configuration and session metadata in `MMQConfigs` and `MMQSessions` datapoints.
4. Expose tag/type MQTT namespaces under `winccoa/<systemname>/`, validated subscriptions, and typed writes. Reserve `winccoa/<systemname>/cns/` for the separate future CNS feature.
5. Optionally support a WinCC OA redundant pair, with explicit durability, recovery, and write-ownership rules.
6. Keep the standalone broker and its ARM builds compatible with `CGO_ENABLED=0`; isolate CGO to an opt-in embedding target.

### 1.2 Required decisions before implementation

Reviewing this document does not approve exceptions to [AGENTS.md](../../AGENTS.md).

| Decision | Required resolution |
|---|---|
| CGO exception | Explicitly approve CGO for the optional embedding target. Package isolation preserves standalone builds but does not itself waive the repository's prohibition. |
| Dual-node exception | Explicitly approve the redundancy/replication scope. It conflicts with the single-node/no-clustering rule even without Hazelcast or Kafka. |
| WinCC OA SDK | Confirm version and patch, supported OS/architecture, compiler/runtime ABI, and deployment requirements before using SDK paths or build commands. Compilation remains developer-run under the API-manager skill. |
| GraphQL contract | Prefer host-side native-transport selection while preserving existing `WinCCOA-Client` configuration. No new SDL, resolver behavior, type, enum, field, or transport selector is authorized by this plan. If dashboard selection requires an interface change, first propose the exact cross-repository change and obtain explicit commitment. |
| Native storage contract | Approve the new OA datapoint layout and decide whether the Java broker must read it. Existing SQLite/Postgres/MongoDB layouts remain unchanged. The OA provider does not automatically meet physical-database parity. |
| Availability and durability | Confirm the degraded-mode data-loss allowance, partition/write policy, supported MQTT semantics, and failover RPO/RTO before milestone 6. |

### 1.3 Verification findings incorporated in this revision

| Finding | Evidence and correction |
|---|---|
| Native transport can reuse an existing subsystem | `internal/bridge/winccoa/` and `internal/graphql/schema/winccoa.graphqls` already exist; start from their contracts rather than inventing another public device type. |
| Storage needs startup refactoring | `broker.New()` selects concrete backends and performs storage work synchronously. Add a production dependency-injection path; do not call it on the OA dispatch thread while storage requests need that same thread. |
| Subscription callback cannot reject as drafted | `processSubscribe()` uses `OnSubscribe` to transform a packet, then decides each filter's result. A `void` callback and `OnSubscribed` side effect cannot supply a per-filter validation decision. |
| Acknowledgement timing needs engine work | `processPublish()` sends PUBACK/PUBREC before `OnPublished`; `processPubrel()` sends PUBCOMP before `OnQosComplete`. Replication must gate the actual acknowledgement paths. |
| Session metadata is insufficient for failover | `SessionStore` stores metadata/subscriptions; `QueueStore`, retained data, and MQTT inflight state are separate. Replicating `MMQSessions` alone cannot restore a complete MQTT session. |
| Callback lifetime examples were unsafe | The bundled `ManagerAPI.md` documents callback-aware `dpDisconnect` overloads and framework ownership in disconnect paths. Remove unconditional `delete wait` after disconnect and verify ownership against the chosen SDK. |
| Redundancy APIs were misinterpreted | The bundled reference defines `isRedundantConnection(const ManagerIdentifier&)` as a connection-to-pair check, not a role/health query. `isReduReplicaConnected()` is not found there; do not assume it exists. |
| Datapoint names and topic grammar need correction | A colon separates the OA system prefix. Use `MMQConfigs_<key>`/`MMQSessions_<key>` for DP names, and define scalar-root, alias, attribute, wildcard, and status-topic rules explicitly. |

## 2. Architecture and Threading

```text
MQTT clients / external dashboard
                |
        Embedded Go broker
        - native transport adapter
        - existing routing/auth/storage interfaces
        - bounded requests and completion tracking
                |
        C ABI: copied buffers + request IDs
                |
        Bounded C++ request queue
                |
        WinCC OA manager's main thread
        - dispatch + bounded queue draining
        - resolve / connect / query / write / storage calls
        - completion callbacks + queued value publication
                |
        WinCC OA Event / Data managers
```

All OA API access, including name/type resolution and connect/disconnect, occurs on the manager thread. Go callbacks may run on arbitrary runtime threads and may only copy/enqueue work. `runtime.LockOSThread` does not make a Go thread the existing OA manager thread.

Use request IDs, operation types, deadlines, cancellation, and explicit asynchronous completions. Go store methods may wait on completions off the manager thread. Never hold a broker, storage, or queue lock across a cross-runtime wait. Never synchronously enter a Go publish/storage path from an OA callback if that path can call back into OA and wait.

OA-to-Go publishing must copy and enqueue data promptly. A Go worker performs broker publication. Define bounded queue capacities and per-tick item/time budgets. Control/storage requests must receive an overload error rather than disappear. Telemetry may be coalesced or dropped only under an explicit policy with counters; do not silently claim delivery after overflow.

The C++ host owns process lifetime, signals, and SDK resources. The library must not call `os.Exit`, install a competing application shutdown handler, or terminate the host on a recoverable error. Review Go runtime signal coexistence for shared/archive embedding. Once the Go library is loaded, the host must install its SIGINT/SIGTERM handlers with `sigaction` and `SA_ONSTACK`; the API-manager template's plain `signal()` call does not set `SA_ONSTACK`, and a signal delivered on a Go-owned thread could then crash the process. The embedded entry point must not call `signal.Notify` for SIGINT/SIGTERM (the standalone `cmd/monstermq-edge/main.go` does); shutdown arrives only through the ABI stop call. Do not promise unloading/reloading a Go runtime library in the same process. [Go signal documentation](https://pkg.go.dev/os/signal@go1.25.0#hdr-Non_Go_programs_that_call_Go_code)

## 3. WinCC OA Manager Lifecycle Integration

```text
STATE_JUST_STARTED -> STATE_INIT -> STATE_ADJUST -> STATE_RUNNING -> shutdown
```

1. Initialize UTF-8 arguments, Resources, manager number, configuration, and SDK-compatible signal handling. Validate bootstrap configuration before starting network listeners.
2. From the manager's startup/run path, call `connectToData(StartDpInitSysMsg::TYPE_CONTAINER | StartDpInitSysMsg::DP_IDENTIFICATION)` and continue dispatching during initialization. Handle termination and failed connections here too.
3. Connect to Event and wait for the required ready state. Do not issue datapoint operations merely because a connection attempt returned.
4. Initialize ABI queues and start broker initialization asynchronously. Keep dispatching while Go loads native storage through the queue. Publish readiness only after stores, authentication, required OA subscriptions, and listener binding succeed.
5. Establish configured queries and optional dynamic subscription restoration. Registration-send success is not operation success: inspect asynchronous answers and errors.
6. Resolve the initial OA role if redundancy is enabled; keep writes disabled while role is unknown. Publish initial broker status when ready.
7. Run timed dispatch and bounded queue draining. `dispatch(long&, long&)` requires lvalues and modifies them; reset both every iteration. A proposed 20 ms idle timeout is a tuning starting point, not a latency guarantee.
8. On OA disconnect, expose degraded health, invalidate stale resolution state, reject/expire pending writes, and recreate subscriptions after reconnection without duplicate registrations.
9. On shutdown, stop admitting requests/listeners, cancel or complete outstanding work, disconnect subscriptions with SDK-correct ownership, and flush/close stores while dispatch remains alive. Only after broker stop completion may the host release callbacks and SDK resources. A signal handler sets a flag; it does not perform this teardown itself.

## 4. WinCC OA Native Datapoint Storage

### 4.1 Storage coverage and bootstrap

Implement OA adapters against `internal/stores/interfaces.go`:

- `DeviceConfigStore`: full CRUD, enumeration/filtering, enable/disable, and reassignment.
- `ArchiveConfigStore`: archive-group configuration and database-connection definitions. These are not covered by `DeviceConfigStore` alone.
- `SessionStore`: all session/subscription methods, including enumeration, will metadata, expiry-related state, and purging. `Storage.Sessions` and `Storage.Subscriptions` must use the intended common provider.

Explicitly select existing providers for `Queue`, `Retained`, `Users`, `Metrics`, and message archives. Native session metadata does not replace those stores. Bootstrap configuration must remain accessible before OA config loading, including storage selection, manager/project settings, listener/auth prerequisites, and native transport bindings. Avoid a circular dependency where loading connection settings requires the broker that needs those settings.

Refactor broker construction to accept an assembled `stores.Storage` and ownership/cleanup policy. Existing standalone factory behavior must remain compatible. Datapoint storage success must have a documented confirmation level; a queued `dpSet` is not durable completion. Existing hooks sometimes log storage errors and continue: where persistence is promised before MQTT success, add an error path before the acknowledgement rather than only swapping the provider.

### 4.2 Proposed datapoint types

These are proposed OA layouts, subject to the storage decision in section 1.2.

| DPT | Elements | Purpose |
|---|---|---|
| `MMQConfigs` | `config: string`, `type: string`, `updated: time` | Versioned JSON containing the full configuration entity, original key, and revision. Categories correspond to implemented store interfaces. |
| `MMQSessions` | `session: string`, `subs: string`, `connected: bool`, `nodeId: string`, `updated: time` | Versioned session metadata and subscription options, with a defined atomic revision/recovery strategy. |

Use legal, collision-safe names such as `MMQConfigs_<encoded-key>` and `MMQSessions_<encoded-client-id>`. A qualified element is `System1:MMQConfigs_<key>.config:_original.._value`; the DPT name is not a system prefix. Keep the original key/client ID in stored data; reject naming-limit overflow or use a collision-checked mapping. Replacing all punctuation with underscores is insufficient.

DP lifecycle calls (`dpCreate`, `dpDelete`, existence checks by DP identity) use the DP name without a trailing dot, e.g. `MMQConfigs_<key>`. Value access always goes through a named element (`.config`, `.type`, `.session`, `.subs`, ...). Never pass an element or config path to a lifecycle call, and never pass the bare DP name as a value address.

Define enumeration by DPT, pagination/batching, maximum JSON size, UTF-8 handling, binary will-payload encoding, schema-version checks, and create/update/delete errors. A get/set/delete ABI alone is insufficient for `GetAll` and `IterateSessions` without an enumeration/index design.

Prevent torn reads and lost updates across `.session`, `.subs`, and metadata: specify a committed revision/snapshot protocol or prove the chosen grouped update is atomic for the required readers. Serialize updates per entity; in redundancy mode define one authoritative writer or an equivalent conflict protocol. Do not silently overwrite an incompatible DPT or unreadable record.

### 4.3 Session and recovery semantics

Handle MQTT 3.1.1 CleanSession separately from MQTT 5 Clean Start and Session Expiry. Transport disconnect does not necessarily delete subscriptions; persistent offline clients still need matching traffic queued. Startup must reconcile stale `connected` flags with actual local connections, session expiry, and ownership.

OA redundancy and backup are useful only for the state actually persisted there and covered by the project configuration. Do not infer a cross-node durability barrier from a local API callback. Validate DP synchronization and export/restore in a real project. Queues, retained messages, MQTT inflight state, and external store backups require their own recovery design.

## 5. Hierarchical Topic Namespace and Subscription Validation

### 5.1 Canonical grammar

The first level after `winccoa/` is always the WinCC OA system name, for the manager's own system as well as for distributed systems. There is no `this`/`local` alias: one element has one topic on every broker, which keeps bridged, archived and multi-broker data unambiguous. (Decision 2026-09-30; replaces the earlier `local`/`this` and `remote/<systemname>` scopes.)

| Topic | Read target |
|---|---|
| `winccoa/System1/tags/Pump101/speed` | `System1:Pump101.speed:_online.._value`, assuming the manager's local system is `System1` |
| `winccoa/System1/types/AnalogDrive/Pump101/speed` | Same local target, after verifying DPT `AnalogDrive` |
| `winccoa/SubstationA/tags/Feeder1/voltage` | `SubstationA:Feeder1.voltage:_online.._value` |
| `winccoa/SubstationA/types/Feeder/Feeder1/voltage` | Same remote target, after verifying DPT `Feeder` |
| `winccoa/System1/tags/ScalarTag` | `System1:ScalarTag.:_online.._value` |
| `winccoa/SubstationA/tags/ScalarTag` | `SubstationA:ScalarTag.:_online.._value` |
| `winccoa/System1/tags/Pump101/speed/_online.._value` | Explicit form of the first local read target |
| `winccoa/SubstationA/tags/Feeder1/voltage/_online.._value` | Explicit form of the remote read target |

The former `winccoa/local|this/...` and `winccoa/remote/<systemname>/...` scopes are superseded, not compatibility aliases. An unknown or disconnected system is unavailable; a remote outage must never fall back to local resolution. `winccoa/<systemname>` itself is the broker status topic of the local system, and `winccoa/<systemname>/cns/...` is reserved. This grammar governs native resolution; existing configured query output keeps its explicit topic contract and must not collide with reserved native/status/CNS topics.

The local system ID must be resolved to its actual name using the selected SDK; do not assume a default-system helper returns a string. Writes use a terminal `/set` and target `:_original.._value`. Do not allow arbitrary config writes through attribute syntax.

Topic-to-DPE conversion joins the DP and element segments with dots and appends a trailing dot only when the result contains no dot (`Pump101/speed` → `Pump101.speed`, never `Pump101.speed.`). A DP-only topic such as `winccoa/System1/tags/Pump101` becomes the root form `Pump101.` only after the resolver confirms that the DPT root is itself a value element. For a structured DPT the struct root is not a value leaf, so the filter or command is rejected rather than connected.

Before implementation, freeze an unambiguous encoding for reserved segments (`tags`, `types`, `cns`, `set`, attribute tokens), slash, percent, wildcard characters, and system/DP/element names. Reject noncanonical forms; preserve case. Define an allowlist of explicit read attributes. Root-element access needs the dot; lifecycle create/delete uses the DP identity. [WinCC OA API messages and root-element naming](https://www.winccoa.com/documentation/WinCCOA/latest/en_US/API/topics/API-08_2.html)

The tag-centric, type-centric, and explicit-attribute forms can share one underlying OA connection, but MQTT topic matching is literal: publish to each subscribed alias or implement documented equivalent routing. A retained value at a canonical topic alone will not reach an alias subscriber. Arbitrary bridge regex/underscore transformations may be lossy and must not be reverse-parsed for writes; keep baseline query output separate from the canonical writable namespace.

### 5.2 Validation and MQTT rules

- Authenticate and apply MQTT ACLs before exposing existence/type information. OA manager credentials must not turn every authenticated MQTT user into an unrestricted OA writer.
- Validate exact native read filters on the manager thread (or against a synchronized, invalidatable catalog); check explicit local/remote scope, system, DPE, attribute, DPT, and authorization. Distinguish nonexistent objects from unavailable remote systems internally.
- Produce one SUBACK result per requested filter before committing its subscription. The current engine has no per-filter result on `OnSubscribe`; design an explicit validation hook/path or prove a safe composition with existing ACL checks. `OnSubscribed` remains a notification for accepted filters.
- Use MQTT 5 `0x87` for denied access and a documented supported failure code such as `0x83` for unavailable validation; MQTT 3.1.1 failures map to `0x80`. Never remove rejected filters from the response or mutate their order.
- `winccoa/<local system>` is the broker-owned status topic (retained JSON) and bypasses DPE parsing while retaining ACL checks.
- Wildcard filters inside the native branches are served by shared `dpQueryConnectSingle` registrations (`+` = one level, `#` = subtree, `types/<T>/...` adds a `_DPT` filter); root filters (no DP name, no type) follow `AllowRootWildcardSubscription` like `#`. Broad filters above the native branches stay ordinary MQTT routing and never create OA registrations. Shared filters targeting native data are rejected. Details and limits: spec section 4.2.
- Invalidate subscriptions/caches when DPs are deleted or renamed, types change, or distributed systems disconnect. Clear stale retained native values according to a documented policy; never replay a deleted DP's value as current.

### 5.3 Future feature: CNS access

Reserve `winccoa/<systemname>/cns/...` for WinCC OA Common Name Service access. The separate [CNS integration plan](plan-winccoa-broker-cns-mqtt-namespace.md) proposes view/tree/node mapping, local and remote scopes, DPE resolution, subscriptions, topology updates, and later optional discovery/writes.

CNS is deferred and is not part of milestones M0–M7 or a prerequisite for releasing native tag/type access. Until enabled by that future feature, explicit CNS subscriptions fail with the documented unsupported/unavailable result, commands are rejected, and the native resolver does not treat `cns` as a system or DPE. Broad MQTT filters do not activate CNS discovery. Protect this reserved branch from external publishes and configured query-output collisions. No CNS catalog, observer, or OA connection is created while the feature is disabled.

## 6. Communication Patterns

### 6.1 Baseline configured queries

Reuse `internal/bridge/winccoa/` address configuration (`query`, `topic`, `answer`, `retained`) and publisher transformations/formats. Compare output with the Kotlin `WinCCOaConnector.kt` implementation under `../main/` in this workspace. Select native transport through host configuration/injection, subject to section 1.2; do not fake a GraphQL endpoint as a native transport selector. *(Removed 2026-09-30, see goal 2.)*

Register `dpQueryConnectSingle` on the manager thread. Confirm the chosen SDK's initial-answer and ongoing-hotlink dispatch mechanism with a minimal live example before finalizing the listener. A plain `WaitForAnswer::callBack(DpMsgAnswer&)` must not be assumed to receive every later update. Use the SDK-supported hotlink handler and capture the returned/confirmed query ID. Treat registration-send failure and asynchronous registration error separately.

Decode complete query tables, including header rows, multiple selected columns, values, timestamps, null/error cases, and initial-answer behavior. Copy data before callback-owned memory expires. Preserve `JSON_ISO`, `JSON_MS`, and `RAW_VALUE` behavior and enqueue publication at the configured topic with QoS 0 and the configured retain flag.

On removal, disable, reload, reconnect, or shutdown, disconnect the query and release the callback exactly once according to verified SDK ownership. No unconditional `delete queryWait` immediately after `dpQueryDisconnect`.

### 6.2 Dynamic subscriptions

Track interests by session generation, client ID, filter, and canonical DPE/attribute. Repeated SUBSCRIBE replaces options and must not increment the count twice. Multiple aliases/clients may share a connection; independent queries and direct connections must not accidentally tear one another down.

On first interest, register `dpConnect`, handle the asynchronous answer, and provide the initial value when required even if it never changes. On last interest removal, call the callback-aware `dpDisconnect` overload matching the original registration and follow SDK ownership. Do not use a no-callback disconnect for a callback registration. Use a supported key/hash for `DpIdentifier`, not an assumed standard-library hash specialization.

A list `dpConnect` fails (error plus `false`) when the list exceeds `Resources::maxConnectMessageSize_` (default 100). Initial restore, reconnect re-registration, and any grouped connect must split interests into batches no larger than the configured limit, track each batch's answer separately, and retry or report failed batches without affecting successful ones.

A connect whose message was sent must be disconnected even if its answer reports an error (stated for `dpConnectExt` in the bundled reference; verify for the other overloads in the selected SDK). Treat a sent-but-failed registration as owned state: disconnect it with the matching callback and release the callback exactly once, rather than dropping it.

Unsubscribe, clean-session removal, expiry, and client takeover update interests. A persistent offline subscription remains an interest when needed for its queued deliveries. Restore and revalidate persistent interests after restart. Do not interpret every network disconnect as unsubscribe.

### 6.3 Writes

A command such as `winccoa/System1/tags/Pump1/speed/set` (local system `System1`) or `winccoa/SubstationA/tags/Pump1/speed/set` with `{"value":1500.0}` becomes a typed write to `System1:Pump1.speed:_original.._value` or `SubstationA:Pump1.speed:_original.._value`, respectively, after ACL, existence, value-type, range, and size validation. Unsupported types and malformed values fail without coercing them to an unrelated type. Define bool, signed/unsigned integer, float, string, time, and dynamic-value handling explicitly.

Reject retained commands to prevent replay on reconnect. Tag OA-origin publications and forwarded commands so values cannot re-enter the command path. Do not run writes directly on a Go callback thread.

The SDK offers a `dpConnect` variant that suppresses hotlinks for values this manager changed. It is an OA-side alternative to broker tagging for loop prevention, but it also hides the confirmed value of an MQTT-originated write from native subscribers on the same manager. Decide and document one policy: either use normal connects and rely on broker origin tagging, so subscribers see written values, or use the suppressing variant and publish accepted writes from the command path. Do not mix both for the same DPE.

Use an asynchronous confirmation callback and distinguish transport acceptance, OA write confirmation, and physical process effect. MQTT QoS does not by itself guarantee exactly-once process actuation. Define a command ID/deduplication and result mechanism before promising retry-safe writes; MQTT packet IDs alone are not global command IDs. Document MQTT 3.1.1 and QoS 0 error reporting, where negative publish acknowledgements are unavailable.

For redundancy, verify OA's actual Event routing and role semantics first; a manager on the passive host must not be presumed unable to write. If forwarding is required, authorize it on both sides, carry a command ID, deadline, and role epoch, reject stale ownership, and provide a bounded failure result when the active node cannot be reached.

## 7. Dual-Node Redundancy and Replication (Conditional Scope)

This milestone requires the explicit single-node exception. The first deliverable remains useful without it.

### 7.1 Role, health, and ownership

Keep OA role, OA partner connectivity, broker peer connectivity, and peer synchronization readiness separate. `isRedundantConnection(manId)` only identifies a connection to a redundant manager pair. The proposed `isReduReplicaConnected()` and `_ReduManager.status` mapping require selected-SDK/project evidence; neither is a verified implementation contract.

Use an explicit state machine such as `UNKNOWN`, `STANDALONE`, `ACTIVE`, `PASSIVE`, with independent `DISCONNECTED`, `SYNCING`, `READY` peer state. Define fencing/ownership for writes and session takeover. A two-node link failure cannot establish exclusive ownership by itself: define the OA-supported authority/fencing mechanism and behavior during a partition before accepting writes on either side.

### 7.2 Replicated state and acknowledgement barriers

OA value/config replication does not replicate broker memory. Inventory and recover all required state: retained messages and deletions, offline queue entries and acknowledgements, session/subscription ownership and expiry, wills, inbound/outbound MQTT inflight phases, delivery progress, and duplicate suppression. Include dynamic OA-topic offline deliveries; equal current datapoint values do not reconstruct missed event history or queues.

For non-OA publishes, use authenticated peer transport (proposed mutual TLS), stable record identities, origin node/epoch, ordered sequence numbers, durable commit acknowledgements, idempotent replay, loop prevention, and bounded buffers. Replicated retained deletes and expiry must survive restart. Do not blindly fan out a replica again or deliver every replica on both nodes to the same logical session.

Gate QoS 1 PUBACK and QoS 2 PUBREC/PUBCOMP at the engine paths identified in section 1.3. `OnPublished` is too late. Define the exact local/peer durability barrier before each positive acknowledgement. Persist the QoS 2 protocol phase and duplicate-handling state, not just payload and PUBREL. Current engine delivery timing must be preserved or deliberately changed with protocol tests. QoS 1 may redeliver; do not promise duplicate-free QoS 1 or exactly-once external effects. [MQTT 5 session state and QoS rules](https://docs.oasis-open.org/mqtt/mqtt/v5.0/mqtt-v5.0.html)

### 7.3 Degraded operation and recovery

The original availability preference is retained as a proposed policy: with no ready peer, acknowledge after local durable commit and report degraded redundancy. Such acknowledgements have a possible data-loss window if that node fails before catch-up. This policy needs an agreed RPO and cannot be presented as unconditional zero-loss failover. Also define strict-mode behavior if the operator requires peer durability.

A broker peer timeout must not hang MQTT processing indefinitely. Distinguish a stopped broker from a healthy OA partner. Do not silently switch to weaker durability midway through a pending request without an explicit policy and observable transition.

After recovery, perform snapshot plus ordered delta replay (or equivalent), including tombstones, queue state, and inflight state. Bound the catch-up log and specify overflow/full-resync behavior. Keep the peer `SYNCING` until a consistent barrier is reached, then enter `READY`. OA Data Manager synchronization cannot fill gaps in non-OA broker state. Retained/current-value equality alone is not sufficient evidence of readiness.

### 7.4 Status topics

Publish retained JSON on `winccoa/<local system>` with node/system identity, role, OA partner connectivity, broker peer connectivity, synchronization readiness, durability mode, and timestamp. Only internal code may publish this topic. With redundancy, both nodes serve the same system, so the status must carry the node identity and a replicated peer status must not overwrite the local node's view.

Publish transitions promptly. A crashed broker cannot publish its own final offline state; document client disconnect/timeout or peer observation as the crash indication. Status is operational information, not a fencing authority.

## 8. C ABI Contract to Finalize Before Coding

The original synchronous get/set/delete callbacks and `void` subscription callback are insufficient. Do not freeze that header as a public ABI. First specify and exercise the following contract in an embedding harness:

| Area | Required contract |
|---|---|
| Versioning | ABI version, struct size, fixed-width fields, calling convention/export visibility, and deterministic rejection of incompatible callers. |
| Instance ownership | An opaque instance handle or an explicitly enforced one-instance-per-process limit; no accidental global cross-talk. |
| Lifecycle | Initialization validation, asynchronous start/stop, queryable state/error, partial-start rollback, idempotent stop, and rejection of operations after shutdown. |
| OA requests | Request ID, operation kind, client/session identity where relevant, deadline, payload, and bounded enqueue result. Operations include resolution, connect/disconnect, query lifecycle, writes, and store enumeration/CRUD. |
| Completion | A nonblocking completion entry point carrying request ID, status, and owned/copied result bytes; handle cancellation, duplicate completion, and late answers after timeout. |
| Subscription decision | A correlated per-filter outcome available before SUBACK and subscription commit, separate from accepted-subscription notifications. |
| Value publication | Topic, explicit payload length, retain/QoS, origin metadata, and enqueue result. Define whether success means accepted or fully published. |
| Errors | Distinct invalid argument/state, not found, unauthorized, unsupported type, timeout, overload, OA failure, and persistence failure codes; recoverable errors do not escape as C++ exceptions or Go panics. |
| Memory | Explicit pointer lifetime, copy/ownership rules, caller-owned output or matching release function, allocation limits, and embedded-NUL-safe payload handling. |
| Redundancy | If approved, one consistently named `monstermq_set_redundancy_state` contract carrying separate role/connectivity/readiness/epoch facts. |

Keep `import "C"` and export shims under the opt-in library package; broker hooks and store adapters use ordinary Go interfaces. C may not retain arbitrary Go strings, slices, or pointers after a call; use copied buffers or documented handles with a defined release lifecycle. [Go cgo pointer rules](https://pkg.go.dev/cmd/cgo#hdr-Passing_pointers)

Validate `c-shared` and/or `c-archive` against the selected Go 1.25 target and the actual OA compiler/linker. A Linux `.so` or `.a` result does not prove Windows DLL/import-library compatibility. Record supported combinations explicitly rather than promising all formats on all platforms. [Go build modes](https://pkg.go.dev/cmd/go#hdr-Build_modes)

## 9. Implementation Milestones and Dependencies

| Milestone | Deliverable | Exit criteria |
|---|---|---|
| M0: Decisions and SDK validation | Section 1.2 decisions; native transport selection; SDK callback/ownership and host-linking proof; namespace/ABI/limits specification. | AC-01 through AC-03; remaining target-dependent unknowns resolved for the selected scope. |
| M1: Embeddable broker and C ABI | Production startup/storage injection; optional library exports; bounded queues and async lifecycle; standalone regression checks. | AC-04 through AC-09. |
| M2: Native manager and baseline queries | Manager lifecycle, initial/live queries, existing payload/transform parity, reconnect and cleanup; existing storage initially. | AC-10 through AC-13. |
| M3: Native datapoint stores | `DeviceConfigStore`, `ArchiveConfigStore`, and `SessionStore`; bootstrap, enumeration, persistence errors, restart/backup behavior. | AC-14 through AC-18. |
| M4: Namespace and dynamic subscriptions | Parser, per-filter validation, aliases, reference management, initial values, persistent/offline interests. | AC-19 through AC-23. |
| M5: Typed writes | Authorization, conversion, confirmation, error/result contract, command replay controls. | AC-24 through AC-26; forwarding deferred to M6. |
| M6: Optional redundancy | Role/fencing model, peer protocol, engine acknowledgement barriers, catch-up, failover, forwarding/status. | AC-27 through AC-33, only after the required exception. |
| M7: Release qualification | Load/fault/soak results, documented limits, packaging and operating guide. | AC-34 through AC-36. |

New deployment configuration must be represented in Go configuration and `yaml-json-schema.json`, and in both deployment examples where applicable. Follow repository parity rules for any persisted schema change. Do not add a dashboard or new device subsystem merely to expose native transport settings.

## 10. Acceptance Criteria

Current status and evidence per criterion: [acceptance-winccoa-native.md](acceptance-winccoa-native.md). Frozen decisions and contracts: [spec-winccoa-native.md](spec-winccoa-native.md). A checkbox is ticked only when the criterion is fully met, including its live WinCC OA part.

All criteria below were **pending** at review time, not claims of completed implementation. Each accepted item needs the tested commit, platform/SDK versions, scenario, expected/actual result, and log/packet-capture/test-report location. Tests belong in `test/integration/` and exercise the real broker listeners. SDK-dependent checks run against the developer-built manager and a real OA project; mocks do not establish OA or failover acceptance.

### 10.1 Scope and design gates

- [x] **AC-01 — Authorized scope:** Record the CGO and optional redundancy decisions, native store compatibility decision, and exact supported feature set. The implementation changes no GraphQL SDL/resolvers or existing database layouts without separately recorded explicit commitment.
- [x] **AC-02 — Verified SDK target:** Record the human-confirmed OA version/patch and target OS/architecture/compiler. A developer-run example demonstrates query initial/live callbacks, connect/disconnect ownership, name/type resolution, and the selected role/health signals. Unsupported assumptions in sections 1.3 and 7.1 are resolved with header/documentation references.
- [x] **AC-03 — Frozen behavior:** Record ABI, topic encoding, wildcard/shared policy, store confirmation semantics, command result semantics, and numeric limits: queue capacity, request timeout, shutdown timeout, maximum payload/record size, maximum interests, connect batch size (≤ `maxConnectMessageSize_`), load target, latency budget, and (if applicable) RPO/RTO. No required limit remains TBD when its milestone is accepted.

### 10.2 Embedding, threading, and lifecycle

- [x] **AC-04 — Standalone compatibility:** `make build`, `make build-arm64`, and `make build-armv7` succeed with their existing `CGO_ENABLED=0` settings. `make test` and `make lint` pass; `make test-race` passes on a supported host. Inspect standalone dependencies to confirm no native-library or OA SDK dependency was introduced.
- [x] **AC-05 — Linkable ABI:** A developer-built C/C++ host links and exchanges messages using the published header for every claimed library/OS/compiler combination. ABI version/size mismatch, null pointers, zero-length/binary payloads, invalid QoS, and oversized inputs return documented results without host termination.
- [x] **AC-06 — Thread confinement and progress:** Instrumented load verifies every OA API call occurs on the manager thread. Startup store reads, runtime writes, OA-to-Go publishing, and shutdown flushes complete while dispatch continues; no cyclic cross-runtime waits or locks block progress.
- [x] **AC-07 — Bounded overload:** Pause OA completion and exceed configured queue capacity. Memory remains bounded; requests fail/expire within the recorded deadline; telemetry drops/coalescing are counted under the chosen policy. Resume processing and verify no expired command is newly executed and no duplicate/late completion corrupts state.
- [x] **AC-08 — Lifecycle failures:** Invalid config, occupied listener port, unavailable OA Event/Data connection, missing/wrong DPT, and failed store load prevent ready status and release partially acquired resources. Termination during initialization and during active traffic finishes within the agreed shutdown bound; repeated stop is safe and no callback uses released host data.
- [ ] *(met except the release-soak part, deferred with AC-35)* **AC-09 — Memory and host integration:** Repeated subscription/reload cycles and the release soak show no unbounded callback/handle/buffer growth. Supported cgo pointer checks and native memory diagnostics report no lifetime violations. SIGINT/SIGTERM stop the host cleanly without breaking the Go runtime's required signal handling: host handlers are installed with `SA_ONSTACK`, the embedded library registers no SIGINT/SIGTERM `signal.Notify`, and signals delivered while Go threads are busy do not crash the process.

### 10.3 Baseline WinCC OA query publishing

- [x] **AC-10 — Initial and live values:** With `answer=true`, a real query publishes its initial matching rows; with `answer=false`, it emits no initial snapshot. Subsequent changes arrive in both cases. An unchanged value does not require an artificial write to initialize a subscriber when an initial answer is requested.
- [x] **AC-11 — Bridge parity:** For identical queries/addresses, native output follows the documented bridge behavior: topic transformations, multiple query columns, timestamps, `JSON_ISO`, `JSON_MS`, `RAW_VALUE`, retain flags, and supported scalar/dynamic types. (A live side-by-side comparison with the GraphQL bridge was removed from scope by the owner on 2026-09-29.) Nulls, malformed results, and unsupported types produce explicit errors rather than crashes or misleading values.
- [x] **AC-12 — Query lifecycle:** Add, disable, remove, and reload addresses during traffic; old queries stop publishing and callbacks are released once. Reconnect to Event with more DPEs than the connect batch limit and verify every batch registers, one live registration per configured address, with the defined initial-answer policy and no registration leaks.
- [x] **AC-13 — Registration failures:** Invalid queries, unauthorized attributes, unavailable remote systems, and asynchronous OA registration errors do not mark a connector connected/ready prematurely. A sent registration whose answer reports an error is still disconnected and its callback released once. Errors are observable and retry/disable behavior follows configured deadlines.

### 10.4 Native storage

- [x] **AC-14 — Full store coverage:** Drive existing broker operations against each OA-backed interface and verify CRUD, enumeration, filtering, toggling, reassignment, subscription options, will metadata, and purge behavior. Database-connection/archive configuration survives restart; other store roles use their explicitly selected providers.
- [x] **AC-15 — Identity and encoding:** Distinct client/entity IDs containing punctuation, Unicode, and long common prefixes cannot collide. Oversized names/JSON are rejected predictably. All metadata and binary will payloads round-trip exactly; storage DPs are created/deleted by DP name without a trailing dot and accessed only through named elements; unsupported record versions and corrupt JSON cannot silently reset data.
- [x] **AC-16 — Commit/error semantics:** Inject denied writes, OA disconnect, timeout, concurrent subscription changes, and termination between record updates. Readers recover a complete committed revision; no newer state is silently overwritten. Every operation claiming durable success meets the recorded confirmation level, including MQTT acknowledgement paths that depend on persistence.
- [x] **AC-17 — Session behavior:** Network tests cover MQTT 3.1.1 CleanSession and MQTT 5 Clean Start/Session Expiry, offline persistent subscribers, restart/reconnect, expiry, client takeover, and last-will state. CONNACK session-present behavior matches restored state; expired sessions and their native interests are removed, and stale connected flags are reconciled.
- [x] **AC-18 — Backup/restore:** Export and restore OA configuration/session datapoints into a prepared test project and verify identity, version, subscriptions, and configuration behavior. The report lists separately restored queue/retained/external state and does not claim complete MQTT recovery from metadata alone.

### 10.5 Namespace and subscriptions

- [x] **AC-19 — Resolution matrix:** Verify every section 5.1 form, nested elements, scalar roots, explicit/default attributes, mandatory local/remote scopes, distributed systems, DPT mismatch, reserved characters/segments, and malformed names. Former unscoped/direct-system forms, missing remote names, and remote names identifying the local system fail deterministically. Identically named local and remote DPEs remain distinct; an unavailable remote system never falls back to local. Unsupported/ambiguous forms cannot address an unintended DPE. Conversion never produces a double trailing dot; a DP-only topic reaches the root form only for DPTs whose root is a value element and is rejected for structured DPTs.
- [x] **AC-20 — Per-filter rejection:** Send one SUBSCRIBE containing valid, missing, unauthorized, unavailable, wildcard/shared, status, and reserved CNS filters under MQTT 3.1.1 and 5. Capture one correctly ordered SUBACK result per filter. Rejected filters leave no MQTT subscription, OA interest, or persisted row; status filters remain usable without DPE lookup. With CNS disabled, explicit CNS filters/commands fail without catalog/observer creation, and external/query-output publication cannot claim the reserved CNS branch.
- [x] **AC-21 — Interest accounting:** Two clients and multiple aliases sharing a DPE create one intended direct OA connection. Repeated SUBSCRIBE replaces options without increasing references; unsubscribe/takeover/expiry remove only their own interests. The last interest disconnects exactly once using the matching callback. Restoring more interests than the connect batch limit registers all of them; a failed batch does not tear down successful batches.
- [x] **AC-22 — Initial, retained, and offline delivery:** Verify a newly accepted exact/alias subscriber receives the defined current value even when unchanged, respecting MQTT retain options. An offline persistent subscriber receives the expected queued changes after reconnect. Non-retained publication and wildcard policy do not accidentally depend on a retained canonical topic.
- [x] **AC-23 — Catalog changes:** Delete/rename/recreate a DP, change its type, and interrupt a dist-link. Stale values/resolutions are invalidated, unavailable writes fail, and reconnection restores only valid interests without leaking old identifiers or retained values.

### 10.6 Commands and authorization

- [x] **AC-24 — Typed writes:** Test every supported value type at valid boundaries and outside range, including malformed JSON, null, invalid UTF-8 where relevant, and oversized payloads. Accepted writes reach only the intended `:_original.._value`; invalid/unsupported writes leave OA unchanged and report the documented result.
- [x] **AC-25 — Access isolation:** Users with read-only, write-only, restricted-topic, and denied permissions cannot bypass policy using aliases, type paths, explicit attributes, shared/wildcard forms, status topics, or native-storage datapoints. Peer forwarding, if enabled, preserves authorization and rejects unauthenticated peers.
- [x] **AC-26 — Command outcomes and replay:** Retained commands are rejected. Verify completion/failure reporting for QoS 0/1/2 and both supported MQTT versions; broker acceptance is distinguishable from OA confirmation. Retry/duplicate command IDs, callback timeout, and OA-origin updates do not cause unintended repeated writes; native subscribers see or do not see the confirmed value of their own writes according to the documented echo policy; any unavoidable uncertain outcome is reported explicitly.

### 10.7 Optional redundancy and failover

- [ ] *(deferred: dual-node exception not granted, 2026-09-29)* **AC-27 — Independent health facts:** Stop only the peer broker while its OA server remains healthy, then perform the converse. Status distinguishes OA health, broker connectivity, and synchronization readiness; an unknown role cannot authorize writes.
- [ ] *(deferred: dual-node exception not granted, 2026-09-29)* **AC-28 — Acknowledgement barriers:** Delay peer commits and capture packets. Positive PUBACK/PUBREC/PUBCOMP cannot precede the configured local/peer durability barrier. Repeat with peer timeout and strict/degraded policy; client waits remain bounded and durability downgrade is observable.
- [ ] *(deferred: dual-node exception not granted, 2026-09-29)* **AC-29 — Crash matrix:** Kill/restart each node before/after local commit, peer commit, PUBACK, PUBREC, PUBREL, PUBCOMP, subscriber delivery, and queue acknowledgement. In synchronized mode, acknowledged state survives according to the agreed RPO. QoS 1 retries are allowed; QoS 2 resumes using correct packet/session phase without duplicate logical delivery. Cover inbound and outbound traffic, retained deletes, wills, expiry, and OA-derived offline queues.
- [ ] *(deferred: dual-node exception not granted, 2026-09-29)* **AC-30 — Recovery barrier:** While the peer is down, publish/update/delete retained data, change sessions/subscriptions, and enqueue/ack offline traffic. Recover the peer with concurrent traffic; snapshots/deltas/tombstones converge without message loops or resurrected state. It remains `SYNCING` until the consistency barrier is verified. Log overflow forces documented full recovery rather than false readiness.
- [ ] *(deferred: dual-node exception not granted, 2026-09-29)* **AC-31 — Partition and ownership:** Partition the peer link and exercise OA role switches, simultaneous reconnect with the same client ID, and commands on both nodes. The agreed fencing model prevents unauthorized dual execution and conflicting session owners; stale role epochs cannot execute commands after a switch.
- [ ] *(deferred: dual-node exception not granted, 2026-09-29)* **AC-32 — Forwarded writes:** With a client on the passive broker, demonstrate the selected native-routing/forwarding path, authorization, active-owner confirmation, deadline failure, and duplicate suppression across retries and failover. Do not accept an old queued command after its deadline or ownership change.
- [ ] *(deferred: dual-node exception not granted, 2026-09-29)* **AC-33 — Status isolation:** Role, connectivity, readiness, and durability changes update retained status within the agreed bound. New subscribers receive current local status; peer replication cannot overwrite the local node's status, and external clients cannot forge it. Crash detection works through the documented external observation mechanism.

### 10.8 Release evidence

- [x] **AC-34 — Performance:** On recorded target hardware, run the agreed concurrent-client, DPE-count, update-rate, payload-size, storage, and redundancy workloads. Report throughput, p50/p95/p99 end-to-end latency, dispatch delay, CPU/RSS, queue high-water marks, and loss/overflow counts. Meet AC-03's budgets; an unmeasured low-latency claim does not pass. (The comparison with the GraphQL bridge was removed from scope by the owner on 2026-09-29.)
- [ ] *(deferred by the owner 2026-09-29: the test project runs on a demo licence that stops it every ~4 h; run later)* **AC-35 — Soak and faults:** Run at least a 24-hour agreed-load soak with client churn and OA reconnects, including peer restart when M6 is in scope. No deadlock, unbounded resource growth, leaked interests, unexplained data loss, or hidden storage errors. Memory plateaus within the agreed budget after warm-up.
- [x] **AC-36 — Operability and compatibility:** Deliver supported-platform build results, manager/project setup, DPT definitions, bootstrap examples, readiness/error diagnostics, backup/recovery steps, durability limitations, and rollback instructions. Configuration validates against the YAML schema; the existing dashboard works with the unchanged GraphQL contract. List any deferred criterion explicitly; do not describe an unqualified M6 as production failover support.

## 11. Review Sources and Remaining Validation

Repository evidence checked:

- [Repository constraints](../../AGENTS.md), [Go toolchain/dependencies](../../go.mod), and [build targets](../../Makefile).
- [Broker construction](../../internal/broker/server.go), [store interfaces](../../internal/stores/interfaces.go), [store aggregate](../../internal/stores/storage.go), and [storage hook](../../internal/broker/hook_storage.go).
- [MQTT acknowledgement and subscription paths](../../internal/mqtt/server.go): `processPublish`, `processPubrel`, `processSubscribe`.
- [Existing OA config](../../internal/bridge/winccoa/config.go), [connector](../../internal/bridge/winccoa/connector_graphql.go), [publisher](../../internal/bridge/winccoa/publisher.go), and [GraphQL contract](../../internal/graphql/schema/winccoa.graphqls).
- Kotlin parity source in this workspace: `../main/broker/src/main/kotlin/devices/winccoa/WinCCOaConnector.kt` and `../main/broker/src/main/resources/schema-{queries,mutations}.graphqls` (the repository guide's `../monster-mq/` is a different checkout layout).
- The invoked `winccoa-api-manager` skill's `references/ManagerAPI.md` (`dispatch`, `dpQueryConnectSingle`, `dpDisconnect`, `dpQueryDisconnect`, `isRedundantConnection`) and `references/SupportingClasses.md` (`WaitForAnswer`, `HotLinkWaitForAnswer`); `winccoa-common` for DP/DPE naming.

Official general references are linked at the relevant decisions above. Online `latest` OA documentation and bundled extracts are guidance, not certification for an unconfirmed SDK. The remaining validation is M0's selected-SDK/API proof, developer-run builds, and the live/network acceptance suite. This document review alone does not satisfy those acceptance criteria.
