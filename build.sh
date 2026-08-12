#!/bin/bash

# build.sh - Master build script for MonsterMQ Edge
#
# Usage:
#   ./build.sh            Build all artifacts locally (binary, debian packages, docker image)
#   ./build.sh --binary   Build native Go binary for host
#   ./build.sh --deb      Build Debian packages for all target architectures (arm64, armhf, amd64)
#   ./build.sh --docker   Build local Docker image (native platform)
#   ./build.sh --publish  Build everything and trigger release publishing

set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

if [ ! -f "version.txt" ]; then
    echo -e "${RED}Error: version.txt not found${NC}"
    exit 1
fi

VERSION=$(head -n 1 version.txt | tr -d '\n' | tr -d '\r')

BUILD_BINARY=false
BUILD_DEB=false
BUILD_DOCKER=false
PUBLISH=false
CLEAN=false
EXPLICIT_TARGET=false

usage() {
    echo "Usage: $0 [options]"
    echo ""
    echo "Options:"
    echo "  --all            Build all artifacts (default)"
    echo "  --binary         Build native Go binary for current machine"
    echo "  --deb            Build Debian packages (arm64, armhf, amd64)"
    echo "  --docker         Build local Docker image (native platform)"
    echo "  -p, --publish    Trigger ./publish.sh after building"
    echo "  --clean          Clean build output directories"
    echo "  -h, --help       Show this help message"
    echo ""
    exit 0
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --all)
            BUILD_BINARY=true
            BUILD_DEB=true
            BUILD_DOCKER=true
            EXPLICIT_TARGET=true
            shift
            ;;
        --binary)
            BUILD_BINARY=true
            EXPLICIT_TARGET=true
            shift
            ;;
        --deb|-deb)
            BUILD_DEB=true
            EXPLICIT_TARGET=true
            shift
            ;;
        --docker)
            BUILD_DOCKER=true
            EXPLICIT_TARGET=true
            shift
            ;;
        -p|--publish)
            PUBLISH=true
            shift
            ;;
        --clean)
            CLEAN=true
            shift
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

# Default to building all if no target or clean specified
if [ "$EXPLICIT_TARGET" = false ] && [ "$CLEAN" = false ]; then
    BUILD_BINARY=true
    BUILD_DEB=true
    BUILD_DOCKER=true
fi

echo -e "${GREEN}=== MonsterMQ Edge Build Pipeline (v${VERSION}) ===${NC}"

if [ "$CLEAN" = true ]; then
    echo -e "${YELLOW}Cleaning output build directories...${NC}"
    make clean
    rm -rf dist/
    rm -f docker/monstermq-edge.tar
    echo -e "${GREEN}✓ Clean complete${NC}"
fi

if [ "$BUILD_BINARY" = false ] && [ "$BUILD_DEB" = false ] && [ "$BUILD_DOCKER" = false ]; then
    echo -e "${GREEN}No build targets specified. Clean operation finished.${NC}"
    exit 0
fi


if [ "$BUILD_BINARY" = true ]; then
    echo -e "${GREEN}[1/3] Building native Go binary...${NC}"
    make build
    echo -e "${GREEN}✓ Native binary built at: ${YELLOW}bin/monstermq-edge${NC}"
fi

if [ "$BUILD_DEB" = true ]; then
    echo -e "${GREEN}[2/3] Building Debian packages (arm64, armhf, amd64)...${NC}"
    make deb-all
    echo -e "${GREEN}✓ Debian packages built in: ${YELLOW}bin/${NC}"
fi

if [ "$BUILD_DOCKER" = true ]; then
    echo -e "${GREEN}[3/3] Building local Docker image...${NC}"
    (cd docker && ./build.sh -n)
    echo -e "${GREEN}✓ Local Docker image built${NC}"
fi

echo -e "${GREEN}=== Build Complete ===${NC}"

if [ "$PUBLISH" = true ]; then
    echo -e "${YELLOW}Triggering release publication...${NC}"
    ./publish.sh
fi

