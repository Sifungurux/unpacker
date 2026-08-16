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
		return orasDigest, nil
	}

	log.Printf("oras failed (%v) — falling back to go-containerregistry", orasErr)
	if err := os.RemoveAll(tmpDir); err != nil {
		return "", fmt.Errorf("clean partial oras output: %w", err)
	}
	craneDigest, craneErr := pullWithCrane(ctx, cfg)
	switch {
	case craneErr == nil:
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
		registry := strings.SplitN(cfg.Image, "/", 2)[0]
		repo.Client = &orasauth.Client{
			Client: retry.DefaultClient,
			Cache:  orasauth.DefaultCache,
			Credential: orasauth.StaticCredential(registry, orasauth.Credential{
				Username: cfg.Creds.Username,
				Password: cfg.Creds.Password,
			}),
		}
	}
	return repo, nil
}

// fetchBlobToFile writes the blob described by desc to destPath, hashing it in
// flight and refusing to leave a file behind unless it matches desc.
func fetchBlobToFile(ctx context.Context, fetcher content.Fetcher, desc ocispec.Descriptor, destPath string) error {
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

// pullWithOras fetches the manifest and each layer blob directly from the registry.
// It does not set PlainHTTP or TLS InsecureSkipVerify — plain-HTTP and
// self-signed-cert registries will intentionally fail here and be handled by
// the crane fallback, which has full insecure transport support when cfg.Insecure is true.
func pullWithOras(ctx context.Context, cfg *Config, tmpDir string) (string, error) {
	repo, err := newOrasRepository(cfg)
	if err != nil {
		return "", err
	}

	ref := "latest"
	if idx := strings.LastIndex(cfg.Image, ":"); idx > strings.LastIndex(cfg.Image, "/") {
		ref = cfg.Image[idx+1:]
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

	// Docker manifests must go through crane (writes OCI layout for umoci).
	// Return an error here so Pull() falls back to pullWithCrane.
	if strings.HasPrefix(desc.MediaType, "application/vnd.docker.") {
		return "", fmt.Errorf("docker manifest type %q: %w", desc.MediaType, errUseCraneFallback)
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

		if err := fetchBlobToFile(ctx, repo, layerDesc, filepath.Join(tmpDir, filename)); err != nil {
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
			// go-containerregistry reads DOCKER_CONFIG pointing to the dir containing config.json
			prev := os.Getenv("DOCKER_CONFIG")
			if err := os.Setenv("DOCKER_CONFIG", filepath.Dir(cfg.Creds.ConfigPath)); err != nil {
				return "", fmt.Errorf("set DOCKER_CONFIG: %w", err)
			}
			defer os.Setenv("DOCKER_CONFIG", prev) //nolint:errcheck // restore for test safety
			opts = append(opts, crane.WithAuthFromKeychain(authn.DefaultKeychain))
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
