# unpacker — Technical Debt

**Surveyed:** 2026-08-17 · **Commit:** `f62d7e3` (v0.6.0) · ~1,300 lines Go + a 427-line integration script
**Status:** 11 of 12 done as of `1eac85a`. D7 stays open by design; D11 needs a decision (see below).

Every item below was checked against the code or reproduced, not inferred. Each has a ready-to-paste prompt; the prompts are self-contained and each names how to prove the fix.

**Start with D2 (CI on pull requests) regardless of what else you pick.** Nothing here — including the fixes for the other items — can be trusted to stay fixed while tests only run on release tags.

Items already tracked in `~/Downloads/unpacker-scm-review-2026-08-16.md` (cosign verification, `--platform`, zstd, cloud keychain, exit codes, `pkg/` API) are **not** repeated here; this document covers debt in what exists rather than features that don't.

---

## Tier 1 — ships broken, or hides breakage

### D1 · ✅ done (`838261a`, PR #10) · The container image installs an x86-64 umoci on every architecture

**What.** `Dockerfile` downloads `umoci.amd64` unconditionally. umoci is how *every Docker image* gets unpacked, so on an arm64 host that path fails with `exec format error` — the crane→umoci route is dead, while the tar route keeps working, so it looks like a media-type problem rather than a broken binary.

**Evidence.**

```
$ podman image inspect unpacker:dev --format '{{.Architecture}} {{.Os}}'
arm64 linux
$ podman run --rm --entrypoint sh unpacker:dev -c 'head -c20 /usr/local/bin/umoci | xxd | head -2'
00000000: 7f45 4c46 0201 0100 ...
00000010: 0200 3e00                 ← e_machine 0x3e = x86-64
```

It runs on this Mac only because the podman VM emulates amd64. A native arm64 Linux node has no such safety net. The pinned version is also **0.4.7 (2021)** against 0.6.0 current.

**Options.**

- **(a) `TARGETARCH` in the Dockerfile** with a per-architecture pinned sha256. Small diff; needs a `--platform` build of both arches to test, and hard-coded hashes to maintain per umoci release.
- **(b) Vendor umoci's Go API** (the review's §4.7). Removes the external binary, the download, and the checksum question in one move — but it is a large change, and umoci's library API is not as stable as its CLI.
- **(c) Pin per-arch hashes *and* bump to 0.6.0** — (a) plus the version bump, which is the version this repo is actually developed against locally.

Recommend (c) now, (b) as a separate decision.

**Prompt.**

```text
In unpacker's Dockerfile, the umoci binary is downloaded as umoci.amd64 regardless of the
image's target architecture, so arm64 images contain an x86-64 binary that only runs under
emulation. Use BuildKit's TARGETARCH to select umoci.amd64 or umoci.arm64, bump UMOCI_VERSION
from 0.4.7 to 0.6.0, and verify each download against a sha256 hard-coded in the Dockerfile
(one per architecture, with a comment recording where the hash came from) rather than against
the checksum file fetched from the same GitHub release. Fail the build on an unsupported
TARGETARCH instead of silently defaulting. Verify by building for both platforms and checking
the binary's architecture inside each image, e.g. podman build --platform linux/arm64 then
`file /usr/local/bin/umoci` reporting aarch64, and `umoci --version` reporting 0.6.0.
```

### D2 · ✅ done (`5e0f0a1`, PR #9) · Tests never run on a pull request

**What.** `.github/workflows/release.yml` is the only workflow and triggers on `on: push: tags: v*`. There is no CI on pull requests or on pushes to `main`. Every commit merged on 2026-08-16/17 — eight of them, including five behaviour changes — was verified only on one laptop.

**Evidence.** `.github/workflows/release.yml`, lines 3–6. No other file in `.github/workflows/`.

**Options.**

- **(a) Unit tests only** on PR + push to main: `go test ./...`, `go vet`, `gofmt -l`. Five minutes of work, catches most regressions, no new runner dependencies.
- **(b) (a) plus the media-type integration suite.** It needs `go`, `flux`, `helm`, `umoci`, `oras`, `curl` on the runner and no Docker daemon, so it is installable in a workflow — but it is a real workflow to write and maintain.
- **(c) (b) plus the container suite**, which additionally needs Docker and a built image.

Recommend (a) immediately, then (b) as a follow-up; the media-type suite is the one that covers the routing logic unit tests can't reach.

**Prompt.**

```text
unpacker has no CI on pull requests — .github/workflows/release.yml only triggers on v* tags,
so go test has never run on a PR. Add .github/workflows/ci.yml running on pull_request and on
push to main: go build ./..., go vet ./..., gofmt -l (failing if it prints anything), and
go test ./... -race -count=1. Use go-version-file: go.mod. Also bump actions/checkout and
actions/setup-go to versions that are not flagged for Node 20 deprecation, in both this
workflow and release.yml. Do not add the integration suite in this change. Verify by opening a
PR and confirming the checks run and are required-able.
```

---

## Tier 2 — correctness and robustness

### D3 · ✅ done (`d7a3a04`, PR #11) · A failed unpack leaves a populated `image/` and no `result.json`

**What.** When extraction fails partway, the partially extracted tree stays on disk and `result.json` is never written. A consumer that trusts the directory rather than the exit code reads a truncated artifact as a complete one.

**Evidence.** Reproduced with `--max-total-bytes 20` against a two-layer artifact: the command exits non-zero, `image/` contains partial output, no `result.json`. The only `os.RemoveAll` in the codebase is `pull.go:52`, for `tmp/` on the oras→crane fallback.

**Options.** Remove `image/` on any unpack failure (simple, but destroys evidence for debugging); or write a `result.json` recording the failure and leave the tree (keeps evidence, requires consumers to read it); or extract into a temporary sibling directory and rename into place on success (atomic-ish, most code).

**Prompt.**

```text
In unpacker, a failed Unpack leaves partially extracted content in <output-dir>/image/ and
writes no result.json, so a consumer that checks the directory rather than the exit code sees a
truncated artifact as a complete one. Make the output atomic: extract into a sibling temp
directory under the output dir and rename it to image/ only on success, removing the temp
directory on failure. Keep the existing behaviour that tmp/ is left in place for debugging.
Add a unit test that a failing extraction (use a tiny --max-total-bytes over a two-layer
artifact) leaves no image/ directory behind, and confirm the test fails without the change.
```

### D4 · ✅ done (`24a51eb`, PR #13) · `CopyFiles` is unbounded

**What.** The plain-file path copies every file in `tmp/` with no size, count, or total limit — the same exposure `ExtractTar` was hardened against in v0.6.0. It is bounded in practice by what was already downloaded (each blob is digest- and size-verified against the manifest), so this is defence in depth rather than an open hole.

**Evidence.** `unpack.go:350`, `func CopyFiles(srcDir, destDir string) error` — no `Limits` parameter.

**Prompt.**

```text
internal/unpacker/unpack.go: CopyFiles has no size limits, unlike ExtractTar which honours
Limits. Give CopyFiles the same treatment — take a Limits value, enforce MaxTotalBytes across
all files copied and MaxFileBytes per file, and fail closed with an error naming the flag.
Reuse the existing limit-enforcement style rather than adding a second mechanism. Update the
callers and the README's extraction-limits section, which currently says the limits apply only
to the tar path. Add unit tests for the per-file and total cases and verify each fails without
the guard.
```

### D5 · ✅ done (`24a51eb`, PR #13) · `pullWithCrane` mutates the process environment

**What.** The docker-config auth path sets `DOCKER_CONFIG` globally and restores it with `defer`. That is process-wide state in a library package: it is not safe if `Pull` is ever called concurrently, and it leaks into anything else sharing the process — the `pkg/` API the review suggests would make this a real problem.

**Evidence.** `pull.go:254–258`, `os.Setenv("DOCKER_CONFIG", ...)` with `defer os.Setenv(...)` to restore.

**Prompt.**

```text
internal/unpacker/pull.go: pullWithCrane authenticates by setting the DOCKER_CONFIG environment
variable process-wide and restoring it with defer, which is unsafe under concurrent use and
leaks into the rest of the process. Replace it with an explicit keychain: build the authn
keychain from the config file path directly (go-containerregistry's authn has helpers for
reading a config file) and pass it via crane.WithAuthFromKeychain, so no environment variable is
touched. Add a unit test that Pull with cfg.Creds.ConfigPath set does not modify os.Getenv
("DOCKER_CONFIG") during or after the call.
```

### D6 · ✅ done (`1eac85a`, PR #14) · gzip is assumed, not detected

**What.** `ExtractTar` calls `gzip.NewReader` unconditionally, so an uncompressed or zstd-compressed layer fails with `open gzip: ...` rather than being handled or clearly rejected. OCI permits `+zstd` and plain `tar` layer types; `firstFileIsTar` also decides routing by sniffing for the gzip magic bytes, so a non-gzip artifact silently takes the file-copy path instead.

**Evidence.** `unpack.go:257` is the only decompression call; `unpack.go:216` sniffs `0x1f 0x8b`.

**Prompt.**

```text
internal/unpacker/unpack.go assumes every allowed layer is gzip: ExtractTar calls
gzip.NewReader unconditionally and firstFileIsTar sniffs for the gzip magic bytes. Detect the
compression from the layer's media type suffix (+gzip, +zstd, or none) with a magic-byte fallback,
support uncompressed tar and zstd (klauspost/compress/zstd is already an indirect dependency —
confirm before adding it as a direct one), and return an error naming the unsupported
compression rather than a confusing "open gzip" for anything else. Keep gzip behaviour
byte-identical. Add unit tests for a plain tar layer and a zstd layer, and one asserting the
error message for an unsupported compression.
```

### D7 · ⏸ open by design · Unpack routing is half manifest-driven, half disk-sniffing

**What.** Since v0.6.0, *what* to extract comes from the manifest, but *which path to take* still comes from sniffing `tmp/` (`firstFileIsTar`, `dirExists(blobs/sha256)`). The two can disagree, and the heuristic is what makes D6's silent fall-through to file-copy possible.

**Evidence.** `unpack.go:101–114`.

**This is a redesign, not a cleanup** — and it touches the single-file-artifact path that the copy fallback exists to serve. Flagging rather than proposing a fix; do it only alongside D6, and only with the integration suite green first.

*Partly addressed by D6*: the sniff (`firstFileIsArchive`) now recognises gzip, zstd and uncompressed tar instead of gzip alone, so a non-gzip archive is no longer misrouted to the copy path. The structural split — routing from disk, content from the manifest — remains.

---

## Tier 3 — hygiene

### D8 · ✅ done (`aac17b5`, PR #12) · Test helpers are duplicated across a package split

Two nearly identical registry helpers exist because `pull_test.go` is `package unpacker_test` while `pull_verify_test.go` is `package unpacker`: `startTestRegistry` (pull_test.go:23) and `startOCIRegistry` (pull_verify_test.go:32), plus three separate push helpers. New tests have to guess which half to use.

```text
internal/unpacker's tests are split across package unpacker and package unpacker_test, which
forced two near-identical registry helpers (startTestRegistry and startOCIRegistry) and three
push helpers. Consolidate: move the external-package tests into package unpacker, keep one
registry helper that takes registry.Option values and an optional corrupting proxy path, and one
push helper per artifact shape (OCI artifact, docker schema2 image, referrer). Do not change what
any test asserts — this is a mechanical consolidation, and the suite must stay green with the
same test names.
```

### D9 · ✅ done (`aac17b5`, PR #12) · `AllowedMediaTypeForTest` is a test seam in the public API

`unpack.go:389` exports a function that exists only so an external-package test can reach `allowedMediaType`. It disappears for free once D8 moves those tests into the package.

```text
Remove AllowedMediaTypeForTest from internal/unpacker/unpack.go and have the media-type matching
test call allowedMediaType directly from inside package unpacker. Depends on D8.
```

### D10 · ✅ done (`aac17b5`, PR #12) · `withDefaults()` is applied at two levels

`extractLayers` (unpack.go:166) and `extractTar` (unpack.go:265) both call it. Harmless today because it is idempotent, but it means the shared multi-layer budget is re-defaulted per layer — if a future change makes a limit relative rather than absolute, this becomes a bug.

```text
internal/unpacker/unpack.go calls Limits.withDefaults() in both extractLayers and extractTar, so
the shared multi-layer budget is re-defaulted on every layer. Apply defaults exactly once at the
entry point (Unpack), pass already-resolved Limits inward, and document on the type that inner
functions expect resolved values. Keep ExtractTar's exported behaviour — a zero-value Limits
passed by an external caller must still resolve to the defaults. Existing limit tests must pass
unchanged.
```

### D11 · ⛔ blocked on a decision · Module path does not match the repository

`go.mod` declares `github.com/Sifungurux/unpacker`; the repo is `github.com/Sifungurux/unpacker`. `go install github.com/Sifungurux/unpacker/cmd/unpacker@latest` cannot work. This is the review's P3-b and is listed here only because it affects every import line.

```text
unpacker's go.mod declares module github.com/Sifungurux/unpacker while the repository is
github.com/Sifungurux/unpacker, so go install from source cannot resolve. Rename the module and
update every import. Confirm the repository is the intended canonical home before doing this —
if the Sifungurux path is the intended published one, the fix is to say so in the README instead.
Verify with go build ./... and go install github.com/Sifungurux/unpacker/cmd/unpacker@latest
against the pushed branch.
```

### D12 · ✅ done (this change) · Stale design docs

`docs/superpowers/plans/2026-04-09-unpacker-go-rewrite.md` and `docs/superpowers/specs/2026-04-09-unpacker-go-rewrite-design.md` describe the April rewrite. They predate digest verification, extraction limits, referrers, multi-layer extraction, and the auth changes, so they now describe a tool that no longer exists.

```text
docs/superpowers/{plans,specs}/2026-04-09-unpacker-go-rewrite*.md describe the original rewrite
and are now contradicted by the code. Either move them under docs/history/ with a header noting
they are a point-in-time record, or delete them if the README and CLAUDE.md cover what they were
for. Do not "update" them — they are a record of a decision, not living documentation.
```

---

## What is left

- **D7** — the routing redesign, deliberately not attempted.
- **D11** — needs you to say which module path is canonical before anything is renamed.

## Suggested order (completed in this order)

1. **D2** — CI on PRs. Everything else is unverifiable without it.
2. **D1** — the arm64 umoci binary. It is the only item that is broken for users right now.
3. **D3** — atomic output, because a partial `image/` is misread silently.
4. **D8 + D9 + D10** — one mechanical cleanup pass; small, low-risk, makes the test suite easier to extend.
5. **D5**, **D4** — robustness, in either order.
6. **D6 (+ D7)** — compression handling and the routing redesign, together and last.
7. **D11**, **D12** — whenever; both need a decision from you more than they need work.
