# MQTT inflight, quota and CONNECT-slot fixes

Created: 2026-10-09

These are the items left open from the 2026-08-12 code review. The full findings
and the fixes already made are in
[plan-mqtt-code-review-findings.md](../done/plan-mqtt-code-review-findings.md).
All locations are in `internal/mqtt/`.

The first three items involve the same state: `Inflight` entries plus the send
and receive quotas guarded by `quotaMu`. Fix them together.

## 1. Deferred QoS send removes ack state (P1)

**Location:** `server.go` `processPacket`, around line 802. This is the
`NextImmediate` resend after an inbound packet.

**Problem:** A packet deferred because the send quota was exhausted is written,
then removed with `Inflight.Delete`, and then `DecreaseSendQuota` is called.
When the client's PUBACK/PUBREC arrives, there is no inflight entry to match it.
The ack is ignored and the send quota is never returned. With Receive Maximum 1,
delivery stalls after the first deferred message.

**Fix:** Keep the packet in `Inflight` until its ack arrives. Replace the
negative-expiry "deferred" marker with the normal expiry, so the packet is
treated as an ordinary inflight message afterwards.

## 2. QoS 2 ack handlers skip state validation (P1)

**Location:** `server.go` `processPubrec` (~1428), `processPubrel` (~1448) and
`processPubcomp` (~1480).

**Problem:**
- None of the handlers check the type of the stored inflight packet before
  acting on it.
- PUBREC (an outbound flow) decreases `receiveQuota`.
- PUBREL (an inbound flow) increases `sendQuota`.
- PUBCOMP increases both quotas, even for an unknown packet ID.

A client can inflate its quotas with unsolicited acks, or corrupt a flow in the
other direction that has the same packet ID.

**Fix:** Validate each transition:
- PUBREC requires a stored outbound PUBLISH QoS 2.
- PUBREL requires a stored inbound PUBREC.
- PUBCOMP requires a stored outbound PUBREL.

Adjust only the quota that belongs to the flow: outbound flows use `sendQuota`,
inbound flows use `receiveQuota`. Unknown IDs get the spec reason code
(Packet Identifier Not Found) and change no quota.

## 3. Quota check-then-decrement is not atomic (P1, partially done)

**Location:**
- `server.go` ~802: `SendQuota() > 0`, then later `DecreaseSendQuota`.
- `server.go` ~993: `ReceiveQuota() == 0` in `processPublish`.
- `server.go` ~1325: `publishToClient`.
- `inflight.go` `Decrease*` and `Increase*` (~134–194).

**Done already:** The getters lock `quotaMu`, so the data race is gone.

**Problem:** The check and the decrement are separate critical sections. Two
concurrent publishers to one client can both see quota 1 and both send.

**Fix:** Add reservation methods such as `TryReserveSendQuota() bool` and
`TryReserveReceiveQuota() bool`. Each one checks and decrements under a single
`quotaMu` lock. Use them in place of the check-then-decrement pairs. The
existing `Decrease*` functions already return `bool`, so this may only need
callers to use that return value instead of a separate getter.

## 4. CONNECT client-limit slot taken after CONNECT (P1, partially done)

**Location:** `server.go` `attachClient` (~435). `ClientsConnected` is
incremented at ~453, which happens after `readConnectionPacket`.

**Done already:** `Options.ConnectTimeout` (default 10s) closes sockets that
never send CONNECT.

**Problem:** Sockets waiting for CONNECT are not counted against
`MaximumClients`. Many slow or silent sockets can still each hold a goroutine
and a buffer for up to `ConnectTimeout`.

**Fix:** Count pending connections separately and give them their own limit,
or reserve the client slot when the socket is accepted and release it if the
handshake fails.

## Regression tests

Add these to `internal/mqtt/review_fixes_test.go`. Run them with `-race`.

- **Item 1:** A client with Receive Maximum 1 is sent three or more QoS 1
  messages. All of them are delivered, each is acked, and the send quota
  returns to 1.
- **Item 2:** Valid QoS 2 transitions in both directions succeed. Invalid ones
  leave both quotas unchanged: PUBREC for a QoS 1 ID, PUBREL without PUBREC,
  and unsolicited PUBCOMP.
- **Item 3:** Concurrent publishers to a single Receive Maximum 1 client while
  the client acks. The inflight count never exceeds 1, and the quota never goes
  negative or above its maximum.
- **Item 4:** With `MaximumClients` reached by pending sockets, further
  sockets are rejected or bounded. Established sessions are unaffected.
