# Unpacker Go Rewrite — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rewrite the Python artifact-unpack tool in Go, replacing skopeo with go-containerregistry, adding an `--insecure` flag, and keeping umoci as an external binary dependency.

**Architecture:** Cobra CLI in `cmd/unpacker/main.go` wires together three internal packages: `auth` (credential resolution), `pull` (oras-go with go-containerregistry fallback), and `unpack` (tar extraction, umoci exec, file copy). The `--insecure` flag threads through to all TLS-sensitive operations.

**Tech Stack:** Go 1.22, `github.com/spf13/cobra`, `oras.land/oras-go/v2`, `github.com/google/go-containerregistry`, `umoci` (external binary)

---

## File Map

| File | Purpose |
|---|---|
| `go.mod` | Module definition and dependencies |
| `cmd/unpacker/main.go` | Cobra CLI: flags, validation, wires auth → pull → unpack |
| `internal/unpacker/auth.go` | `Credentials` struct + `Resolve()` — credential resolution |
| `internal/unpacker/auth_test.go` | Tests for credential resolution |
| `internal/unpacker/pull.go` | `Pull()` — oras Stage 1 + crane Stage 2 fallback, writes manifest.json |
| `internal/unpacker/pull_test.go` | Tests using go-containerregistry test registry |
| `internal/unpacker/unpack.go` | `Unpack()` — reads manifest, dispatches to extractTar / runUmoci / copyFiles |
| `internal/unpacker/unpack_test.go` | Tests for tar extraction and file copy using temp dirs |
| `Dockerfile` | Multi-stage build: Go builder + Alpine runtime with umoci |

---

## Task 1: Project Scaffold

**Files:**
- Create: `go.mod`
- Create: `cmd/unpacker/main.go`
- Create: `internal/unpacker/auth.go`
- Create: `internal/unpacker/pull.go`
- Create: `internal/unpacker/unpack.go`

- [ ] **Step 1: Initialise the Go module**

```bash
cd ~/Development/unpacker
go mod init github.com/energinet/unpacker
```

Expected output: `go: creating new go.mod: module github.com/energinet/unpacker`

- [ ] **Step 2: Create directory structure**

```bash
mkdir -p cmd/unpacker internal/unpacker
```

- [ ] **Step 3: Create stub `internal/unpacker/auth.go`**

```go
package unpacker
```

- [ ] **Step 4: Create stub `internal/unpacker/pull.go`**

```go
package unpacker
```

- [ ] **Step 5: Create stub `internal/unpacker/unpack.go`**

```go
package unpacker
```

- [ ] **Step 6: Create stub `cmd/unpacker/main.go`**

```go
package main

func main() {}
```

- [ ] **Step 7: Verify the module builds**

```bash
go build ./...
```

Expected: no output, exit 0.

- [ ] **Step 8: Add dependencies**

```bash
go get github.com/spf13/cobra@latest
go get oras.land/oras-go/v2@latest
go get github.com/google/go-containerregistry@latest
```

- [ ] **Step 9: Commit**

```bash
git add go.mod go.sum cmd/ internal/
git commit -m "feat: scaffold Go module and directory structure"
```

---

## Task 2: Auth Module

**Files:**
- Modify: `internal/unpacker/auth.go`
- Create: `internal/unpacker/auth_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/unpacker/auth_test.go`:

```go
package unpacker_test

import (
	"os"
	"testing"

	"github.com/energinet/unpacker/internal/unpacker"
)

func TestResolve_Public(t *testing.T) {
	creds, err := unpacker.Resolve("", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !creds.Public {
		t.Error("expected Public=true")
	}
}

func TestResolve_ConfigPath(t *testing.T) {
	creds, err := unpacker.Resolve("/path/to/config.json", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds.ConfigPath != "/path/to/config.json" {
		t.Errorf("expected ConfigPath=/path/to/config.json, got %s", creds.ConfigPath)
	}
}

func TestResolve_EnvVars(t *testing.T) {
	t.Setenv("USERNAME", "user")
	t.Setenv("PASSWORD", "pass")

	creds, err := unpacker.Resolve("", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds.Username != "user" || creds.Password != "pass" {
		t.Errorf("expected user/pass, got %s/%s", creds.Username, creds.Password)
	}
}

func TestResolve_NoCreds_Error(t *testing.T) {
	os.Unsetenv("USERNAME")
	os.Unsetenv("PASSWORD")

	_, err := unpacker.Resolve("", false)
	if err == nil {
		t.Error("expected error for private registry without credentials")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./internal/unpacker/... -run TestResolve -v
```

Expected: FAIL — `unpacker.Resolve` and `unpacker.Credentials` undefined.

- [ ] **Step 3: Implement `internal/unpacker/auth.go`**

```go
package unpacker

import (
	"fmt"
	"os"
)

// Credentials holds authentication information for a registry.
type Credentials struct {
	Username   string
	Password   string
	ConfigPath string
	Public     bool
}

// Resolve determines registry credentials from flags and environment variables.
// Resolution order:
//  1. public=true → no credentials
//  2. configPath set → use docker config file
//  3. USERNAME + PASSWORD env vars → basic auth
//  4. none → error
func Resolve(configPath string, public bool) (*Credentials, error) {
	if public {
		return &Credentials{Public: true}, nil
	}
	if configPath != "" {
		return &Credentials{ConfigPath: configPath}, nil
	}
	username := os.Getenv("USERNAME")
	password := os.Getenv("PASSWORD")
	if username != "" && password != "" {
		return &Credentials{Username: username, Password: password}, nil
	}
	return nil, fmt.Errorf("private registry requires --config or USERNAME/PASSWORD environment variables")
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./internal/unpacker/... -run TestResolve -v
```

Expected: all four tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/unpacker/auth.go internal/unpacker/auth_test.go
git commit -m "feat: add auth credential resolution"
```

---

## Task 3: Unpack Module

**Files:**
- Modify: `internal/unpacker/unpack.go`
- Create: `internal/unpacker/unpack_test.go`

- [ ] **Step 1: Write failing tests**

Create `internal/unpacker/unpack_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./internal/unpacker/... -run "TestExtractTar|TestCopyFiles" -v
```

Expected: FAIL — `unpacker.ExtractTar` and `unpacker.CopyFiles` undefined.

- [ ] **Step 3: Implement `internal/unpacker/unpack.go`**

```go
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
	}

	blobsDir := filepath.Join(tmpDir, "blobs", "sha256")
	hasTar := firstFileIsTar(tmpDir)
	hasBlobs := dirExists(blobsDir)

	if hasTar || hasBlobs {
		if useAllowedType && mediaType != "image" {
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
		defer f.Close()
		buf := make([]byte, 2)
		n, _ := f.Read(buf)
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
	log.Printf("umoci: %s", out)
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
		src.Close()
		dst.Close()
		log.Printf("copied %s", entry.Name())
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./internal/unpacker/... -run "TestExtractTar|TestCopyFiles" -v
```

Expected: all three tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/unpacker/unpack.go internal/unpacker/unpack_test.go
git commit -m "feat: add unpack logic — tar extraction, umoci exec, file copy"
```

---

## Task 4: Pull Module

**Files:**
- Modify: `internal/unpacker/pull.go`
- Create: `internal/unpacker/pull_test.go`

- [ ] **Step 1: Write failing tests**

Create `internal/unpacker/pull_test.go`:

```go
package unpacker_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/types"

	"github.com/energinet/unpacker/internal/unpacker"
)

// startTestRegistry spins up an in-process OCI registry and returns its address.
func startTestRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// pushTestImage pushes a minimal image to the test registry and returns the full reference.
func pushTestImage(t *testing.T, registryAddr, repo string) string {
	t.Helper()
	ref := registryAddr + "/" + repo + ":latest"
	layer := static.NewLayer([]byte("fake content"), types.OCIContentDescriptor)
	img, err := mutate.AppendLayer(empty.Image, layer)
	if err != nil {
		t.Fatalf("build test image: %v", err)
	}
	if err := crane.Push(img, ref, crane.Insecure); err != nil {
		t.Fatalf("push test image: %v", err)
	}
	return ref
}

func TestPull_CraneFallback_WritesOCILayout(t *testing.T) {
	addr := startTestRegistry(t)
	image := pushTestImage(t, addr, "test/myimage")

	outputDir := t.TempDir()
	cfg := &unpacker.Config{
		Image:     image,
		OutputDir: outputDir,
		Insecure:  true,
		Creds:     &unpacker.Credentials{Public: true},
	}

	if err := unpacker.Pull(context.Background(), cfg); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	// crane fallback writes OCI layout to tmp/
	if _, err := os.Stat(filepath.Join(outputDir, "tmp", "index.json")); err != nil {
		t.Errorf("expected OCI layout index.json in tmp/: %v", err)
	}
}

func TestPull_ManifestWritten(t *testing.T) {
	addr := startTestRegistry(t)
	image := pushTestImage(t, addr, "test/artifact")

	outputDir := t.TempDir()
	cfg := &unpacker.Config{
		Image:     image,
		OutputDir: outputDir,
		Insecure:  true,
		Creds:     &unpacker.Credentials{Public: true},
	}

	if err := unpacker.Pull(context.Background(), cfg); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	// either oras or crane fallback — manifest.json must exist
	manifestPath := filepath.Join(outputDir, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("manifest.json not written: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Errorf("manifest.json is not valid JSON: %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./internal/unpacker/... -run "TestPull" -v
```

Expected: FAIL — `unpacker.Pull` undefined.

- [ ] **Step 3: Implement `internal/unpacker/pull.go`**

```go
package unpacker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"crypto/tls"
	"net/http"

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
	repo.PlainHTTP = cfg.Insecure

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
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./internal/unpacker/... -run "TestPull" -v
```

Expected: both tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/unpacker/pull.go internal/unpacker/pull_test.go
git commit -m "feat: add pull logic — oras with go-containerregistry fallback"
```

---

## Task 5: CLI Wiring

**Files:**
- Modify: `cmd/unpacker/main.go`

- [ ] **Step 1: Implement `cmd/unpacker/main.go`**

```go
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/energinet/unpacker/internal/unpacker"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	var outputDir string
	var mediatypes []string
	var configPath string
	var public bool
	var insecure bool

	cmd := &cobra.Command{
		Use:   "unpacker IMAGE",
		Short: "Pull and unpack OCI and Docker artifacts from a registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			image := args[0]

			creds, err := unpacker.Resolve(configPath, public)
			if err != nil {
				return fmt.Errorf("credentials: %w", err)
			}

			cfg := &unpacker.Config{
				Image:        image,
				OutputDir:    outputDir,
				AllowedTypes: mediatypes,
				Insecure:     insecure,
				Creds:        creds,
			}

			if err := unpacker.Pull(context.Background(), cfg); err != nil {
				return fmt.Errorf("pull: %w", err)
			}

			if err := unpacker.Unpack(cfg); err != nil {
				return fmt.Errorf("unpack: %w", err)
			}

			return nil
		},
	}

	cmd.Flags().StringVarP(&outputDir, "output-dir", "o", ".", "Output directory")
	cmd.Flags().StringArrayVarP(&mediatypes, "mediatype", "m", []string{"flux", "helm"}, "Allowed mediatype (repeatable)")
	cmd.Flags().StringVarP(&configPath, "config", "c", "", "Path to dockerconfig.json for auth")
	cmd.Flags().BoolVarP(&public, "public", "p", false, "Pull from a public registry (no auth required)")
	cmd.Flags().BoolVarP(&insecure, "insecure", "k", false, "Skip TLS verification (self-signed certs)")

	return cmd
}
```

- [ ] **Step 2: Build and verify help output**

```bash
go build -o unpacker ./cmd/unpacker
./unpacker --help
```

Expected output:
```
Pull and unpack OCI and Docker artifacts from a registry

Usage:
  unpacker IMAGE [flags]

Flags:
  -c, --config string         Path to dockerconfig.json for auth
  -h, --help                  help for unpacker
  -k, --insecure              Skip TLS verification (self-signed certs)
  -m, --mediatype stringArray Allowed mediatype (repeatable) (default [flux,helm])
  -o, --output-dir string     Output directory (default ".")
  -p, --public                Pull from a public registry (no auth required)
```

- [ ] **Step 3: Verify error on missing credentials**

```bash
./unpacker myregistry.example.com/myimage:latest
```

Expected: exits non-zero with message containing `USERNAME/PASSWORD environment variables`

- [ ] **Step 4: Commit**

```bash
git add cmd/unpacker/main.go
git commit -m "feat: wire Cobra CLI — flags, auth, pull, unpack"
```

---

## Task 6: Dockerfile

**Files:**
- Create: `Dockerfile`

- [ ] **Step 1: Write the Dockerfile**

```dockerfile
FROM golang:1.22-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o unpacker ./cmd/unpacker

FROM alpine:3.19
ARG UMOCI_VERSION=0.4.7
RUN apk add --no-cache ca-certificates && \
    wget -O /usr/local/bin/umoci \
      https://github.com/opencontainers/umoci/releases/download/v${UMOCI_VERSION}/umoci.amd64 && \
    chmod +x /usr/local/bin/umoci
COPY --from=builder /build/unpacker /usr/local/bin/unpacker
ENTRYPOINT ["unpacker"]
```

- [ ] **Step 2: Build the image**

```bash
docker build -t unpacker:dev .
```

Expected: image builds successfully.

- [ ] **Step 3: Verify the binary runs inside the container**

```bash
docker run --rm unpacker:dev --help
```

Expected: same help output as Step 2 of Task 5.

- [ ] **Step 4: Commit**

```bash
git add Dockerfile
git commit -m "feat: add multi-stage Dockerfile with umoci"
```
