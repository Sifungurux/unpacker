package unpacker

import (
	"context"
	"crypto/tls"
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
	oras "oras.land/oras-go/v2"
	orasfile "oras.land/oras-go/v2/content/file"
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
		return pullWithCrane(ctx, cfg, tmpDir)
	}
	return nil
}

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

	// parse tag from image reference
	ref := "latest"
	if idx := strings.LastIndex(cfg.Image, ":"); idx > strings.LastIndex(cfg.Image, "/") {
		ref = cfg.Image[idx+1:]
	}

	store, err := orasfile.New(tmpDir)
	if err != nil {
		return fmt.Errorf("create file store: %w", err)
	}
	defer store.Close()

	desc, err := oras.Copy(ctx, repo, ref, store, ref, oras.DefaultCopyOptions)
	if err != nil {
		return fmt.Errorf("oras copy: %w", err)
	}

	// fetch manifest and write manifest.json
	manifestReader, err := repo.Fetch(ctx, desc)
	if err != nil {
		return fmt.Errorf("fetch manifest: %w", err)
	}
	defer manifestReader.Close()

	manifestBytes, err := io.ReadAll(manifestReader)
	if err != nil {
		return fmt.Errorf("read manifest bytes: %w", err)
	}

	return os.WriteFile(filepath.Join(cfg.OutputDir, "manifest.json"), manifestBytes, 0644)
}

func pullWithCrane(ctx context.Context, cfg *Config, tmpDir string) error {
	// clean up any partial oras output
	if err := os.RemoveAll(tmpDir); err != nil {
		return fmt.Errorf("clean tmp dir: %w", err)
	}

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
			if err := os.Setenv("DOCKER_CONFIG", filepath.Dir(cfg.Creds.ConfigPath)); err != nil {
				return fmt.Errorf("set DOCKER_CONFIG: %w", err)
			}
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
