package unpacker_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
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
