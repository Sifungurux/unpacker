package unpacker

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	orasremote "oras.land/oras-go/v2/registry/remote"
	orasauth "oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

// Pull pulls the image to <outputDir>/tmp/ and writes <outputDir>/manifest.json.
// It tries oras-go first (OCI artifacts), then falls back to go-containerregistry (Docker images).
func Pull(ctx context.Context, cfg *Config) error {
	tmpDir := filepath.Join(cfg.OutputDir, "tmp")
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return fmt.Errorf("create tmp dir: %w", err)
	}

	log.Printf("pulling %s with oras", cfg.Image)
	if err := pullWithOras(ctx, cfg, tmpDir); err != nil {
		log.Printf("oras failed (%v) — falling back to go-containerregistry", err)
		if err := os.RemoveAll(tmpDir); err != nil {
			return fmt.Errorf("clean partial oras output: %w", err)
		}
		return pullWithCrane(ctx, cfg)
	}
	return nil
}

// pullWithOras fetches the manifest and each layer blob directly from the registry.
// It does not set PlainHTTP or TLS InsecureSkipVerify — plain-HTTP and
// self-signed-cert registries will intentionally fail here and be handled by
// the crane fallback, which has full insecure transport support when cfg.Insecure is true.
func pullWithOras(ctx context.Context, cfg *Config, tmpDir string) error {
	repo, err := orasremote.NewRepository(cfg.Image)
	if err != nil {
		return fmt.Errorf("parse image reference: %w", err)
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

	ref := "latest"
	if idx := strings.LastIndex(cfg.Image, ":"); idx > strings.LastIndex(cfg.Image, "/") {
		ref = cfg.Image[idx+1:]
	}

	// fetch manifest
	_, manifestReader, err := repo.FetchReference(ctx, ref)
	if err != nil {
		return fmt.Errorf("fetch manifest: %w", err)
	}
	manifestBytes, err := io.ReadAll(manifestReader)
	manifestReader.Close()
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}

	if err := os.WriteFile(filepath.Join(cfg.OutputDir, "manifest.json"), manifestBytes, 0644); err != nil {
		return fmt.Errorf("write manifest.json: %w", err)
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
		return fmt.Errorf("parse manifest: %w", err)
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

		desc := ocispec.Descriptor{
			MediaType: layer.MediaType,
			Digest:    digest.Digest(layer.Digest),
			Size:      layer.Size,
		}

		rc, err := repo.Fetch(ctx, desc)
		if err != nil {
			return fmt.Errorf("fetch layer %s: %w", layer.Digest, err)
		}

		destPath := filepath.Join(tmpDir, filename)
		f, err := os.Create(destPath)
		if err != nil {
			rc.Close()
			return fmt.Errorf("create blob file: %w", err)
		}

		_, copyErr := io.Copy(f, rc)
		rc.Close()
		f.Close()
		if copyErr != nil {
			return fmt.Errorf("write blob %s: %w", layer.Digest, copyErr)
		}
	}

	return nil
}

func pullWithCrane(ctx context.Context, cfg *Config) error {
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
				return fmt.Errorf("set DOCKER_CONFIG: %w", err)
			}
			defer os.Setenv("DOCKER_CONFIG", prev) //nolint:errcheck // restore for test safety
			opts = append(opts, crane.WithAuthFromKeychain(authn.DefaultKeychain))
		}
	}

	img, err := crane.Pull(cfg.Image, opts...)
	if err != nil {
		return fmt.Errorf("crane pull: %w", err)
	}

	// write as OCI layout so umoci can unpack it
	p, err := layout.Write(tmpDir, empty.Index)
	if err != nil {
		return fmt.Errorf("create OCI layout: %w", err)
	}
	if err := p.AppendImage(img); err != nil {
		return fmt.Errorf("append image to layout: %w", err)
	}

	// write manifest.json from the image manifest
	rawManifest, err := img.RawManifest()
	if err != nil {
		return fmt.Errorf("get manifest: %w", err)
	}
	return os.WriteFile(filepath.Join(cfg.OutputDir, "manifest.json"), rawManifest, 0644)
}
