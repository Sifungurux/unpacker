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
	Image         string
	OutputDir     string
	AllowedTypes  []string
	Insecure      bool
	WithReferrers bool
	Creds         *Credentials
	Limits
}

// Default extraction limits. A tar.gz says nothing trustworthy about how much
// it expands to, so extraction is bounded by what is actually written rather
// than by any header the archive declares.
const (
	DefaultMaxTotalBytes int64 = 1 << 30   // 1 GiB
	DefaultMaxFileBytes  int64 = 512 << 20 // 512 MiB
	DefaultMaxEntries    int   = 100_000
)

// Limits bounds what one archive may expand to on disk.
type Limits struct {
	MaxTotalBytes int64
	MaxFileBytes  int64
	MaxEntries    int
}

// withDefaults fills in any unset field. Zero and negative both mean "not
// configured" and resolve to the default — these are a safety control, so an
// unset value must never mean unlimited.
func (l Limits) withDefaults() Limits {
	if l.MaxTotalBytes <= 0 {
		l.MaxTotalBytes = DefaultMaxTotalBytes
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = DefaultMaxFileBytes
	}
	if l.MaxEntries <= 0 {
		l.MaxEntries = DefaultMaxEntries
	}
	return l
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

	// hasTar: oras file store saved blob by annotated filename (e.g. chart-1.0.0.tgz)
	// hasBlobs: OCI layout with blobs/sha256/ structure (crane output or oras with digest naming)
	if hasTar {
		if useAllowedType {
			return extractFirstTar(tmpDir, imageDir, cfg.Limits)
		}
		return runUmoci(tmpDir, imageDir)
	}
	if hasBlobs {
		if useAllowedType {
			return extractOrasArtifact(tmpDir, imageDir, digest, cfg.Limits)
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

// extractFirstTar extracts the first regular file in tmpDir as a tar.gz.
// Used when oras stores the blob under its annotated filename rather than by digest.
func extractFirstTar(tmpDir, imageDir string, lim Limits) error {
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return fmt.Errorf("read tmp dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := os.MkdirAll(imageDir, 0755); err != nil {
			return fmt.Errorf("create image dir: %w", err)
		}
		return ExtractTar(filepath.Join(tmpDir, e.Name()), imageDir, lim)
	}
	return fmt.Errorf("no file found in tmp dir")
}

func extractOrasArtifact(tmpDir, imageDir, digest string, lim Limits) error {
	parts := strings.SplitN(digest, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid digest format: %s", digest)
	}
	algo, blobName := parts[0], parts[1]

	// The blob is named by its digest when oras pulled it, by its hex under
	// blobs/<algo>/ when the crane fallback wrote an OCI layout. The layout
	// case is reached whenever oras fails on an artifact whose media type is
	// allowed — without it, extraction fails with a confusing "no such file".
	srcPath := filepath.Join(tmpDir, digest)
	for _, candidate := range []string{
		filepath.Join(tmpDir, blobName),
		filepath.Join(tmpDir, "blobs", algo, blobName),
	} {
		if _, err := os.Stat(srcPath); !os.IsNotExist(err) {
			break
		}
		srcPath = candidate
	}

	cleanTmp := filepath.Clean(tmpDir) + string(os.PathSeparator)
	if !strings.HasPrefix(filepath.Clean(srcPath)+string(os.PathSeparator), cleanTmp) {
		return fmt.Errorf("digest resolves outside tmp dir: %s", digest)
	}

	if err := os.MkdirAll(imageDir, 0755); err != nil {
		return fmt.Errorf("create image dir: %w", err)
	}
	return ExtractTar(srcPath, imageDir, lim)
}

// ExtractTar extracts a .tar.gz file to destDir, bounded by lim. Exported for testing.
func ExtractTar(tarPath, destDir string, lim Limits) error {
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
	lim = lim.withDefaults()

	var entries int
	var total int64

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}

		entries++
		if entries > lim.MaxEntries {
			return fmt.Errorf("archive has more than %d entries (--max-entries)", lim.MaxEntries)
		}

		target := filepath.Join(destDir, filepath.Clean(hdr.Name))
		// skip the root directory entry (e.g. "./") — it resolves to destDir itself
		if target == filepath.Clean(destDir) {
			continue
		}
		if !strings.HasPrefix(target, cleanDest) {
			return fmt.Errorf("illegal path in tar: %s", hdr.Name)
		}

		// Perm() keeps only the 0777 bits, dropping setuid/setgid/sticky:
		// an archive must not be able to plant a privileged binary.
		mode := hdr.FileInfo().Mode().Perm()

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, mode); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			// Read one byte past whichever limit binds first, so an
			// over-large entry is detected without writing it out in full.
			// hdr.Size is not consulted: it is attacker-controlled, and only
			// the bytes actually written bound what lands on disk.
			allowed := lim.MaxFileBytes
			if remaining := lim.MaxTotalBytes - total; remaining < allowed {
				allowed = remaining
			}
			n, copyErr := io.Copy(out, io.LimitReader(tr, allowed+1))
			out.Close()
			if copyErr != nil {
				return copyErr
			}
			switch {
			case n > lim.MaxFileBytes:
				return fmt.Errorf("file %s exceeds the %d byte limit for a single file (--max-file-bytes)",
					hdr.Name, lim.MaxFileBytes)
			case total+n > lim.MaxTotalBytes:
				return fmt.Errorf("archive expands past the %d byte total limit (--max-total-bytes)",
					lim.MaxTotalBytes)
			}
			total += n
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
