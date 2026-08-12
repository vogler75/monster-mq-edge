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

## Improvement opportunities

`Client.ReadPacket` allocates the full remaining packet buffer and immediately
copies it once more before decoding. The first allocation is already unique to
this packet, so decoding directly from it eliminates one full payload-sized
copy per inbound packet.

Location: `internal/mqtt/clients.go:471`

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

## Checks performed

- `go test ./internal/mqtt/...` completed successfully, but the package has no
  local test files.
- `go test -race ./test/integration` completed successfully. It does not cover
  the Receive Maximum, inline wildcard, or MEMORY retained-store paths above.
