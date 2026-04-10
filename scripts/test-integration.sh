#!/usr/bin/env bash
# Integration smoke test for unpacker.
# Requires: docker with unpacker:dev image already built.
#
# Usage:
#   ./scripts/test-integration.sh
#   IMAGE=unpacker:latest ./scripts/test-integration.sh

set -euo pipefail

IMAGE="${IMAGE:-unpacker:dev}"
# Public Helm OCI chart — small, stable, well-known
TEST_ARTIFACT="ghcr.io/stefanprodan/charts/podinfo:6.7.1"

pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

# Named volume avoids macOS bind-mount permission issues (non-root container user)
VOLUME="unpacker-test-$$"
docker volume create "$VOLUME" > /dev/null
trap 'docker volume rm "$VOLUME" > /dev/null' EXIT

# Volume is owned by root by default — open it up for the non-root container user
docker run --rm -v "$VOLUME:/out" alpine chmod 777 /out

echo "==> Image:    $IMAGE"
echo "==> Artifact: $TEST_ARTIFACT"
echo ""

echo "--- Pulling and unpacking..."
docker run --rm \
  -v "$VOLUME:/out" \
  "$IMAGE" \
  --public \
  --output-dir /out \
  --mediatype helm \
  "$TEST_ARTIFACT" || true

echo ""
echo "--- Volume contents after run (all entries)..."
docker run --rm -v "$VOLUME:/out" alpine find /out | sort
echo ""
echo "--- manifest.json..."
docker run --rm -v "$VOLUME:/out" alpine cat /out/manifest.json

echo ""
echo "--- Verifying output..."

FILE_COUNT=$(docker run --rm -v "$VOLUME:/out" alpine sh -c 'find /out/image -type f 2>/dev/null | wc -l' | tr -d ' ')

if [ "$FILE_COUNT" -eq 0 ]; then
  fail "image/ directory is empty or was not created"
fi

pass "$FILE_COUNT file(s) extracted to image/"
docker run --rm -v "$VOLUME:/out" alpine find /out/image -type f | sort | sed 's|.*/image/|  |'
