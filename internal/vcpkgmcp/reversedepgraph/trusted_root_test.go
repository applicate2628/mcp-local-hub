package reversedepgraph

import (
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/process"
)

func TestBindTrustedRootReturnsCanonicalOperatorRoot(t *testing.T) {
	trusted := t.TempDir()
	want, err := process.CanonicalizePathStrict(trusted)
	if err != nil {
		t.Fatal(err)
	}
	got, err := BindTrustedRoot(trusted, trusted)
	if err != nil {
		t.Fatalf("matching trusted root rejected: %v", err)
	}
	if got != want {
		t.Fatalf("bound root = %q, want trusted canonical root %q", got, want)
	}
}

func TestBindTrustedRootAdmitsAliasButReturnsTrustedCanonicalPath(t *testing.T) {
	trusted := t.TempDir()
	alias := filepath.Join(t.TempDir(), "trusted-alias")
	if err := os.Symlink(trusted, alias); err != nil {
		t.Skipf("directory link unavailable: %v", err)
	}
	want, err := process.CanonicalizePathStrict(trusted)
	if err != nil {
		t.Fatal(err)
	}
	got, err := BindTrustedRoot(alias, trusted)
	if err != nil {
		t.Fatalf("trusted alias rejected: %v", err)
	}
	if got != want {
		t.Fatalf("bound root = %q, want trusted canonical root %q", got, want)
	}
}

func TestBindTrustedRootRefusesUnresolvableOrNonDirectoryInputs(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("not a root"), 0o600); err != nil {
		t.Fatal(err)
	}
	trusted := t.TempDir()
	for name, roots := range map[string]struct{ requested, configured string }{
		"missing-requested": {requested: missing, configured: trusted},
		"missing-trusted":   {requested: missing, configured: missing},
		"file-requested":    {requested: file, configured: trusted},
		"file-trusted":      {requested: file, configured: file},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := BindTrustedRoot(roots.requested, roots.configured); err == nil {
				t.Fatal("unresolvable or non-directory root was accepted")
			}
		})
	}
}
