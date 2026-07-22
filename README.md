# unpacker

Pull and unpack OCI and Docker artifacts from a container registry.

Supports both OCI artifacts (Helm charts, Flux sources, custom artifacts) and standard Docker images. Tries [oras-go](https://oras.land) first for OCI artifacts, falls back to [go-containerregistry](https://github.com/google/go-containerregistry) for standard Docker images. Uses [umoci](https://github.com/opencontainers/umoci) for rootless OCI image unpacking.

## Usage

```
unpacker [OPTIONS] IMAGE

Arguments:
  IMAGE   Full image reference (registry/repo:tag)

Options:
  -o, --output-dir   string    Output directory (default: .)
  -m, --mediatype    string    Allowed mediatype, repeatable (default: flux, helm)
  -c, --config       string    Path to dockerconfig.json for auth
  -p, --public                 Pull from a public registry (no auth required)
  -k, --insecure               Skip TLS verification / allow plain HTTP registries
  -v, --version                Print version
  -h, --help                   Show help
```

### Examples

Pull a public Helm OCI chart:
```bash
unpacker --public --output-dir ./output ghcr.io/stefanprodan/charts/podinfo:6.7.1
```

Pull a public Flux OCI source:
```bash
unpacker --public --output-dir ./output ghcr.io/fluxcd/flux-manifests:v2.0.0
```

Pull from a private registry using a docker config:
```bash
unpacker --config ~/.docker/config.json --output-dir ./output myregistry.example.com/myimage:latest
```

Pull from a private registry using environment variables:
```bash
export USERNAME=myuser
export PASSWORD=mypassword
unpacker --output-dir ./output myregistry.example.com/myimage:latest
```

Pull from a registry with a self-signed cert or plain HTTP (e.g. local registry):
```bash
unpacker --insecure --public --output-dir ./output localhost:5000/myartifact:latest
```

Pull with a custom allowed mediatype:
```bash
unpacker --public -m kustomize -m helm --output-dir ./output ghcr.io/myorg/myartifact:latest
```

## Output Structure

After a successful pull and unpack:

```
<output-dir>/
├── tmp/          Raw pulled content (blobs or OCI layout)
├── image/        Unpacked artifact contents
└── manifest.json Registry manifest for the pulled image
```

## Auth

Authentication is resolved in this order:

1. `--public` flag — no credentials, plain pull
2. `--config` flag — path to a `dockerconfig.json` (standard Docker auth format)
3. `USERNAME` + `PASSWORD` environment variables — basic auth
4. None of the above — error exit

## How It Works

### Pull

**Stage 1 — oras-go (OCI artifacts)**

Fetches the manifest directly from the registry and downloads each layer blob to `tmp/`. Used for OCI artifacts: Helm charts, Flux sources, or any artifact pushed with oras. Blobs are stored by their annotated filename if present (`org.opencontainers.image.title`), otherwise by hex digest.

When `--insecure` is set, plain-HTTP registries are allowed at this stage (`PlainHTTP = true`).

**Stage 2 — go-containerregistry / crane (Docker images)**

If Stage 1 detects a Docker manifest type (`application/vnd.docker.*`), it returns early and crane handles the pull. Crane writes a fully-tagged OCI image layout to `tmp/` so umoci can unpack it. The `--insecure` flag enables `InsecureSkipVerify` on the crane transport for self-signed certificates.

### Unpack

Reads `manifest.json` to determine the mediaType of the first layer, then selects one of three paths:

| Path | Condition | Tool |
|---|---|---|
| Tar extraction | mediaType matches allowed list and a tar blob is on disk | stdlib `archive/tar` |
| OCI image unpack | OCI layout present (`blobs/sha256/` directory) | `umoci` binary |
| File copy | Plain files, no tar or blobs detected | stdlib `io.Copy` |

## Requirements

### Runtime
- `umoci` — must be on `$PATH` for Docker image unpacking. Not required for OCI artifact extraction or file copy. Install from [GitHub releases](https://github.com/opencontainers/umoci/releases).

### Build
- Go 1.25+
- Docker (for container image builds)

## Building

### Binary

```bash
go build -o unpacker ./cmd/unpacker
```

### Container image

```bash
docker build -t unpacker:latest .
```

The Dockerfile uses a two-stage build:
- **Builder**: `golang:1.25-alpine` — compiles a static binary
- **Runtime**: `alpine:3.21` — downloads and SHA-256 verifies umoci from its GitHub release, runs as non-root user `unpacker`

The umoci checksum is verified against the release's own `umoci.sha256sum` file. To upgrade umoci, change `UMOCI_VERSION` in the Dockerfile — no separate hash update needed.

### Releasing

Pushing a tag matching `v*` (e.g. `v0.2.0`) triggers `.github/workflows/release.yml`, which runs the test suite and then [goreleaser](https://goreleaser.com) (config in `.goreleaser.yaml`) to build binaries for linux/darwin, amd64/arm64, and publish them as a GitHub Release with checksums and a changelog. `unpacker --version` reports the tag it was built from.

To dry-run the release build locally:

```bash
goreleaser release --snapshot --clean
```

## Testing

### Unit tests

```bash
go test ./...
```

Unit tests use an in-process OCI registry. No external dependencies required.

### Integration tests

Requires Docker with the `unpacker:dev` image already built.

```bash
docker build -t unpacker:dev .
./scripts/test-integration.sh
```

The integration test suite runs four scenarios:

| Test | What it covers |
|---|---|
| Helm OCI chart (public) | oras pull → tar extraction |
| Docker image (alpine) | crane fallback → umoci unpack |
| Self-created OCI artifact (local registry, plain HTTP) | oras + `--insecure` → tar extraction |
| Single-file OCI artifact with content verification | oras → file copy, exact content check |

The local registry tests spin up a `registry:2` container and push artifacts using the oras CLI container (`ghcr.io/oras-project/oras:v1.3.0`). Everything is cleaned up on exit.

To test a different image tag:
```bash
IMAGE=unpacker:latest ./scripts/test-integration.sh
```

## Dependencies

| Package | Purpose |
|---|---|
| `github.com/spf13/cobra` | CLI framework |
| `oras.land/oras-go/v2` | OCI artifact pull (Stage 1) |
| `github.com/google/go-containerregistry` | Docker image pull fallback (Stage 2) |
| `github.com/opencontainers/image-spec` | OCI descriptor types |
| `github.com/opencontainers/go-digest` | Digest parsing |
| `umoci` (external binary) | Rootless OCI image unpacking |

## Project Layout

```
unpacker/
├── cmd/unpacker/main.go          Cobra CLI entry point
├── internal/unpacker/
│   ├── auth.go                   Credential resolution
│   ├── pull.go                   Pull logic (oras + crane fallback)
│   └── unpack.go                 Unpack logic (tar / umoci / copy) + Config struct
├── scripts/
│   └── test-integration.sh       Integration test suite
├── Dockerfile
├── .dockerignore
└── docs/superpowers/
    ├── specs/                    Design document
    └── plans/                    Implementation plan
```
