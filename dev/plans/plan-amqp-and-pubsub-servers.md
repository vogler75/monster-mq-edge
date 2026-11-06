# Plan: AMQP and other pub/sub protocol servers

**Status:** proposed (2026-10-21), awaiting owner sign-off (see "Decisions needed").
**Scope:** which pub/sub protocols edge should speak next, in what order, and how each one maps onto the
existing MQTT core. Reuses the shared building blocks S1-S7 from
[plan-protocol-servers-shared.md](plan-protocol-servers-shared.md); new shared pieces are P1-P4 below.

## Context

Edge already speaks MQTT 3.1.1/5 (native), NATS (`internal/nats/`) and Redis RESP2 Pub/Sub
(`internal/redisserver/`); a Kafka server exists ([plan-kafka-server.md](../done/plan-kafka-server.md)). All
protocol servers are in-process adapters over the MQTT core: they publish through `ProtocolPublish` (S1),
subscribe through `pubsub.Bus` (S3), register sessions in `SessionStore` (S5) and recheck read ACLs at
delivery (S7).

**Parity.** Main (`../main/broker/src/main/kotlin/extensions/`, read 2026-10-21) has none of the protocols in
this plan. Everything here is therefore **[edge-only]** until main ports it. The plan is designed so that:

- **No GraphQL change** is needed. Servers are configured by YAML like NATS and Redis; `enabledFeatures`
  is `[String!]!`, so new flag names are values, not schema changes.
- **No storage change** is needed. Durable queues reuse the existing session, subscription and V2
  queue tables (P2), so a database written by edge still opens in main.

Under AGENTS.md each protocol below still needs explicit owner sign-off before code lands.

## Candidate protocols

| Protocol | Where it is used | Fit with the MQTT core | Size (est.) | Recommendation |
|---|---|---|---|---|
| **AMQP 0-9-1** | RabbitMQ ecosystem, most enterprise/IT backends, Celery, Spring AMQP, MassTransit | Very good: topic exchange = MQTT topic tree, durable queues = persistent sessions | ~2,500 LoC | **Phase 1** |
| **STOMP 1.2** (+ over WebSocket) | Browsers, ActiveMQ/Artemis, RabbitMQ, Spring WebSocket, simple scripts | Very good, text protocol, same queue model as AMQP | ~800 LoC | **Phase 2** |
| **MQTT-SN 1.2** (UDP) | Battery/sensor nodes, Zigbee/6LoWPAN/LoRa gateways | Native: it *is* MQTT; each MQTT-SN client becomes an engine session | ~1,500 LoC | **Phase 3** |
| **AMQP 1.0** | Azure Service Bus/IoT Hub/Event Hubs, Artemis, Solace, Qpid, **OPC UA PubSub AMQP transport** | Good, but large type system and link/settlement model | ~4,000 LoC | **Phase 4** |
| **CoAP + Observe** (RFC 7252/7641, `draft-ietf-core-coap-pubsub`) | Constrained devices, LwM2M, Thread/Matter-adjacent | Good: PUT/POST = publish, GET+Observe = subscribe | ~1,500 LoC (no DTLS) | **Phase 5** |
| OPC UA PubSub (UADP/UDP) | Industrial field level | Needs an information model / DataSetMetaData layer, not just transport | large | Defer; separate plan |
| ZeroMQ (ZMTP 3.x PUB/SUB) | Brokerless, scientific/robotics | Prefix-match subscriptions only, no ack/durability | ~1,000 LoC | Not now |
| Apache Pulsar binary | Pulsar clusters | Protobuf, cursors, partitions; client bridge more useful than server | large | Not now |
| DDS / RTPS (ROS 2) | Robotics, defense | Discovery + QoS model is heavy; better as a bridge | very large | Not now |
| XMPP PubSub (XEP-0060) | Legacy | Low demand | medium | No |
| Cloud APIs (Google Pub/Sub, SNS/SQS emulation) | Cloud dev tooling | HTTP APIs; possible later via the REST server | medium | No |

Sparkplug B is a payload format on MQTT and stays off-limits per AGENTS.md. Kafka has its own plan.

## Shared building blocks (new)

These land with the first protocol that needs them. Names are binding.

### P1. Protocol server scaffold (`internal/protoserver/`), first used by AMQP

NATS and Redis each duplicate: TCP/TLS listener, accept loop with `TCP_NODELAY`, connection set,
graceful close, stale session cleanup by client ID prefix on start (NATS E9), anonymous-vs-user-management
auth, per-client in/out counters (S5) and the delivery-time ACL check (S7). Extract them:

```go
type Listener struct { /* net.Listener, conns, wg, closed */ }
func Listen(addr string, tlsCfg *tls.Config, logger *slog.Logger) (*Listener, error)
func (l *Listener) Serve(handle func(net.Conn))
func (l *Listener) Close() error

func CleanupStaleSessions(ss stores.SessionStore, prefix, nodeID string)
func Authenticate(cache *auth.Cache, userMgmt bool, user, pass string) (username string, ok bool)
func DeliveryAllowed(cache *auth.Cache, userMgmt bool, user, clientID, topic string) bool // S7
```

New servers use it from day one. Moving NATS and Redis onto it is an optional follow-up, not part of
this plan.

### P2. Durable queues as virtual persistent sessions, first used by AMQP

A durable AMQP queue, a STOMP `/queue/...` destination, an AMQP 1.0 durable link and an MQTT-SN
`clean=false` client all need "messages survive while nobody is connected, and survive a restart".
Edge already has this for MQTT: `QueueHook` (`internal/broker/hook_queue.go`) enqueues into the V2
queue store for **offline** persistent sessions, using `topic.SubscriptionIndex`.

Rule: a durable queue is a persistent session with client ID `<proto>-q-<vhost>-<queue>` (sanitised),
`CleanSession=false`, its bindings stored as that session's subscriptions in `SessionStore`, its
backlog in `QueueStore`. New `QueueHook` API:

```go
// AttachVirtual marks a virtual persistent session online; the caller then drains
// QueueStore and receives live messages via the returned bus subscription.
func (h *QueueHook) AttachVirtual(clientID string) error
// DetachVirtual marks it offline again; OnPublished resumes enqueueing for it.
func (h *QueueHook) DetachVirtual(clientID string)
func (h *QueueHook) RegisterVirtual(clientID string, filters []string, qos byte) error   // declare/bind
func (h *QueueHook) UnregisterVirtual(clientID string, purge bool) error              // delete
```

- **Hand-over without loss or duplicates.** Attach subscribes on the bus first, then drains the queue
  store, then switches the session to online; messages that arrive in both paths are deduplicated by
  `MessageUUID` within the drain window.
- **Ack.** A consumer ack maps to `QueueStore.Ack(clientID, uuid)`; nack/requeue and connection loss
  call `ResetVisibility`.
- **Limits.** `MaxQueueMessages` applies as for MQTT (`EnqueueMultiLimited`).
- **Dashboard.** These sessions appear in the sessions list with their prefix and queued count, like an
  offline MQTT persistent session. Removing one from the dashboard deletes the queue.
- **PeerLink.** Virtual sessions follow `WithPeerLink` ownership like MQTT sessions; no new rules.

Non-durable, exclusive or auto-delete queues do **not** use P2; they are plain bus subscriptions (S3).

### P3. Topic name mapping (`internal/protoserver/topicmap.go`)

Per protocol, a pure function pair, table-tested:

| Protocol | Name -> MQTT topic | Wildcards |
|---|---|---|
| AMQP 0-9-1 topic exchange | `.` -> `/`, `/` -> `.` (swap, as RabbitMQ's MQTT plugin) | `*` -> `+`, `#` -> `#` |
| STOMP | `/topic/<t>` -> `<t>` verbatim; `/queue/<q>` -> P2 queue | MQTT syntax in `selector`-free `SUBSCRIBE` |
| AMQP 1.0 | address `topic://<t>` or bare `<t>` -> `<t>` | MQTT syntax |
| MQTT-SN | topic ID registry per session; short topics; predefined IDs from config | MQTT syntax |
| CoAP | URI path `/ps/<t>` -> `<t>` | none; one observe per topic (`/ps/` + filter allowed as edge extension) |

The AMQP swap is the only lossy-looking one; it is a bijection, so round trips are exact.
`ProtocolPublish` and the ACL always see the MQTT topic.

### P4. Message property mapping

`stores.BrokerMessage` already carries what S1 needs. Common mapping:

| MQTT 5 | AMQP 0-9-1 | STOMP | AMQP 1.0 | CoAP |
|---|---|---|---|---|
| QoS 0 / 1 | `delivery-mode` 1 / 2 (config default) | `receipt` -> QoS 1 | settled / unsettled | NON / CON |
| retain | header `x-mqtt-retain` | header `retain:true` | annotation `x-opt-retain` | `PUT` (retain) vs `POST` |
| ResponseTopic | `reply-to` (mapped by P3) | `reply-to` | `reply-to` | - |
| CorrelationData | `correlation-id` | `correlation-id` | `correlation-id` | token |
| ContentType | `content-type` | `content-type` | `content-type` | Content-Format |
| MessageExpiry | `expiration` (ms) | `expires` | `ttl` | Max-Age |
| user properties | `headers` table (string values) | extra headers | application-properties | - |

This makes request/reply work across protocols (an AMQP RPC client can call a NATS or MQTT responder).

## Per-protocol plans

| Phase | Plan | Depends on |
|---|---|---|
| 1 | [plan-amqp091-server.md](plan-amqp091-server.md) | P1, P2, P3, P4 (lands them) |
| 2 | [plan-stomp-server.md](plan-stomp-server.md) | P1-P4 |
| 3 | [plan-mqttsn-gateway.md](plan-mqttsn-gateway.md) | P1 (UDP) |
| 4 | [plan-amqp10-server.md](plan-amqp10-server.md) | P1-P4, AMQP 0-9-1 listener if the port is shared |
| 5 | [plan-coap-server.md](plan-coap-server.md) | P1 (UDP), P3, P4 |

Each plan has its own config block, tasks, tests, acceptance criteria and decisions.

## Cross-cutting

- **Footprint.** Each server is off by default behind its Features flag; measure binary growth and idle
  RSS on a Pi 4 per phase and record it in the doc page. Budget proposal: < 1 MiB binary growth per
  protocol, < 50 KiB RSS per idle connection.
- **Dashboard.** Sessions badge is chosen by client ID prefix (`dashboard/src/js/sessions.js`); the
  dashboard repo needs badges for `amqp-`, `stomp-`, `mqttsn-`, `amqp10-`, `coap-`. No GraphQL change.
  `sessionToGraphQL` returns null MQTT-only fields for these prefixes, as for NATS/Redis.
- **Metrics.** Per-protocol connection gauge and in/out counters in the Prometheus endpoint.
- **Main broker.** File one issue in vogler75/monster-mq per signed-off protocol so main can port it
  later; until then each doc page states "edge-only".
- **Security.** All TCP servers support TLS via `tlsutil`; none accepts unauthenticated connections when
  user management is on and anonymous is disabled.

## Order of work

```text
P1 scaffold ─┬─ P3/P4 mapping ─┬─ AMQP 0-9-1 (phase 1) ── STOMP (phase 2)
             │                 └───────────────────────── AMQP 1.0 (phase 4)
P2 virtual sessions ───────────┘
MQTT-SN (phase 3) needs only P1 + the engine; can run in parallel with phase 1/2.
CoAP (phase 5) needs P1 + P3 only.
```

## Decisions needed

Protocol-specific decisions are in each plan. Shared ones:

- [ ] Sign-off per protocol (AGENTS.md off-limits rule): AMQP 0-9-1, STOMP, MQTT-SN, AMQP 1.0, CoAP.
- [ ] P2 virtual persistent sessions: OK to show durable queues as sessions in the dashboard and in
      the session tables (no new tables).
- [ ] Whether main should get the same servers (one issue each) or they stay edge-only.
