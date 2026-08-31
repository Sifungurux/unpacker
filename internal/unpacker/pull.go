package unpacker

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/types"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	orasremote "oras.land/oras-go/v2/registry/remote"
	orasauth "oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

// Pull pulls the image to <outputDir>/tmp/ and writes <outputDir>/manifest.json.
// It tries oras-go first (OCI artifacts), then falls back to go-containerregistry (Docker images).
//
// It returns the digest the registry resolved the reference to. That is the
// digest referrers are attached to, and for the crane path it is deliberately
// the digest as served — not the digest of the media-type-normalised manifest
// written to the OCI layout, which no registry knows about.
func Pull(ctx context.Context, cfg *Config) (string, error) {
	tmpDir := filepath.Join(cfg.OutputDir, "tmp")
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return "", fmt.Errorf("create tmp dir: %w", err)
	}

	log.Printf("pulling %s with oras", cfg.Image)
	orasDigest, orasErr := pullWithOras(ctx, cfg, tmpDir)
	if orasErr == nil {
		log.Printf("resolved %s to %s", cfg.Image, orasDigest)
		return orasDigest, nil
	}

	log.Printf("oras failed (%v) — falling back to go-containerregistry", orasErr)
	if err := os.RemoveAll(tmpDir); err != nil {
		return "", fmt.Errorf("clean partial oras output: %w", err)
	}
	craneDigest, craneErr := pullWithCrane(ctx, cfg)
	switch {
	case craneErr == nil:
		log.Printf("resolved %s to %s", cfg.Image, craneDigest)
		return craneDigest, nil
	case errors.Is(orasErr, errUseCraneFallback):
		// oras declined on purpose; it has nothing to say about the failure
		return "", craneErr
	default:
		// crane's error describes its own attempt, which can hide why the
		// first one failed — e.g. a digest mismatch reported as a pull error.
		return "", fmt.Errorf("oras: %w; crane fallback: %w", orasErr, craneErr)
	}
}

// newOrasRepository builds a repository client for cfg, shared by the pull and
// referrers paths so both authenticate and speak the same transport.
func newOrasRepository(cfg *Config) (*orasremote.Repository, error) {
	repo, err := orasremote.NewRepository(cfg.Image)
	if err != nil {
		return nil, fmt.Errorf("parse image reference: %w", err)
	}

	if cfg.Insecure {
		repo.PlainHTTP = true
	}

	if cfg.Creds != nil && cfg.Creds.Username != "" {
		// Plain HTTP puts basic auth on the wire in the clear. Refusing by
		// default means a typo'd --insecure against a real registry cannot
		// leak a password; --insecure-allow-credentials is the way to say
		// "yes, this really is my own test registry".
		if repo.PlainHTTP && !cfg.AllowInsecureCredentials {
			return nil, fmt.Errorf("refusing to send credentials over plain HTTP to %s: "+
				"re-run with --insecure-allow-credentials if that is intended", repo.Reference.Registry)
		}
		// The credential is scoped to the registry oras parsed out of the
		// reference, not to a hand-split prefix of the image string: a bare
		// reference has no host in it at all, and docker.io resolves to a
		// different auth host than it is written as.
		repo.Client = &orasauth.Client{
			Client: retry.DefaultClient,
			Cache:  orasauth.DefaultCache,
			Credential: orasauth.StaticCredential(repo.Reference.Registry, orasauth.Credential{
				Username: cfg.Creds.Username,
				Password: cfg.Creds.Password,
			}),
		}
	}
	return repo, nil
}

// downloadBudget bounds the download phase the way Limits bounds extraction.
// Without it --max-total-bytes only applies once the bytes are already in
// tmp/, so a registry declaring a 500 GB layer fills the disk before any limit
// is consulted — on a shared CI runner that is a denial of service against
// everything else on the box.
//
// This trusts desc.Size where extraction deliberately does not trust hdr.Size.
// The difference is which way the lie has to go: a registry that over-declares
// is rejected here, and one that under-declares still fails VerifyReader's
// size check while the bytes are in flight, which is where they actually stop.
type downloadBudget struct {
	maxFile   int64
	remaining int64
}

func newDownloadBudget(lim Limits) *downloadBudget {
	lim = lim.withDefaults()
	return &downloadBudget{maxFile: lim.MaxFileBytes, remaining: lim.MaxTotalBytes}
}

// charge reserves desc.Size against the budget before a byte is fetched.
func (b *downloadBudget) charge(desc ocispec.Descriptor) error {
	if b == nil {
		return nil
	}
	if desc.Size > b.maxFile {
		return fmt.Errorf("blob %s declares %d bytes, over the %d byte limit for a single file (--max-file-bytes)",
			desc.Digest, desc.Size, b.maxFile)
	}
	if desc.Size > b.remaining {
		return fmt.Errorf("blob %s declares %d bytes, past the %d byte total limit for this pull (--max-total-bytes)",
			desc.Digest, desc.Size, b.remaining)
	}
	b.remaining -= desc.Size
	return nil
}

// fetchBlobToFile writes the blob described by desc to destPath, hashing it in
// flight and refusing to leave a file behind unless it matches desc. budget
// bounds what may be written before it is; a nil budget is unbounded.
func fetchBlobToFile(ctx context.Context, fetcher content.Fetcher, desc ocispec.Descriptor, destPath string, budget *downloadBudget) error {
	if err := budget.charge(desc); err != nil {
		return err
	}

	rc, err := fetcher.Fetch(ctx, desc)
	if err != nil {
		return fmt.Errorf("fetch blob %s: %w", desc.Digest, err)
	}

	f, err := os.Create(destPath)
	if err != nil {
		rc.Close()
		return fmt.Errorf("create blob file: %w", err)
	}

	// VerifyReader hashes the blob as io.Copy drains it; Verify() then
	// compares that hash and the byte count against the descriptor. A
	// mismatch means the file on disk is not the blob that was named, so it
	// must not survive.
	vr := content.NewVerifyReader(rc, desc)
	_, copyErr := io.Copy(f, vr)
	if copyErr == nil {
		// must run before rc.Close(): Verify() peeks the body for
		// trailing bytes beyond the descriptor's declared size.
		copyErr = vr.Verify()
	}
	rc.Close()
	if closeErr := f.Close(); copyErr == nil {
		// a failed flush leaves a short file that hashed fine in flight
		copyErr = closeErr
	}
	if copyErr != nil {
		os.Remove(destPath) //nolint:errcheck // already returning an error
		return fmt.Errorf("blob %s (%s) failed verification against its declared digest: %w",
			filepath.Base(destPath), desc.Digest, copyErr)
	}
	return nil
}

// errUseCraneFallback marks the expected "this isn't an OCI artifact" exit from
// pullWithOras, as opposed to a real failure worth reporting alongside crane's.
var errUseCraneFallback = errors.New("not an OCI artifact")

// craneFallbackReason reports why a manifest cannot be handled on the oras
// path, or "" when it can. The distinction is images versus artifacts, not
// OCI versus Docker -- getting that backwards is what caused both of the
// failures this function exists to prevent.
//
// It used to be a prefix test on "application/vnd.docker.", which let two
// shapes through that oras cannot actually handle:
//
//   - an OCI image INDEX. Not a docker media type, so it passed. The code
//     below then looked for a "layers" field, which an index does not have
//     (it has "manifests"), found none, downloaded no blobs and returned
//     SUCCESS. Every multi-platform image published as an OCI index --
//     measured at 47 of 95 references in one real fleet -- unpacked to an
//     empty directory while reporting that it had worked. Anything reading
//     that directory to decide whether an image is safe was reading
//     nothing.
//
//   - an OCI image MANIFEST. oras downloaded the layer blobs as bare files,
//     which is right for an artifact and wrong for an image: umoci needs an
//     oci-layout, and rejected the directory with "invalid image detected"
//     one step later.
//
// An index also needs crane for a second reason: choosing a platform. oras
// fetches the reference it was given, and for an index that is the list
// itself, not any image in it.
//
// A genuine OCI artifact -- a helm chart, a flux manifest -- keeps the oras
// path, which is the whole reason this tool uses oras at all. The test is
// the config media type, which is what the image-spec uses to say "this
// manifest describes a runnable image" as opposed to arbitrary content.
func craneFallbackReason(mediaType string, manifestBytes []byte) string {
	if strings.HasPrefix(mediaType, "application/vnd.docker.") {
		return fmt.Sprintf("docker manifest type %q", mediaType)
	}
	if mediaType == ocispec.MediaTypeImageIndex {
		return fmt.Sprintf("OCI image index %q needs a platform chosen", mediaType)
	}
	if mediaType == ocispec.MediaTypeImageManifest {
		var m struct {
			Config struct {
				MediaType string `json:"mediaType"`
			} `json:"config"`
		}
		// An unparseable manifest is not this function's error to report --
		// the caller parses it again and fails there with a better message.
		// Treating it as an artifact keeps that behaviour unchanged.
		if err := json.Unmarshal(manifestBytes, &m); err != nil {
			return ""
		}
		if m.Config.MediaType == ocispec.MediaTypeImageConfig {
			return fmt.Sprintf("OCI image manifest with config %q", m.Config.MediaType)
		}
	}
	return ""
}

// pullWithOras fetches the manifest and each layer blob directly from the registry.
//
// --insecure means two different things across the two paths, which is worth
// knowing before reading either: here it sets PlainHTTP (no TLS at all, via
// newOrasRepository), while pullWithCrane sets InsecureSkipVerify (TLS, but
// unverified). Both refuse to carry credentials without
// --insecure-allow-credentials.
func pullWithOras(ctx context.Context, cfg *Config, tmpDir string) (string, error) {
	repo, err := newOrasRepository(cfg)
	if err != nil {
		return "", err
	}

	// The reference was already parsed when the repository was built. Splitting
	// the string here instead used to turn repo@sha256:abc into the *tag* abc,
	// so digest-pinned pulls looked up a tag that does not exist. oras handles
	// repo:tag, repo@digest and repo:tag@digest (digest wins, per the spec).
	ref := repo.Reference.Reference
	if ref == "" {
		ref = "latest"
	}

	// fetch manifest
	desc, manifestReader, err := repo.FetchReference(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("fetch manifest: %w", err)
	}
	// content.ReadAll hashes the body as it reads and rejects it unless the
	// bytes match desc.Digest and desc.Size. Without this the registry could
	// serve any manifest it liked for the requested tag.
	manifestBytes, err := content.ReadAll(manifestReader, desc)
	manifestReader.Close()
	if err != nil {
		return "", fmt.Errorf("read manifest %s: %w", desc.Digest, err)
	}

	// Anything that is a container IMAGE rather than an OCI artifact has to
	// go through crane, which writes the OCI layout umoci needs. Return an
	// error here so Pull() falls back to pullWithCrane.
	if reason := craneFallbackReason(desc.MediaType, manifestBytes); reason != "" {
		return "", fmt.Errorf("%s: %w", reason, errUseCraneFallback)
	}

	if err := os.WriteFile(filepath.Join(cfg.OutputDir, "manifest.json"), manifestBytes, 0644); err != nil {
		return "", fmt.Errorf("write manifest.json: %w", err)
	}

	// parse layers from manifest
	var m struct {
		Layers []struct {
			MediaType   string            `json:"mediaType"`
			Digest      string            `json:"digest"`
			Size        int64             `json:"size"`
			Annotations map[string]string `json:"annotations"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return "", fmt.Errorf("parse manifest: %w", err)
	}

	// One budget for the whole pull: the total limit is shared across an
	// artifact's layers, the same way it is during extraction.
	budget := newDownloadBudget(cfg.Limits)

	// fetch each layer blob directly and write to tmpDir
	for _, layer := range m.Layers {
		filename := layer.Annotations["org.opencontainers.image.title"]
		if filename == "" {
			// fall back to hex digest as filename
			parts := strings.SplitN(layer.Digest, ":", 2)
			if len(parts) == 2 {
				filename = parts[1]
			} else {
				filename = layer.Digest
			}
		}

		layerDesc := ocispec.Descriptor{
			MediaType: layer.MediaType,
			Digest:    digest.Digest(layer.Digest),
			Size:      layer.Size,
		}

		if err := fetchBlobToFile(ctx, repo, layerDesc, filepath.Join(tmpDir, filename), budget); err != nil {
			return "", err
		}
	}

	// desc is the manifest descriptor: the digest referrers attach to
	return desc.Digest.String(), nil
}

func pullWithCrane(ctx context.Context, cfg *Config) (string, error) {
	tmpDir := filepath.Join(cfg.OutputDir, "tmp")

	opts := []crane.Option{crane.WithContext(ctx)}

	if cfg.Insecure {
		// Same guard as newOrasRepository, for the same reason: an
		// unverified certificate is a certificate an interceptor can
		// present, so basic auth over it is basic auth handed away. This
		// path carries more traffic than the oras one — every image index
		// and every Docker manifest routes here.
		if cfg.Creds != nil && cfg.Creds.Username != "" && !cfg.AllowInsecureCredentials {
			return "", fmt.Errorf("refusing to send credentials over unverified TLS to %s: "+
				"re-run with --insecure-allow-credentials if that is intended", cfg.Image)
		}
		transport := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		}
		opts = append(opts, crane.WithTransport(transport))
	}

	if cfg.Creds != nil {
		switch {
		case cfg.Creds.Username != "":
			opts = append(opts, crane.WithAuth(&authn.Basic{
				Username: cfg.Creds.Username,
				Password: cfg.Creds.Password,
			}))
		case cfg.Creds.ConfigPath != "":
			// Reads the file directly rather than pointing DOCKER_CONFIG at it:
			// that was process-wide state in a library, unsafe under concurrent
			// use and visible to everything else sharing the process.
			opts = append(opts, crane.WithAuthFromKeychain(configFileKeychain{path: cfg.Creds.ConfigPath}))
		}
	}

	img, err := crane.Pull(cfg.Image, opts...)
	if err != nil {
		return "", fmt.Errorf("crane pull: %w", err)
	}
	// Taken before the OCI relabelling below, which changes the manifest bytes
	// and so its digest: referrers hang off the digest the registry serves.
	servedDigest, err := img.Digest()
	if err != nil {
		return "", fmt.Errorf("resolve image digest: %w", err)
	}
	img = ociImage{img}

	// crane.Pull is lazy — it has the manifest and none of the blobs, so the
	// declared sizes are known here and the bytes are not yet on disk.
	// AppendImage below is what writes them. This is the majority path (every
	// Docker manifest, every index, every image with an image config), so
	// leaving it unbounded would leave the limits covering the smaller half.
	manifest, err := img.Manifest()
	if err != nil {
		return "", fmt.Errorf("read manifest: %w", err)
	}
	budget := newDownloadBudget(cfg.Limits)
	for _, desc := range append([]v1.Descriptor{manifest.Config}, manifest.Layers...) {
		if err := budget.charge(ocispec.Descriptor{
			Digest: digest.Digest(desc.Digest.String()),
			Size:   desc.Size,
		}); err != nil {
			return "", err
		}
	}

	// write as OCI layout so umoci can unpack it.
	// tag annotation is required so umoci can resolve the image by name.
	p, err := layout.Write(tmpDir, empty.Index)
	if err != nil {
		return "", fmt.Errorf("create OCI layout: %w", err)
	}
	if err := p.AppendImage(img, layout.WithAnnotations(map[string]string{
		"org.opencontainers.image.ref.name": "latest",
	})); err != nil {
		return "", fmt.Errorf("append image to layout: %w", err)
	}

	// write manifest.json from the image manifest
	rawManifest, err := img.RawManifest()
	if err != nil {
		return "", fmt.Errorf("get manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(cfg.OutputDir, "manifest.json"), rawManifest, 0644); err != nil {
		return "", err
	}
	return servedDigest.String(), nil
}

// ociImage wraps a v1.Image so its manifest always declares OCI media types
// for itself, its config, and its layers — regardless of what the source
// registry served. umoci (invoked by runUmoci in unpack.go) rejects any
// manifest/config/layer descriptor whose mediaType isn't the OCI one, and
// registries serving old Docker schema2 manifests (application/vnd.docker.*)
// fail that check even though the underlying blob bytes are byte-identical
// to their OCI counterparts. Relabeling here is therefore sufficient — no
// blob content is rewritten.
type ociImage struct {
	v1.Image
}

func (o ociImage) MediaType() (types.MediaType, error) {
	return types.OCIManifestSchema1, nil
}

func (o ociImage) RawManifest() ([]byte, error) {
	b, err := o.Image.RawManifest()
	if err != nil {
		return nil, err
	}
	var m v1.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	m.MediaType = types.OCIManifestSchema1
	m.Config.MediaType = types.OCIConfigJSON
	for i := range m.Layers {
		m.Layers[i].MediaType = ociLayerMediaType(m.Layers[i].MediaType)
	}
	return json.Marshal(&m)
}

func (o ociImage) Manifest() (*v1.Manifest, error) { return partial.Manifest(o) }
func (o ociImage) Digest() (v1.Hash, error)        { return partial.Digest(o) }
func (o ociImage) Size() (int64, error)            { return partial.Size(o) }

// ociLayerMediaType maps a Docker layer media type to its OCI equivalent.
// Anything else (already OCI, or a type we don't recognize) passes through
// unchanged.
func ociLayerMediaType(mt types.MediaType) types.MediaType {
	switch mt {
	case types.DockerLayer:
		return types.OCILayer
	case types.DockerUncompressedLayer:
		return types.OCIUncompressedLayer
	case types.DockerForeignLayer:
		return types.OCIRestrictedLayer
	default:
		return mt
	}
}
