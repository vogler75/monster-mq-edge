# Spec: WinCC OA Native Embedding (frozen behavior for AC-01..AC-03)

Companion to [plan-winccoa-broker-embedded-manager.md](plan-winccoa-broker-embedded-manager.md).
This document records the decisions and frozen contracts the plan's M0 exit
criteria require. Changing anything here after a milestone is accepted needs a
new decision entry.

## 1. Decisions (AC-01)

Recorded 2026-09-29 by the repository owner.

| Decision | Outcome |
|---|---|
| CGO exception | Approved for the opt-in embedding library only (`embed/cabi`, build tag `winccoa_embed`). `make build`, `build-arm64`, `build-armv7` stay `CGO_ENABLED=0` and must not import that package. |
| Dual-node exception | Not required since 2026-10-03 (AGENTS.md rule removed); PeerLink is M6a; AC-27..AC-33 stay deferred. No redundancy ABI is exported. |
| Scope | M0..M5: embedding, native queries, native datapoint stores, namespace/subscriptions, typed writes. |
| Native storage | New OA datapoint layout (`MMQConfigs`, `MMQSessions`) is approved as an opt-in store for device config, archive/database-connection config and sessions. Existing SQLite/Postgres/MongoDB layouts are unchanged. The Java broker is not required to read the OA layout. |
| GraphQL contract | Unchanged. No SDL or resolver change. Native transport is selected by host bootstrap configuration (section 6), not by a GraphQL field. |
| Availability/durability | Native scope single node; non-OA publishes may be forwarded by PeerLink (M6a, RPO per plan-peerlink 15.4). Store success means OA-confirmed `dpSet` answer (section 5). |
| PeerLink (M6a, recorded 2026-10-03) | In-memory, pull-based forwarding of publishes between MonsterMQ Edge brokers ([plan-peerlink.md](plan-peerlink.md)). It supersedes only the "non-OA publishes" paragraph of [plan-winccoa-broker-embedded-manager.md](plan-winccoa-broker-embedded-manager.md) §7.2, and only for in-memory forwarding. AC-27..AC-33 stay deferred; AC-28 (PUBACK barrier) and AC-30 (full recovery on overflow) are explicitly replaced by plan-peerlink sections 15 and 8.5. While native mode is active, `<TopicRoot>` is never forwarded and each node's status stays local. The entry also covers the optional `peerLink` object in the native status JSON (plan-peerlink 20.2). No ABI, C++ manager or SDL change. |

## 2. Verified SDK target (AC-02)

| Item | Value |
|---|---|
| WinCC OA | 3.21, API at `/opt/WinCC_OA/3.21/api` (confirmed by the owner; patch level to be recorded from the live project's `WCCILpmon -version`) |
| OS / arch | Debian GNU/Linux 12 (bookworm), aarch64 |
| Compiler | system gcc/g++ (Debian 12), C++17 (`CMAKE_CXX_STANDARD 17` from `CMakeDefines.txt`) |
| Go | 1.25 language level (`go.mod`); built with the installed Go toolchain |
| Test project | `~/WinCC_OA_Proj/Test321` |
| Build mode | `c-archive` linked into the manager executable. `c-shared` and Windows are not claimed. |

Header facts checked in `/opt/WinCC_OA/3.21/api/include/Manager/Manager.hxx`:

- `dpQueryConnectSingle(const CharString&, PVSSulong &queryId, bool values, WaitForAnswer*, bool del)` returns only "message sent"; the answer message must be processed for success (line ~1805).
- `dpQueryDisconnect(PVSSulong id, WaitForAnswer*)`: "the corresponding wait object will be deleted by the framework anyway" (line ~1975). The manager never deletes a query callback after disconnect.
- `dpConnect(const DpIdentList&, WaitForAnswer*, bool del)` fails with an error and `false` above `Resources::maxConnectMessageSize_` (default 100).
- `dpConnectNoSource(...)` suppresses hotlinks for values this manager changed.
- `dpDisconnect(const DpIdentList&, const HotLinkWaitForAnswer*)`: "The object is deleted by the framework." The no-callback overloads are "Only for dpConnects without an waitForAnswerObject".
- `getSystemName(CharString&)` returns the local system name; `getSystemId(const CharString&, SystemNumType&)` resolves a remote one.
- `isReduReplicaConnected()` is not used (M6 deferred).

Demonstrated live by the probe (acceptance record, AC-02): `del=true` keeps a hotlink callback after the answer and the framework deletes it on disconnect (`del=false` leaks it); `values=false` suppresses the initial query rows; query answers and hotlinks carry `[uint queryId, dyn_dyn_anytype table]`.

## 3. C ABI (header `embed/cabi/monstermq.h`)

- ABI version `MMQ_ABI_VERSION = 1`. Every struct begins with `uint32_t struct_size`; the library rejects a smaller size or a different major ABI with `MMQ_E_ABI`.
- Fixed-width integers only; default C calling convention; symbols exported with default visibility.
- One broker instance per process, enforced: a second `mmq_create` returns `MMQ_E_STATE`. The handle is opaque (`uint64_t`).
- Lifecycle: `mmq_create` (validate config, no listeners) -> `mmq_start` (asynchronous; poll `mmq_state`) -> `mmq_stop(timeout_ms)` (asynchronous, idempotent) -> `mmq_destroy` (only after state `STOPPED` or `FAILED`). Calls after destroy return `MMQ_E_STATE`.
- Go -> host requests: the host registers `submit(user, const mmq_request*)`. It is called on arbitrary Go threads, must copy the request and return immediately (`MMQ_OK` or `MMQ_E_OVERLOAD`). The request buffer is valid only during the call. An optional `wake(user)` callback lets the host interrupt its dispatch wait.
- Host -> Go completion: `mmq_complete(handle, request_id, status, data, len)`. Non-blocking, copies `data`. Unknown, duplicate or late (after deadline) completions are counted and ignored and return `MMQ_E_NOT_FOUND`. When that happens for a successful `QUERY_CONNECT` or `DP_CONNECT`, nobody owns the registration: the host disconnects it immediately (with the registration's own callback) instead of leaking it.
- The host drops a request whose deadline has passed before it reaches the manager thread and completes it with `MMQ_E_TIMEOUT` without executing it, so an expired write is never executed late.
- A sent `QUERY_CONNECT`/`DP_CONNECT` whose OA answer reports an error is disconnected by the host with the matching callback before the error completion is delivered (spec of the embedded-manager plan, section 6.2).
- Host -> Go events: `mmq_event(handle, ref, data, len)` for hotlink/query data of a registered subscription reference. Non-blocking; returns `MMQ_E_OVERLOAD` when the event queue is full (the host counts it; the drop policy is section 7).
- Query tables larger than half of `MMQ_MAX_MESSAGE` are split into several events; every chunk repeats the header row, and all but the last chunk of an initial answer carry `FlagMore` (bit 3).
- Payload encoding: the TLV format of section 3.1 in both directions. Payloads are length-delimited; embedded NULs are allowed.
- No Go pointer is retained by C and no C pointer is retained by Go after a call returns.
- Status codes: `MMQ_OK 0`, `MMQ_E_INVALID -1`, `MMQ_E_STATE -2`, `MMQ_E_NOT_FOUND -3`, `MMQ_E_UNAUTHORIZED -4`, `MMQ_E_TYPE -5`, `MMQ_E_TIMEOUT -6`, `MMQ_E_OVERLOAD -7`, `MMQ_E_OA -8`, `MMQ_E_PERSIST -9`, `MMQ_E_ABI -10`, `MMQ_E_TOO_LARGE -11`, `MMQ_E_UNAVAILABLE -12`. Go panics are recovered at every export and reported as `MMQ_E_STATE`; C++ exceptions never cross the boundary.

### 3.1 TLV message format

A message is a sequence of fields: `u8 tag`, `u32 little-endian length`, `length` bytes. Unknown tags are skipped. Integers inside fields are little-endian. Tags:

| Tag | Name | Content |
|---|---|---|
| 1 | op | u32 operation code |
| 2 | name | UTF-8 datapoint / DPE / type / system name (repeatable) |
| 3 | query | UTF-8 dpQuery string |
| 4 | flags | u32 bit flags |
| 5 | value | typed value (section 3.2), repeatable |
| 6 | ref | u64 subscription/query reference |
| 7 | error | UTF-8 error text |
| 8 | row | nested TLV of `value` fields (one query-table row) |
| 9 | typename | UTF-8 DPT name |
| 10 | elemtype | u32 OA element type id |
| 11 | sysname | UTF-8 system name |
| 12 | exists | u8 0/1 |
| 13 | time | i64 unix milliseconds |
| 14 | count | u32 |

Operations (tag 1): `RESOLVE 1`, `SYSINFO 2`, `QUERY_CONNECT 3`, `QUERY_DISCONNECT 4`, `DP_CONNECT 5`, `DP_DISCONNECT 6`, `DP_SET 7`, `DP_GET 8`, `DP_NAMES 9`, `DP_CREATE 10`, `DP_DELETE 11`, `TYPE_CHECK 12`.

### 3.2 Typed values

Element types reported by `RESOLVE`/`TYPE_CHECK` use the same numbers as value kinds, with `0` for a structure node (not a value element) and `255` for an element type the embedding does not support.

`u8 kind` followed by the body: `0 null`, `1 bool (u8)`, `2 int (i64)`, `3 uint (u64)`, `4 float (f64)`, `5 string (UTF-8)`, `6 time (i64 unix ms)`, `7 bytes (blob)`, `8 dyn (nested TLV of value fields)`, `9 langtext (UTF-8 of the active language)`, `10 bit32 (u32)`.

## 4. Topic namespace (frozen)

- Native branches: `winccoa/systems/<system>/tags/<dp>[/<elem>...][/<attr>]` and `winccoa/systems/<system>/types/<dpt>/<dp>[/<elem>...][/<attr>]` for every system, the local one included. `winccoa`, `systems`, `tags` and `types` are the defaults of `WinCCOaNative.TopicRoot`, `SystemsName`, `TagsName` and `TypesName` (section 6); everything in this section applies to the configured names. The root must not start with `$` or contain wildcards or empty levels; `SystemsName`/`TagsName`/`TypesName` are single levels, must differ and must not be `cns`. Invalid names stop the broker at startup.
- Local shortcut (`LocalShortcut: true`, default): the local system is also reachable without the system part, `winccoa/tags/...`, `winccoa/types/...` (reserved `winccoa/cns/...`), broker status also on `winccoa`. A shortcut topic and its explicit form are aliases of one element like the tags and types forms: one registration, publication to every subscribed form, writes through both; ACLs must allow the explicit tags form as well. Wildcard filters in both forms share one query per form. `LocalShortcut: false` rejects the shortcut forms (`0x8F`). `winccoa/+/...` and `winccoa/systems/+/...` are ordinary MQTT filters.
- Broker-owned: `winccoa/systems/<local system>` carries the broker status as retained JSON (`nodeId`, `system`, `oa`, `ready`, `role`, `timestamp`). Reserved: `winccoa/systems/<system>/cns/...` (disabled).
- Segment encoding: percent-encoding with uppercase hex. Only `%`, `/`, `+`, `#`, NUL, and characters below 0x20 are encoded, except that a segment spelling a reserved token (`set`, an attribute token) is written with its first character encoded to address an element of that name. Any other percent-escape or lowercase hex is noncanonical and rejected. Decoded names may not contain `.`, `:`, or control characters, and may not be empty. Case is preserved.
- Attribute allowlist (read): `_online.._value` (default), `_online.._stime`, `_online.._status`, `_online.._invalid`. A terminal segment starting with `_` that is not in the allowlist is rejected.
- Command: terminal `set` after the element path; the write target is always `:_original.._value`. Reserved tokens are reserved only in the terminal position: `.../Pump1/set/_online.._value` reads an element named `set`, and a terminal element named `set` is written `%73et`.
- DPE building: segments are joined with `.`; a trailing `.` is appended only when the joined name has no dot. A DP-only path resolves to `<dp>.` only when the root element of its DPT is a value element; otherwise the filter/command is rejected (`not a value element`).
- A system other than the local one is resolved remotely. Unknown or disconnected systems are unavailable (`0x83`) and never fall back to local.
- Filters: shared subscriptions (`$share/<g>/...`) targeting native branches are rejected. Broad filters above the native branches (`#`, `winccoa/#`, `winccoa/+/...`) are ordinary MQTT filters: they receive native publications caused by other subscribers but never create OA registrations. Wildcards inside a native branch are served by queries (section 4.2).

### 4.1 SUBACK codes

| Case | MQTT 5 | MQTT 3.1.1 |
|---|---|---|
| Accepted | granted QoS | granted QoS |
| ACL denied | `0x87` | `0x80` |
| Malformed / noncanonical / DPE missing / not a value element / type mismatch | `0x8F` | `0x80` |
| OA or remote system unavailable, validation timeout | `0x83` | `0x80` |
| Wildcard filter that cannot be expressed as a query, or root filter while `AllowRootWildcardSubscription` is false | `0x8F` | `0x80` |
| Wildcard query limit reached | `0x83` | `0x80` |
| Shared filter in native branch | `0x9E` | `0x80` |
| CNS branch (disabled) | `0x83` | `0x80` |

Order and count always match the SUBSCRIBE packet. Rejected filters create no MQTT subscription, no OA interest, no persisted row.

### 4.2 Wildcard filters

A native filter with `+` or `#` after `tags/` or `types/` becomes one `dpQueryConnectSingle` (`SELECT '_online.._value', '_online.._stime' FROM '<pattern>' [WHERE _DPT = "<type>"] [REMOTE '<system>']`), shared by every subscription with the same filter semantics:

| Filter | Query pattern |
|---|---|
| `winccoa/systems/<sys>/tags/#` | `'*.**'` |
| `winccoa/systems/<sys>/tags/Pump1/#` | `'Pump1.**'` |
| `winccoa/systems/<sys>/tags/Pump1/value/#` | `'{Pump1.value,Pump1.value.**}'` |
| `winccoa/systems/<sys>/tags/+/speed` | `'*.speed'` |
| `winccoa/systems/<sys>/tags/+` | `'*.'` (scalar roots) |
| `winccoa/systems/<sys>/types/Pump/#` | `'*.**' WHERE _DPT = "Pump"` |
| `winccoa/systems/<sys>/types/Pump/+/value/#` | `'{*.value,*.value.**}' WHERE _DPT = "Pump"` |
| `winccoa/systems/<sys>/types/#`, `types/+/...` | any type; rows are published under their DP's type |
| `<sys>` is not the local system | `... REMOTE '<sys>'` |

- `+` matches exactly one name level (`*`), a trailing `#` matches the level and everything below (`**`, which also includes the root of a scalar DP). Partial-level wildcards do not exist in MQTT; names containing OA pattern characters (`*?[]{},'"`), attribute segments and names starting with `_` are rejected (`0x8F`).
- Rows are published as `{"time","value"}` to the exact topic of each element in the filter's form (`tags/...` or `types/<DPT>/...`), QoS 1, not retained. Internal (`_`) and store (`MMQ*`) datapoints are never published.
- Current values: the initial query answer (possibly split into several events, `FlagMore`) is delivered to the subscribers that are waiting; later subscribers of the same filter get the cached current values at once. Retain handling applies as for exact filters.
- One change is published once: a query does not publish a topic that has an exact native subscription (that path publishes it), and overlapping queries are deduplicated by value and `_online.._stime`.
- Datapoints created after the subscription are reported by the running query (verified on 3.21); deleted ones simply stop.
- Root filters (no datapoint name and no type: `tags/#`, `tags/+/...`, `types/#`, `types/+/...`) are governed by `AllowRootWildcardSubscription` like `#`: when it is `false` they are rejected with `0x8F`. The broker-wide `#` is rejected with `0x8F` as in the Java broker.
- Limits: at most 1000 distinct wildcard queries; above that `0x83`. The last unsubscribe disconnects the query; a lost remote system disconnects its queries and a returning one registers them again.
- External publishes to any `winccoa/systems/<system>[/...]` topic other than a `.../set` command are rejected, except a retained empty payload on `winccoa/systems/<system>` for a system other than the local one (clears a stale status, e.g. after a system rename). Configured query output topics must not resolve into `winccoa/...` (validated at connector start).

## 5. Store semantics

- A store write succeeds only after the OA answer to `dpSet` with a `WaitForAnswer` reports no error (`confirmation level: OA event manager accepted`). A plain queued `dpSet` is never reported as success.
- `MMQConfigs` elements: `config` (string, JSON envelope `{"v":1,"kind":..,"key":..,"rev":n,"data":{...}}`), `type` (string, category), `updated` (time). `MMQSessions`: `session` (string, JSON envelope with session info and subscriptions), `subs` (string, unused in v1, kept for layout stability), `connected` (bool), `nodeId` (string), `updated` (time). Session info and subscriptions are written in one grouped `dpSet` of the `session` element, so a reader always sees one committed revision.
- DP names: `MMQConfigs_<enc>` / `MMQSessions_<enc>` where `<enc>` is `k` + lowercase hex of the SHA-256 of the key, first 24 hex digits, plus a collision check against the stored original key. DP lifecycle calls use this name without a trailing dot; value access uses the named elements.
- Maximum envelope size: 256 KiB. Larger records are rejected with `MMQ_E_TOO_LARGE`. Binary will payloads are base64 in the envelope.
- Enumeration uses `DP_NAMES` with the DPT filter, in batches of at most 1000 names per request.
- Users (`UserStoreType: WINCCOA`): one `MMQUsers_<enc>` datapoint per user (`<enc>` from the user name). Elements: `user` (string, checked against the name on load), `passwordHash` (string, bcrypt), `enabled`, `canSubscribe`, `canPublish`, `isAdmin` (bool), `acl` (string, JSON list of the user's rules `{id, topic, subscribe, publish, priority, created}`, rule ids are UUIDs), `created`, `updated` (time). Every change writes all elements in one confirmed `dpSet`; deleting a user deletes the datapoint and its rules. Rules are ordered by priority (highest first), then by creation. `MMQUsers_*` are protected like the other store datapoints.
- Datapoint types: at startup the broker sends `TYPE_CHECK` with `FlagCreate` for every store type in use. The manager creates a missing type as a flat structure of the given elements (`dpTypeCreate`) and answers after the Data manager confirmed it; the broker then checks the layout until the type is known (bound 10 s). An existing type is never changed; a different layout stops the broker.
- Retained messages (`RetainedStoreType: WINCCOA`): one `MMQRetained_<enc>` datapoint per retained topic, `<enc>` as above from the topic. Elements: `value` (blob, payload), `topic` (string, the MQTT topic; checked against the name on load), `user` (string, MQTT user name of the publisher, empty for anonymous and broker-internal publishes), `qos` (uint), `expiry` (uint, message expiry interval in seconds, 0 = none), `updated` (time). All elements are written in one confirmed `dpSet`. Topics at or below `TopicRoot` (the broker's status topics) get no datapoint and are kept in memory only. A retained publish with an empty payload, expiry and purges delete the datapoint (`dpDelete`). All retained messages are loaded at startup and served from memory; payloads are read in batches of at most 32 that are split further when an answer would exceed the 1 MiB ABI limit, which also bounds one retained payload. Changes made to these datapoints in WinCC OA while the broker runs are not picked up.
- Unknown envelope version or corrupt JSON returns an error; the record is never overwritten implicitly.

## 6. Configuration

Bootstrap `config.yaml` key `WinCCOaNative` (read before any OA access):

```yaml
WinCCOaNative:
  Enabled: true            # only effective inside the embedding manager
  Namespace: true          # expose winccoa/systems/<system> namespace and writes
  TopicRoot: winccoa       # first level(s), may contain / (e.g. plant/oa)
  TagsName: tags           # one level
  TypesName: types         # one level, different from TagsName
  SystemsName: systems     # level before every system name
  LocalShortcut: true      # local system also as winccoa/tags|types/...
  EchoPolicy: BROKER_TAG   # BROKER_TAG | NO_SOURCE
ConfigStoreType: WINCCOA   # device + archive configs in MMQConfigs datapoints
SessionStoreType: WINCCOA  # sessions + subscriptions in MMQSessions datapoints
RetainedStoreType: WINCCOA # retained messages in MMQRetained datapoints
UserStoreType: WINCCOA     # MQTT users + ACL rules in MMQUsers datapoints
```

`DefaultStoreType: WINCCOA` makes configs, sessions, retained messages and users WinCC OA datapoints; the queue and the metrics fall back to `MEMORY` (only `MEMORY`, for metrics also `NONE`, may be configured), no database file is opened, and no SQLite handle is offered to archive groups.

`WINCCOA` is a value of the normal top-level store keys and is only accepted inside the embedding manager; a standalone broker refuses to start with it.

`WinCCOA-Client` devices always connect through the WinCC OA GraphQL server, as in the standalone broker (the native query transport was removed on 2026-09-30). In the embedding manager a device whose output topics fall below `TopicRoot` is not started.

Echo policy: `BROKER_TAG` (default) uses `dpConnect`; writes from MQTT come back as hotlinks and are published normally, and commands are only accepted from non-inline clients so OA-origin publications can never become commands. `NO_SOURCE` uses `dpConnectNoSource` and publishes the confirmed value of an accepted write from the command path.

## 7. Numeric limits

| Limit | Value |
|---|---|
| Go -> host request queue (host side) | 4096 requests |
| Pending requests awaiting completion | 4096; above -> `MMQ_E_OVERLOAD` without submitting |
| Host -> Go event queue | 16384 events split over 4 workers (a reference always maps to the same worker, keeping its order); full -> event dropped and counted |
| Per-dispatch-tick drain budget | 256 requests or 5 ms, whichever first |
| Dispatch wait | 2 ms (latency floor for Go -> OA requests) |
| Default request timeout | 5 s (resolve, write), 10 s (store operations, connect batches) |
| Shutdown bound | 10 s total; host tears down SDK resources after `STOPPED` or the bound |
| Max ABI message | 1 MiB |
| Max store record envelope | 256 KiB |
| Max command payload | 64 KiB |
| Connect batch size | 100 names per `DP_CONNECT` request; a batch never mixes systems, so an outage of one system only drops that system's registrations. The host registers each name with its own single-element `dpConnect`: a list `dpConnect` delivers every element of the list whenever one changes (verified on 3.21), which republished unchanged values |
| Write batching | all `DP_SET` requests drained in one dispatch tick go out as one `dpSet` message; the answer has one group per item, so each request is confirmed individually |
| Max native interests (distinct DPEs) | 10000 |
| Load target (AC-34) | 50 MQTT clients, 5000 DPEs, 2000 value changes/s, payload <= 1 KiB |
| Latency budget (AC-34) | OA change -> MQTT subscriber p99 <= 50 ms at the load target; write request -> OA confirmation p99 <= 100 ms; MQTT write -> OA -> subscriber round trip (what `cmd/mmqload` measures live) p99 <= 150 ms, no loss |
| Memory budget (AC-35) | RSS plateau <= 256 MiB at the load target |

## 8. Command result semantics

- Writable element types and JSON forms: bool (`true`/`false`), int and char/long (integer within int32), uint/ulong/bit64 and bit32 (integer within uint32), float (finite number), string (JSON string), time (RFC 3339 string or epoch milliseconds), blob (base64 string). Language texts and dyn types are read-only. No cross-family coercion (a string is never parsed as a number).
- Native value publications use QoS 1 and are not retained; the current value on subscribe is delivered to the subscribing client only (retain flag set, retain handling honored), so no native value is ever stored in the retained store.

- Payload `{"value": <json>, "id": "<optional command id>", "replyTo": "<optional topic>"}` or a bare JSON scalar. Max 64 KiB.
- Retained commands are rejected (MQTT 5 QoS>0: PUBACK `0x99` payload format invalid is not used; `0x83` with reason "retained command"). MQTT 3.1.1 QoS>0 rejections disconnect, per the engine's existing rule; QoS 0 rejections are silent and only visible through the result topic.
- Synchronous validation (ACL, parse, DPE existence, type conversion) happens before the PUBACK. Failures: MQTT 5 PUBACK `0x87` (ACL), `0x90` topic name invalid (parse/missing DPE), `0x99` payload format invalid (type conversion), `0x83` (OA unavailable).
- OA confirmation is asynchronous. The result goes to the MQTT 5 Response Topic (with Correlation Data) or `replyTo`: `{"id":..,"topic":..,"status":"confirmed|failed|timeout","error":..}`. `confirmed` means the OA event manager accepted the value; it does not claim physical process effect.
- Duplicate suppression: commands with an `id` seen in the last 10 minutes (per client, max 10000 ids) are not re-executed; the earlier result is re-sent. Commands without `id` have no retry safety.
