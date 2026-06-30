package cli

import (
	"os"
	"path/filepath"
	"runtime"
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

// writeEditorScript writes an OS-appropriate fake $EDITOR script that
// overwrites its first argument (the temp vault file `secrets edit` passes)
// with editedYAML, then returns the script's path. `secrets edit` execs the
// editor as `exec.Command(editor, tmpPath)` — a single executable + one arg —
// so a script (not a binary+flags string) is the portable way to inject
// content. The YAML is written to a sibling file (not embedded in the script)
// so quoting/escaping never corrupts it.
func writeEditorScript(t *testing.T, editedYAML string) string {
	t.Helper()
	dir := t.TempDir()
	yamlFile := filepath.Join(dir, "edited.yaml")
	if err := os.WriteFile(yamlFile, []byte(editedYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		script := filepath.Join(dir, "editor.bat")
		// %1 = the tmp vault path; copy our YAML over it. /Y suppresses prompt.
		body := "@echo off\r\ncopy /Y \"" + yamlFile + "\" \"%~1\" >nul\r\n"
		if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
		return script
	}
	script := filepath.Join(dir, "editor.sh")
	body := "#!/bin/sh\ncp \"" + yamlFile + "\" \"$1\"\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

// runSecretsEdit drives `secrets edit` with a fake $EDITOR that rewrites the
// vault YAML to editedYAML, exercising the real ExportYAML → editor →
// ImportYAML → audit-diff path.
func runSecretsEdit(t *testing.T, editedYAML string) error {
	t.Helper()
	t.Setenv("EDITOR", writeEditorScript(t, editedYAML))
	cmd := newSecretsCmdReal()
	cmd.SetArgs([]string{"edit"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	return cmd.Execute()
}

// TestCLISecretsEdit_EmitsPerKeyAuditEventsWithoutValues (P2.4 finding 3):
// `secrets edit` (bulk ImportYAML) must emit accurate per-key audit events —
// secret-rotated for added/changed keys, secret-deleted for removed keys —
// carrying key NAMES only, never values, and only after the committed import.
func TestCLISecretsEdit_EmitsPerKeyAuditEventsWithoutValues(t *testing.T) {
	_, logPath := secretsCliAuditEnv(t)
	initVaultForCliTest(t)

	// Seed three keys via the audited set path, then truncate the log so the
	// edit-diff assertion sees ONLY the edit's events.
	const keepVal = "unchanged-value-KEEP"
	const oldChangedVal = "old-value-CHANGED"
	const removedVal = "removed-value-GONE"
	for k, v := range map[string]string{
		"KEEP_KEY":    keepVal,
		"CHANGE_KEY":  oldChangedVal,
		"REMOVE_KEY":  removedVal,
	} {
		if err := runSecretsCmd(t, v, "set", k, "--from-stdin"); err != nil {
			t.Fatalf("seed set %s: %v", k, err)
		}
	}
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// Edited YAML: KEEP unchanged, CHANGE rotated, REMOVE dropped, ADD new.
	const newChangedVal = "new-value-CHANGED"
	const addedVal = "added-value-NEW"
	edited := "KEEP_KEY: " + keepVal + "\n" +
		"CHANGE_KEY: " + newChangedVal + "\n" +
		"ADD_KEY: " + addedVal + "\n"
	if err := runSecretsEdit(t, edited); err != nil {
		t.Fatalf("secrets edit: %v", err)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read hub-mcp.log: %v", err)
	}
	body := string(raw)

	// CHANGE_KEY (value changed) and ADD_KEY (new) → secret-rotated.
	if !strings.Contains(body, "CHANGE_KEY") {
		t.Errorf("changed key CHANGE_KEY missing a rotated event:\n%s", body)
	}
	if !strings.Contains(body, "ADD_KEY") {
		t.Errorf("added key ADD_KEY missing a rotated event:\n%s", body)
	}
	if !strings.Contains(body, `"event":"secret-rotated"`) {
		t.Errorf("no secret-rotated event for added/changed keys:\n%s", body)
	}
	// REMOVE_KEY → secret-deleted.
	if !strings.Contains(body, `"event":"secret-deleted"`) || !strings.Contains(body, "REMOVE_KEY") {
		t.Errorf("removed key REMOVE_KEY missing a secret-deleted event:\n%s", body)
	}
	// KEEP_KEY unchanged → NO event for it.
	if strings.Contains(body, "KEEP_KEY") {
		t.Errorf("unchanged key KEEP_KEY must emit no event:\n%s", body)
	}
	// NO secret VALUE may appear in the audit log.
	for _, v := range []string{keepVal, oldChangedVal, newChangedVal, removedVal, addedVal} {
		if strings.Contains(body, v) {
			t.Errorf("LEAK: secret value %q present in edit audit log:\n%s", v, body)
		}
	}
}

// TestCLISecretsEdit_FailedImportEmitsNoAuditEvent (P2.4 finding 3): if the
// editor writes invalid YAML, ImportYAML fails, the RMW returns non-nil, and
// NO audit event is emitted.
func TestCLISecretsEdit_FailedImportEmitsNoAuditEvent(t *testing.T) {
	_, logPath := secretsCliAuditEnv(t)
	initVaultForCliTest(t)
	if err := runSecretsCmd(t, "v1", "set", "K1", "--from-stdin"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Invalid YAML (a bare scalar that does not unmarshal into map[string]string).
	err := runSecretsEdit(t, ":\n  - not a map\n:::bad")
	if err == nil {
		t.Fatal("secrets edit with invalid YAML: want error, got nil")
	}
	if raw, rerr := os.ReadFile(logPath); rerr == nil {
		b := string(raw)
		if strings.Contains(b, "secret-rotated") || strings.Contains(b, "secret-deleted") {
			t.Errorf("audit event emitted despite failed (uncommitted) edit import:\n%s", b)
		}
	}
}

// TestSecretsConcurrentSetHelperProcess is invoked AS A SUBPROCESS by the
// editor script in the concurrency test below. When MCPHUB_TEST_CONCURRENT_SET
// is set, it writes that key (value from MCPHUB_TEST_CONCURRENT_VAL) directly
// into the PARENT's vault and exits, simulating a concurrent CLI/GUI write that
// lands on disk WHILE the operator's editor session is open (before
// `secrets edit` acquires the flock).
//
// It uses EXPLICIT vault/key paths passed via MCPHUB_TEST_VAULT_PATH /
// MCPHUB_TEST_KEY_PATH rather than secrets.Default*Path(): the cli test binary's
// own TestMain (settings_registry_test.go) re-points LOCALAPPDATA to a fresh
// per-invocation fence dir, so a subprocess that resolved Default*Path() would
// write to a DIFFERENT vault than the parent. The explicit paths bypass that
// re-resolution so the concurrent write lands in the parent's vault.
func TestSecretsConcurrentSetHelperProcess(t *testing.T) {
	key := os.Getenv("MCPHUB_TEST_CONCURRENT_SET")
	if key == "" {
		return // normal test run, not the helper invocation
	}
	val := os.Getenv("MCPHUB_TEST_CONCURRENT_VAL")
	vaultPath := os.Getenv("MCPHUB_TEST_VAULT_PATH")
	keyPath := os.Getenv("MCPHUB_TEST_KEY_PATH")
	err := secrets.WithVaultLock(vaultPath, func() error {
		v, oerr := secrets.OpenVault(keyPath, vaultPath)
		if oerr != nil {
			return oerr
		}
		return v.Set(key, val)
	})
	if err != nil {
		os.Stderr.WriteString(err.Error())
		os.Exit(3)
	}
	os.Exit(0)
}

// writeConcurrentEditorScript writes a fake $EDITOR that FIRST performs a
// concurrent vault write (re-invoking this test binary's concurrent-set helper
// to add concurrentKey into the parent's vault at the EXPLICIT paths) and THEN
// copies editedYAML over the temp file. The concurrent write therefore commits
// to disk BEFORE `secrets edit` acquires the flock — exactly the race the
// under-lock baseline must capture.
func writeConcurrentEditorScript(t *testing.T, editedYAML, concurrentKey, concurrentVal, vaultPath, keyPath string) string {
	t.Helper()
	dir := t.TempDir()
	yamlFile := filepath.Join(dir, "edited.yaml")
	if err := os.WriteFile(yamlFile, []byte(editedYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	self := os.Args[0]
	if runtime.GOOS == "windows" {
		script := filepath.Join(dir, "editor.bat")
		body := "@echo off\r\n" +
			"set \"MCPHUB_TEST_CONCURRENT_SET=" + concurrentKey + "\"\r\n" +
			"set \"MCPHUB_TEST_CONCURRENT_VAL=" + concurrentVal + "\"\r\n" +
			"set \"MCPHUB_TEST_VAULT_PATH=" + vaultPath + "\"\r\n" +
			"set \"MCPHUB_TEST_KEY_PATH=" + keyPath + "\"\r\n" +
			"\"" + self + "\" -test.run=^TestSecretsConcurrentSetHelperProcess$ >nul 2>&1\r\n" +
			"copy /Y \"" + yamlFile + "\" \"%~1\" >nul\r\n"
		if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
		return script
	}
	script := filepath.Join(dir, "editor.sh")
	body := "#!/bin/sh\n" +
		"MCPHUB_TEST_CONCURRENT_SET=" + concurrentKey +
		" MCPHUB_TEST_CONCURRENT_VAL=" + concurrentVal +
		" MCPHUB_TEST_VAULT_PATH=\"" + vaultPath + "\"" +
		" MCPHUB_TEST_KEY_PATH=\"" + keyPath + "\"" +
		" \"" + self + "\" -test.run=^TestSecretsConcurrentSetHelperProcess$ >/dev/null 2>&1\n" +
		"cp \"" + yamlFile + "\" \"$1\"\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

// TestCLISecretsEdit_AuditBaselineComputedUnderLock (bot r4 finding 2): the
// before/after diff that drives the per-key audit events must be computed from
// the on-disk state captured UNDER the same lock as the ImportYAML — NOT a
// stale snapshot taken before the editor session. A concurrent write that
// lands during the edit (here: CONCURRENT_KEY, added by the editor script's
// helper subprocess before the flock is taken) is wholesale-clobbered by
// ImportYAML; the audit MUST report that clobber as secret-deleted. With the
// pre-fix stale-snapshot baseline the concurrent key was invisible, so the
// delete went unrecorded — this test fails on the old code and passes on the
// under-lock baseline.
func TestCLISecretsEdit_AuditBaselineComputedUnderLock(t *testing.T) {
	_, logPath := secretsCliAuditEnv(t)
	initVaultForCliTest(t)

	// Seed one key the operator will keep.
	if err := runSecretsCmd(t, "keepval", "set", "KEEP_KEY", "--from-stdin"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// The operator's edit (derived from the PRE-editor export) keeps only
	// KEEP_KEY. It does NOT mention CONCURRENT_KEY because that key did not
	// exist when the export was taken — the editor script adds it concurrently.
	// Pass the PARENT's resolved vault/key paths so the subprocess writes into
	// THIS test's vault (not its own TestMain fence).
	t.Setenv("EDITOR", writeConcurrentEditorScript(t,
		"KEEP_KEY: keepval\n", "CONCURRENT_KEY", "raceval",
		secrets.DefaultVaultPath(), secrets.DefaultKeyPath()))
	cmd := newSecretsCmdReal()
	cmd.SetArgs([]string{"edit"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("secrets edit: %v", err)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read hub-mcp.log: %v", err)
	}
	body := string(raw)
	// The concurrent key was clobbered by the wholesale ImportYAML; the
	// under-lock baseline must have seen it, so the diff reports it deleted.
	if !strings.Contains(body, `"event":"secret-deleted"`) || !strings.Contains(body, "CONCURRENT_KEY") {
		t.Errorf("under-lock baseline missed the concurrently-added-then-clobbered key (expected secret-deleted CONCURRENT_KEY):\n%s", body)
	}
	// No value may leak.
	if strings.Contains(body, "raceval") || strings.Contains(body, "keepval") {
		t.Errorf("LEAK: a secret value appears in the edit audit log:\n%s", body)
	}
}
