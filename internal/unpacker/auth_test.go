package unpacker

import (
	"testing"
)

func TestResolve_Public(t *testing.T) {
	creds, err := Resolve("", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !creds.Public {
		t.Error("expected Public=true")
	}
}

func TestResolve_ConfigPath(t *testing.T) {
	creds, err := Resolve("/path/to/config.json", false)
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

	creds, err := Resolve("", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds.Username != "user" || creds.Password != "pass" {
		t.Errorf("expected user/pass, got %s/%s", creds.Username, creds.Password)
	}
}

func TestResolve_NoCreds_Error(t *testing.T) {
	t.Setenv("USERNAME", "")
	t.Setenv("PASSWORD", "")

	_, err := Resolve("", false)
	if err == nil {
		t.Error("expected error for private registry without credentials")
	}
}

func TestResolve_PrefixedEnvVars(t *testing.T) {
	t.Setenv("UNPACKER_USERNAME", "prefixed-user")
	t.Setenv("UNPACKER_PASSWORD", "prefixed-pass")
	t.Setenv("USERNAME", "ambient-user")
	t.Setenv("PASSWORD", "ambient-pass")

	creds, err := Resolve("", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The unprefixed names are set by Windows and by many CI images, so the
	// explicit ones have to win rather than merely be available.
	if creds.Username != "prefixed-user" || creds.Password != "prefixed-pass" {
		t.Errorf("got %s/%s, want the UNPACKER_-prefixed values", creds.Username, creds.Password)
	}
}
