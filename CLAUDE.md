# unpacker (Go)

CLI to pull and unpack OCI and Docker artifacts (Helm charts, Flux sources, custom artifacts, standard Docker images) from a container registry.

## Architecture

Tries oras-go first for OCI artifacts, falls back to go-containerregistry for standard Docker images, uses umoci for rootless OCI unpacking.

## Commands

```bash
unpacker -o <output-dir> -m <mediatype> -c <dockerconfig.json> IMAGE
unpacker --public --output-dir ./output ghcr.io/stefanprodan/charts/podinfo:6.7.1
```

Flags: `-o/--output-dir`, `-m/--mediatype` (repeatable, default flux+helm), `-c/--config`, `-p/--public`, `-k/--insecure`.

## Key Facts

- This is the Go successor to the Python CLI in `../artifact-unpack` (same problem space, different implementation) — confirm with the user which one is the deployed/maintained version before extending either
- `-k/--insecure` skips TLS verification — never default this on, it's meant for local/dev registries only
