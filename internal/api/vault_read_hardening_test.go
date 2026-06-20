package api

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/secrets"
)

func newVaultReadHardeningFixture(t *testing.T) (string, string) {
	t.Helper()
	statePathsHelper(t)
	stateDir := hardenedTempDir(t)
	daemonStateRootOverride = stateDir
	resetStrictModeIntentCacheForTest()
	t.Setenv(RequireSingleUserHomeEnv, "")

	dir := hardenedTempDir(t)
	keyPath := filepath.Join(dir, ".age-key")
	vaultPath := filepath.Join(dir, "secrets.age")
	if err := secrets.InitVault(keyPath, vaultPath); err != nil {
		t.Fatalf("InitVault: %v", err)
	}
	return keyPath, vaultPath
}

func TestOpenVaultRefusesReadBroadenedAgeKeyDefaultMode(t *testing.T) {
	keyPath, vaultPath := newVaultReadHardeningFixture(t)
	broadenVaultReadForTest(t, keyPath)

	_, err := secrets.OpenVault(keyPath, vaultPath)
	if err == nil {
		t.Fatalf("OpenVault read broadened .age-key; want fail-closed")
	}
	if !errors.Is(err, vaultReadBroadeningErrorForTest()) {
		t.Fatalf("OpenVault read-broadened .age-key err = %v, want %v", err, vaultReadBroadeningErrorForTest())
	}
}

func TestOpenVaultRefusesReadBroadenedVaultBlobDefaultMode(t *testing.T) {
	keyPath, vaultPath := newVaultReadHardeningFixture(t)
	broadenVaultReadForTest(t, vaultPath)

	_, err := secrets.OpenVault(keyPath, vaultPath)
	if err == nil {
		t.Fatalf("OpenVault read broadened secrets.age; want fail-closed")
	}
	if !errors.Is(err, vaultReadBroadeningErrorForTest()) {
		t.Fatalf("OpenVault read-broadened secrets.age err = %v, want %v", err, vaultReadBroadeningErrorForTest())
	}
}

func TestOpenVaultRefusesSymlinkedAgeKey(t *testing.T) {
	keyPath, vaultPath := newVaultReadHardeningFixture(t)
	linkPath := filepath.Join(filepath.Dir(keyPath), ".age-key-link")
	if err := os.Symlink(keyPath, linkPath); err != nil {
		t.Skipf("symlink unsupported in this environment: %v", err)
	}

	_, err := secrets.OpenVault(linkPath, vaultPath)
	if err == nil {
		t.Fatalf("OpenVault followed symlinked .age-key; want fail-closed")
	}
	if !errors.Is(err, ErrIrregularFile) {
		t.Fatalf("OpenVault symlinked .age-key err = %v, want ErrIrregularFile", err)
	}
}

func TestOpenVaultRefusesSymlinkedVaultBlob(t *testing.T) {
	keyPath, vaultPath := newVaultReadHardeningFixture(t)
	linkPath := filepath.Join(filepath.Dir(vaultPath), "secrets-link.age")
	if err := os.Symlink(vaultPath, linkPath); err != nil {
		t.Skipf("symlink unsupported in this environment: %v", err)
	}

	_, err := secrets.OpenVault(keyPath, linkPath)
	if err == nil {
		t.Fatalf("OpenVault followed symlinked secrets.age; want fail-closed")
	}
	if !errors.Is(err, ErrIrregularFile) {
		t.Fatalf("OpenVault symlinked secrets.age err = %v, want ErrIrregularFile", err)
	}
}
