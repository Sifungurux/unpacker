package unpacker_test

import (
	"context"
	"encoding/json"
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

	"github.com/energinet/unpacker/internal/unpacker"
)

// startTestRegistry spins up an in-process OCI registry and returns its address.
func startTestRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// pushTestImage pushes a minimal image to the test registry and returns the full reference.
func pushTestImage(t *testing.T, registryAddr, repo string) string {
	t.Helper()
	ref := registryAddr + "/" + repo + ":latest"
	layer := static.NewLayer([]byte("fake content"), types.OCIContentDescriptor)
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatalf("build test image: %v", err)
	}
	if err := crane.Push(img, ref, crane.Insecure); err != nil {
		t.Fatalf("push test image: %v", err)
	}
	return ref
}

func TestPull_CraneFallback_WritesOCILayout(t *testing.T) {
	addr := startTestRegistry(t)
	image := pushTestImage(t, addr, "test/myimage")

	outputDir := t.TempDir()
	cfg := &unpacker.Config{
		Image:     image,
		OutputDir: outputDir,
		Insecure:  true,
		Creds:     &unpacker.Credentials{Public: true},
	}

	if err := unpacker.Pull(context.Background(), cfg); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	// crane fallback writes OCI layout to tmp/
	if _, err := os.Stat(filepath.Join(outputDir, "tmp", "index.json")); err != nil {
		t.Errorf("expected OCI layout index.json in tmp/: %v", err)
	}
}

// pushDockerSchema2Image pushes an image whose manifest, config, and layer
// all declare legacy Docker media types (application/vnd.docker.*), mirroring
// real Docker Hub images still served with the schema2 manifest format
// (e.g. docker.io/cloudelements/eicar:latest). Returns the full reference.
func pushDockerSchema2Image(t *testing.T, registryAddr, repo string) string {
	t.Helper()
	ref := registryAddr + "/" + repo + ":latest"
	layer := static.NewLayer([]byte("fake content"), types.DockerLayer)
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatalf("build test image: %v", err)
	}
	img = mutate.MediaType(img, types.DockerManifestSchema2)
	img = mutate.ConfigMediaType(img, types.DockerConfigJSON)
	if err := crane.Push(img, ref, crane.Insecure); err != nil {
		t.Fatalf("push test image: %v", err)
	}
	return ref
}

// TestPull_CraneFallback_ConvertsDockerManifestToOCI is a regression test for
// the bug where oras correctly detected a Docker schema2 manifest and fell
// back to go-containerregistry, but the fallback then wrote the original
// Docker media types straight through to the on-disk OCI layout — which
// umoci (strictly OCI-only) rejected one step later. Pull() must normalize
// the manifest, config descriptor, and layer descriptors to their OCI
// equivalents as part of the fallback.
func TestPull_CraneFallback_ConvertsDockerManifestToOCI(t *testing.T) {
	addr := startTestRegistry(t)
	image := pushDockerSchema2Image(t, addr, "test/schema2image")

	outputDir := t.TempDir()
	cfg := &unpacker.Config{
		Image:     image,
		OutputDir: outputDir,
		Insecure:  true,
		Creds:     &unpacker.Credentials{Public: true},
	}

	if err := unpacker.Pull(context.Background(), cfg); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	// index.json's manifest descriptor must declare the OCI manifest media type.
	indexData, err := os.ReadFile(filepath.Join(outputDir, "tmp", "index.json"))
	if err != nil {
		t.Fatalf("read index.json: %v", err)
	}
	var index struct {
		Manifests []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(indexData, &index); err != nil {
		t.Fatalf("parse index.json: %v", err)
	}
	if len(index.Manifests) != 1 {
		t.Fatalf("expected 1 manifest in index.json, got %d", len(index.Manifests))
	}
	if got := index.Manifests[0].MediaType; got != string(types.OCIManifestSchema1) {
		t.Errorf("index.json manifest mediaType = %q, want %q", got, types.OCIManifestSchema1)
	}

	// The manifest blob itself must declare OCI media types throughout.
	digestParts := strings.SplitN(index.Manifests[0].Digest, ":", 2)
	if len(digestParts) != 2 {
		t.Fatalf("unexpected digest format: %s", index.Manifests[0].Digest)
	}
	blobPath := filepath.Join(outputDir, "tmp", "blobs", digestParts[0], digestParts[1])
	blobData, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("read manifest blob: %v", err)
	}
	var manifest struct {
		MediaType string `json:"mediaType"`
		Config    struct {
			MediaType string `json:"mediaType"`
		} `json:"config"`
		Layers []struct {
			MediaType string `json:"mediaType"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(blobData, &manifest); err != nil {
		t.Fatalf("parse manifest blob: %v", err)
	}
	if manifest.MediaType != string(types.OCIManifestSchema1) {
		t.Errorf("manifest mediaType = %q, want %q", manifest.MediaType, types.OCIManifestSchema1)
	}
	if manifest.Config.MediaType != string(types.OCIConfigJSON) {
		t.Errorf("config mediaType = %q, want %q", manifest.Config.MediaType, types.OCIConfigJSON)
	}
	if len(manifest.Layers) != 1 || manifest.Layers[0].MediaType != string(types.OCILayer) {
		t.Errorf("layer mediaType = %+v, want single layer of %q", manifest.Layers, types.OCILayer)
	}

	// manifest.json (used by Unpack()'s mediatype routing) must match.
	outerData, err := os.ReadFile(filepath.Join(outputDir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest.json: %v", err)
	}
	var outer struct {
		MediaType string `json:"mediaType"`
	}
	if err := json.Unmarshal(outerData, &outer); err != nil {
		t.Fatalf("parse manifest.json: %v", err)
	}
	if outer.MediaType != string(types.OCIManifestSchema1) {
		t.Errorf("manifest.json mediaType = %q, want %q", outer.MediaType, types.OCIManifestSchema1)
	}
}

func TestPull_ManifestWritten(t *testing.T) {
	addr := startTestRegistry(t)
	image := pushTestImage(t, addr, "test/artifact")

	outputDir := t.TempDir()
	cfg := &unpacker.Config{
		Image:     image,
		OutputDir: outputDir,
		Insecure:  true,
		Creds:     &unpacker.Credentials{Public: true},
	}

	if err := unpacker.Pull(context.Background(), cfg); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	// either oras or crane fallback — manifest.json must exist
	manifestPath := filepath.Join(outputDir, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("manifest.json not written: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Errorf("manifest.json is not valid JSON: %v", err)
	}
}
