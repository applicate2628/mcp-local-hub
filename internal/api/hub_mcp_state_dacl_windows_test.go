//go:build windows

// hub_mcp_state_dacl_windows_test.go — Windows-specific DACL synthesis
// tests for the shared DACL primitives used by inode-anchored reads.
//
// Builds a DACL that includes a read-capable ALLOW ACE for the
// Authenticated Users SID (S-1-5-11), applies it to a fresh file via
// SetNamedSecurityInfo, and asserts that the inode-anchored reader rejects
// the file with ErrDaclOutsideAllowlist.

package api

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// TestReadStateFileInodeAnchoredRejectsAuthenticatedUsersAllow builds a
// DACL granting GENERIC_READ to S-1-5-11 (Authenticated Users) on a
// freshly-created file, then asserts the reader rejects it with
// ErrDaclOutsideAllowlist. This is the canonical enterprise-stance
// test: Group-Policy ACLs that grant read to Domain Users / Auth
// Users / corporate management SIDs must fail closed.
//
// Uses hardenedTempDir so the parent-dir DACL gate accepts the
// parent; the only failure signal under test is the FILE's own DACL.
func TestReadStateFileInodeAnchoredRejectsAuthenticatedUsersAllow(t *testing.T) {
	dir := hardenedTempDir(t)
	target := filepath.Join(dir, hubMcpTokensFileLeaf)
	if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}

	applyFileDACLWithAuthUsersReadACE(t, target)

	_, err := readStateFileInodeAnchored(target)
	if err == nil {
		t.Fatalf("readStateFileInodeAnchored must reject Authenticated Users read ALLOW; got nil")
	}
	if !errors.Is(err, ErrDaclOutsideAllowlist) {
		t.Errorf("expected ErrDaclOutsideAllowlist, got %v", err)
	}
}

func applyFileDACLWithAuthUsersReadACE(t *testing.T, target string) {
	t.Helper()
	currentSID, err := currentUserSID()
	if err != nil {
		t.Fatalf("currentUserSID: %v", err)
	}
	authUsersSID, err := windows.StringToSid("S-1-5-11")
	if err != nil {
		t.Fatalf("Authenticated Users sid: %v", err)
	}

	// Divergent fixture: NOT the allowlist triple — current-user
	// GENERIC_ALL plus a disallowed Authenticated Users GENERIC_READ
	// ACE. Kept open-coded so the verifier's reject path is exercised;
	// only the ACLFromEntries + SetNamedSecurityInfo apply boilerplate
	// is shared.
	entries := []windows.EXPLICIT_ACCESS{
		explicitAccessAllow(currentSID, windows.TRUSTEE_IS_USER, windows.GENERIC_ALL),
		explicitAccessAllow(authUsersSID, windows.TRUSTEE_IS_WELL_KNOWN_GROUP, windows.GENERIC_READ),
	}
	applyProtectedDACLFromEntries(t, target, entries)
}

func applyFileDACLWithAuthUsersWriteACE(t *testing.T, target string) {
	t.Helper()
	currentSID, err := currentUserSID()
	if err != nil {
		t.Fatalf("currentUserSID: %v", err)
	}
	authUsersSID, err := windows.StringToSid("S-1-5-11")
	if err != nil {
		t.Fatalf("Authenticated Users sid: %v", err)
	}

	entries := []windows.EXPLICIT_ACCESS{
		explicitAccessAllow(currentSID, windows.TRUSTEE_IS_USER, windows.GENERIC_ALL),
		explicitAccessAllow(authUsersSID, windows.TRUSTEE_IS_WELL_KNOWN_GROUP, windows.GENERIC_WRITE),
	}
	applyProtectedDACLFromEntries(t, target, entries)
}

// TestReadStateFileInodeAnchoredAcceptsAllowlistOnly synthesizes the
// happy-path DACL (current-user + LocalSystem + BuiltinAdministrators
// GENERIC_ALL) and asserts the reader accepts. Symmetric coverage for
// the synthesis suite — without this case we can't tell the reject test
// passed for the right reason.
//
// Uses hardenedTempDir so both the parent-dir AND the file-DACL gates
// pass; if either failed for a different reason, this test would
// surface that ambiguity.
func TestReadStateFileInodeAnchoredAcceptsAllowlistOnly(t *testing.T) {
	dir := hardenedTempDir(t)
	target := filepath.Join(dir, "tight.json")
	if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}

	entries, err := allowlistExplicitAccess()
	if err != nil {
		t.Fatalf("allowlistExplicitAccess: %v", err)
	}
	applyProtectedDACLFromEntries(t, target, entries)

	if _, err := readStateFileInodeAnchored(target); err != nil {
		t.Errorf("readStateFileInodeAnchored must accept allowlist-only DACL; got %v", err)
	}
}

// applyAllowlistOnlyDACL applies an allowlist-conforming PROTECTED
// DACL to target via SetNamedSecurityInfo. Used by the parent-DACL
// reject test below to ensure the FILE's own DACL is conforming, so
// the only signal under test is the parent-dir DACL gate.
func applyAllowlistOnlyDACL(t *testing.T, target string) {
	t.Helper()
	entries, err := allowlistExplicitAccess()
	if err != nil {
		t.Fatalf("allowlistExplicitAccess: %v", err)
	}
	applyProtectedDACLFromEntries(t, target, entries)
}

// TestReadStateFileInodeAnchoredRejectsPermissiveParentDACL pins the
// STRICT-mode behavior (MCPHUB_REQUIRE_SINGLE_USER_HOME=1): a
// parent dir with an Authenticated Users:GenericRead ACE is
// rejected even when the file's own DACL is allowlist-conforming.
//
// v0.4.2 inverts the default — see
// TestReadStateFileInodeAnchoredAcceptsPermissiveParentDACL_DefaultRelax
// below. Strict mode preserves the v0.4.0-v0.4.1 refuse-on-broadened-
// parent posture for corp-managed / multi-tenant hosts.
func TestReadStateFileInodeAnchoredRejectsPermissiveParentDACL(t *testing.T) {
	t.Setenv(RequireSingleUserHomeEnv, "1") // STRICT mode

	parent := filepath.Join(t.TempDir(), "leaky-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	synthesizeDirWithAuthUsersReadACE(t, parent)

	target := filepath.Join(parent, "hub-mcp-tokens.json")
	if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	applyAllowlistOnlyDACL(t, target)

	_, err := readStateFileInodeAnchored(target)
	if err == nil {
		t.Fatalf("readStateFileInodeAnchored must reject permissive parent dir under strict mode; got nil")
	}
	if !errors.Is(err, ErrDaclOutsideAllowlist) {
		t.Errorf("expected ErrDaclOutsideAllowlist (wrapped), got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, parent) {
		t.Errorf("error %q must mention parent dir %q", msg, parent)
	}
	if !strings.Contains(msg, "parent") {
		t.Errorf("error %q must use the word 'parent' to signal the dir-DACL gate", msg)
	}
}

// TestReadStateFileInodeAnchoredAcceptsPermissiveParentDACL_DefaultRelax
// covers v0.4.2's new default: when MCPHUB_REQUIRE_SINGLE_USER_HOME
// is NOT set, a permissive parent dir is tolerated. The file's own
// DACL is the load-bearing safety layer; the file handle is opened
// relative to the parent handle and read directly.
//
// Manual-smoke motivation: workstation %LOCALAPPDATA%\mcp-local-hub
// broadened to a third-party installer SID. Without this relax, B4
// marker reads (PR #187) and other state-file operations failed
// closed, breaking every matrix Apply on the GUI.
func TestReadStateFileInodeAnchoredAcceptsPermissiveParentDACL_DefaultRelax(t *testing.T) {
	t.Setenv(RequireSingleUserHomeEnv, "") // DEFAULT mode

	parent := filepath.Join(t.TempDir(), "leaky-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	synthesizeDirWithAuthUsersReadACE(t, parent)

	target := filepath.Join(parent, "hub-mcp-tokens.json")
	if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	applyAllowlistOnlyDACL(t, target)

	if _, err := readStateFileInodeAnchored(target); err != nil {
		t.Errorf("default-relax: expected nil (file DACL is allowlist-clean); got %v", err)
	}
}

func TestReadStateFileInodeAnchored_FileDACLDefaultRelaxesStrictRejects(t *testing.T) {
	statePathsHelper(t)
	stateDir := hardenedTempDir(t)
	daemonStateRootOverride = stateDir
	resetStrictModeIntentCacheForTest()
	t.Setenv(RequireSingleUserHomeEnv, "")

	dir := hardenedTempDir(t)
	target := filepath.Join(dir, "supervisor-intent.json")
	want := []byte(`{"strict_mode":false}`)
	if err := os.WriteFile(target, want, 0600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	applyFileDACLWithAuthUsersReadACE(t, target)

	got, err := readStateFileInodeAnchored(target)
	if err != nil {
		t.Fatalf("default mode must read file with broadened DACL via inode-anchored handle: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("read payload = %q, want %q", got, want)
	}

	events, err := RecentHubMcpEvents(20)
	if err != nil {
		t.Fatalf("read recent events: %v", err)
	}
	var found map[string]any
	for _, ev := range events {
		if ev["event"] == "hub-mcp-state-read-unhardened-file-fallback" {
			found = ev
			break
		}
	}
	if found == nil {
		t.Fatalf("no hub-mcp-state-read-unhardened-file-fallback event in last %d entries (got %d events)", 20, len(events))
	}
	if found["level"] != "warn" {
		t.Errorf("event level = %v, want \"warn\"", found["level"])
	}
	if path, _ := found["path"].(string); path != target {
		t.Errorf("event path = %v, want %q", found["path"], target)
	}
	if reason, _ := found["reason"].(string); !strings.Contains(reason, "default-relax-on-solo-host") {
		t.Errorf("event reason = %v, want default-relax-on-solo-host", found["reason"])
	}

	t.Setenv(RequireSingleUserHomeEnv, "1")
	if _, err := readStateFileInodeAnchored(target); err == nil {
		t.Fatalf("strict mode must reject file with broadened DACL")
	} else if !errors.Is(err, ErrDaclOutsideAllowlist) {
		t.Fatalf("strict mode err = %v, want ErrDaclOutsideAllowlist", err)
	}
}

func TestReadStateFileInodeAnchored_FileDACLWriteBroadenedDefaultRejects(t *testing.T) {
	statePathsHelper(t)
	stateDir := hardenedTempDir(t)
	daemonStateRootOverride = stateDir
	resetStrictModeIntentCacheForTest()
	t.Setenv(RequireSingleUserHomeEnv, "")

	dir := hardenedTempDir(t)
	target := filepath.Join(dir, "supervisor-intent.json")
	if err := os.WriteFile(target, []byte(`{"strict_mode":false}`), 0600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	applyFileDACLWithAuthUsersWriteACE(t, target)

	_, err := readStateFileInodeAnchoredWithStrictPolicy(target, func() bool { return false })
	if err == nil {
		t.Fatalf("default mode must reject file DACL that grants write access to a non-allowlisted SID")
	}
	if !errors.Is(err, ErrDaclOutsideAllowlist) {
		t.Fatalf("err = %v, want ErrDaclOutsideAllowlist", err)
	}
	got := err.Error()
	for _, want := range []string{"Remediate:", "icacls", target, "/inheritance:r", "/remove:g", `"*S-1-5-11"`, "/grant:r"} {
		if !strings.Contains(got, want) {
			t.Fatalf("write-broadened DACL error missing %q: %v", want, err)
		}
	}
}

func TestReadStateFileInodeAnchored_FileDACLReadBroadenedDefaultRefusesSecretState(t *testing.T) {
	statePathsHelper(t)
	stateDir := hardenedTempDir(t)
	daemonStateRootOverride = stateDir
	resetStrictModeIntentCacheForTest()
	t.Setenv(RequireSingleUserHomeEnv, "")

	dir := hardenedTempDir(t)
	nonSecret := filepath.Join(dir, "supervisor-intent.json")
	if err := os.WriteFile(nonSecret, []byte(`{"strict_mode":false}`), 0600); err != nil {
		t.Fatalf("write non-secret target: %v", err)
	}
	applyFileDACLWithAuthUsersReadACE(t, nonSecret)
	if _, err := readStateFileInodeAnchored(nonSecret); err != nil {
		t.Fatalf("default mode must still relax read-broadened non-secret state file: %v", err)
	}

	secret := filepath.Join(dir, hubMcpTokensFileLeaf)
	if err := os.WriteFile(secret, []byte(`{"tokens":{"claude-code":"`+strings.Repeat("a", 64)+`"}}`), 0600); err != nil {
		t.Fatalf("write secret target: %v", err)
	}
	applyFileDACLWithAuthUsersReadACE(t, secret)
	if _, err := readStateFileInodeAnchored(secret); err == nil {
		t.Fatalf("default mode must refuse read-broadened secret-bearing state file %s", hubMcpTokensFileLeaf)
	} else if !errors.Is(err, ErrDaclOutsideAllowlist) {
		t.Fatalf("secret read-broadened error = %v, want ErrDaclOutsideAllowlist", err)
	} else {
		got := err.Error()
		for _, want := range []string{"Remediate:", "icacls", secret, "/inheritance:r", "/remove:g", `"*S-1-5-11"`, "/grant:r"} {
			if !strings.Contains(got, want) {
				t.Fatalf("secret read-broadened DACL error missing %q: %v", want, err)
			}
		}
	}
}

func TestReadStateFileInodeAnchored_RejectsSymlinkTarget_DefaultAndStrict(t *testing.T) {
	dir := hardenedTempDir(t)
	real := filepath.Join(dir, "real.json")
	if err := os.WriteFile(real, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported on this host: %v", err)
	}

	cases := []struct {
		name   string
		strict bool
	}{
		{name: "default", strict: false},
		{name: "strict", strict: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readStateFileInodeAnchoredWithStrictPolicy(link, func() bool { return tc.strict })
			if err == nil {
				t.Fatalf("readStateFileInodeAnchored must refuse symlink target in %s mode", tc.name)
			}
			if !errors.Is(err, ErrIrregularFile) {
				t.Fatalf("err = %v, want ErrIrregularFile", err)
			}
		})
	}
}

// TestOwnerSIDAllowed pins the owner allowlist contract: the bug-bash
// A1 fix relaxed the strict owner==current-user check so default
// Windows home directories (C:\Users\<user> owned by SYSTEM with the
// user as DACL grantee) pass. The allowlist must accept exactly
// current-user, SYSTEM, and BuiltinAdministrators — anything else
// (Authenticated Users, Domain Users, a random Group Policy SID) is
// rejected. The DACL gate (covered by the existing
// TestReadStateFileInodeAnchoredRejectsAuthenticatedUsersAllow test) is the
// confidentiality boundary; owner is the integrity boundary, and a
// pure unit test is cheaper to maintain than provisioning admin in CI.
func TestOwnerSIDAllowed(t *testing.T) {
	current, err := currentUserSID()
	if err != nil {
		t.Fatalf("currentUserSID: %v", err)
	}
	system, err := windows.StringToSid("S-1-5-18")
	if err != nil {
		t.Fatal(err)
	}
	admins, err := windows.StringToSid("S-1-5-32-544")
	if err != nil {
		t.Fatal(err)
	}
	authUsers, err := windows.StringToSid("S-1-5-11")
	if err != nil {
		t.Fatal(err)
	}
	allowlist := []*windows.SID{current, system, admins}

	cases := []struct {
		name  string
		owner *windows.SID
		want  bool
	}{
		{"current user owns", current, true},
		{"SYSTEM owns (default Windows home dir)", system, true},
		{"BuiltinAdministrators owns", admins, true},
		{"Authenticated Users owns (rejected — corp-policy)", authUsers, false},
		{"nil owner", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ownerSIDAllowed(tc.owner, allowlist)
			if got != tc.want {
				t.Errorf("ownerSIDAllowed(%v) = %v, want %v", sidString(tc.owner), got, tc.want)
			}
		})
	}
}

// TestReadStateFileInodeAnchoredRejectsDirectoryTarget asserts that the
// reader refuses a directory at the state-file path — a defense
// against attacker-controlled directory substitutions on a path that
// should hold a regular file. FILE_FLAG_BACKUP_SEMANTICS in the
// CreateFile call (required to also open parent dirs) would otherwise
// let the directory pass through to the DACL gate.
//
// codex bot r2 P2 closure.
func TestReadStateFileInodeAnchoredRejectsDirectoryTarget(t *testing.T) {
	dir := hardenedTempDir(t)
	// Create a directory at the state-file path. Production callers
	// expect this path to be a regular file.
	target := filepath.Join(dir, "hub-mcp-tokens.json")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("mkdir target as dir: %v", err)
	}
	_, err := readStateFileInodeAnchored(target)
	if err == nil {
		t.Fatalf("readStateFileInodeAnchored must reject directory target; got nil")
	}
	if !errors.Is(err, ErrIrregularFile) {
		t.Errorf("expected ErrIrregularFile for directory; got %v", err)
	}
}
