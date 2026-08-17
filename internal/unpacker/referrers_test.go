package unpacker

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const testSBOM = `{"spdxVersion":"SPDX-2.3","name":"unpacker-test"}`

// pushReferrer attaches an artifact to the manifest that ref resolves to, the
// way cosign or an SBOM tool would: a manifest carrying a subject, an empty
// config, and the payload as a layer.
func pushReferrer(t *testing.T, cfg *Config, artifactType, title string, payload []byte) ocispec.Descriptor {
	t.Helper()
	ctx := context.Background()

	repo, err := newOrasRepository(cfg)
	if err != nil {
		t.Fatalf("repository: %v", err)
	}

	subject, err := repo.Resolve(ctx, "latest")
	if err != nil {
		t.Fatalf("resolve subject: %v", err)
	}

	push := func(desc ocispec.Descriptor, data []byte) {
		t.Helper()
		if err := repo.Push(ctx, desc, bytes.NewReader(data)); err != nil {
			t.Fatalf("push %s: %v", desc.MediaType, err)
		}
	}

	emptyJSON := []byte("{}")
	configDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeEmptyJSON,
		Digest:    digest.FromBytes(emptyJSON),
		Size:      int64(len(emptyJSON)),
	}
	push(configDesc, emptyJSON)

	layerDesc := ocispec.Descriptor{
		MediaType:   artifactType,
		Digest:      digest.FromBytes(payload),
		Size:        int64(len(payload)),
		Annotations: map[string]string{ocispec.AnnotationTitle: title},
	}
	push(layerDesc, payload)

	manifest := ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: artifactType,
		Config:       configDesc,
		Layers:       []ocispec.Descriptor{layerDesc},
		Subject:      &subject,
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal referrer manifest: %v", err)
	}
	manifestDesc := ocispec.Descriptor{
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: artifactType,
		Digest:       digest.FromBytes(manifestBytes),
		Size:         int64(len(manifestBytes)),
	}
	push(manifestDesc, manifestBytes)

	return subject
}

func TestFetchReferrers_DownloadsAttachedArtifacts(t *testing.T) {
	addr := startRegistry(t, "", registry.WithReferrersSupport(true))
	cfg, _ := orasTestConfig(t, pushOCIArtifact(t, addr, "test/withrefs"))
	subject := pushReferrer(t, cfg, "application/spdx+json", "sbom.spdx.json", []byte(testSBOM))

	result, err := FetchReferrers(context.Background(), cfg, subject.Digest.String())
	if err != nil {
		t.Fatalf("FetchReferrers: %v", err)
	}
	if len(result.Referrers) != 1 {
		t.Fatalf("expected 1 referrer, got %d: %+v", len(result.Referrers), result.Referrers)
	}

	ref := result.Referrers[0]
	if ref.ArtifactType != "application/spdx+json" {
		t.Errorf("artifactType = %q, want application/spdx+json", ref.ArtifactType)
	}

	// the payload must land under referrers/<type>/<digest>/ with its title
	dir := filepath.Join(cfg.OutputDir, ref.Path)
	got, err := os.ReadFile(filepath.Join(dir, "sbom.spdx.json"))
	if err != nil {
		t.Fatalf("read downloaded SBOM: %v", err)
	}
	if string(got) != testSBOM {
		t.Errorf("SBOM content = %q, want %q", got, testSBOM)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		t.Errorf("referrer manifest not written: %v", err)
	}

	// result.json must describe what was written
	data, err := os.ReadFile(filepath.Join(cfg.OutputDir, "result.json"))
	if err != nil {
		t.Fatalf("read result.json: %v", err)
	}
	var onDisk Result
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("parse result.json: %v", err)
	}
	if onDisk.Digest != subject.Digest.String() {
		t.Errorf("result.json digest = %q, want %q", onDisk.Digest, subject.Digest)
	}
	if len(onDisk.Referrers) != 1 || onDisk.Referrers[0].Digest != ref.Digest {
		t.Errorf("result.json referrers = %+v, want the one downloaded", onDisk.Referrers)
	}
	if !slicesContains(onDisk.Referrers[0].Files, "sbom.spdx.json") {
		t.Errorf("result.json files = %v, want it to list sbom.spdx.json", onDisk.Referrers[0].Files)
	}
}

// TestFetchReferrers_WrongSubjectFindsNothing is the control for the test
// above: it uses the same registry with the same referrer attached, and only
// the subject digest differs. Without it, a happy-path pass would not
// distinguish "found the artifact attached to this digest" from "found
// whatever the registry felt like returning".
func TestFetchReferrers_WrongSubjectFindsNothing(t *testing.T) {
	addr := startRegistry(t, "", registry.WithReferrersSupport(true))
	cfg, _ := orasTestConfig(t, pushOCIArtifact(t, addr, "test/wrongsubject"))
	pushReferrer(t, cfg, "application/spdx+json", "sbom.spdx.json", []byte(testSBOM))

	// a well-formed digest that nothing is attached to
	other := digest.FromString("not the subject")
	result, err := FetchReferrers(context.Background(), cfg, other.String())
	if err != nil {
		t.Fatalf("FetchReferrers: %v", err)
	}
	if len(result.Referrers) != 0 {
		t.Errorf("expected no referrers for an unrelated digest, got %+v", result.Referrers)
	}
}

// TestFetchReferrers_NoReferrersSupport covers the registries that predate OCI
// 1.1: the in-process registry only serves the referrers API when asked to, so
// leaving it off reproduces one exactly. This must be a no-op, not a failure.
func TestFetchReferrers_NoReferrersSupport(t *testing.T) {
	addr := startRegistry(t, "") // referrers API deliberately not enabled
	cfg, _ := orasTestConfig(t, pushOCIArtifact(t, addr, "test/norefs"))

	repo, err := newOrasRepository(cfg)
	if err != nil {
		t.Fatalf("repository: %v", err)
	}
	subject, err := repo.Resolve(context.Background(), "latest")
	if err != nil {
		t.Fatalf("resolve subject: %v", err)
	}

	result, err := FetchReferrers(context.Background(), cfg, subject.Digest.String())
	if err != nil {
		t.Fatalf("FetchReferrers must not fail on a registry without referrers support: %v", err)
	}
	if len(result.Referrers) != 0 {
		t.Errorf("expected no referrers, got %+v", result.Referrers)
	}
	if _, err := os.Stat(filepath.Join(cfg.OutputDir, "referrers")); !os.IsNotExist(err) {
		t.Errorf("referrers/ should not be created when there is nothing to download: %v", err)
	}

	// result.json is still written, so a consumer can tell "asked, found none"
	// apart from "never asked"
	data, err := os.ReadFile(filepath.Join(cfg.OutputDir, "result.json"))
	if err != nil {
		t.Fatalf("result.json not written: %v", err)
	}
	var onDisk Result
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("parse result.json: %v", err)
	}
	if len(onDisk.Referrers) != 0 {
		t.Errorf("result.json referrers = %+v, want empty", onDisk.Referrers)
	}
}

// The artifact type and title annotation are supplied by the registry, so
// neither may reach into a path.
func TestReferrerPathsAreConfined(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"application/spdx+json", "application-spdx-json"},
		{"../../etc", "etc"},
		{"..", "unknown"},
		{"", "unknown"},
		{"a/b/c", "a-b-c"},
	} {
		if got := typeDirName(tc.in); got != tc.want {
			t.Errorf("typeDirName(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if filepath.Base(typeDirName(tc.in)) != typeDirName(tc.in) {
			t.Errorf("typeDirName(%q) = %q, which is not a single path element", tc.in, typeDirName(tc.in))
		}
	}

	for _, tc := range []struct{ in, want string }{
		{"sbom.json", "sbom.json"},
		{"../../escape", "escape"},
		{"/etc/passwd", "passwd"},
		{"..", "fallback"},
		{"", "fallback"},
	} {
		if got := safeFileName(tc.in, "fallback"); got != tc.want {
			t.Errorf("safeFileName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func slicesContains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
