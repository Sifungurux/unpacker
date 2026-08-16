#!/usr/bin/env bash
# Integration smoke test for unpacker.
#
# Two suites, each skipped (not failed) when its prerequisites are missing:
#
#   Container suite  — needs a running Docker daemon and the unpacker:dev image.
#                      Covers the published container end to end.
#   Media-type suite — needs go, flux, helm and umoci; no daemon. Pushes real
#                      Flux and Helm artifacts to a local registry and checks
#                      that Unpack() routes each media type the right way.
#
# Usage:
#   ./scripts/test-integration.sh
#   IMAGE=unpacker:latest ./scripts/test-integration.sh
#   REGISTRY_PORT=5200 ./scripts/test-integration.sh

set -euo pipefail

IMAGE="${IMAGE:-unpacker:dev}"
REGISTRY_PORT="${REGISTRY_PORT:-5111}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
skip() { echo "SKIP: $*"; echo ""; }

# Resources cleaned up on exit; each is only set once actually created.
REGISTRY_PID=""
DOCKER_REGISTRY=""
DOCKER_NETWORK=""
DOCKER_ARTIFACT_VOL=""
WORKDIR=""

cleanup() {
  if [ -n "$REGISTRY_PID" ]; then
    { kill "$REGISTRY_PID" && wait "$REGISTRY_PID"; } 2>/dev/null || true
  fi
  if [ -n "$DOCKER_REGISTRY" ]; then docker rm -f "$DOCKER_REGISTRY" >/dev/null 2>&1 || true; fi
  if [ -n "$DOCKER_NETWORK" ]; then docker network rm "$DOCKER_NETWORK" >/dev/null 2>&1 || true; fi
  if [ -n "$DOCKER_ARTIFACT_VOL" ]; then docker volume rm "$DOCKER_ARTIFACT_VOL" >/dev/null 2>&1 || true; fi
  if [ -n "$WORKDIR" ]; then rm -rf "$WORKDIR"; fi
}
trap cleanup EXIT

have() { command -v "$1" >/dev/null 2>&1; }

missing_tools() {
  local missing=""
  for tool in "$@"; do
    have "$tool" || missing="$missing $tool"
  done
  echo "$missing"
}

# ---------------------------------------------------------------------------
# Container suite — pulls through the published image, needs a Docker daemon
# ---------------------------------------------------------------------------

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

container_suite() {
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
  DOCKER_NETWORK="unpacker-net-$$"
  DOCKER_REGISTRY="unpacker-registry-$$"
  DOCKER_ARTIFACT_VOL="unpacker-artifact-$$"

  docker network create "$DOCKER_NETWORK" > /dev/null
  docker volume create "$DOCKER_ARTIFACT_VOL" > /dev/null
  docker run -d --name "$DOCKER_REGISTRY" --network "$DOCKER_NETWORK" registry:2 > /dev/null

  # Wait for registry to be ready
  sleep 1

  # Build a simple tar.gz artifact: two text files
  docker run --rm -v "$DOCKER_ARTIFACT_VOL:/workspace" alpine sh -c "
    mkdir -p /workspace/content && \
    echo 'hello from OCI artifact' > /workspace/content/hello.txt && \
    echo 'second file' > /workspace/content/world.txt && \
    tar czf /workspace/artifact.tgz -C /workspace/content .
  "

  # Push the artifact to the local registry using the oras CLI container
  docker run --rm \
    --network "$DOCKER_NETWORK" \
    -v "$DOCKER_ARTIFACT_VOL:/workspace" \
    --workdir /workspace \
    ghcr.io/oras-project/oras:v1.3.0 \
    push "${DOCKER_REGISTRY}:5000/test/artifact:v1" \
    --plain-http \
    artifact.tgz:application/vnd.cncf.flux.content.v1.tar+gzip

  echo "    Artifact: ${DOCKER_REGISTRY}:5000/test/artifact:v1"

  # Pull and unpack via unpacker (--insecure for plain HTTP)
  local oci_volume="unpacker-test-$$-${RANDOM}"
  docker volume create "$oci_volume" > /dev/null
  docker run --rm -v "$oci_volume:/out" alpine chmod 777 /out

  docker run --rm \
    --network "$DOCKER_NETWORK" \
    -v "$oci_volume:/out" \
    "$IMAGE" \
    --public \
    --insecure \
    --output-dir /out \
    "${DOCKER_REGISTRY}:5000/test/artifact:v1"

  local file_count
  file_count=$(docker run --rm -v "$oci_volume:/out" alpine sh -c 'find /out/image -type f 2>/dev/null | wc -l' | tr -d ' ')
  docker volume rm "$oci_volume" > /dev/null

  if [ "$file_count" -eq 0 ]; then
    fail "Self-created OCI artifact — image/ directory is empty or was not created"
  fi
  pass "Self-created OCI artifact — $file_count file(s) extracted"
  echo ""

  # Test 4: Single-file OCI artifact — verify filename and content
  echo "==> Test: Single-file OCI artifact (content verification)"

  local expected_content="hello from unpacker content test"
  local single_vol="unpacker-single-$$"
  docker volume create "$single_vol" > /dev/null

  # Create the file
  docker run --rm -v "$single_vol:/workspace" alpine \
    sh -c "printf '%s' '$expected_content' > /workspace/message.txt"

  # Push as a plain-file OCI artifact (no tar — exercises CopyFiles path)
  docker run --rm \
    --network "$DOCKER_NETWORK" \
    -v "$single_vol:/workspace" \
    --workdir /workspace \
    ghcr.io/oras-project/oras:v1.3.0 \
    push "${DOCKER_REGISTRY}:5000/test/single-file:v1" \
    --plain-http \
    message.txt:text/plain

  docker volume rm "$single_vol" > /dev/null
  echo "    Artifact: ${DOCKER_REGISTRY}:5000/test/single-file:v1"

  # Pull and unpack
  local single_out_vol="unpacker-single-out-$$"
  docker volume create "$single_out_vol" > /dev/null
  docker run --rm -v "$single_out_vol:/out" alpine chmod 777 /out

  docker run --rm \
    --network "$DOCKER_NETWORK" \
    -v "$single_out_vol:/out" \
    "$IMAGE" \
    --public \
    --insecure \
    --output-dir /out \
    "${DOCKER_REGISTRY}:5000/test/single-file:v1"

  # Verify filename exists and content matches
  local actual
  actual=$(docker run --rm -v "$single_out_vol:/out" alpine cat /out/image/message.txt 2>/dev/null)
  docker volume rm "$single_out_vol" > /dev/null

  if [ "$actual" != "$expected_content" ]; then
    fail "Single-file OCI artifact — content mismatch\n  expected: '$expected_content'\n  got:      '$actual'"
  fi
  pass "Single-file OCI artifact — message.txt content verified: '$actual'"
  echo ""
}

# ---------------------------------------------------------------------------
# Media-type suite — real Flux and Helm artifacts, no Docker daemon
#
# Unpack() picks its extraction path from the first layer's media type: a type
# matching --mediatype is extracted as a tarball, anything else is handed to
# umoci as an OCI image. These tests push artifacts with the media types the
# flux and helm CLIs actually produce, so that routing stays covered by the
# real formats rather than hand-written media type strings.
# ---------------------------------------------------------------------------

FLUX_MEDIATYPE="application/vnd.cncf.flux.content.v1.tar+gzip"
HELM_MEDIATYPE="application/vnd.cncf.helm.chart.content.v1.tar+gzip"

start_local_registry() {
  # Pinned to the go-containerregistry version in go.mod so the registry under
  # test never drifts from the library unpacker links against.
  local version
  version=$(cd "$REPO_ROOT" && go list -m -f '{{.Version}}' github.com/google/go-containerregistry)

  # A leftover registry would serve stale artifacts and make these tests pass
  # for the wrong reason, so refuse to run against an occupied port.
  if curl -sf --max-time 2 "http://localhost:$REGISTRY_PORT/v2/" > /dev/null 2>&1; then
    fail "something is already listening on port $REGISTRY_PORT — stop it or set REGISTRY_PORT"
  fi

  echo "    Starting registry on port $REGISTRY_PORT (go-containerregistry $version)"
  # Installed rather than 'go run': go run spawns the server as a child, so
  # killing it on exit would leave the real process holding the port.
  GOBIN="$WORKDIR" go install "github.com/google/go-containerregistry/cmd/registry@$version"
  "$WORKDIR/registry" -port "$REGISTRY_PORT" > "$WORKDIR/registry.log" 2>&1 &
  REGISTRY_PID=$!

  for _ in $(seq 1 30); do
    if curl -sf "http://localhost:$REGISTRY_PORT/v2/" > /dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "--- registry log ---" >&2
  cat "$WORKDIR/registry.log" >&2
  fail "local registry did not come up on port $REGISTRY_PORT"
}

mediatype_suite() {
  WORKDIR=$(mktemp -d)
  local unpacker="$WORKDIR/unpacker"

  echo "==> Building unpacker from source"
  (cd "$REPO_ROOT" && go build -o "$unpacker" ./cmd/unpacker)

  start_local_registry
  local registry="localhost:$REGISTRY_PORT"
  echo ""

  # Test 5: Flux artifact built by the flux CLI — flux media type, tar extraction
  echo "==> Test: Flux artifact (flux push artifact)"
  mkdir -p "$WORKDIR/manifests"
  cat > "$WORKDIR/manifests/configmap.yaml" <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: unpacker-demo
data:
  key: value
EOF

  flux push artifact "oci://$registry/test/flux-artifact:v1" \
    --path="$WORKDIR/manifests" \
    --source=test \
    --revision="test@sha1:0000000" \
    --insecure-registry > /dev/null

  local flux_out="$WORKDIR/out-flux"
  "$unpacker" --public --insecure --output-dir "$flux_out" "$registry/test/flux-artifact:v1"

  grep -q "$FLUX_MEDIATYPE" "$flux_out/manifest.json" \
    || fail "Flux artifact — manifest.json does not declare $FLUX_MEDIATYPE"
  cmp -s "$WORKDIR/manifests/configmap.yaml" "$flux_out/image/configmap.yaml" \
    || fail "Flux artifact — extracted configmap.yaml differs from the pushed file"
  pass "Flux artifact — $FLUX_MEDIATYPE extracted, content matches"
  echo ""

  # Test 6: Helm chart pushed by the helm CLI — helm media type, tar extraction
  echo "==> Test: Helm chart (helm push)"
  local chart="unpacker-test-chart"
  mkdir -p "$WORKDIR/$chart/templates"
  cat > "$WORKDIR/$chart/Chart.yaml" <<EOF
apiVersion: v2
name: $chart
description: Minimal chart used by unpacker's integration tests
type: application
version: 0.1.0
appVersion: "1.0.0"
EOF
  cat > "$WORKDIR/$chart/templates/configmap.yaml" <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-config
data:
  key: value
EOF

  (cd "$WORKDIR" && helm package "$chart" > /dev/null)
  helm push "$WORKDIR/$chart-0.1.0.tgz" "oci://$registry/charts" --plain-http > /dev/null 2>&1

  local helm_out="$WORKDIR/out-helm"
  "$unpacker" --public --insecure --output-dir "$helm_out" "$registry/charts/$chart:0.1.0"

  grep -q "$HELM_MEDIATYPE" "$helm_out/manifest.json" \
    || fail "Helm chart — manifest.json does not declare $HELM_MEDIATYPE"
  grep -q "name: $chart" "$helm_out/image/$chart/Chart.yaml" \
    || fail "Helm chart — extracted Chart.yaml does not name $chart"
  pass "Helm chart — $HELM_MEDIATYPE extracted, Chart.yaml verified"
  echo ""

  # Test 7: media type outside --mediatype must NOT be tar-extracted.
  # The same flux artifact, pulled with only 'helm' allowed, has to fall
  # through to umoci — which rejects it, since it is not an OCI image layout.
  # Without this the allowlist could silently accept everything.
  echo "==> Test: Media type outside --mediatype is not extracted"
  local reject_out="$WORKDIR/out-reject"
  if "$unpacker" --public --insecure --mediatype helm \
      --output-dir "$reject_out" "$registry/test/flux-artifact:v1" > /dev/null 2>&1; then
    fail "Media type gating — flux artifact was accepted with --mediatype helm"
  fi
  if [ -d "$reject_out/image" ]; then
    fail "Media type gating — image/ was created for a disallowed media type"
  fi
  pass "Media type gating — flux artifact refused when only helm is allowed"
  echo ""
}

# ---------------------------------------------------------------------------

ran_any=0

if ! have docker; then
  skip "Container suite — docker not installed"
elif ! docker info > /dev/null 2>&1; then
  skip "Container suite — docker daemon not reachable (start Docker and re-run)"
else
  container_suite
  ran_any=1
fi

missing=$(missing_tools go flux helm umoci curl)
if [ -n "$missing" ]; then
  skip "Media-type suite — missing:$missing"
else
  mediatype_suite
  ran_any=1
fi

if [ "$ran_any" -eq 0 ]; then
  fail "No suite could run — install Docker, or go + flux + helm + umoci"
fi

echo "All tests passed."
