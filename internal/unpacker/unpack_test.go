package unpacker_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
