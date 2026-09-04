#!/usr/bin/env bash
# Integration smoke test for unpacker.
#
# Two suites. By default each is skipped (not failed) when its prerequisites are
# missing; naming one with SUITES= makes its prerequisites mandatory instead.
#
#   Container suite  — needs docker or podman plus the unpacker:dev image.
#                      Covers the published container end to end.
#   Media-type suite — needs go, flux, helm and umoci; no daemon. Pushes real
#                      Flux and Helm artifacts to a local registry and checks
#                      that Unpack() routes each media type the right way.
#
# Usage:
#   ./scripts/test-integration.sh
#   IMAGE=unpacker:latest ./scripts/test-integration.sh
#   REGISTRY_PORT=5200 ./scripts/test-integration.sh
#   ENGINE=podman ./scripts/test-integration.sh
#   SUITES=mediatype ./scripts/test-integration.sh   # and fail if it cannot run

set -euo pipefail

IMAGE="${IMAGE:-unpacker:dev}"
REGISTRY_PORT="${REGISTRY_PORT:-5111}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# all | container | mediatype. Naming one makes its prerequisites mandatory
# rather than a reason to skip — see the dispatcher at the bottom.
SUITES="${SUITES:-all}"

# Engine the caller asked for, if any; the one actually in use is resolved below.
ENGINE_REQUESTED="${ENGINE:-}"
ENGINE=""

# Fully qualified so podman never has to resolve a short name (it may prompt,
# or resolve against a different registry than docker would).
ALPINE_IMAGE="docker.io/library/alpine:3.21"
REGISTRY_IMAGE="docker.io/library/registry:2"
ORAS_IMAGE="ghcr.io/oras-project/oras:v1.3.0"

pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
skip() { echo "SKIP: $*"; echo ""; }

# Resources cleaned up on exit; each is only set once actually created.
REGISTRY_PID=""
CTR_REGISTRY=""
CTR_NETWORK=""
CTR_ARTIFACT_VOL=""
WORKDIR=""

cleanup() {
  if [ -n "$REGISTRY_PID" ]; then
    { kill "$REGISTRY_PID" && wait "$REGISTRY_PID"; } 2>/dev/null || true
  fi
  if [ -n "$ENGINE" ] && [ -n "$CTR_REGISTRY" ]; then "$ENGINE" rm -f "$CTR_REGISTRY" >/dev/null 2>&1 || true; fi
  if [ -n "$ENGINE" ] && [ -n "$CTR_NETWORK" ]; then "$ENGINE" network rm "$CTR_NETWORK" >/dev/null 2>&1 || true; fi
  if [ -n "$ENGINE" ] && [ -n "$CTR_ARTIFACT_VOL" ]; then "$ENGINE" volume rm "$CTR_ARTIFACT_VOL" >/dev/null 2>&1 || true; fi
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
# Container suite — pulls through the published image, needs docker or podman
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
  "$ENGINE" volume create "$volume" > /dev/null
  # Volume is owned by root by default — open it up for the non-root container user
  "$ENGINE" run --rm -v "$volume:/out" "$ALPINE_IMAGE" chmod 777 /out

  "$ENGINE" run --rm \
    -v "$volume:/out" \
    "$IMAGE" \
    --public \
    --output-dir /out \
    ${extra_flags[@]+"${extra_flags[@]}"} \
    "$artifact"

  local file_count
  file_count=$("$ENGINE" run --rm -v "$volume:/out" "$ALPINE_IMAGE" sh -c 'find /out/image -type f 2>/dev/null | wc -l' | tr -d ' ')

  "$ENGINE" volume rm "$volume" > /dev/null

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
  CTR_NETWORK="unpacker-net-$$"
  CTR_REGISTRY="unpacker-registry-$$"
  CTR_ARTIFACT_VOL="unpacker-artifact-$$"

  "$ENGINE" network create "$CTR_NETWORK" > /dev/null
  "$ENGINE" volume create "$CTR_ARTIFACT_VOL" > /dev/null
  "$ENGINE" run -d --name "$CTR_REGISTRY" --network "$CTR_NETWORK" "$REGISTRY_IMAGE" > /dev/null

  # Wait for registry to be ready
  sleep 1

  # Build a simple tar.gz artifact: two text files
  "$ENGINE" run --rm -v "$CTR_ARTIFACT_VOL:/workspace" "$ALPINE_IMAGE" sh -c "
    mkdir -p /workspace/content && \
    echo 'hello from OCI artifact' > /workspace/content/hello.txt && \
    echo 'second file' > /workspace/content/world.txt && \
    tar czf /workspace/artifact.tgz -C /workspace/content .
  "

  # Push the artifact to the local registry using the oras CLI container
  "$ENGINE" run --rm \
    --network "$CTR_NETWORK" \
    -v "$CTR_ARTIFACT_VOL:/workspace" \
    --workdir /workspace \
    "$ORAS_IMAGE" \
    push "${CTR_REGISTRY}:5000/test/artifact:v1" \
    --plain-http \
    artifact.tgz:application/vnd.cncf.flux.content.v1.tar+gzip

  echo "    Artifact: ${CTR_REGISTRY}:5000/test/artifact:v1"

  # Pull and unpack via unpacker (--insecure for plain HTTP)
  local oci_volume="unpacker-test-$$-${RANDOM}"
  "$ENGINE" volume create "$oci_volume" > /dev/null
  "$ENGINE" run --rm -v "$oci_volume:/out" "$ALPINE_IMAGE" chmod 777 /out

  "$ENGINE" run --rm \
    --network "$CTR_NETWORK" \
    -v "$oci_volume:/out" \
    "$IMAGE" \
    --public \
    --insecure \
    --output-dir /out \
    "${CTR_REGISTRY}:5000/test/artifact:v1"

  local file_count
  file_count=$("$ENGINE" run --rm -v "$oci_volume:/out" "$ALPINE_IMAGE" sh -c 'find /out/image -type f 2>/dev/null | wc -l' | tr -d ' ')
  "$ENGINE" volume rm "$oci_volume" > /dev/null

  if [ "$file_count" -eq 0 ]; then
    fail "Self-created OCI artifact — image/ directory is empty or was not created"
  fi
  pass "Self-created OCI artifact — $file_count file(s) extracted"
  echo ""

  # Test 4: Single-file OCI artifact — verify filename and content
  echo "==> Test: Single-file OCI artifact (content verification)"

  local expected_content="hello from unpacker content test"
  local single_vol="unpacker-single-$$"
  "$ENGINE" volume create "$single_vol" > /dev/null

  # Create the file
  "$ENGINE" run --rm -v "$single_vol:/workspace" "$ALPINE_IMAGE" \
    sh -c "printf '%s' '$expected_content' > /workspace/message.txt"

  # Push as a plain-file OCI artifact (no tar — exercises CopyFiles path)
  "$ENGINE" run --rm \
    --network "$CTR_NETWORK" \
    -v "$single_vol:/workspace" \
    --workdir /workspace \
    "$ORAS_IMAGE" \
    push "${CTR_REGISTRY}:5000/test/single-file:v1" \
    --plain-http \
    message.txt:text/plain

  "$ENGINE" volume rm "$single_vol" > /dev/null
  echo "    Artifact: ${CTR_REGISTRY}:5000/test/single-file:v1"

  # Pull and unpack
  local single_out_vol="unpacker-single-out-$$"
  "$ENGINE" volume create "$single_out_vol" > /dev/null
  "$ENGINE" run --rm -v "$single_out_vol:/out" "$ALPINE_IMAGE" chmod 777 /out

  "$ENGINE" run --rm \
    --network "$CTR_NETWORK" \
    -v "$single_out_vol:/out" \
    "$IMAGE" \
    --public \
    --insecure \
    --output-dir /out \
    "${CTR_REGISTRY}:5000/test/single-file:v1"

  # Verify filename exists and content matches
  local actual
  actual=$("$ENGINE" run --rm -v "$single_out_vol:/out" "$ALPINE_IMAGE" cat /out/image/message.txt 2>/dev/null)
  "$ENGINE" volume rm "$single_out_vol" > /dev/null

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

  # Test 7: every matching layer must be extracted, not just the first.
  # A multi-blob artifact used to lose everything after layer one, silently
  # and with a zero exit code.
  echo "==> Test: Multi-layer artifact extracts every layer"
  mkdir -p "$WORKDIR/l1" "$WORKDIR/l2"
  echo "from layer one" > "$WORKDIR/l1/first.txt"
  echo "from layer two" > "$WORKDIR/l2/second.txt"
  (cd "$WORKDIR/l1" && tar czf ../layer1.tgz .)
  (cd "$WORKDIR/l2" && tar czf ../layer2.tgz .)

  (cd "$WORKDIR" && oras push "$registry/test/multi:v1" --plain-http \
    "layer1.tgz:$FLUX_MEDIATYPE" \
    "layer2.tgz:$FLUX_MEDIATYPE" > /dev/null)

  local multi_out="$WORKDIR/out-multi"
  "$unpacker" --public --insecure --output-dir "$multi_out" "$registry/test/multi:v1"

  for f in first.txt second.txt; do
    [ -f "$multi_out/image/$f" ] || fail "Multi-layer artifact — $f missing from image/"
  done
  grep -q "from layer two" "$multi_out/image/second.txt" \
    || fail "Multi-layer artifact — second layer content wrong"
  pass "Multi-layer artifact — both layers extracted"
  echo ""

  # Test 8: media type outside --mediatype must NOT be tar-extracted.
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

# Pick a container engine: whatever ENGINE names, else the first one whose
# daemon actually answers. Having the docker CLI installed proves nothing —
# on a podman host it is usually present but pointed at a socket that is not
# there.
pick_engine() {
  local candidate
  for candidate in ${ENGINE_REQUESTED:-docker podman}; do
    if have "$candidate" && "$candidate" info > /dev/null 2>&1; then
      ENGINE="$candidate"
      return 0
    fi
  done
  return 1
}

ran_any=0

# Naming a suite makes its prerequisites mandatory. Skipping is the right
# default on a laptop, where you may have podman but not flux; it is wrong in
# CI, where a skipped suite and a passing one look identical in a green tick.
# So: SUITES=mediatype means "run that suite or fail trying".
want() { [ "$SUITES" = "all" ] || [ "$SUITES" = "$1" ]; }
required() { [ "$SUITES" != "all" ]; }

case "$SUITES" in
  all|container|mediatype) ;;
  *) fail "SUITES must be one of: all, container, mediatype (got '$SUITES')" ;;
esac

if want container; then
  if pick_engine; then
    echo "Container engine: $ENGINE"
    container_suite
    ran_any=1
  elif required; then
    fail "Container suite — no usable container engine, and SUITES=$SUITES requires it"
  elif [ -n "$ENGINE_REQUESTED" ]; then
    skip "Container suite — '$ENGINE_REQUESTED' is not usable (is it installed and running?)"
  else
    skip "Container suite — no usable container engine (start Docker or podman)"
  fi
fi

if want mediatype; then
  missing=$(missing_tools go flux helm umoci oras curl)
  if [ -z "$missing" ]; then
    mediatype_suite
    ran_any=1
  elif required; then
    fail "Media-type suite — missing:$missing, and SUITES=$SUITES requires it"
  else
    skip "Media-type suite — missing:$missing"
  fi
fi

if [ "$ran_any" -eq 0 ]; then
  fail "No suite could run — start docker or podman, or install go + flux + helm + umoci"
fi

echo "All tests passed."
