package unpacker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"

	orasauth "oras.land/oras-go/v2/registry/remote/auth"
)

func credsCfg(image string, insecure, allowInsecureCreds bool) *Config {
	return &Config{
		Image:                    image,
		Insecure:                 insecure,
		AllowInsecureCredentials: allowInsecureCreds,
		Creds:                    &Credentials{Username: "user", Password: "hunter2"},
	}
}

// Basic auth over plain HTTP puts the password on the wire in the clear, so
// --insecure alone must not be enough to send it.
func TestNewOrasRepository_RefusesCredentialsOverPlainHTTP(t *testing.T) {
	_, err := newOrasRepository(credsCfg("registry.example.com/app:v1", true, false))
	if err == nil {
		t.Fatal("expected credentials over plain HTTP to be refused")
	}
	if !strings.Contains(err.Error(), "--insecure-allow-credentials") {
		t.Errorf("error = %q, want it to name the opt-out flag", err)
	}
}

func TestNewOrasRepository_AllowsCredentialsOverPlainHTTPWhenAskedTo(t *testing.T) {
	repo, err := newOrasRepository(credsCfg("registry.example.com/app:v1", true, true))
	if err != nil {
		t.Fatalf("--insecure-allow-credentials should permit it: %v", err)
	}
	if repo.Client == nil {
		t.Fatal("expected an authenticated client")
	}
}

// TLS is the normal case: credentials attach without any opt-in, and are
// scoped to the registry parsed out of the reference rather than offered to
// every host the client happens to talk to.
func TestNewOrasRepository_ScopesCredentialsToTheParsedRegistry(t *testing.T) {
	repo, err := newOrasRepository(credsCfg("registry.example.com/app:v1", false, false))
	if err != nil {
		t.Fatalf("newOrasRepository: %v", err)
	}
	client, ok := repo.Client.(*orasauth.Client)
	if !ok {
		t.Fatalf("expected an oras auth client, got %T", repo.Client)
	}

	got, err := client.Credential(context.Background(), "registry.example.com")
	if err != nil {
		t.Fatalf("credential lookup: %v", err)
	}
	if got.Username != "user" || got.Password != "hunter2" {
		t.Errorf("credential for the target registry = %+v, want the configured one", got)
	}

	other, err := client.Credential(context.Background(), "evil.example.net")
	if err != nil {
		t.Fatalf("credential lookup: %v", err)
	}
	if other != orasauth.EmptyCredential {
		t.Errorf("credential offered to an unrelated host: %+v", other)
	}
}

// A reference with no registry is rejected before any credential is built, so
// there is no path where a password is sent to a guessed host.
func TestNewOrasRepository_RejectsReferenceWithoutRegistry(t *testing.T) {
	if _, err := newOrasRepository(credsCfg("alpine:3.21", false, false)); err == nil {
		t.Error("expected a bare reference to be rejected")
	}
}

// TestPullWithCrane_DoesNotTouchDockerConfigEnv: authenticating from a docker
// config file used to work by setting DOCKER_CONFIG process-wide and restoring
// it afterwards — global state in a library, unsafe under concurrent use.
func TestPullWithCrane_DoesNotTouchDockerConfigEnv(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", "/sentinel/value")

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"auths":{"registry.example.com":{"auth":"dXNlcjpwYXNz"}}}`), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		Image:     "registry.example.invalid/app:v1",
		OutputDir: t.TempDir(),
		Creds:     &Credentials{ConfigPath: configPath},
	}
	// The pull itself cannot succeed against a non-existent registry; what
	// matters is that the environment is untouched either way.
	_, _ = pullWithCrane(context.Background(), cfg)

	if got := os.Getenv("DOCKER_CONFIG"); got != "/sentinel/value" {
		t.Errorf("DOCKER_CONFIG = %q after the pull, want it untouched", got)
	}
}

// The keychain must read the file it was given, including resolving the
// base64 "auth" field the way docker's own config loader does.
func TestConfigFileKeychain_ReadsTheGivenFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	// dXNlcjpwYXNz == "user:pass"
	if err := os.WriteFile(configPath, []byte(`{"auths":{"registry.example.com":{"auth":"dXNlcjpwYXNz"}}}`), 0600); err != nil {
		t.Fatal(err)
	}

	repo, err := name.NewRepository("registry.example.com/app")
	if err != nil {
		t.Fatal(err)
	}
	auth, err := configFileKeychain{path: configPath}.Resolve(repo)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got, err := auth.Authorization()
	if err != nil {
		t.Fatalf("Authorization: %v", err)
	}
	if got.Username != "user" || got.Password != "pass" {
		t.Errorf("got %s/%s, want user/pass", got.Username, got.Password)
	}

	// a registry the file says nothing about gets nothing
	other, err := name.NewRepository("other.example.net/app")
	if err != nil {
		t.Fatal(err)
	}
	otherAuth, err := configFileKeychain{path: configPath}.Resolve(other)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if otherAuth != authn.Anonymous {
		t.Errorf("credentials offered for an unlisted registry: %+v", otherAuth)
	}
}

// The crane path carries more traffic than the oras one — every image index
// and every Docker manifest routes to it — so the same guard has to hold
// there. --insecure builds a transport with InsecureSkipVerify, which is a
// certificate an interceptor can satisfy with anything.
func TestPullWithCrane_RefusesCredentialsOverInsecureTLS(t *testing.T) {
	_, err := pullWithCrane(context.Background(), credsCfg("registry.example.com/app:v1", true, false))
	if err == nil {
		t.Fatal("expected credentials over unverified TLS to be refused")
	}
	if !strings.Contains(err.Error(), "--insecure-allow-credentials") {
		t.Errorf("error = %q, want it to name the opt-out flag", err)
	}
}

// The opt-in has to work, or the guard is just a broken --insecure. This gets
// past the refusal and fails later on the network, which is the point: the
// error must not be the credential one.
func TestPullWithCrane_AllowsCredentialsOverInsecureTLSWhenAskedTo(t *testing.T) {
	cfg := credsCfg("registry.example.com/app:v1", true, true)
	cfg.OutputDir = t.TempDir()
	_, err := pullWithCrane(context.Background(), cfg)
	if err != nil && strings.Contains(err.Error(), "--insecure-allow-credentials") {
		t.Errorf("--insecure-allow-credentials should permit it, got %q", err)
	}
}
