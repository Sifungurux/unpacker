# Unpacker — Go Rewrite Design

**Date:** 2026-04-09
**Status:** Approved

## Overview

Rewrite of the Python `artifact-unpack` tool in Go. Pulls OCI and Docker artifacts from a container registry and unpacks them to a local directory. Replaces the `skopeo` subprocess with `go-containerregistry`. Retains `umoci` as an external binary dependency for OCI image unpacking.

## CLI

Single binary, single command.

```
unpacker [OPTIONS] IMAGE

Options:
  -o, --output-dir   string    Output directory (default: .)
  -m, --mediatype    string    Allowed mediatype, repeatable (default: flux, helm)
  -c, --config       string    Path to dockerconfig.json for auth
  -p, --public                 Pull from a public registry (no auth required)
  -k, --insecure               Skip TLS verification (for self-signed certs)
```

Auth credentials for private registries are read from `USERNAME` and `PASSWORD` environment variables. If neither `--config` nor env vars are set and `--public` is not passed, the CLI exits with a clear error.

## Project Layout

```
unpacker/
├── cmd/unpacker/main.go          Entry point, Cobra CLI setup
├── internal/unpacker/
│   ├── auth.go                   Registry auth helpers (env vars, docker config)
│   ├── pull.go                   Pull logic: oras-go with go-containerregistry fallback
│   └── unpack.go                 Unpack logic: tar extraction + umoci exec
├── docs/superpowers/specs/       Design documents
├── go.mod
└── Dockerfile
```

## Architecture & Data Flow

```
CLI (cobra)
  └── unpacker.Run(cfg)
        ├── auth.Resolve(cfg)          → credentials
        ├── pull.Pull(cfg, creds)
        │     ├── oras.Pull()          → <output-dir>/tmp/ + manifest.json   [Stage 1]
        │     └── crane.Copy()         → <output-dir>/tmp/                   [Stage 2 fallback]
        └── unpack.Unpack(cfg)
              ├── Path 1: tar extract  → <output-dir>/image/   (ORAS artifact)
              ├── Path 2: umoci exec   → <output-dir>/image/   (OCI image)
              └── Path 3: file copy    → <output-dir>/image/   (plain files)
```

## Dependencies

| Package | Purpose |
|---|---|
| `github.com/spf13/cobra` | CLI framework |
| `oras.land/oras-go/v2` | OCI artifact pull (Stage 1) |
| `github.com/google/go-containerregistry` | Docker image pull fallback (Stage 2) |
| `umoci` (external binary) | Rootless OCI image unpacking (Path 2) |

## Pull Logic

**Stage 1 — oras-go**
Attempts to pull as an OCI artifact. On success, writes files to `<output-dir>/tmp/` and writes `manifest.json` containing the manifest response. The `--insecure` flag is applied via `oras.NewClient(oras.WithInsecureConnections())`.

**Stage 2 — go-containerregistry fallback**
If Stage 1 fails (e.g. Docker artifact lacking OCI metadata), uses `crane.Copy()` to copy `docker://<image>` → `oci:<output-dir>/tmp:latest`. The `--insecure` flag is applied via a custom `http.Transport` with `TLSClientConfig.InsecureSkipVerify = true`.

## Unpack Logic

Reads `manifest.json` after pull to determine mediaType and digest of the first layer.

**Path 1 — ORAS artifact** (mediaType matches allowed list)
Rename blob by digest, extract tarball to `<output-dir>/image/` using stdlib `archive/tar` and `compress/gzip`. No external tools.

**Path 2 — OCI image** (fallback pull used, or mediaType not in allowed list)
Execute `umoci --log error raw unpack --rootless --image <output-dir>/tmp <output-dir>/image` via `os/exec` with a proper argument slice (no `shell=true`).

**Path 3 — Plain files** (no tarball detected, no blobs directory)
Copy all files from `<output-dir>/tmp/` to `<output-dir>/image/` using `io.Copy`.

## Auth

Resolved in order:
1. `--public` flag → no credentials
2. `--config` flag → docker config file passed to both oras and crane
3. `USERNAME` + `PASSWORD` env vars → basic auth on both clients
4. None of the above + private registry → error, exit non-zero

## Error Handling

- All errors are returned and propagated to the CLI layer
- CLI prints error and exits non-zero
- No silent failures or empty fallbacks
- `umoci` stderr is captured and logged on failure

## TLS / Insecure

The `--insecure` flag (`-k`) threads through to:
- oras client: `WithInsecureConnections()`
- go-containerregistry transport: `InsecureSkipVerify: true`
- umoci: `--insecure` flag passed to the exec command if set
