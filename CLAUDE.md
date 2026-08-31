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
- `-k/--insecure` means plain HTTP on the oras path and unverified TLS on the crane path — two different things, one flag. Never default this on, it's meant for local/dev registries only. It does **not** by itself permit credentials over plain HTTP; that needs `--insecure-allow-credentials`
- Credentials: `UNPACKER_USERNAME`/`UNPACKER_PASSWORD` (the unprefixed `USERNAME`/`PASSWORD` still work but warn — Windows and many CI images set them)
- `--mediatype` matches a full media type exactly, or a bare word against a whole component of it — not a raw substring
- Layer compression (gzip / zstd / uncompressed tar) is detected from the bytes, not the media type suffix
- The limits bound the **download** phase too: a blob is rejected on its declared size before it is fetched (`downloadBudget`). That trusts `desc.Size` where extraction deliberately distrusts `hdr.Size` — fine, because under-declaring still fails `VerifyReader`. Extraction limits apply to the tar and copy paths; only `runUmoci` is unbounded by them. `--max-total-bytes` is shared across a multi-layer artifact's layers, not applied per layer
- `Unpack` extracts **every** layer whose media type matches, in manifest order (later layers overwrite earlier) — extracting only the first silently loses content
- `Pull` returns the digest **as the registry served it** — for the crane path that is deliberately not the digest of the normalised manifest written to the OCI layout, because referrers attach to the served one
- A referrer whose manifest names a different `subject` is rejected: digest verification proves the registry served what it advertised, not that the artifact is about this image. One with no subject at all is skipped with a warning rather than rejected, so a registry whose fallback listing omits the field does not break `--with-referrers`
- Extraction creates directories and regular files only. Links, devices and FIFOs are counted, logged and skipped **by design** — that is why the 2026 link-handling CVEs never applied here. Do not add link support
- A registry without the referrers API is a no-op, not an error: oras falls back to the referrers tag schema and an absent tag yields an empty list with no error
- We do **not** use oras-go's file store or its tar-extraction helpers: `pullWithOras` writes blobs via `fetchBlobToFile` and extraction is our own `extractTar`. Keep it that way — it is why the oras-go extraction CVEs have never applied to us. It does **not** buy immunity generally: `repo.Referrers` goes through oras-go's `auth.Client`, so credential CVEs in that client *are* on our reachable path. `govulncheck` runs in CI for this reason
