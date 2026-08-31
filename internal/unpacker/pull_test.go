package unpacker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

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
	addr := startRegistry(t, "")
	image := pushTestImage(t, addr, "test/myimage")

	outputDir := t.TempDir()
	cfg := &Config{
		Image:     image,
		OutputDir: outputDir,
		Insecure:  true,
		Creds:     &Credentials{Public: true},
	}

	if _, err := Pull(context.Background(), cfg); err != nil {
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
	addr := startRegistry(t, "")
	image := pushDockerSchema2Image(t, addr, "test/schema2image")

	outputDir := t.TempDir()
	cfg := &Config{
		Image:     image,
		OutputDir: outputDir,
		Insecure:  true,
		Creds:     &Credentials{Public: true},
	}

	resolved, err := Pull(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	// The digest Pull reports has to be the one the registry serves — that is
	// what referrers attach to. This fallback rewrites the manifest's media
	// types, which changes its bytes and therefore its digest, so returning
	// the normalised digest instead would be silently wrong.
	served, err := crane.Digest(image, crane.Insecure)
	if err != nil {
		t.Fatalf("crane.Digest: %v", err)
	}
	if resolved != served {
		t.Errorf("Pull returned digest %q, want the digest the registry serves %q", resolved, served)
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
	if resolved == index.Manifests[0].Digest {
		t.Errorf("Pull returned the normalised on-disk digest %q; the registry serves a different one", resolved)
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
	addr := startRegistry(t, "")
	image := pushTestImage(t, addr, "test/artifact")

	outputDir := t.TempDir()
	cfg := &Config{
		Image:     image,
		OutputDir: outputDir,
		Insecure:  true,
		Creds:     &Credentials{Public: true},
	}

	if _, err := Pull(context.Background(), cfg); err != nil {
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

// pushOCIIndex pushes a multi-platform OCI image INDEX and returns the ref.
//
// This is the shape that matters most in practice: nearly every modern
// public image is published as an index, and `docker pull` resolves the
// platform for you so nobody notices what the manifest actually is.
func pushOCIIndex(t *testing.T, registryAddr, repo string) string {
	t.Helper()
	ref := registryAddr + "/" + repo + ":latest"

	build := func(content, arch string) v1.Image {
		layer := static.NewLayer([]byte(content), types.OCILayer)
		img, err := mutate.AppendLayers(empty.Image, layer)
		if err != nil {
			t.Fatalf("build test image: %v", err)
		}
		img = mutate.MediaType(img, types.OCIManifestSchema1)
		img = mutate.ConfigMediaType(img, types.OCIConfigJSON)
		return img
	}

	idx := mutate.IndexMediaType(empty.Index, types.OCIImageIndex)
	for _, p := range []struct{ arch, content string }{
		{"amd64", "amd64 content"},
		{"arm64", "arm64 content"},
	} {
		img := build(p.content, p.arch)
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{
			Add:        img,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: p.arch}},
		})
	}

	tag, err := name.NewTag(ref, name.Insecure)
	if err != nil {
		t.Fatalf("parse ref: %v", err)
	}
	if err := remote.WriteIndex(tag, idx); err != nil {
		t.Fatalf("push index: %v", err)
	}
	return ref
}

// pushOCIImage pushes a single-platform OCI image MANIFEST (not an index,
// and not a docker manifest) and returns the ref.
func pushOCIImage(t *testing.T, registryAddr, repo string) string {
	t.Helper()
	ref := registryAddr + "/" + repo + ":latest"
	layer := static.NewLayer([]byte("oci content"), types.OCILayer)
	img, err := mutate.AppendLayers(empty.Image, layer)
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

// TestPull_OCIIndex_ExtractsFiles is the regression test for the worst
// failure this tool has had: an OCI image index pulled successfully, exit
// code 0, and NOTHING on disk.
//
// oras only declined manifests whose media type began with
// "application/vnd.docker.". An OCI index passed that check, so oras kept
// the pull -- then parsed the index for a "layers" field it does not have
// (an index has "manifests"), found none, downloaded no blobs, and
// returned success.
//
// Measured against a real fleet before this was fixed: 47 of 95 public
// image references were OCI indexes, and every one of them unpacked to an
// empty directory. A malware scanner reading that directory reports the
// image clean.
func TestPull_OCIIndex_ExtractsFiles(t *testing.T) {
	addr := startRegistry(t, "")
	image := pushOCIIndex(t, addr, "test/ociindex")

	outputDir := t.TempDir()
	cfg := &Config{
		Image:     image,
		OutputDir: outputDir,
		Insecure:  true,
		Creds:     &Credentials{Public: true},
	}

	if _, err := Pull(context.Background(), cfg); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	// The blobs are what prove it. An index that resolved to nothing
	// leaves tmp/ empty while still returning a digest.
	blobs := filepath.Join(outputDir, "tmp", "blobs", "sha256")
	entries, err := os.ReadDir(blobs)
	if err != nil {
		t.Fatalf("no OCI layout written for an index pull (%v) -- the pull produced nothing", err)
	}
	if len(entries) == 0 {
		t.Fatal("OCI layout contains no blobs -- the index resolved to nothing and the pull still reported success")
	}
}

// TestPull_OCIImageManifest_ProducesAnUnpackableLayout covers the other
// half of the same routing bug. An OCI image manifest is not a docker
// manifest, so oras kept it and downloaded the layer blobs as bare files
// -- a directory umoci then rejects with "invalid image detected",
// because it is not an OCI layout.
func TestPull_OCIImageManifest_ProducesAnUnpackableLayout(t *testing.T) {
	addr := startRegistry(t, "")
	image := pushOCIImage(t, addr, "test/ociimage")

	outputDir := t.TempDir()
	cfg := &Config{
		Image:     image,
		OutputDir: outputDir,
		Insecure:  true,
		Creds:     &Credentials{Public: true},
	}

	if _, err := Pull(context.Background(), cfg); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	// oci-layout is the file umoci checks first. Its absence is exactly
	// the "invalid image detected" failure, caught here rather than one
	// step later in an error message that names neither the media type
	// nor the reference.
	if _, err := os.Stat(filepath.Join(outputDir, "tmp", "oci-layout")); err != nil {
		t.Fatalf("no oci-layout written for an OCI image manifest (%v) -- umoci rejects this directory", err)
	}
}

// The crane path handles every Docker manifest, every index and every image
// with an image config — the majority of pulls and the largest inputs. Bounding
// only the oras path would have left the limits covering the smaller half.
func TestPull_CraneRejectsOversizedLayerBeforeWriting(t *testing.T) {
	addr := startRegistry(t, "")
	image := pushOCIIndex(t, addr, "test/craneoversize")

	outputDir := t.TempDir()
	cfg := &Config{
		Image:     image,
		OutputDir: outputDir,
		Insecure:  true,
		Creds:     &Credentials{Public: true},
		Limits:    Limits{MaxFileBytes: 1, MaxTotalBytes: 1 << 20, MaxEntries: 10},
	}

	_, err := Pull(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected a layer over --max-file-bytes to be rejected on the crane path")
	}
	if !strings.Contains(err.Error(), "--max-file-bytes") {
		t.Errorf("error = %q, want it to name --max-file-bytes", err)
	}

	// crane.Pull is lazy, so the rejection lands before any blob is written.
	if _, statErr := os.Stat(filepath.Join(outputDir, "tmp", "blobs")); !os.IsNotExist(statErr) {
		t.Errorf("blobs/ exists; want nothing written for a rejected pull")
	}
}
