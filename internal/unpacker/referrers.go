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
	// Verification is nil when no verify flag was passed. That has to stay
	// distinguishable from a failed verification: absent means nobody asked.
	Verification *Verification `json:"verification,omitempty"`
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
		return result, WriteResult(cfg, result)
	case err != nil:
		return nil, fmt.Errorf("query referrers for %s: %w", subjectDigest, err)
	case len(descs) == 0:
		log.Printf("no referrers found for %s (the registry may not implement the referrers API)", subjectDigest)
		return result, WriteResult(cfg, result)
	}

	if max := cfg.maxReferrers(); len(descs) > max {
		return nil, fmt.Errorf("registry listed %d referrers for %s, over the limit of %d (--max-referrers)",
			len(descs), subjectDigest, max)
	}

	// Shared with the pull phase in spirit but not in state: referrers are a
	// separate download, so they get their own budget rather than eating the
	// artifact's.
	budget := newDownloadBudget(cfg.Limits)

	baseDir := filepath.Join(cfg.OutputDir, "referrers")
	for _, desc := range descs {
		ref, err := downloadReferrer(ctx, repo, desc, baseDir, subject, budget)
		if err != nil {
			// Referrers are fetched after Unpack has already published
			// image/, so returning here without a result.json would leave a
			// complete-looking tree next to the *previous* run's result.
			// Record what did land, then fail.
			if writeErr := WriteResult(cfg, result); writeErr != nil {
				return nil, fmt.Errorf("%w (and writing result.json failed: %v)", err, writeErr)
			}
			return nil, err
		}
		if ref.Digest == "" {
			continue // skipped: see downloadReferrer
		}
		log.Printf("referrer %s (%s) -> %s", desc.Digest, ref.ArtifactType, ref.Path)
		result.Referrers = append(result.Referrers, ref)
	}

	return result, WriteResult(cfg, result)
}

// downloadReferrer writes one referrer's manifest and its layer blobs under
// baseDir, returning what was written.
// A zero Referrer with a nil error means the referrer was skipped, not that
// nothing went wrong.
func downloadReferrer(ctx context.Context, repo content.Fetcher, desc ocispec.Descriptor, baseDir string, subject digest.Digest, budget *downloadBudget) (Referrer, error) {
	if err := budget.charge(desc); err != nil {
		return Referrer{}, fmt.Errorf("referrer %s: %w", desc.Digest, err)
	}

	// FetchAll verifies the bytes against desc before returning them
	manifestBytes, err := content.FetchAll(ctx, repo, desc)
	if err != nil {
		return Referrer{}, fmt.Errorf("fetch referrer manifest %s: %w", desc.Digest, err)
	}

	var m struct {
		ArtifactType string               `json:"artifactType"`
		Config       ocispec.Descriptor   `json:"config"`
		Layers       []ocispec.Descriptor `json:"layers"`
		Subject      *ocispec.Descriptor  `json:"subject"`
	}
	parseErr := json.Unmarshal(manifestBytes, &m)

	// Digest verification proves the registry served the bytes it advertised.
	// It does not prove the artifact claims to be about *this* image. On the
	// referrers-tag-schema fallback the listing is an ordinary tag whose
	// contents the registry controls outright, so without this check a
	// compromised registry can file a genuine, correctly-signed SBOM
	// belonging to some other benign image under this digest.
	switch {
	case m.Subject != nil && m.Subject.Digest != subject:
		return Referrer{}, fmt.Errorf("referrer %s claims subject %s, not %s: refusing to file it under this image",
			desc.Digest, m.Subject.Digest, subject)
	case m.Subject == nil:
		// Per OCI 1.1 a referrer is defined by having a subject, so this
		// should not happen — but rejecting the run would break
		// --with-referrers against any registry whose fallback listing omits
		// it. Skipping keeps unverifiable content off disk without turning a
		// working pull into a failure.
		// ponytail: skip-and-warn for compatibility; make it a hard error
		// once no real registry is seen doing this.
		log.Printf("referrer %s declares no subject — skipped, as nothing ties it to %s", desc.Digest, subject)
		return Referrer{}, nil
	}

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
		if err := fetchBlobToFile(ctx, repo, layer, filepath.Join(dir, name), budget); err != nil {
			return Referrer{}, fmt.Errorf("referrer %s: %w", desc.Digest, err)
		}
		ref.Files = append(ref.Files, name)
	}
	return ref, nil
}

// WriteResult writes result.json to the output dir. It is written on every
// run, not only with --with-referrers, so the resolved digest is always
// recorded for whatever consumes the output.
func WriteResult(cfg *Config, result *Result) error {
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
