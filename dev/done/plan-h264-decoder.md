# Native Go H.264 decoder

## Objective

Implement our own reusable H.264 decoder and use it in the broker's RTSP
camera snapshot path. All codec and H.264 RTP payload handling code must be
written here, without importing, wrapping, copying, or translating an existing
open source codec implementation. Preserve CGO_ENABLED=0 and ARM64/ARMv7 builds.
The existing RTSP session transport may remain in use. No schema or storage
changes are needed.

## Sources and verification

- ITU-T H.264 (08/2021), clauses 7–9 and Annex A:
  https://www.itu.int/rec/T-REC-H.264-202108-S/en
- RFC 6184 for H.264 RTP payload framing:
  https://www.rfc-editor.org/rfc/rfc6184
- FFmpeg is a development-only independent encoder/decoder oracle. It is not
  imported, invoked, or needed by the library or the broker at runtime.
- Check in small generated compressed streams and independent decoded pixel
  references, with regeneration instructions. Exercise real RTSP and MQTT
  listeners in test/integration, including fragmented RTP and packet loss.

## Delivered implementation and validation

- [x] Public library API, bounded bit reader, NAL/RBSP framing, SPS/PPS parsing for the implemented formats.
- [x] CAVLC, inverse quantization/transform, intra prediction and reconstruction (4x4 and 8x8).
- [x] Inter prediction, reference picture management, deblocking, frame ordering.
- [x] CABAC and common Baseline/Main/High 8-bit 4:2:0 camera streams.
- [x] Own RFC 6184 single NAL, STAP-A and FU-A depacketizer with loss recovery.
- [x] Integrate with camera selection, JPEG caching, error reporting, shutdown.
- [x] Pixel comparison for varied content, I/P/B pictures, multiple slices,
      cropping, entropy modes and parameter changes; malformed-input regression
      tests, bounded allocations and short fuzz runs.
- [x] RTSP-to-MQTT tests, focused race checks, repository tests and no-CGO ARM builds.
- [x] Document public API, supported features, limits and measured M4 performance.

Accepted by the user after successful testing with their camera on 2026-09-17.
This plan is archived with the supported formats and limitations documented.


## Implementation checkpoint (2026-09-17)

The delivered decoder supports I/P/B pictures. Potential follow-up work concerns
validation breadth, malformed-stream hardening and snapshot performance. The
passing corpus does not establish full H.264 conformance.

Implemented in `pkg/h264/` using only the Go standard library:

- SPS/PPS and slice parsing, Annex B splitting and emulation prevention.
- CAVLC and CABAC arithmetic decoding and context models from normative tables.
- I/P/B reconstruction for Baseline/Main/High 8-bit progressive 4:2:0, including
  all intra modes, 4x4/8x8 transforms, subpartitions, fractional compensation,
  both reference lists, spatial/temporal direct modes and explicit/implicit
  weighting. Reference motion history stores IDs, avoiding image-retention chains.
- SPS/PPS scaling matrices with default/inheritance rules and widened scaling
  arithmetic for ARMv7.
- POC types 0/1/2, display reordering and `Flush`, including IDR/MMCO5 resets and
  `no_output_of_prior_pics_flag`. Errors/discontinuities discard pending output.
- Deblocking compares both lists, including swapped reference pairs.
- CABAC termination/zero-word validation; final arithmetic-byte stuffing is
  tolerated because FFmpeg/libx264 fixtures contain nonzero unused bits there.
- Typed errors, input/pixel bounds, IDR recovery, owned output frames, YCbCr and
  range/matrix-aware RGB/JPEG conversion.
- RTSP integration, cleanup, loss reporting and recovery; no schema, storage,
  go.mod or go.sum changes.

Current verification evidence:

- 33 checked-in streams, 446 decoded pictures compared sample-for-sample with
  independent FFmpeg output. `ffprobe -count_frames` independently verified the
  count. Corpus includes CAVLC/CABAC (init 0/1/2), I/P/B, B-pyramid, direct modes,
  4x4/8x8 transforms, scaling matrices, multiple slices, cropping and references.
- Minimal independently written PCM/skip streams test all POC methods, wraparound,
  IDR/MMCO5 resets, prior-output discard, flush and explicit P/B weights.
- Real TCP/UDP RTSP to MQTT JPEG snapshots, including B-pyramid, SDP/in-band
  parameters, FU-A/STAP-A, wraparound, injected loss, recovery and stop.
- Parameter changes, output ownership, byte/pixel limits and CABAC tails tested.
- Latest `CGO_ENABLED=0 make test`, `make lint`, native/ARM64/ARMv7 builds passed.
- Latest focused H264/RTSPH264 race tests passed (10 seconds).
- Multi-access-unit fuzzing exercises B-picture reference chains. Two 20-second
  runs passed (17,017 executions before the CABAC-tail changes; 1,692 afterward,
  including corpus exploration/minimization).
- Full race suite exposes an existing GraphQL Start/Stop race at server.go
  lines 257/270/273. Reproduced against a pristine `git archive HEAD` under
  /tmp/monstermq-h264/baseline, with MCP/mTLS tests repeated three times. Baseline
  log: /tmp/monstermq-h264/baseline-race.log. Do not claim full-race success.
- Isolated Apple M4/darwin-arm64 benchmarks (CGO disabled, 1-second runs):
  128x96 High CABAC IDR 0.525 ms, 143,771 B and 24 allocations per picture;
  640x360 High CABAC IDR 5.824 ms, 2,465,173 B and 24 allocations per picture.
  These are intra benchmarks, not predictive-stream or Raspberry Pi throughput.

Live camera verification supplied by the user:

- Added `cmd/h264capture` with URL/transport/count/deadline flags, JPEG output,
  optional Annex B + raw YCbCr recording and cancellation. Uses the same native
  decoder, own depacketizer and existing RTSP session transport. Output defaults
  to a unique temporary directory; existing output files are never overwritten.
- Camera advertised High-profile 1280x720 progressive H.264. Its SPS permits
  frame-number gaps; parser now accepts the permission flag. An actual missing
  reference still returns explicit `ErrUnsupported` when gaps are permitted.
  A synthetic regression verifies both allowed/forbidden-gap behavior.
- Captured 5 and then 30 consecutive frames over TCP, including a second IDR.
  Both captures match independent FFmpeg output byte-for-byte. The longer test
  compares 41,472,000 Y/Cb/Cr samples. Mean decoder time was about 18.3 ms per
  returned access unit on the Apple M4, excluding JPEG conversion and file I/O.
- Live images and recordings are only under /tmp/monstermq-h264/camera-live-2
  and camera-live-3; never copy private camera pictures into the fixture corpus.
  Camera URL remains a runtime argument, not hardcoded into source or tests.
- A further five-frame live capture with `go run -race` passed. The capture
  command passed `go vet` and CGO-disabled Linux ARM64/ARMv7 builds. Its default
  temporary-output behavior was exercised by that live race run.

Optional follow-up work:

1. Implement permitted frame-number gap inference (non-existing reference
   pictures), currently an explicit unsupported case only when a gap occurs.
   Expand coverage of adaptive short-/long-term reference marking (MMCO 1–4/6),
   SPS-level scaling inheritance, longer predictive streams and realistic timestamp ordering. A real 720p
   camera now has exact independent pixel validation. Existing explicit P/B weighting and POC tests are passing.
2. Audit malformed input and allocation bounds across the newly supported paths;
   expand fuzz seeds to custom matrices and test loss while B frames await display.
3. Encode JPEG only when a snapshot is needed instead of encoding every decoded
   frame. Measure whole-stream decoding and memory use across resolutions. Real 720p
   decoding was measured on M4; Raspberry Pi throughput is not yet measured.
4. Repeat relevant full checks after future changes. Keep
   unsupported extensions explicit and the original no-external-codec constraint.

Normative source material was downloaded (not another implementation) to
`/tmp/monstermq-h264/spec.pdf`; text extraction is `spec.txt`. The spec is
ITU-T H.264 (08/2021), downloaded from the ITU publication link. Numeric CAVLC
and CABAC tables came from this specification. The scratch table extraction
script is `/tmp/monstermq-h264/cabac_tables.py`. No source code was copied or
translated from an existing decoder. Do not replace this implementation with a
third-party codec in subsequent work.
