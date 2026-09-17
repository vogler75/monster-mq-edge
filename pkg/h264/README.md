# H.264 decoder

A native Go decoder and RFC 6184 depacketizer implemented in this repository
from the specifications. The package imports only the Go standard library.
It does not use an existing codec library, CGO, a subprocess, native shared
libraries, WebAssembly, or downloaded runtime assets.

The implementation decodes progressive, 8-bit YCbCr 4:2:0 I/P/B pictures.
Validation and performance work are ongoing. Supported coding tools include:

- CAVLC or CABAC entropy coding (all three `cabac_init_idc` settings).
- 4x4 and 8x8 integer transforms; intra 4x4, 8x8 and 16x16 prediction.
- P-skip, 16x16/16x8/8x16 partitions, and 8x8/8x4/4x8/4x4 subpartitions.
- B-skip, list 0/list 1 and bidirectional partitions, spatial and temporal
  direct prediction, implicit and explicit B weighting, and B-pyramid references.
- Quarter-pixel luma and eighth-pixel chroma motion compensation.
- Multiple reference pictures, list modifications, explicit P weighting and
  short-/long-term reference marking. Not all combinations have reference
  fixture coverage yet.
- SPS/PPS scaling matrices, default matrices and inheritance rules.
- All three picture-order methods, bounded display reordering and end-of-stream
  flushing, including IDR and MMCO5 resets.
- In-loop deblocking, multiple consecutive slices and frame cropping.
- Parameter sets delivered in-band or separately from an RTSP SDP.

Explicit limitations include transform bypass, field/MBAFF coding, slice groups/ASO, data partitioning, redundant pictures,
10-bit/4:2:2/4:4:4, and scalable/multiview extensions. Unsupported coding tools
return errors; the decoder does not substitute a placeholder image.
An SPS may permit frame-number gaps, but a stream that actually requires
non-existing reference pictures still returns `ErrUnsupported`.

## Library use

```go
import "monstermq.io/edge/pkg/h264"

dec := h264.NewDecoder(h264.Config{})

// Optional out-of-band parameter sets from SDP, without NAL start codes.
_, err := dec.Decode([][]byte{sps, pps})
if err != nil {
    return err
}

// Access units must be complete and in decoding order. Feed every picture,
// including pictures you do not intend to publish, to maintain references.
frames, err := dec.Decode(accessUnitNALs)
if err != nil {
    return err
}
for _, frame := range frames {
    // Pixels holds cropped planar YCbCr samples, in the source colour range.
    // RGB / WriteJPEG apply range and matrix conversion.
    if err := frame.WriteJPEG(output, 85); err != nil {
        return err
    }
}
```

A decoder belongs to one stream. Decoder and depacketizer instances require
serial access. Returned picture storage remains valid across subsequent calls;
do not modify it, since it can also be a prediction reference.

`SplitAnnexB` separates NAL units in a complete Annex B byte stream. It is not an
access-unit parser: callers must still group slices into complete pictures.
`Decode` returns frames in display order, which can differ from input order.
A call can return zero or several frames. `Frame.PictureOrderCount` is the
stream's POC within its current IDR/MMCO5 interval, not a wall-clock timestamp.
At end of stream, process the remaining pictures returned by `dec.Flush()`.
Do not flush between access units. A parameter-set-only call returns no frames.

For RTP, pass `SequenceNumber`, `Timestamp`, `Marker` and `Payload` to
`Depacketizer.Push`. A nil result with nil error means more packets are needed.
The depacketizer supports single-NAL, STAP-A and FU-A packets, owns its output,
and rejects interleaved packetization. Apply transport jitter buffering before
calling it. Reset the depacketizer on SSRC changes.

On packet loss, call `Decoder.Discontinuity()` to discard references and delayed
pictures while retaining parameter sets. The decoder waits for an IDR picture before resuming.
Decode errors also invalidate reference pictures. `Reset()` additionally clears
parameter sets; supply the SDP sets again after resetting.

Zero configuration selects limits of 4096×2160 coded pixels, 16 MiB per access
unit and 1024 NAL units. Reference and output queues are bounded by the SPS,
with at most 16 references and 16 delayed pictures. Colocated motion metadata uses picture IDs rather than
pointers to older reference buffers. Camera resolution and reference count affect memory usage. No hardware acceleration is
used. RGB/JPEG conversion supports BT.601, BT.709 and non-constant-luminance
BT.2020; unspecified matrix coefficients use BT.601.

## Broker integration

The existing `RtspCamera` feature now chooses an H.264 track when an MJPEG track
is absent. It uses this package for RTP reassembly and pixel decoding, then the
Go standard library to encode JPEG snapshots. The existing RTSP library still
handles session negotiation and TCP/UDP transport. No H.264 depacketizer or
pixel decoder is imported from it.

Existing camera configurations require no migration. Snapshot topics, metadata,
triggering and round-robin slots are preserved.
`lastError` reports decoding failures. A lost or damaged picture clears the
cached snapshot and decoding resumes at a subsequent IDR.

The bridge retains the latest decoded picture and encodes JPEG only when a
continuous timer, manual request or MQTT trigger needs a snapshot. JPEG encoding
runs outside the RTP callback, so a slow snapshot interval avoids encoding all
the intervening pictures. In the default `FULL` mode, reference decoding still
processes the full stream; CPU use depends on the camera's resolution, frame
rate and coding tools as well as the snapshot interval. Raspberry Pi runtime
throughput has not been measured.

On Apple M4 (darwin/arm64, CGO disabled), a 20-second full-broker replay of the
recorded 1280x720 High-profile camera stream at 25 fps, with a 1000 ms snapshot
interval, used 30.3% of one CPU core after the snapshot/motion/deblocking fixes,
versus 103.3% before them. The updated broker decoded all 500 incoming frames;
the previous version had reached 446 when measured. Both published 19 snapshots
during that window. This is one stream on one machine, not a throughput guarantee.

For slower processors, select **H.264 Decoding → Keyframes only — lower CPU**
in the camera's dashboard settings. The persisted per-camera JSON/GraphQL field
is `h264DecodeMode: KEYFRAMES_ONLY`; `FULL` is the default for existing cameras.
Saving a changed mode restarts only that camera connection. There is no
broker-wide YAML option.

The same 20-second M4 replay in `KEYFRAMES_ONLY` mode used 10.1% of one CPU
core, decoding and publishing 16 selected pictures. CPU savings and snapshot
frequency depend on the stream's IDR cadence; the Atom E3940 has not been measured.

Keyframes-only decoding selects independent IDR pictures, at most once per
camera `intervalMs`, and skips intervening P/B and non-IDR I pictures. Parameter
sets are still processed. A selected IDR is returned immediately even when the
stream normally reorders B pictures. Packet loss or a source change resets the
limiter so the next intact IDR can restore snapshots promptly. In this mode,
`framesReceived` counts the selected pictures actually decoded.

This trades snapshot freshness for CPU use. Triggers return the latest decoded
keyframe; they do not force the camera to send a new one. A 2000 ms capture
interval decodes at most one keyframe every two seconds during uninterrupted
reception, and updates can be slower when keyframes arrive less frequently.
The camera must send periodic IDRs; some cameras using only gradual intra
refresh will not produce regular snapshots in this mode. MJPEG is unaffected,
and incoming RTSP network traffic is not reduced. For full-motion decoding,
reducing the camera's own frame rate or using a lower-resolution substream
reduces the work at the source; merely increasing the broker's snapshot interval
does not remove H.264 reference decoding in `FULL` mode.

## Verification

The standalone capture program tests a live camera and saves JPEG snapshots:

```sh
CGO_ENABLED=0 go run ./cmd/h264capture -url 'rtsp://CAMERA:554/live/0' -out /tmp/camera-check -frames 5
```

Use `-transport udp` to test UDP, `-timeout 30s` to change the total deadline,
and `-record` to save the compressed stream plus decoded planar pixels for
independent comparison. Without `-out`, a fresh temporary directory is created. Existing files are
not overwritten. Recording and comparison need no changes to the camera.

`test/integration/h264_test.go` compares every decoded Y, Cb and Cr sample
against independent reference output. The fixtures cover I/P/B pictures,
CAVLC/CABAC, Baseline/Main/High coding tools, cropping, multiple slices,
reference lists, scaling matrices, direct prediction, weighting and deblocking.
Synthetic PCM streams separately exercise all POC methods, frame/POC wraparound,
IDR/MMCO5 resets, discard/flush behavior and explicit P/B weights. A live 1280x720 High-profile camera was also checked: 30 consecutive frames,
including two IDR pictures, matched independent decoded pixels exactly. These
tests do not establish full H.264 conformance.

CABAC validates termination and following zero words. For compatibility with
FFmpeg/libx264 output, unused stuffing bits in the final arithmetic-coded byte
are accepted even when nonzero; trailing nonzero bytes are rejected.

`test/integration/rtsp_h264_test.go` drives real RTSP and MQTT listeners, with
TCP/UDP transport, fragmented NAL units, in-band/SDP parameters, moving images,
sequence/timestamp wraparound, and session shutdown. It also covers packet-loss
recovery, malformed input and fuzzing.

```sh
CGO_ENABLED=0 go test ./test/integration -run 'Test(H264|RTSPH264)' -count=1
go test -race ./test/integration -run 'Test(H264|RTSPH264)' -count=1
CGO_ENABLED=0 go test ./test/integration -run '^$' -fuzz '^FuzzH264Decoder$' -fuzztime=30s
CGO_ENABLED=0 go test ./test/integration -run '^$' -bench '^BenchmarkH264Decode' -benchmem
make build-arm64 build-armv7
```

The normal tests use checked-in fixtures and need no FFmpeg installation.
The generation script uses FFmpeg/libx264 as independent development tools,
never as runtime dependencies. See the fixture README for provenance.

Implementation sources:

- [ITU-T H.264 (08/2021)](https://www.itu.int/rec/T-REC-H.264-202108-S/en),
  clauses 7–9: syntax, reconstruction and entropy coding. Numeric tables in the
  code identify their normative table or equation numbers.
- [RFC 6184](https://www.rfc-editor.org/rfc/rfc6184), sections 5 and 7:
  RTP packetization and parameter transport.
