package api

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestOpenVaultAllowsVaultBlobAboveHubStateCap(t *testing.T) {
	keyPath, vaultPath := newVaultReadHardeningFixture(t)
	vault, err := secrets.OpenVault(keyPath, vaultPath)
	if err != nil {
		t.Fatalf("OpenVault initial: %v", err)
	}
	want := strings.Repeat("x", int(maxStateFileBytes)+1)
	if err := vault.Set("BIG", want); err != nil {
		t.Fatalf("set large vault value: %v", err)
	}
	info, err := os.Stat(vaultPath)
	if err != nil {
		t.Fatalf("stat large vault: %v", err)
	}
	if info.Size() <= maxStateFileBytes {
		t.Fatalf("large vault fixture size = %d, want above hub-state cap %d", info.Size(), maxStateFileBytes)
	}

	reopened, err := secrets.OpenVault(keyPath, vaultPath)
	if err != nil {
		t.Fatalf("OpenVault rejected vault blob above hub-state cap: %v", err)
	}
	got, err := reopened.Get("BIG")
	if err != nil {
		t.Fatalf("Get BIG after reopen: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("BIG value length = %d, want %d", len(got), len(want))
	}
}

func TestReadHubEndpointRefusesReadBroadenedDefaultMode(t *testing.T) {
	stateDir := hubMcpStateTestHelper(t)
	target := filepath.Join(stateDir, hubMcpEndpointFileLeaf)
	if err := os.WriteFile(target, []byte(`{"port":9125,"instance_id":"secret-instance","pid":123,"started_at":"2026-06-20T12:00:00Z"}`), 0o600); err != nil {
		t.Fatalf("write endpoint: %v", err)
	}
	broadenVaultReadForTest(t, target)

	_, err := readHubMcpStateFile(hubMcpEndpointFileLeaf)
	if err == nil {
		t.Fatalf("readHubMcpStateFile read-broadened %s; want fail-closed", hubMcpEndpointFileLeaf)
	}
	if !errors.Is(err, vaultReadBroadeningErrorForTest()) {
		t.Fatalf("read-broadened endpoint err = %v, want %v", err, vaultReadBroadeningErrorForTest())
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
