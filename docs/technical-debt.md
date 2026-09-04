# unpacker — Technical Debt

**Surveyed:** 2026-08-17 (D1–D12) · re-surveyed 2026-09-03 (D13–D19)
**Commit:** `1dfa1d6` (v0.9.0) · ~4,400 lines Go including tests, plus a 427-line integration script
**Status:** D1–D12: ten done, D7 open by design, D11 needs a decision. D13–D19 are open.

The second survey folds in what the 2026-08-31 supply-chain review left unfixed after v0.8.0
and v0.9.0 shipped, plus two items from that work that neither review covers.

Every item below was checked against the code or reproduced, not inferred. Each has a ready-to-paste prompt; the prompts are self-contained and each names how to prove the fix.

**Start with D13 (the integration suite in CI).** D2 put unit tests on PRs and explicitly
deferred the integration suite; that deferral was never picked up, so the media-type suite —
the one that covers routing logic unit tests cannot reach — has still never run in CI, across
three releases including a security release.

The original advice, still true for D1–D12:
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

### D11 · ✅ done (this change) · Module path did not match the repository

`go.mod` declared a module path belonging to a different organisation than the repository, so
`go install github.com/Sifungurux/unpacker/cmd/unpacker@latest` could not resolve. Renamed to
match the repository, and every reference to the old organisation removed from the tree.

```text
unpacker's go.mod declares a module path under a different organisation than the repository at
github.com/Sifungurux/unpacker, so go install from source cannot resolve. Rename the module to
match the repository and update every import. Remove every remaining reference to the old
organisation, including in archived docs under docs/history -- where the archived plan should
note that the path was rewritten rather than silently presenting the new one as what was
originally run. Verify with `git grep -i <old-org>` returning nothing, go build ./..., and
go test ./... -race.
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


# Second survey — 2026-09-03, at `1dfa1d6` (v0.9.0)

What the 2026-08-31 supply-chain review left open after v0.8.0 and v0.9.0, plus two items from
that work which neither review covers. Every item was confirmed against the code on this commit.

---

## Tier 1 — ships broken, or hides breakage

### D13 · ✅ done (this change) · The integration suite has never run in CI

**What.** D2 added `ci.yml` with unit tests, `vet`, `gofmt` and (since v0.8.0) `govulncheck`,
and its prompt said in as many words: *"Do not add the integration suite in this change."* That
follow-up was never picked up. `scripts/test-integration.sh` — 427 lines, eight cases covering
the routing and media-type logic unit tests cannot reach — runs only when someone remembers to
run it locally.

Three releases have shipped since, including a security release and a signature-verification
release, both of which changed pull and unpack behaviour.

**Evidence.**

```
$ grep -c test-integration .github/workflows/*.yml
.github/workflows/ci.yml:0
.github/workflows/release.yml:0
```

**Why it is Tier 1.** It is the same argument D2 made and it has not been answered. The suite
is also the only thing that exercises the **built container image**, so the arm64 umoci fix
(D1) and anything else baked into the image is currently verified by hand or not at all.

**Options.**

- **(a) Media-type suite only.** Needs `go`, `flux`, `helm`, `umoci`, `curl` and no daemon —
  all installable on `ubuntu-latest`. Covers routing, media-type gating, multi-layer extraction.
- **(b) (a) plus the container suite**, which additionally needs a working Docker/Podman and a
  built image, and is the slower and flakier half.

Recommend (a) now as a separate job, (b) only if it proves stable.

**Prompt.**

```text
unpacker's scripts/test-integration.sh has never run in CI: .github/workflows/ci.yml runs only
unit tests, vet, gofmt and govulncheck, and D2's prompt explicitly deferred the integration
suite. Three releases have shipped since, two of which changed pull and unpack behaviour.

Add a second job to ci.yml that runs the media-type half of the suite. It needs go, flux, helm,
umoci and curl on the runner and no container daemon. Install flux, helm and umoci by pinned
version with a checksum check, not by piping a script from the internet. Do not add the
container suite in this change -- it needs a built image and is the flakier half; say so in a
comment so the next person knows it was a decision.

The script currently skips a suite when its prerequisites are missing. That is right locally and
wrong in CI, where a silent skip is indistinguishable from a pass: add a flag or environment
variable that makes a missing prerequisite a hard failure, and set it in the workflow.

Verify by opening a PR and confirming the new job runs the media-type cases and reports them
individually -- and by deliberately breaking one case in a scratch commit to confirm the job
goes red rather than skipping.
```

---

## Tier 2 — correctness and robustness

### D14 · ⛔ open · `runUmoci` is outside the limit system

**What.** `--max-total-bytes`, `--max-file-bytes` and `--max-entries` bound the tar and copy
paths, and since v0.8.0 the download phase too. `runUmoci` shells out to the `umoci` binary with
none of them. The container-image path handles the largest inputs of any route, so the
decompression-bomb protection has its hole exactly where it is most needed.

**Evidence.** `internal/unpacker/unpack.go:455` — `cmd := exec.Command("umoci", args...)`, no
`Limits` in scope. `CLAUDE.md` documents this honestly ("only `runUmoci` is unbounded by them"),
so it is a known hole rather than a surprise.

**Options.**

- **(a) Pre-check the layout.** Sum the OCI layout's declared layer sizes against the remaining
  budget before invoking umoci. Cheap, and catches the declared-size case — but a layer that
  lies about its size still expands unbounded.
- **(b) Bound the child process.** `ulimit -f` or a disk quota on the umoci process. Catches
  actual expansion, is platform-specific, and umoci's failure will be confusing.
- **(c) Vendor umoci's Go API** (D1 option (b)) and bring the path inside the limit system
  properly. Correct, and much the largest change.

Recommend (a) now — it closes the declared-size case for one screen of code — and (c) as its own
decision. Note (a) alone must not be described as bounding the umoci path; it bounds what the
manifest *claims*.

**Prompt.**

```text
internal/unpacker/unpack.go: runUmoci (around line 455) shells out to the umoci binary with no
size or entry limit, so --max-total-bytes protects the tar and copy paths but not the
container-image path, which handles the largest inputs. CLAUDE.md already records this.

Before invoking umoci, read the OCI layout in tmp/ and sum its manifest's declared layer sizes
against the run's MaxTotalBytes, failing with an error naming the flag if it would be exceeded.
Reuse the downloadBudget type from pull.go rather than adding a second mechanism.

Be precise in the comment and the docs about what this does and does not do: it bounds what the
manifest declares, not what umoci actually writes, so a layer that under-declares its size still
expands unbounded. Update CLAUDE.md's Key Facts and the README's limits section to say exactly
that rather than implying the path is now bounded.

Add a test that a layout whose declared layer sizes exceed --max-total-bytes is rejected before
umoci is invoked, and confirm it fails without the guard.
```

### D15 · ⛔ open · No timeout and no signal handling

**What.** `main.go` builds `context.Background()` and passes it to every network call. A
registry that accepts a connection and then stalls hangs the process indefinitely. In a
scheduled monitor pipeline that is a wedged job holding a runner, not a failed one that gets
retried. Ctrl-C during a pull leaves `tmp/` and any staging directory behind.

**Evidence.** `cmd/unpacker/main.go:83` — `ctx := context.Background()`.

**Prompt.**

```text
cmd/unpacker/main.go builds ctx := context.Background() (around line 83), so a registry that
accepts a connection and stalls hangs unpacker forever -- in a scheduled pipeline that is a
wedged job rather than a failed one.

Add a --timeout duration flag defaulting to 10m, and wrap the context with
signal.NotifyContext for SIGINT and SIGTERM so Ctrl-C and a scheduler's termination both cancel
the run rather than killing it mid-write. Make the timeout cover the whole run, not each
request.

On cancellation the staging directory Unpack uses must be cleaned up -- image/ is published by
rename, so a cancelled run must leave no partial staging directory behind. tmp/ should still be
left in place for debugging, as today.

Distinguish the two in the error message: a timeout should say so and name --timeout, and a
signal should say which signal, so a wedged job and an operator's Ctrl-C are not both "context
canceled".

Add a test using an httptest server that accepts and never responds, asserting the run fails
within a short --timeout rather than hanging, and one asserting no staging directory survives.
```

### D16 · ⛔ open · Releases are unsigned, and the workflow's actions are not pinned

**What.** `.goreleaser.yaml` produces `checksums.txt` and nothing else: no signature, no SBOM,
no provenance. A tool whose headline v0.9.0 feature is *verifying other people's signatures*
ships its own binaries unsigned. Separately, every workflow step is pinned to a mutable tag
(`@v7`), so a compromised or retagged action lands straight in a release build.

**Evidence.**

```
$ grep -nE '^signs:|^sboms:|cosign|sbom' .goreleaser.yaml
(no output)
$ grep -h 'uses:' .github/workflows/*.yml | sort -u
      - uses: actions/checkout@v7
      - uses: actions/setup-go@v7
      - uses: goreleaser/goreleaser-action@v7
```

**Prompt.**

```text
unpacker verifies cosign signatures as of v0.9.0 but ships its own releases unsigned:
.goreleaser.yaml emits checksums.txt and nothing else.

Add keyless cosign signing and SBOM generation to .goreleaser.yaml (the signs: and sboms:
blocks), and give the release workflow the id-token: write permission keyless signing needs.
Sign the checksums file rather than each archive individually unless there is a reason not to.

Separately, pin every GitHub Action to a full commit SHA rather than a mutable @v7 tag, in both
ci.yml and release.yml, with the version in a trailing comment so renovate/dependabot can still
read it. A mutable tag in a release workflow is the supply-chain hole this tool exists to help
people find.

Verify by cutting a pre-release tag and confirming the release carries a .sig and an SBOM, and
that `cosign verify-blob --certificate-identity-regexp ... checksums.txt` passes against it.
Document the verification command in the README so consumers can actually use it.
```

---

## Tier 3 — gaps and decisions

### D17 · ⛔ open · No notation verification

**What.** v0.9.0 added cosign. The review scoped notation as an explicit follow-up and it has
not been started. Flux's OCIRepository CRD supports both providers, so a monitor tracking
Flux-signed artifacts may need it.

**Evidence.** No `notation` anywhere in `internal/` (grep hits are all `annotations`).

**Decide before building.** Notation matters only if something in your estate signs with it. If
everything is cosign, this stays closed and the ledger should say so rather than carrying a
permanently open item.

**Prompt.**

```text
unpacker verifies cosign signatures but not notation ones. Add notation verification alongside
the existing cosign support, following the shape verify.go already uses: discovery through the
referrers API, verification gating Unpack, and the outcome recorded in result.json's
verification object.

Use notation-go with a trust policy and trust store. Add --verify-notation-trust-policy and
--verify-notation-trust-store, and make them mutually exclusive with the cosign verify flags in
VerifyConfig.Validate the same way the existing combinations are -- silently ignoring one
verification mode because another was passed is the failure that function exists to prevent.

Extend the verification object with the provider that was used, so a consumer can tell a cosign
result from a notation one.

Test against the in-process registry with a notation-signed artifact, mirroring the existing
cosign tests: a valid signature verifies, an untrusted identity is refused with no image/
published. Follow TestVerify_RealCosignV3Bundle's example and check in a fixture produced by
the real notation CLI rather than one written to match the policy.
```

### D18 · ⏸ open by design · A referrer with no `subject` is skipped, not rejected

**What.** v0.8.0 rejects a referrer whose `subject` names another digest, but a referrer with
*no* subject is logged and skipped rather than failing the run. Per OCI 1.1 a referrer is
defined by having a subject, so rejecting would be more correct — the skip exists so that a
registry whose fallback listing omits the field does not break `--with-referrers`.

**Evidence.** `internal/unpacker/referrers.go:149`, marked in-code:

```go
// ponytail: skip-and-warn for compatibility; make it a hard error
// once no real registry is seen doing this.
```

**New evidence since.** The cosign v3 signature captured in `internal/unpacker/testdata/` was
attached through the OCI 1.1 fallback tag by a registry with no referrers API, and its manifest
**did** carry a `subject`. That is one data point in favour of tightening, not yet a policy.

**Prompt.**

```text
internal/unpacker/referrers.go around line 149 skips a referrer whose manifest has no subject,
with a ponytail: marker saying to make it a hard error once no real registry is seen omitting
the field. Per OCI 1.1 a referrer is defined by having a subject, and the cosign bundle captured
in internal/unpacker/testdata was attached through the fallback tag with a subject present.

Before changing anything, gather evidence rather than reasoning from the spec: check what
Harbor, ECR, GCR, Docker Hub and a plain registry:2 actually return for a referrers listing,
including the fallback tag path. Report what you find.

If nothing omits the subject, make it a hard error, delete the ponytail marker, convert
TestDownloadReferrer_SkipsSubjectlessReferrer into a rejection test, and note the behaviour
change in the README and CLAUDE.md -- it turns a warning into a failed run, so it belongs in a
minor release, not a patch.

If something does omit it, leave the skip, update the marker to name that registry, and close
the question in the ledger so it is not re-derived a third time.
```

### D19 · ✅ done (this change) · Smaller review leftovers

Five items from the review's P3, none individually worth a section. Confirmed present at
`1dfa1d6`.

| | Gap | Evidence |
|---|---|---|
| a | No `--platform`; an index takes crane's default (linux/amd64) with no way to pick another or fetch all | `grep -c platform cmd/unpacker/main.go` → 0 |
| b | Everything exits 1; a pipeline cannot branch on not-found vs auth-failed vs verification-failed without parsing stderr | `main.go:19`, single `os.Exit(1)` |
| c | `result.json` is not written when `Pull` or `Unpack` fails, so a consumer reading the file sees the *previous* run's result | `WriteResult` is only reached on the referrers and verification paths |
| d | Extracted modes come from the archive, so a `0777` directory or `0000` file is reproduced faithfully; `Perm()` handles the dangerous bits, not the awkward ones | `unpack.go`, `hdr.FileInfo().Mode().Perm()` |
| e | Alpine base pinned to `3.21`; newer stable branches exist | `Dockerfile:11` |

**Prompt (c — the one with a correctness argument).**

```text
unpacker writes result.json on the referrers and verification paths but not when Pull or Unpack
fails, so a consumer that reads the file rather than the exit code sees the *previous* run's
result and treats a failed run as the last successful one. This is the same class of bug D3
fixed for the image/ directory.

Write result.json on every terminating path, with an error field naming the stage that failed
(pull, unpack, verify) and the message. Keep the existing invariant that a successful run's
result.json is unchanged in shape -- the error field is absent on success, the way verification
is absent when not requested.

Add a test that a failed pull leaves a result.json describing the failure rather than a stale
one from an earlier run into the same output directory.
```

**Prompt (a, b, d, e — batch these only if you want them; each is independent).**

```text
Four small leftovers in unpacker, from the 2026-08-31 review's P3. Do them as separate commits.

(a) Add --platform (e.g. linux/arm64) for image indexes. crane currently picks its default,
    linux/amd64, with no way to choose. Also support --platform all to unpack every platform in
    an index into image/<os>-<arch>/. Test that an index with two platforms unpacks the one
    asked for, and that the existing default is unchanged when the flag is absent.

(b) Give the CLI structured exit codes so a pipeline can branch without parsing stderr:
    distinguish not-found, auth-failed, verification-failed and limit-exceeded from a generic
    failure. Document the table in the README. Keep 0 for success and 1 for anything
    unclassified.

(d) Clamp extracted file and directory modes with a umask-style mask so an archive cannot
    reproduce a 0777 directory or a 0000 file. Perm() already drops setuid/setgid/sticky; this
    is about the merely awkward cases. Do not change the setuid handling or its test.

(e) Bump the Dockerfile's Alpine base from 3.21 to the current stable branch and confirm the
    integration suite's container half still passes against the rebuilt image.
```

---

## What is left

**Open from the first survey**

- **D7** — the routing redesign. Open by design; partly mitigated by D6. Do it only with the
  integration suite green in CI, which is D13.
- **D11** — the module path (`Sifungurux` vs `Sifungurux`). Still blocked on your decision, not
  on work. It is the reason `go install ...@latest` cannot resolve.

**Open from the second survey**

- ~~**D13** — integration suite in CI.~~ Done: the media-type half runs on every PR, with
  `SUITES=mediatype` making its prerequisites mandatory rather than a reason to skip. The
  container half remains out, deliberately.
- **D14** — `runUmoci` outside the limit system.
- **D15** — no timeout, no signal handling.
- **D16** — unsigned releases, unpinned actions.
- **D17** — notation. Needs a decision on whether anything in the estate signs with it.
- **D18** — subjectless referrer; needs registry evidence before tightening.
- ~~**D19** — five small leftovers.~~ Done, except that `--platform all` was deliberately not
  built: it needs several OCI layouts and a different `image/` layout, which is a larger change
  than the rest of D19 combined. Run once per platform instead.

## Suggested order

1. **D15** — timeout and signals. Cheap, and the failure it prevents is a wedged scheduled job.
2. **D16** — sign the releases. Mostly workflow work, no code risk, and the credibility argument
   writes itself now that v0.9.0 verifies signatures.
3. **D14** — bound the umoci path, with honest wording about what the bound covers.
4. **D18** — gather registry evidence, then either tighten or close it.
7. **D11**, **D17** — both need a decision from you more than they need work.
6. **D7** — last, and only with CI green.
