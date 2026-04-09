package unpacker_test

import (
	"archive/tar"
	"compress/gzip"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/energinet/unpacker/internal/unpacker"
)

// makeTarGz creates an in-memory .tar.gz with a single file.
func makeTarGz(t *testing.T, filename, content string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	body := []byte(content)
	tw.WriteHeader(&tar.Header{
		Name: filename,
		Mode: 0644,
		Size: int64(len(body)),
	})
	tw.Write(body)
	tw.Close()
	gz.Close()

	f, err := os.CreateTemp(t.TempDir(), "*.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	f.Write(buf.Bytes())
	f.Close()
	return f.Name()
}

func TestExtractTar(t *testing.T) {
	tarPath := makeTarGz(t, "hello.txt", "hello world")
	destDir := t.TempDir()

	if err := unpacker.ExtractTar(tarPath, destDir); err != nil {
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
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "../escape.txt", Mode: 0644, Size: 3})
	tw.Write([]byte("bad"))
	tw.Close()
	gz.Close()

	f, _ := os.CreateTemp(t.TempDir(), "*.tar.gz")
	f.Write(buf.Bytes())
	f.Close()

	err := unpacker.ExtractTar(f.Name(), t.TempDir())
	if err == nil {
		t.Error("expected path traversal error, got nil")
	}
}
