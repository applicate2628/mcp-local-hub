//go:build windows

package secrets

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestVaultSaveAtomicUnsupportedParentSyncIsNonFatal(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".age-key")
	vaultPath := filepath.Join(dir, "secrets.age")
	if err := InitVault(keyPath, vaultPath); err != nil {
		t.Fatalf("InitVault: %v", err)
	}
	v, err := OpenVault(keyPath, vaultPath)
	if err != nil {
		t.Fatalf("OpenVault: %v", err)
	}

	previousSync := vaultAtomicSyncParentDir
	var syncedDir string
	vaultAtomicSyncParentDir = func(dir string) error {
		syncedDir = dir
		return windows.ERROR_INVALID_FUNCTION
	}
	t.Cleanup(func() { vaultAtomicSyncParentDir = previousSync })

	if err := v.Set("API_KEY", "new-value"); err != nil {
		t.Fatalf("unsupported parent directory sync must not fail vault write: %v", err)
	}
	if syncedDir != dir {
		t.Fatalf("synced parent dir = %q, want %q", syncedDir, dir)
	}
	if _, err := os.Stat(vaultPath); err != nil {
		t.Fatalf("vault destination missing after unsupported parent sync: %v", err)
	}
}
