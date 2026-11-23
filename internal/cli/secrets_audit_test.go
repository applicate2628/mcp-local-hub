package cli

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
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
	return secretsCliAuditEnvAt(t, t.TempDir())
}

func secretsCliAuditEnvAt(t *testing.T, root string) (stateDir, logPath string) {
	t.Helper()
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
	script := writeEditorScript(t, editedYAML)
	editor := installEditorTestCommand(t, script, func(path string) error {
		return os.WriteFile(path, []byte(editedYAML), 0o600)
	})
	t.Setenv("EDITOR", editor)
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
		"KEEP_KEY":   keepVal,
		"CHANGE_KEY": oldChangedVal,
		"REMOVE_KEY": removedVal,
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

const (
	concurrentSecretsSelector     = "-test.run=^TestSecretsConcurrentSetHelperProcess$"
	concurrentSecretsRootEnv      = "MCPHUB_TEST_CONCURRENT_STATE_ROOT"
	concurrentSecretsDirPrefix    = "mcphub-cli-secrets-concurrent-"
	concurrentSecretsStateLeaf    = "mcp-local-hub"
	concurrentSecretsFixtureKey   = "CONCURRENT_KEY"
	concurrentSecretsFixtureValue = "raceval"
)

// The old child inputs carried independent destinations and values.
// They conflict with this single-root synthetic role.
var obsoleteConcurrentSecretsHelperEnv = [...]string{
	"MCPHUB_TEST_CONCURRENT_SET", "MCPHUB_TEST_CONCURRENT_VAL",
	"MCPHUB_TEST_VAULT_PATH", "MCPHUB_TEST_KEY_PATH",
}

func validConcurrentSecretsFixtureRoot(root string) bool {
	parent := filepath.Base(filepath.Dir(root))
	return filepath.IsAbs(root) && filepath.Clean(root) == root &&
		filepath.Base(root) == concurrentSecretsStateLeaf &&
		strings.HasPrefix(parent, concurrentSecretsDirPrefix) && parent != concurrentSecretsDirPrefix
}

func concurrentSecretsTempBase() (string, error) {
	base, err := filepath.Abs(os.TempDir())
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(base)
}

type concurrentSecretsPaths struct {
	root, key, vault, lock string
}

func concurrentSecretsFixturePaths(root string) concurrentSecretsPaths {
	vault := filepath.Join(root, "secrets.age")
	return concurrentSecretsPaths{root, filepath.Join(root, ".age-key"), vault, vault + ".lock"}
}

type concurrentSecretsObjectIdentity struct {
	Volume, File, Creation uint64
}

type concurrentSecretsFixtureIdentity struct {
	Root, Key, Vault, Lock concurrentSecretsObjectIdentity
}

func (expected concurrentSecretsFixtureIdentity) sameScope(actual concurrentSecretsFixtureIdentity) bool {
	return expected.Root == actual.Root && expected.Key == actual.Key && expected.Lock == actual.Lock
}

func observeConcurrentSecretsFixture(ctx context.Context, paths concurrentSecretsPaths) (concurrentSecretsFixtureIdentity, os.FileInfo, error) {
	var result concurrentSecretsFixtureIdentity
	if !validConcurrentSecretsFixtureRoot(paths.root) || os.Getenv(api.RequireSingleUserHomeEnv) != "1" {
		return result, nil, errors.New("invalid concurrent secrets fixture root or policy")
	}
	base, err := concurrentSecretsTempBase()
	if err != nil {
		return result, nil, errors.New("concurrent secrets fixture temp base unavailable")
	}
	parent := filepath.Dir(paths.root)
	relative, err := filepath.Rel(base, parent)
	if err != nil || relative != filepath.Base(parent) {
		return result, nil, errors.New("concurrent secrets fixture is outside its temp namespace")
	}
	if exists, err := api.SecureVerifyExistingParentDirForConfigLock(ctx, paths.root); err != nil || !exists {
		return result, nil, errors.New("concurrent secrets fixture directory ownership or path refused")
	}
	var lockInfo os.FileInfo
	for _, object := range []struct {
		path string
		dir  bool
		id   *concurrentSecretsObjectIdentity
	}{
		{paths.root, true, &result.Root},
		{paths.key, false, &result.Key},
		{paths.vault, false, &result.Vault},
		{paths.lock, false, &result.Lock},
	} {
		identity, info, err := observeConcurrentSecretsObject(object.path, object.dir)
		if err != nil {
			return result, nil, errors.New("concurrent secrets fixture object identity refused")
		}
		*object.id = identity
		if object.path == paths.lock {
			lockInfo = info
		} else if !object.dir && info.Size() == 0 {
			return result, nil, errors.New("concurrent secrets fixture key or vault is empty")
		}
	}
	return result, lockInfo, nil
}

// The parent keeps the hardened temp fixture private through child completion
// and import. The lock-handle check binds that fixture's actual acquired lock.
func withConcurrentSecretsFixtureLock(ctx context.Context, paths concurrentSecretsPaths, expected concurrentSecretsFixtureIdentity) (retErr error) {
	before, lockInfo, err := observeConcurrentSecretsFixture(ctx, paths)
	if err != nil {
		return err
	}
	if before != expected {
		return errors.New("concurrent secrets fixture identity changed before lock")
	}
	// Use the vault's existing cross-process lock protocol, but prohibit
	// creation: this actor can only acquire the prepared original lock.
	lock := flock.New(paths.lock, flock.SetFlag(os.O_RDWR))
	if err := lock.Lock(); err != nil {
		return errors.New("acquire original concurrent secrets fixture lock")
	}
	defer func() {
		if err := lock.Unlock(); err != nil {
			retErr = errors.Join(retErr, errors.New("release original concurrent secrets fixture lock"))
		}
	}()
	acquired, err := lock.Stat()
	if err != nil || !os.SameFile(lockInfo, acquired) {
		return errors.New("concurrent secrets fixture lock identity changed at acquisition")
	}
	held, heldLockInfo, err := observeConcurrentSecretsFixture(ctx, paths)
	if err != nil {
		return err
	}
	if held != expected || !os.SameFile(acquired, heldLockInfo) {
		return errors.New("concurrent secrets fixture identity changed under lock")
	}
	vault, err := secrets.OpenVault(paths.key, paths.vault)
	if err != nil {
		return errors.New("open original concurrent secrets fixture vault")
	}
	if err := vault.Set(concurrentSecretsFixtureKey, concurrentSecretsFixtureValue); err != nil {
		return errors.New("update original concurrent secrets fixture vault")
	}
	return nil
}

// The central TestMain selector admits this child before package fixture
// setup. A marker-free run of the same test remains an ordinary no-op.
func TestSecretsConcurrentSetHelperProcess(t *testing.T) {
	dispatch := classifyCLITestHelperDispatch(os.Args, os.LookupEnv)
	if dispatch.invalid {
		t.Fatal("invalid concurrent secrets helper dispatch")
	}
	if dispatch.concurrentSecretsRoot == "" {
		return
	}
	var expected concurrentSecretsFixtureIdentity
	if err := binary.Read(os.Stdin, binary.LittleEndian, &expected); err != nil {
		t.Fatal("receive concurrent secrets fixture identity")
	}
	var extra [1]byte
	if _, err := io.ReadFull(os.Stdin, extra[:]); !errors.Is(err, io.EOF) {
		t.Fatal("concurrent secrets fixture identity has trailing input")
	}
	if err := withConcurrentSecretsFixtureLock(t.Context(), concurrentSecretsFixturePaths(dispatch.concurrentSecretsRoot), expected); err != nil {
		t.Fatal(err)
	}
}

// This case's existing editor actor admits the selected child while the
// editor is open and before import takes the vault lock.
func editWithConcurrentSecretsFixture(t *testing.T, paths concurrentSecretsPaths, expected concurrentSecretsFixtureIdentity, editedYAML string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal("resolve concurrent secrets helper executable")
	}
	edit := func(path string) error {
		ctx := t.Context()
		if deadline, ok := t.Deadline(); ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, deadline)
			defer cancel()
		}
		var proof bytes.Buffer
		if err := binary.Write(&proof, binary.LittleEndian, expected); err != nil {
			return errors.New("encode concurrent secrets fixture identity")
		}
		cmd := exec.CommandContext(ctx, exe, concurrentSecretsSelector)
		cmd.Env = append(withoutCLITestHelperEnvironment(cmd.Environ()),
			concurrentSecretsRootEnv+"="+paths.root,
			api.RequireSingleUserHomeEnv+"=1",
		)
		cmd.Stdin = &proof
		if err := cmd.Run(); err != nil {
			return errors.New("concurrent secrets fixture child failed")
		}
		actual, _, err := observeConcurrentSecretsFixture(ctx, paths)
		if err != nil || !expected.sameScope(actual) || actual.Vault == expected.Vault {
			return errors.New("concurrent secrets fixture child did not replace only its vault")
		}
		return os.WriteFile(path, []byte(editedYAML), 0o600)
	}
	editor := installEditorTestCommand(t, exe, edit)
	if runtime.GOOS != "windows" {
		// Windows installs the callback in installEditorTestCommand.
		// This case can use the same seam without shell interpolation.
		originalRun := editorCommandRun
		t.Cleanup(func() { editorCommandRun = originalRun })
		editorCommandRun = func(cmd *exec.Cmd) error {
			if len(cmd.Args) != 2 {
				return errors.New("concurrent secrets fixture editor args refused")
			}
			return edit(cmd.Args[1])
		}
	}
	return editor
}

// TestCLISecretsEdit_AuditBaselineComputedUnderLock (bot r4 finding 2): the
// before/after diff that drives the per-key audit events must be computed from
// the on-disk state captured UNDER the same lock as the ImportYAML — NOT a
// stale snapshot taken before the editor session. A concurrent write that
// lands during the edit (here: CONCURRENT_KEY, added by the selected fixture
// subprocess before the flock is taken) is wholesale-clobbered by
// ImportYAML; the audit MUST report that clobber as secret-deleted. With the
// pre-fix stale-snapshot baseline the concurrent key was invisible, so the
// delete went unrecorded — this test fails on the old code and passes on the
// under-lock baseline.
func TestCLISecretsEdit_AuditBaselineComputedUnderLock(t *testing.T) {
	base, err := concurrentSecretsTempBase()
	if err != nil {
		t.Fatal("resolve concurrent secrets fixture temp base")
	}
	root, err := os.MkdirTemp(base, concurrentSecretsDirPrefix)
	if err != nil {
		t.Fatal("create concurrent secrets fixture root")
	}
	t.Cleanup(func() {
		if err := apitest.RemoveTestMainRoot(root); err != nil {
			t.Error("remove concurrent secrets fixture root")
		}
	})
	apitest.HardenedDir(t, root)
	stateDir, logPath := secretsCliAuditEnvAt(t, root)
	apitest.HardenedDir(t, stateDir)
	t.Setenv(api.RequireSingleUserHomeEnv, "1")
	paths := concurrentSecretsFixturePaths(stateDir)
	// Prepare the exact pair and keeper seed under the real vault lock.
	// Default path discovery is safe only after both fixture leaves exist.
	if err := secrets.WithVaultLock(paths.vault, func() error {
		if err := secrets.InitVault(paths.key, paths.vault); err != nil {
			return err
		}
		vault, err := secrets.OpenVault(paths.key, paths.vault)
		if err != nil {
			return err
		}
		return vault.Set("KEEP_KEY", "keepval")
	}); err != nil {
		t.Fatal("initialize and seed exact concurrent secrets fixture")
	}
	expected, _, err := observeConcurrentSecretsFixture(t.Context(), paths)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// The exported edit keeps only KEEP_KEY. The selected child adds the
	// concurrent key to this fixture before the import takes its lock.
	const editedYAML = "KEEP_KEY: keepval\n"
	editor := editWithConcurrentSecretsFixture(t, paths, expected, editedYAML)
	if secrets.DefaultKeyPath() != paths.key || secrets.DefaultVaultPath() != paths.vault {
		t.Fatal("real editor case did not select the prepared fixture pair")
	}
	t.Setenv("EDITOR", editor)
	cmd := newSecretsCmdReal()
	cmd.SetArgs([]string{"edit"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	if err := cmd.Execute(); err != nil {
		t.Fatal("concurrent secrets fixture edit failed")
	}
	final, _, err := observeConcurrentSecretsFixture(t.Context(), paths)
	if err != nil || !expected.sameScope(final) {
		t.Fatal("edited fixture lost its original root, key, or lock")
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
	if strings.Contains(body, concurrentSecretsFixtureValue) || strings.Contains(body, "keepval") {
		t.Errorf("LEAK: a secret value appears in the edit audit log:\n%s", body)
	}
}

// TestCLISecretsMigrate_AuditAndUnderLockBaseline (bot r7 finding 4): migrate
// must (a) audit each imported key by NAME (never value), and (b) apply each
// Set to the CURRENT on-disk vault state, not a snapshot taken once before the
// interactive prompt loop. A key written concurrently DURING the prompt window
// (here: CONCURRENT_KEY, written by a goroutine that fires after migrate has
// already opened+scanned and is blocked reading the y/N prompt) must SURVIVE
// the import — proving migrate re-opens the vault under the lock rather than
// clobbering with a stale pre-loop snapshot. With the pre-fix code (one v
// opened before the loop) the concurrent key was clobbered.
func TestCLISecretsMigrate_AuditAndUnderLockBaseline(t *testing.T) {
	_, logPath := secretsCliAuditEnv(t)
	// claude-code's config path resolves via os.UserHomeDir() (Windows:
	// USERPROFILE), which secretsCliAuditEnv does NOT set — pin it to the same
	// root so the vault, log, and .claude.json co-locate.
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	initVaultForCliTest(t)

	// Seed the claude-code config at the path clientConfigPath actually
	// resolves, with one importable candidate (matches secretLineRe: KEY
	// ending in _TOKEN, non-placeholder).
	claudeCfg, cperr := clientConfigPath("claude-code")
	if cperr != nil {
		t.Fatalf("resolve claude-code config path: %v", cperr)
	}
	if err := os.MkdirAll(filepath.Dir(claudeCfg), 0o700); err != nil {
		t.Fatal(err)
	}
	const importedVal = "real-migrate-token-ABC123"
	cfg := `{"mcpServers":{"x":{"env":{"MIGRATE_API_TOKEN":"` + importedVal + `"}}}}`
	if err := os.WriteFile(claudeCfg, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	// Concurrent writer: add CONCURRENT_KEY to the vault once the migrate has
	// started (it blocks on the first y/N prompt). A small delay lets migrate
	// open+scan the config and reach the prompt before this commits.
	concurrentDone := make(chan struct{})
	go func() {
		defer close(concurrentDone)
		time.Sleep(150 * time.Millisecond)
		_ = secrets.WithVaultLock(secrets.DefaultVaultPath(), func() error {
			v, oerr := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
			if oerr != nil {
				return oerr
			}
			return v.Set("CONCURRENT_KEY", "raceval")
		})
	}()

	// Feed "y\n" after a delay longer than the concurrent write, so the import
	// commits AFTER CONCURRENT_KEY is on disk — exercising re-open-under-lock.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = origStdin })
	go func() {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.WriteString("y\n")
		_ = w.Close()
	}()

	cmd := newSecretsCmdReal()
	cmd.SetArgs([]string{"migrate", "--from-client", "claude-code"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("secrets migrate: %v", err)
	}
	<-concurrentDone

	// The imported key must be in the vault.
	v, oerr := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if oerr != nil {
		t.Fatalf("open vault: %v", oerr)
	}
	if _, gerr := v.Get("MIGRATE_API_TOKEN"); gerr != nil {
		t.Errorf("imported key MIGRATE_API_TOKEN missing from vault: %v", gerr)
	}
	// UNDER-LOCK PROOF: the concurrently-added key must SURVIVE (not clobbered
	// by a stale pre-loop snapshot).
	if _, gerr := v.Get("CONCURRENT_KEY"); gerr != nil {
		t.Errorf("CONCURRENT_KEY was clobbered by migrate — Set used a stale pre-loop snapshot, not the under-lock baseline (finding 4): %v", gerr)
	}

	// Audit: secret-rotated with the imported key NAME, source cli-migrate, no value.
	raw, rerr := os.ReadFile(logPath)
	if rerr != nil {
		t.Fatalf("read hub-mcp.log: %v", rerr)
	}
	body := string(raw)
	if !strings.Contains(body, `"event":"secret-rotated"`) || !strings.Contains(body, "MIGRATE_API_TOKEN") {
		t.Errorf("migrate did not audit the imported key by name:\n%s", body)
	}
	if !strings.Contains(body, `"source":"cli-migrate"`) {
		t.Errorf("migrate audit missing source=cli-migrate:\n%s", body)
	}
	if strings.Contains(body, importedVal) {
		t.Errorf("LEAK: imported secret value present in migrate audit log:\n%s", body)
	}
}
