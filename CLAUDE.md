# unpacker (Go)

CLI to pull and unpack OCI and Docker artifacts (Helm charts, Flux sources, custom artifacts, standard Docker images) from a container registry.

## Architecture

Tries oras-go first for OCI artifacts, falls back to go-containerregistry for standard Docker images, uses umoci for rootless OCI unpacking.

## Commands

```bash
unpacker -o <output-dir> -m <mediatype> -c <dockerconfig.json> IMAGE
unpacker --public --output-dir ./output ghcr.io/stefanprodan/charts/podinfo:6.7.1
```

Flags: `-o/--output-dir`, `-m/--mediatype` (repeatable, default flux+helm), `-c/--config`, `-p/--public`, `-k/--insecure`, `--max-total-bytes` (1GiB), `--max-file-bytes` (512MiB), `--max-entries` (100k), `--with-referrers`, `--insecure-allow-credentials`.

## Testing

```bash
go test ./...                   # unit tests, in-process registry, no external deps
./scripts/test-integration.sh   # two suites; each skips if its prerequisites are missing
```

`test-integration.sh` uses whichever of docker/podman answers `info` (force with `ENGINE=`). Its container suite runs against the **built image**, so rebuild before running it or you are testing the previous binary: `podman build -t unpacker:dev .`. The media-type suite needs `go`, `flux`, `helm`, `umoci`, `curl` and no daemon.

**After merging a PR, check out the target branch, pull, and run the tests there** — then report the result. A branch that was green on its own can still break `main`: something else landed in between, the rebase resolved differently, or the image the integration suite tests was never rebuilt. Run the integration suite too when the change touches pull or unpack behavior.

## Output

`<output-dir>/`: `manifest.json` (pulled manifest), `tmp/` (raw blobs or OCI layout), `image/` (unpacked artifact), `result.json` (image ref + resolved digest + referrers, written on every run), and with `--with-referrers` also `referrers/<artifact-type>/<digest>/`.

References may be `repo:tag`, `repo@sha256:...`, or `repo:tag@sha256:...` — oras parses all three, so never re-derive the reference by splitting the image string.

## Key Facts

- This is the Go successor to the Python CLI in `../artifact-unpack` (same problem space, different implementation) — confirm with the user which one is the deployed/maintained version before extending either
- `-k/--insecure` skips TLS verification — never default this on, it's meant for local/dev registries only. It does **not** by itself permit credentials over plain HTTP; that needs `--insecure-allow-credentials`
- Credentials: `UNPACKER_USERNAME`/`UNPACKER_PASSWORD` (the unprefixed `USERNAME`/`PASSWORD` still work but warn — Windows and many CI images set them)
- `--mediatype` matches a full media type exactly, or a bare word against a whole component of it — not a raw substring
- Extraction limits apply to the tar and copy paths; only `runUmoci` is unbounded by them. `--max-total-bytes` is shared across a multi-layer artifact's layers, not applied per layer
- `Unpack` extracts **every** layer whose media type matches, in manifest order (later layers overwrite earlier) — extracting only the first silently loses content
- `Pull` returns the digest **as the registry served it** — for the crane path that is deliberately not the digest of the normalised manifest written to the OCI layout, because referrers attach to the served one
- A registry without the referrers API is a no-op, not an error: oras falls back to the referrers tag schema and an absent tag yields an empty list with no error
