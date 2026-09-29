#!/bin/bash
# AC-35 soak for an embedded broker (WCCOAmmq) that runs under PMON.
# 10-minute rounds of cmd/mmqload with fresh clients; one CSV line per round.
#
#   winccoa/scripts/mmq-soak.sh <broker-url> <hours> <out.csv> [manager-pid-file]
#
# Needs the MMQLoad datapoints (mmqCreateLoadDps.ctl) and build/mmqload
# (go build -o build/mmqload ./cmd/mmqload).
set -u
BROKER=${1:-tcp://127.0.0.1:1883}
HOURS=${2:-24}
OUT=${3:-soak.csv}
PIDFILE=${4:-}
END=$(( $(date +%s) + HOURS*3600 ))
echo "time,round,sent,received,lost,p50,p99,max,rss_kib" > "$OUT"
r=0
while [ "$(date +%s)" -lt "$END" ]; do
  r=$((r+1))
  if ! ./build/mmqload -broker "$BROKER" -clients 50 -writers 4 -dpes 5000 -rate 2000 -duration 600s > soak-last.json 2>&1; then
    echo "$(date -Iseconds),$r,broker unreachable,,,,,," >> "$OUT"
    sleep 30   # e.g. project restart; do not spin
    continue
  fi
  j() { grep "\"$1\"" soak-last.json | head -1 | sed -E 's/.*: "?([^",]*)"?,?/\1/'; }
  RSS=""
  [ -n "$PIDFILE" ] && RSS=$(awk '/VmRSS/{print $2}' "/proc/$(cat "$PIDFILE")/status" 2>/dev/null)
  echo "$(date -Iseconds),$r,$(j sent),$(j received),$(j lost),$(j p50),$(j p99),$(j max),$RSS" >> "$OUT"
done
