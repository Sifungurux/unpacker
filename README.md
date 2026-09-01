# unpacker

Pull and unpack OCI and Docker artifacts from a container registry.

Supports both OCI artifacts (Helm charts, Flux sources, custom artifacts) and standard Docker images. Tries [oras-go](https://oras.land) first for OCI artifacts, falls back to [go-containerregistry](https://github.com/google/go-containerregistry) for standard Docker images. Uses [umoci](https://github.com/opencontainers/umoci) for rootless OCI image unpacking.

## Usage

```
unpacker [OPTIONS] IMAGE

Arguments:
  IMAGE   Full image reference: registry/repo:tag, registry/repo@sha256:...,
          or registry/repo:tag@sha256:... (the digest wins). Bare
          registry/repo defaults to :latest.

Options:
  -o, --output-dir   string    Output directory (default: .)
  -m, --mediatype    string    Allowed mediatype, repeatable (default: flux, helm)
  -c, --config       string    Path to dockerconfig.json for auth
  -p, --public                 Pull from a public registry (no auth required)
  -k, --insecure               Allow plain HTTP (oras path) / skip TLS verification (crane path)
      --insecure-allow-credentials  Permit credentials over plain HTTP (unencrypted)
      --with-referrers         Download artifacts attached to the image and write result.json
      --max-total-bytes  int    Max total bytes written per artifact (default: 1 GiB)
      --max-file-bytes   int    Max bytes written for one file (default: 512 MiB)
      --max-entries      int    Max entries in an archive (default: 100000)
      --max-referrers    int    Max referrers to download for one image (default: 100)
      --verify-cosign-identity  string  Keyless: regex the Fulcio cert SAN must match
      --verify-cosign-oidc-issuer url   Keyless: required with the above
      --verify-cosign-key       path    Key-based: a cosign public key
      --verify-trusted-root     path    Private cluster: static trusted_root.json
      --verify-tuf-mirror       url     Private cluster: TUF repository
      --verify-tuf-root         path    Private cluster: TUF bootstrap root.json
  -v, --version                Print version
  -h, --help                   Show help
```

### Authentication

Credentials come from `--config <dockerconfig.json>` or the `UNPACKER_USERNAME` / `UNPACKER_PASSWORD` environment variables. The unprefixed `USERNAME` / `PASSWORD` still work but warn: Windows sets `USERNAME` for every login session and many CI images export both, so unpacker could otherwise pick up an unrelated value and send it to a registry.

`--insecure` means two different things depending on which path the manifest routes to: plain HTTP (no TLS at all) on the oras path, and unverified TLS on the crane path. Credentials are **refused over both** — `--insecure` alone will not send a password to a registry that has not proved who it is. Add `--insecure-allow-credentials` when the target really is your own test registry. They are scoped to the registry parsed out of the reference, so they are never offered to another host.

### Media type matching

`--mediatype` accepts either a full media type (`application/vnd.cncf.helm.chart.content.v1.tar+gzip`, matched exactly) or a bare word (`helm`), which matches a whole component of the media type. `helm` covers `application/vnd.cncf.helm.chart.content.v1.tar+gzip` but not `application/vnd.example.nothelm.v1+json`.

### Resolved digest and `result.json`

Every run writes `<output-dir>/result.json` and logs the digest the reference resolved to, so whatever consumes the output records exactly what was analysed rather than a tag that may move:

```json
{ "image": "ghcr.io/myorg/app:v1", "digest": "sha256:b7df…", "referrers": [] }
```

Prefer digest-pinned references (`repo@sha256:...`) in a pipeline. For the crane fallback path the recorded digest is the one **the registry served**, not the digest of the media-type-normalised manifest written to the OCI layout.

### Attached artifacts (`--with-referrers`)

After the pull, unpacker asks the registry's [OCI 1.1 referrers API](https://github.com/opencontainers/distribution-spec/blob/main/spec.md#listing-referrers) what is attached to the digest it resolved — SBOMs, in-toto attestations, cosign signatures — and downloads each one:

```
<output-dir>/
├── referrers/
│   └── <artifact-type>/<referrer-digest>/
│       ├── manifest.json      the referrer's own manifest
│       └── <payload>          each layer, named by its title annotation
└── result.json                what was resolved and what was downloaded
```

```bash
unpacker --public --with-referrers --output-dir ./output ghcr.io/myorg/app:v1
```

```json
{
  "image": "ghcr.io/myorg/app:v1",
  "digest": "sha256:b7df…",
  "referrers": [
    {
      "artifactType": "application/spdx+json",
      "digest": "sha256:09a8…",
      "path": "referrers/application-spdx-json/09a8…",
      "files": ["manifest.json", "sbom.spdx.json"]
    }
  ]
}
```

Registries that predate OCI 1.1 are **a no-op, not a failure**: unpacker logs that nothing was found and still writes `result.json` with an empty list, so a consumer can tell "asked and found none" apart from "never asked". A referrer that *is* advertised but cannot be fetched or fails digest verification **is** fatal — the command exits non-zero after the main artifact has already been unpacked, so treat a non-zero exit with `image/` populated as "the artifact is there, its attachments are not". Artifact types and title annotations come from the registry, so both are reduced to a single safe path element before anything is written.

For the crane fallback path the subject is the digest **as the registry served it**, not the digest of the media-type-normalised manifest written to the OCI layout — referrers hang off the former.

### Extraction limits

The same limits bound the **download** phase as well as extraction: a blob whose declared size is over `--max-file-bytes`, or which would take the pull past `--max-total-bytes`, is rejected before a byte of it is fetched. Without that, a registry declaring a 500 GB layer fills the disk before any limit is consulted. `--max-referrers` caps how many attached artifacts one image may pull.

A `.tar.gz` says nothing trustworthy about how far it expands, so when an artifact is extracted as a tarball the extraction is bounded by what is actually written to disk rather than by the sizes the archive declares. `--max-total-bytes` covers the whole artifact — a multi-layer artifact shares one budget across its layers rather than getting the limit per layer. The same three limits bound the plain-file copy path; only `umoci` (which does its own unpacking) is outside them. (These limits apply to the tar and file-copy paths; `umoci` does its own unpacking and is not covered by them.) Exceeding any limit fails the run with an error naming the flag involved, and setuid/setgid/sticky bits are stripped from every extracted file so an archive cannot plant a privileged binary. Raise a limit when an artifact is legitimately larger:

```bash
unpacker --public --max-total-bytes $((4 * 1024 * 1024 * 1024)) --output-dir ./output ghcr.io/myorg/big-artifact:v1
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

`image/` appears only when the unpack has fully succeeded — extraction happens in a staging directory that is renamed into place at the end, so a failed run leaves no half-extracted tree to be mistaken for a complete artifact. `tmp/` is left behind either way, for debugging.

### Layer compression

Layers may be gzip (`+gzip`), zstd (`+zstd`) or uncompressed tar. The compression is detected from the bytes rather than from the media type, so a mislabelled layer still extracts, and anything else fails with an error naming what was actually found rather than blaming gzip.

### Multi-layer artifacts

Every layer whose media type matches `--mediatype` is extracted into `image/` in manifest order, so a later layer overwrites an earlier one exactly as an image would. Layers whose media type does not match are left in `tmp/` — they are not tarballs this path knows how to read.

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

```bash
docker build -t unpacker:dev .   # or: podman build -t unpacker:dev .
./scripts/test-integration.sh
```

The image cross-compiles the binary and selects the matching `umoci` release for `TARGETARCH`, so `--platform linux/amd64` and `--platform linux/arm64` both produce a working image. Adding an architecture means adding its pinned umoci hash to the Dockerfile — the build fails loudly rather than installing the wrong binary.

The script runs two suites. Each is **skipped, not failed**, when its prerequisites are missing, so it is useful on a machine with no container engine at all.

**Container suite** — needs docker or podman (whichever daemon actually answers; override with `ENGINE=podman`) and the `unpacker:dev` image. Exercises the published container end to end:

| Test | What it covers |
|---|---|
| Helm OCI chart (public) | oras pull → tar extraction |
| Docker image (alpine) | crane fallback → umoci unpack |
| Self-created OCI artifact (local registry, plain HTTP) | oras + `--insecure` → tar extraction |
| Single-file OCI artifact with content verification | oras → file copy, exact content check |

These spin up a `registry:2` container and push with the oras CLI container (`ghcr.io/oras-project/oras:v1.3.0`).

**Media-type suite** — needs `go`, `flux`, `helm`, `umoci` and `curl`; no daemon. Builds `unpacker` from source and pushes artifacts produced by the real CLIs to a local registry (`go-containerregistry`'s `cmd/registry`, pinned to the version in `go.mod`), covering how `Unpack()` routes each media type:

| Test | What it covers |
|---|---|
| Flux artifact (`flux push artifact`) | `application/vnd.cncf.flux.content.v1.tar+gzip` → tar extraction, extracted file compared byte-for-byte with the pushed one |
| Helm chart (`helm push`) | `application/vnd.cncf.helm.chart.content.v1.tar+gzip` → tar extraction, `Chart.yaml` verified |
| Media type outside `--mediatype` | the same Flux artifact pulled with only `helm` allowed must fall through to umoci and be **refused**, leaving no `image/` — proves the allowlist gates rather than passes everything |

Everything is cleaned up on exit. Both suites are configurable:

```bash
IMAGE=unpacker:latest ./scripts/test-integration.sh   # container suite image
ENGINE=podman ./scripts/test-integration.sh           # force a container engine
REGISTRY_PORT=5200 ./scripts/test-integration.sh      # media-type suite registry port
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

## Signature verification

Verification is a precondition of unpacking, not a step after it: a refused
signature means no `image/` directory is ever published.

Requesting verification implies `--with-referrers`: the signature *is* a
referrer, so it has to be fetched, and the bundle a run was accepted on is kept
next to the artifact it vouches for. Referrers are now also fetched **before**
the unpack rather than after it, so a referrer that fails its subject check now
fails the run before `image/` is published rather than after.

```bash
# public Sigstore, keyless
unpacker --verify-cosign-identity '^https://github\.com/myorg/.*' \
         --verify-cosign-oidc-issuer https://token.actions.githubusercontent.com \
         -o ./out ghcr.io/myorg/app:v1

# private cluster, static trust material
unpacker --verify-trusted-root ./trusted_root.json \
         --verify-cosign-identity '^https://gitlab.corp/myorg/.*' \
         --verify-cosign-oidc-issuer https://gitlab.corp \
         -o ./out registry.corp/app:v1

# private cluster, TUF
unpacker --verify-tuf-mirror https://tuf.corp --verify-tuf-root ./root.json ...

# a plain cosign key, no Fulcio and no cluster
unpacker --verify-cosign-key ./cosign.pub -o ./out registry.corp/app:v1
```

Signatures are discovered **through the OCI 1.1 referrers API only** — what
`cosign sign --registry-referrers-mode oci-1-1` attaches. cosign's legacy
`sha256-<hex>.sig` tag scheme is not consulted, so an artifact signed that way
reads as unsigned and the run fails rather than passing.

### What gets recorded

`result.json` gains a `verification` object naming the mode, the identity or key,
the trust source, the bundle digests checked, and where the timestamp came from.
Its absence means verification was never requested — that distinction is
deliberate and unambiguous.

`timestampSource` is worth reading. **Rekor v2 issues no inclusion promise**, so
against a Rekor-v2 cluster with no timestamp authority there is no signed
attestation of *when* a signature was made. Log inclusion is still fully
verified; only the time is unproven, and the field says `none-rekor-v2` rather
than implying the log vouched for it. Add a timestamp authority to the cluster
if you need that gap closed.

A trusted root with neither a Rekor log nor a timestamp authority is refused
outright: nothing would attest to the signature at all.
