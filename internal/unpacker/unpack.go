package unpacker

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Config holds all runtime configuration passed from the CLI.
type Config struct {
	Image        string
	OutputDir    string
	AllowedTypes []string
	Insecure     bool
	// AllowInsecureCredentials permits basic auth over plain HTTP, which is
	// otherwise refused because the password would travel in the clear.
	AllowInsecureCredentials bool
	WithReferrers            bool
	Creds                    *Credentials
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
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Annotations map[string]string `json:"annotations"`
}

// Unpack reads manifest.json from outputDir and extracts the artifact to outputDir/image/.
// It selects one of three paths:
//   - Path 1: tar extract (ORAS artifact, mediaType matches allowed list)
//   - Path 2: umoci exec (OCI image)
//   - Path 3: file copy (plain files, no tarball or blobs dir found)
//
// The output directory appears only once the unpack has fully succeeded: a
// half-extracted image/ left behind by a failed run reads as a complete
// artifact to anything that looks at the directory rather than the exit code.
func Unpack(cfg *Config) error {
	imageDir := filepath.Join(cfg.OutputDir, "image")
	stagingDir := filepath.Join(cfg.OutputDir, ".image.staging")

	// A previous crash could have left staging behind; umoci also refuses to
	// unpack into a directory that already exists.
	if err := os.RemoveAll(stagingDir); err != nil {
		return fmt.Errorf("clear staging dir: %w", err)
	}

	if err := unpackInto(cfg, stagingDir); err != nil {
		os.RemoveAll(stagingDir) //nolint:errcheck // already returning an error
		return err
	}

	// tmp/ is deliberately left in place either way, for debugging.
	if err := os.RemoveAll(imageDir); err != nil {
		return fmt.Errorf("replace previous image dir: %w", err)
	}
	if err := os.Rename(stagingDir, imageDir); err != nil {
		return fmt.Errorf("publish image dir: %w", err)
	}
	return nil
}

func unpackInto(cfg *Config, imageDir string) error {
	tmpDir := filepath.Join(cfg.OutputDir, "tmp")
	// Resolved once here; everything below takes limits that are already
	// defaulted, so no inner step can re-apply them to a running budget.
	lim := cfg.Limits.withDefaults()

	var allowed []layer

	manifestPath := filepath.Join(cfg.OutputDir, "manifest.json")
	if data, err := os.ReadFile(manifestPath); err == nil {
		var m manifest
		if err := json.Unmarshal(data, &m); err != nil {
			return fmt.Errorf("parse manifest: %w", err)
		}
		// Every matching layer is extracted, not just the first. An artifact
		// can carry several blobs, and dropping the rest loses content with no
		// error — the worst failure mode for something feeding an audit.
		for _, l := range m.Layers {
			if allowedMediaType(l.MediaType, cfg.AllowedTypes) {
				allowed = append(allowed, l)
			}
		}
	} else {
		log.Printf("manifest.json not found, proceeding without mediatype information: %v", err)
	}

	blobsDir := filepath.Join(tmpDir, "blobs", "sha256")
	hasTar := firstFileIsArchive(tmpDir)
	hasBlobs := dirExists(blobsDir)

	// hasTar: oras file store saved blob by annotated filename (e.g. chart-1.0.0.tgz)
	// hasBlobs: OCI layout with blobs/sha256/ structure (crane output or oras with digest naming)
	if hasTar || hasBlobs {
		if len(allowed) > 0 {
			return extractLayers(tmpDir, imageDir, allowed, lim)
		}
		return runUmoci(tmpDir, imageDir)
	}

	return CopyFiles(tmpDir, imageDir, lim)
}

// allowedMediaType reports whether mediaType is covered by --mediatype.
//
// A value containing "/" is matched in full, so --mediatype
// application/vnd.cncf.helm.chart.content.v1.tar+gzip means exactly that.
// A bare word like "helm" matches a whole component of the media type —
// "helm" covers application/vnd.cncf.helm.chart.content.v1.tar+gzip but not
// application/vnd.example.nothelm.v1+json, which a plain substring test let
// through.
func allowedMediaType(mediaType string, allowedTypes []string) bool {
	for _, allowed := range allowedTypes {
		if allowed == "" {
			continue
		}
		if strings.Contains(allowed, "/") {
			if strings.EqualFold(mediaType, allowed) {
				return true
			}
			continue
		}
		for _, part := range mediaTypeComponents(mediaType) {
			if strings.EqualFold(part, allowed) {
				return true
			}
		}
	}
	return false
}

// mediaTypeComponents splits a media type into the words that make it up:
// application/vnd.cncf.flux.content.v1.tar+gzip becomes application, vnd,
// cncf, flux, content, v1, tar, gzip.
func mediaTypeComponents(mediaType string) []string {
	return strings.FieldsFunc(mediaType, func(r rune) bool {
		switch r {
		case '/', '.', '+', '-', ';', ' ', ',', '_':
			return true
		}
		return false
	})
}

// extractLayers extracts every allowed layer into imageDir in manifest order,
// so a later layer overwrites an earlier one exactly as an image would.
// lim must already be resolved by withDefaults.
func extractLayers(tmpDir, imageDir string, layers []layer, lim Limits) error {
	if err := os.MkdirAll(imageDir, 0755); err != nil {
		return fmt.Errorf("create image dir: %w", err)
	}
	// The byte budget is shared across layers: --max-total-bytes bounds what
	// the artifact expands to, not what each of its layers does.
	budget := lim
	for _, l := range layers {
		src, err := blobPath(tmpDir, l)
		if err != nil {
			return err
		}
		written, err := extractTar(src, imageDir, budget)
		if err != nil {
			return fmt.Errorf("extract layer %s: %w", l.Digest, err)
		}
		budget.MaxTotalBytes -= written
	}
	if len(layers) > 1 {
		log.Printf("extracted %d layers", len(layers))
	}
	return nil
}

// blobPath finds where Pull put a layer's blob. oras names it by the title
// annotation when there is one and by hex digest otherwise; the crane fallback
// writes an OCI layout instead.
func blobPath(tmpDir string, l layer) (string, error) {
	parts := strings.SplitN(l.Digest, ":", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid digest format: %s", l.Digest)
	}
	algo, hex := parts[0], parts[1]

	var candidates []string
	if title := l.Annotations["org.opencontainers.image.title"]; title != "" {
		candidates = append(candidates, filepath.Join(tmpDir, filepath.Base(filepath.Clean("/"+title))))
	}
	candidates = append(candidates,
		filepath.Join(tmpDir, l.Digest),
		filepath.Join(tmpDir, hex),
		filepath.Join(tmpDir, "blobs", algo, hex),
	)

	cleanTmp := filepath.Clean(tmpDir) + string(os.PathSeparator)
	for _, c := range candidates {
		if !strings.HasPrefix(filepath.Clean(c)+string(os.PathSeparator), cleanTmp) {
			return "", fmt.Errorf("layer %s resolves outside tmp dir", l.Digest)
		}
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("no blob on disk for layer %s", l.Digest)
}

// firstFileIsArchive reports whether the first regular file in dir looks like
// an archive this package can extract. It is a routing hint only — what gets
// extracted comes from the manifest.
func firstFileIsArchive(dir string) bool {
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
		_, kind := sniffCompression(bufio.NewReader(f))
		f.Close()
		return kind != compressionUnknown
	}
	return false
}

func dirExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ExtractTar extracts a .tar.gz file to destDir, bounded by lim. A zero-value
// lim resolves to the defaults. Exported for testing.
func ExtractTar(tarPath, destDir string, lim Limits) error {
	_, err := extractTar(tarPath, destDir, lim.withDefaults())
	return err
}

// extractTar is ExtractTar plus the number of bytes written, so a caller
// extracting several layers can share one budget between them. lim must
// already be resolved by withDefaults.
func extractTar(tarPath, destDir string, lim Limits) (int64, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return 0, fmt.Errorf("open tar: %w", err)
	}
	defer f.Close()

	stream, closeStream, err := tarStream(f)
	if err != nil {
		return 0, err
	}
	defer closeStream()

	tr := tar.NewReader(stream)
	cleanDest := filepath.Clean(destDir) + string(os.PathSeparator)

	var entries int
	var total int64

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return total, fmt.Errorf("read tar entry: %w", err)
		}

		entries++
		if entries > lim.MaxEntries {
			return total, fmt.Errorf("archive has more than %d entries (--max-entries)", lim.MaxEntries)
		}

		target := filepath.Join(destDir, filepath.Clean(hdr.Name))
		// skip the root directory entry (e.g. "./") — it resolves to destDir itself
		if target == filepath.Clean(destDir) {
			continue
		}
		if !strings.HasPrefix(target, cleanDest) {
			return total, fmt.Errorf("illegal path in tar: %s", hdr.Name)
		}

		// Perm() keeps only the 0777 bits, dropping setuid/setgid/sticky:
		// an archive must not be able to plant a privileged binary.
		mode := hdr.FileInfo().Mode().Perm()

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, mode); err != nil {
				return total, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return total, err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return total, err
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
				return total, copyErr
			}
			switch {
			case n > lim.MaxFileBytes:
				return total, fmt.Errorf("file %s exceeds the %d byte limit for a single file (--max-file-bytes)",
					hdr.Name, lim.MaxFileBytes)
			case total+n > lim.MaxTotalBytes:
				return total, fmt.Errorf("archive expands past the %d byte total limit (--max-total-bytes)",
					lim.MaxTotalBytes)
			}
			total += n
		}
	}
	return total, nil
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

// CopyFiles copies all regular files from srcDir to destDir, bounded by lim
// the same way extraction is: the copy path handles blobs that are not
// archives, but "not an archive" is not a reason to write unbounded content.
// A zero-value lim resolves to the defaults. Exported for testing.
func CopyFiles(srcDir, destDir string, lim Limits) error {
	lim = lim.withDefaults()

	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("create dest dir: %w", err)
	}
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("read src dir: %w", err)
	}

	var copied int
	var total int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		copied++
		if copied > lim.MaxEntries {
			return fmt.Errorf("more than %d files to copy (--max-entries)", lim.MaxEntries)
		}

		src, err := os.Open(filepath.Join(srcDir, entry.Name()))
		if err != nil {
			return err
		}
		dstPath := filepath.Join(destDir, entry.Name())
		dst, err := os.Create(dstPath)
		if err != nil {
			src.Close()
			return err
		}

		// Same shape as extraction: read one byte past whichever limit binds
		// first, so an over-large file is caught without being written whole.
		allowed := lim.MaxFileBytes
		if remaining := lim.MaxTotalBytes - total; remaining < allowed {
			allowed = remaining
		}
		n, copyErr := io.Copy(dst, io.LimitReader(src, allowed+1))
		src.Close()
		if copyErr == nil {
			copyErr = dst.Sync()
		}
		if closeErr := dst.Close(); copyErr == nil {
			copyErr = closeErr
		}
		if copyErr != nil {
			return copyErr
		}
		switch {
		case n > lim.MaxFileBytes:
			return fmt.Errorf("file %s exceeds the %d byte limit for a single file (--max-file-bytes)",
				entry.Name(), lim.MaxFileBytes)
		case total+n > lim.MaxTotalBytes:
			return fmt.Errorf("copied files expand past the %d byte total limit (--max-total-bytes)",
				lim.MaxTotalBytes)
		}
		total += n

		log.Printf("copied %s", entry.Name())
	}
	return nil
}

// Compression handling.
//
// The compression is detected from the bytes rather than from the layer's
// media type: a mislabelled layer still extracts, and the error for something
// genuinely unsupported names what was actually found. OCI allows +gzip,
// +zstd and uncompressed tar layers.
type compressionKind int

const (
	compressionUnknown compressionKind = iota
	compressionGzip
	compressionZstd
	compressionNone // uncompressed tar
)

func (c compressionKind) String() string {
	switch c {
	case compressionGzip:
		return "gzip"
	case compressionZstd:
		return "zstd"
	case compressionNone:
		return "uncompressed tar"
	default:
		return "unrecognised"
	}
}

var (
	magicGzip = []byte{0x1f, 0x8b}
	magicZstd = []byte{0x28, 0xb5, 0x2f, 0xfd}
)

// sniffCompression peeks at br without consuming it.
func sniffCompression(br *bufio.Reader) ([]byte, compressionKind) {
	// 262 covers the ustar magic at offset 257 in a tar header
	head, _ := br.Peek(262)
	switch {
	case bytes.HasPrefix(head, magicGzip):
		return head, compressionGzip
	case bytes.HasPrefix(head, magicZstd):
		return head, compressionZstd
	case len(head) >= 262 && bytes.HasPrefix(head[257:262], []byte("ustar")):
		return head, compressionNone
	default:
		return head, compressionUnknown
	}
}

// tarStream returns the uncompressed tar stream from r, plus a function to
// release whatever decompressor it set up.
func tarStream(r io.Reader) (io.Reader, func(), error) {
	br := bufio.NewReader(r)
	head, kind := sniffCompression(br)

	switch kind {
	case compressionGzip:
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, nil, fmt.Errorf("open gzip: %w", err)
		}
		return gz, func() { gz.Close() }, nil
	case compressionZstd:
		zr, err := zstd.NewReader(br)
		if err != nil {
			return nil, nil, fmt.Errorf("open zstd: %w", err)
		}
		return zr, zr.Close, nil
	case compressionNone:
		return br, func() {}, nil
	default:
		n := min(len(head), 4)
		return nil, nil, fmt.Errorf("unsupported layer compression: first bytes %x are not gzip, zstd or an uncompressed tar", head[:n])
	}
}
