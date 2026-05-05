#!/usr/bin/env bash
set -eu

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)

IMAGE_REPO="${IMAGE_REPO:-rocworks/monstermq-edge}"

# Multi-arch platforms for buildx
PLATFORMS="${PLATFORMS:-linux/amd64,linux/arm64,linux/arm/v7}"

# Detect buildx command (standalone on Mac vs subcommand on Linux)
if command -v docker-buildx > /dev/null 2>&1; then
    BUILDX="docker-buildx"
elif docker buildx version > /dev/null 2>&1; then
    BUILDX="docker buildx"
else
    BUILDX=""
fi

# Parse command line arguments
TESTING=false
PUBLISH_MODE="ask"  # can be "ask", "yes", or "no"
SAVE_TAR=true       # save image as tar for offline/sneakernet deployment

usage() {
    echo "Usage: $0 [--testing|-t] [-n] [-y] [--no-tar]"
    echo "  --testing, -t      Build testing image (tag: testing)"
    echo "  -n                 Do not publish to Docker Hub (local build only)"
    echo "  -y                 Multi-arch build and publish to Docker Hub without asking"
    echo "  --no-tar           Skip saving image as tar file"
    echo ""
    echo "Without -n or -y, the script asks whether to publish."
    echo "Local builds (no publish) build only the native platform and save a tar file."
    echo "Publishing builds multi-arch (${PLATFORMS}) and pushes to Docker Hub."
}

for arg in "$@"; do
    case $arg in
        --testing|-t)
            TESTING=true
            ;;
        -n)
            PUBLISH_MODE="no"
            ;;
        -y)
            PUBLISH_MODE="yes"
            ;;
        --no-tar)
            SAVE_TAR=false
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            echo "Unknown argument: $arg"
            usage
            exit 1
            ;;
    esac
done

# Determine version
if [ "$TESTING" = true ]; then
    VERSION="testing"
    echo -e "${YELLOW}Building Docker image for MonsterMQ Edge (testing)...${NC}"
else
    if [ ! -f "${REPO_ROOT}/version.txt" ]; then
        echo -e "${RED}Error: version.txt not found at ${REPO_ROOT}/version.txt${NC}"
        exit 1
    fi
    # Use the full version (including +sha if present) for the build-time ldflags,
    # but use the bare semver for the docker tag.
    FULL_VERSION=$(head -n 1 "${REPO_ROOT}/version.txt" | tr -d '\n' | tr -d '\r')
    VERSION=$(echo "$FULL_VERSION" | cut -d'+' -f1)
    echo -e "${YELLOW}Building Docker image for MonsterMQ Edge v${VERSION}...${NC}"
fi

# Determine if we should push (needed before build because buildx combines build+push for multi-arch)
SHOULD_PUSH=false

if [ "$PUBLISH_MODE" = "yes" ]; then
    SHOULD_PUSH=true
elif [ "$PUBLISH_MODE" = "ask" ]; then
    echo ""
    read -p "Do you want to build multi-arch and push to Docker Hub? (y/n) " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        SHOULD_PUSH=true
    fi
fi

cd "$REPO_ROOT"

# Build Docker image(s)
if [ "$SHOULD_PUSH" = true ]; then
    if [ -z "$BUILDX" ]; then
        echo -e "${RED}✗ docker buildx is required for multi-arch publish${NC}"
        exit 1
    fi

    BUILDER_NAME="monstermq-edge-builder"
    if ! $BUILDX inspect "$BUILDER_NAME" > /dev/null 2>&1; then
        echo -e "${YELLOW}Creating buildx builder '${BUILDER_NAME}'...${NC}"
        $BUILDX create --name "$BUILDER_NAME" --use
    else
        $BUILDX use "$BUILDER_NAME"
    fi

    echo -e "${YELLOW}Building and pushing multi-arch images (${PLATFORMS})...${NC}"
    if [ "$TESTING" = true ]; then
        BUILD_VERSION="${FULL_VERSION:-testing}"
        $BUILDX build --platform "$PLATFORMS" --push \
            --build-arg VERSION="testing" \
            -t "${IMAGE_REPO}:testing" \
            -f docker/Dockerfile \
            .
    else
        $BUILDX build --platform "$PLATFORMS" --push \
            --build-arg VERSION="${FULL_VERSION}" \
            -t "${IMAGE_REPO}:${VERSION}" \
            -t "${IMAGE_REPO}:latest" \
            -f docker/Dockerfile \
            .
    fi

    echo -e "${GREEN}✓ Multi-arch images built and pushed${NC}"
    if [ "$TESTING" = true ]; then
        echo -e "${GREEN}  - ${IMAGE_REPO}:testing${NC}"
    else
        echo -e "${GREEN}  - ${IMAGE_REPO}:${VERSION}${NC}"
        echo -e "${GREEN}  - ${IMAGE_REPO}:latest${NC}"
    fi
else
    # Local build only (native platform), with optional tar save
    if [ -n "$BUILDX" ]; then
        DOCKER_BUILD_LOCAL="$BUILDX build --load"
    else
        echo -e "${YELLOW}buildx not available, using legacy builder${NC}"
        DOCKER_BUILD_LOCAL="DOCKER_BUILDKIT=0 docker build"
    fi

    echo -e "${YELLOW}Building Docker image (local, native platform only)...${NC}"
    if [ "$TESTING" = true ]; then
        eval $DOCKER_BUILD_LOCAL \
            --build-arg VERSION="testing" \
            -t "${IMAGE_REPO}:testing" \
            -f docker/Dockerfile \
            .
        IMAGE_TAG="${IMAGE_REPO}:testing"
    else
        eval $DOCKER_BUILD_LOCAL \
            --build-arg VERSION="${FULL_VERSION}" \
            -t "${IMAGE_REPO}:${VERSION}" \
            -t "${IMAGE_REPO}:latest" \
            -f docker/Dockerfile \
            .
        IMAGE_TAG="${IMAGE_REPO}:${VERSION}"
    fi

    echo -e "${GREEN}✓ Docker image built successfully (native platform)${NC}"
    if [ "$TESTING" = true ]; then
        echo -e "${GREEN}  - ${IMAGE_REPO}:testing${NC}"
    else
        echo -e "${GREEN}  - ${IMAGE_REPO}:${VERSION}${NC}"
        echo -e "${GREEN}  - ${IMAGE_REPO}:latest${NC}"
    fi

    if [ "$SAVE_TAR" = true ]; then
        echo -e "${YELLOW}Saving image to ${SCRIPT_DIR}/monstermq-edge.tar...${NC}"
        docker image save "${IMAGE_TAG}" > "${SCRIPT_DIR}/monstermq-edge.tar"
        echo -e "${GREEN}✓ Image saved to monstermq-edge.tar${NC}"
    fi

    echo -e "${YELLOW}Image built for native platform only. To build multi-arch and push, run with -y${NC}"
fi
