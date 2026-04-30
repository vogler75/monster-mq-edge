#!/usr/bin/env sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
IMAGE_NAME="${IMAGE_NAME:-rocworks/monstermq-edge:latest}"

cd "$REPO_ROOT"
docker build -f docker/Dockerfile -t "$IMAGE_NAME" .
docker image save "$IMAGE_NAME" > "$SCRIPT_DIR/monstermq-edge.tar"
