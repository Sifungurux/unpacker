package unpacker

import (
	"crypto"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	digest "github.com/opencontainers/go-digest"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/sigstore/sigstore/pkg/signature"
)

// CosignBundleArtifactType is what `cosign sign --registry-referrers-mode
// oci-1-1` attaches. Discovery is deliberately limited to the referrers API:
// cosign's legacy sha256-<hex>.sig tag scheme is not consulted, so a signature
// pushed that way reads as absent — a refused pull rather than a false pass.
const CosignBundleArtifactType = "application/vnd.dev.sigstore.bundle.v0.3+json"

// VerifyConfig is the verification half of Config. The zero value means no
// verification was requested, which must stay distinguishable from "verified"
// all the way out to result.json.
type VerifyConfig struct {
	CosignIdentity   string
	CosignOIDCIssuer string
	CosignKeyPath    string
	TrustedRootPath  string
	TUFMirror        string
	TUFRootPath      string
}

// Requested reports whether the user asked for verification at all.
func (v VerifyConfig) Requested() bool {
	return v.CosignIdentity != "" || v.CosignKeyPath != ""
}

func (v VerifyConfig) usesTrustRoot() bool {
	return v.TrustedRootPath != "" || v.TUFMirror != "" || v.TUFRootPath != ""
}

// Validate rejects flag combinations rather than silently picking one. A trust
// flag that is accepted and then ignored is the failure mode this whole change
// exists to avoid.
func (v VerifyConfig) Validate() error {
	switch {
	case v.CosignIdentity != "" && v.CosignKeyPath != "":
		return errors.New("--verify-cosign-identity and --verify-cosign-key are different verification modes: pass one")
	case v.CosignIdentity != "" && v.CosignOIDCIssuer == "":
		return errors.New("--verify-cosign-identity needs --verify-cosign-oidc-issuer: an identity regex unanchored to an issuer matches across issuers")
	case v.CosignOIDCIssuer != "" && v.CosignIdentity == "":
		return errors.New("--verify-cosign-oidc-issuer needs --verify-cosign-identity")
	case v.CosignKeyPath != "" && v.usesTrustRoot():
		return errors.New("--verify-cosign-key verifies against the key alone and never consults a trusted root: drop the --verify-trusted-root/--verify-tuf-* flag")
	case v.TUFRootPath != "" && v.TUFMirror == "":
		return errors.New("--verify-tuf-root needs --verify-tuf-mirror")
	case v.usesTrustRoot() && !v.Requested():
		return errors.New("a --verify-trusted-root/--verify-tuf-* flag says what to trust but not what to check: " +
			"add --verify-cosign-identity or --verify-cosign-key, or drop it")
	case v.TrustedRootPath != "" && v.TUFMirror != "":
		return errors.New("--verify-trusted-root and --verify-tuf-mirror are two ways to get the same material: pass one")
	}
	return nil
}

// Verification records what was checked against what. It is written to
// result.json so a consumer can tell "verified" from "nobody asked" without
// reading the exit code.
type Verification struct {
	Mode            string   `json:"mode"` // "keyless" or "key"
	Identity        string   `json:"identity,omitempty"`
	OIDCIssuer      string   `json:"oidcIssuer,omitempty"`
	KeyPath         string   `json:"keyPath,omitempty"`
	TrustSource     string   `json:"trustSource"`     // "public", "trusted-root", "tuf"
	TimestampSource string   `json:"timestampSource"` // see observerOptions
	Bundles         []string `json:"bundles"`         // digests of the bundles checked
	Verified        bool     `json:"verified"`
	Error           string   `json:"error,omitempty"`
}

// trustedMaterial resolves what unpacker will trust, and names the source for
// result.json. Precedence: an explicit file, then a TUF repository, then the
// public good instance.
func trustedMaterial(v VerifyConfig) (root.TrustedMaterial, string, error) {
	switch {
	case v.CosignKeyPath != "":
		material, err := publicKeyMaterial(v.CosignKeyPath)
		return material, "key", err

	case v.TrustedRootPath != "":
		tr, err := root.NewTrustedRootFromPath(v.TrustedRootPath)
		if err != nil {
			return nil, "", fmt.Errorf("read trusted root %s: %w", v.TrustedRootPath, err)
		}
		return tr, "trusted-root", nil

	case v.TUFMirror != "":
		opts := tuf.DefaultOptions()
		opts.RepositoryBaseURL = v.TUFMirror
		if v.TUFRootPath != "" {
			rootJSON, err := os.ReadFile(v.TUFRootPath)
			if err != nil {
				return nil, "", fmt.Errorf("read TUF root %s: %w", v.TUFRootPath, err)
			}
			opts.Root = rootJSON
		}
		tr, err := root.FetchTrustedRootWithOptions(opts)
		if err != nil {
			return nil, "", fmt.Errorf("fetch trusted root from %s: %w", v.TUFMirror, err)
		}
		return tr, "tuf", nil

	default:
		tr, err := root.FetchTrustedRoot()
		if err != nil {
			return nil, "", fmt.Errorf("fetch the public Sigstore trusted root: %w", err)
		}
		return tr, "public", nil
	}
}

// publicKeyMaterial builds trust from a bare cosign public key. There is no
// certificate chain and no trusted root involved, which is why combining
// --verify-cosign-key with a trust-root flag is rejected outright.
func publicKeyMaterial(path string) (root.TrustedMaterial, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read cosign key %s: %w", path, err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("cosign key %s is not PEM", path)
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse cosign key %s: %w", path, err)
	}
	verifier, err := signature.LoadVerifier(pub, crypto.SHA256)
	if err != nil {
		return nil, fmt.Errorf("load cosign key %s: %w", path, err)
	}
	// A bare key carries no validity window of its own, so it is trusted for
	// all time. The signature's own timestamp is what bounds it.
	key := root.NewExpiringKey(verifier, time.Time{}, time.Time{})
	return root.NewTrustedPublicKeyMaterial(func(string) (root.TimeConstrainedVerifier, error) {
		return key, nil
	}), nil
}

// observerOptions decides where a signature's *time* comes from, which cannot
// be left implicit: NewSignedEntityVerifier refuses to construct without at
// least one source.
//
// Rekor v2 is the case that shapes this. sigstore-go takes an integrated
// timestamp only from an inclusion promise (pkg/verify/tlog.go), and Rekor v2
// issues an inclusion proof against a checkpoint instead — so a Rekor-v2-only
// cluster with no timestamp authority yields no observer timestamp at all.
// Log *inclusion* is still fully verified there; only the attestation of when
// is missing, so the run proceeds with the current time and result.json says
// so rather than a flag default hiding it.
func observerOptions(tm root.TrustedMaterial) ([]verify.VerifierOption, string, error) {
	// A bare cosign key has no log and no timestamp authority by
	// construction, so the fail-closed rule below -- which is about a cluster
	// that attests nothing -- would wrongly reject every key-based run.
	if _, isBareKey := tm.(*root.TrustedPublicKeyMaterial); isBareKey {
		return []verify.VerifierOption{verify.WithCurrentTime()}, "none-bare-key", nil
	}

	var (
		opts     []verify.VerifierOption
		hasRekor bool
		hasTSA   bool
	)
	if tr, ok := tm.(interface {
		RekorLogs() map[string]*root.TransparencyLog
	}); ok {
		hasRekor = len(tr.RekorLogs()) > 0
	}
	if tr, ok := tm.(interface {
		TimestampingAuthorities() []root.TimestampingAuthority
	}); ok {
		hasTSA = len(tr.TimestampingAuthorities()) > 0
	}

	if hasRekor {
		// Inclusion is required whenever the cluster runs a log, on both
		// Rekor versions.
		opts = append(opts, verify.WithTransparencyLog(1))
	}

	switch {
	case hasTSA:
		return append(opts, verify.WithSignedTimestamps(1)), "tsa", nil
	case hasRekor:
		// Rekor v1 supplies an integrated timestamp; v2 does not, and the
		// verifier cannot tell which until it sees the entry. Asking for the
		// current time covers both: a v1 entry still has its inclusion
		// verified above, and a v2 entry is not rejected for lacking a
		// promise it never issues.
		return append(opts, verify.WithCurrentTime()), "rekor", nil
	default:
		return nil, "", errors.New("the trusted root has no Rekor log and no timestamp authority, " +
			"so nothing attests to this signature: verification cannot be meaningful against it")
	}
}

// Verify checks the signatures attached to resolvedDigest, and is a
// precondition of unpacking rather than a step after it.
//
// bundles are the sigstore bundles FetchReferrers already wrote to disk. Every
// one of them has been digest-verified against its descriptor and checked to
// name resolvedDigest as its subject before it gets here.
func Verify(cfg *Config, resolvedDigest string, result *Result) (*Verification, error) {
	v := cfg.Verify
	rec := &Verification{
		Mode:       "keyless",
		Identity:   v.CosignIdentity,
		OIDCIssuer: v.CosignOIDCIssuer,
		KeyPath:    v.CosignKeyPath,
		Bundles:    []string{},
	}
	if v.CosignKeyPath != "" {
		rec.Mode = "key"
	}

	subject, err := digest.Parse(resolvedDigest)
	if err != nil {
		return rec, fmt.Errorf("parse resolved digest %q: %w", resolvedDigest, err)
	}

	sv, err := newSignatureVerifier(v, subject)
	if err != nil {
		return rec, err
	}
	rec.TrustSource = sv.trustSource
	timestampSource := sv.timestampSource

	paths := bundlePaths(cfg, result)
	if len(paths) == 0 {
		return rec, fmt.Errorf("%w: no %s referrer attached to %s: nothing to verify "+
			"(cosign's default tag scheme is not consulted — sign with --registry-referrers-mode oci-1-1)",
			ErrVerification, CosignBundleArtifactType, resolvedDigest)
	}

	var lastErr error
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return rec, fmt.Errorf("read signature bundle %s: %w", path, err)
		}
		var b bundle.Bundle
		if err := b.UnmarshalJSON(raw); err != nil {
			lastErr = fmt.Errorf("parse signature bundle %s: %w", filepath.Base(path), err)
			continue
		}
		rec.Bundles = append(rec.Bundles, digest.FromBytes(raw).String())

		res, err := sv.verifyEntity(&b)
		if err != nil {
			lastErr = fmt.Errorf("bundle %s: %w", filepath.Base(path), err)
			continue
		}
		// Recorded from what was actually observed rather than predicted from
		// the trusted root: with Rekor v2 and no TSA there is no observed
		// timestamp at all, and result.json should say that plainly instead of
		// implying the log vouched for the time.
		rec.TimestampSource = observedTimestamp(res, timestampSource)

		// One good signature is enough: the policy already pins the artifact
		// digest and the identity, so anything that satisfies it is a
		// signature by someone trusted over this exact image.
		rec.Verified = true
		log.Printf("verified %s against %s (%s trust, %s timestamp)", resolvedDigest, filepath.Base(path), sv.trustSource, rec.TimestampSource)
		return rec, nil
	}

	if rec.TimestampSource == "" {
		rec.TimestampSource = timestampSource
	}
	if lastErr == nil {
		lastErr = errors.New("no signature satisfied the policy")
	}
	rec.Error = lastErr.Error()
	return rec, fmt.Errorf("%w: %w", ErrVerification, lastErr)
}

// signatureVerifier is trust resolution, observer choice and policy resolved
// once, so that applying them to a bundle is separable from reading one off
// disk. The split is what lets the policy be tested against a signed entity
// directly, without a JSON round-trip standing in the way.
type signatureVerifier struct {
	verifier        *verify.Verifier
	policy          verify.PolicyBuilder
	trustSource     string
	timestampSource string
}

func newSignatureVerifier(v VerifyConfig, subject digest.Digest) (*signatureVerifier, error) {
	tm, source, err := trustedMaterial(v)
	if err != nil {
		return nil, err
	}

	verifierOpts, timestampSource, err := observerOptions(tm)
	if err != nil {
		return nil, err
	}

	verifier, err := verify.NewSignedEntityVerifier(tm, verifierOpts...)
	if err != nil {
		return nil, fmt.Errorf("build verifier: %w", err)
	}

	policyOpts, err := policyOptions(v)
	if err != nil {
		return nil, err
	}

	// Bound to the digest Pull resolved, never the tag, so a tag repointed
	// between resolve and verify cannot pass.
	digestBytes, err := hexDigestBytes(subject)
	if err != nil {
		return nil, err
	}

	return &signatureVerifier{
		verifier:        verifier,
		policy:          verify.NewPolicy(verify.WithArtifactDigest(subject.Algorithm().String(), digestBytes), policyOpts...),
		trustSource:     source,
		timestampSource: timestampSource,
	}, nil
}

func (s *signatureVerifier) verifyEntity(se verify.SignedEntity) (*verify.VerificationResult, error) {
	return s.verifier.Verify(se, s.policy)
}

// observedTimestamp names where the verified time actually came from. An empty
// set means the signature was accepted on inclusion and certificate validity
// alone -- the Rekor v2 case, where the log issues no inclusion promise.
func observedTimestamp(res *verify.VerificationResult, planned string) string {
	if res == nil || len(res.VerifiedTimestamps) == 0 {
		if planned == "rekor" {
			return "none-rekor-v2"
		}
		return "none"
	}
	types := make([]string, 0, len(res.VerifiedTimestamps))
	for _, ts := range res.VerifiedTimestamps {
		// "CurrentTime" is what sigstore-go reports when it was told to fall
		// back to the clock, which is the absence of an observer rather than
		// one more kind of observer. Reporting it as a source would be the
		// exact overstatement result.json exists to avoid.
		if ts.Type == "CurrentTime" {
			continue
		}
		types = append(types, ts.Type)
	}
	if len(types) == 0 {
		if planned == "rekor" {
			return "none-rekor-v2"
		}
		return planned
	}
	sort.Strings(types)
	return strings.Join(slicesCompact(types), "+")
}

// slicesCompact drops adjacent duplicates: two Rekor entries are one source.
func slicesCompact(in []string) []string {
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

func policyOptions(v VerifyConfig) ([]verify.PolicyOption, error) {
	if v.CosignKeyPath != "" {
		return []verify.PolicyOption{verify.WithKey()}, nil
	}
	// Both are regexes: an exact identity is just an anchored one, and
	// requiring the caller to say so keeps a bare substring from matching more
	// than they meant.
	id, err := verify.NewShortCertificateIdentity(v.CosignOIDCIssuer, "", "", v.CosignIdentity)
	if err != nil {
		return nil, fmt.Errorf("build certificate identity: %w", err)
	}
	return []verify.PolicyOption{verify.WithCertificateIdentity(id)}, nil
}

// bundlePaths finds the sigstore bundles among what FetchReferrers downloaded.
func bundlePaths(cfg *Config, result *Result) []string {
	var paths []string
	for _, ref := range result.Referrers {
		if ref.ArtifactType != CosignBundleArtifactType {
			continue
		}
		for _, name := range ref.Files {
			if name == "manifest.json" {
				continue
			}
			paths = append(paths, filepath.Join(cfg.OutputDir, ref.Path, name))
		}
	}
	return paths
}

func hexDigestBytes(d digest.Digest) ([]byte, error) {
	raw, err := hex.DecodeString(d.Encoded())
	if err != nil {
		return nil, fmt.Errorf("decode digest %s: %w", d, err)
	}
	return raw, nil
}
