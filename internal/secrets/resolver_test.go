package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolver_Secret(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".age-key")
	vaultPath := filepath.Join(dir, "secrets.age")
	_ = InitVault(keyPath, vaultPath)
	v, _ := OpenVault(keyPath, vaultPath)
	v.Set("API_KEY", "xyz123")

	r := NewResolver(v, nil)
	got, err := r.Resolve("secret:API_KEY")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "xyz123" {
		t.Errorf("Resolve = %q, want xyz123", got)
	}
}

func TestResolver_File(t *testing.T) {
	local := map[string]string{"email": "user@example.com"}
	r := NewResolver(nil, local)
	got, err := r.Resolve("file:email")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "user@example.com" {
		t.Errorf("Resolve = %q, want user@example.com", got)
	}
}

func TestResolver_Env(t *testing.T) {
	t.Setenv("MCP_TEST_VAR", "env-value")
	r := NewResolver(nil, nil)
	got, err := r.Resolve("$MCP_TEST_VAR")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "env-value" {
		t.Errorf("Resolve = %q, want env-value", got)
	}
}

func TestResolver_Literal(t *testing.T) {
	r := NewResolver(nil, nil)
	got, err := r.Resolve("plain-text")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "plain-text" {
		t.Errorf("Resolve = %q, want plain-text", got)
	}
}

func TestResolver_SecretMissing(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".age-key")
	vaultPath := filepath.Join(dir, "secrets.age")
	_ = InitVault(keyPath, vaultPath)
	v, _ := OpenVault(keyPath, vaultPath)

	r := NewResolver(v, nil)
	if _, err := r.Resolve("secret:NONEXISTENT"); err == nil {
		t.Error("expected error for missing secret, got nil")
	}
}

func TestResolver_EnvMissing(t *testing.T) {
	// Ensure variable is not set
	os.Unsetenv("MCP_DEFINITELY_NOT_SET")
	r := NewResolver(nil, nil)
	if _, err := r.Resolve("$MCP_DEFINITELY_NOT_SET"); err == nil {
		t.Error("expected error for missing env var, got nil")
	}
}
