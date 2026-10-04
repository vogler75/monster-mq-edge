# Spec: PeerLink and WinCC OA redundancy (as built, port guide for the JVM broker)

**Status: implemented in MonsterMQ Edge (branch winccoa-native, 2026-10-04). Normative for the JVM port.**

This spec describes what the Go code does, not what the plan proposed. Where the two differ, the code wins; section 18 lists every difference from [plan-peerlink.md](plan-peerlink.md). The plan still holds the design rationale and the rejected alternatives.

Conventions:

- Go paths are relative to `internal/` unless they start with another top-level directory. Line numbers refer to commit `e52f96f`.
- Kotlin paths use `K/` = `/media/psf/Workspace/monster/main/broker/src/main/kotlin/`.
- "Source" is the broker that captures publishes into its log and serves them. "Consumer" is the broker that dials, pulls and injects. In a pair, each node is both, over two TCP connections.
- "MUST" in sections 3 and 13 marks what a wire-compatible implementation has to do.

---

## 1. Purpose and scope

PeerLink is a generic, pull-based, in-memory link between MonsterMQ brokers.

- The source captures every accepted publish outside the excluded topics into a bounded in-memory log with absolute offsets.
- Each configured consumer pulls the log over `mmq-peer/1` (TCP, optionally TLS) and injects the records into its own engine with full MQTT fidelity: QoS, retain and retained deletes, MQTT 5 publish properties, publisher client id and username, publish time, and remaining expiry.
- The source frees a record once every configured consumer has committed it.
- Topology: explicit links, configured on both sides, identified by a canonical NodeId. Records travel exactly one hop (split horizon). More than two nodes need a full mesh.

**Not in scope:**

- Replication of sessions, subscriptions, offline queues or inflight state.
- A durable log or spilling to disk. A source crash loses what was not pulled yet.
- Transitive (multi-hop) forwarding.
- PUBACK gated on peer durability, fencing, or role ownership.

**Owner decisions (2026-10-03/04):**

| Decision | Effect |
|---|---|
| Generic link | PeerLink works without WinCC OA. A standalone broker forwards every topic except `$` topics and `Capture.Exclude`. |
| WinCC OA namespace excluded only with native mode | `<TopicRoot>` is excluded only while the broker is embedded in WinCC OA with `WinCCOaNative.Enabled` and `Namespace`. One predicate serves capture, snapshot, the receiver filter and the announced root, so it can later depend on the host role. |
| All internal publishes forwarded | Publish APIs, bridges, scripts, host monitoring, Redfish, RTSP and native command replies are captured like client publishes. Duplicates are avoided by assigning each device to one node. |
| RPO in memory | PUBACK means "accepted locally". Losses are bounded by the log size and counted on both sides (section 9). |
| PEM only | Certificates and keys are PEM files. Truststores are PEM or a simple PKCS12. No JKS, no encrypted keys. |
| Active-active without special handling | Clients may connect to both brokers. Retained conflicts resolve by arrival order; there is no newest-wins policy. |
| `oaRetained` | With `RetainedStoreType: WINCCOA` on both sides of one WinCC OA system, the receiver updates only its in-memory retained view; WinCC OA replicates `MMQRetained` itself. |
| Q29 write forwarding | Forwarding WinCC OA writes from the passive to the active host is a follow-up with its own plan, not part of v1. |

---

## 2. Architecture overview

```
 Node A (NodeId a)                                         Node B (NodeId b)
 ---------------------------------------------             ---------------------------------------------
 MQTT clients, inline publishers                           MQTT clients, inline publishers
        | publish                                                  ^ deliver
        v                                                          |
 engine A --taps--> Hook: capture filter --append--> Log A    engine B <--InjectPacket / RetainOnly--+
   (OnRetainMessage, OnPublished,          (epoch, offsets,                                          |
    OnWillSent; replicas never captured)    C[b], LSO/LEO)    Injector (one goroutine per source)    |
                                                 ^   |              ^                                 |
                                   FETCH/COMMIT  |   | BATCH        | handoff (1-2 batches)           |
                                                 |   v              |                                 |
                                         Session on A  <==TCP/TLS==> Puller on B: reader + writer ----+
```

- **Source log** (`peerlink/log.go`): per-process epoch, offsets from 1, one committed offset `C[c]` per configured consumer, bounded by count and bytes.
- **Consumer puller** (`peerlink/puller.go`): dial, handshake, optional retained snapshot, then streaming with a reader goroutine, a writer goroutine (FETCH, COMMIT, PING) and an injector goroutine.
- **Injector** (`peerlink/inject.go`): validates, filters and injects each record through a dedicated inline client, then commits.
- **Split horizon**: a packet injected by PeerLink carries `pk.Forward`, and the capture hook skips it. A record is therefore never forwarded a second time.

The reverse direction (B to A) is a second, independent link on a second TCP connection.

---

## 3. Wire protocol `mmq-peer/1` (normative)

The codec is `peerlink/wire/frame.go` (F), `peerlink/wire/record.go` (R) and `peerlink/wire/mac.go` (M). The source side is `peerlink/server.go` (S) and the consumer side is `peerlink/puller.go` (P).

### 3.1 Transport and sniffing

- One TCP connection per link; the consumer dials. Both sides set TCP keepalive to 15 s. The dial timeout is 5 s.
- TLS is optional. ALPN `mmq-peer/1` selects the peer protocol; ALPN `http/1.1` selects the status endpoint (`tlsutil/tlsutil.go:17-19`).
- Minimum TLS version: 1.2, or 1.3 when any shared secret is configured, because the MAC binds to the TLS 1.3 exporter (`tlsutil/tlsutil.go:142-148`).
- Default port: 1890. One port serves TLS, plaintext peers and loopback HTTP.

The source reads the first byte within 3 s (S:21, 382-434):

| First byte | Handling |
|---|---|
| `0x16` | TLS handshake, 10 s timeout (`tlsFailures` on failure). ALPN `http/1.1`: status endpoint (no resync) if the client certificate belongs to a Serve peer, else close (`refusedHttp`). Any other ALPN: the peer protocol. |
| `'M'` (0x4D) | Plaintext peer protocol. Refused (`refusedPlaintext`) on a TLS listener unless `Listener.AllowPlaintext`. |
| `'G'` / `'P'` | Plaintext HTTP, from a loopback address only (`refusedHttp` otherwise). |
| anything else | Close (`refusedSniff`). |

### 3.2 Preamble

Sent once by the consumer right after TCP or TLS setup. It is not a frame. 8 bytes (F:242-270):

```
off 0  [4]  magic        ASCII "MMQP"
off 4  u16  versionMajor 1
off 6  u16  versionMinor 0
```

- The source reads the preamble, then sends SERVER_HELLO. It never speaks first.
- A bad magic closes the connection silently. A major version other than 1 gets `GOAWAY(version)` and close (S:557-560). The minor version is ignored by both sides.

### 3.3 Encoding rules

- Every integer is little-endian. `i64` fields are two's complement.
- `str8` = `u8 len` + bytes. `str16` = `u16 len` + bytes. Encoders cut at 255 / 65535 bytes on a UTF-8 boundary (F:428-451). Frame decoders do not validate UTF-8 in frame strings.
- Frame bodies may be followed by extra bytes, which decoders ignore. Every listed field is mandatory: a body that ends before the last listed field is `ErrShortFrame`, a protocol fault (F:453-518).

### 3.4 Frame header and size caps

```
u32  frameLen   = 1 + len(body)     counts the type byte, not itself
u8   type
[frameLen-1] body
```

- `frameLen == 0` (`ErrFrameEmpty`) and `frameLen > cap` (`ErrFrameTooLarge`) are protocol faults. The reader closes the connection; the source sends `GOAWAY(protocol)` first for an oversize frame before authentication and for either fault after it, the consumer sends it after the handshake.

| Phase | Reader | Cap on `frameLen` |
|---|---|---|
| Before HELLO_OK | both | 4096 (`MaxPreAuthFrame`, F:33) |
| After authentication | source reading consumer frames | 65536 (`MaxConsumerFrame`, F:35) |
| After HELLO_OK | consumer reading source frames | `min(max(Fetch.MaxBytes, HELLO_OK.maxRecordBytes) + 65536, Receive.MaxFrameBytes)` (P:537-545). Default `Receive.MaxFrameBytes` = 16 MiB + 64 KiB. If `HELLO_OK.maxRecordBytes + 65536` exceeds it, the consumer logs an ERROR once; such records arrive as tombstones because HELLO.maxRecordBytes already told the source the lower limit. |

### 3.5 Frame catalogue

| Type | Id | Direction | Body |
|---|---|---|---|
| SERVER_HELLO | 0x01 | S→C | 45 bytes |
| HELLO | 0x02 | C→S | 111 fixed bytes + 4 strings |
| HELLO_OK | 0x03 | S→C | 111 fixed bytes + 3 strings |
| GOAWAY | 0x04 | both | `u16 code` + `str16 reason` |
| FETCH | 0x10 | C→S | 40 bytes |
| BATCH | 0x11 | S→C | 68-byte header + records |
| COMMIT | 0x12 | C→S | `u64 commit` |
| PING | 0x13 | C→S | `u64 token` |
| PONG | 0x14 | S→C | `u64 token` (echo) |

**Unknown and misdirected frames:**

| Where | Unknown type | Known type in the wrong direction |
|---|---|---|
| Source, before authentication | The first frame after SERVER_HELLO must be HELLO. A GOAWAY closes silently. Anything else, unknown types included, gets `GOAWAY(protocol)` (S:584-591). | same |
| Source, after authentication | Body read and ignored (S:886-887) | SERVER_HELLO, HELLO, HELLO_OK, BATCH, PONG: `GOAWAY(protocol, "unexpected X")` (S:883-885) |
| Consumer, handshake | Skipped (P:436-455) | Any known type other than the expected one or GOAWAY: close as a protocol error, without sending GOAWAY |
| Consumer, streaming | Body discarded (P:686-692) | Anything other than BATCH, PONG, GOAWAY: `GOAWAY(protocol)` |

#### SERVER_HELLO (0x01), body 45 bytes (F:521-550)

| off | size | field | value |
|---|---|---|---|
| 0 | 2 | versionMajor u16 | 1 |
| 2 | 2 | versionMinor u16 | 0 |
| 4 | 8 | capabilities u64 | the source's own set (3.6) |
| 12 | 1 | authModes u8 | b0 `CLIENT_CERT_REQUESTED`: TLS link and listener `ClientAuth` is not NONE. b1 `SHARED_SECRET`: any Serve peer has secrets; set on plaintext too (S:564-572). |
| 13 | 32 | nonceS | 32 bytes from a CSPRNG |

SERVER_HELLO discloses no NodeId, epoch or TopicRoot.

#### HELLO (0x02) (F:553-610)

| off | size | field | value |
|---|---|---|---|
| 0 | 2 | flags u16 | b0 `MAC`: the mac field is valid |
| 2 | 8 | capabilities u64 | the consumer's own set |
| 10 | 8 | instanceId u64 | random per process, low bit forced to 1, never 0 (`peerlink/manager.go:189-191`) |
| 18 | 8 | lastEpoch u64 | 0 = no epoch known |
| 26 | 8 | resumeOffset u64 | the consumer's `appliedNext`; 0 when no epoch is known |
| 34 | 8 | lastSeenLeo u64 | last `leo` seen from this source; 0 when no epoch is known |
| 42 | 4 | maxRecordBytes u32 | largest record frame (including `recLen`) the consumer accepts; 0 = no limit |
| 46 | 1 | retainedClass u8 | 0 MEMORY, 1 DB, 2 WINCCOA |
| 47 | 32 | nonceC | 32 bytes from a CSPRNG |
| 79 | 32 | mac | all zero when flags b0 is clear |
| 111 | str8 | consumerNodeId | own canonical NodeId |
| … | str8 | expectedSourceNodeId | the configured canonical NodeId of the source |
| … | str16 | topicRoot | own namespace root; empty unless WinCC OA native mode is active |
| … | str8 | oaSystem | own WinCC OA system name; empty unless embedded with native mode |

The Go consumer sets `maxRecordBytes = min(Receive.MaxFrameBytes − 65536, Log.MaxRecordBytes or MaxMessageSize + 64 KiB)` (P:593-599).

#### HELLO_OK (0x03) (F:613-675)

| off | size | field | value |
|---|---|---|---|
| 0 | 2 | flags u16 | b0 `SOURCE_RESET`, b1 `CONSUMER_STATE_USED`, b2 `SNAPSHOT_AVAILABLE` |
| 2 | 8 | capabilities u64 | agreed set = source own ∧ HELLO.capabilities |
| 10 | 8 | epoch u64 | non-zero, random per source process |
| 18 | 8 | resumeAt u64 | first offset the consumer will get |
| 26 | 8 | logStart u64 | LSO after resume trimming |
| 34 | 8 | leo u64 | |
| 42 | 8 | committed u64 | `C[c]` after resume |
| 50 | 8 | lostOnResume u64 | records evicted before the consumer could get them |
| 58 | 8 | wallNowMs i64 | Unix ms |
| 66 | 8 | monoNowMs u64 | ms since the source log was created |
| 74 | 4 | maxRecordBytes u32 | source capture cap = `min(Log.MaxRecordBytes, Log.MaxBytes / 4)` |
| 78 | 1 | retainedClass u8 | as in HELLO |
| 79 | 32 | macS | all zero unless a consumer MAC matched |
| 111 | str8 | sourceNodeId | own canonical NodeId |
| … | str16 | topicRoot | as in HELLO |
| … | str8 | oaSystem | as in HELLO |

#### GOAWAY (0x04) (F:678-697)

Body: `u16 code`, `str16 reason`. The sender closes after sending.

| Code | Name | Consumer treats as config error |
|---|---|---|
| 1 | version | yes |
| 2 | unknown_peer | yes |
| 3 | not_allowed | yes |
| 4 | auth_failed | yes |
| 5 | identity_mismatch | yes |
| 6 | self_connection | yes |
| 7 | wrong_node | yes |
| 8 | superseded | no |
| 9 | shutdown | no |
| 10 | protocol | no |
| 11 | offset_out_of_range | no |
| 12 | busy (defined, never sent) | no |
| 13 | duplicate_node | yes |

A config error makes the consumer wait the full `Fetch.ReconnectMaxMs` and log an ERROR at most every 5 min per code (F:214-221, P:230-260).

Reasons:

- When authentication is configured on the source (any Serve peer has secrets, or listener TLS with `ClientAuth` not NONE), every HELLO-check refusal is sent as `GOAWAY(auth_failed)` with an empty reason. Version and "expected HELLO" GOAWAYs keep their code but get an empty reason (S:551-556, 740-743).
- `GOAWAY(duplicate_node, "duplicate NodeId")` keeps its code and reason.
- GOAWAYs after authentication carry a short reason.
- The consumer always sends an empty reason.

#### FETCH (0x10), body 40 bytes (F:700-741)

| off | size | field |
|---|---|---|
| 0 | 4 | fetchId u32 (echoed by the BATCH) |
| 4 | 2 | flags u16 (b0 `SNAPSHOT`) |
| 6 | 2 | lingerMs u16 |
| 8 | 8 | offset u64 |
| 16 | 8 | commit u64 (0 = no change) |
| 24 | 4 | maxRecords u32 |
| 28 | 4 | maxBytes u32 |
| 32 | 4 | minRecords u32 |
| 36 | 4 | maxWaitMs u32 |

#### BATCH (0x11): 68-byte header, then `recordsBytes` of records (F:744-789)

| off | size | field |
|---|---|---|
| 0 | 4 | fetchId u32 |
| 4 | 2 | flags u16 |
| 6 | 2 | reserved u16 (0) |
| 8 | 8 | baseOffset u64 |
| 16 | 4 | count u32 |
| 20 | 4 | recordsBytes u32 |
| 24 | 8 | logStart u64 |
| 32 | 8 | leo u64 |
| 40 | 8 | lost u64 |
| 48 | 8 | sourceMonoMs u64 |
| 56 | 8 | sourceWallMs i64 |
| 64 | 4 | crc32c u32 (valid only if flags b2) |

The source writes `frameLen = 1 + 68 + recordsBytes`.

BATCH flags:

| Bit | Name | Meaning |
|---|---|---|
| b0 | GAP | `lost > 0`: the requested offset was below LSO |
| b1 | EMPTY | `count == 0` |
| b2 | CRC | `crc32c` is valid |
| b3 | SNAPSHOT | snapshot batch |
| b4 | SNAPSHOT_END | last snapshot batch |
| b5 | TRUNCATED | the read stopped at a slot evicted concurrently |

BATCH decode checks (F:836-853), each a protocol fault:

1. Body shorter than 68 bytes: `ErrShortFrame`.
2. `68 + recordsBytes > body length`: `ErrBatchRecords`.
3. `count × 4 > recordsBytes`: `ErrBatchCountRange`.

Bytes after the records region are ignored and not covered by the CRC.

#### COMMIT (0x12), PING (0x13), PONG (0x14)

Each body is one `u64`. COMMIT carries the commit offset. PING carries a token (the Go consumer sends Unix µs). PONG echoes the token; the source answers immediately from its read loop.

### 3.6 Capabilities

| Bit | Name | Offered when |
|---|---|---|
| b0 | BATCH_CRC | Always on plaintext. On TLS only with `Fetch.CrcOnTls` (`peerlink/manager.go:703-712`). |
| b1 | SNAPSHOT_FILL | `Snapshot.Mode == FILL` |
| b2 | RESYNC_NEWER | Always |
| b3 | TOMBSTONE | Always |

`CapsV1 = 0x0F`. The agreed set is the intersection of both own sets. Unknown bits are ignored. TOMBSTONE agreement is not checked: the source substitutes tombstones regardless.

### 3.7 Record format

Records are identical in the log and on the wire (R:12-53).

| off | size | field | notes |
|---|---|---|---|
| 0 | 4 | recLen u32 | bytes after this field |
| 4 | 1 | recVersion u8 | 1 |
| 5 | 1 | hdrLen u8 | 44 written; readers accept ≥ 44 and start the variable part at `hdrLen` |
| 6 | 2 | flags u16 | below |
| 8 | 8 | publishWallNs i64 | Unix ns at capture |
| 16 | 8 | captureMonoMs u64 | ms since the source log was created |
| 24 | 4 | expirySec u32 | message expiry interval; 0 = none |
| 28 | 1 | payloadFormat u8 | valid if flags b7 |
| 29 | 1 | reserved u8 | 0 |
| 30 | 2 | topicLen u16 | |
| 32 | 2 | clientIdLen u16 | |
| 34 | 2 | usernameLen u16 | |
| 36 | 4 | propsLen u32 | |
| 40 | 4 | payloadLen u32 | |
| hdrLen | … | topic, clientId, username, props, payload | in this order |

Flags (R:56-65):

| Bits | Meaning |
|---|---|
| b0-1 | QoS (mask 0x0003) |
| b2 | retain |
| b3 | dup |
| b4 | will |
| b5 | inline (published by an internal client) |
| b6 | reserved |
| b7 | payloadFormat present |
| b8 | snapshot |
| b9 | SKIPPED (tombstone) |
| b10-15 | reserved, ignored |

**Property TLVs** in the props block: `u8 id | u32 len | value` (R:36). The ids are MQTT 5 property ids:

| Id | Property | Value |
|---|---|---|
| 0x03 | ContentType | UTF-8 |
| 0x08 | ResponseTopic | UTF-8 |
| 0x09 | CorrelationData | bytes |
| 0x26 | UserProperty | `u16 keyLen` + key + value; one TLV per pair, `len = 2 + keyLen + valueLen` |

- The encoder writes only non-empty values, in the fixed order ContentType, ResponseTopic, CorrelationData, then the user properties in their original order (R:250-262).
- MessageExpiryInterval and PayloadFormat live in the fixed header. TopicAlias and SubscriptionIdentifier are never carried.
- The decoder lets the last of repeated 0x03/0x08/0x09 TLVs win, skips unknown ids and counts them (`unknownProps`).
- Every string or property value is at most 65535 bytes. A record that does not fit its length fields is not encodable (`RecordSize == 0`, counted `captureDropped{invalid}`).

**Record decode checks**, in order; each failure is a malformed record (R:373-441):

1. `4 + recLen` fits in the remaining buffer.
2. `4 + recLen ≥ 44`.
3. `recVersion == 1`.
4. `hdrLen ≥ 44`.
5. `hdrLen + topicLen + clientIdLen + usernameLen + propsLen + payloadLen == 4 + recLen`.
6. QoS ≠ 3.
7. If SKIPPED: stop here (tombstone).
8. Topic: 1..65535 bytes, valid UTF-8, no NUL, `+` or `#`. A leading `$` is not a decode error but a receiver drop (`namespace`).
9. clientId and username: valid UTF-8 without NUL; may be empty.
10. Props: every TLV complete; ContentType and ResponseTopic ≤ 65535 bytes, valid UTF-8, no NUL; CorrelationData ≤ 65535 bytes; UserProperty `len ≥ 2`, `2 + keyLen ≤ len`, value ≤ 65535 bytes, key and value valid strings.
11. Batch-relative: `captureMonoMs ≤ BATCH.sourceMonoMs` (R:593-597, 637-639). This check also applies to tombstones.

**Tombstone** (R:315-328): exactly 44 bytes. `recLen = 40`, version 1, `hdrLen = 44`, flags = original flags | SKIPPED, bytes 8..28 (publishWallNs, captureMonoMs, expirySec, payloadFormat) copied from the original, byte 29 and all length fields 0.

**Records region.** `count` records back to back at implicit offsets `baseOffset + i`. Malformed records and tombstones use up their offset. Structural faults:

- fewer than 4 bytes left, or `4 + recLen` beyond the region: `ErrRecordOverrun`;
- region used up before `count` records, or bytes left after `count` records: `ErrBatchCount`.

On a structural fault the consumer keeps what it applied, counts the undelimited rest as `dropped{malformed}`, logs an ERROR and advances to `baseOffset + count` (`peerlink/inject.go:236-246`). It does not close the link.

### 3.8 CRC32C

- Standard CRC-32C (Castagnoli), as `java.util.zip.CRC32C`.
- Coverage: BATCH header bytes 0..63 (flags already including b2), then every record frame in order, after tombstone substitution (F:800-870).
- Stored little-endian at header offset 64.
- When BATCH_CRC is agreed the source sets it on every batch, snapshot batches included.

### 3.9 Handshake

| Step | Direction | Action |
|---|---|---|
| 0 | | Admission, sniffing, optional TLS (11.4) |
| 1 | C→S | Preamble |
| 2 | S→C | SERVER_HELLO |
| 3 | C | `versionMajor == 1`, else `GOAWAY(version)` |
| 4 | C→S | HELLO |
| 5 | S | Source checks, first failure wins |
| 6 | S | Admit the session (takeover, duplicate detection), log resume |
| 7 | S→C | HELLO_OK; the source clears the deadline and switches to the 64 KiB frame cap |
| 8 | C | Consumer checks, then apply HELLO_OK to the link state |

The whole handshake has a 10 s deadline on both sides.

**Source checks** (`checkHello`, S:474-535):

| # | Check | Code |
|---|---|---|
| 1 | `Canonical(consumerNodeId)` invalid (lowercase, 1-64 chars of `[a-z0-9._-]`) | unknown_peer |
| 2 | canonical consumer id == own id | self_connection |
| 3 | not a Serve peer, but configured | not_allowed |
| 3 | not configured at all | unknown_peer |
| 4 | `Canonical(expectedSourceNodeId)` invalid or ≠ own id | wrong_node |
| 5 | client certificate present and does not verify for this peer (trust, pins, clientAuth EKU, identity) | identity_mismatch |
| 5 | no client certificate and the peer has `Tls.RequireClientCert` | identity_mismatch |
| 6 | peer has secrets and the link is plaintext | auth_failed |
| 7 | peer has secrets and HELLO flags b0 is clear | auth_failed |
| 7 | MAC matches none of the peer's secrets | auth_failed |
| 8 | neither certificate nor MAC authenticated, and `AllowUnauthenticatedPeers` is false | auth_failed |

After the checks pass (S:620-681):

1. **Admit.** An existing active session of this consumer gets `GOAWAY(superseded)`. Duplicate detection: if, within 60 s, three or more takeovers alternate between exactly two instanceIds, the newer instance gets `GOAWAY(duplicate_node, "duplicate NodeId")` and that instanceId is refused for 5 min.
2. If the broker is shutting down: `GOAWAY(shutdown)`.
3. **Resume** (3.10). An error gets `GOAWAY(offset_out_of_range)`.
4. Compute `oaRetained`, `topicRootMismatch` (both roots non-empty and different) and `retainedClassMismatch`; each mismatch logs one WARN per consumer.
5. `SNAPSHOT_AVAILABLE` is set only when all hold: (`lastEpoch == 0` or SOURCE_RESET), SNAPSHOT_FILL agreed, not `oaRetained`, and a retained store exists.
6. `macS` is computed with the secret whose index matched; otherwise it stays zero.
7. Send HELLO_OK. Release the pre-auth slot. Remember the remote IP as authenticated. The consumer state becomes CONNECTED.

**Consumer checks** (P:507-583):

| # | Check | Code |
|---|---|---|
| 1 | `Canonical(sourceNodeId)` == own id | self_connection |
| 2 | `Canonical(sourceNodeId)` invalid or ≠ the configured id | wrong_node |
| 3 | TLS, `InsecureSkipVerify` off, the server certificate fails verification (trust or pins, serverAuth EKU, identity), and the consumer has no secrets | identity_mismatch |
| 4 | TLS and secrets: the exporter is empty, or `macS` matches none of the secrets | auth_failed |
| 5 | neither certificate verified nor MAC ok, and `AllowUnauthenticatedPeers` is false | auth_failed |

With a secret, a certificate verification failure is tolerated and the MAC decides.

Then the consumer applies HELLO_OK (P:547-569):

- If `epoch` differs from the stored epoch and a stored epoch exists: `sourceResets++`, `resetLostLowerBound += max(0, lastSeenLeo − appliedNext)`, WARN.
- Store `epoch`, set `appliedNext = resumeAt`, `lastSeenLeo = leo`, `gapLostTotal += lostOnResume` (WARN when > 0).
- Compute `oaRetained`, `topicRootMismatch` and `retainedClassMismatch` as the source does.

**MAC** (M:12-81):

```
lp(x)    = u16 LE len(x) || x            (inputs over 65535 bytes are cut)
exporter = TLS 1.3 exporter, label "monstermq-peer/1", no context, 32 bytes
mac      = HMAC-SHA256(secret, lp("mmq-peer/1 C") | lp(nonceS) | lp(nonceC) | lp(consumerId) | lp(sourceId) | lp(exporter))
macS     = HMAC-SHA256(secret, lp("mmq-peer/1 S") | lp(nonceC) | lp(nonceS) | lp(sourceId) | lp(consumerId) | lp(exporter))
```

- `consumerId` and `sourceId` are the **canonical lowercase** NodeIds, never the received spelling. The source uses `Canonical(HELLO.consumerNodeId)` and its own id; the consumer uses its own id and the configured id of the source.
- `secret` is the base64-decoded secret (standard or URL alphabet, padded or not, at least 16 bytes decoded).
- The consumer signs with the first secret. Both sides accept any secret of the list (rotation). Comparison is constant-time.
- The consumer sets the MAC flag only on TLS, with secrets, and with a non-empty exporter.
- Go's `ExportKeyingMaterial(label, nil, 32)` under TLS 1.3 equals the RFC 8446 exporter with an empty context.

**`oaRetained`** = both retainedClass values are WINCCOA and both oaSystem strings are equal and non-empty (`peerlink/manager.go:715-717`). Both sides compute it independently.

### 3.10 Resume rules (`Log.Resume`, `peerlink/log.go:859-900`)

Offsets start at 1; `C[c]` starts at 1 in each epoch. A `resumeOffset` of 0 is treated as 1.

| HELLO | Decision |
|---|---|
| `lastEpoch == epoch` and `resumeOffset > leo` | Error, sent as `GOAWAY(offset_out_of_range)`. The consumer then forgets epoch and `appliedNext` (sets both to 0), so the next HELLO is a fresh start. |
| `lastEpoch == epoch` | `C = max(C, resumeOffset)`, trimming if the LWM rises. If `resumeOffset ≥ LSO`: `resumeAt = resumeOffset`, flag CONSUMER_STATE_USED. Else `resumeAt = LSO`, `lostOnResume = LSO − resumeOffset`. |
| any other `lastEpoch` (0 or old) | `SOURCE_RESET = (lastEpoch != 0)`. `resumeAt = max(C, LSO)`. `lostOnResume = max(0, LSO − C)`. |

### 3.11 Fetch, batch, commit, ping

**Source, on a FETCH** (S:827-1007):

1. Update `lastFetch` and run loss accounting for this consumer.
2. If `commit != 0`, apply it at once: `C = max(C, commit)`. A commit above `leo` gets `GOAWAY(protocol, "commit beyond log end")`.
3. Queue the FETCH (queue depth 2). Fetches are answered in order, one BATCH each.

Limits: `maxRecords` 0 → 4096, capped at 65536; `maxBytes` 0 → 1 MiB; `minRecords` at least 1; `maxWait = min(maxWaitMs, 60 s)`. One read also spans at most 16 chunks of 1024 records.

**Stream FETCH** (SNAPSHOT bit clear):

1. `offset == 0` or `offset > leo`: `GOAWAY(offset_out_of_range)`.
2. Long poll only if `offset ≥ LSO`, `offset + minRecords > leo` and `maxWait > 0`: wait until `leo ≥ offset + minRecords` or the timeout. A GAP fetch never waits.
3. Read from `base = max(offset, LSO)`, `lost = base − offset`. At least one record is returned when one exists, even if it exceeds `maxBytes`; after that, reading stops before `maxBytes` would be exceeded. A concurrently evicted slot stops the read and sets TRUNCATED; an empty truncated read is retried up to 3 times so the result becomes a GAP.
4. Linger: if `lingerMs > 0`, `0 < count < maxRecords`, `lost == 0` and `bytes < maxBytes`, wait until `leo ≥ offset + maxRecords` or `lingerMs`, then read again.
5. Header: echo `fetchId`; `baseOffset = base` (also for an empty batch); `count`, `logStart`, `leo`, `lost` from the read; flags GAP, TRUNCATED, EMPTY, CRC as applicable; `sourceMonoMs` and `sourceWallMs` stamped at write time.
6. Tombstones: every record frame longer (including `recLen`) than a non-zero `HELLO.maxRecordBytes` is replaced by its tombstone (`servedSkipped{size}`).

**Snapshot FETCH** (SNAPSHOT bit set, `peerlink/snapshot.go:90-148`):

- The source does not distinguish FILL from NEWER; both are a plain FETCH(SNAPSHOT).
- The retained set is materialised at the first FETCH(SNAPSHOT) of a session: non-empty payloads only, the capture filter (4.1) applied, truncated at `Snapshot.MaxTopics`. A failed scan ends the session.
- If no snapshot is possible on this session (neither FILL nor NEWER agreed, or no retained store), the source answers `SNAPSHOT | EMPTY | SNAPSHOT_END`.
- Each batch: up to `maxRecords` records and `maxBytes` bytes (at least one record); flags SNAPSHOT plus EMPTY, CRC, SNAPSHOT_END as applicable; `baseOffset = 0`; current `logStart`/`leo`; `lost = 0` except on the SNAPSHOT_END batch, which carries the truncated-topic count. Tombstone substitution applies.
- `offset`, `minRecords`, `maxWaitMs` and `lingerMs` are ignored.
- After SNAPSHOT_END the state resets; a later FETCH(SNAPSHOT) builds a fresh snapshot.

Snapshot record fields:

| Field | Value |
|---|---|
| flags | SNAPSHOT, RETAIN, QoS, payloadFormat bit; dup cleared |
| clientId | the retained message's origin client id |
| username | empty |
| publishWallNs | `created_sec × 1e9` (second resolution) |
| captureMonoMs | `nowMono − age`, clamped at 0 |
| expirySec | `remaining + ageSec`, or 0 without expiry; expired values are skipped |

**Consumer** (P:876-1115):

- First stream FETCH: `offset = appliedNext`; each later FETCH: `offset = baseOffset + count` of the previous batch.
- `commit` in FETCH = `appliedNext` only if it grew since the last COMMIT or FETCH commit on this session, else 0.
- `minRecords = 1`, `maxWaitMs = max(Fetch.MaxWaitMs, 10)`, `lingerMs = Fetch.LingerMs`, `maxRecords = Fetch.MaxRecords`, `maxBytes = Fetch.MaxBytes`.
- Snapshot FETCH: `offset = 0`, `commit = 0`, `lingerMs = 0`, `maxWaitMs = 0`, `minRecords = 1`. Every reply must have SNAPSHOT set; a non-snapshot reply during the snapshot phase, or a snapshot reply while streaming, gets `GOAWAY(protocol)`.
- COMMIT `baseOffset + count` after each applied batch when it grew; during a long apply, every 64 records once 100 ms have passed since the last commit.
- Graceful stop: final COMMIT, then `GOAWAY(shutdown)`.
- Records with `offset < appliedNext` are skipped as duplicates (`dupSkipped`).
- CRC failure: `GOAWAY(protocol)` and reconnect. The third consecutive failure at the same `baseOffset` (stream batches only) makes the batch poison: all `count` records count as `dropped{malformed}` and the consumer advances.

**Record age** on the consumer:

```
ageMs = (BATCH.sourceMonoMs − captureMonoMs) + (local now − batch receive time) + rtt/2
expired = expirySec > 0 && ageMs ≥ expirySec × 1000
```

### 3.12 Timers

| Who | Timer | Value (default `KeepAliveSeconds` = 10) |
|---|---|---|
| Both | handshake deadline | 10 s |
| Source | sniff | 3 s |
| Source | TLS handshake | 10 s |
| Source | read deadline per consumer frame | 3 × keepAlive (30 s) |
| Source | write deadline per BATCH, set once | keepAlive × (1 + totalBytes / 64 KiB) |
| Source | other writes (PONG, GOAWAY) | keepAlive |
| Consumer | read deadline until a BATCH header | maxWait + keepAlive |
| Consumer | read deadline after the header | keepAlive per 64 KiB chunk |
| Consumer | PING while streaming | every keepAlive/2, only while no FETCH is outstanding |
| Consumer | PING during the snapshot | every max(keepAlive/2, 100 ms), unconditional |
| Consumer | dial | 5 s |

PONG updates the RTT estimate.

### 3.13 Compatibility notes for other implementations

- **All HELLO and HELLO_OK fields are mandatory**, including the trailing `str8 oaSystem`. A non-WinCC-OA implementation sends an empty string (one `0x00` byte). A peer that omits the field is rejected with `ErrShortFrame` / `GOAWAY(protocol)`. Appending new fields after `oaSystem` is safe.
- New frame types are safe only after authentication. Before HELLO_OK the source accepts nothing but HELLO or GOAWAY.
- Canonicalise NodeIds (lowercase, `[a-z0-9._-]{1,64}`) before comparing them and before computing a MAC.
- Every integer field is unsigned unless marked `i64`; on the JVM use `Long`/`Int` with unsigned helpers (`java.lang.Long.compareUnsigned` etc.).
- Encode user properties as one TLV per pair in original order; keep duplicate keys.
- Clock fields (`sourceMonoMs`, `captureMonoMs`, `monoNowMs`) are milliseconds of a monotonic clock that starts at 0 when the source log is created. `captureMonoMs` of a record must never exceed the `sourceMonoMs` of the batch that carries it.

---

## 4. Source behaviour

### 4.1 Capture

**Tap points** (`peerlink/hook.go:61-132`). All taps run synchronously on the publisher's goroutine after the publish was accepted, so per-publisher FIFO order is kept.

| Tap | Captures |
|---|---|
| `OnRetainMessage` | Retained publishes (when `RetainAvailable == 1`), inside the retained apply, before the PUBACK and before fan-out. Skips wills and replicas. |
| `OnPublished` | Everything else: non-retained publishes. A replica increments `skipPeer` and may feed the echo table, then returns. |
| `OnWillSent` | Wills. Replica check first; then skip (`skipWill`) if `Capture.Wills` is false or the broker is draining. A retained will reaches `OnRetainMessage` too, is recognised by `Packet.Will`, and is captured once, here, as a will record. |

The hook is registered after the WinCC OA native hook and before StorageHook and QueueHook (`broker/server.go:336-419`).

**Capture chain** (`peerlink/hook.go:165-224`):

1. Capture inactive (no Serve peer, or switched off by drain/close): if the log is sealed and the publish would have been captured, count `uncapturedAtShutdown`; return.
2. `pk.Ignore` (native commands, topics branch): return, not counted.
3. Filter `accept(topic)`, else `filtered`:
   - empty topic or a topic starting with `$`;
   - under the own WinCC OA root, only while native mode is active;
   - `Capture.Include` minus `Capture.Exclude` (Exclude wins). Empty Include means `#`. Exclude unset means `[<HMI.SyncBaseTopic>/#]` (default `monstermq/hmi/sync/#`); an explicit `[]` excludes nothing.
4. Build the record: QoS, retain, dup, payload format, topic, payload, ContentType, ResponseTopic, CorrelationData, user properties; flags INLINE (internal client) and WILL; expiry = `min(MessageExpiryInterval, MaximumMessageExpiryInterval)`.
5. Publisher: `clientId` = the client id (`inline` for internal publishers); `username` = the session's username. An invalid username is stripped and counted `usernameStripped`.
6. Wills carry `expirySec = 0`.
7. Inline publishes must have valid strings (topic, UTF-8 without NUL), else `captureDropped{invalid}`.
8. Echo suppression (only with `Capture.EchoSuppressMs > 0`, never for wills): a publish whose topic, payload hash and retain flag match a replica applied within the window counts `echoSuppressed` and is not captured.
9. Size: not encodable → `captureDropped{invalid}`; larger than `min(Log.MaxRecordBytes, Log.MaxBytes / 4)` → `captureDropped{size}`.
10. Timestamps from one clock read: `publishWallNs` and `captureMonoMs`. Encode and append.

**Retained ordering.** When a log exists and the retained class is not WINCCOA, the engine serialises, per topic (64 FNV stripes), the retained-store update together with the `OnRetainMessage` hooks (`SerializeRetained`, `mqtt/server.go:1167-1196`). Log order then equals retained apply order per topic. The WINCCOA class is excluded because its writes wait for WinCC OA answers that may queue behind a publish of the same topic.

**Shutdown wills.** `BeginDrain` runs before the MQTT listeners close, so wills caused by the broker's own shutdown are not captured (`skipWill`). Delayed wills fire only while the client is still registered; a delayed will of a departed client is not captured and not counted.

**Recapture** (`peerlink/hook.go:149-163`). `recapture(pk)` appends a local retained value again as published by its origin, with no username and the remaining expiry. It is used only for superseded retained wills (6.4).

### 4.2 The log (`peerlink/log.go`)

| Aspect | Behaviour |
|---|---|
| Structure | Chunks of 1024 slots holding pointers to immutable encoded record frames; 4 spare chunks pre-allocated by a background goroutine (`spareMisses` on a miss) |
| Offsets | `LSO = LEO = LWM = 1` at start. Records live at `[LSO, LEO)`. |
| Epoch | Random non-zero u64 from a CSPRNG at log creation; new on every process start |
| Consumers | Static: one per Serve peer. Each starts with `committed = served = 1`, state NEVER_CONNECTED, and pins the log from offset 1. WARN once per consumer after `Log.NeverConnectedWarnSec` (300 s). |
| Byte accounting | Allocation size class per record (8 KiB pages above 32 KiB) plus about 16 KiB per attached chunk |
| Append | Validate; if sealed count `uncaptured` and discard; else store at `LEO`, `LEO++`, count `appended{client|inline|will}`. With zero consumers a record is trimmed at once. Then evict, then wake due waiters. |
| Eviction | While `LEO − LSO > MaxMessages` or `bytes > MaxBytes`, drop the record at LSO, never the one just appended. Counters `evictedBy{count|bytes}`; `evictedUnread` when the dropped offset is ≥ LWM. |
| Commit and trim | `C[c] = max(C[c], off)`; above LEO is an error. `LWM = min(C[*])`; everything below LWM is freed (`trimmed`). Each commit signals the drain waiter. |
| Loss accounting | On every FETCH, resume, read and status snapshot (not while a read of that consumer is in progress): `a = max(acctNext, committed, served)`; if `LSO > a` then `lostTotal += LSO − a`, `acctNext = LSO`. A read advances `served`. |
| Read | Snapshot LSO, LEO and up to 16 chunk pointers under the lock; load slots outside it. `from < LSO` gives `base = LSO`, `lost = LSO − from`. |
| Waiters | One reusable waiter per session; woken when `LEO` reaches its target, on timeout, or on close |
| Seal and drain | `Drain` fixes `target = LEO` and waits until every CONNECTED consumer has `committed ≥ target` (poll 20 ms and on commit), or the timeout. Then `Seal`: later appends count `uncapturedAtShutdown`; LEO is final. |
| capacitySeconds | `(MaxBytes − bytes) / EWMA(appended bytes/s)`, EWMA weights 0.8/0.2 sampled every second; null until the rate exceeds 1 B/s |

### 4.3 Sessions

**Admission** (`peerlink/server.go:268-435`), in order:

1. `Listener.AllowedNetworks`, if non-empty, before TLS (`refusedNetwork`).
2. Pre-auth slot: at most `Listener.MaxPreAuthPerIp` (default 2) per IPv4 address or IPv6 /64, and 16 in total. IPs of configured peer addresses (re-resolved every minute) and IPs that authenticated within 24 h (up to 256 remembered) bypass the global cap (`refusedBusy`). The slot is released after HELLO_OK.
3. Sniffing (3.1).
4. Pre-auth frame cap 4096.

**Session** (`peerlink/server.go:813-1064`): a read loop (FETCH, COMMIT, PING, GOAWAY) and a serve loop (one BATCH per FETCH). Writes use `writev` on plain TCP and a 64 KiB buffered writer on TLS. On release, the consumer state becomes DISCONNECTED only if this session was still the active one. `C[c]` survives every state.

**Takeover.** A new authenticated session of the same consumer supersedes the active one (`GOAWAY(superseded)`). Duplicate detection is described in 3.9; it fires only on sustained flapping between two live processes, not on ordinary restarts.

### 4.4 Snapshot serving

See 3.11. `snapshotServed` counts snapshot records; they do not count in `messageBusOut`. A snapshot can be served even on an `oaRetained` link if the consumer asks; the Go consumer never asks then.

---

## 5. Consumer behaviour

**States:** STOPPED, BACKOFF, DIALING, HANDSHAKE, SNAPSHOT, STREAMING. One run loop per pulled peer.

**Backoff** (`peerlink/puller.go:213-256`):

- Starts at 200 ms, doubles up to `Fetch.ReconnectMaxMs` (30 s), ±20 % jitter.
- Reset to 200 ms after a session that applied anything or lived ≥ 30 s.
- Config-error GOAWAY (3.5): wait the full cap.
- Resync request: reconnect at once.

**Snapshot decision** (`peerlink/puller.go:345-398`):

- `snapPending` is cleared when FILL is not agreed or not configured, or the link is `oaRetained`; it is set by `SNAPSHOT_AVAILABLE`.
- An operator resync runs a NEWER snapshot if RESYNC_NEWER is agreed; it also clears `snapPending`.
- Otherwise a pending FILL runs. `snapPending` is cleared only after SNAPSHOT_END has been applied and flushed, so an interrupted FILL is retried on the next session even when it resumes the same epoch (`snapshotsInterrupted`, WARN unless the stop was deliberate).

**Snapshot phase** (`peerlink/puller.go:724-807`): synchronous, one FETCH(SNAPSHOT) per batch, a separate goroutine sends PING. For DB and WINCCOA retained classes the local retained topics and their created times are preloaded once with a full store scan; MEMORY uses per-record engine lookups. At SNAPSHOT_END: `snapshotTruncated += lost`, flush retained replicas, INFO. Snapshot records are never paced and never stale.

**Streaming** (`peerlink/puller.go:876-1115`):

| Goroutine | Role |
|---|---|
| Writer | The only writer: FETCH, COMMIT (only if greater than the last sent), PING (only while no FETCH is outstanding), final COMMIT and GOAWAY(shutdown) |
| Reader | Sends FETCH, reads BATCH, checks CRC, hands batches to the injector. `Fetch.Pipeline` (clamped 1..2): with 2, the next FETCH goes out as soon as a BATCH header is read; with 1, right after the batch is handed off (handoff capacity = pipeline). |
| Injector | Applies batches in order, flushes retained replicas, sets `appliedNext = baseOffset + count`, sends COMMIT. Mid-batch: every 64 records, if ≥ 100 ms passed, flush, advance and send a non-blocking COMMIT. |

**Graceful stop** (only while STREAMING): the injector finishes the current batch, flushes, sends the final COMMIT and GOAWAY(shutdown), and waits up to 2 s for the writer. A batch still in the handoff buffer is neither applied nor committed and is re-served later. In other states, stop cancels at once.

**Dedup:** `offset < appliedNext` → `dupSkipped`.

**Pacing** (`peerlink/inject.go:106-175`):

- `lag = BATCH.leo − offset − 1`.
- If `Receive.CatchUpRateFactor > 0` (default 3) and `lag > Fetch.MaxRecords`: rate = `max(factor × smoothed source append rate, 1000/s)`. The append rate is an EWMA (0.7/0.3) of LEO deltas over source mono time, sampled in windows of at least 50 ms.
- `Receive.MaxApplyRate > 0` caps the rate at all times.
- Token bucket with burst = rate/10 and a 2 ms minimum sleep; `paced` counts sleeps.

**Clock skew:** `skew = sourceWallMs − (tsend + trecv)/2`, sampled only when `trecv − tsend ≤ 2·RTT + 20 ms`, smoothed 7/8; WARN when `|skew| > 1 s`, at most every 10 min. It corrects NEWER comparisons and snapshot backdating.

**GAP:** `gapLostTotal += lost`, WARN at most every 10 s.

**`GOAWAY(offset_out_of_range)`:** epoch and `appliedNext` are set to 0.

---

## 6. Receiver injection

### 6.1 Injector client

One inline client per source: id `peerlink:<sourceNodeId>`, listener `peerlink`, protocol version 5, not in the client registry, so it bypasses ACL, topic validation, size and receive quota (`peerlink/puller.go:128-129`). Network clients with id `inline` or an id starting with `peerlink:` are refused at CONNECT with reason 0x85 (`refusedClientIds`).

### 6.2 Per-record validation and drop reasons (in this order)

| # | Step | Result |
|---|---|---|
| 1 | Record decode checks (3.7) | `dropped{malformed}`; a structural fault drops the rest of the batch |
| 2 | Stream record with `offset < appliedNext` | `dupSkipped` |
| 3 | Tombstone | `dropped{size_source}`; retained also `retainedDiverged{size_source}` |
| 4 | Topic starts with `$`, or lies under the own root (native active) or under the source's announced root | `dropped{namespace}` |
| 5 | `Peers[].Receive.Include/Exclude` | `dropped{filtered}` |
| 6 | Payload larger than the receiver's `MaxMessageSize` | `dropped{size}`; retained also `retainedDiverged{size}` |
| 7 | FILL snapshot record and the topic exists locally | `snapshotSkippedPresent` |
| 8 | Compute `ageMs` (3.11) | |
| 9 | Expired, except a retained delete | `dropped{expired}`; retained also `retainedDiverged{expired}` |
| 10 | Stale: `Receive.MaxRecordAgeMs > 0` and `ageMs` above it, never for snapshot records; non-retained | `dropped{stale}` |
| 11 | Will, and the client is connected here or established a session here at or after `now − age` | `dropped{will_superseded}`; see 6.4 |
| 12 | NEWER snapshot record and a local value exists: skip if `srcSec ≤ localCreated + 1` (`srcSec` = skew-corrected publish second) | `snapshotSkippedPresent`, else `snapshotNewer` and apply |

The eight `dropped` reasons in the status are `malformed`, `size_source`, `namespace`, `filtered`, `size`, `expired`, `stale`, `will_superseded`.

### 6.3 Packet construction (`peerlink/inject.go:358-389`)

| Field | Value |
|---|---|
| Fixed header | PUBLISH, QoS and retain from the record; DUP not set; `PacketID = 1` when QoS > 0 |
| Topic, payload | from the record |
| `Origin` | original client id, so NoLocal works per logical client |
| `Created` | `max((nowMs − ageMs) / 1000, 1)` |
| Properties | from the record; `MessageExpiryInterval = expirySec` (the original interval). The engine computes `Expiry = Created + min(Max, interval)`, which preserves the remaining expiry to about ±1 s. |
| Snapshot backdating | If the skew-corrected wall time is older than `Created`, `Created` moves back to it and the interval grows by the same amount, so the absolute expiry is unchanged. |
| `Receive.MarkReplicas` | appends user property `mmq-peer-src = <sourceNodeId>` |
| `Forward` | `{SourceNode, ClientID, Username, TimeNs = publishWallNs, Epoch, Offset, Dup, Will, Snapshot}` |

Strings are interned per source (two generations of 64k entries or 8 MiB each; strings above 256 bytes are not interned).

### 6.4 Apply

- **Retained and stale, or an expired retained delete:** `RetainOnly` (update the retained store, fire the retained hooks, no delivery, no OnPublished, no counters). Counted `retainOnly`; the value also enters the echo table.
- **Otherwise:** inject as a publish (`InjectPacket`); counted `injected`, plus `snapshotFilled` for snapshot records.
- **Engine error:** `rejected`, WARN at most every 10 s.
- `appliedBytes` and the apply-delay histogram observe each applied record.

**Will supersession.** A will whose client is (or became) connected on the receiver is dropped. If that will was retained, the source has already applied it to its retained store. The receiver therefore reads its own current retained value of the topic and, if it serves the source and the value is non-empty, recaptures it into its own log (`supersededWillResent`). The source then converges back to the live value. Session establishment times are kept in 16 shards, pruned after 24 h, at most 16k per shard, and recorded only when pullers exist.

### 6.5 Gates for replicas

| Gate | Behaviour |
|---|---|
| Shared subscriptions | `Receive.SharedSubscriptions: SKIP` (default): replicas are not delivered to shared groups (`sharedSkipped`). `DELIVER` delivers them. |
| Offline QoS > 0 delivery | Skipped for replicas unless `Receive.Queue` (`QueueOfflineReplicas`) |
| QueueHook | Returns if `!Receive.Queue`, or the replica is a will or a snapshot value. Removes the original publisher (`Origin`) from the offline list. With PeerLink, hydrates offline queues only for sessions last connected to this node. |
| Bus and archives | Replicas count `messageBusIn` instead of `messagesIn`/client in. Wills and snapshot values stop here. Bus gated by `Receive.Bus`, archives by `Receive.Archive` (both default on). Archive rows use the source client id, `Time = publishWallNs`, `IsDup = Forward.Dup`, `OriginNode = sourceNodeId`, and a UUID derived from `fnv64(src) ^ epoch` and the offset (random for snapshot values). |
| Bridge outbound | Bus messages with `OriginNode` set are not sent to outbound MQTT bridges unless `Receive.BridgeOutbound` |

### 6.6 Metrics semantics

| Metric | Replicas |
|---|---|
| `messagesIn`, per-client in | not counted |
| `messageBusIn` | one per replica that reaches OnPublished, wills and snapshot fills included; not `RetainOnly` |
| `messageBusOut` | live records served, tombstones included; snapshot records excluded |
| `messagesOut` | counts deliveries of replicas to local subscribers |
| `$SYS` messages received | includes replicas |

### 6.7 Retained replicas per store class

| Receiver store | Behaviour |
|---|---|
| MEMORY | Engine map updated by the apply; immediate store write |
| DB (SQLite, PostgreSQL, MongoDB; WINCCOA without `oaRetained`) | Per-source pending map `topic → last value`, written by `FlushReplicas` with one `AddAll` and one `DelAll` |
| WINCCOA with `oaRetained` | In-memory view only (`ApplyCached`): no WinCC OA call, entry marked cached so a later local write still creates the datapoint |

Pending-map rules (`broker/hook_storage.go:411-695`):

- A replica removes pending values of other sources for the same topic.
- A local retained write drops all pending values of the topic and waits for an in-flight flush of it, which then must not re-queue.
- Flushes are serialised. Failed values are re-queued unless superseded; the error is counted (`retainedFlushErrors`) but the commit still proceeds.
- The injector flushes after every batch, before mid-batch commits, at SNAPSHOT_END and on finish. Snapshot DB writes therefore become visible only at SNAPSHOT_END. Until a flush, new subscribers in DB mode see the old value.

---

## 7. Loop prevention

**Rule (split horizon).** A packet is a replica when its client is on listener `peerlink` or it carries `Forward`. Every tap tests this first and never captures a replica (`peerlink/hook.go:98-132`). Snapshot values are replicas too. Paths therefore have length 1; a chain delivers one hop only, so meshes must be full.

**Defence in depth:** `self_connection` and `wrong_node` on both sides; the namespace is dropped on both sides; reserved client ids.

**Remaining loop and duplicate risks:**

| Relay | Effect | Guard |
|---|---|---|
| Inbound MQTT bridge subscribed to a peer broker | Republished as an internal publish, captured, sent back; unbounded with topic remapping | Startup WARN when a bridge host equals a peer's address host (`broker/peerlink.go:200-205`) |
| Outbound bridge to a peer with `BridgeOutbound: true` | Loop | Default `false` |
| Scripts, Redfish, any bus consumer deriving a publish from a replica | Captured and forwarded; an identity republish loops; devices running on both nodes duplicate | WARNs for shared config store with node `local`/`*`, Redfish, HostMonitoring without `{NodeId}` (`broker/peerlink.go:150-218`) |
| External client connected to both brokers that republishes | Loop | `Capture.EchoSuppressMs` (exact topic, payload, retain within the window; not wills); `Receive.MarkReplicas` lets clients filter |
| Superseded retained will | One value sent back on purpose | Arrives as a normal record; does not ping-pong |

---

## 8. Shutdown sequence and drain

`broker.Server.Close()` → `closePeerLink()` (`broker/server.go:859-917`):

1. `StopPullers` (10 s budget): graceful batch finish, flush, final COMMIT, `GOAWAY(shutdown)`.
2. `BeginDrain`: wills are no longer captured.
3. `CloseListeners`: close every MQTT listener, disconnect clients with reason 0x8B, wait for their goroutines including wills; later connections are refused.
4. Stop internal publishers (bridges, WinCC UA/OA bridges, RTSP, scripts, host monitoring, HMI sync, Redfish), then the GraphQL/REST/MCP publish APIs, then the native status publisher.
5. `Drain(timeout = DrainOnShutdownMs + 5 s)`:
   - `drainTarget = LEO`;
   - wait up to `Log.DrainOnShutdownMs` (default 2000; 0 = no wait) until every CONNECTED consumer has committed `drainTarget`;
   - seal the log, deactivate capture, close the peer listener;
   - `GOAWAY(shutdown)` to all sessions; wait for their goroutines;
   - per consumer `shutdownUnserved = LEO_final − C[c]` (WARN if > 0, else INFO); `uncapturedAtShutdown` (WARN if > 0). Commits arriving between the seal and the session close still count.
6. Close the manager and the log.
7. The rest of the old order (`native.Stop`, stores). The native service is the only inline publisher still running during the drain; its late publishes count as uncaptured.

A consumer counts as CONNECTED from HELLO_OK on, so the drain also waits for a consumer in its snapshot phase.

---

## 9. Delivery guarantees

**Ordering:** per source in offset order (one injector); per publisher FIFO; per retained topic in apply order (except the WINCCOA class).

**QoS:** the injector uses the inline path; each subscriber gets `min(QoS, subscription QoS, MaximumQos)`. QoS 2 is not exactly-once across a consumer crash.

| Case | Outcome | Counters |
|---|---|---|
| Connection drop, both processes up | Exactly once (resume at `appliedNext` plus dedup) | `reconnects`, `dupSkipped` |
| Graceful consumer stop while streaming | Exactly once (final COMMIT) | |
| Source drain within the deadline | Exactly once up to `drainTarget` | `shutdownUnserved = 0` |
| Consumer crash | Records applied after the last COMMIT the source saw are replayed: at most about one batch or 100 ms of apply | |
| QoS 1 DUP retransmit on the source | Captured again (duplicate) | |
| Snapshot FILL after a consumer restart | Absent retained topics re-injected and delivered live with `retain = false` | `snapshotFilled` |
| Source crash | Backlog lost | `sourceResets`, `resetLostLowerBound` (lower bound) |
| Log overflow | Oldest records lost | source `lostTotal`, `evictedUnread`; consumer `gapLostTotal`, `lostOnResume` |
| Drain timeout or disconnected consumer | Remaining records lost | `shutdownUnserved` |
| Publishes after the seal | Not captured | `uncapturedAtShutdown` |
| Capture drops | | `captureDropped{size,invalid}`, `filtered`, `echoSuppressed` |
| Wills not captured | Shutdown wills, `Capture.Wills: false` (`skipWill`); delayed wills of departed clients (not counted) | `skipWill` |
| Receiver policy drops | | `dropped{...}`, `rejected` |
| Snapshot truncation | | `snapshotTruncated` |

**Retained divergence:** size, size_source and expired retained values are not applied (`retainedDiverged`); FILL never overwrites a present value; NEWER cannot delete; active-active resolves by arrival order; a DB flush failure keeps the value in memory with retry while the commit proceeds, so a consumer crash before a successful retry loses it.

**Not delivered by design:** shared groups (SKIP), offline queues (`Receive.Queue: false`), bus and archives for wills and snapshot values, superseded wills.

---

## 10. Configuration reference

All keys live under `PeerLink` (`config/config.go:806-960`, getters `:983-1148`). `Load` decodes this section a second time with unknown fields forbidden, so an unknown key fails startup even while `Enabled` is false (`config/load.go:25-48`). Validation runs only when `Enabled` is true.

| Key | Type | Default | Validation |
|---|---|---|---|
| `Runtime.MemoryLimitMB` (top level) | int | 0 (Go default) | ≥ 0; passed to the Go soft memory limit |
| `Enabled` | bool | false | |
| `AllowUnauthenticatedPeers` | bool | false | needs non-empty `Listener.AllowedNetworks`; forbidden with `UserManagement.Enabled` |
| `SharedSecrets` | []string base64 | – | each ≥ 16 decoded bytes; need `Tls.Enabled`; first entry signs |
| `KeepAliveSeconds` | int | 10 | ≥ 1 |
| `Listener.Address` | string | `0.0.0.0` | |
| `Listener.Port` | int | 0 → 1890 | 0..65535 |
| `Listener.AllowedNetworks` | []CIDR | – | valid CIDRs; checked before TLS, also for the status endpoint |
| `Listener.MaxPreAuthPerIp` | int | 2 | ≥ 1 |
| `Listener.AllowPlaintext` | bool | false | on a serving TLS listener needs the waiver |
| `Tls.Enabled` | bool | false | needs `CertPath` + `KeyPath`, or `AutoGenerate`; also the dialer default |
| `Tls.CertPath` / `Tls.KeyPath` | string | with `AutoGenerate`: `certs/peer-{NodeId}.pem` / `.key` | `{NodeId}` expanded |
| `Tls.TrustStorePath` | string | – | |
| `Tls.TrustStoreType` | string | PEM | PEM or PKCS12 |
| `Tls.TrustStorePassword` | string | – | |
| `Tls.ClientAuth` | enum | NONE | NONE, REQUEST, REQUIRED; non-NONE needs `Tls.Enabled` |
| `Tls.IdentityFallback` | enum | NONE | NONE, DNS, CN |
| `Tls.AutoGenerate` | bool | false | |
| `Log.MaxMessages` | int | 2,000,000 | ≥ max(100, `Fetch.MaxRecords`) |
| `Log.MaxBytes` | int64 | 256 MiB | ≥ 1 MiB and ≥ 4 × `MaxRecordBytes` |
| `Log.MaxRecordBytes` | int | 0 → `MaxMessageSize` (1 MiB if 0) + 64 KiB | ≥ 0 |
| `Log.DrainOnShutdownMs` | int | 2000 | ≥ 0 |
| `Log.NeverConnectedWarnSec` | int | 300 | ≥ 0 |
| `Capture.Wills` | bool | true | |
| `Capture.Include` | []filter | `["#"]` | valid filters |
| `Capture.Exclude` | []filter | unset → `[<HMI.SyncBaseTopic>/#]`; `[]` → none | valid filters |
| `Capture.EchoSuppressMs` | int | 0 | ≥ 0 |
| `Snapshot.Mode` | string | FILL | FILL or OFF |
| `Snapshot.MaxTopics` | int | 1,000,000 | ≥ 1 |
| `Fetch.MaxRecords` | int | 4096 | ≥ 1 |
| `Fetch.MaxBytes` | int | 1 MiB | ≥ 1 |
| `Fetch.MaxWaitMs` | int | 1000 | 10 ≤ value < `KeepAliveSeconds` × 1000 |
| `Fetch.LingerMs` | int | 0 | ≥ 0 |
| `Fetch.Pipeline` | int | 1 | 1 or 2 |
| `Fetch.CrcOnTls` | bool | false | |
| `Fetch.ReconnectMaxMs` | int | 30000 | > 0 |
| `Receive.Bus` / `Receive.Archive` | bool | true / true | |
| `Receive.BridgeOutbound` | bool | false | |
| `Receive.Queue` | bool | false | |
| `Receive.SharedSubscriptions` | string | SKIP | SKIP or DELIVER |
| `Receive.MarkReplicas` | bool | false | |
| `Receive.CatchUpRateFactor` | float | 3.0 | 0 (off) or ≥ 1.5 |
| `Receive.MaxApplyRate` | int | 0 | ≥ 0 |
| `Receive.MaxRecordAgeMs` | int | 0 | ≥ 0 |
| `Receive.MaxFrameBytes` | int | 16 MiB + 64 KiB | ≥ `Fetch.MaxBytes` + 64 KiB |
| `Receive.InjectWorkers` | int | 1 | 1..16; values > 1 are reserved and only log a WARN |
| `Peers[].NodeId` | string | – | canonical; no duplicates (case-insensitive) |
| `Peers[].Address` | host:port | – | if set, this node pulls from the peer |
| `Peers[].Serve` | bool | true | the peer may pull from this node |
| `Peers[].SharedSecrets` | []string | – | replace the group secrets for this peer |
| `Peers[].Tls.Enabled` | bool | `Tls.Enabled` | dialer TLS |
| `Peers[].Tls.PinnedSha256` | []string | – | 64 hex digits (colons/spaces allowed), SPKI or certificate SHA-256; need TLS |
| `Peers[].Tls.CertificateIdentity` | string | – | exact URI SAN or DNS SAN |
| `Peers[].Tls.ServerName` | string | – | SNI only |
| `Peers[].Tls.RequireClientCert` | bool | false | needs `ClientAuth` REQUEST or REQUIRED |
| `Peers[].Tls.InsecureSkipVerify` | bool | false | |
| `Peers[].Receive.Include` / `Exclude` | []filter | `["#"]` / – | valid filters |

**NodeId.** Resolution order: explicit `NodeId`, then the hostname, then `edge`; with PeerLink enabled the `edge` fallback is an error. The canonical form is lowercase, 1-64 bytes of `[a-z0-9._-]`. The `Peers` entry equal to the own NodeId is ignored (INFO), so one file can serve every host. When the NodeId came from the hostname, an entry equal to the hostname's first DNS label also matches, and the link then uses that label as its NodeId (HELLO, MAC, certificate URI SAN, injector ids). At least one other peer is required.

**Fail-closed rules** (`config/config.go:1220-1496`), all errors:

- A peer needs `Address` or `Serve: true`.
- Secrets need TLS in every direction they are used. Pins need TLS.
- A Serve peer with `ClientAuth` set needs `TrustStorePath` or pins.
- A TLS dialer with no truststore, pin or secret needs `InsecureSkipVerify: true`.
- Without the waiver, the Serve direction needs listener TLS plus secrets, or a required client certificate (`REQUIRED`, or `REQUEST` with `RequireClientCert`) with trust; the Pull direction needs dialer TLS plus secrets, or trust without `InsecureSkipVerify`.

**Startup warnings:** waiver set (logged twice: config and manager); group secrets with more than two nodes; `Tls.TrustStorePath` equal to the TCPS truststore; `RetainedStoreType: MEMORY` with `Snapshot.Mode: OFF`; `MemoryLimitMB` below 2.2 × `Log.MaxBytes` + 150 MiB; no `Peers` entry matches this node (two or more peers). From the broker build: HostMonitoring base topic without `{NodeId}`, Redfish on every node, MQTT bridge host equal to a peer host, devices with node `local`/`*` under a shared config store (WINCCOA, PostgreSQL, MongoDB). From the manager: `InjectWorkers > 1`, pull-only status port not bindable, never-connected consumer.

**Example: pair with TLS and a shared secret** (`README.md:415-426`):

```yaml
NodeId: edge-a                      # edge-b on the other host; the rest is identical
PeerLink:
  Enabled: true
  Tls:
    Enabled: true
    AutoGenerate: true              # self-signed certs/peer-{NodeId}.pem and .key on first start
  SharedSecrets: ["<base64, at least 16 bytes: openssl rand -base64 32>"]   # same on both hosts
  Peers:
    - { NodeId: edge-a, Address: "edge-a.local:1890" }
    - { NodeId: edge-b, Address: "edge-b.local:1890" }
```

**Example: mTLS mesh** (`README.md:437-451`). Each certificate has the URI SAN `urn:monstermq:node:<NodeId>` and the serverAuth and clientAuth EKUs, issued by a dedicated peer CA:

```yaml
NodeId: edge-a
PeerLink:
  Enabled: true
  Tls:
    Enabled: true
    CertPath: certs/peer-{NodeId}.pem
    KeyPath: certs/peer-{NodeId}.key
    TrustStorePath: certs/peer-ca.pem   # dedicated peer CA; system roots are never used
    ClientAuth: REQUIRED
  Peers:
    - { NodeId: edge-a, Address: "edge-a.local:1890" }
    - { NodeId: edge-b, Address: "edge-b.local:1890" }
    - { NodeId: edge-c, Address: "edge-c.local:1890" }
```

Without a CA: `AutoGenerate: true` and `ClientAuth: REQUIRED` on every broker, and per peer `Tls: { PinnedSha256: ["<SPKI SHA-256>"] }` with the `spkiSha256` value each node logs at startup. Secrets and pins are lists: add the new value on both sides, move it first on both, remove the old one.

---

## 11. Security

### 11.1 TLS and trust (`tlsutil/`)

- Key pair: PEM certificate and PEM key, both paths required.
- Truststore: PEM, or PKCS12 holding one key plus certificates. An empty path gives an empty pool. **System roots are never used.**
- Listener: with pins on any peer, the TLS stack requests a certificate without chain verification; otherwise it verifies against the roots. A post-handshake check always runs `FindPeer` with the clientAuth EKU; with `REQUIRED`, a missing certificate fails.
- Dialer: the TLS library's hostname check is off; the real check runs on every handshake, resumed ones included: pin match, or chain against the roots, with the serverAuth EKU. With a secret and no pins, a certificate failure is tolerated (the MAC authenticates). SNI is sent but never used for verification.
- Verification never checks host names. A certificate without EKUs is valid for any usage.

### 11.2 Identity

- With `Peers[].Tls.CertificateIdentity`: an exact URI SAN or a case-insensitive DNS SAN.
- Otherwise: URI SAN `urn:monstermq:node:<NodeId>`, case-insensitive.
- `Tls.IdentityFallback` DNS or CN applies only when the certificate carries no `urn:monstermq:node:` URI at all, so a certificate naming another node never passes by its CN.

### 11.3 Pins and generated certificates

- Pins: 64 hex digits of the SPKI SHA-256 or the certificate SHA-256; lists allow rotation.
- `AutoGenerate` (`tlsutil/generate.go:22-124`): both files exist → used unchanged; neither → new ECDSA P-256 key (PKCS8, mode 0600) and self-signed certificate (0644); only the key → the certificate is recreated, so the SPKI pin stays; only the certificate → error. Certificate: CN = NodeId, O = MonsterMQ, URI SAN, serverAuth + clientAuth, 10 years, 128-bit serial. Atomic writes. The SPKI hash is logged at INFO.

### 11.4 Shared secret and admission

- The MAC (3.9) binds the secret to the TLS session through the exporter, which needs TLS 1.3; a secret over plaintext is refused at config time and at handshake time.
- Group secrets let any holder claim any NodeId of the group; prefer per-peer secrets with more than two nodes (WARN).
- Admission (4.3) caps unauthenticated connections per IP and globally; authenticated and configured IPs bypass the global cap.
- Refusal logs are rate-limited per peer and code; the claimed id is truncated to 64 characters and never used as a limiter key.

### 11.5 Status endpoint exposure

- `GET /peerlink/v1/status`; `POST /peerlink/v1/resync?source=<nodeId>` (202 `{"source","mode":"NEWER","status":"requested"}`; 400 for an unknown or missing source, 405 for a wrong method, 404 for an unknown path; errors are `{"error": "..."}`).
- Plaintext: loopback clients only; refused with 403 when an `Origin` header is present or `Host` is not loopback (DNS rebinding guard). 16 KiB request limit, 5 s deadline.
- TLS: status only (no resync), for a certificate-authenticated Serve peer; secret-only peers cannot read it.
- A pull-only node binds `127.0.0.1:<Port>` for these endpoints; a conflict is a WARN.
- `AllowedNetworks` applies here too, so it must include `127.0.0.1/32` for local access.
- The JSON contains no secrets.

### 11.6 Reserved ids

Client ids `inline` and `peerlink:*` are refused for network clients with CONNACK 0x85 (`refusedClientIds`). With `UserManagement.Enabled`, unauthenticated peers are impossible because replicas bypass ACLs.

---

## 12. Observability

### 12.1 Status JSON (`peerlink/status.go:130-261`)

| Object | Fields |
|---|---|
| top level | `enabled`, `nodeId`, `epoch`, `listen`, `tls` |
| `log` | `epoch`, `lso`, `leo`, `lwm`, `records`, `bytes`, `maxBytes`, `maxMessages`, `capacitySeconds` (null until rate > 1), `appended{client,inline,will}`, `trimmed`, `evictedUnread`, `evictedBy{count,bytes}`, `captureDropped{size,invalid}`, `skipPeer`, `skipWill`, `filtered`, `echoSuppressed`, `sharedSkipped`, `refusedClientIds`, `usernameStripped`, `spareMisses`, `uncapturedAtShutdown`, `sealed`, `active` |
| `admission` | `accepted`, `refusedNetwork`, `refusedBusy`, `refusedSniff`, `refusedPlaintext`, `refusedHttp`, `tlsFailures`, `authFailures{code}`, `preAuth` |
| `consumers[]` | `nodeId`, `state`, `remote`, `committed`, `served`, `lag`, `lostTotal`, `servedRecords`, `servedBytes`, `servedSkipped{size}`, `snapshotServed`, `sessions`, `duplicateConsumer`, `authFailures`, `lastFetch`, `shutdownUnserved`, `oaRetained`, `topicRootMismatch`, `retainedClassMismatch` |
| `sources[]` | `nodeId`, `address`, `state`, `epoch`, `appliedNext`, `sourceLeo`, `lagRecords`, `batches`, `injected`, `retainOnly`, `appliedBytes`, `dupSkipped`, `dropped{8 reasons}`, `retainedDiverged{size,size_source,expired}`, `rejected`, `unknownProps`, `gapLostTotal`, `sourceResets`, `resetLostLowerBound`, `reconnects`, `sessions`, `crcErrors`, `snapshotFilled`, `snapshotSkippedPresent`, `snapshotNewer`, `snapshotTruncated`, `snapshots`, `snapshotsInterrupted`, `retainedFlushErrors`, `supersededWillResent`, `paced`, `lastError`, `clockSkewMs`, `rttMs`, `topicRootMismatch`, `retainedClassMismatch`, `oaRetained`, `applyDelayMs{p50,p99,p99_9}` (bucket upper bounds, -1 without samples) |

### 12.2 Native status object

With WinCC OA native mode, the retained broker status under the namespace root carries `peerLink{enabled, consumers[nodeId, state, lag, lostTotal], sources[nodeId, state, lagRecords, gapLostTotal, sourceResets, retainedDiverged (sum), lastError]}` (`peerlink/status.go:264-303`). It lies under the namespace, so it is never forwarded. Republished every 5 s and on every link state change (puller to STREAMING, BACKOFF or STOPPED; consumer connect or disconnect).

### 12.3 Metrics

GraphQL `BrokerMetrics.messageBusIn` / `messageBusOut` are filled from PeerLink counters (no SDL change); semantics in 6.6.

### 12.4 Log events

| Level | Events |
|---|---|
| INFO | listening; consumer connected / disconnected; consumer sent GOAWAY; streaming from source; source closed the link; snapshot applied; oaRetained decision; consumer drained |
| WARN (rate-limited, 10 s per key unless noted) | link down; gap; lost before resume; source restarted; snapshot interrupted / truncated / scan failed; flush failed; clock skew (10 min); malformed record; replica rejected; retained diverged; never connected (once); TopicRoot or retained class mismatch (once); shutdownUnserved; uncaptured; InjectWorkers reserved; AllowUnauthenticatedPeers; pull-only status port |
| ERROR | handshake refused (10 s per peer and code); link refused, config error (5 min per code); duplicate NodeId; poison batch; batch-structural fault; MaxFrameBytes too small (once) |

---

## 13. MQTT core changes required

These are the engine capabilities PeerLink needs, stated so they map to any broker core.

| # | Capability | Why | Go |
|---|---|---|---|
| E1 | An internal publisher may preset the publisher identity (`Origin`) and the creation time (`Created`, if `0 < Created ≤ now`) of an injected publish; the engine keeps them | NoLocal per original client; expiry and retained age computed from the capture instant | `stampPublish`, `mqtt/server.go:1093-1108` |
| E2 | A replica marker on the in-flight publish, visible to every hook (publish, retained, subscriber selection, published), never encoded, not copied into retained or inflight copies | Split horizon; per-replica gates in storage, queue, bus, archive and bridge; shared-group skip; original publisher and time for archives | `Packet.Forward`, `mqtt/packets/packets.go:131,149-159` |
| E6 | A will marker on the publish produced by the will logic | A retained will passes the retained hook and must be captured once, as a will | `Packet.Will`, `mqtt/server.go:1796` |
| E4 | "Retain only": validate, stamp, update the retained store and fire the retained hooks, without delivery, without published hooks and without counters | Stale retained replicas and expired retained deletes | `RetainOnly`, `mqtt/server.go:1113-1147` |
| E5 | Close the client listeners and disconnect clients (reason 0x8B), running their wills synchronously, while hooks, the event loop and internal publishing stay alive | The drain needs "no more client publishes" while capture and serving continue | `CloseListeners`, `mqtt/server.go:1756-1767` |
| E5b | Refuse new connections once shutdown has begun | No publisher can appear after E5 | `Listeners.AddClient`, `ErrListenersClosed` |
| — | Optional per-topic serialisation of the retained-store update together with the retained hooks (`SerializeRetained`), skippable for stores whose writes block on external answers | Log order equals retained apply order per topic | `mqtt/server.go:1167-1196` |
| — | Optional skip of offline (queued) delivery for replicas (`QueueOfflineReplicas`) | A replica must not be backlogged for a session that may have moved to the peer | `mqtt/server.go:1279-1287` |
| — | Synchronous post-accept taps on the publisher thread for retained, published and will, with retained capture inside the retained apply, before ack and fan-out | Per-publisher FIFO; no window between retained update and capture | |
| — | Connect-time refusal before auth; session-established hook; subscriber-selection hook | Reserved ids; will supersession; shared-group skip | |
| — | An inline injector client: not in the client registry, protocol v5, bypasses ACL, topic validation, size limit and receive quota; one per source | The receiver validates everything itself; v5 is needed for expiry | `peerlink/puller.go:128-129` |

E3 (delayed-will stub) and E7 (private counters) from the plan are not implemented.

Broker-level changes: StorageHook replica branch (bus-in counter, gates, derived UUID, pending retained map with `FlushReplicas`, local-write supersede, `ApplyCached`); QueueHook (replica skip, origin removal, own-node hydration); bus adapter `OriginNode` filter; `messageBusIn/Out` metrics; a retained accessor with `Snapshot`, `Has`, `Created`, `Get`, `FlushReplicas`.

---

## 14. WinCC OA redundancy features (edge only)

These features exist only in the broker embedded in WinCC OA (`WCCOAmmq`). They are described so that a JVM variant (MonsterOA / Oa4jBridge) can follow.

### 14.1 `connectToRedundantHosts`

- On a redundant system the passive Event Manager receives but does not execute writes of a manager connected only to it: store writes time out and native writes are lost.
- Fix: `[monstermq] connectToRedundantHosts = 1` or the option `-connectToRedundantHosts`, and distinct manager numbers on the two hosts (`-num 1`, `-num 2`). The passive host's broker then writes through the active Event Manager (`winccoa/README.md:328-378`).
- This is a standard WinCC OA option; no broker code handles it. The only accommodation is that repeated redundancy hotlinks are ignored (`winccoanative/redu.go:84-108`).

### 14.2 Broker role in the status

The C++ manager extends `OpSysInfo` (op 2) with TLV tags `TagRedundant` = 15 (`Resources::isRedundant()`), `TagReplica` = 16, `TagHost` = 17 twice (Event Manager hosts 1 and 2) and `TagLocalHost` = 18 (`gethostname()`) (`winccoa/manager/MonsterMQManager.cxx:359-373`, `oahost/tlv.go:31-35`). Older hosts omit them.

**Own host** (`ownReduHost`, `winccoanative/redu.go:27-43`): the first DNS label of `LocalHost`, lowercased, compared with the first label of Event Manager hosts 1 and 2; fallback: the replica number if 1 or 2, else 0.

**Active flags:** `dpConnect` with answer on `<sys>:_ReduManager.Status.Active` (host 1) and `<sys>:_ReduManager_2.Status.Active` (host 2). Every change republishes the status at once (switchover tracking).

| Field | Present | Value |
|---|---|---|
| `role` | always | `STANDALONE` (not redundant), `UNKNOWN` (own host or its flag unknown), `ACTIVE`, `PASSIVE` |
| `redundant` | always | bool |
| `host` | redundant | own host, 1 or 2 (0 = not determined) |
| `hostName` | redundant | Event Manager host name of the own host |
| `activeHost` | redundant | 1 or 2 when exactly one flag is true; 0 when unknown or both active (split mode) |

The status is the retained JSON on `<Root>/<Systems>/<sys>` and `<Root>`, QoS 1; it includes the `peerLink` object when PeerLink is on, and is republished every 5 s and on PeerLink state changes. There is no GraphQL, REST or `$SYS` exposure.

### 14.3 Namespace exclusion predicate

`peerLinkNamespaceRoot(nativeActive, root)` returns the TopicRoot only while native mode (`Namespace`) is active, else nothing (`broker/peerlink.go:25-34`). It is a function so the decision can later depend on the host role. It feeds capture, snapshot, the receiver filter and the root announced in HELLO/HELLO_OK. The receiver also drops topics under the root the source announces. Reason: WinCC OA itself replicates `<TopicRoot>`.

### 14.4 `oaSystem` and `oaRetained`

- `oaSystem` is the local WinCC OA system name, sent in HELLO and HELLO_OK only when embedded with native mode.
- `oaRetained` (3.9): no snapshot on that link; retained replicas from that source only update the cache (`ApplyCached`); shown per link in the status.
- Retained capture is not serialised for the WINCCOA store class.
- Deployment rule: two independent WinCC OA projects linked by PeerLink need different system names, or they are taken for one system and forwarded retained values are not stored.

### 14.5 Topics branch (summary)

- Datapoint type `MMQTopic` with elements `topic` (TEXT), `value` (BLOB, last-value storage off), `retained` (BLOB); created if missing, never changed.
- Datapoints are created lazily on the first publish. An empty retained publish clears `retained` and deletes the datapoint. The broker consumes the publish; WinCC OA delivers it back.
- `TopicDpNames`: HASH (default) = `MMQTopic_k` + first 24 hex chars of `SHA-256("topic\0" + topic)`; NAME = `MMQTopic_` + escaped topic (`%XX`), falling back to HASH above 128 characters. All brokers of a distributed system must use the same setting.
- Wildcard subscriptions: one `dpQueryConnectSingle` per system, `SELECT '<attr>' FROM 'MMQTopic_*.topic' [REMOTE '<sys>'] WHERE _DPT = "MMQTopic"` (WinCC OA requires REMOTE directly after FROM; fixed in 192ad05), rows fed into a per-level topic tree (`#` also matches the parent level), then one `dpConnect` per match. Rows not matching this broker's naming scheme are skipped.
- Collision guard: a write to a datapoint whose `topic` element holds a different topic fails with "topic name invalid" before any write or delete; on the subscribe side such a datapoint is marked foreign, warned about once and never delivered.

### 14.6 Open verification items on a real redundant pair

- Do answers to `dpSetWait`, `dpCreate`, `dpDelete` arrive once or once per Event Manager with `connectToRedundantHosts`?
- Are `dpConnect` / `dpQueryConnect` registrations restored after a switchover?
- Do hotlinks arrive twice, once per connection?
- The WinCC OA specific PeerLink parts (`oaRetained`/`ApplyCached`, the native `peerLink` status object, the WINCCOA-store device WARN) are implemented but untested against a live project.

[plan-winccoa-node-redundancy-status.md](plan-winccoa-node-redundancy-status.md) is partly implemented: the per-host `node/1`, `node/2` topics and `eventConnections` do not exist.

---

## 15. Related edge changes

- **Per-client session message rates** (`metrics/collector.go:33,72-115`, `broker/hook_storage.go:249-252,322-341`): per-client in/out counters, converted at each collection tick to `count / interval` (tumbling window of `Metrics.CollectionIntervalSeconds`, default 1 s). In counts publishes from network clients (not internal clients, not replicas); out counts PUBLISH packets sent. Exists only with `Metrics.Enabled`, not persisted, forgotten on disconnect. Exposed only as GraphQL `Session.metrics { messagesIn, messagesOut }`; `metricsHistory` is empty.
- **Version in the embedded build:** `make embed-lib` now passes `-ldflags "-X monstermq.io/edge/internal/version.Version=$(VERSION)"` (from `version.txt`), so `libmonstermq.a` reports the release version (`Makefile:81-83`).
- **`Runtime.MemoryLimitMB`:** a top-level setting for the Go soft memory limit, logged at INFO at startup.

---

## 16. Tests and coverage

**Integration tests** (`test/integration/peerlink_*_test.go`, in-process nodes on ports 27300-27399): forwarding (QoS, retained set/delete, MQTT 5 fidelity, bidirectional no-echo, echo suppression, mark replicas, three-node mesh and chain), lifecycle (consumer restart, connection drops through a proxy, source restart, overflow, snapshot FILL, drain, wills and supersession, shutdown wills, device WARNs), receive (shared subscriptions SKIP/DELIVER, offline queue and NoLocal, inline origin and namespace/HMI exclusion, expiry, stale records, size tombstone, catch-up pacing), review fixes (retained capture order, will after failover, overflow while disconnected, bounded bridge loop, TLS migration, string retention, soak behind `PEERLINK_SOAK`, slow link behind `PEERLINK_NETEM`), security (shared secret rotation, mTLS identity, fail-closed, AllowedNetworks, raw GOAWAY codes, admission, status and resync, canonical NodeIds), and `BenchmarkInjectPublish`.

**Unit tests:** `peerlink` (auth, handshake, 20 link tests, receiver, 14 regressions, log incl. `-race`), `peerlink/wire` (3 fuzzers, frame, record, MAC, codec), `tlsutil` (31 tests: empty pool without system roots, missing EKU, pins, exporter, resumed session, ALPN, identity), `config/peerlink_test.go` (defaults, strict decode, about 90 validation cases), engine tests for E1-E6, queue and storage hook tests, config schema test.

**Coverage against the plan's PL list** (`plan-peerlink.md` §22.2): 22 covered, 6 mostly covered, 14 partial, 2 missing.

| Status | Ids and gaps |
|---|---|
| Mostly | 03 (archive rows not checked), 04 (1 s idle, not 10 s), 10 (no end-to-end FILL after restart), 16 (no TLS-terminating relay), 35 (scaled down), 41 (both-directions-down interval only implied) |
| Partial | 05 (no ring), 07 (no REST/script/inbound-bridge forwarding; shared-store WARN untested), 08 (no crash variant), 13 (retained expiry on B; WINCCOA variant), 18 (no crash-restart, no commit-regression check), 21 (non-loopback plaintext refusal), 23 (no broker-level snapshot race under load), 25 (no RSS or loss accounting), 27 (codec only, no v1.0↔v1.1 peer), 28 (QoS 1 publishers during close, 0x8B, scripts during close), 31 (`BridgeOutbound: true`, inbound and remap cases), 33 (no persistent-session failover test), 34 (criterion differs), 36 (64 KiB payloads, completion only), 38 (`retainedDiverged{expired}` not asserted), 44 (decision logic only, no simulated WinCC OA host) |
| Missing | PL-24 (load matrix: skip stub), PL-37 (write-path counting-connection test) |

**Deferred:**

- M6 items: segmented arena log, conditional silent retained clear, E3, E7, API username in `Forward`, Redfish NodeId assignment (Q28), write forwarding to the active WinCC OA host (Q29), certificate hot reload, adaptive `Fetch.MaxBytes`, subscriber-fill backpressure.
- `Receive.InjectWorkers > 1` is reserved (WARN only).
- Performance gates G0-G7 are not measured on reference hardware (Pi 4 arm64/armv7, x86); `dev/bench/peerlink/` and the load harness do not exist. `Log.MaxBytes` 256 MiB stays provisional until `T_restart` is measured.

---

## 17. JVM port plan

Target: the Kotlin MonsterMQ broker (`/media/psf/Workspace/monster/main`). This section is based on a code map of that broker; line numbers refer to its current tree.

### 17.1 Integration points

| Area | Recommendation |
|---|---|
| Capture | At the top of `SessionHandler.publishMessage(message, forwardToExternalBus)` (`K/handlers/SessionHandler.kt:1539`), only when `forwardToExternalBus && message.peer == null`. Every client publish, will and internal publish (GraphQL, REST, MCP, bridges, OA, flows, agents, `publishInternal`) passes here once, after ACL and schema checks; QoS 1 before PUBACK, QoS 2 at PUBREL. Remote-cluster and external-bus inbound paths are excluded by construction. Wills are recognised by a new `isWill` and not captured while draining. Filters: `$` topics, `!OA/#` while Oa4jBridge is active, Include/Exclude, default exclude of the HMI sync tree (`monstermq/hmi/sync/#`). |
| Message model | Extend `BrokerMessage` (`K/data/BrokerMessage.kt:15-35`) with `peer: PeerForward?` (`sourceNode, epoch, offset, username, timeNs, will, snapshot`), `isWill` (set in the `MqttWill` constructor, `:155-165`) and optional `username`. Carry them through every clone (`:179-203`) and `publishInternal` (`SessionHandler.kt:1974-1994`), preferably via one copy helper. A dropped marker re-captures the message: an echo loop. |
| Codec | Add a versioned trailing section to `BrokerMessageCodec` (`K/data/BrokerMessageCodec.kt:9-97`) with `senderId`, MQTT 5 properties and the replica marker. Needed whenever a replica crosses the Vert.x event bus; it also fixes the existing loss of properties and `senderId`. |
| Injection | A `PeerLinkExtension` verticle; one ordered context per source; inject with `sessionHandler.publishMessage(replica)`: `clientId = senderId =` original publisher (NoLocal per logical client), `time =` backdated capture instant (`now − ageMs`, see 6.3), `messageId = 0`, `isDup = false`, `messageUuid` derived from `(source, epoch, offset)`. |
| Receiver gates (no hook system; add `msg.peer != null` checks) | Archives and last-value in `MessageHandler.saveMessage` (`:333-361`) → `Receive.Archive`; retained always applies. GraphQL listeners `notifyMessageListeners` (`SessionHandler.kt:2011`) → `Receive.Bus`. MQTT bridge outbound (`MqttClientConnector.kt:571-575`) → `Receive.BridgeOutbound`. Offline/created queue branches (`SessionHandler.kt:1868-1915`, `2093-2108`) → `Receive.Queue`. **Do not gate the queue-first path of online persistent sessions** (`:1795-1830`, `:2081-2086`), or they receive nothing. External-bus forward (`:1569`) must skip replicas. Will supersession can use `sessionHandler.isConnected(clientId)` (`:709`). Shared subscriptions do not exist in the Kotlin broker; nothing to gate. |
| Metrics | Increment `messageBusIn/Out` (`SessionHandler.kt:59-64`). GraphQL `BrokerMetrics.messageBusIn/Out` and Prometheus `bus_in/bus_out` already read them: no SDL change. The injector is not a client, so `messagesIn` stays unchanged, as on the edge. |
| Server and dialer | Vert.x `NetServer` / `NetClient`. Sniff the first byte, then `NetSocket.upgradeToSsl(SSLOptions, Buffer)` (Vert.x 5.1.1) replaying the bytes read. `MqttServer.buildKeyCertOptions` / `buildTrustOptions` (`K/MqttServer.kt:118-131`) for key and trust material. `sslSession().peerCertificates` and `applicationLayerProtocol()` for identity and ALPN. A CIDR matcher has to be written. |
| NodeId | New `PeerLink.NodeId`, defaulting to `NodeName`, canonicalised. Never `Monster.getClusterNodeId()`, which returns `"local"` standalone (`K/Monster.kt:189-196`). |
| Config | A `PeerLink` block read in `Monster.startMonster`; `Features.PeerLink` (`Features.kt`), schema block modelled on Zenoh (`yaml-json-schema.json:103-183`, `additionalProperties: false`), deploy gate and GraphQL gates. The schema is not validated at runtime: validate explicitly in code and fail fast (`exitProcess(1)`), including the fail-closed rules of section 10. |
| Coexistence | v1: refuse PeerLink together with `-cluster`, the Kafka bus or Zenoh (as `Monster.kt:1796-1799` does for Kafka/Zenoh). Otherwise replicas leak into the other federation (per-JVM logs, transitive forwarding, loops). |
| Shutdown | The JVM broker has no shutdown orchestration. Add a `Runtime.addShutdownHook` that blocks on a Vert.x future chain: stop pullers (final COMMIT); set draining; undeploy MQTT/NATS/WS servers; disconnect all clients (`sessionHandler.disconnectClient`, which publishes wills, hence draining first); stop internal publishers (devices, flows, scripts, agents, GraphQL/REST/MCP, HMI sync, Redfish, Oa4jBridge); drain to `drainTarget`; seal, log unserved counts, GOAWAY, close. MonsterOA / JManager exit needs the same hook. |
| WinCC OA | MonsterOA has no native namespace and no role awareness. Exclude `!OA/#` from capture, snapshot and the receiver filter while Oa4jBridge is active (each node derives those values from its own OA system). Always send `oaSystem = ""` and `topicRoot = ""` (17.4), and never run `oaRetained`. |

### 17.2 Milestones

| M | Scope | Exit |
|---|---|---|
| J0 Decisions | Wire compatibility with Go `mmq-peer/1` (recommended: yes); coexistence rule (exclusive in v1); NodeId source; retained-path policy for replicas; TLS exporter availability on the target JVM | Owner sign-off recorded |
| J1 Codec and log | `wire` codec in Kotlin (frames, records, TLVs, tombstones, CRC32C, MAC); in-memory chunked log with offsets, epochs, commits, eviction and loss accounting; golden-vector tests generated by the Go codec | Byte-identical encode/decode against Go vectors; fuzz-style round trips |
| J2 One-way link | Server with sniffing and handshake, puller, injector, capture at the tap point, `BrokerMessage` extensions and codec trailer, receiver gates, config block and validation (plain TCP) | Forward QoS 0/1/2, retained set/delete, MQTT 5 properties; no echo |
| J3 Failure semantics | Resume and dedup, GAP and overflow, takeover and duplicate detection, keepalive and deadlines, backoff, wills with supersession, snapshot FILL and NEWER resync, pacing, the shutdown hook with drain | Restart, drop, overflow and drain scenarios with exact counters |
| J4 Security | TLS listener and dialer, URI SAN identity, pins, PEM auto-generation, shared secret MAC over TLS 1.3, admission, fail-closed validation | mTLS and secret cases; wrong secret gives `auth_failed`; fail-closed startup |
| J5 Observability and docs | Status endpoint and resync, metrics, log events, README | Status JSON field names identical to 12.1 |
| J6 Cross-implementation tests | Go edge broker against Kotlin broker in both directions, plus a scripted Go test peer for malformed and older-version cases | Bidirectional forwarding, resume, snapshot and auth interoperate; PL ids mapped |

### 17.3 Risks

| ID | Risk | Mitigation |
|---|---|---|
| R1 | Retained writes are asynchronous through a 100k `ArrayBlockingQueue` that drops silently on overflow (`MessageHandler.kt:29, 326-330`); catch-up can lose retained values uncounted, and FILL "absent" checks race the writer | Pacing, a drop counter, or a synchronous retained path for replicas |
| R2 | Echo loops when a clone or constructor drops the replica marker (`publishInternal`, `cloneWith*`, Zenoh envelope, codec) | One copy helper; tests that every path keeps `peer` |
| R3 | Queue-first delivery to online persistent sessions; a naive replica gate starves them | Gate only offline/created branches |
| R4 | `userProperties: Map` loses order and duplicate keys; wills carry no MQTT 5 properties | Document, or change to `List<Pair>` |
| R5 | `"local"` standalone node ids make per-node device assignment impossible (`DeviceConfig.kt:79-80`); duplicate device output with a shared config store | Node-matching change using `PeerLink.NodeId` |
| R6 | No shutdown sequence exists; server-initiated disconnects publish wills | New shutdown hook; draining flag before disconnects |
| R7 | Coexistence with Hazelcast cluster, Kafka bus, Zenoh | Mutually exclusive in v1 |
| R8 | Expired retained entries remain in stores (`isExpired` only at delivery) | Snapshot must skip expired values |
| R9 | One broker per JVM and no process-spawning harness | New pytest/process fixture; use the Go edge broker as counterpart |
| R10 | Wire compatibility undecided; the Go plan listed Kotlin interop as a non-goal | Decide in J0; cheap now, enables J6 |

### 17.4 Open questions with recommendations

| Question | Recommendation |
|---|---|
| Stay wire-compatible with the Go edge broker? | Yes. Implement section 3 byte-exactly; it costs little now and gives cross-implementation tests and mixed edge/JVM deployments. |
| `userProperties` as `Map` | Accept as a documented fidelity limit in v1 (order and duplicates lost on JVM receivers); plan a `List<Pair>` change separately. Encoding on the JVM side still emits one TLV per pair. |
| Node assignment for standalone nodes | Make `DeviceConfig.isAssignedToNode` accept `PeerLink.NodeId` and document "assign each device to one node"; until then warn when PeerLink is on with a shared config store. |
| Retained async queue drop | For replicas, either write retained synchronously on the injector context or count drops and pace on queue depth; never drop uncounted. |
| TLS exporter on the JVM | The shared-secret mode needs the RFC 8446 exporter with label `monstermq-peer/1`, empty context, 32 bytes. Verify that the chosen TLS provider (JSSE version, or BouncyCastle JSSE) exposes it; if not, ship mTLS and pins first and add secrets later. |
| Announced `topicRoot` for MonsterOA | Send empty in v1 and filter `!OA/#` locally on capture, snapshot and receive. Announcing `!OA` gains nothing (the Kotlin source never captures `!OA/#`) and makes a Go peer in native mode log a TopicRoot-mismatch WARN. |

---

## 18. Deviations from plan-peerlink.md

Each item names the plan section it differs from.

**Architecture and wiring (plan 6)**

1. `RetainedAccess` also has `Created`, `Get` and a context on `Snapshot`; `New` takes a `Deps` struct (`peerlink/manager.go:28-86`).
2. The peer listener and the injector clients are created inside `peerlink.New`, not as separate build steps (`peerlink/manager.go:296-322`).
3. The manager is built and registered before StorageHook and QueueHook (`broker/server.go:362-419`); the plan said both "after the queue hook" and "before StorageHook/QueueHook".
4. The hook is registered whenever PeerLink is enabled and is inactive without a log (`peerlink/hook.go:165-171`).
5. `New` also sets `SerializeRetained` and `QueueOfflineReplicas` (`peerlink/manager.go:288-291`).

**Capture (plan 7)**

6. Inline capture validates all strings (`ValidContent`), not only the topic (`peerlink/hook.go:202-205`).
7. An invalid username is stripped and counted `usernameStripped` (`peerlink/hook.go:190-197`).
8. `OnWillSent` tests for a replica first (`peerlink/hook.go:123-132`).

**Log (plan 8)**

9. Byte accounting uses the allocation size class per record plus about 16 KiB per chunk, not "+16 B per slot" (`peerlink/log.go:420,1055-1063`).
10. The never-connected WARN fires once per consumer (`peerlink/manager.go:475-484`).
11. Fetch limits not in the plan: `maxWait ≤ 60 s`, `maxRecords` default 4096 and cap 65536, `maxBytes` default 1 MiB, 16 chunks per read, consumer `MaxWaitMs ≥ 10` (`peerlink/server.go:928-941`, `peerlink/log.go:626-629`, `config/config.go:1458`).
12. Eviction never removes the record just appended (`peerlink/log.go:447-472`).

**Wire protocol (plan 9-10)**

13. Before authentication the source accepts only HELLO (or GOAWAY); unknown types get `GOAWAY(protocol)`, not "ignored" (`peerlink/server.go:584-591`).
14. NodeId canonicalisation (lowercase, `[a-z0-9._-]{1,64}`) applies to every comparison and to the MAC inputs; an invalid consumer id is `unknown_peer` (`peerlink/server.go:476-498`, `config/config.go:1152-1164`).
15. Extra `auth_failed` step: peer has secrets and HELLO flags b0 is clear (`peerlink/server.go:518-520`).
16. The consumer tolerates a server certificate failure when it has secrets, skips the check with `InsecureSkipVerify`, and its GOAWAYs carry no reason (`peerlink/puller.go:514-532,586-590`).
17. TOMBSTONE agreement is not enforced; tombstones are substituted regardless (`peerlink/server.go:1011-1023`).
18. RESYNC_NEWER has no source-side meaning; FILL and NEWER are both a plain FETCH(SNAPSHOT). The source serves a snapshot when asked even under `oaRetained`; only `SNAPSHOT_AVAILABLE` is withheld (`peerlink/server.go:652,676`, `peerlink/snapshot.go:90-101`).
19. `SNAPSHOT_AVAILABLE` additionally requires FILL agreed, not `oaRetained`, and a retained store (`peerlink/server.go:676-678`).
20. `resumeOffset = 0` with a matching epoch is treated as offset 1, with `lost = LSO − 1` when `LSO > 1`; it does not fall back to `C[c]` (`peerlink/log.go:863-886`).
21. Extra record check: `captureMonoMs > BATCH.sourceMonoMs` is malformed (`peerlink/wire/record.go:637-639`).
22. Tombstones keep the original flags, timestamps, expiry and payload format; substitution compares the full frame length with `HELLO.maxRecordBytes` (`peerlink/wire/record.go:315-328`, `peerlink/server.go:1017`).
23. Property TLVs are written in a fixed order without empty values; only user properties keep their relative order; the last repeated singleton wins on decode (`peerlink/wire/record.go:250-262,519-549`).
24. `expirySec`: wills always 0; snapshot records carry `remaining + ageSec`; snapshot `publishWallNs` has second resolution (`peerlink/hook.go:198-201`, `peerlink/snapshot.go:52-85`).
25. Long poll only when `offset ≥ LSO` (a GAP fetch never waits); linger semantics as in 3.11 (`peerlink/server.go:955-976`).
26. EMPTY batches carry `baseOffset = max(offset, LSO)` and may combine with GAP; snapshot batches use `baseOffset = 0` (`peerlink/server.go:977-993`, `peerlink/snapshot.go:126-134`).
27. FETCH carries the commit only when it grew since the last one sent, else 0 (`peerlink/puller.go:945-948`).
28. The source write deadline is set once per batch as `keepAlive × (1 + bytes/64 KiB)`, not a progress deadline per 64 KiB (`peerlink/server.go:1046`).
29. Unplanned fault rules: `frameLen == 0` is a protocol fault; bytes after `recordsBytes` are ignored and not CRC-covered; CRC poison handling is off for snapshot batches (`peerlink/wire/frame.go:354-356,832-853`, `peerlink/puller.go:713`).
30. HELLO and HELLO_OK fields cannot be truncated; a peer omitting `oaSystem` is rejected (`peerlink/wire/frame.go:593-610,656-675`).
31. `SERVER_HELLO.authModes` b1 is set when any Serve peer has secrets, also on plaintext; b0 only on TLS with `ClientAuth` not NONE (`peerlink/server.go:564-572`).

**State machines (plan 11)**

32. The consumer becomes CONNECTED and supersedes the old session at HELLO time (admit before HELLO_OK), not at first STREAMING, so the drain also waits for consumers in the snapshot phase (`peerlink/server.go:620-702`).
33. With `Pipeline: 1` the reader still prefetches after handoff (handoff capacity 1); the duplicate window is unchanged (`peerlink/puller.go:627-696`).
34. During the snapshot, PING is sent unconditionally by a separate goroutine, not only while no FETCH is outstanding (`peerlink/puller.go:742-758`).

**Receiver (plan 12)**

35. Check order: dedup before the tombstone check, the FILL presence check before the age checks, NEWER after will supersession (`peerlink/inject.go:255-356`).
36. Snapshot records are never stale (`peerlink/inject.go:326`).
37. `MaxApplyRate` caps the apply rate always, not only during catch-up (`peerlink/inject.go:106-175`).
38. `Receive.InjectWorkers > 1` is not implemented; it only logs a WARN (`peerlink/manager.go:292-295`).
39. QueueHook, StorageHook bus and archives also skip snapshot replicas, not only wills (`broker/hook_queue.go:107`, `broker/hook_storage.go:331-396`).
40. The mid-batch commit is checked only every 64 records and is a non-blocking send; the constant `commitEvery` (`peerlink/puller.go:40`) is unused and 100 ms is hard-coded (`peerlink/inject.go:270-273`).

**Engine (plan 13)**

41. E3 (delayed-will stub) and E7 (private `Info`) are not implemented; `$SYS` counters include replicas (`mqtt/server.go:961`).
42. Engine items beyond E1-E7: `SerializeRetained`, `QueueOfflineReplicas`, `Listeners.AddClient`/`ErrListenersClosed` (`mqtt/server.go:1167-1196,1279-1287`, `mqtt/listeners/listeners.go:131-157`).
43. A local retained write also waits for in-flight replica flushes of the topic and blocks their requeue (`broker/hook_storage.go:416-418,617-636`).

**Delivery (plan 15)**

44. K5 (retained capture window) is closed by `SerializeRetained`, except for the WINCCOA retained class (`mqtt/server.go:1167-1171`, `peerlink/manager.go:285-290`).

**Retained (plan 16)**

45. Snapshot records carry no username, so filled DB rows have an empty username (`peerlink/snapshot.go:52-85`).
46. The snapshot is materialised lazily at the first FETCH(SNAPSHOT) of a session; without an agreed capability an empty snapshot with END is served (`peerlink/snapshot.go:90-101`).
47. Retained DB writes from a snapshot are flushed only at SNAPSHOT_END (`peerlink/puller.go:833-841`).

**Observability (plan 20)**

48. The status JSON has extra fields: `lwm`, `maxBytes`, `maxMessages`, `sharedSkipped`, `refusedClientIds`, `usernameStripped`, `sealed`, `active`, `admission`, `snapshotServed`, `snapshotNewer`, `snapshots`, `snapshotsInterrupted`, `retainedFlushErrors`, `supersededWillResent`, `rttMs`, `lagRecords` (`peerlink/status.go:130-261`).
49. The native status is republished every 5 s plus on every state change, without a rate limit (`broker/peerlink.go:250-273`).
50. Loopback endpoints also require a loopback `Host` header and no `Origin` header (`peerlink/status.go:333-356`).
51. Clock-skew samples are taken only when `trecv − tsend ≤ 2·RTT + 20 ms` (`peerlink/puller.go:627-696`).
