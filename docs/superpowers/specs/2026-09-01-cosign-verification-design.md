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

## Known gap: the artifact a cosign image signature actually covers

**Status: open. The keyless and key-based machinery is complete and tested, but
the artifact binding is not yet correct for signatures produced by the real
`cosign` binary.**

`newSignatureVerifier` binds with
`verify.WithArtifactDigest("sha256", <resolved manifest digest>)`. Reading
cosign v3.1.3 shows that is right for *attestations* and wrong for *signatures*:

- `cmd/cosign/cli/sign/sign.go:244` builds a simple-signing payload
  (`sigPayload.Cosign`) carrying `critical.image.docker-manifest-digest`, and
  `:302` signs **those payload bytes** via `sign.PlainData{Data: payload}`.
- So a signature bundle's `messageSignature.messageDigest` is
  `sha256(payload)`, not the manifest digest.
- cosign's own `verifyImageAttestationsSigstoreBundle` uses
  `WithArtifactDigest(image digest)` because an in-toto statement names the
  image as its subject. The signature path instead goes through
  `sig.Payload()`.

The tests do not catch this because they sign exactly the bytes the policy
expects: the fixture defines the contract instead of testing it. That is also
why `TestVerify_AttachedBundleCarriesASubject` currently proves only that
`attachBundle` sets a subject.

Today's behaviour is safe but not useful for image signatures: a genuine cosign
signature is *refused*, not falsely accepted.

The fix, once a real bundle settles where the payload bytes live on the
registry in referrers mode:

1. Bind the artifact to the payload bytes rather than the manifest digest.
2. Parse the payload and require
   `critical.image.docker-manifest-digest == <resolved digest>`, which is the
   check that actually ties a signature to this image.
3. Replace the fixtures with one real `cosign sign --registry-referrers-mode
   oci-1-1` bundle checked into testdata, so the contract stops being
   self-defined.

`--verify-cosign-key` over a plain blob is unaffected; only the container-image
signature path is wrong.
