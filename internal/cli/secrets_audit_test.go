package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/secrets"
)

// secretsCliAuditEnv redirects the vault AND the hub-mcp event log into one
// per-test temp dir so a CLI secrets mutation can be driven AND its emitted
// audit line read back. The vault resolves via LOCALAPPDATA
// (secrets.UserDataDir → <root>/mcp-local-hub). The audit log resolves via
// api.DaemonStateDir; the cli package's TestMain installs a GLOBAL
// daemonStateRootOverride (the mcphub-cli-test-state-* fence), which is
// checked BEFORE any env var, so we must point the log at the vault dir via
// api.SetDaemonStateRootForTest (NOT MCPHUB_STATE_DIR_OVERRIDE, which the
// override short-circuits). The override value is used verbatim as the state
// dir, so we set it to <root>/mcp-local-hub to co-locate log with vault.
// Returns the state/log dir + the log path.
func secretsCliAuditEnv(t *testing.T) (stateDir, logPath string) {
	t.Helper()
	root := t.TempDir()
	stateDir = filepath.Join(root, "mcp-local-hub")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOCALAPPDATA", root)  // Windows vault root
	t.Setenv("XDG_DATA_HOME", root) // Linux vault root
	t.Setenv("HOME", root)          // macOS fallback
	restore := api.SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restore)
	return stateDir, filepath.Join(stateDir, "hub-mcp.log")
}

// initVaultForCliTest creates a fresh vault in the redirected dir.
func initVaultForCliTest(t *testing.T) {
	t.Helper()
	keyPath := secrets.DefaultKeyPath()
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := secrets.WithVaultLock(secrets.DefaultVaultPath(), func() error {
		return secrets.InitVault(keyPath, secrets.DefaultVaultPath())
	}); err != nil {
		t.Fatalf("InitVault: %v", err)
	}
}

// runSecretsCmd drives the real `secrets` cobra subtree with args, feeding
// stdinValue (when non-empty) through a redirected os.Stdin so the
// --from-stdin path of `secrets set` works without an interactive TTY.
func runSecretsCmd(t *testing.T, stdinValue string, args ...string) error {
	t.Helper()
	if stdinValue != "" {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		orig := os.Stdin
		os.Stdin = r
		t.Cleanup(func() { os.Stdin = orig })
		go func() {
			_, _ = w.WriteString(stdinValue)
			_ = w.Close()
		}()
	}
	cmd := newSecretsCmdReal()
	cmd.SetArgs(args)
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	return cmd.Execute()
}

func TestCLISecretsSet_EmitsAuditEventWithoutValue(t *testing.T) {
	_, logPath := secretsCliAuditEnv(t)
	initVaultForCliTest(t)

	const val = "cli-super-secret-value-ABC"
	if err := runSecretsCmd(t, val, "set", "WOLFRAM_APP_ID", "--from-stdin"); err != nil {
		t.Fatalf("secrets set: %v", err)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read hub-mcp.log (did MCPHUB_STATE_DIR_OVERRIDE redirect? requires -tags=test_state_path_env): %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `"event":"secret-rotated"`) {
		t.Errorf("secret-rotated event missing after CLI set:\n%s", body)
	}
	if !strings.Contains(body, "WOLFRAM_APP_ID") {
		t.Errorf("set key name missing from audit log:\n%s", body)
	}
	if !strings.Contains(body, `"actor_user"`) {
		t.Errorf("actor_user missing from audit log:\n%s", body)
	}
	if !strings.Contains(body, `"source":"cli"`) {
		t.Errorf("source:cli marker missing from audit log:\n%s", body)
	}
	if strings.Contains(body, val) {
		t.Errorf("LEAK: CLI-set secret value present in audit log:\n%s", body)
	}
}

func TestCLISecretsSet_FailedWriteEmitsNoAuditEvent(t *testing.T) {
	_, logPath := secretsCliAuditEnv(t)
	// Deliberately do NOT init the vault. OpenVault inside the set RMW fails,
	// so the write never commits and no audit event must be emitted.
	err := runSecretsCmd(t, "irrelevant", "set", "SOME_KEY", "--from-stdin")
	if err == nil {
		t.Fatal("secrets set on uninitialized vault: want error, got nil")
	}
	if raw, rerr := os.ReadFile(logPath); rerr == nil {
		if strings.Contains(string(raw), "secret-rotated") {
			t.Errorf("secret-rotated emitted despite failed (uncommitted) CLI set:\n%s", raw)
		}
	}
	// (a missing log file is also acceptable: nothing was emitted.)
}

func TestCLISecretsDelete_EmitsAuditEventWithoutValue(t *testing.T) {
	_, logPath := secretsCliAuditEnv(t)
	initVaultForCliTest(t)

	const val = "cli-delete-me-value-XYZ"
	if err := runSecretsCmd(t, val, "set", "UNPAYWALL_EMAIL", "--from-stdin"); err != nil {
		t.Fatalf("secrets set (setup): %v", err)
	}
	if err := runSecretsCmd(t, "", "delete", "UNPAYWALL_EMAIL"); err != nil {
		t.Fatalf("secrets delete: %v", err)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read hub-mcp.log: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `"event":"secret-deleted"`) {
		t.Errorf("secret-deleted event missing after CLI delete:\n%s", body)
	}
	if !strings.Contains(body, "UNPAYWALL_EMAIL") {
		t.Errorf("deleted key name missing from audit log:\n%s", body)
	}
	if strings.Contains(body, val) {
		t.Errorf("LEAK: CLI-deleted secret value present in audit log:\n%s", body)
	}
}

func TestCLISecretsDelete_FailedWriteEmitsNoAuditEvent(t *testing.T) {
	_, logPath := secretsCliAuditEnv(t)
	initVaultForCliTest(t)

	// Delete a key that was never set → v.Delete returns an error, the RMW
	// returns non-nil, no audit event is emitted.
	err := runSecretsCmd(t, "", "delete", "NEVER_SET_KEY")
	if err == nil {
		t.Fatal("secrets delete on missing key: want error, got nil")
	}
	if raw, rerr := os.ReadFile(logPath); rerr == nil {
		if strings.Contains(string(raw), "secret-deleted") {
			t.Errorf("secret-deleted emitted despite failed (uncommitted) CLI delete:\n%s", raw)
		}
	}
}
