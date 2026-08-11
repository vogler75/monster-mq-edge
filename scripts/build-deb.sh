#!/bin/bash

# build-deb.sh - Build a Debian (.deb) package for MonsterMQ Edge on macOS / Linux
#
# This script:
# 1. Resolves architecture (arm64, armhf, amd64)
# 2. Reads or overrides the version (default: from version.txt)
# 3. Compiles the static Go binary for target GOOS=linux
# 4. Stages files (binary, config, systemd service, control files)
# 5. Packages everything into a compliant .deb using tar and ar

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Default values
TARGET_ARCH="arm64"
MAINTAINER="info@monstermq.com"
DESCRIPTION="MonsterMQ Edge MQTT Broker"
VERSION_OVERRIDE=""

usage() {
    echo "Usage: $0 [options]"
    echo ""
    echo "Options:"
    echo "  -a, --arch ARCH        Target architecture (arm64, armhf, amd64) [default: arm64]"
    echo "  -v, --version VERSION  Override version string [default: read from version.txt]"
    echo "  -m, --maintainer NAME  Override maintainer info"
    echo "  -d, --description DESC Override package description"
    echo "  -h, --help             Show this help message"
    echo ""
    exit 1
}

# Parse command line arguments
while [[ $# -gt 0 ]]; do
    case "$1" in
        -a|--arch)
            TARGET_ARCH="$2"
            shift 2
            ;;
        -v|--version)
            VERSION_OVERRIDE="$2"
            shift 2
            ;;
        -m|--maintainer)
            MAINTAINER="$2"
            shift 2
            ;;
        -d|--description)
            DESCRIPTION="$2"
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

# Validate target architecture and map to Go build variables
case "$TARGET_ARCH" in
    arm64)
        GOARCH="arm64"
        GOARM=""
        DEB_ARCH="arm64"
        ;;
    armhf|arm)
        GOARCH="arm"
        GOARM="7"
        DEB_ARCH="armhf"
        ;;
    amd64|x86_64)
        GOARCH="amd64"
        GOARM=""
        DEB_ARCH="amd64"
        ;;
    *)
        echo -e "${RED}Unsupported architecture: $TARGET_ARCH. Choose from: arm64, armhf, amd64.${NC}"
        exit 1
        ;;
esac

# Resolve version
if [ -n "$VERSION_OVERRIDE" ]; then
    VERSION="$VERSION_OVERRIDE"
else
    if [ -f "version.txt" ]; then
        VERSION=$(head -n 1 version.txt | tr -d '\n' | tr -d '\r')
    else
        VERSION="0.1.0-dev"
    fi
fi

# Convert version '+' characters to Debian-compliant '~' (often used for pre-releases/git shas)
DEB_VERSION=$(echo "$VERSION" | tr '+' '~')

echo -e "${GREEN}=== Building MonsterMQ Edge Debian Package ===${NC}"
echo -e "Target Architecture : ${YELLOW}${DEB_ARCH}${NC} (Go: GOARCH=${GOARCH}${GOARM:+, GOARM=${GOARM}})"
echo -e "Version             : ${YELLOW}${DEB_VERSION}${NC}"
echo -e "Maintainer          : ${YELLOW}${MAINTAINER}${NC}"

# Define build and staging paths
STAGING_DIR="dist/stage_${DEB_ARCH}"
CONTROL_DIR="${STAGING_DIR}/control"
DATA_DIR="${STAGING_DIR}/data"
OUT_DIR="bin"

# Cleanup previous builds
rm -rf "$STAGING_DIR"
mkdir -p "$CONTROL_DIR"
mkdir -p "${DATA_DIR}/usr/local/bin"
mkdir -p "${DATA_DIR}/etc/monstermq"
mkdir -p "${DATA_DIR}/lib/systemd/system"
mkdir -p "$OUT_DIR"

# 1. Compile Go binary
echo -e "${GREEN}1. Compiling Go binary...${NC}"
LDFLAGS="-s -w -X monstermq.io/edge/internal/version.Version=${VERSION}"
PKG="./cmd/monstermq-edge"

export GOOS=linux
export GOARCH="$GOARCH"
export CGO_ENABLED=0
if [ -n "$GOARM" ]; then
    export GOARM="$GOARM"
else
    unset GOARM
fi

go build -trimpath -ldflags="$LDFLAGS" -o "${DATA_DIR}/usr/local/bin/monstermq-edge" "$PKG"

# 2. Stage Config & Systemd files
echo -e "${GREEN}2. Staging config and systemd files...${NC}"
cp scripts/deb/config.yaml "${DATA_DIR}/etc/monstermq/config.yaml"
cp systemd/monstermq-edge.service "${DATA_DIR}/lib/systemd/system/monstermq-edge.service"

# Ensure correct permissions in data directory
chmod 755 "${DATA_DIR}/usr/local/bin/monstermq-edge"
chmod 640 "${DATA_DIR}/etc/monstermq/config.yaml"
chmod 644 "${DATA_DIR}/lib/systemd/system/monstermq-edge.service"

# 3. Stage Control scripts and templates
echo -e "${GREEN}3. Staging Debian control files...${NC}"
# Substitute template variables in control file
sed -e "s/{{VERSION}}/${DEB_VERSION}/g" \
    -e "s/{{ARCH}}/${DEB_ARCH}/g" \
    -e "s/{{MAINTAINER}}/${MAINTAINER}/g" \
    -e "s/{{DESCRIPTION}}/${DESCRIPTION}/g" \
    scripts/deb/control.template > "${CONTROL_DIR}/control"

# Copy maintainer scripts and conffiles
cp scripts/deb/postinst "${CONTROL_DIR}/postinst"
cp scripts/deb/prerm "${CONTROL_DIR}/prerm"
cp scripts/deb/postrm "${CONTROL_DIR}/postrm"
cp scripts/deb/conffiles "${CONTROL_DIR}/conffiles"

# Make maintainer scripts executable and set conffiles permission
chmod 755 "${CONTROL_DIR}/postinst" "${CONTROL_DIR}/prerm" "${CONTROL_DIR}/postrm"
chmod 644 "${CONTROL_DIR}/conffiles"

# 4. Package archives
echo -e "${GREEN}4. Creating package archives...${NC}"
# We set COPYFILE_DISABLE=1 to prevent macOS tar from including metadata files (._files)
export COPYFILE_DISABLE=1

# Generate control.tar.gz
(
    cd "$CONTROL_DIR"
    tar --format=ustar --no-xattrs --no-acls --owner=root --group=root -czf "../control.tar.gz" .
)

# Generate data.tar.gz
(
    cd "$DATA_DIR"
    tar --format=ustar --no-xattrs --no-acls --owner=root --group=root -czf "../data.tar.gz" .
)

# Create debian-binary specification file
echo "2.0" > "${STAGING_DIR}/debian-binary"

# 5. Assemble the .deb package
echo -e "${GREEN}5. Assembling .deb package...${NC}"
PACKAGE_NAME="${OUT_DIR}/monstermq-edge_${DEB_VERSION}_${DEB_ARCH}.deb"

# The files MUST be archived in this exact order: debian-binary, control.tar.gz, data.tar.gz
(
    cd "$STAGING_DIR"
    rm -f "../../${PACKAGE_NAME}"
    ar rcS "../../${PACKAGE_NAME}" debian-binary control.tar.gz data.tar.gz
)

# Cleanup staging directory
rm -rf "$STAGING_DIR"

echo -e "${GREEN}✓ Package built successfully: ${YELLOW}${PACKAGE_NAME}${NC}"
