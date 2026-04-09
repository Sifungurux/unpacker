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
