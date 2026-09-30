# Plan: WinCC OA CNS Access over MQTT (Future Feature)

## 1. Status and Relationship to the Embedded Manager

**Status: planned for a later release; no implementation in the initial embedded-manager scope.**

Expose WinCC OA Common Name Service (CNS) views and their linked values under `winccoa/cns/...`. Build on the lifecycle, C ABI queues, canonical DPE subscriptions, authorization, and typed-write handling in the [embedded broker plan](plan-winccoa-broker-embedded-manager.md). CNS provides another way to find the same underlying data; it does not require a separate broker or storage backend.

The initial embedded release reserves this prefix and rejects explicit CNS access while the feature is disabled. It does not enumerate CNS, install observers, or open CNS-derived subscriptions. CNS acceptance is independent of the initial release's M0–M7 criteria.

The user has selected the `winccoa/cns/` prefix and deferred implementation. Detailed grammar and rollout below are proposals to finalize before implementation. The OA SDK version/patch and C++ target remain unconfirmed; this document does not claim a tested SDK implementation or approve GraphQL/storage/CGO/clustering exceptions.

## 2. Goals and Scope

1. Let MQTT clients address plant-model nodes without knowing physical datapoint names.
2. Support the same explicit local/remote distinction as native tag/type access, inside the CNS prefix.
3. Reuse one underlying DPE subscription where several CNS nodes or tag/type paths refer to the same value.
4. Keep subscriptions correct when a view/node is renamed, moved, deleted, or linked to a different DPE.
5. Add bounded discovery and wildcard subscriptions after exact value access works; optionally add writes after safe relinking/command semantics are demonstrated.

The first CNS increment reads current values through exact subscriptions to configured views. It does not create/edit/delete CNS trees, expose arbitrary OA configs/properties, traverse all structured datapoint elements implicitly, or query history. CNS engineering remains in WinCC OA. No new GraphQL contract, UI, database schema, or redundancy protocol is needed for this plan.

## 3. CNS Model and SDK Validation

CNS organizes systems into views containing trees. Each tree starts at a root node; nodes can link to datapoints/elements. Addressing uses identifiers rather than translated display names, for example `System1.Plant:Line1.Pump101.Speed`; a view path ends in a colon (`System1.Plant:`). This distinction keeps MQTT addresses independent of display language. [Official CNS model and addressing](https://www.winccoa.com/documentation/WinCCOA/latest/en_US/CNS/cns-01.html)

Resolve the node's linked data before choosing a value operation. A node can exist without a linked datapoint; existence alone does not establish a readable value. Inspect the link's data-identifier type as well as the target, rather than treating every node as a scalar DPE. [Official node-link resolution semantics](https://www.winccoa.com/documentation/WinCCOA/latest/en_US/CNS_Ctrl/cnsGetId_2.html)

The API-manager reference exposes `Manager::getCNS()` returning the manager-global `CommonNameService` interface. Before coding, inspect that interface in the human-confirmed SDK and demonstrate:

- Enumerating allowed views, tree roots, and child identifiers, including remote-system availability.
- Resolving a node link and data-identifier type to a concrete, system-qualified DPE.
- Receiving topology/link changes, obtaining a consistent startup catalog, and releasing observers correctly.
- Checking access restrictions and handling startup, disconnect, and reconnect.

CTRL functions such as `cnsGetId` and `cnsAddObserver` describe required behavior, not C++ method signatures. Confirm C++ equivalents, thread affinity, readiness, callback ownership, and distributed change coverage against the selected SDK. All calls stay on the manager thread through the existing bounded request queue.

## 4. Proposed MQTT Namespace

### 4.1 Value topics

```text
winccoa/cns/local/<view>/<root-node>/<child-node>/...
winccoa/cns/remote/<systemname>/<view>/<root-node>/<child-node>/...
```

Assume the manager's local system is `System1` and the example CNS links already exist:

| MQTT topic | CNS node | Linked read target |
|---|---|---|
| `winccoa/cns/local/Plant/Line1/Pump101/Speed` | `System1.Plant:Line1.Pump101.Speed` | `System1:Pump101.speed:_online.._value` |
| `winccoa/cns/local/Plant/Line1/Temperature` | `System1.Plant:Line1.Temperature` | `System1:Temperature.:_online.._value` for a scalar-root link |
| `winccoa/cns/remote/SubstationA/Electrical/Feeders/Feeder1/Voltage` | `SubstationA.Electrical:Feeders.Feeder1.Voltage` | `SubstationA:Feeder1.voltage:_online.._value` |

`Plant` and `Electrical` are view IDs; `Line1` and `Feeders` are root-node IDs. Remaining levels are actual CNS child IDs. Every node level is resolved through CNS; never infer `Pump101.speed` by replacing slashes in the MQTT path with dots.

`this` selects the manager's system for CNS lookup. `remote/<systemname>` selects another system and never falls back to local. Reject the local system's name in the remote branch, consistent with native tag/type access. The system containing the CNS node and the system containing its linked DPE are separate facts; if cross-system links are supported by the selected SDK, validate and authorize both.

Use CNS ID segments in topics; display names are optional metadata. Share the native namespace's canonical segment-encoding utility. Before C0 exits, freeze encoding for slash, percent, MQTT wildcard characters, and reserved terminal names (`set`, `$meta`, `$children`) so an actual node with such an ID cannot be interpreted as an operation. Encode/decode each segment exactly once; reject alternate encodings and malformed UTF-8, preserving case. Assemble OA paths through validated SDK identifiers, not user-text concatenation.

The initial CNS read form always targets `:_online.._value`; no arbitrary attribute suffix is accepted. Only a node whose link resolves to a supported value-bearing DPE (including a scalar root) can be read. An unlinked grouping node or a link to a structured root without a supported scalar value has no implicit value topic. For this increment, model readable leaves as explicit CNS nodes; adding DPE suffixes or structure expansion requires a later grammar decision.

### 4.2 Relationship to native tag/type topics

The example Speed node and `winccoa/this/tags/Pump101/speed` resolve to the same underlying value but remain different MQTT topics. Register the DPE once in the shared interest registry where possible and fan out to each authorized, interested path. Reference ownership must include the CNS mapping revision; removing a CNS alias must not remove a direct tag subscription.

Use the baseline native value-payload contract and retained policy. Do not apply lossy query-bridge regex or underscore transformations to CNS identifier paths. Different views may legitimately expose one DPE at different paths.

### 4.3 Later discovery and wildcard access

After exact reads, propose read-only metadata topics:

```text
winccoa/cns/local/<view>/$meta
winccoa/cns/local/<view>/$children
winccoa/cns/local/<view>/<node-path>/$meta
winccoa/cns/local/<view>/<node-path>/$children
```

The remote equivalents use `winccoa/cns/remote/<systemname>/...`. Metadata describes kind, canonical CNS path, optional display names, authorized link information, mapping revision, and availability. Child listings contain immediate child IDs, not a recursive dump. Freeze payload version, byte/child limits, and an explicit oversize result before implementation; pagination, if needed, requires a separately specified request/result contract. Never truncate a listing and present it as complete.

Metadata is optional and independently authorized. Do not retain a shared unfiltered catalog that exposes hidden child names or linked DPEs to other users. If visibility differs per principal, defer shared retained listings until an authorization-safe delivery design is demonstrated. Views are configured in the initial release; automatic discovery of all systems/views is not required.

Later wildcard subscriptions, for example `winccoa/cns/local/Plant/Line1/#`, match MQTT paths in a bounded authorized catalog. Do not translate MQTT `+`/`#` directly into OA query wildcard syntax. Specify metadata inclusion, future-node discovery, empty-match behavior, expansion limits, per-session accounting, and shared-subscription behavior before enabling them. Broad ordinary MQTT subscriptions such as `#` must not trigger a full CNS scan or unlimited DPE registration.

## 5. Resolution and Subscription Flow

1. Authenticate and authorize the requested CNS topic and configured scope/view before catalog lookup.
2. Parse and decode its local/remote scope and ID segments; resolve the existing CNS node on the OA manager thread.
3. Resolve the linked system/DPE and identifier type. Normalize scalar-root access correctly; reject grouping nodes, unsupported targets, missing links, and unavailable systems for value subscriptions.
4. Apply the same underlying DPE access policy used by native tag/type access, plus CNS/view restrictions. A new view must not provide a route around a denied target. Define this target-level policy if native topic ACLs alone cannot express it.
5. Record `(CNS path, topic, resolved DPE, mapping revision, session generation)` and obtain an interest in the shared canonical DPE registry.
6. Complete per-filter subscription validation before SUBACK/commit, with rollback on failure. Publish the initial confirmed value and subsequent changes on the requested CNS topic, using the base plan's bounded queues, delivery rules, and retain options.
7. On unsubscribe, expiry, takeover, or shutdown, release only that mapping's interests. Persistent offline interests follow the base plan so required offline traffic is still queued.

Do not browse CNS per publication. Keep bounded forward mappings from CNS path to DPE and reverse mappings from DPE to interested topics. Account for catalog size, alias fan-out, subscription count, and event-queue limits in configuration and metrics.

## 6. Topology Changes, Retained Values, and Recovery

Prefer the selected SDK's change observer over periodic full enumeration. OA documents CNS observers for structural/data changes; the C++ delivery and remote-system behavior still need proof. [Official CNS observer behavior](https://www.winccoa.com/documentation/WinCCOA/latest/en_US/CNS_Ctrl/cnsAddObserver.html)

On startup, reconcile a bounded catalog snapshot with changes received during enumeration before declaring it ready. Observers only enqueue invalidations/revisions; they must not block dispatch with tree walks. Coalesce changes and reconcile affected subtrees. If observer events are unavailable or overflow, mark the catalog stale and run a bounded rescan before restoring readiness. Record the refresh interval/staleness limit if polling is necessary.

| Change | Planned behavior |
|---|---|
| Display-name change | Update permitted metadata; identifier-based MQTT value topics stay unchanged. |
| ID rename or node move | Treat as old-path removal and new-path creation. Remove old retained data. Exact subscriptions remain subscriptions to the old MQTT name and receive no silent redirect; an enabled wildcard may acquire the new path. |
| Link changes to another DPE | Invalidate the old mapping generation, clear its retained value, release its interest, resolve/authorize the new target, and establish its initial value before resuming. Drop late callbacks from the old generation. |
| Node/view/link deletion or DPE deletion | Remove affected mappings/interests and clear retained value/metadata. Preserve unrelated aliases and direct tag/type subscriptions. |
| Remote disconnect or uncertain catalog | Mark affected mappings unavailable; reject new resolutions/writes and stop treating cached values as current. Clear retained current values under the agreed base-plan policy; reconcile before resuming. |
| Reconnect/restart | Revalidate mappings, targets, authorization, and persistent interests; reconcile previously owned retained topics so removed nodes cannot reappear as current. |

Track owned CNS retained topics using existing broker storage/catalog facilities; define a bounded cleanup strategy without inventing a new database schema. A failed cleanup cannot produce a ready/current claim for stale data. If the selected SDK cannot order topology and value events reliably, invalidate and re-establish the affected subscription conservatively.

Offline queues may already contain historical messages under the old path. Define them as historical accepted publications; never relabel them as values from a newly linked target or replay them into writes. Record a mapping generation/source identity in the delivery contract, or adopt a documented purge policy, before claiming unambiguous delivery across relinking. Do not silently change the base payload contract to add this metadata.

## 7. Optional Later Writes through CNS

After exact-read and change-handling acceptance, propose terminal `/set`:

```text
winccoa/cns/local/Plant/Line1/Pump101/Speed/set
winccoa/cns/remote/SubstationA/Electrical/Feeders/Feeder1/Voltage/set
```

Resolve a value-bearing node to the same typed `:_original.._value` write used by native tag/type commands. Preserve command ID, deadline, origin, principal, and confirmed target/mapping generation. Recheck generation and authorization immediately before issuing the write; if a node was relinked while the command waited, fail it rather than silently writing to either a stale or unintended new target.

Reuse the base plan's range checks, retained-command rejection, result reporting, deduplication, uncertain-outcome reporting, and optional active-owner forwarding. A CNS path is an alias, not a stronger delivery guarantee. CNS commands never create/change tree nodes or links. Keep writes disabled until their own criteria pass; metadata nodes and unlinked/structured grouping nodes are not writable.

## 8. Integration and Rollout

Select the feature and permitted scope/views in host configuration; avoid new GraphQL fields/resolvers or a new public device type. Document any proposed settings in the relevant config parser/schema/examples according to repository conventions. Do not extend existing store schemas. The base CGO exception and any redundancy exception remain prerequisites inherited from the embedded manager, not newly granted here.

| Milestone | Deliverable and dependencies | Acceptance |
|---|---|---|
| C0: SDK and contract proof | After native lifecycle/ABI and exact DPE subscriptions work, verify the selected CNS C++ interface; freeze grammar/encoding, target ACL policy, payload/queue/relink semantics, numeric bounds, and enabled views. | CNS-01, CNS-02 |
| C1: Exact read access | Local and remote exact CNS resolution, shared DPE interests, initial/live values, authorization, topology invalidation, and restart cleanup. Disabled by default until qualified. | CNS-03 through CNS-08, CNS-11, CNS-12 |
| C2: Discovery and wildcards | Optional bounded metadata browsing and wildcard expansion with dynamic-node handling. Exact reads can ship independently. | CNS-09 plus applicable C1 regression criteria |
| C3: Writes | Optional typed `/set` commands, mapping-generation checks, results and forwarding only where already supported by the base feature. | CNS-10 plus applicable C1 regression criteria |

Do not couple CNS rollout to broker replication. Single-node CNS must work independently. If redundancy is enabled later, each node reconstructs mappings against its OA view and uses the existing ownership/delivery mechanism; it does not replicate callbacks or build a second peer protocol.

## 9. Acceptance Criteria

All criteria are pending. Use the real broker listeners and a developer-built C++ manager connected to a test OA project; record commit, SDK/OS/compiler, fixture topology, expected/actual behavior, and logs. Optional C2/C3 criteria apply only when those increments are enabled.

- [ ] **CNS-01 — SDK proof:** Demonstrate C++ view/tree/node enumeration, link/type resolution, topology observation and cleanup, remote disconnect/reconnect, and startup consistency using the human-confirmed SDK. Record unsupported capabilities and any bounded polling fallback; CTRL examples alone do not satisfy this criterion.
- [ ] **CNS-02 — Grammar and isolation:** Resolve all section 4 examples with colliding local/remote node names, scalar roots, Unicode and reserved IDs. Display-language changes leave topics unchanged. Malformed/alternate encodings and a local system addressed through `remote` fail; no remote failure falls back locally. Disabled CNS creates no catalogs/observers/interests and rejects explicit access without changing tag/type/status behavior.
- [ ] **CNS-03 — Target resolution:** Test value-bearing DPEs, scalar roots, unlinked grouping nodes, structured roots, dangling links, and unsupported identifier types. Reads reach only the actual linked DPE; the path text is never interpreted as a datapoint name. Apply the defined authorization/error behavior to cross-system links if supported.
- [ ] **CNS-04 — Values and reference ownership:** Two CNS nodes and one native tag topic linked to the same DPE share the intended underlying registration. Each interested topic receives initial/live values according to the base contract; repeated SUBSCRIBE does not leak interests. Removing one path leaves the others functional; the last interest disconnects once.
- [ ] **CNS-05 — Access policy:** A user denied a target cannot recover read/write access through another CNS view, alias, remote scope, or wildcard. Existence, link metadata, and child listings do not leak unauthorized information. External publishers and configured query output cannot forge CNS-owned values/metadata.
- [ ] **CNS-06 — Topology changes:** Rename, move, delete, and relink nodes during live updates. Old paths/retained values are removed, old-generation callbacks cannot publish into the new mapping, exact subscriptions are not redirected, and new mappings are reauthorized. Display-name edits affect only metadata.
- [ ] **CNS-07 — Persistent sessions and restart:** Offline subscriptions queue the defined traffic; reconnect restores valid interests. Restart after missed topology events removes orphan retained topics and stale mappings. Historical queued messages follow the agreed relink policy and are never misrepresented as current data from a new target.
- [ ] **CNS-08 — Failure and resynchronization:** Lose a remote system, interrupt catalog initialization, overflow the observer queue, and restore connectivity. Affected mappings become unavailable within the agreed bound; bounded reconciliation restores one valid registration per interest, with no false readiness, stale retained replay, or leaked observers.
- [ ] **CNS-09 — Optional discovery/wildcards:** Metadata distinguishes grouping/value-bearing nodes and uses bounded authorized child listings. Oversize catalogs return the specified incomplete/error result. Wildcard matching, future nodes, empty matches, metadata inclusion, and shared filters follow the frozen contract; broad `#` cannot initiate unlimited scans. Removed nodes clean up interests and metadata.
- [ ] **CNS-10 — Optional writes:** Valid typed commands write the resolved target and return the base command result. Retained, unauthorized, unsupported, expired, duplicate, and relinked-in-flight commands cause no unintended write. Role changes/forwarding use existing ownership rules and never redirect a queued command to a newly linked target.
- [ ] **CNS-11 — Capacity and observability:** Before load tests, record maximum catalog size, depth, fan-out, interests, queue bytes, refresh/staleness interval, and latency/memory budgets. At those limits, sustained updates plus topology churn meet the budgets; overload is bounded and observable. Dispatch remains responsive and there is no CNS enumeration per value publication.
- [ ] **CNS-12 — Release compatibility:** Document supported SDK/platform, configuration, encoding, examples, unknown/stale/error semantics, restart cleanup, and enabled increments. Base broker tests pass; GraphQL SDL/resolvers, existing DB layouts, and standalone pure-Go builds remain compatible. Disabling CNS tears down its observers/interests and clears or invalidates its owned retained data without disturbing other namespaces.
