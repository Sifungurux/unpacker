# unpacker (Go)

CLI to pull and unpack OCI and Docker artifacts (Helm charts, Flux sources, custom artifacts, standard Docker images) from a container registry.

## Architecture

Tries oras-go first for OCI artifacts, falls back to go-containerregistry for standard Docker images, uses umoci for rootless OCI unpacking.

## Commands

```bash
unpacker -o <output-dir> -m <mediatype> -c <dockerconfig.json> IMAGE
unpacker --public --output-dir ./output ghcr.io/stefanprodan/charts/podinfo:6.7.1
```

Flags: `-o/--output-dir`, `-m/--mediatype` (repeatable, default flux+helm), `-c/--config`, `-p/--public`, `-k/--insecure`, `--max-total-bytes` (1GiB), `--max-file-bytes` (512MiB), `--max-entries` (100k).

## Testing

```bash
go test ./...                   # unit tests, in-process registry, no external deps
./scripts/test-integration.sh   # two suites; each skips if its prerequisites are missing
```

`test-integration.sh` uses whichever of docker/podman answers `info` (force with `ENGINE=`). Its container suite runs against the **built image**, so rebuild before running it or you are testing the previous binary: `podman build -t unpacker:dev .`. The media-type suite needs `go`, `flux`, `helm`, `umoci`, `curl` and no daemon.

**After merging a PR, check out the target branch, pull, and run the tests there** — then report the result. A branch that was green on its own can still break `main`: something else landed in between, the rebase resolved differently, or the image the integration suite tests was never rebuilt. Run the integration suite too when the change touches pull or unpack behavior.

## Key Facts

- This is the Go successor to the Python CLI in `../artifact-unpack` (same problem space, different implementation) — confirm with the user which one is the deployed/maintained version before extending either
- `-k/--insecure` skips TLS verification — never default this on, it's meant for local/dev registries only
- Extraction limits apply to the tar path only; `runUmoci` and `CopyFiles` are not bounded by them
