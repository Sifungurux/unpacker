package unpacker

import (
	"context"
	"strings"
	"testing"

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
