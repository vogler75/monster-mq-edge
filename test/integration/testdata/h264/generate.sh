#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

# FFmpeg/libx264 generate independent input and expected pixels only. Neither is
# used by the decoder or required when running the checked-in tests.
encode() {
  local name="$1" source="$2" qp="$3" params="$4"
  ffmpeg -nostdin -v error -y -f lavfi -i "$source" -frames:v 1 \
    -c:v libx264 -profile:v baseline -qp "$qp" \
    -x264-params "keyint=1:cabac=0:8x8dct=0:$params" -f h264 "$name.264"
  ffmpeg -nostdin -v error -y -i "$name.264" -f rawvideo -pix_fmt yuv420p "$name.yuv"
}
encode intra-baseline 'testsrc2=size=64x48:rate=1' 24 ''
encode intra-crop 'testsrc2=size=94x70:rate=1' 19 ''
encode intra-slices 'testsrc2=size=128x96:rate=1' 32 'slices=3'
encode intra-detail 'testsrc2=size=128x96:rate=1' 5 ''
encode intra-flat 'color=c=gray:size=32x32:rate=1' 37 ''
encode intra-plane 'nullsrc=size=64x64,geq=lum=X+Y:cb=100+X/4:cr=150-Y/4' 27 ''
encode intra-no-deblock 'testsrc2=size=128x96:rate=1' 27 'no-deblock=1'
encode intra-deblock-offset 'testsrc2=size=128x96:rate=1' 34 'deblock=3,-2'

encode_motion() {
  local name="$1" source="$2" qp="$3" params="$4"
  ffmpeg -nostdin -v error -y -f lavfi -i "$source" -frames:v 20 \
    -c:v libx264 -profile:v baseline -qp "$qp" \
    -x264-params "keyint=12:min-keyint=12:scenecut=0:cabac=0:8x8dct=0:bframes=0:aud=1:$params" -f h264 "$name.264"
  ffmpeg -nostdin -v error -y -i "$name.264" -f rawvideo -pix_fmt yuv420p "$name.yuv"
}
encode_motion motion-baseline 'testsrc2=size=96x64:rate=10' 24 'ref=1'
encode_motion motion-refs 'testsrc2=size=128x96:rate=10' 28 'ref=3'
encode_motion motion-slices 'testsrc2=size=94x70:rate=10' 20 'ref=3:slices=3'

encode_cabac_intra() {
  local name="$1" source="$2" qp="$3" params="$4"
  ffmpeg -nostdin -v error -y -f lavfi -i "$source" -frames:v 1 \
    -c:v libx264 -profile:v main -qp "$qp" \
    -x264-params "keyint=1:cabac=1:8x8dct=0:$params" -f h264 "$name.264"
  ffmpeg -nostdin -v error -y -i "$name.264" -f rawvideo -pix_fmt yuv420p "$name.yuv"
}
encode_cabac_intra intra-cabac 'testsrc2=size=96x64:rate=1' 24 ''
encode_cabac_intra intra-cabac-slices 'testsrc2=size=94x70:rate=1' 18 'slices=3'
encode_cabac_intra intra-cabac-flat 'color=c=gray:size=32x32:rate=1' 32 ''
encode_cabac_intra intra-cabac-detail 'testsrc2=size=128x96:rate=1' 5 ''

encode_cabac_motion() {
  local name="$1" params="$2"
  ffmpeg -nostdin -v error -y -f lavfi -i 'testsrc2=size=128x96:rate=10' -frames:v 20 \
    -c:v libx264 -profile:v main -qp 24 \
    -x264-params "keyint=12:min-keyint=12:scenecut=0:cabac=1:8x8dct=0:bframes=0:aud=1:$params" -f h264 "$name.264"
  ffmpeg -nostdin -v error -y -i "$name.264" -f rawvideo -pix_fmt yuv420p "$name.yuv"
}
encode_cabac_motion motion-cabac 'ref=1:cabac-idc=0'
encode_cabac_motion motion-cabac-refs 'ref=3:cabac-idc=1'
encode_cabac_motion motion-cabac-slices 'ref=3:cabac-idc=2:slices=3'

encode_high() {
  local name="$1" frames="$2" params="$3"
  ffmpeg -nostdin -v error -y -f lavfi -i 'testsrc2=size=128x96:rate=10' -frames:v "$frames" \
    -c:v libx264 -profile:v high -qp 24 \
    -x264-params "keyint=12:min-keyint=12:scenecut=0:8x8dct=1:bframes=0:aud=1:$params" -f h264 "$name.264"
  ffmpeg -nostdin -v error -y -i "$name.264" -f rawvideo -pix_fmt yuv420p "$name.yuv"
}
encode_high intra-high-cabac 1 'cabac=1'
encode_high intra-high-cavlc 1 'cabac=0'
encode_high motion-high-cabac 20 'cabac=1:ref=3'
encode_high motion-high-cavlc 20 'cabac=0:ref=3'

ffmpeg -nostdin -v error -y -f lavfi -i 'testsrc2=size=640x360:rate=1' -frames:v 1 \
  -c:v libx264 -profile:v high -qp 24 -x264-params 'keyint=1:cabac=1:8x8dct=1' \
  -f h264 intra-360p.264
ffmpeg -nostdin -v error -y -i intra-360p.264 -f rawvideo -pix_fmt yuv420p intra-360p.yuv

encode_b() {
  local name="$1" params="$2"
  ffmpeg -nostdin -v error -y -f lavfi -i 'testsrc2=size=128x96:rate=10' -frames:v 30 \
    -c:v libx264 -profile:v high -qp 24 \
    -x264-params "keyint=15:min-keyint=15:scenecut=0:aud=1:bframes=3:b-adapt=0:b-pyramid=0:ref=3:$params" -f h264 "$name.264"
  ffmpeg -nostdin -v error -y -i "$name.264" -f rawvideo -pix_fmt yuv420p "$name.yuv"
}
encode_b motion-b-cavlc 'cabac=0:8x8dct=0:direct=spatial:weightb=0'
encode_b motion-b-temporal 'cabac=0:8x8dct=0:direct=temporal:weightb=1'
encode_b motion-b-cabac 'cabac=1:8x8dct=1:direct=spatial:weightb=1'
encode_b motion-b-pyramid 'cabac=1:8x8dct=1:direct=temporal:weightb=1:b-pyramid=normal'
encode_b motion-b-high-cavlc 'cabac=0:8x8dct=1:direct=spatial:weightb=1:b-pyramid=normal'
encode_b motion-b-slices 'cabac=1:8x8dct=0:direct=temporal:weightb=1:slices=3:cabac-idc=1'
encode_b motion-b-init2 'cabac=1:8x8dct=1:direct=spatial:weightb=0:cabac-idc=2'
encode_high intra-scaling-jvt 1 'cabac=1:cqm=jvt'
encode_b motion-scaling-jvt 'cabac=1:8x8dct=1:cqm=jvt:direct=auto:b-pyramid=normal'
cqm4='8,12,15,18,11,16,19,24,14,20,26,31,17,23,29,37'
cqm8=''
for ((i=0; i<64; i++)); do
  if [[ -n "$cqm8" ]]; then cqm8+=,; fi
  cqm8+=$((8 + i/8*3 + i%8*2))
done
encode_b motion-scaling-custom "cabac=0:8x8dct=1:cqm4=$cqm4:cqm8=$cqm8:direct=temporal"
