package api

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/secrets"
)

// secretsTestEnv redirects DefaultKeyPath / DefaultVaultPath to a
// per-test tempdir so each test gets isolated vault state.
func secretsTestEnv(t *testing.T) (keyPath, vaultPath string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)  // Windows path
	t.Setenv("XDG_DATA_HOME", dir) // Linux path
	t.Setenv("HOME", dir)          // macOS Library fallback root
	keyPath = filepath.Join(dir, "mcp-local-hub", ".age-key")
	vaultPath = filepath.Join(dir, "mcp-local-hub", "secrets.age")
	return keyPath, vaultPath
}

func TestSecretsInit_IdempotentOnExistingVault(t *testing.T) {
	_, _ = secretsTestEnv(t)
	a := NewAPI()

	res1, err := a.SecretsInit()
	if err != nil {
		t.Fatalf("first SecretsInit: %v", err)
	}
	if res1.VaultState != "ok" {
		t.Errorf("first init vault_state = %q, want %q", res1.VaultState, "ok")
	}

	res2, err := a.SecretsInit()
	if err != nil {
		t.Fatalf("second SecretsInit: %v", err)
	}
	if res2.VaultState != "ok" {
		t.Errorf("second init vault_state = %q, want %q", res2.VaultState, "ok")
	}
	if res2.Code != "" || res2.CleanupStatus != "" {
		t.Errorf("idempotent path leaked extra fields: %+v", res2)
	}
}

func TestSecretsInit_FreshInitCreatesFiles(t *testing.T) {
	keyPath, vaultPath := secretsTestEnv(t)
	a := NewAPI()

	if fileExists(keyPath) || fileExists(vaultPath) {
		t.Fatalf("test setup leak: files already exist")
	}

	res, err := a.SecretsInit()
	if err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	if res.VaultState != "ok" {
		t.Errorf("vault_state = %q, want ok", res.VaultState)
	}
	if !fileExists(keyPath) {
		t.Error("key file not created")
	}
	if !fileExists(vaultPath) {
		t.Error("vault file not created")
	}
}

func TestSecretsInit_PartialFailureCleansBothArtifacts(t *testing.T) {
	keyPath, vaultPath := secretsTestEnv(t)
	a := NewAPI()

	// Inject: simulate "key written, vault write failed" — write the key
	// ourselves, write a partial vault, then return error.
	initVaultFn = func(kp, vp string) error {
		if err := os.MkdirAll(filepath.Dir(kp), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(kp, []byte("AGE-SECRET-KEY-FAKE\n"), 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(vp, []byte("partial"), 0o600); err != nil {
			return err
		}
		return fmt.Errorf("simulated mid-init failure")
	}
	defer func() { initVaultFn = secrets.InitVault }()

	_, err := a.SecretsInit()
	var initFailed *SecretsInitFailed
	if !errors.As(err, &initFailed) {
		t.Fatalf("err = %T %v, want *SecretsInitFailed", err, err)
	}
	if initFailed.CleanupStatus != "ok" {
		t.Errorf("cleanup_status = %q, want ok", initFailed.CleanupStatus)
	}
	if fileExists(keyPath) {
		t.Errorf("orphan key file %s still exists after cleanup", keyPath)
	}
	if fileExists(vaultPath) {
		t.Errorf("orphan vault file %s still exists after cleanup", vaultPath)
	}
}

func TestSecretsInit_PartialFailureKeyOnly(t *testing.T) {
	keyPath, vaultPath := secretsTestEnv(t)
	a := NewAPI()

	// Simulate "key written, vault never created".
	initVaultFn = func(kp, vp string) error {
		if err := os.MkdirAll(filepath.Dir(kp), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(kp, []byte("AGE-SECRET-KEY-FAKE\n"), 0o600); err != nil {
			return err
		}
		return fmt.Errorf("simulated key-only failure")
	}
	defer func() { initVaultFn = secrets.InitVault }()

	_, err := a.SecretsInit()
	var initFailed *SecretsInitFailed
	if !errors.As(err, &initFailed) {
		t.Fatalf("err = %T %v, want *SecretsInitFailed", err, err)
	}
	if initFailed.CleanupStatus != "ok" {
		t.Errorf("cleanup_status = %q, want ok (vault never existed, key removed)", initFailed.CleanupStatus)
	}
	if fileExists(keyPath) {
		t.Errorf("orphan key file %s still exists after cleanup", keyPath)
	}
	_ = vaultPath
}

func TestSecretsInit_PartialFailureCleanupAlsoFails(t *testing.T) {
	keyPath, _ := secretsTestEnv(t)
	a := NewAPI()

	// Simulate "key written" then make the parent directory read-only
	// so os.Remove fails. After the test we restore perms so t.TempDir
	// cleanup works.
	parent := filepath.Dir(keyPath)
	initVaultFn = func(kp, vp string) error {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(kp, []byte("AGE-SECRET-KEY-FAKE\n"), 0o600); err != nil {
			return err
		}
		// Lock the parent so os.Remove(kp) inside the wrapper fails.
		if err := os.Chmod(parent, 0o500); err != nil {
			return err
		}
		return fmt.Errorf("simulated mid-init failure with locked parent")
	}
	defer func() {
		initVaultFn = secrets.InitVault
		_ = os.Chmod(parent, 0o700) // restore so t.TempDir cleanup works
	}()

	_, err := a.SecretsInit()
	var initFailed *SecretsInitFailed
	if !errors.As(err, &initFailed) {
		t.Fatalf("err = %T %v, want *SecretsInitFailed", err, err)
	}
	if initFailed.CleanupStatus != "failed" {
		// On Windows, chmod 0o500 may not actually deny os.Remove for
		// the owner. Skip this check on platforms that don't enforce
		// the read-only-dir-blocks-remove invariant.
		if runtime.GOOS == "windows" {
			t.Skip("skipping cleanup-failed assertion on Windows: chmod 0o500 doesn't block owner Remove")
		}
		t.Errorf("cleanup_status = %q, want failed", initFailed.CleanupStatus)
	}
	if initFailed.OrphanPath == "" && runtime.GOOS != "windows" {
		t.Errorf("orphan_path empty, want non-empty when cleanup failed")
	}
}

func TestSecretsInit_OrphanKey(t *testing.T) {
	keyPath, _ := secretsTestEnv(t)
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewAPI()
	_, err := a.SecretsInit()
	var blocked *SecretsInitBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %T %v, want *SecretsInitBlocked", err, err)
	}
	if !blocked.KeyExists || blocked.VaultExists {
		t.Errorf("blocked = %+v, want KeyExists=true VaultExists=false", *blocked)
	}
}

// TestClassifyVaultOpenError_AccessDeniedNotCorrupt is the P2.1 regression:
// a fail-closed read-hardening DACL refusal (the broadened-%LOCALAPPDATA%
// corp/sandbox case) must classify as "access_denied", NOT "corrupt".
// Mis-classifying it as "corrupt" makes the GUI steer the operator toward
// deleting the vault (irreversible data loss) when the real fix is a
// one-command icacls / chmod. The DACL-refusal errors are synthesized
// exactly the way the read path + OpenVault wrapping produce them
// (hub_mcp_state_read_inode_windows.go wraps the typed
// *DACLAllowlistViolation via %w; OpenVault wraps that again as
// "read identity"/"read vault").
func TestClassifyVaultOpenError_AccessDeniedNotCorrupt(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			// Typed chain: secret-bearing relax branch (read access to a
			// non-allowlisted SID) re-wraps the original *DACLAllowlistViolation.
			name: "typed DACL violation through OpenVault read-identity wrap",
			err: fmt.Errorf("read identity: %w",
				fmt.Errorf("file C:\\Users\\x\\AppData\\Local\\mcp-local-hub\\.age-key not single-user safe: %w; default-relax refuses read access granted to a non-allowlisted SID because the state file is secret-bearing.",
					&DACLAllowlistViolation{SID: "S-1-5-21-1-2-3-1001", Mask: 0x80000000})),
			want: "access_denied",
		},
		{
			// Bare ErrDaclOutsideAllowlist sentinel in the chain.
			name: "ErrDaclOutsideAllowlist sentinel through read-vault wrap",
			err:  fmt.Errorf("read vault: %w", fmt.Errorf("file secrets.age not single-user safe: %w", ErrDaclOutsideAllowlist)),
			want: "access_denied",
		},
		{
			// Wrong-owner sentinel: also a permission/identity refusal, not corruption.
			name: "ErrWrongOwner through read-identity wrap",
			err:  fmt.Errorf("read identity: %w", ErrWrongOwner),
			want: "access_denied",
		},
		{
			// Substring fallback: a relax branch that wraps a NON-typed cause
			// (e.g. the write/DAC/DELETE verifier error) but still carries the
			// "not single-user safe" sentinel text.
			name: "substring fallback when typed error not in chain",
			err:  fmt.Errorf("read vault: %w", errors.New("file secrets.age not single-user safe: write access granted to a non-allowlisted SID")),
			want: "access_denied",
		},
		{
			name: "parse identity stays corrupt",
			err:  fmt.Errorf("parse identity: %w", errors.New("bad pem")),
			want: "corrupt",
		},
		{
			name: "unmarshal vault stays corrupt",
			err:  errors.New("unmarshal vault: invalid character"),
			want: "corrupt",
		},
		{
			// Cipher rejection that does NOT also contain the "no identity"
			// substring (which the earlier corrupt discriminator owns for
			// OpenVault's own empty-key-file error). This is the genuine
			// decrypt_failed path: key parses fine, age rejects the header.
			name: "age decrypt header failure stays decrypt_failed",
			err:  errors.New("read vault: age decrypt: parsing age header: unexpected internal error"),
			want: "decrypt_failed",
		},
		{
			name: "unknown stays corrupt",
			err:  errors.New("some other failure"),
			want: "corrupt",
		},
		{
			// bot r7 finding 1: a CORRUPT-identity error whose PATH happens to
			// contain the bare token "DACL" (e.g. a profile/CWD named
			// "DACL-test") must NOT be misclassified as access_denied. The
			// narrowed fallback matches only the exact "not single-user safe"
			// phrase, which a path never contains.
			name: "corrupt error with DACL in the path stays corrupt",
			err:  fmt.Errorf("parse identity: %w", errors.New(`open C:\Users\x\DACL-test\mcp-local-hub\.age-key: bad pem`)),
			want: "corrupt",
		},
		{
			// Sibling: an unmarshal error mentioning DACL in a path is still corrupt.
			name: "unmarshal error with DACL-named dir stays corrupt",
			err:  errors.New(`unmarshal vault: invalid character (vault at /home/u/DACL/secrets.age)`),
			want: "corrupt",
		},
		{
			// ErrTooLoose (POSIX group/world bits) is an access refusal, not corruption.
			name: "ErrTooLoose through read-vault wrap is access_denied",
			err:  fmt.Errorf("read vault: %w", ErrTooLoose),
			want: "access_denied",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyVaultOpenError(tc.err); got != tc.want {
				t.Errorf("classifyVaultOpenError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestVaultAccessDeniedFix_WrongOwnerVsDaclBranch (bot r7 finding 2): a
// wrong-OWNER refusal must yield ownership-repair guidance (NOT DACL-tighten /
// repair-state-dacl, which would not change the owner); a DACL-breadth refusal
// keeps the permission-tighten guidance. This setup redirects the vault to a
// canonical temp location so the DACL branch can legitimately mention
// repair-state-dacl.
func TestVaultAccessDeniedFix_WrongOwnerVsDaclBranch(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("HOME", dir)
	stateDir := filepath.Join(dir, "mcp-local-hub")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Make the canonical vault file exist so VaultAtCanonicalLocation() is true
	// (the DACL branch then may mention repair-state-dacl).
	if err := os.WriteFile(filepath.Join(stateDir, "secrets.age"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	wrongOwner := vaultAccessDeniedFix(fmt.Errorf("read identity: %w", ErrWrongOwner))
	if !strings.Contains(strings.ToLower(wrongOwner), "owner") {
		t.Errorf("wrong-owner Fix should talk about ownership; got %q", wrongOwner)
	}
	if strings.Contains(wrongOwner, "repair-state-dacl") {
		t.Errorf("wrong-owner Fix must NOT suggest repair-state-dacl (does not change owner); got %q", wrongOwner)
	}
	if strings.Contains(wrongOwner, "/inheritance:r") {
		t.Errorf("wrong-owner Fix must NOT give DACL-breadth (icacls /inheritance) guidance; got %q", wrongOwner)
	}

	dacl := vaultAccessDeniedFix(fmt.Errorf("read vault: %w", ErrDaclOutsideAllowlist))
	if !strings.Contains(dacl, "repair-state-dacl") {
		t.Errorf("DACL-breadth Fix (canonical vault) should mention repair-state-dacl; got %q", dacl)
	}
	if strings.Contains(strings.ToLower(dacl), "setowner") || strings.Contains(strings.ToLower(dacl), "chown") {
		t.Errorf("DACL-breadth Fix must NOT give ownership-restore guidance; got %q", dacl)
	}
}

func TestSecretsListWithUsage_OkVault(t *testing.T) {
	_, _ = secretsTestEnv(t)
	manifestDir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)
	if err := os.MkdirAll(filepath.Join(manifestDir, "alpha"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "alpha", "manifest.yaml"),
		[]byte("name: alpha\nenv:\n  OPENAI_API_KEY: secret:K1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewAPI()
	if _, err := a.SecretsInit(); err != nil {
		t.Fatal(err)
	}
	if err := a.SecretsSet("K1", "value-1"); err != nil {
		t.Fatal(err)
	}
	if err := a.SecretsSet("K_orphan", "value-orphan"); err != nil {
		t.Fatal(err)
	}

	env, err := a.SecretsListWithUsage()
	if err != nil {
		t.Fatalf("SecretsListWithUsage: %v", err)
	}
	if env.VaultState != "ok" {
		t.Errorf("vault_state = %q", env.VaultState)
	}
	if len(env.Secrets) != 2 {
		t.Fatalf("len(secrets) = %d, want 2 (K1 present + K_orphan present)", len(env.Secrets))
	}
	// Sorted alphabetically: K1, K_orphan.
	if env.Secrets[0].Name != "K1" || env.Secrets[0].State != "present" {
		t.Errorf("secrets[0] = %+v", env.Secrets[0])
	}
	if len(env.Secrets[0].UsedBy) != 1 || env.Secrets[0].UsedBy[0].Server != "alpha" || env.Secrets[0].UsedBy[0].EnvVar != "OPENAI_API_KEY" {
		t.Errorf("secrets[0].used_by = %+v", env.Secrets[0].UsedBy)
	}
	if env.Secrets[1].Name != "K_orphan" || env.Secrets[1].State != "present" {
		t.Errorf("secrets[1] = %+v", env.Secrets[1])
	}
	if len(env.Secrets[1].UsedBy) != 0 {
		t.Errorf("secrets[1].used_by should be empty")
	}
	if env.Remediation != "" {
		t.Errorf("remediation = %q, want empty for vault_state=ok", env.Remediation)
	}
}

func TestSecretsListWithUsage_ReferencedMissingWhenVaultOk(t *testing.T) {
	_, _ = secretsTestEnv(t)
	manifestDir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)
	if err := os.MkdirAll(filepath.Join(manifestDir, "alpha"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "alpha", "manifest.yaml"),
		[]byte("name: alpha\nenv:\n  WOLFRAM: secret:K_missing\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewAPI()
	if _, err := a.SecretsInit(); err != nil {
		t.Fatal(err)
	}
	env, err := a.SecretsListWithUsage()
	if err != nil {
		t.Fatal(err)
	}
	if env.VaultState != "ok" {
		t.Errorf("vault_state = %q", env.VaultState)
	}
	if len(env.Secrets) != 1 || env.Secrets[0].State != "referenced_missing" || env.Secrets[0].Name != "K_missing" {
		t.Errorf("secrets = %+v", env.Secrets)
	}
}

func TestSecretsListWithUsage_UnverifiedWhenVaultMissing(t *testing.T) {
	_, _ = secretsTestEnv(t)
	manifestDir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)
	if err := os.MkdirAll(filepath.Join(manifestDir, "alpha"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "alpha", "manifest.yaml"),
		[]byte("name: alpha\nenv:\n  OPENAI: secret:K1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Note: do NOT call SecretsInit. Vault should be missing.
	a := NewAPI()

	env, err := a.SecretsListWithUsage()
	if err != nil {
		t.Fatal(err)
	}
	if env.VaultState != "missing" {
		t.Errorf("vault_state = %q, want missing", env.VaultState)
	}
	if len(env.Secrets) != 1 || env.Secrets[0].State != "referenced_unverified" {
		t.Errorf("secrets[0] = %+v, want referenced_unverified", env.Secrets[0])
	}
	if env.Remediation != "" {
		t.Errorf("remediation = %q, want empty for vault_state=missing", env.Remediation)
	}
}

// TestSecretsListWithUsage_RemediationSingleOwner pins the deep-review P3
// single-owner refactor: the /api/secrets envelope carries the backend's
// vaultAccessDeniedFix text verbatim in Remediation when vault_state ==
// "access_denied", and Remediation is empty for a non-access_denied state
// (already covered above/elsewhere, re-asserted here for the "ok" case to
// keep the two states pinned side by side).
func TestSecretsListWithUsage_RemediationSingleOwner(t *testing.T) {
	keyPath, vaultPath := secretsTestEnv(t)
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("placeholder-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vaultPath, []byte("placeholder-vault"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantErr := fmt.Errorf("file %s not single-user safe: %w",
		vaultPath,
		&DACLAllowlistViolation{SID: "S-1-5-21-1234", Mask: 0x120089},
	)
	restore := secrets.SetVaultFileReader(func(path string) ([]byte, error) {
		return nil, wantErr
	})
	defer restore()

	a := NewAPI()
	env, err := a.SecretsListWithUsage()
	if err != nil {
		t.Fatal(err)
	}
	if env.VaultState != "access_denied" {
		t.Fatalf("vault_state = %q, want access_denied", env.VaultState)
	}
	want := vaultAccessDeniedFix(wantErr)
	if env.Remediation != want {
		t.Errorf("remediation = %q, want %q (vaultAccessDeniedFix output)", env.Remediation, want)
	}
	if env.Remediation == "" {
		t.Error("remediation must not be empty for vault_state=access_denied")
	}
}

func TestClassifyVault_DACLRefusalIsAccessDenied(t *testing.T) {
	keyPath, vaultPath := secretsTestEnv(t)
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("placeholder-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vaultPath, []byte("placeholder-vault"), 0o600); err != nil {
		t.Fatal(err)
	}
	restore := secrets.SetVaultFileReader(func(path string) ([]byte, error) {
		return nil, fmt.Errorf("file %s not single-user safe: %w",
			path,
			&DACLAllowlistViolation{SID: "S-1-5-21-1234", Mask: 0x120089},
		)
	})
	defer restore()

	state, keys, accessErr := classifyVault(keyPath, vaultPath)
	if state != "access_denied" {
		t.Fatalf("classifyVault state = %q, want access_denied", state)
	}
	if keys != nil {
		t.Fatalf("classifyVault keys = %+v, want nil", keys)
	}
	if accessErr == nil {
		t.Fatal("classifyVault accessErr = nil, want the underlying OpenVault error")
	}
}

func TestClassifyVault_DACLSubstringFallbackIsAccessDenied(t *testing.T) {
	keyPath, vaultPath := secretsTestEnv(t)
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("placeholder-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vaultPath, []byte("placeholder-vault"), 0o600); err != nil {
		t.Fatal(err)
	}
	restore := secrets.SetVaultFileReader(func(path string) ([]byte, error) {
		return nil, fmt.Errorf("read gate refused: file %s not single-user safe: DACL grants read to a non-owner SID", path)
	})
	defer restore()

	state, _, accessErr := classifyVault(keyPath, vaultPath)
	if state != "access_denied" {
		t.Fatalf("classifyVault state = %q, want access_denied", state)
	}
	if accessErr == nil {
		t.Fatal("classifyVault accessErr = nil, want the underlying OpenVault error")
	}
}

func TestSecretsRotate_OverwritesExisting(t *testing.T) {
	_, _ = secretsTestEnv(t)
	a := NewAPI()
	if _, err := a.SecretsInit(); err != nil {
		t.Fatal(err)
	}
	if err := a.SecretsSet("K1", "old"); err != nil {
		t.Fatal(err)
	}
	res, err := a.SecretsRotate("K1", "new", false)
	if err != nil {
		t.Fatalf("SecretsRotate: %v", err)
	}
	if !res.VaultUpdated {
		t.Error("vault_updated = false")
	}
	if len(res.RestartResults) != 0 {
		t.Errorf("restart_results = %+v, want empty (restart=false)", res.RestartResults)
	}
}

// secretsAuditTestEnv composes secretsTestEnv (vault paths) with a
// co-located hub-mcp event log so a test can both mutate the vault AND
// read back the emitted audit line. It points daemonStateRootOverride at
// the vault's own state dir so LogHubMcpEvent writes
// <vaultDir>/hub-mcp.log next to the vault, never the operator's real log.
func secretsAuditTestEnv(t *testing.T) (keyPath, vaultPath, logPath string) {
	t.Helper()
	keyPath, vaultPath = secretsTestEnv(t)
	statePathsHelper(t) // saves+restores daemonStateRootOverride
	stateDir := filepath.Dir(vaultPath)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	daemonStateRootOverride = stateDir
	return keyPath, vaultPath, filepath.Join(stateDir, "hub-mcp.log")
}

// TestSecretsRotate_EmitsAuditEventWithoutValue (P2.4): a successful rotate
// must record a secret-rotated audit event carrying the key NAME + actor
// but NEVER the secret value (old or new).
func TestSecretsRotate_EmitsAuditEventWithoutValue(t *testing.T) {
	_, _, logPath := secretsAuditTestEnv(t)
	a := NewAPI()
	if _, err := a.SecretsInit(); err != nil {
		t.Fatal(err)
	}
	const oldVal = "old-super-secret-value-OLD"
	const newVal = "new-super-secret-value-NEW"
	if err := a.SecretsSet("WOLFRAM_APP_ID", oldVal); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SecretsRotate("WOLFRAM_APP_ID", newVal, false); err != nil {
		t.Fatalf("SecretsRotate: %v", err)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read hub-mcp.log: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `"event":"secret-rotated"`) {
		t.Errorf("secret-rotated event missing from log:\n%s", body)
	}
	if !strings.Contains(body, "WOLFRAM_APP_ID") {
		t.Errorf("rotated key name missing from audit log:\n%s", body)
	}
	if !strings.Contains(body, `"actor_user"`) {
		t.Errorf("actor_user missing from audit log:\n%s", body)
	}
	// The load-bearing no-value guarantee: neither the old nor the new
	// secret value may ever appear in the audit log.
	if strings.Contains(body, oldVal) {
		t.Errorf("LEAK: old secret value present in audit log:\n%s", body)
	}
	if strings.Contains(body, newVal) {
		t.Errorf("LEAK: new secret value present in audit log:\n%s", body)
	}
}

// TestSecretsRotate_FailedWriteEmitsNoAuditEvent (P2.4): a rotate that
// never commits the vault (key-not-found) must NOT emit a misleading
// secret-rotated record.
func TestSecretsRotate_FailedWriteEmitsNoAuditEvent(t *testing.T) {
	_, _, logPath := secretsAuditTestEnv(t)
	a := NewAPI()
	if _, err := a.SecretsInit(); err != nil {
		t.Fatal(err)
	}
	// Rotate a key that does not exist → SECRETS_KEY_NOT_FOUND, no commit.
	_, err := a.SecretsRotate("NEVER_SET_KEY", "irrelevant", false)
	if err == nil {
		t.Fatal("SecretsRotate on missing key: want error, got nil")
	}
	if raw, rerr := os.ReadFile(logPath); rerr == nil {
		if strings.Contains(string(raw), "secret-rotated") {
			t.Errorf("secret-rotated emitted despite failed (uncommitted) rotate:\n%s", raw)
		}
	}
	// (a missing log file is also acceptable: nothing was emitted.)
}

// TestSecretsDelete_EmitsAuditEventWithoutValue (P2.4): a successful
// (confirmed) delete must record a secret-deleted audit event carrying the
// key NAME + actor but never the secret value.
func TestSecretsDelete_EmitsAuditEventWithoutValue(t *testing.T) {
	_, _, logPath := secretsAuditTestEnv(t)
	a := NewAPI()
	if _, err := a.SecretsInit(); err != nil {
		t.Fatal(err)
	}
	const val = "delete-me-secret-value-XYZ"
	if err := a.SecretsSet("UNPAYWALL_EMAIL", val); err != nil {
		t.Fatal(err)
	}
	// confirm=true bypasses the ref/scan guards and commits the delete.
	if err := a.SecretsDelete("UNPAYWALL_EMAIL", true); err != nil {
		t.Fatalf("SecretsDelete: %v", err)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read hub-mcp.log: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `"event":"secret-deleted"`) {
		t.Errorf("secret-deleted event missing from log:\n%s", body)
	}
	if !strings.Contains(body, "UNPAYWALL_EMAIL") {
		t.Errorf("deleted key name missing from audit log:\n%s", body)
	}
	if strings.Contains(body, val) {
		t.Errorf("LEAK: secret value present in audit log:\n%s", body)
	}
}

// TestSecretsDelete_RefusedEmitsNoAuditEvent (P2.4): a delete refused by the
// D5 guard (key-not-found, no commit) must NOT emit secret-deleted.
func TestSecretsDelete_RefusedEmitsNoAuditEvent(t *testing.T) {
	_, _, logPath := secretsAuditTestEnv(t)
	a := NewAPI()
	if _, err := a.SecretsInit(); err != nil {
		t.Fatal(err)
	}
	err := a.SecretsDelete("NEVER_SET_KEY", true)
	if err == nil {
		t.Fatal("SecretsDelete on missing key: want error, got nil")
	}
	if raw, rerr := os.ReadFile(logPath); rerr == nil {
		if strings.Contains(string(raw), "secret-deleted") {
			t.Errorf("secret-deleted emitted despite refused (uncommitted) delete:\n%s", raw)
		}
	}
}

// TestSecretsSet_EmitsAuditEventWithoutValue (P2.4 finding 1): the GUI
// Add-Secret path (POST /api/secrets → SecretsSet) must record a
// secret-rotated audit event with the key NAME + actor + created marker, and
// NEVER the secret value.
func TestSecretsSet_EmitsAuditEventWithoutValue(t *testing.T) {
	_, _, logPath := secretsAuditTestEnv(t)
	a := NewAPI()
	if _, err := a.SecretsInit(); err != nil {
		t.Fatal(err)
	}
	const val = "gui-add-secret-value-NEW"
	if err := a.SecretsSet("PAPER_SEARCH_KEY", val); err != nil {
		t.Fatalf("SecretsSet: %v", err)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read hub-mcp.log: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `"event":"secret-rotated"`) {
		t.Errorf("secret-rotated event missing after SecretsSet:\n%s", body)
	}
	if !strings.Contains(body, "PAPER_SEARCH_KEY") {
		t.Errorf("set key name missing from audit log:\n%s", body)
	}
	if !strings.Contains(body, `"created":true`) {
		t.Errorf("created:true marker missing from audit log:\n%s", body)
	}
	if !strings.Contains(body, `"actor_user"`) {
		t.Errorf("actor_user missing from audit log:\n%s", body)
	}
	if strings.Contains(body, val) {
		t.Errorf("LEAK: SecretsSet value present in audit log:\n%s", body)
	}
}

// TestSecretsSet_FailedWriteEmitsNoAuditEvent (P2.4 finding 1): a rejected
// SecretsSet (duplicate key → SECRETS_KEY_EXISTS, no commit) must NOT emit a
// misleading secret-rotated record.
func TestSecretsSet_FailedWriteEmitsNoAuditEvent(t *testing.T) {
	_, _, logPath := secretsAuditTestEnv(t)
	a := NewAPI()
	if _, err := a.SecretsInit(); err != nil {
		t.Fatal(err)
	}
	if err := a.SecretsSet("DUP_KEY", "first"); err != nil {
		t.Fatal(err)
	}
	// Truncate the log so the legitimate first-set event does not pollute the
	// assertion; we are testing that the SECOND (rejected) set emits nothing.
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := a.SecretsSet("DUP_KEY", "second")
	if err == nil {
		t.Fatal("SecretsSet on existing key: want SECRETS_KEY_EXISTS, got nil")
	}
	if raw, rerr := os.ReadFile(logPath); rerr == nil {
		if strings.Contains(string(raw), "secret-rotated") {
			t.Errorf("secret-rotated emitted despite rejected (uncommitted) SecretsSet:\n%s", raw)
		}
	}
}

// TestAPIAuditEmitsRunWithoutVaultMutexHeld (bot r7 finding 3): the API
// SecretsSet / SecretsRotate / SecretsDelete audit emits must run AFTER
// vaultMutex is released (LogHubMcpEvent takes a blocking flock; coupling it
// with the vault mutex is a contention/deadlock hazard). The probe runs at
// emit time and asserts vaultMutex is acquirable (TryLock succeeds) — which is
// only true when the emit is out-of-lock.
func TestAPIAuditEmitsRunWithoutVaultMutexHeld(t *testing.T) {
	_, _, _ = secretsAuditTestEnv(t)
	a := NewAPI()
	if _, err := a.SecretsInit(); err != nil {
		t.Fatal(err)
	}

	var emits int
	restore := setAuditEmitProbeForTest(func(event string) {
		// If the emit runs while vaultMutex is held, TryLock fails → finding 3.
		if !vaultMutex.TryLock() {
			t.Errorf("audit emit %q ran while vaultMutex was HELD (finding 3: emit must be out-of-lock)", event)
			return
		}
		vaultMutex.Unlock()
		emits++
	})
	defer restore()

	// SecretsSet (create) → secret-rotated emit.
	if err := a.SecretsSet("K_AUDIT_LOCK", "v1"); err != nil {
		t.Fatalf("SecretsSet: %v", err)
	}
	// SecretsRotate → secret-rotated emit.
	if _, err := a.SecretsRotate("K_AUDIT_LOCK", "v2", false); err != nil {
		t.Fatalf("SecretsRotate: %v", err)
	}
	// SecretsDelete (confirm) → secret-deleted emit.
	if err := a.SecretsDelete("K_AUDIT_LOCK", true); err != nil {
		t.Fatalf("SecretsDelete: %v", err)
	}
	if emits != 3 {
		t.Errorf("expected 3 out-of-lock audit emits (set+rotate+delete); got %d", emits)
	}
}

func TestVaultFlockHelperProcess(t *testing.T) {
	if os.Getenv("MCPHUB_TEST_HOLD_VAULT_LOCK") != "1" {
		return
	}
	vaultPath := os.Getenv("MCPHUB_TEST_VAULT_PATH")
	readyPath := os.Getenv("MCPHUB_TEST_READY_PATH")
	releasePath := os.Getenv("MCPHUB_TEST_RELEASE_PATH")
	if vaultPath == "" || readyPath == "" || releasePath == "" {
		fmt.Fprintln(os.Stderr, "missing helper env")
		os.Exit(2)
	}
	err := secrets.WithVaultLock(vaultPath, func() error {
		if err := os.WriteFile(readyPath, []byte("ready"), 0o600); err != nil {
			return err
		}
		for {
			if _, err := os.Stat(releasePath); err == nil {
				return nil
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func startVaultFlockHolder(t *testing.T, vaultPath string) func() {
	t.Helper()
	dir := t.TempDir()
	readyPath := filepath.Join(dir, "ready")
	releasePath := filepath.Join(dir, "release")
	cmd := exec.Command(os.Args[0], "-test.run=TestVaultFlockHelperProcess")
	cmd.Env = append(os.Environ(),
		"MCPHUB_TEST_HOLD_VAULT_LOCK=1",
		"MCPHUB_TEST_VAULT_PATH="+vaultPath,
		"MCPHUB_TEST_READY_PATH="+readyPath,
		"MCPHUB_TEST_RELEASE_PATH="+releasePath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start vault flock helper: %v", err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		_ = os.WriteFile(releasePath, []byte("release"), 0o600)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("vault flock helper exited: %v; stderr: %s", err, stderr.String())
			}
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatalf("vault flock helper did not exit after release; stderr: %s", stderr.String())
		}
	}
	t.Cleanup(release)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			return release
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat helper ready file: %v", err)
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("vault flock helper did not acquire lock; stderr: %s", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSecretsRotate_TakesVaultFlock(t *testing.T) {
	_, vaultPath := secretsTestEnv(t)
	a := NewAPI()
	if _, err := a.SecretsInit(); err != nil {
		t.Fatal(err)
	}
	if err := a.SecretsSet("K1", "old"); err != nil {
		t.Fatal(err)
	}

	release := startVaultFlockHolder(t, vaultPath)
	done := make(chan error, 1)
	go func() {
		_, err := a.SecretsRotate("K1", "new", false)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("SecretsRotate completed while another process held the flock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SecretsRotate after flock release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SecretsRotate did not complete after flock release")
	}
	v, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.Get("K1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "new" {
		t.Fatalf("K1 = %q, want new", got)
	}
}

func TestSecretsDelete_TakesVaultFlock(t *testing.T) {
	_, vaultPath := secretsTestEnv(t)
	a := NewAPI()
	if _, err := a.SecretsInit(); err != nil {
		t.Fatal(err)
	}
	if err := a.SecretsSet("K1", "old"); err != nil {
		t.Fatal(err)
	}

	release := startVaultFlockHolder(t, vaultPath)
	done := make(chan error, 1)
	go func() { done <- a.SecretsDelete("K1", true) }()
	select {
	case err := <-done:
		t.Fatalf("SecretsDelete completed while another process held the flock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SecretsDelete after flock release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SecretsDelete did not complete after flock release")
	}
	v, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Get("K1"); err == nil {
		t.Fatal("K1 still present after delete")
	}
}

func TestSecretsDelete_RequiresConfirmWithRefs(t *testing.T) {
	_, _ = secretsTestEnv(t)
	manifestDir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)
	if err := os.MkdirAll(filepath.Join(manifestDir, "alpha"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "alpha", "manifest.yaml"),
		[]byte("name: alpha\nenv:\n  OPENAI: secret:K1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	if _, err := a.SecretsInit(); err != nil {
		t.Fatal(err)
	}
	if err := a.SecretsSet("K1", "v"); err != nil {
		t.Fatal(err)
	}
	err := a.SecretsDelete("K1", false)
	var de *SecretsDeleteError
	if !errors.As(err, &de) {
		t.Fatalf("err = %T %v, want *SecretsDeleteError", err, err)
	}
	if de.Code != "SECRETS_HAS_REFS" {
		t.Errorf("code = %q", de.Code)
	}
	if len(de.UsedBy) != 1 || de.UsedBy[0].Server != "alpha" || de.UsedBy[0].EnvVar != "OPENAI" {
		t.Errorf("used_by = %+v", de.UsedBy)
	}
}

func TestSecretsInit_RaceDoesNotDeleteConcurrentVault(t *testing.T) {
	keyPath, vaultPath := secretsTestEnv(t)
	a := NewAPI()

	// Simulate concurrent init: another request just created both files.
	// initVaultFn returns the "already exists" error (matches what
	// secrets.InitVault returns at vault.go:28-33).
	initVaultFn = func(kp, vp string) error {
		// Pretend another request wrote both files just before we got here.
		if err := os.MkdirAll(filepath.Dir(kp), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(kp, []byte("CONCURRENT-IDENTITY"), 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(vp, []byte("CONCURRENT-VAULT-CIPHERTEXT"), 0o600); err != nil {
			return err
		}
		return fmt.Errorf("identity file already exists: %s", kp)
	}
	defer func() { initVaultFn = secrets.InitVault }()

	_, err := a.SecretsInit()
	var blocked *SecretsInitBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %T %v, want *SecretsInitBlocked (race must NOT trigger cleanup)", err, err)
	}

	// CRITICAL: the concurrent request's files MUST still exist.
	keyBytes, kerr := os.ReadFile(keyPath)
	if kerr != nil {
		t.Fatalf("concurrent key file was deleted by cleanup: %v", kerr)
	}
	if string(keyBytes) != "CONCURRENT-IDENTITY" {
		t.Errorf("key file contents changed: %q", keyBytes)
	}
	vaultBytes, verr := os.ReadFile(vaultPath)
	if verr != nil {
		t.Fatalf("concurrent vault file was deleted by cleanup: %v", verr)
	}
	if string(vaultBytes) != "CONCURRENT-VAULT-CIPHERTEXT" {
		t.Errorf("vault file contents changed: %q", vaultBytes)
	}
}

func TestSecretsInit_VaultAlreadyExistsRaceDoesNotDelete(t *testing.T) {
	keyPath, vaultPath := secretsTestEnv(t)
	a := NewAPI()

	// Simulate the rarer race where only the vault file exists from a
	// concurrent request (e.g. they're between key write and vault write).
	initVaultFn = func(kp, vp string) error {
		if err := os.MkdirAll(filepath.Dir(kp), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(kp, []byte("OUR-PARTIAL-KEY"), 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(vp, []byte("CONCURRENT-VAULT"), 0o600); err != nil {
			return err
		}
		return fmt.Errorf("vault file already exists: %s", vp)
	}
	defer func() { initVaultFn = secrets.InitVault }()

	_, err := a.SecretsInit()
	var blocked *SecretsInitBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %T %v, want *SecretsInitBlocked", err, err)
	}
	// Both files should remain.
	if _, kerr := os.Stat(keyPath); kerr != nil {
		t.Errorf("key file deleted: %v", kerr)
	}
	if _, verr := os.Stat(vaultPath); verr != nil {
		t.Errorf("vault file deleted: %v", verr)
	}
}

func TestSecretsDelete_WithConfirmBypassesRefs(t *testing.T) {
	_, _ = secretsTestEnv(t)
	manifestDir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)
	if err := os.MkdirAll(filepath.Join(manifestDir, "alpha"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "alpha", "manifest.yaml"),
		[]byte("name: alpha\nenv:\n  OPENAI: secret:K1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	if _, err := a.SecretsInit(); err != nil {
		t.Fatal(err)
	}
	if err := a.SecretsSet("K1", "v"); err != nil {
		t.Fatal(err)
	}
	if err := a.SecretsDelete("K1", true); err != nil {
		t.Fatalf("SecretsDelete confirm=true: %v", err)
	}
}

func TestSecretsInit_ConcurrentCallsSerializeCleanly(t *testing.T) {
	_, _ = secretsTestEnv(t)
	a := NewAPI()

	// Spawn N concurrent SecretsInit calls. With the package mutex in
	// place, calls are serialized: exactly one performs the actual
	// InitVault write; all subsequent callers acquire the lock after the
	// vault is committed and hit the Case-1 idempotent OpenVault path,
	// returning vault_state="ok". No call should return an error (errors
	// indicate corrupt state or orphan detection, not normal idempotent
	// return). The critical post-condition is that the resulting vault is
	// openable — proving no key/vault mismatch occurred.
	const N = 8
	type result struct {
		res SecretsInitResult
		err error
	}
	results := make(chan result, N)
	var wg sync.WaitGroup
	for range N {
		wg.Go(func() {
			r, e := a.SecretsInit()
			results <- result{r, e}
		})
	}
	wg.Wait()
	close(results)

	okCount := 0
	for r := range results {
		if r.err != nil {
			t.Errorf("unexpected error from concurrent SecretsInit: %v", r.err)
			continue
		}
		if r.res.VaultState == "ok" {
			okCount++
		} else {
			t.Errorf("unexpected vault_state %q (want ok)", r.res.VaultState)
		}
	}
	if okCount != N {
		t.Errorf("expected all %d callers to return vault_state=ok (serialized idempotent path), got %d", N, okCount)
	}

	// Verify the vault is actually openable with the written key.
	// This is the critical invariant: no key/vault mismatch from interleaved writes.
	v, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		t.Fatalf("vault not openable after concurrent init: %v", err)
	}
	if len(v.List()) != 0 {
		t.Errorf("expected empty vault after init, got keys: %v", v.List())
	}
}

func TestSecretsSet_RejectsReservedName(t *testing.T) {
	_, _ = secretsTestEnv(t)
	a := NewAPI()
	if _, err := a.SecretsInit(); err != nil {
		t.Fatal(err)
	}
	err := a.SecretsSet("init", "value")
	var opErr *SecretsOpError
	if !errors.As(err, &opErr) {
		t.Fatalf("err = %T %v, want *SecretsOpError", err, err)
	}
	if opErr.Code != "SECRETS_INVALID_NAME" {
		t.Errorf("code = %q, want SECRETS_INVALID_NAME", opErr.Code)
	}
	if !strings.Contains(opErr.Msg, "reserved") {
		t.Errorf("Msg = %q, want to mention 'reserved'", opErr.Msg)
	}
}

func TestSecretsSet_ConcurrentCallsAreSerialized(t *testing.T) {
	_, _ = secretsTestEnv(t)
	a := NewAPI()
	if _, err := a.SecretsInit(); err != nil {
		t.Fatal(err)
	}

	// Spawn N concurrent SecretsSet calls with distinct names.
	// All should succeed; the resulting vault must be openable and
	// contain all N keys (no FS-level corruption from interleaved saves).
	const N = 16
	errs := make(chan error, N)
	var wg sync.WaitGroup
	for i := range N {
		wg.Go(func() {
			errs <- a.SecretsSet(fmt.Sprintf("KEY_%d", i), fmt.Sprintf("value_%d", i))
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Errorf("SecretsSet failed: %v", e)
		}
	}

	v, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		t.Fatalf("vault unreadable after concurrent Set: %v", err)
	}
	if got := len(v.List()); got != N {
		t.Errorf("vault has %d keys, want %d (concurrent Sets lost updates)", got, N)
	}
}
