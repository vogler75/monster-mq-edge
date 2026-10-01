#!/bin/bash

# build.sh - Build the WinCC OA API manager WCCOAmmq (broker embedded as c-archive)
#
# Usage:
#   ./winccoa/build.sh                       Build libmonstermq.a and WCCOAmmq
#   ./winccoa/build.sh --test                Also run the C ABI harness
#   ./winccoa/build.sh --clean               Rebuild the manager from an empty CMake build directory
#   ./winccoa/build.sh --install <project>   Copy WCCOAmmq to <project>/bin
#   ./winccoa/build.sh --restart <index>     Stop and start PMON manager <index> (needs woa)

set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
MANAGER_DIR="$SCRIPT_DIR/manager"
BUILD_DIR="$MANAGER_DIR/build"

export API_ROOT="${API_ROOT:-/opt/WinCC_OA/3.21/api}"

RUN_TEST=false
CLEAN=false
INSTALL_PROJECT=""
RESTART_INDEX=""

usage() {
    echo "Usage: $0 [options]"
    echo ""
    echo "Options:"
    echo "  --test               Run the C ABI harness (make embed-test)"
    echo "  --clean              Remove the CMake build directory first"
    echo "  --install <project>  Copy WCCOAmmq to <project>/bin"
    echo "  --restart <index>    Stop and start PMON manager <index> after the build"
    echo "  -h, --help           Show this help message"
    echo ""
    echo "Environment: API_ROOT (default /opt/WinCC_OA/3.21/api), WOA_PMON_* for --restart"
    exit 0
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --test)
            RUN_TEST=true
            shift
            ;;
        --clean)
            CLEAN=true
            shift
            ;;
        --install)
            [ -n "$2" ] || { echo -e "${RED}--install needs a project directory${NC}"; exit 1; }
            INSTALL_PROJECT="$2"
            shift 2
            ;;
        --restart)
            [[ "$2" =~ ^[0-9]+$ ]] || { echo -e "${RED}--restart needs a PMON manager index${NC}"; exit 1; }
            RESTART_INDEX="$2"
            shift 2
            ;;
        -h|--help)
            usage
            ;;
        *)
            echo -e "${RED}Unknown option: $1${NC}"
            usage
            ;;
    esac
done

if [ ! -f "$API_ROOT/CMakeDefines.txt" ]; then
    echo -e "${RED}Error: WinCC OA API not found at $API_ROOT (set API_ROOT)${NC}"
    exit 1
fi

echo -e "${GREEN}=== WCCOAmmq Build (API_ROOT=$API_ROOT) ===${NC}"

echo -e "${GREEN}[1/2] Building libmonstermq.a...${NC}"
if [ "$RUN_TEST" = true ]; then
    make -C "$REPO_DIR" embed-test
else
    make -C "$REPO_DIR" embed-lib
fi

echo -e "${GREEN}[2/2] Building WCCOAmmq...${NC}"
if [ "$CLEAN" = true ]; then
    rm -rf "$BUILD_DIR"
fi
mkdir -p "$BUILD_DIR"
(cd "$BUILD_DIR" && cmake .. >/dev/null && make -j"$(nproc)")
echo -e "${GREEN}✓ Manager built at: ${YELLOW}$BUILD_DIR/WCCOAmmq${NC}"

if [ -n "$INSTALL_PROJECT" ]; then
    if [ ! -d "$INSTALL_PROJECT/bin" ]; then
        echo -e "${RED}Error: $INSTALL_PROJECT/bin not found${NC}"
        exit 1
    fi
    # Copy to a temp name and rename, so a running manager keeps its binary.
    cp "$BUILD_DIR/WCCOAmmq" "$INSTALL_PROJECT/bin/.WCCOAmmq.new"
    mv -f "$INSTALL_PROJECT/bin/.WCCOAmmq.new" "$INSTALL_PROJECT/bin/WCCOAmmq"
    echo -e "${GREEN}✓ Installed to: ${YELLOW}$INSTALL_PROJECT/bin/WCCOAmmq${NC}"
fi

if [ -n "$RESTART_INDEX" ]; then
    if ! command -v woa >/dev/null 2>&1; then
        echo -e "${RED}Error: woa not found; restart manager $RESTART_INDEX in the console${NC}"
        exit 1
    fi
    echo -e "${YELLOW}Restarting PMON manager $RESTART_INDEX...${NC}"
    woa pmon manager stop "$RESTART_INDEX" || true
    for _ in $(seq 1 30); do
        pgrep -f "WCCOAmmq .*-pmonIndex $RESTART_INDEX( |$)" >/dev/null || break
        sleep 1
    done
    if pgrep -f "WCCOAmmq .*-pmonIndex $RESTART_INDEX( |$)" >/dev/null; then
        echo -e "${RED}Error: manager $RESTART_INDEX did not stop within 30s${NC}"
        exit 1
    fi
    woa pmon manager start "$RESTART_INDEX"
    echo -e "${GREEN}✓ Manager $RESTART_INDEX started${NC}"
fi

echo -e "${GREEN}=== Build Complete ===${NC}"
