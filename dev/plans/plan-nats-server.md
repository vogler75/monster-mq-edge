# NATS Server Implementation Plan

Status: proposed, awaiting owner sign-off (2026-10-03).

Edge gets the main broker's NATS server as a 1:1 port: same config key, same
wire behaviour, same GraphQL schema. The source of truth is the main broker's
code (`../main/broker`), not the upstream NATS specification; every deviation
the edge needs is listed below.

Companion plan: [plan-kafka-server.md](plan-kafka-server.md). NATS goes first.

## Context & current state

The main broker ships a NATS protocol server; the edge broker has none. It is
separate from the NATS client bridge, which this plan does not port.

### Main broker (JVM)

| | NATS server |
| --- | --- |
| Source | `NatsServer.kt` (41 lines), `NatsClient.kt` (401 lines) |
| Switched on by | top-level `NATS: <port>`, 0 = off; no Features flag |
| Configured via | YAML only |
| Storage | none |

The Features name `Nats` belongs to the client bridge, not to this server.

### monster-mq-edge (Go) today

- Single static binary, zero CGO (only exception: the opt-in WinCC OA
  embedding library); MQTT via the vendored mochi-mqtt fork in `internal/mqtt/`.
- SQLite, PostgreSQL and MongoDB with the main broker's schemas; device configs
  with one manager per device type (`bridge/mqttclient`, `bridge/winccua`);
  PeerLink broker-to-broker forwarding.
- GraphQL already carries the main broker's `brokerConfig.natsPort`, hard-wired
  to 0 (`resolvers/resolver.go:734`).
- AGENTS.md lists NATS as off-limits without owner sign-off. This plan is that
  sign-off request; AGENTS.md gets updated with the ported server.

### Main broker building blocks and their edge counterparts

| Main broker | Edge |
| --- | --- |
| `SessionHandler.subscribeInternalClient` / `unsubscribeInternalClient` | `mqtt.Server.Subscribe` / `Unsubscribe` (inline subscription) |
| `SessionHandler.publishMessage` with client ID `nats-<uuid>` | `mqtt.Server.InjectPacket` with a per-connection inline client of the same ID, so storage hooks and archives see the same publisher |
| `SessionHandler.setClient` / `onlineClient` / `delClient` | `stores.SessionStore.SetClient` / `DelClient` |
| `UserManager.authenticate`, `canPublish`, `canSubscribe`, enabled `Anonymous` user | `auth.Cache` (bcrypt, ACL) and `UserManagement.AnonymousEnabled` |

## Parity rules for the port

The server is an in-process adapter over the local MQTT core, as in the main
broker, and behaves exactly like the main broker's code.

1. **Behaviour comes from the main broker's code.** Where it differs from
   upstream NATS (`a.>` matches `a`, no NATS headers), edge differs the same way.
2. **Same config key.** Top-level `NATS: <port>` as a plain integer, schema
   entry copied from the main broker's `yaml-json-schema.json`. No new `Nats:`
   YAML block; in the main broker that name configures the client bridge.
3. **No GraphQL change.** `brokerConfig.natsPort` exists and only has to report
   the port. `enabledFeatures` does not change.
4. **Zero CGO, no protocol library in the broker.** The main broker hand-rolls
   the codec (`NatsClient.kt`); edge does the same. `nats.go` appears only in
   `test/integration`.
5. **Same session identity.** NATS clients use client IDs `nats-<uuid>`, so the
   shared dashboard shows its NATS badge and hides MQTT-only fields.

The main broker has no shared topic-mapping module: NATS conversion lives in
`NatsClient.kt`. Edge keeps it that way; the earlier `internal/protocolmap/`
idea is dropped.

## Issue — NATS server, port of NatsServer.kt and NatsClient.kt

**Title:** Port the main broker's NATS server to the edge broker

**Summary:** A plain-TCP listener for the NATS text protocol in
`internal/nats/`, switched on by top-level `NATS: <port>`. Each connection is a
clean, non-persistent session `nats-<uuid>` that subscribes and publishes on
the local MQTT core at QoS 0, following `NatsClient.kt` line by line.

**Motivation:** The same NATS clients and the same dashboard work against edge
and the main broker with identical results.

### Scope

One phase, because the main broker's server is small:

- Listener: TCP only, `TCP_NODELAY`, binds `0.0.0.0:<NATS>`; a bind failure
  aborts startup.
- Commands `CONNECT`, `PUB`, `SUB`, `UNSUB`, `PING`, `PONG`, case-insensitive.
- Auth: user and password from `CONNECT`, anonymous access, per-topic ACL on
  `PUB` and `SUB`.
- Delivery between NATS subjects and MQTT topics at QoS 0; no retained messages
  on `SUB`.

Not ported, because the main broker's NATS server has none of it: JetStream-style
persistence backed by the local storage layer, HPUB/HMSG headers, TLS,
queue-group load balancing, token/nkey/JWT auth, server-side PING.

### Protocol behaviour to copy

| Area | Main broker behaviour (`NatsClient.kt`) |
| --- | --- |
| INFO | Sent on accept, before reading: `INFO {"server_id":"monstermq","server_name":"MonsterMQ","version":"0.0.1","proto":1,"max_payload":1048576,"auth_required":<user management on>}`, keys in this order, nothing else |
| Framing | Lines end with exactly CRLF; empty lines ignored; tokens split on single spaces; after a valid `PUB` read exactly `#bytes + 2` bytes; a `PUB` line and its payload in one TCP read must work |
| Errors | `-ERR '<text>'` with single quotes; unknown verb gives `-ERR 'Unknown Protocol Operation'` and the connection stays open |
| CONNECT | JSON required, empty gives `-ERR 'Invalid CONNECT JSON'`; only `verbose`, `user`, `pass` are read; `+OK` only after a successful `CONNECT` with `verbose` |
| Auth | User management off: everything allowed, no ACL. On: empty `user` is accepted as anonymous if allowed, else bcrypt check; failure sends `-ERR 'Authorization Violation'` and closes. `PUB`/`SUB`/`UNSUB` before auth get the same error without closing |
| ACL | Checked on the converted MQTT topic, only when a user is set: `-ERR 'Permissions Violation for Publish to "<subject>"'` or `... for Subscription to "<subject>"` |
| SUB | `SUB <subject> [queue] <sid>`, SID is the last token; queue group ignored; one inline MQTT subscription per distinct filter at QoS 0, no retained delivery; `AllowRootWildcardSubscription` not applied |
| UNSUB | `UNSUB <sid> [max]`; `max_msgs` ignored; unknown SID is silent; the inline subscription goes when its last SID goes |
| PUB | `PUB <subject> [reply] <#bytes>`; reply-to parsed and dropped; published at QoS 0, retain false, publisher `nats-<uuid>`; no `$SYS` block, no wildcard rejection, no rate limit |
| MSG | `MSG <subject> <sid> <#bytes>` CRLF payload CRLF, one per matching SID; subject from the concrete topic; never a reply-to; a client receives its own publishes |
| PING/PONG | `PING` answered with `PONG`, even before `CONNECT`; `PONG` ignored; the server never pings; no idle or auth timeout; `max_payload` advertised, not enforced |
| Subject and topic | NATS to MQTT: `.` to `/`, `*` to `+`, `>` to `#`. MQTT to NATS: `/` to `.`, `+` to `*`, `#` to `>`, space to `_`. Character-wise, global, no escaping; fan-out uses MQTT matching, so `a/#` matches `a` |
| Session | After `CONNECT`: session `nats-<uuid>`, clean, connected, information `{"RemoteAddress","LocalAddress","ProtocolVersion":"NATS","SSL":false}`; deleted, not marked offline, on close, socket error or GraphQL session removal |

Edge-only adjustments, because the edge has different building blocks:

- Anonymous access follows `UserManagement.AnonymousEnabled`; the main broker
  checks for an enabled `Anonymous` user.
- ACL goes through the same check as the MQTT auth hook, so the WinCC OA alias
  rule (tags form must also be allowed) applies to NATS too.

### Config

```yaml
NATS: 4222   # 0 = disabled (default)
```

The `yaml-json-schema.json` entry is copied from the main broker (title
"NATS Port", 0 to 65535, default 0). There are no host, TLS or auth options,
and no Features flag; `enabledFeatures` does not change.
`brokerConfig.natsPort` reports the configured port.

### Known defects in the main broker

These are bugs, not features. Recommendation: fix them in both repos at once
instead of copying them.

- A negative `PUB` byte count throws inside the parser.
- A rejected `PUB` does not skip its payload, so the payload line is parsed as a
  command and draws a second `-ERR`.
- `SUB` with a reused SID leaves that SID under the old filter, where it keeps
  receiving.
- Auth runs asynchronously while parsing continues, so a `SUB` sent right after
  `CONNECT` can be rejected.
- NATS sessions never count messages in or out.

### Implementation plan / tasks

1. `internal/nats/server.go`: listener on the `NATS` port; a bind failure aborts
   startup.
2. `internal/nats/conn.go`: per-connection parser (line and fixed-size mode),
   INFO, CONNECT, PING/PONG, error strings as in the table.
3. Auth and ACL through `auth.Cache` and the auth hook's ACL check.
4. SID registry (SID to filter, filter to SIDs) on
   `mqtt.Server.Subscribe`/`Unsubscribe`; MSG fan-out with the MQTT matcher.
5. `PUB` through a per-connection inline client `nats-<uuid>` and
   `InjectPacket` at QoS 0.
6. Session lifecycle with `SessionStore.SetClient`/`DelClient`; GraphQL session
   removal closes the socket.
7. Config: `NATS` in `config.go`, `yaml-json-schema.json`,
   `config.yaml.example`, `scripts/deb/config.yaml`; fill
   `brokerConfig.natsPort`.
8. Integration tests in `test/integration` with `nats.go`: exact INFO bytes,
   pub/sub both ways with an MQTT client, wildcards, auth off/on/anonymous, ACL
   errors, session appears and disappears. The main broker has no NATS tests,
   so these become the reference for both.
9. Docs: README feature line and a page mirroring the main broker's
   `doc/nats.md`; AGENTS.md allows the NATS server.

### Acceptance criteria

- For the same inputs, a NATS client sees the same INFO line, `-ERR` strings,
  subjects and payloads from edge as from the main broker.
- NATS and MQTT clients exchange messages both ways, including wildcard
  subscriptions.
- `NATS: 0` binds nothing; `brokerConfig.natsPort` shows the configured port.
- A connected NATS client appears in `sessions` with the `nats-` prefix and
  disappears on disconnect.
- The standalone binary stays CGO-free.

## Sequencing and decisions

**NATS first, Kafka second.** The NATS server is about 440 lines in the main
broker, needs no GraphQL change and no storage, and proves the inline-client and
session pattern that the Kafka server reuses. It stays off by default: `NATS: 0`.

### Decisions needed

- [ ] Sign-off: allow the NATS server on edge; AGENTS.md lists it as off-limits
      today.
- [ ] For the defects listed above: fix them in both repos (recommended) or copy
      them into edge.

Settled by copying the main broker: no JetStream; hand-rolled codec, no protocol
library; NATS character replacement for subjects.

---

*Source: the main broker at `../main/broker` as read on 2026-10-03:
`NatsServer.kt`, `NatsClient.kt`, `doc/nats.md`.*
