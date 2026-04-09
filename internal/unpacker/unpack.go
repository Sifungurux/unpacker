package unpacker

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Config holds all runtime configuration passed from the CLI.
type Config struct {
	Image        string
	OutputDir    string
	AllowedTypes []string
	Insecure     bool
	Creds        *Credentials
}

type manifest struct {
	Layers []layer `json:"layers"`
}

type layer struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
}

// Unpack reads manifest.json from outputDir and extracts the artifact to outputDir/image/.
// It selects one of three paths:
//   - Path 1: tar extract (ORAS artifact, mediaType matches allowed list)
//   - Path 2: umoci exec (OCI image)
//   - Path 3: file copy (plain files, no tarball or blobs dir found)
func Unpack(cfg *Config) error {
	tmpDir := filepath.Join(cfg.OutputDir, "tmp")
	imageDir := filepath.Join(cfg.OutputDir, "image")

	var mediaType, digest string
	var useAllowedType bool

	manifestPath := filepath.Join(cfg.OutputDir, "manifest.json")
	if data, err := os.ReadFile(manifestPath); err == nil {
		var m manifest
		if err := json.Unmarshal(data, &m); err != nil {
			return fmt.Errorf("parse manifest: %w", err)
		}
		if len(m.Layers) > 0 {
			mediaType = m.Layers[0].MediaType
			digest = m.Layers[0].Digest
			for _, allowed := range cfg.AllowedTypes {
				if strings.Contains(mediaType, allowed) {
					useAllowedType = true
					break
				}
			}
		}
	} else {
		log.Printf("manifest.json not found, proceeding without mediatype information: %v", err)
	}

	blobsDir := filepath.Join(tmpDir, "blobs", "sha256")
	hasTar := firstFileIsTar(tmpDir)
	hasBlobs := dirExists(blobsDir)

	if hasTar || hasBlobs {
		if useAllowedType {
			return extractOrasArtifact(tmpDir, imageDir, digest)
		}
		return runUmoci(tmpDir, imageDir)
	}

	return CopyFiles(tmpDir, imageDir)
}

func firstFileIsTar(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			return false
		}
		buf := make([]byte, 2)
		n, _ := f.Read(buf)
		f.Close()
		return n == 2 && buf[0] == 0x1f && buf[1] == 0x8b
	}
	return false
}

func dirExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func extractOrasArtifact(tmpDir, imageDir, digest string) error {
	parts := strings.SplitN(digest, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid digest format: %s", digest)
	}
	blobName := parts[1]

	srcPath := filepath.Join(tmpDir, digest)
	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		srcPath = filepath.Join(tmpDir, blobName)
	}

	cleanTmp := filepath.Clean(tmpDir) + string(os.PathSeparator)
	if !strings.HasPrefix(filepath.Clean(srcPath)+string(os.PathSeparator), cleanTmp) {
		return fmt.Errorf("digest resolves outside tmp dir: %s", digest)
	}

	if err := os.MkdirAll(imageDir, 0755); err != nil {
		return fmt.Errorf("create image dir: %w", err)
	}
	return ExtractTar(srcPath, imageDir)
}

// ExtractTar extracts a .tar.gz file to destDir. Exported for testing.
func ExtractTar(tarPath, destDir string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return fmt.Errorf("open tar: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("open gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	cleanDest := filepath.Clean(destDir) + string(os.PathSeparator)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}

		target := filepath.Join(destDir, filepath.Clean(hdr.Name))
		if !strings.HasPrefix(target, cleanDest) {
			return fmt.Errorf("illegal path in tar: %s", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, hdr.FileInfo().Mode()); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, hdr.FileInfo().Mode())
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		}
	}
	return nil
}

func runUmoci(tmpDir, imageDir string) error {
	args := []string{"--log", "error", "raw", "unpack", "--rootless", "--image", tmpDir, imageDir}
	cmd := exec.Command("umoci", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("umoci failed: %w\n%s", err, out)
	}
	if len(out) > 0 {
		log.Printf("umoci: %s", out)
	}
	return nil
}

// CopyFiles copies all regular files from srcDir to destDir. Exported for testing.
func CopyFiles(srcDir, destDir string) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("create dest dir: %w", err)
	}
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("read src dir: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		src, err := os.Open(filepath.Join(srcDir, entry.Name()))
		if err != nil {
			return err
		}
		dst, err := os.Create(filepath.Join(destDir, entry.Name()))
		if err != nil {
			src.Close()
			return err
		}
		if _, err := io.Copy(dst, src); err != nil {
			src.Close()
			dst.Close()
			return err
		}
		if err := dst.Sync(); err != nil {
			src.Close()
			dst.Close()
			return err
		}
		src.Close()
		dst.Close()
		log.Printf("copied %s", entry.Name())
	}
	return nil
}
