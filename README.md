# unpacker

Pull and unpack OCI and Docker artifacts from a container registry.

Supports both OCI artifacts (Helm charts, Flux sources) and standard Docker images. Tries [oras-go](https://oras.land) first, falls back to [go-containerregistry](https://github.com/google/go-containerregistry) for Docker images that lack OCI metadata. Uses [umoci](https://github.com/opencontainers/umoci) for rootless OCI image unpacking.

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
  -k, --insecure               Skip TLS verification (self-signed certs)
  -h, --help                   Show help
```

### Examples

Pull a public OCI artifact:
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

Pull from a registry with a self-signed certificate:
```bash
unpacker --insecure --config ~/.docker/config.json --output-dir ./output myregistry.internal/myimage:latest
```

Pull with a custom allowed mediatype:
```bash
unpacker --public -m kustomize -m helm --output-dir ./output ghcr.io/myorg/myartifact:latest
```

## Output Structure

After a successful pull and unpack:

```
<output-dir>/
├── tmp/          Raw pulled content (OCI layout or file blobs)
├── image/        Unpacked artifact contents
└── manifest.json Registry manifest for the pulled image
```

## Auth

Authentication is resolved in this order:

1. `--public` flag — no credentials, plain pull
2. `--config` flag — path to a `dockerconfig.json` (standard Docker auth format)
3. `USERNAME` + `PASSWORD` environment variables — basic auth
4. None of the above — error exit for private registries

## How It Works

### Pull

**Stage 1 — oras-go (OCI artifacts)**
Attempts to pull as an OCI artifact using oras-go. Writes blobs to `tmp/` and `manifest.json`. Used for Helm charts, Flux sources, and other ORAS-compatible artifacts.

**Stage 2 — go-containerregistry (Docker images)**
If Stage 1 fails (e.g. standard Docker image without OCI metadata), falls back to go-containerregistry. Writes an OCI image layout to `tmp/`. Replaces the original skopeo dependency entirely.

> **Note on `--insecure`:** The insecure flag applies to the crane (Stage 2) transport via `InsecureSkipVerify`. Plain-HTTP and self-signed registries will fail Stage 1 (oras) and be handled cleanly by the Stage 2 fallback. This is by design.

### Unpack

Reads `manifest.json` to determine the mediaType of the first layer, then selects one of three paths:

| Path | Condition | Tool |
|---|---|---|
| Tar extraction | mediaType matches allowed list | stdlib `archive/tar` |
| OCI image unpack | OCI layout present (blobs dir or index.json) | `umoci` binary |
| File copy | Plain files, no tarball detected | stdlib `io.Copy` |

## Requirements

### Runtime
- `umoci` — must be on `$PATH`. Install from [GitHub releases](https://github.com/opencontainers/umoci/releases).

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

The Dockerfile is a two-stage build:
- **Builder**: `golang:1.24-alpine` — compiles a static binary
- **Runtime**: `alpine:3.21` — downloads umoci from GitHub releases (SHA-256 verified), runs as non-root user `unpacker`

## Running Tests

```bash
go test ./...
```

Tests use an in-process OCI registry (no external dependencies required). umoci is not tested directly — its path is exercised by the unpack dispatch logic, which is integration-tested separately.

## Dependencies

| Package | Purpose |
|---|---|
| `github.com/spf13/cobra` | CLI framework |
| `oras.land/oras-go/v2` | OCI artifact pull (Stage 1) |
| `github.com/google/go-containerregistry` | Docker image pull fallback (Stage 2) |
| `umoci` (external binary) | Rootless OCI image unpacking |

## Project Layout

```
unpacker/
├── cmd/unpacker/main.go          Cobra CLI entry point
├── internal/unpacker/
│   ├── auth.go                   Credential resolution
│   ├── pull.go                   Pull logic (oras + crane fallback)
│   └── unpack.go                 Unpack logic (tar / umoci / copy) + Config struct
├── Dockerfile
├── .dockerignore
└── docs/superpowers/
    ├── specs/                    Design document
    └── plans/                    Implementation plan
```

## Pipeline

The project builds as an OCI artifact container using Azure DevOps (`.pipelines/build-release.yaml`). The pipeline builds the container image and updates the allowlist with the new digest.
