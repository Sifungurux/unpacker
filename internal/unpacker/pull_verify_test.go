package unpacker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"oras.land/oras-go/v2/content"
)

const testBlobContent = "fake content"

// startOCIRegistry runs an in-process registry. Responses to GET requests whose
// path contains corruptPath (empty = none) get one byte flipped, simulating a
// registry that serves content not matching the digest it was asked for. The
// body length is unchanged, so only the digest check can catch it.
//
// Blob responses advertise "Accept-Ranges: bytes" like GHCR and Docker Hub do;
// oras hands back a different reader implementation for those, and that reader's
// end-of-body behaviour is what the verification depends on.
func startOCIRegistry(t *testing.T, corruptPath string, opts ...registry.Option) string {
	t.Helper()
	inner := registry.New(opts...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isBlobGet := r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/blobs/sha256:")
		corrupt := corruptPath != "" && r.Method == http.MethodGet && strings.Contains(r.URL.Path, corruptPath)
		if !isBlobGet && !corrupt {
			inner.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		inner.ServeHTTP(rec, r)
		body := rec.Body.Bytes()
		if corrupt && len(body) > 0 {
			body[0] ^= 0xff
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		if isBlobGet && rec.Code == http.StatusOK {
			w.Header().Set("Accept-Ranges", "bytes")
		}
		w.WriteHeader(rec.Code)
		w.Write(body) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// pushOCIArtifact pushes an image with OCI media types throughout, so
// pullWithOras handles it instead of bailing out to the crane fallback.
func pushOCIArtifact(t *testing.T, registryAddr, repo string) string {
	t.Helper()
	ref := registryAddr + "/" + repo + ":latest"
	img, err := mutate.AppendLayers(empty.Image, static.NewLayer([]byte(testBlobContent), types.OCILayer))
	if err != nil {
		t.Fatalf("build test image: %v", err)
	}
	img = mutate.MediaType(img, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, types.OCIConfigJSON)
	if err := crane.Push(img, ref, crane.Insecure); err != nil {
		t.Fatalf("push test image: %v", err)
	}
	return ref
}

func orasTestConfig(t *testing.T, image string) (*Config, string) {
	t.Helper()
	outputDir := t.TempDir()
	tmpDir := filepath.Join(outputDir, "tmp")
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	return &Config{
		Image:     image,
		OutputDir: outputDir,
		Insecure:  true,
		Creds:     &Credentials{Public: true},
	}, tmpDir
}

func TestPullWithOras_VerifiesGoodBlob(t *testing.T) {
	addr := startOCIRegistry(t, "")
	cfg, tmpDir := orasTestConfig(t, pushOCIArtifact(t, addr, "test/ociartifact"))

	if _, err := pullWithOras(context.Background(), cfg, tmpDir); err != nil {
		t.Fatalf("pullWithOras: %v", err)
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("read tmp dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 blob in tmp/, got %d: %v", len(entries), entries)
	}
	got, err := os.ReadFile(filepath.Join(tmpDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if string(got) != testBlobContent {
		t.Errorf("blob content = %q, want %q", got, testBlobContent)
	}
}

func TestPullWithOras_CorruptedBlobRejected(t *testing.T) {
	addr := startOCIRegistry(t, "/blobs/sha256:")
	cfg, tmpDir := orasTestConfig(t, pushOCIArtifact(t, addr, "test/corruptblob"))

	_, err := pullWithOras(context.Background(), cfg, tmpDir)
	if err == nil {
		t.Fatal("pullWithOras accepted a blob that does not match its manifest digest")
	}
	if !errors.Is(err, content.ErrMismatchedDigest) {
		t.Errorf("error = %v, want a digest mismatch", err)
	}
	if !strings.Contains(err.Error(), "failed verification") {
		t.Errorf("error = %q, want it to mention failed verification", err)
	}

	// the partially written blob must not be left behind for Unpack() to use
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("read tmp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected no files in tmp/ after rejection, got %v", entries)
	}

	// Pull() falls back to crane when oras fails. The corrupt blob must not
	// sneak through that path, and crane's own error must not bury the reason
	// the first attempt failed.
	_, pullErr := Pull(context.Background(), cfg)
	if pullErr == nil {
		t.Fatal("Pull accepted a corrupted blob via the crane fallback")
	}
	if !errors.Is(pullErr, content.ErrMismatchedDigest) {
		t.Errorf("Pull error = %v, want the oras digest mismatch preserved", pullErr)
	}
	if !strings.Contains(pullErr.Error(), "crane fallback") {
		t.Errorf("Pull error = %q, want the crane failure reported too", pullErr)
	}
}

func TestPullWithOras_CorruptedManifestRejected(t *testing.T) {
	addr := startOCIRegistry(t, "/manifests/")
	cfg, tmpDir := orasTestConfig(t, pushOCIArtifact(t, addr, "test/corruptmanifest"))

	_, err := pullWithOras(context.Background(), cfg, tmpDir)
	if err == nil {
		t.Fatal("pullWithOras accepted a manifest that does not match its digest")
	}
	if !strings.Contains(err.Error(), "read manifest") {
		t.Errorf("error = %q, want it to mention the manifest", err)
	}
	if _, statErr := os.Stat(filepath.Join(cfg.OutputDir, "manifest.json")); !os.IsNotExist(statErr) {
		t.Errorf("manifest.json written despite digest mismatch: %v", statErr)
	}
}

// TestPullWithOras_ReferenceForms covers the three ways a reference can name a
// manifest. Digest-pinned pulls used to resolve the text after the last ":" as
// a tag, so repo@sha256:abc looked up the tag "abc" and 404'd — the form a
// supply-chain pipeline is most likely to use was the one that did not work.
func TestPullWithOras_ReferenceForms(t *testing.T) {
	addr := startOCIRegistry(t, "")
	image := pushOCIArtifact(t, addr, "test/refforms")

	// resolve the tag once to learn the digest the other forms should hit
	tagCfg, tagTmp := orasTestConfig(t, image)
	want, err := pullWithOras(context.Background(), tagCfg, tagTmp)
	if err != nil {
		t.Fatalf("pull by tag: %v", err)
	}

	repoRef := addr + "/test/refforms"
	for _, tc := range []struct{ name, image string }{
		{"tag", repoRef + ":latest"},
		{"digest", repoRef + "@" + want},
		{"tag and digest", repoRef + ":latest@" + want},
		{"no reference defaults to latest", repoRef},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, tmpDir := orasTestConfig(t, tc.image)
			got, err := pullWithOras(context.Background(), cfg, tmpDir)
			if err != nil {
				t.Fatalf("pullWithOras(%q): %v", tc.image, err)
			}
			if got != want {
				t.Errorf("pullWithOras(%q) resolved to %s, want %s", tc.image, got, want)
			}
			if _, err := os.Stat(filepath.Join(cfg.OutputDir, "manifest.json")); err != nil {
				t.Errorf("manifest.json not written for %q: %v", tc.image, err)
			}
		})
	}
}
