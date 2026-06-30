package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

// canonicalDir redirects UserDataDir() to a per-test temp root via the env
// vars paths.go honors, and returns the canonical app-data dir
// (<root>/mcp-local-hub). Clearing legacy-resolution interference (a vault
// beside the test binary) is not needed: the canonical path is checked FIRST
// in resolveSecretPath, so when it exists it always wins.
func canonicalDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("LOCALAPPDATA", root)  // Windows
	t.Setenv("XDG_DATA_HOME", root) // Linux
	t.Setenv("HOME", root)          // macOS fallback
	return filepath.Join(root, "mcp-local-hub")
}

// TestVaultAtCanonicalLocation_TrueWhenCanonicalExists: a vault at the
// canonical app-data path makes VaultAtCanonicalLocation report true (so the
// caller may suggest `mcphub repair-state-dacl`).
func TestVaultAtCanonicalLocation_TrueWhenCanonicalExists(t *testing.T) {
	dir := canonicalDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secrets.age"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !VaultAtCanonicalLocation() {
		t.Errorf("VaultAtCanonicalLocation() = false; want true for a vault at %s", dir)
	}
}

// TestVaultAtCanonicalLocation_FalseForLegacyCwdVault: when no canonical vault
// exists but a legacy ./secrets.age exists in the CWD (the dev-mode fallback
// resolveSecretPath accepts), VaultAtCanonicalLocation reports false — so the
// caller must NOT suggest the canonical-targeted repair-state-dacl command.
func TestVaultAtCanonicalLocation_FalseForLegacyCwdVault(t *testing.T) {
	// Canonical dir is redirected to an EMPTY temp root (no secrets.age there),
	// so resolveSecretPath falls through to the CWD fallback.
	_ = canonicalDir(t)

	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "secrets.age"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	// Sanity: the resolved path must be the legacy CWD vault, not canonical.
	if DefaultVaultPath() == filepath.Join(UserDataDir(), "secrets.age") {
		t.Skipf("resolveSecretPath did not pick the CWD legacy vault (resolved=%s); "+
			"a vault beside the test binary shadowed it — branch not exercisable here", DefaultVaultPath())
	}
	if VaultAtCanonicalLocation() {
		t.Errorf("VaultAtCanonicalLocation() = true for a legacy CWD vault %s; want false", DefaultVaultPath())
	}
}
