package unpacker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
)

// Result is written to <output-dir>/result.json when referrers are requested.
type Result struct {
	Image     string     `json:"image"`
	Digest    string     `json:"digest"`
	Referrers []Referrer `json:"referrers"`
}

// Referrer is one artifact attached to the pulled image — an SBOM, an in-toto
// attestation, a cosign signature — as downloaded to disk.
type Referrer struct {
	ArtifactType string            `json:"artifactType"`
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	Path         string            `json:"path"`  // relative to the output dir
	Files        []string          `json:"files"` // written inside Path
	Annotations  map[string]string `json:"annotations,omitempty"`
}

// FetchReferrers queries the OCI 1.1 referrers API for subjectDigest and
// downloads every attached artifact to <output-dir>/referrers/<type>/<digest>/,
// then writes <output-dir>/result.json.
//
// Registries that predate OCI 1.1 are a no-op, not a failure: oras falls back
// to the referrers tag schema, and when that is absent too the answer is simply
// an empty list. Only a transport or verification failure is an error.
func FetchReferrers(ctx context.Context, cfg *Config, subjectDigest string) (*Result, error) {
	result := &Result{Image: cfg.Image, Digest: subjectDigest, Referrers: []Referrer{}}

	subject, err := digest.Parse(subjectDigest)
	if err != nil {
		return nil, fmt.Errorf("parse subject digest %q: %w", subjectDigest, err)
	}

	repo, err := newOrasRepository(cfg)
	if err != nil {
		return nil, err
	}

	var descs []ocispec.Descriptor
	err = repo.Referrers(ctx, ocispec.Descriptor{Digest: subject}, "", func(refs []ocispec.Descriptor) error {
		descs = append(descs, refs...)
		return nil
	})
	switch {
	case errors.Is(err, errdef.ErrUnsupported):
		log.Printf("registry does not support the referrers API — no referrers fetched for %s", subjectDigest)
		return result, writeResult(cfg, result)
	case err != nil:
		return nil, fmt.Errorf("query referrers for %s: %w", subjectDigest, err)
	case len(descs) == 0:
		log.Printf("no referrers found for %s (the registry may not implement the referrers API)", subjectDigest)
		return result, writeResult(cfg, result)
	}

	baseDir := filepath.Join(cfg.OutputDir, "referrers")
	for _, desc := range descs {
		ref, err := downloadReferrer(ctx, repo, desc, baseDir)
		if err != nil {
			return nil, err
		}
		log.Printf("referrer %s (%s) -> %s", desc.Digest, ref.ArtifactType, ref.Path)
		result.Referrers = append(result.Referrers, ref)
	}

	return result, writeResult(cfg, result)
}

// downloadReferrer writes one referrer's manifest and its layer blobs under
// baseDir, returning what was written.
func downloadReferrer(ctx context.Context, repo content.Fetcher, desc ocispec.Descriptor, baseDir string) (Referrer, error) {
	// FetchAll verifies the bytes against desc before returning them
	manifestBytes, err := content.FetchAll(ctx, repo, desc)
	if err != nil {
		return Referrer{}, fmt.Errorf("fetch referrer manifest %s: %w", desc.Digest, err)
	}

	var m struct {
		ArtifactType string               `json:"artifactType"`
		Config       ocispec.Descriptor   `json:"config"`
		Layers       []ocispec.Descriptor `json:"layers"`
	}
	parseErr := json.Unmarshal(manifestBytes, &m)

	// The manifest is the authoritative source for the artifact type. Some
	// registries fill the referrers index entry from config.mediaType instead
	// (the pre-artifactType convention), which would file an SBOM under the
	// empty-config type and vary the layout by registry.
	artifactType := firstNonEmpty(m.ArtifactType, desc.ArtifactType, m.Config.MediaType, desc.MediaType)

	relPath := filepath.Join("referrers", typeDirName(artifactType), desc.Digest.Encoded())
	dir := filepath.Join(baseDir, typeDirName(artifactType), desc.Digest.Encoded())
	if err := os.MkdirAll(dir, 0755); err != nil {
		return Referrer{}, fmt.Errorf("create referrer dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifestBytes, 0644); err != nil {
		return Referrer{}, fmt.Errorf("write referrer manifest: %w", err)
	}

	ref := Referrer{
		ArtifactType: artifactType,
		MediaType:    desc.MediaType,
		Digest:       desc.Digest.String(),
		Size:         desc.Size,
		Path:         relPath,
		Files:        []string{"manifest.json"},
		Annotations:  desc.Annotations,
	}

	if parseErr != nil {
		// An index or an unparseable body still has its manifest on disk; the
		// payload just isn't a layer list we can walk.
		log.Printf("referrer %s: manifest is not a layer list (%v) — kept manifest.json only", desc.Digest, parseErr)
		return ref, nil
	}

	for i, layer := range m.Layers {
		name := safeFileName(layer.Annotations[ocispec.AnnotationTitle], layer.Digest.Encoded())
		if name == "" {
			name = fmt.Sprintf("layer-%d", i)
		}
		if err := fetchBlobToFile(ctx, repo, layer, filepath.Join(dir, name)); err != nil {
			return Referrer{}, fmt.Errorf("referrer %s: %w", desc.Digest, err)
		}
		ref.Files = append(ref.Files, name)
	}
	return ref, nil
}

func writeResult(cfg *Config, result *Result) error {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("encode result.json: %w", err)
	}
	return os.WriteFile(filepath.Join(cfg.OutputDir, "result.json"), append(data, '\n'), 0644)
}

// typeDirName turns an artifact type into one directory name. The value comes
// from the registry, so it is not allowed to contribute a path separator, a
// parent reference, or an unbounded name.
func typeDirName(artifactType string) string {
	var b strings.Builder
	for _, r := range artifactType {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), "-.")
	if name == "" {
		return "unknown"
	}
	if len(name) > 100 {
		name = name[:100]
	}
	return name
}

// safeFileName reduces a registry-supplied title annotation to a bare file
// name, falling back when it does not survive that.
func safeFileName(name, fallback string) string {
	base := filepath.Base(filepath.Clean("/" + name))
	if base == "." || base == ".." || base == string(filepath.Separator) || base == "" {
		return fallback
	}
	return base
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
