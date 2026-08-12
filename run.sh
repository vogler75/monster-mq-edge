#!/bin/bash
# MonsterMQ Edge run script (Go broker).
# Usage: ./run.sh [SCRIPT_OPTIONS] [-- BROKER_OPTIONS]
#
# Script options (before --):
#   -b,  -build       Build the binary (CGO_ENABLED=0) before starting
#   -c,  -compile     Type-check / compile all packages (no binary output)
#   -n,  -norun       Don't start the broker (combine with -b/-c for build-only)
#   -nk, -nokill      Don't kill any already-running monstermq-edge first
#   -h,  -help        Show this help
#
# Broker options (after --):
#   Anything after -- is passed straight to the binary.
#   See ./bin/monstermq-edge -h for available flags (-config, -log-level, -version).
#
# Examples:
#   ./run.sh                       Start with config.yaml (or config.yaml.example)
#   ./run.sh -b                    Build, then start
#   ./run.sh -b -n                 Build only
#   ./run.sh -b -- -config foo.yaml

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

BIN="bin/monstermq-edge"
BUILD=false
COMPILE=false
NORUN=false
NOKILL=false
REMAINING=()
SAW_SEP=false

for arg in "$@"; do
    if [ "$SAW_SEP" = true ]; then
        REMAINING+=("$arg")
        continue
    fi
    case "$arg" in
        -b|-build)        BUILD=true ;;
        -c|-compile)      COMPILE=true ;;
        -n|-norun)        NORUN=true ;;
        -nk|-nokill)      NOKILL=true ;;
        -h|-help|--help)  sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        --)               SAW_SEP=true ;;
        *)                REMAINING+=("$arg") ;;
    esac
done

if [ "$BUILD" = true ]; then
    echo "Building monstermq-edge..."
    mkdir -p bin
    VERSION=$(head -n 1 "$SCRIPT_DIR/version.txt" 2>/dev/null | tr -d '\n' | tr -d '\r' || echo "dev")
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X monstermq.io/edge/internal/version.Version=$VERSION" -o "$BIN" ./cmd/monstermq-edge
    echo "Built: $BIN ($(du -h "$BIN" | cut -f1))"
fi

if [ "$COMPILE" = true ] && [ "$BUILD" = false ]; then
    echo "Compiling all packages..."
    go build ./...
fi


[ "$NORUN" = true ] && exit 0

if [ ! -x "$BIN" ]; then
    echo "Binary $BIN not found. Run with -b first." >&2
    exit 1
fi

if [ "$NOKILL" = false ]; then
    EXISTING=$(pgrep -f "$BIN" || true)
    if [ -n "$EXISTING" ]; then
        echo "Killing existing instances: $EXISTING"
        kill $EXISTING 2>/dev/null || true
        for i in 1 2 3 4 5; do
            sleep 0.5
            STILL=$(pgrep -f "$BIN" || true)
            [ -z "$STILL" ] && break
        done
        STILL=$(pgrep -f "$BIN" || true)
        [ -n "$STILL" ] && kill -9 $STILL 2>/dev/null || true
    fi
fi

# Pick a config: explicit -config wins; otherwise prefer config.yaml then the example.
HAS_CONFIG=false
for arg in "${REMAINING[@]}"; do
    [ "$arg" = "-config" ] && HAS_CONFIG=true
done
if [ "$HAS_CONFIG" = false ]; then
    if [ -f "config.yaml" ]; then
        REMAINING=("-config" "config.yaml" "${REMAINING[@]}")
    elif [ -f "config.yaml.example" ]; then
        REMAINING=("-config" "config.yaml.example" "${REMAINING[@]}")
    fi
fi

echo "Starting: $BIN ${REMAINING[*]}"
exec "$BIN" "${REMAINING[@]}"
