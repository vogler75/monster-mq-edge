# MQTT code-review findings

Review date: 2026-08-12

Scope: `internal/mqtt/` and the direct broker integration needed to validate
the retained-message behaviour. The package is an in-repository copy of
`mochi-mqtt/server/v2` 2.7.9 with edge-specific modifications.

## Findings

### P1: Sending a deferred QoS message removes its acknowledgement state

`processPacket` sends the next message deferred by a client's MQTT v5 Receive
Maximum, then deletes that message from `Inflight` immediately.

```go
next, ok := cl.State.Inflight.NextImmediate()
if ok {
	_ = cl.WritePacket(next)
	if ok := cl.State.Inflight.Delete(next.PacketID); ok {
		atomic.AddInt64(&s.Info.Inflight, -1)
	}
	cl.State.Inflight.DecreaseSendQuota()
}
```

The deferred packet is an outbound QoS 1 or 2 PUBLISH that was previously
entered into `Inflight` by `publishToClient`. Removing it before its PUBACK or
PUBCOMP arrives causes the acknowledgement to be ignored. The send quota then
remains exhausted; later QoS publications are deferred permanently. A
disconnect after the send also cannot cause a retransmission.

Location: `internal/mqtt/server.go:767`

Recommendation: keep the packet in `Inflight` after it is first written and
remove it only in its corresponding acknowledgement handler. Replace the
negative-expiry "deferred" marker with its normal expiry before sending.

### P1: QoS 2 acknowledgement handlers do not validate state transitions

`processPubrec`, `processPubrel`, and `processPubcomp` identify a flow only by
packet ID. They do not require the existing in-flight packet to be in the
expected phase (`Publish`, `Pubrec`, or `Pubrel` respectively); `processPubcomp`
does not require an existing entry at all.

The same handlers also alter the wrong quota for their direction:

- Receiving a PUBREC for an outbound QoS 2 PUBLISH decreases the server's
  inbound `receiveQuota`.
- Receiving a PUBREL for an inbound QoS 2 PUBLISH increases the client's
  outbound `sendQuota`.
- Receiving any PUBCOMP increases both quotas, including when it does not
  belong to an active flow.

A peer can send an out-of-order acknowledgement for a known packet ID and make
the server drop a QoS 2 message as complete. It can also inflate the available
send quota beyond its advertised Receive Maximum.

Location: `internal/mqtt/server.go:1260`

Recommendation: validate the stored packet type at every phase, only advance
the valid state transition, and adjust only the quota belonging to that flow:
`receiveQuota` when an inbound QoS flow completes and `sendQuota` when an
outbound QoS flow completes.

### P1: Quota access mixes mutex-protected writes with atomic reads

The edge change to `Inflight` writes quota fields as ordinary integers while
holding `quotaMu`. Several server paths read those same fields through
`atomic.LoadInt32` without acquiring that mutex.

Locations:

- Writers: `internal/mqtt/inflight.go:121`
- Readers: `internal/mqtt/server.go:767`, `:911`, `:1154`, and `:1162`

Atomic access does not synchronize with a non-atomic write, so concurrent
publishing and acknowledgement processing races. The race detector did not
reach this timing in the current integration suite.

Recommendation: expose locked getter/reservation methods on `Inflight` and
use them exclusively, or return to fully atomic fields with compare-and-swap
operations. Avoid a separate check followed later by an ignored decrement
result.

### P2: `NextImmediate` recursively acquires an `RWMutex`

`NextImmediate` acquires `Inflight.RLock` and then calls `GetAll`, which tries
to acquire the same `RLock` again. Go explicitly prohibits recursive read
locking when a writer may be pending: the second read lock can wait for the
writer while the writer waits for the first read lock to be released.

Location: `internal/mqtt/inflight.go:95`

Recommendation: remove the outer lock and use `GetAll(true)`, or introduce an
unlocked helper used while holding the outer lock.

### P2: Inline `#` subscriptions miss nested topics

In the wildcard branch of `scanSubscribers`, ordinary and shared subscriptions
are collected from `wild`, but inline subscriptions are collected from
`particle`.

```go
if wild := particle.particles.get("#"); wild != nil && partKey != "+" {
	x.gatherSubscriptions(topic, wild, subs)
	x.gatherSharedSubscriptions(wild, subs)
	x.gatherInlineSubscriptions(particle, subs)
}
```

An inline subscription such as `foo/#` therefore does not receive a publish to
`foo/bar`.

Location: `internal/mqtt/topics.go:622`

Recommendation: call `x.gatherInlineSubscriptions(wild, subs)`.

### P1: MEMORY retained-store mode retains nothing

The MQTT server treats every hook that provides `OnSelectRetainedMessages` as
the retained-message source and bypasses its own in-memory retained index.
`StorageHook` always reports that it provides the hook, but it deliberately
does no work when `retainedInMemory` is true. As a result, a retained PUBLISH
in `RetainedStoreType: MEMORY` is neither persisted nor recorded in the
broker's topic index.

Locations:

- Broker decision: `internal/mqtt/server.go:1040`
- Unconditional hook capability: `internal/broker/hook_storage.go:57`
- MEMORY no-op: `internal/broker/hook_storage.go:179`

Recommendation: only report `OnSelectRetainedMessages` from `StorageHook` for
database-backed retained stores. That lets `Server.retainMessage` use
`Topics.RetainMessage` in MEMORY mode.

## Second-pass findings

### P0: A truncated MQTT v5 SUBSCRIBE packet can panic the broker

`SubscribeDecode` decodes a topic filter and then reads its subscription
options using `buf[offset]` without verifying that the byte exists. A packet
whose Remaining Length ends immediately after the encoded filter makes
`offset == len(buf)` and causes an index-out-of-range panic.

An unhandled panic in any connection goroutine terminates the broker process,
so any connected MQTT v5 client can use this as a remote denial of service.
The MQTT v3 branch immediately below uses the bounds-checked `decodeByte`
helper and does not have this problem.

Location: `internal/mqtt/packets/packets.go:959`

Recommendation: decode the options byte with `decodeByte` and return a
malformed-packet error when it is absent. Add decoder fuzzing and a specific
truncated-SUBSCRIBE regression test.

### P1: Connections waiting for CONNECT have no deadline or client-limit slot

`attachClient` starts a write loop and blocks reading the initial CONNECT
packet before setting a socket deadline. It reserves a `MaximumClients` slot
only after the full packet has been received and parsed.

An attacker can therefore open many connections and send nothing, or send a
partial fixed header or Remaining Length. Each connection retains a socket and
goroutine indefinitely while remaining outside the configured client limit.

Locations: `internal/mqtt/server.go:412`, `:415`, `:424`, and `:448`

Recommendation: apply a short configurable CONNECT-handshake deadline before
the first read and account for pending connections separately or reserve the
connection slot as soon as the listener accepts the socket.

### P1: The configured MaxMessageSize is not applied to the MQTT server

`config.Config.MaxMessageSize` defaults to 1 MiB and is present in the example
configuration and JSON schema, but broker construction does not copy it into
`mqtt.Options.Capabilities.MaximumPacketSize`. The MQTT capability consequently
keeps its zero/unlimited default.

`Client.ReadPacket` allocates the peer-controlled Remaining Length and then
duplicates the full buffer before decoding it. A client can request nearly the
MQTT maximum of 256 MiB, temporarily consuming about twice that amount for one
packet. Multiple connections can turn this into memory exhaustion.

Locations:

- Unused config value: `internal/config/config.go:139`
- Broker construction: `internal/broker/server.go:122`
- Allocation and duplicate copy: `internal/mqtt/clients.go:471` and `:481`

Recommendation: validate `MaxMessageSize`, set
`Capabilities.MaximumPacketSize` during broker construction, and reject an
oversized packet before allocating its body. Retain a safe non-zero deployment
default.

### P1: Topic aliases are resolved after topic validation and ACL checks

For an alias-only MQTT v5 PUBLISH, `processPublish` validates and authorizes an
empty `TopicName`. It resolves the alias only after the ACL check. This can
deny otherwise authorized alias traffic, or allow a permissive/custom ACL hook
to accept a publication without ever checking the actual topic.

`InboundTopicAliases.Set` also stores and returns an empty topic when an alias
has not previously been registered. An unknown alias-only PUBLISH can therefore
continue through publication hooks with an invalid empty topic instead of
being rejected with `Topic Alias invalid`.

Locations:

- Validation and ACL: `internal/mqtt/server.go:907` and `:915`
- Late alias resolution: `internal/mqtt/server.go:953`
- Unknown-alias handling: `internal/mqtt/topics.go:50`

Recommendation: resolve aliases first, distinguish lookup from registration,
reject an unknown alias, then validate and authorize the resolved topic name.

### P1: StorageHook persists subscriptions which the broker rejected

`processSubscribe` calculates one SUBACK reason code per filter and skips
adding invalid or unauthorized filters to the MQTT topic index. It still calls
`OnSubscribed` with the complete original filter list. `StorageHook` ignores
the supplied reason codes and writes every filter to both the database and the
edge subscription index.

The queue hook uses that second index to select offline recipients and writes
queued packets directly on reconnect. A rejected or unauthorized subscription
can therefore be queued and delivered later even though it never existed in
the live MQTT session.

The reverse problem exists for an UNSUBSCRIBE rejected because its packet ID
is already in use: the live subscription remains, but `OnUnsubscribed` removes
it from persistent storage because that hook receives no reason codes.

Locations:

- Reason-code calculation: `internal/mqtt/server.go:1333`
- Hook invocation: `internal/mqtt/server.go:1383` and `:1440`
- Persistence which ignores the result: `internal/broker/hook_storage.go:106`
- Direct queued delivery: `internal/broker/hook_queue.go:208`

Recommendation: persist only filters whose SUBACK reason code is a granted QoS.
Extend unsubscribe hook handling so persistence is changed only for filters
which the broker actually removed.

### P1: Expired sessions leave subscriptions and inflight state in memory

The regular clean-session disconnect path calls `ClearInflights` and
`UnsubscribeClient` before deleting the client. The periodic expiry path only
calls `OnClientExpired` and removes the client from `Clients`.

Consequently, every expired persistent session can leave stale entries in the
MQTT topic trie, inflight maps, and the corresponding server counters. Publish
resolution continues to visit those client IDs even though `Clients.Get`
cannot find them.

Locations:

- Correct immediate-expiry cleanup: `internal/mqtt/server.go:496`
- Incomplete periodic cleanup: `internal/mqtt/server.go:1790`

Recommendation: clear inflights and unsubscribe the client before deleting an
expired session, using the same takeover safeguards as the disconnect path.

### P2: Receive Maximum incorrectly blocks QoS 0 publications

`processPublish` disconnects a client whenever its inbound receive quota is
zero without first checking the new packet's QoS. MQTT v5 Receive Maximum only
limits concurrent QoS 1 and QoS 2 PUBLISH packets; QoS 0 traffic remains
permitted while those flows are outstanding.

Location: `internal/mqtt/server.go:911`

Recommendation: apply the receive-quota check and reservation only when
`pk.FixedHeader.Qos > 0`.

### P2: A valid reason-only MQTT v5 DISCONNECT is decoded as success

`DisconnectDecode` reads the reason code only when Remaining Length is greater
than one. MQTT v5 permits a packet containing a reason byte without a property
length byte. A valid reason-only `0x04` (`Disconnect with Will Message`) is
therefore left as reason zero, causing the normal disconnect path to clear the
Will rather than publish it.

Location: `internal/mqtt/packets/packets.go:568`

Recommendation: decode the reason when Remaining Length is at least one and
decode properties when more bytes remain. Validate that the property-length
field accounts for the entire remainder.

### P2: Database-backed retained messages discard their expiry

`StorageHook.OnRetainMessage` persists the topic, payload, QoS, client, and
timestamp but does not copy `MessageExpiryInterval`. The database row therefore
has no expiry even when the incoming retained PUBLISH did. It can survive
indefinitely and reappear after a restart.

The selection path supports reading a stored expiry interval, but returns its
original duration without setting the packet's absolute `Expiry`. For rows
written by another compatible broker, subscribers receive the original TTL
rather than the remaining TTL.

Locations:

- Persistence: `internal/broker/hook_storage.go:177`
- Selection: `internal/broker/hook_storage.go:202`
- Outbound remaining-TTL adjustment: `internal/mqtt/clients.go:536`

Recommendation: persist the effective expiry interval and timestamp, omit
expired rows, reconstruct an absolute expiry on load, and send the remaining
interval to subscribers.

### P2: Topic-filter validation accepts malformed wildcard filters

`IsValidFilter` checks only that `#` is the final character. It does not require
`#` or `+` to occupy an entire topic level, and it does not reject an empty
shared-subscription group. Filters such as `sport/tennis#`, `foo+bar`, and
`$share//foo` are accepted, acknowledged, and stored even though they are not
valid MQTT filters.

Location: `internal/mqtt/topics.go:723`

Recommendation: validate filters level by level: `#` must be a complete final
level, `+` must be a complete level, and both the shared group and nested filter
must be non-empty.

## Improvement opportunities

- `Client.ReadPacket` allocates the full remaining packet buffer and immediately
  copies it once more before decoding. The first allocation is already unique
  to this packet, so decoding directly from it eliminates one full payload-sized
  copy per inbound packet. Location: `internal/mqtt/clients.go:471`.
- The maximum-packet-size comparison counts the fixed-header byte and payload
  but omits the one to four bytes used to encode Remaining Length. Once the
  limit is wired to configuration, packets can exceed it by up to four bytes.
  Location: `internal/mqtt/clients.go:457`.
- `DecodeLength` does not reject an encoding after its fourth byte. Continuation
  bytes whose low seven bits are zero can extend the read beyond the MQTT
  variable-byte integer limit. Location: `internal/mqtt/packets/codec.go:146`.
- `AuthDecode` requires both a reason code and properties. MQTT v5 also permits
  an empty AUTH packet, with success and no properties as the defaults.
  Location: `internal/mqtt/packets/packets.go:1141`.
- `ClearExpiredInflights` invokes `OnQosDropped`, and the server invokes the same
  hook again for every returned packet ID. Hooks receive two notifications per
  expiry, with the second lacking the original packet fields. Locations:
  `internal/mqtt/clients.go:343` and `internal/mqtt/server.go:1827`.
- TCP, generic net, and Unix listeners can accept a connection immediately
  before shutdown, observe the shutdown flag afterward, and neither hand the
  socket to the broker nor close it. Their connection wait-group increment also
  occurs inside the later handler, allowing a narrow `Add`-versus-`Wait` race.
  Locations: `internal/mqtt/listeners/tcp.go:94`,
  `internal/mqtt/listeners/net.go:61`, and
  `internal/mqtt/listeners/unixsock.go:71`.

## Recommended regression tests

- MQTT v5 client with Receive Maximum 1: publish at least three QoS 1 messages
  to it and verify all acknowledgements, in-flight state, and subsequent
  delivery.
- Valid and invalid QoS 2 state-machine transitions, including mismatched
  packet types and unsolicited PUBCOMP.
- Concurrent publishing plus acknowledgements under `go test -race`.
- Inline `foo/#` subscription receiving `foo/bar`.
- Retained PUBLISH followed by a new subscription with
  `RetainedStoreType: MEMORY`.
- Truncated MQTT v5 SUBSCRIBE packets, plus fuzz tests for every packet decoder,
  must return an error without panicking.
- Pending sockets which never complete CONNECT must time out and must be bounded
  independently from established sessions.
- A PUBLISH larger than `MaxMessageSize` must be rejected before its body is
  allocated.
- Registered, unregistered, authorized, and unauthorized topic-alias PUBLISH
  packets.
- Rejected subscriptions and unsubscriptions must not change the database or
  edge subscription index; verify that the offline queue cannot deliver them.
- Periodic session expiry must remove topic-index entries, inflight packets, and
  update counters.
- QoS 0 publishing while the QoS 1/2 inbound receive quota is exhausted.
- Reason-only MQTT v5 DISCONNECT with reason `0x04` must publish the Will.
- Database-backed retained expiry before and after a broker restart, including
  verification that subscribers receive the remaining TTL.
- Invalid wildcard and shared-subscription filters such as `foo+bar`,
  `foo/bar#`, and `$share//foo`.

## Checks performed

- `go test ./internal/mqtt/...` completed successfully, but the package has no
  local test files.
- `go test -race ./test/integration` completed successfully. It does not cover
  the Receive Maximum, inline wildcard, or MEMORY retained-store paths above.
- `go test ./...` completed successfully after the second pass. All packages
  below `internal/mqtt/` currently report `[no test files]`, so the malformed
  packet, alias, quota, expiry, and listener-lifecycle paths remain uncovered.
