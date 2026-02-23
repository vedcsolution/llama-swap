#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC_DIR="${SRC_DIR:-$SCRIPT_DIR/llama.cpp}"
IMAGE_TAG="${IMAGE_TAG:-llama-cpp-spark:last}"
DOCKERFILE="${DOCKERFILE:-$SRC_DIR/Dockerfile.llama-cpp-spark}"
CUDA_VERSION="${CUDA_VERSION:-13.1.1}"
UBUNTU_VERSION="${UBUNTU_VERSION:-24.04}"
CUDA_ARCH="${CUDA_ARCH:-121}"

if [[ ! -d "$SRC_DIR/.git" ]]; then
  echo "Error: llama.cpp source not found at $SRC_DIR" >&2
  exit 1
fi

if [[ ! -f "$DOCKERFILE" ]]; then
  echo "Error: Dockerfile not found at $DOCKERFILE" >&2
  exit 1
fi

echo "[build] src=$SRC_DIR"
echo "[build] dockerfile=$DOCKERFILE"
echo "[build] image=$IMAGE_TAG"
echo "[build] cuda=${CUDA_VERSION} ubuntu=${UBUNTU_VERSION} arch=${CUDA_ARCH}"

docker build \
  --target server \
  -t "$IMAGE_TAG" \
  --build-arg CUDA_VERSION="$CUDA_VERSION" \
  --build-arg UBUNTU_VERSION="$UBUNTU_VERSION" \
  --build-arg CUDA_ARCH="$CUDA_ARCH" \
  -f "$DOCKERFILE" \
  "$SRC_DIR"

echo "[ok] Built $IMAGE_TAG"
