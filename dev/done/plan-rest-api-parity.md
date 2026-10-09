# Plan: main-broker REST API on MonsterMQ Edge

## Implementation status (2026-09-17)

The `/api/v1` routes, configuration switch, shared edge auth, current/retained/history
reads, publish variants, bulk and Influx writes, SSE, OpenAPI, offline docs, and
real-listener integration tests are implemented. `go test ./...`, REST race tests,
`go vet ./...`, and CGO-disabled native/Linux AMD64/ARM64/ARMv7 builds pass.
The live JVM fixture confirmed login/error responses and JSON, text, numeric,
and binary *current/retained* value formats. Edge history encoding follows the
main `PayloadDecoder.kt` source and is covered by SQLite integration tests.

This plan remains active because live JVM history fixtures for all supported
backends have not yet been completed. Stalled-reader SSE overload and
persistent-SQLite restart integration tests pass. The isolated JVM broker's archive
group returned no history rows despite receiving MQTT publications, so that
fixture could not be captured in this run.

The topic GET also supports `?raw` for an exact current or retained topic.
It returns the original payload bytes, using an image media type for recognized
JPEG/PNG/GIF/WebP payloads and `application/octet-stream` otherwise. MQTT
wildcard topic filters return multiple values in the normal JSON `messages`
array; wildcard and history reads reject `?raw` with HTTP 400.

## Goal and contract

Expose the main broker's `/api/v1` HTTP contract on the edge broker's existing
GraphQL listener (default port 4000). A script that publishes, reads the latest
value of a topic, reads history, or subscribes to updates should be able to
change only the host and credentials when moving between brokers. In particular,
`GET /api/v1/topics/sensor/temperature` must read the Default archive
group's last-value store without a request body or GraphQL query string.

Use these main-broker sources as the contract, checking running responses where
the implementation and documentation differ:

- `../main/broker/src/main/kotlin/extensions/RestApiServer.kt`: route selection,
  authentication, publishing, reads, SSE, and response codes.
- `../main/broker/src/main/resources/openapi.yaml`: public API description.
- `../main/doc/rest-api.md`: user-facing examples.
- `../main/broker/src/main/kotlin/stores/PayloadDecoder.kt`: archived payload
  representation. Historical rows are not serialized like live `BrokerMessage`.

This is the core `/api/v1` API. Prometheus, I3X, Redfish, MCP, and an end-user
dashboard are separate services and are outside this plan. No new database
tables, GraphQL fields, device types, CGO dependencies, or dedicated HTTP port.

## Endpoint catalog

| Method and route | Function and main-broker behavior |
| --- | --- |
| `POST /api/v1/login` | JSON username/password login; return `success`, `token`, and `username`. No prior authorization required. |
| `POST /api/v1/topics/{topic}` | Publish raw body bytes; `qos` and `retain` query parameters. |
| `PUT /api/v1/topics/{topic}?payload=...` | Publish the URL parameter as UTF-8 bytes, without a request body; same MQTT options. |
| `POST /api/v1/write` | Bulk publish from JSON `messages` objects or compact `records` arrays; report accepted count and indexed errors. |
| `POST /api/v1/write/influx` | Ingest InfluxDB Line Protocol. `format=simple` publishes one topic per field; `format=json` publishes one JSON object per measurement. Support `base`, `qos`, and `retain`. |
| `GET /api/v1/topics/{topic}?retained` | Read matching retained messages. Presence of `retained` selects this mode before archive parameters. |
| `GET /api/v1/topics/{topic}[?group={name}]` | Read matching current values. Omitted or empty `group` selects `Default`. |
| `GET /api/v1/topics/{topic}?start=...&end=...[&group={name}]` | Read history when either time bound is present; omitted or empty `group` selects `Default`; optional `limit`. |
| `GET /api/v1/subscribe?topic=...` | Server-Sent Events (SSE) for one or more repeated MQTT topic filters; keepalive comments and disconnect cleanup. |
| `GET /api/v1/openapi.yaml` | Serve the edge API's OpenAPI document. |
| `GET /api/v1/docs` | Serve a self-contained API documentation viewer using local assets. No CDN or general dashboard. |

`GET /health`, GraphQL, and Redfish retain their existing routes. A bare
`GET /api/v1/topics/{topic}` reads current values from `Default`; this is an
intentional edge behavior change from the main broker. Read success uses `{"messages": [...]}`;
no match uses an empty array. Topic paths preserve `/`, and clients URL-encode
`+` and `#` wildcard characters. Reject malformed encodings and invalid MQTT
filters rather than broadening a query accidentally.

## Edge design

1. **Configuration and routing.** Add `RestApi.Enabled` to
   `internal/config/config.go`, `yaml-json-schema.json`,
   `config.yaml.example`, and `scripts/deb/config.yaml`. Match the main broker's
   enabled-by-default behavior when GraphQL is enabled, while allowing an
   explicit opt-out. Register `/api/v1` on the existing `chi` router in
   `internal/graphql/server.go`, before any future catch-all HMI route. Keep
   the HTTP listener lifecycle tied to GraphQL. Put handlers in a small
   `internal/restapi` package rather than duplicating resolver methods.
2. **Share underlying services.** Pass the existing MQTT publish function,
   `stores.Storage.Retained`, `archive.Manager`, `pubsub.Bus`, auth cache, and
   config into the REST handler from `internal/broker/server.go`. Reuse
   `server.Publish` so REST writes reach MQTT subscribers, retained storage,
   archive fanout, bridges, and GraphQL subscriptions through the normal path.
   Do not scan a database in the MQTT publish hook.
3. **Authentication and ACL.** Reuse the GraphQL Basic/Bearer credential
   parser and `auth.Cache` instead of adding a second user store or token
   system. `/login` returns the same edge session token as GraphQL login;
   tokens work on both edge endpoints and expire/revoke according to the
   existing edge session rules. This preserves the main broker's JSON field
   names and Bearer usage, but edge tokens are opaque and broker-local, not
   main-broker JWTs. Document this one intentional difference in OpenAPI and
   the user guide. When user management is disabled, use anonymous access; when
   it is enabled, honor `AnonymousEnabled`, return 401 for missing/invalid
   credentials, and apply publish/subscribe ACLs to every affected topic.
   For wildcard reads and SSE, check each concrete delivered topic as well as
   the requested filter, so a broad filter cannot leak a denied topic.
4. **Reads and payloads.** Use `archive.Manager.Get(name)` and the group's
   `LastValue()` / `Archive()` stores; use the retained store for `?retained`.
   Call `FindMatchingMessages` for retained/current reads and `GetHistory`
   for history. Keep the main broker's 404 cases for missing groups or stores,
   HTTP 400 for malformed timestamps, and `limit` default 1000 / clamp 1..100000.
   Format retained/current entries as `topic`, decoded `value`, ISO timestamp,
   `qos`, and `retain`. JSON values should be JSON and text should be strings;
   determine the main broker's actual behavior for binary live values with
   golden fixtures before choosing an edge representation. Map historical rows
   to the main broker's actual history keys (`topic`, epoch-millisecond
   `timestamp`, `qos`, `client_id`, and
   `payload` or `payload_base64`); do not JSON-marshal Go `[]byte` directly.
   Capture main-broker fixtures for JSON, text, binary, and each supported
   archive backend before freezing the formatter.
5. **Publishing.** Validate topic names (no publish wildcards), parse URL
   parameters and bulk records without losing binary raw-body bytes, and keep
   QoS/retain defaults and response fields compatible with the main broker.
   Route bulk and Influx writes through the same publish/ACL helper, preserve
   partial successes, and avoid an unbounded request allocation. Reflect any
   edge-specific size cap in OpenAPI and test HTTP 413 at the boundary.
6. **SSE.** Subscribe via `pubsub.Bus`, write `text/event-stream` events in the
   main broker's `data: {topic,value,timestamp}` shape, send `: connected`
   and 30-second `: keepalive` comments, and unsubscribe on cancellation.
   Use a bounded per-client buffer; close a slow client instead of blocking MQTT
   publication or silently presenting a gap as a complete stream. SSE has no
   durable replay, matching the main broker.
7. **Docs and portability.** Copy the public route contract into an edge
   OpenAPI file, adjust the bearer description for edge sessions, and serve
   `/docs` with bundled local assets so the single binary works offline.
   Update edge README/API examples. Keep the implementation pure Go and build
   Linux AMD64, ARM64, and ARMv7 without CGO.

## Acceptance criteria catalog

All behavior tests below run through real HTTP and MQTT listeners in
`test/integration/`; no mocked broker. Use the same request corpus against the
main broker where practical to verify status codes and response field names.

| ID | Acceptance criterion | Black-box evidence |
| --- | --- | --- |
| AC-01 | With GraphQL and REST enabled, every catalog route is reachable on the GraphQL port; `RestApi.Enabled: false` removes only `/api/v1`; disabling GraphQL leaves no `/api/v1` listener. | Start three configurations and probe routes, `/graphql`, and `/health`. |
| AC-02 | `POST /login` returns the main JSON keys; valid credentials yield a token accepted by both edge REST and GraphQL. Disabled user management returns anonymous success with no token. | Login and follow-up read through both protocols. |
| AC-03 | No-auth, Basic, and Bearer behavior follows the edge `UserManagement` settings; missing/invalid credentials yield 401, denied topic access yields 403, and error bodies contain `error`. | HTTP requests under enabled/disabled/anonymous configurations. |
| AC-04 | Wildcard reads/SSE never return a concrete topic denied by ACL, including when subscription-time wildcard checks are relaxed. Every bulk/Influx item is ACL-checked separately. | Publish allowed and denied topics under one wildcard and inspect responses/events. |
| AC-05 | `POST /topics/{topic}` publishes the exact raw bytes (including non-UTF-8), QoS, and retain flag through normal MQTT delivery. Invalid publish topics are rejected. | MQTT subscriber compares bytes; retained read confirms the stored value and flag; invalid-topic requests. |
| AC-06 | `PUT /topics/{topic}?payload=...` publishes URL-decoded UTF-8 bytes with no body; missing `payload` yields 400. | HTTP PUT followed by MQTT receive. |
| AC-07 | `POST /write` accepts object `messages` and compact `records` with optional QoS/retain; response `success`, `count`, and indexed `errors` match main behavior for partial success. | Mixed valid/invalid items; verify exactly which MQTT messages arrive. |
| AC-08 | `POST /write/influx` maps measurement/tags/fields to topics for `format=simple`; `format=json` emits one JSON value and preserves the main timestamp convention. Full success is 204; partial success reports count and errors. | Post line-protocol fixtures, inspect MQTT topics/payloads and status. |
| AC-09 | `GET /topics/{topic}?retained` returns `{"messages": [...]}` for exact/wildcard matches; retained mode wins if `group` is also present. Missing retained store yields 404. | Publish retained values, URL-encoded `+`/`#` read, empty/missing-store read. |
| AC-10 | Bare `GET /topics/{topic}`, `?group=`, and `?group=Default` return current archived values from `Default` with the main live-message keys and an empty array for no match. Missing named group or last-value store yields 404. | Publish to group, wait for flush, read exact/wildcard paths. |
| AC-11 | Adding `start` or `end` selects history from the named group or `Default` when `group` is omitted/empty, parses ISO 8601 bounds, returns newest-first matching rows, and enforces default/bounded `limit`; malformed bounds yield 400 and an unavailable archive yields 404. | Time-separated publishes and boundary/error queries. |
| AC-12 | JSON, text, and binary read payloads follow the verified main-broker representation; retained/current timestamps are ISO strings, history timestamps are epoch milliseconds. | Compare golden responses from main and edge on matching fixtures. |
| AC-13 | A bare topic GET uses `Default`; a valid filter with no matches yields 200 and `messages: []`; malformed escapes and malformed filters do not broaden access. | HTTP status/body assertions. |
| AC-14 | `GET /subscribe` accepts repeated topic filters, emits the main SSE JSON shape and keepalive, and stops delivering after disconnect. Missing topic yields 400; unauthorized filters yield 403. | Hold a real SSE connection while MQTT publishes on two filters; close and inspect cleanup. |
| AC-15 | Slow SSE clients cannot block MQTT publication or grow memory without bound; overload terminates that stream. | Backpressure integration test with a stalled reader and continuing MQTT publisher. |
| AC-16 | `/openapi.yaml` covers every exposed operation, documented parameter, auth scheme, and response shape; `/docs` loads without an external network request. Both follow API authentication rules except `/login`. | Fetch spec and docs from isolated broker; compare route catalog. |
| AC-17 | Request size, history limit, and SSE client bounds are explicit and do not cause process instability on edge hardware. Expected over-limit responses are documented. | Boundary tests and a short concurrent read/write/SSE load test. |
| AC-18 | REST and GraphQL reads observe the same last-value, retained, and archive state after MQTT and REST writes; no new storage schema or divergent publish path is introduced. | Cross-protocol integration test, including broker restart with persistent SQLite. |
| AC-19 | `go test ./...`, relevant race tests, `go vet ./...`, CGO-disabled native build, and Linux AMD64/ARM64/ARMv7 builds pass. | CI/build output. |

## Delivery sequence

1. Record main-broker golden responses and resolve history payload differences
   across stores; add initial black-box fixtures and OpenAPI contract checks.
2. Add config, route registration, shared auth/publish helpers, and login.
3. Implement single/bulk/Influx writes and retained/current/history reads.
4. Implement SSE with lifecycle and backpressure handling.
5. Publish OpenAPI/docs, then run the catalog and cross-compile checks.

Move this plan to `dev/done/` only after every applicable acceptance criterion
passes and any intentionally different behavior is documented.
