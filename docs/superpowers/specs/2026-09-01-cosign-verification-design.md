# Cosign signature verification

**Status:** approved 2026-09-01 · **Issue:** review finding P1-b

unpacker pulls and unpacks artifacts for a supply-chain monitor but verifies no
signatures, so it faithfully extracts unsigned content. This adds cosign
verification as a precondition of a successful run, following the model Flux's
OCIRepository CRD uses: verification gates the pull rather than happening after
it.

Scope is cosign only. Notation is a follow-up.

## Discovery: referrers API only

Signatures are discovered through the OCI 1.1 referrers API — what
`cosign sign --registry-referrers-mode oci-1-1` produces — and not through
cosign's legacy `sha256-<hex>.sig` tag scheme.

This reuses `FetchReferrers` rather than adding a second discovery path. The
cost is that a default `cosign sign` pipeline, which still writes the tag
scheme against most registries, will produce "no signature found". That fails
closed, so the failure mode is a refused pull rather than a false pass.

Two existing behaviours now sit in front of verification and are load-bearing
for it:

- `--max-referrers` caps the listing a signature is discovered in.
- The v0.8.0 subject check skips a referrer with no `subject`. A bundle
  attached via the referrers API carries one — a test asserts this, because if
  it did not, verification would silently find nothing on exactly this path.

## Flags

```
--verify-cosign-identity <regex>      keyless: SAN match on the Fulcio cert
--verify-cosign-oidc-issuer <url>     keyless: required with the above
--verify-cosign-key <path>            key-based: a cosign public key
--verify-trusted-root <path>          private: static trusted_root.json
--verify-tuf-mirror <url>             private: TUF repository
--verify-tuf-root <path>              private: TUF bootstrap root.json
```

No verify flag leaves the verification behaviour off entirely and adds no
`verification` block to `result.json`. It is *not* byte-identical to v0.8.0 in
one respect: referrers are now fetched before the unpack rather than after it,
so with `--with-referrers` a referrer that fails its subject check now fails
the run before `image/` is published instead of after.

**Composition errors**, all rejected at flag-parse rather than ignored:

| Combination | Why |
|---|---|
| identity + key | Two different verification modes |
| identity without issuer | An unanchored SAN regex matches across issuers |
| key + any trust-root flag | A bare public key uses `NewTrustedPublicKeyMaterial` and never consults a trusted root. Accepting a trust flag and ignoring it is the failure S7 was about |

## Trust resolution

Three-way, in precedence order:

1. `--verify-trusted-root` → `root.NewTrustedRootFromPath`
2. `--verify-tuf-mirror` (+ `--verify-tuf-root`) → `root.FetchTrustedRootWithOptions`
3. neither → the public good instance

## Observers and timestamps

`verify.NewSignedEntityVerifier` refuses to construct without at least one
timestamp source, so this cannot be left implicit.

Rekor v2 is the case that drives the design. `sigstore-go` v1.3.0 supports it
fully, but `pkg/verify/tlog.go:213` takes an integrated timestamp only from an
inclusion promise:

```go
if trustIntegratedTime && entry.HasInclusionPromise() && !hasTimestampMap[keyID] {
```

Rekor v2 issues no inclusion promise — it gives an inclusion proof against a
checkpoint — so a Rekor-v2-only cluster with no TSA yields zero observer
timestamps. Log inclusion is still fully verified on that path;
`verifiedLogIDsMap` is populated from successful entry verification either way.
Only the attestation of *when* is missing.

Options are therefore derived from the trusted root's contents:

| Trust material | Verifier options |
|---|---|
| Rekor present (v1 or v2) | `WithTransparencyLog(1)` — inclusion always required |
| TSA present | `WithSignedTimestamps(1)` |
| Rekor v1, no TSA | `WithIntegratedTimestamps(1)` |
| Rekor v2, no TSA | `WithCurrentTime()` |
| Neither Rekor nor TSA | fail closed |

Fail-closed survives only for the case it was meant for: nothing attests to the
artifact at all. Where inclusion is proven but time is not, the run succeeds and
`result.json` records the timestamp source, so the gap is visible in the output
rather than buried in a flag default.

No flag gates this. `--verify-allow-unobserved` was considered and rejected:
Rekor v2 with no TSA is the primary deployment here, and a flag every run must
pass is friction that teaches people to pass it everywhere.

## Where verification sits

Today `main.go` runs `Pull → Unpack → FetchReferrers`. Verification must gate
`Unpack`, and signatures are discovered by the referrers query, so the fetch
moves ahead of the unpack:

```
Pull            resolve the digest
FetchReferrers  referrers/ + result.json      (moved ahead of Unpack)
Verify          against the resolved digest, from the fetched bundles
Unpack          only on success
```

Verification runs against the digest `Pull` resolved, never the tag, so a tag
repointed between resolve and verify cannot pass.

A verify run queries referrers whether or not `--with-referrers` was passed,
because that is where the bundle lives; `--with-referrers` continues to control
only whether `referrers/` is *kept* on disk.

`Unpack`'s existing staging-and-rename means a failed verification publishes no
`image/`. `result.json` stays written on every run — the invariant repaired in
`65b6f41` must survive this reordering.

## result.json

A `verification` object records: whether verification was requested, the mode
(`keyless` or `key`), the identity and issuer or the key path, the trust source
(`public`, `trusted-root`, `tuf`), the timestamp source (`rekor-v1-set`, `tsa`,
`none-rekor-v2`), the bundle digests checked, and the outcome.

Absent means not requested. That distinction must be unambiguous from
`result.json` alone: a consumer has to be able to tell "verified" from "nobody
asked".

## Testing

Against the in-process registry:

- a correctly signed artifact verifies and unpacks
- an unsigned artifact fails when verification is requested
- a signature from a different key fails
- a signature with a non-matching identity fails
- a failed verification leaves no `image/` and a `result.json` recording it
- with no verify flags, behaviour and `result.json` are unchanged from v0.8.0
- a tag repointed between resolve and verify cannot pass
- a trusted root with neither Rekor nor TSA fails with a clear error
- a cosign bundle attached via the referrers API carries a `subject`

The integration suite must still pass.

## Out of scope

Notation, `--platform`, structured exit codes, bounding `runUmoci`,
`--timeout`, release signing.

## Resolved: what a cosign image signature actually covers

An earlier revision of this document recorded the artifact binding as broken,
from reading `cmd/cosign/cli/sign/sign.go:244` where cosign builds a
simple-signing payload carrying `critical.image.docker-manifest-digest` and
signs those bytes. That reading was of the **legacy** path.

Signing a real image with cosign v3.1.3 settles it. `--new-bundle-format`
defaults to true, and the bundle it produces is a **DSSE envelope** wrapping an
in-toto Statement:

```json
{"_type":"https://in-toto.io/Statement/v1",
 "subject":[{"digest":{"sha256":"bc15499c…"}}],
 "predicateType":"https://sigstore.dev/cosign/sign/v1",
 "predicate":{}}
```

The statement's subject digest **is** the image manifest digest, so
`verify.WithArtifactDigest(<resolved digest>)` is the correct binding. The
referrer carries `artifactType: application/vnd.dev.sigstore.bundle.v0.3+json`
and a `subject` naming the image, so discovery and the v0.8.0 subject check
both work unchanged.

Confirmed end to end against a local registry: a genuine signature verifies, a
different key is refused with no `image/` published, and the same signature is
refused against a different resolved digest.
`TestVerify_RealCosignV3Bundle` pins this using the captured bundle in
`testdata/`, so the contract is no longer self-defined by a fixture.

### Remaining limitation

`cosign sign --new-bundle-format=false` produces the legacy simple-signing
payload, whose signature does not cover the manifest digest. Those signatures
are **refused**, not falsely accepted. The flag is deprecated upstream ("this
will be the only supported format in future versions"), so this is a
documented limitation rather than planned work.

Signatures pushed to cosign's legacy `sha256-<hex>.sig` tag are likewise not
consulted. Note this is distinct from the OCI 1.1 referrers *fallback tag*
(`sha256-<hex>`, no suffix), which oras reads and which was exercised in the
end-to-end check above — the local registry had no referrers API and cosign
fell back to it.
