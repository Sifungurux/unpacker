#!/usr/bin/env bash
# Integration smoke test for unpacker.
# Requires: docker with unpacker:dev image already built.
#
# Usage:
#   ./scripts/test-integration.sh
#   IMAGE=unpacker:latest ./scripts/test-integration.sh

set -euo pipefail

IMAGE="${IMAGE:-unpacker:dev}"

pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

run_test() {
  local name="$1"
  local artifact="$2"
  shift 2
  local extra_flags=("$@")

  echo "==> Test: $name"
  echo "    Artifact: $artifact"

  # Named volume avoids macOS bind-mount permission issues (non-root container user)
  local volume="unpacker-test-$$-${RANDOM}"
  docker volume create "$volume" > /dev/null
  # Volume is owned by root by default — open it up for the non-root container user
  docker run --rm -v "$volume:/out" alpine chmod 777 /out

  docker run --rm \
    -v "$volume:/out" \
    "$IMAGE" \
    --public \
    --output-dir /out \
    ${extra_flags[@]+"${extra_flags[@]}"} \
    "$artifact"

  local file_count
  file_count=$(docker run --rm -v "$volume:/out" alpine sh -c 'find /out/image -type f 2>/dev/null | wc -l' | tr -d ' ')

  docker volume rm "$volume" > /dev/null

  if [ "$file_count" -eq 0 ]; then
    fail "$name — image/ directory is empty or was not created"
  fi

  pass "$name — $file_count file(s) extracted"
  echo ""
}

echo "Image: $IMAGE"
echo ""

# Test 1: OCI artifact (Helm chart) — exercises oras path + tar extraction
run_test "Helm OCI chart" \
  "ghcr.io/stefanprodan/charts/podinfo:6.7.1" \
  --mediatype helm

# Test 2: Docker image — exercises crane fallback + umoci unpack
# No --mediatype override needed: docker layer types don't match the defaults
# (flux/helm) so Unpack routes to umoci automatically
run_test "Docker image (alpine)" \
  "alpine:3.21"

# Test 3: Self-created OCI artifact — local registry, plain HTTP, oras path
echo "==> Test: Self-created OCI artifact (local registry)"
NETWORK="unpacker-net-$$"
REGISTRY="unpacker-registry-$$"
ARTIFACT_VOL="unpacker-artifact-$$"

docker network create "$NETWORK" > /dev/null
docker volume create "$ARTIFACT_VOL" > /dev/null
docker run -d --name "$REGISTRY" --network "$NETWORK" registry:2 > /dev/null
trap 'docker rm -f "$REGISTRY" > /dev/null 2>&1; docker network rm "$NETWORK" > /dev/null 2>&1; docker volume rm "$ARTIFACT_VOL" > /dev/null 2>&1' EXIT

# Wait for registry to be ready
sleep 1

# Build a simple tar.gz artifact: two text files
docker run --rm -v "$ARTIFACT_VOL:/workspace" alpine sh -c "
  mkdir -p /workspace/content && \
  echo 'hello from OCI artifact' > /workspace/content/hello.txt && \
  echo 'second file' > /workspace/content/world.txt && \
  tar czf /workspace/artifact.tgz -C /workspace/content .
"

# Push the artifact to the local registry using the oras CLI container
docker run --rm \
  --network "$NETWORK" \
  -v "$ARTIFACT_VOL:/workspace" \
  --workdir /workspace \
  ghcr.io/oras-project/oras:v1.3.0 \
  push "${REGISTRY}:5000/test/artifact:v1" \
  --plain-http \
  artifact.tgz:application/vnd.cncf.flux.content.v1.tar+gzip

echo "    Artifact: ${REGISTRY}:5000/test/artifact:v1"

# Pull and unpack via unpacker (--insecure for plain HTTP)
OCI_VOLUME="unpacker-test-$$-${RANDOM}"
docker volume create "$OCI_VOLUME" > /dev/null
docker run --rm -v "$OCI_VOLUME:/out" alpine chmod 777 /out

docker run --rm \
  --network "$NETWORK" \
  -v "$OCI_VOLUME:/out" \
  "$IMAGE" \
  --public \
  --insecure \
  --output-dir /out \
  "${REGISTRY}:5000/test/artifact:v1"

file_count=$(docker run --rm -v "$OCI_VOLUME:/out" alpine sh -c 'find /out/image -type f 2>/dev/null | wc -l' | tr -d ' ')
docker volume rm "$OCI_VOLUME" > /dev/null

if [ "$file_count" -eq 0 ]; then
  fail "Self-created OCI artifact — image/ directory is empty or was not created"
fi
pass "Self-created OCI artifact — $file_count file(s) extracted"
echo ""

# Test 4: Single-file OCI artifact — verify filename and content
echo "==> Test: Single-file OCI artifact (content verification)"

EXPECTED_CONTENT="hello from unpacker content test"
SINGLE_VOL="unpacker-single-$$"
docker volume create "$SINGLE_VOL" > /dev/null

# Create the file
docker run --rm -v "$SINGLE_VOL:/workspace" alpine \
  sh -c "printf '%s' '$EXPECTED_CONTENT' > /workspace/message.txt"

# Push as a plain-file OCI artifact (no tar — exercises CopyFiles path)
docker run --rm \
  --network "$NETWORK" \
  -v "$SINGLE_VOL:/workspace" \
  --workdir /workspace \
  ghcr.io/oras-project/oras:v1.3.0 \
  push "${REGISTRY}:5000/test/single-file:v1" \
  --plain-http \
  message.txt:text/plain

docker volume rm "$SINGLE_VOL" > /dev/null
echo "    Artifact: ${REGISTRY}:5000/test/single-file:v1"

# Pull and unpack
SINGLE_OUT_VOL="unpacker-single-out-$$"
docker volume create "$SINGLE_OUT_VOL" > /dev/null
docker run --rm -v "$SINGLE_OUT_VOL:/out" alpine chmod 777 /out

docker run --rm \
  --network "$NETWORK" \
  -v "$SINGLE_OUT_VOL:/out" \
  "$IMAGE" \
  --public \
  --insecure \
  --output-dir /out \
  "${REGISTRY}:5000/test/single-file:v1"

# Verify filename exists and content matches
actual=$(docker run --rm -v "$SINGLE_OUT_VOL:/out" alpine cat /out/image/message.txt 2>/dev/null)
docker volume rm "$SINGLE_OUT_VOL" > /dev/null

if [ "$actual" != "$EXPECTED_CONTENT" ]; then
  fail "Single-file OCI artifact — content mismatch\n  expected: '$EXPECTED_CONTENT'\n  got:      '$actual'"
fi
pass "Single-file OCI artifact — message.txt content verified: '$actual'"
echo ""

echo "All tests passed."
