package unpacker

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/sign"
	sgtest "github.com/sigstore/sigstore-go/pkg/testing/ca"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	testIdentity = "https://github.com/myorg/myrepo/.github/workflows/release.yml@refs/heads/main"
	testIssuer   = "https://token.actions.githubusercontent.com"
)

// signEntity signs artifact with sigstore-go's virtual Sigstore and returns
// the signed entity alongside a trusted_root.json naming that CA -- the same
// file a private cluster would ship.
//
// rekorV2 picks the log version and, with it, the cluster shape: a Rekor v2
// deployment is modelled without a timestamp authority, because that pairing
// is the whole reason this case exists. v2 issues no inclusion promise, so
// with no TSA either there is no observer timestamp at all.
func signEntity(t *testing.T, artifact []byte, identity, issuer string, rekorV2 bool) (*sgtest.TestEntity, string) {
	t.Helper()

	sigstore, err := sgtest.NewVirtualSigstore()
	if err != nil {
		t.Fatalf("virtual sigstore: %v", err)
	}

	var entity *sgtest.TestEntity
	if rekorV2 {
		entity, err = sigstore.SignWithVersion(identity, issuer, artifact, "0.0.2")
	} else {
		entity, err = sigstore.Sign(identity, issuer, artifact)
	}
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	// VirtualSigstore is a TrustedMaterial but not serialisable, so rebuild a
	// real TrustedRoot from its parts: these tests are about unpacker loading
	// trust from a trusted_root.json the way a private cluster ships one, not
	// about handing it an in-memory object.
	tsas := sigstore.TimestampingAuthorities()
	if rekorV2 {
		tsas = nil
	}
	tr, err := root.NewTrustedRoot(root.TrustedRootMediaType01,
		sigstore.FulcioCertificateAuthorities(),
		normaliseLogIDs(sigstore.CTLogs()),
		tsas,
		normaliseLogIDs(sigstore.RekorLogs()))
	if err != nil {
		t.Fatalf("build trusted root: %v", err)
	}
	trJSON, err := tr.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal trusted root: %v", err)
	}
	path := filepath.Join(t.TempDir(), "trusted_root.json")
	if err := os.WriteFile(path, trJSON, 0644); err != nil {
		t.Fatal(err)
	}
	return entity, path
}

// normaliseLogIDs works around a VirtualSigstore quirk: it sets a log's ID to
// the ASCII bytes of its hex key ID rather than the raw bytes, so marshalling
// and reparsing double-encodes the map key and no bundle entry ever matches
// its log. A real cluster's trusted_root.json is keyed by the raw ID.
func normaliseLogIDs(logs map[string]*root.TransparencyLog) map[string]*root.TransparencyLog {
	out := make(map[string]*root.TransparencyLog, len(logs))
	for hexID, tlog := range logs {
		clone := *tlog
		if raw, err := hex.DecodeString(hexID); err == nil {
			clone.ID = raw
		}
		out[hexID] = &clone
	}
	return out
}

// verifierFor builds the production verifier for a keyless policy against a
// trusted_root.json, which is the path a private cluster takes.
func verifierFor(t *testing.T, subject digest.Digest, trustedRoot, identity, issuer string) *signatureVerifier {
	t.Helper()
	sv, err := newSignatureVerifier(VerifyConfig{
		CosignIdentity:   identity,
		CosignOIDCIssuer: issuer,
		TrustedRootPath:  trustedRoot,
	}, subject)
	if err != nil {
		t.Fatalf("newSignatureVerifier: %v", err)
	}
	return sv
}

// The Rekor v1 path, where the log's inclusion promise supplies an integrated
// timestamp.
func TestVerify_KeylessRekorV1(t *testing.T) {
	artifact := []byte("an artifact")
	entity, trustedRoot := signEntity(t, artifact, testIdentity, testIssuer, false)

	sv := verifierFor(t, digest.FromBytes(artifact), trustedRoot, testIdentity, testIssuer)
	res, err := sv.verifyEntity(entity)
	if err != nil {
		t.Fatalf("a correctly signed artifact should verify: %v", err)
	}
	if sv.trustSource != "trusted-root" {
		t.Errorf("trustSource = %q, want trusted-root", sv.trustSource)
	}
	if got := observedTimestamp(res, sv.timestampSource); got == "none-rekor-v2" {
		t.Errorf("timestampSource = %q, want an observed timestamp on the v1 path", got)
	}
}

// The deployment this feature was built for. Rekor v2 issues no inclusion
// promise, so nothing observes the time -- but log inclusion is still
// verified. result.json has to say the timestamp is absent rather than imply
// the log vouched for it.
func TestVerify_KeylessRekorV2RecordsNoTimestamp(t *testing.T) {
	artifact := []byte("an artifact signed against rekor v2")
	entity, trustedRoot := signEntity(t, artifact, testIdentity, testIssuer, true)

	sv := verifierFor(t, digest.FromBytes(artifact), trustedRoot, testIdentity, testIssuer)
	res, err := sv.verifyEntity(entity)
	if err != nil {
		t.Fatalf("a Rekor v2 signature should verify: %v", err)
	}
	if got := observedTimestamp(res, sv.timestampSource); got != "none-rekor-v2" {
		t.Errorf("timestampSource = %q, want none-rekor-v2 -- the gap must be visible in result.json", got)
	}
}

func TestVerify_RejectsWrongIdentity(t *testing.T) {
	artifact := []byte("an artifact")
	entity, trustedRoot := signEntity(t, artifact, testIdentity, testIssuer, false)

	sv := verifierFor(t, digest.FromBytes(artifact), trustedRoot, `^https://github\.com/someone-else/.*$`, testIssuer)
	if _, err := sv.verifyEntity(entity); err == nil {
		t.Fatal("expected a signature from another identity to be refused")
	}
}

func TestVerify_RejectsWrongIssuer(t *testing.T) {
	artifact := []byte("an artifact")
	entity, trustedRoot := signEntity(t, artifact, testIdentity, testIssuer, false)

	sv := verifierFor(t, digest.FromBytes(artifact), trustedRoot, testIdentity, "https://accounts.google.com")
	if _, err := sv.verifyEntity(entity); err == nil {
		t.Fatal("expected a signature from another issuer to be refused")
	}
}

// A signature made by a different Sigstore instance: well-formed, but chaining
// to a CA this run does not trust.
func TestVerify_RejectsUntrustedCA(t *testing.T) {
	artifact := []byte("an artifact")
	entity, _ := signEntity(t, artifact, testIdentity, testIssuer, false)
	_, otherRoot := signEntity(t, []byte("unrelated"), testIdentity, testIssuer, false)

	sv := verifierFor(t, digest.FromBytes(artifact), otherRoot, testIdentity, testIssuer)
	if _, err := sv.verifyEntity(entity); err == nil {
		t.Fatal("expected a signature from an untrusted CA to be refused")
	}
}

// Binding is to the digest Pull resolved, so a signature over a different
// artifact cannot be replayed onto this one. That is also what stops a tag
// repointed between resolve and verify from passing.
func TestVerify_RejectsSignatureOverAnotherDigest(t *testing.T) {
	entity, trustedRoot := signEntity(t, []byte("the artifact that was signed"), testIdentity, testIssuer, false)

	other := digest.FromBytes([]byte("the artifact we actually resolved"))
	sv := verifierFor(t, other, trustedRoot, testIdentity, testIssuer)
	if _, err := sv.verifyEntity(entity); err == nil {
		t.Fatal("expected a signature over a different digest to be refused")
	}
}

// keySignBundle signs artifact with an ephemeral key and returns the bundle as
// JSON plus the public key on disk: the --verify-cosign-key path end to end,
// with no Fulcio, no log and no trusted root.
func keySignBundle(t *testing.T, artifact []byte) (bundleJSON []byte, keyPath string) {
	t.Helper()

	keypair, err := sign.NewEphemeralKeypair(nil)
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}
	pb, err := sign.Bundle(&sign.PlainData{Data: artifact}, keypair, sign.BundleOptions{})
	if err != nil {
		t.Fatalf("sign bundle: %v", err)
	}
	bundleJSON, err = protojson.Marshal(pb)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}

	pem, err := keypair.GetPublicKeyPem()
	if err != nil {
		t.Fatalf("public key: %v", err)
	}
	keyPath = filepath.Join(t.TempDir(), "cosign.pub")
	if err := os.WriteFile(keyPath, []byte(pem), 0644); err != nil {
		t.Fatal(err)
	}
	return bundleJSON, keyPath
}

// attachBundle pushes bundleJSON as a sigstore-bundle referrer of subject, the
// way `cosign sign --registry-referrers-mode oci-1-1` does.
func attachBundle(t *testing.T, cfg *Config, subject ocispec.Descriptor, bundleJSON []byte) {
	t.Helper()
	ctx := context.Background()

	repo, err := newOrasRepository(cfg)
	if err != nil {
		t.Fatalf("repository: %v", err)
	}
	push := func(desc ocispec.Descriptor, data []byte) {
		t.Helper()
		if err := repo.Push(ctx, desc, bytes.NewReader(data)); err != nil {
			t.Fatalf("push %s: %v", desc.MediaType, err)
		}
	}
	push(ocispec.DescriptorEmptyJSON, ocispec.DescriptorEmptyJSON.Data)

	layer := ocispec.Descriptor{
		MediaType:   CosignBundleArtifactType,
		Digest:      digest.FromBytes(bundleJSON),
		Size:        int64(len(bundleJSON)),
		Annotations: map[string]string{ocispec.AnnotationTitle: "bundle.sigstore.json"},
	}
	push(layer, bundleJSON)

	manifestBytes, err := json.Marshal(ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: CosignBundleArtifactType,
		Config:       ocispec.DescriptorEmptyJSON,
		Layers:       []ocispec.Descriptor{layer},
		Subject:      &subject,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	push(ocispec.Descriptor{
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: CosignBundleArtifactType,
		Digest:       digest.FromBytes(manifestBytes),
		Size:         int64(len(manifestBytes)),
	}, manifestBytes)
}

// manifestBytesOf returns the raw manifest the reference resolves to. cosign
// signs those bytes, so their digest is the subject a signature binds to.
func manifestBytesOf(t *testing.T, cfg *Config) (ocispec.Descriptor, []byte) {
	t.Helper()
	repo, err := newOrasRepository(cfg)
	if err != nil {
		t.Fatalf("repository: %v", err)
	}
	desc, rc, err := repo.FetchReference(context.Background(), "latest")
	if err != nil {
		t.Fatalf("fetch manifest: %v", err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	return desc, raw
}

// The whole run, through the registry and off disk: discover the bundle as a
// referrer, parse it, and verify against the resolved digest.
func TestVerify_KeyBasedEndToEnd(t *testing.T) {
	addr := startRegistry(t, "", registry.WithReferrersSupport(true))
	cfg, _ := orasTestConfig(t, pushOCIArtifact(t, addr, "test/keysigned"))

	subject, manifestRaw := manifestBytesOf(t, cfg)
	bundleJSON, keyPath := keySignBundle(t, manifestRaw)
	attachBundle(t, cfg, subject, bundleJSON)

	cfg.Verify = VerifyConfig{CosignKeyPath: keyPath}

	result, err := FetchReferrers(context.Background(), cfg, subject.Digest.String())
	if err != nil {
		t.Fatalf("FetchReferrers: %v", err)
	}
	rec, err := Verify(cfg, subject.Digest.String(), result)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !rec.Verified {
		t.Error("expected the key-signed artifact to verify")
	}
	if rec.Mode != "key" {
		t.Errorf("mode = %q, want key", rec.Mode)
	}
	if rec.TimestampSource != "none-bare-key" {
		t.Errorf("timestampSource = %q, want none-bare-key: a bare key has no log and no TSA", rec.TimestampSource)
	}
	if len(rec.Bundles) != 1 {
		t.Errorf("bundles = %v, want the one that was checked", rec.Bundles)
	}
}

// A signature by a key the caller does not hold must be refused, and the
// refusal recorded for result.json.
func TestVerify_RejectsWrongKey(t *testing.T) {
	addr := startRegistry(t, "", registry.WithReferrersSupport(true))
	cfg, _ := orasTestConfig(t, pushOCIArtifact(t, addr, "test/wrongkey"))

	subject, manifestRaw := manifestBytesOf(t, cfg)
	bundleJSON, _ := keySignBundle(t, manifestRaw)
	_, otherKey := keySignBundle(t, []byte("something else"))
	attachBundle(t, cfg, subject, bundleJSON)

	cfg.Verify = VerifyConfig{CosignKeyPath: otherKey}

	result, err := FetchReferrers(context.Background(), cfg, subject.Digest.String())
	if err != nil {
		t.Fatalf("FetchReferrers: %v", err)
	}
	rec, err := Verify(cfg, subject.Digest.String(), result)
	if err == nil {
		t.Fatal("expected a signature by another key to be refused")
	}
	if rec.Verified {
		t.Error("verified = true on a refused signature")
	}
	if rec.Error == "" {
		t.Error("expected the failure to be recorded for result.json")
	}
}

// Referrers-only discovery means an artifact signed the legacy tag way reads
// as unsigned. That must fail closed, never pass.
func TestVerify_UnsignedArtifactFails(t *testing.T) {
	addr := startRegistry(t, "", registry.WithReferrersSupport(true))
	cfg, _ := orasTestConfig(t, pushOCIArtifact(t, addr, "test/unsigned"))
	subject, _ := manifestBytesOf(t, cfg)
	_, keyPath := keySignBundle(t, []byte("irrelevant"))

	cfg.Verify = VerifyConfig{CosignKeyPath: keyPath}

	result, err := FetchReferrers(context.Background(), cfg, subject.Digest.String())
	if err != nil {
		t.Fatalf("FetchReferrers: %v", err)
	}
	if _, err := Verify(cfg, subject.Digest.String(), result); err == nil {
		t.Fatal("expected an unsigned artifact to fail when verification is requested")
	} else if !strings.Contains(err.Error(), "nothing to verify") {
		t.Errorf("error = %q, want it to say there was nothing to verify", err)
	}
}

// A cosign bundle attached through the referrers API carries a subject. If it
// did not, v0.8.0's subject check would skip it and verification would
// silently find nothing on exactly the path this feature uses.
func TestVerify_AttachedBundleCarriesASubject(t *testing.T) {
	addr := startRegistry(t, "", registry.WithReferrersSupport(true))
	cfg, _ := orasTestConfig(t, pushOCIArtifact(t, addr, "test/subjectcheck"))

	subject, manifestRaw := manifestBytesOf(t, cfg)
	bundleJSON, _ := keySignBundle(t, manifestRaw)
	attachBundle(t, cfg, subject, bundleJSON)

	result, err := FetchReferrers(context.Background(), cfg, subject.Digest.String())
	if err != nil {
		t.Fatalf("FetchReferrers: %v", err)
	}
	for _, ref := range result.Referrers {
		if ref.ArtifactType == CosignBundleArtifactType {
			return
		}
	}
	t.Fatalf("the sigstore bundle was not among the referrers (%+v) -- the subject check dropped it", result.Referrers)
}

// Nothing attests to the signature at all: not a case to paper over with the
// current time, because unlike Rekor v2 there is no log inclusion either.
func TestObserverOptions_FailsClosedWithoutRekorOrTSA(t *testing.T) {
	tr, err := root.NewTrustedRoot(root.TrustedRootMediaType01, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("trusted root: %v", err)
	}
	if _, _, err := observerOptions(tr); err == nil {
		t.Fatal("expected a trusted root with no Rekor and no TSA to be refused")
	} else if !strings.Contains(err.Error(), "timestamp authority") {
		t.Errorf("error = %q, want it to name the missing trust material", err)
	}
}

func TestVerifyConfig_RejectsBadFlagCombinations(t *testing.T) {
	cases := map[string]VerifyConfig{
		"identity and key together": {CosignIdentity: "x", CosignOIDCIssuer: "y", CosignKeyPath: "k"},
		"identity without issuer":   {CosignIdentity: "x"},
		"issuer without identity":   {CosignOIDCIssuer: "y"},
		"key with a trusted root":   {CosignKeyPath: "k", TrustedRootPath: "tr"},
		"key with a TUF mirror":     {CosignKeyPath: "k", TUFMirror: "https://tuf"},
		"TUF root without a mirror": {CosignIdentity: "x", CosignOIDCIssuer: "y", TUFRootPath: "r"},
		"trusted root and TUF both": {CosignIdentity: "x", CosignOIDCIssuer: "y", TrustedRootPath: "tr", TUFMirror: "https://tuf"},
		// Trust material with nothing to check is the same silently-ignored
		// flag this whole guard exists to prevent.
		"trusted root, nothing to check": {TrustedRootPath: "tr"},
		"TUF mirror, nothing to check":   {TUFMirror: "https://tuf"},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if err := cfg.Validate(); err == nil {
				t.Error("expected this combination to be rejected rather than silently resolved")
			}
		})
	}
}

func TestVerifyConfig_NotRequestedByDefault(t *testing.T) {
	if (VerifyConfig{}).Requested() {
		t.Error("the zero VerifyConfig must mean no verification was asked for")
	}
	if err := (VerifyConfig{}).Validate(); err != nil {
		t.Errorf("the zero VerifyConfig must validate: %v", err)
	}
}

// A signature produced by the real cosign binary, captured from
//
//	cosign sign --registry-referrers-mode oci-1-1 --key cosign.key \
//	    localhost:5555/test/app@sha256:bc15499c...
//
// with cosign v3.1.3. The other tests in this file sign exactly the bytes the
// policy expects, so they cannot catch a wrong artifact binding -- the fixture
// would just define the contract. This one is ground truth: it pins that
// cosign v3 wraps an in-toto Statement whose subject digest is the *image
// manifest digest*, which is what makes WithArtifactDigest(resolved digest)
// the correct binding.
//
// If cosign changes what it signs, this test fails and the binding needs
// revisiting. That is the point of it.
const realCosignSubject = "sha256:bc15499cd1b3d5322682d5008879149408494616eaacd975ed0caa9ddda42dee"

func TestVerify_RealCosignV3Bundle(t *testing.T) {
	outputDir := t.TempDir()
	refPath := filepath.Join("referrers", "sig", "abc")
	if err := os.MkdirAll(filepath.Join(outputDir, refPath), 0755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "cosign-v3-image-signature.sigstore.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, refPath, "bundle.json"), raw, 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		OutputDir: outputDir,
		Verify:    VerifyConfig{CosignKeyPath: filepath.Join("testdata", "cosign-v3-image-signature.pub")},
	}
	result := &Result{Referrers: []Referrer{{
		ArtifactType: CosignBundleArtifactType,
		Path:         refPath,
		Files:        []string{"manifest.json", "bundle.json"},
	}}}

	rec, err := Verify(cfg, realCosignSubject, result)
	if err != nil {
		t.Fatalf("a real cosign v3 signature should verify: %v", err)
	}
	if !rec.Verified {
		t.Error("verified = false on a genuine cosign signature")
	}

	// And the binding has to be to *this* image, not merely to a well-formed
	// bundle: the same signature must be refused under another digest.
	other := digest.FromString("a different image entirely").String()
	if _, err := Verify(cfg, other, result); err == nil {
		t.Error("expected the signature to be refused against a different resolved digest")
	}
}
