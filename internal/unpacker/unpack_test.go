package unpacker_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/energinet/unpacker/internal/unpacker"
)

// tarEntry describes one regular file to place in a test archive.
type tarEntry struct {
	name string
	mode int64
	body []byte
}

// makeTarGzEntries writes an in-memory .tar.gz containing entries and returns its path.
func makeTarGzEntries(t *testing.T, entries ...tarEntry) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0644
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     e.name,
			Mode:     mode,
			Size:     int64(len(e.body)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.CreateTemp(t.TempDir(), "*.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return f.Name()
}

// makeTarGz creates an in-memory .tar.gz with a single file.
func makeTarGz(t *testing.T, filename, content string) string {
	t.Helper()
	return makeTarGzEntries(t, tarEntry{name: filename, body: []byte(content)})
}

func TestExtractTar(t *testing.T) {
	tarPath := makeTarGz(t, "hello.txt", "hello world")
	destDir := t.TempDir()

	// zero-value Limits: every field falls back to its default
	if err := unpacker.ExtractTar(tarPath, destDir, unpacker.Limits{}); err != nil {
		t.Fatalf("ExtractTar: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(destDir, "hello.txt"))
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if string(got) != "hello world" {
		t.Errorf("expected 'hello world', got %q", got)
	}
}

func TestCopyFiles(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("aaa"), 0644)
	os.WriteFile(filepath.Join(src, "b.txt"), []byte("bbb"), 0644)

	dest := t.TempDir()
	if err := unpacker.CopyFiles(src, dest); err != nil {
		t.Fatalf("CopyFiles: %v", err)
	}

	for _, name := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(dest, name)); err != nil {
			t.Errorf("expected %s in dest, got: %v", name, err)
		}
	}
}

func TestExtractTar_PathTraversal(t *testing.T) {
	tarPath := makeTarGzEntries(t, tarEntry{name: "../escape.txt", body: []byte("bad")})

	err := unpacker.ExtractTar(tarPath, t.TempDir(), unpacker.Limits{})
	if err == nil {
		t.Error("expected path traversal error, got nil")
	}
}

// A .tar.gz declares nothing trustworthy about how far it expands, so these
// cover the three ways an archive can try to fill the disk. Each limit is set
// small so the test stays fast; the production defaults are much larger.

func TestExtractTar_RejectsOversizedFile(t *testing.T) {
	const limit = 1000
	tarPath := makeTarGzEntries(t, tarEntry{name: "big.bin", body: bytes.Repeat([]byte("A"), 5000)})
	destDir := t.TempDir()

	err := unpacker.ExtractTar(tarPath, destDir, unpacker.Limits{
		MaxFileBytes:  limit,
		MaxTotalBytes: 1 << 20,
		MaxEntries:    10,
	})
	if err == nil {
		t.Fatal("expected an error for a file over the single-file limit")
	}
	if !strings.Contains(err.Error(), "--max-file-bytes") {
		t.Errorf("error = %q, want it to name --max-file-bytes", err)
	}

	// the oversized entry must not have been written out in full
	if fi, statErr := os.Stat(filepath.Join(destDir, "big.bin")); statErr == nil && fi.Size() > limit+1 {
		t.Errorf("wrote %d bytes for an entry limited to %d", fi.Size(), limit)
	}
}

func TestExtractTar_RejectsOversizedTotal(t *testing.T) {
	// each entry is within the per-file limit; only the sum is over
	tarPath := makeTarGzEntries(t,
		tarEntry{name: "a.bin", body: bytes.Repeat([]byte("A"), 400)},
		tarEntry{name: "b.bin", body: bytes.Repeat([]byte("B"), 400)},
		tarEntry{name: "c.bin", body: bytes.Repeat([]byte("C"), 400)},
	)

	err := unpacker.ExtractTar(tarPath, t.TempDir(), unpacker.Limits{
		MaxFileBytes:  1000,
		MaxTotalBytes: 1000,
		MaxEntries:    10,
	})
	if err == nil {
		t.Fatal("expected an error for an archive over the total limit")
	}
	if !strings.Contains(err.Error(), "--max-total-bytes") {
		t.Errorf("error = %q, want it to name --max-total-bytes", err)
	}
}

func TestExtractTar_RejectsTooManyEntries(t *testing.T) {
	var entries []tarEntry
	for i := range 6 {
		entries = append(entries, tarEntry{name: fmt.Sprintf("f%d.txt", i), body: []byte("x")})
	}
	tarPath := makeTarGzEntries(t, entries...)

	err := unpacker.ExtractTar(tarPath, t.TempDir(), unpacker.Limits{
		MaxFileBytes:  1 << 20,
		MaxTotalBytes: 1 << 20,
		MaxEntries:    3,
	})
	if err == nil {
		t.Fatal("expected an error for an archive over the entry limit")
	}
	if !strings.Contains(err.Error(), "--max-entries") {
		t.Errorf("error = %q, want it to name --max-entries", err)
	}
}

// TestExtractTar_StripsSpecialModeBits guards against an archive planting a
// setuid file.
//
// Two things make this assertion easy to write so that it can never fail:
//   - the entry is empty on purpose. Linux clears S_ISUID when an unprivileged
//     process writes to a file, so an entry with content loses the bit to the
//     kernel and the test would pass even with the masking removed.
//   - it only bites on Linux, where unpacker actually runs and CI builds.
//     macOS drops S_ISUID on create, so this passes there either way.
//
// Verified by removing the mask and watching this fail in a Linux container.
func TestExtractTar_StripsSpecialModeBits(t *testing.T) {
	tarPath := makeTarGzEntries(t, tarEntry{name: "rooted", mode: 0o4755})
	destDir := t.TempDir()

	if err := unpacker.ExtractTar(tarPath, destDir, unpacker.Limits{}); err != nil {
		t.Fatalf("ExtractTar: %v", err)
	}

	fi, err := os.Stat(filepath.Join(destDir, "rooted"))
	if err != nil {
		t.Fatalf("stat extracted file: %v", err)
	}
	if special := fi.Mode() & (os.ModeSetuid | os.ModeSetgid | os.ModeSticky); special != 0 {
		t.Errorf("extracted mode %v kept special bits %v, want them stripped", fi.Mode(), special)
	}
	if perm := fi.Mode().Perm(); perm&0o111 == 0 {
		t.Errorf("extracted mode %v lost its permission bits entirely", fi.Mode())
	}
}

// stageArtifact writes a manifest.json and the matching blobs into a fresh
// output dir, the way Pull would leave them for Unpack.
func stageArtifact(t *testing.T, layers []map[string]any, blobs map[string]string) string {
	t.Helper()
	outputDir := t.TempDir()
	tmpDir := filepath.Join(outputDir, "tmp")
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		t.Fatal(err)
	}
	for name, tarPath := range blobs {
		data, err := os.ReadFile(tarPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmpDir, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := json.Marshal(map[string]any{"layers": layers})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, "manifest.json"), manifest, 0644); err != nil {
		t.Fatal(err)
	}
	return outputDir
}

func fluxLayer(title string) map[string]any {
	return map[string]any{
		"mediaType":   "application/vnd.cncf.flux.content.v1.tar+gzip",
		"digest":      "sha256:" + strings.Repeat("a", 64),
		"annotations": map[string]string{"org.opencontainers.image.title": title},
	}
}

// TestUnpack_ExtractsEveryMatchingLayer is a regression test for silent content
// loss: only the first layer used to be extracted, so a multi-blob artifact
// lost everything after it — with no error and a zero exit code.
func TestUnpack_ExtractsEveryMatchingLayer(t *testing.T) {
	one := makeTarGzEntries(t, tarEntry{name: "first.txt", body: []byte("from layer one")})
	two := makeTarGzEntries(t, tarEntry{name: "second.txt", body: []byte("from layer two")})

	l1, l2 := fluxLayer("layer1.tgz"), fluxLayer("layer2.tgz")
	l2["digest"] = "sha256:" + strings.Repeat("b", 64)

	outputDir := stageArtifact(t, []map[string]any{l1, l2},
		map[string]string{"layer1.tgz": one, "layer2.tgz": two})

	cfg := &unpacker.Config{OutputDir: outputDir, AllowedTypes: []string{"flux", "helm"}}
	if err := unpacker.Unpack(cfg); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	for name, want := range map[string]string{
		"first.txt":  "from layer one",
		"second.txt": "from layer two",
	} {
		got, err := os.ReadFile(filepath.Join(outputDir, "image", name))
		if err != nil {
			t.Errorf("%s missing from image/: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestUnpack_LaterLayerWins(t *testing.T) {
	one := makeTarGzEntries(t, tarEntry{name: "shared.txt", body: []byte("old")})
	two := makeTarGzEntries(t, tarEntry{name: "shared.txt", body: []byte("new")})

	l1, l2 := fluxLayer("layer1.tgz"), fluxLayer("layer2.tgz")
	l2["digest"] = "sha256:" + strings.Repeat("b", 64)

	outputDir := stageArtifact(t, []map[string]any{l1, l2},
		map[string]string{"layer1.tgz": one, "layer2.tgz": two})

	cfg := &unpacker.Config{OutputDir: outputDir, AllowedTypes: []string{"flux"}}
	if err := unpacker.Unpack(cfg); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(outputDir, "image", "shared.txt"))
	if err != nil {
		t.Fatalf("read shared.txt: %v", err)
	}
	if string(got) != "new" {
		t.Errorf("shared.txt = %q, want the later layer's %q", got, "new")
	}
}

// The total-bytes budget covers the artifact, not each layer: two layers that
// each fit must still fail together when their sum does not.
func TestUnpack_TotalLimitIsSharedAcrossLayers(t *testing.T) {
	one := makeTarGzEntries(t, tarEntry{name: "a.bin", body: bytes.Repeat([]byte("A"), 400)})
	two := makeTarGzEntries(t, tarEntry{name: "b.bin", body: bytes.Repeat([]byte("B"), 400)})

	l1, l2 := fluxLayer("layer1.tgz"), fluxLayer("layer2.tgz")
	l2["digest"] = "sha256:" + strings.Repeat("b", 64)

	outputDir := stageArtifact(t, []map[string]any{l1, l2},
		map[string]string{"layer1.tgz": one, "layer2.tgz": two})

	cfg := &unpacker.Config{
		OutputDir:    outputDir,
		AllowedTypes: []string{"flux"},
		Limits:       unpacker.Limits{MaxTotalBytes: 600, MaxFileBytes: 500, MaxEntries: 10},
	}
	err := unpacker.Unpack(cfg)
	if err == nil {
		t.Fatal("expected the shared byte budget to be exceeded across two layers")
	}
	if !strings.Contains(err.Error(), "--max-total-bytes") {
		t.Errorf("error = %q, want it to name --max-total-bytes", err)
	}
}
