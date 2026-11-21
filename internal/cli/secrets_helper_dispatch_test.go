package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/process"
	"mcp-local-hub/internal/secrets"
)

func TestCLITestBinaryConcurrentSecretsHelperUsesCallerStateRoot(t *testing.T) {
	stateDir, _ := secretsCliAuditEnv(t)
	apitest.HardenedDir(t, stateDir)
	vaultPath := filepath.Join(stateDir, "secrets.age")
	keyPath := filepath.Join(stateDir, ".age-key")
	if secrets.DefaultVaultPath() != vaultPath || secrets.DefaultKeyPath() != keyPath {
		t.Fatal("vault fixture resolved outside its owned state directory")
	}
	initVaultForCliTest(t)

	// The ordinary parent TestMain owns its package root. The child must use
	// this vault's root without creating another package root in childTemp.
	childTemp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSecretsConcurrentSetHelperProcess$")
	process.NoConsole(cmd)
	cmd.Env = append(withoutCLITestHelperEnvironment(os.Environ()),
		"TEMP="+childTemp, "TMP="+childTemp, "TMPDIR="+childTemp,
		"MCPHUB_TEST_CONCURRENT_SET=fixture-key",
		"MCPHUB_TEST_CONCURRENT_VAL=fixture-value",
		"MCPHUB_TEST_VAULT_PATH="+vaultPath,
		"MCPHUB_TEST_KEY_PATH="+keyPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start original test helper: %v", err)
	}
	// This is the sole waiter; CommandContext kills only this original child
	// at the deadline, and Wait settles it before fixture cleanup.
	waitErr := cmd.Wait()
	roots, globErr := filepath.Glob(filepath.Join(childTemp, "mcphub-cli-test-state-*"))
	if waitErr != nil || ctx.Err() != nil || globErr != nil || len(roots) != 0 {
		t.Fatalf("helper wait=%v deadline=%v roots=%d glob=%v stderr=%s", waitErr, ctx.Err(), len(roots), globErr, stderr.String())
	}
	vault, err := secrets.OpenVault(keyPath, vaultPath)
	if err != nil {
		t.Fatalf("read caller vault: %v", err)
	}
	value, err := vault.Get("fixture-key")
	if err != nil || value != "fixture-value" {
		t.Fatal("helper did not update the caller vault")
	}
}
