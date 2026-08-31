//go:build windows

package api

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestAdoptLeaseNamespaceFailureProjectionContainsOnlyStableIDs(t *testing.T) {
	canary := errors.New(`private-path-canary foreign-sid-canary secret-canary`)
	err := newLeaseNamespaceFailure(AdoptLeaseReasonNamespaceLegacyDACL, AdoptLeaseActionMigrateLegacy, canary)
	staged := newAdoptStageError("lease-acquire", "uncommitted", err)
	got := staged.Error()
	if got != "E_ADOPT_LEASE_NAMESPACE_REFUSED reason=namespace-legacy-dacl action=migrate-legacy-lease-namespace" {
		t.Fatalf("public projection=%q", got)
	}
	for _, forbidden := range []string{"private-path-canary", "foreign-sid-canary", "secret-canary"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("public projection leaked %q: %q", forbidden, got)
		}
	}
	var failure *LeaseFailure
	if !errors.As(err, &failure) || !errors.Is(err, canary) {
		t.Fatal("protected in-process cause chain was not retained")
	}
}

func TestAdoptLeaseNamespaceFailureEventContainsOnlyStableProjection(t *testing.T) {
	stateDir := isolateStateDir(t)
	canary := errors.New(`private-path-canary foreign-sid-canary secret-canary`)
	emitAdoptLeaseFailed("graphify", newLeaseNamespaceFailure(AdoptLeaseReasonNamespaceLegacyDACL, AdoptLeaseActionMigrateLegacy, canary))
	raw, err := os.ReadFile(filepath.Join(stateDir, SupervisorEventLogFileLeaf))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{`"failure_id":"E_ADOPT_LEASE_NAMESPACE_REFUSED"`, `"reason_id":"namespace-legacy-dacl"`, `"action":"migrate-legacy-lease-namespace"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %s in %s", want, text)
		}
	}
	for _, forbidden := range []string{"private-path-canary", "foreign-sid-canary", "secret-canary"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("event leaked %q", forbidden)
		}
	}
}

func TestAdoptLeaseAcquireClassifiesRecognizedLegacyNamespaceWithoutMutation(t *testing.T) {
	_, namespace, _ := seedRecognizedLegacyLeaseNamespace(t, []string{"existing.lease"}, false)
	before := readSortedNames(t, namespace)
	lease, acquired, err := tryAcquireAdoptManifestLease("graphify")
	if lease != nil {
		t.Cleanup(func() { _ = lease.Unlock() })
	}
	var failure *LeaseFailure
	if acquired || !errors.As(err, &failure) || failure.FailureID != adoptLeaseFailureNamespaceRefused ||
		failure.ReasonID != AdoptLeaseReasonNamespaceLegacyDACL || failure.Action != AdoptLeaseActionMigrateLegacy {
		t.Fatalf("acquire=(lease=%v acquired=%v err=%v failure=%+v)", lease != nil, acquired, err, failure)
	}
	if got := readSortedNames(t, namespace); !reflect.DeepEqual(got, before) {
		t.Fatalf("failed acquire mutated namespace: before=%v after=%v", before, got)
	}
}

func TestAdoptLeaseNamespaceLegacyMigrationDryRunAndApply(t *testing.T) {
	stateDir, namespace, lease := seedRecognizedLegacyLeaseNamespace(t, []string{"graphify.lease"}, true)
	beforeNames := readSortedNames(t, namespace)
	beforeLeaseID := windowsFileIdentityForTest(t, lease)
	snapshot := filepath.Join(namespace, "existing-snapshot")
	beforeSnapshotSDDL := windowsSDDLForTest(t, snapshot)
	beforeNamespaceSDDL := windowsSDDLForTest(t, namespace)

	report, err := MigrateLegacyAdoptLeaseNamespace(AdoptLeaseNamespaceMigrationOpts{})
	if err != nil || report.State != AdoptLeaseNamespaceLegacy || !report.MigrationEligible || report.LeaseLeafCount != 1 || report.SnapshotDirCount != 1 {
		t.Fatalf("dry-run report=%+v err=%v", report, err)
	}
	if got := windowsSDDLForTest(t, namespace); got != beforeNamespaceSDDL {
		t.Fatal("dry-run changed namespace security descriptor")
	}

	report, err = MigrateLegacyAdoptLeaseNamespace(AdoptLeaseNamespaceMigrationOpts{Yes: true})
	if err != nil {
		t.Fatalf("apply migration: report=%+v err=%v", report, err)
	}
	if report.State != AdoptLeaseNamespaceReady || report.ChangedLeafCount != 1 || !report.NamespaceChanged || report.RollbackPerformed {
		t.Fatalf("apply report=%+v", report)
	}
	assertWindowsPathDACLAllowlist(t, namespace, true)
	assertWindowsPathDACLAllowlist(t, lease, false)
	if got := windowsSDDLForTest(t, snapshot); got != beforeSnapshotSDDL {
		t.Fatal("safe snapshot sibling DACL changed")
	}
	if got := readSortedNames(t, namespace); !reflect.DeepEqual(got, beforeNames) {
		t.Fatalf("entry names changed: before=%v after=%v", beforeNames, got)
	}
	if got := windowsFileIdentityForTest(t, lease); !sameWindowsAdoptLeaseIdentity(beforeLeaseID, got) {
		t.Fatal("lease identity changed")
	}
	if info, err := os.Stat(lease); err != nil || info.Size() != 0 {
		t.Fatalf("lease content/size changed: info=%v err=%v", info, err)
	}
	_ = stateDir
}

func TestAdoptLeaseNamespaceLegacyMigrationRollbackRestoresAllDACLs(t *testing.T) {
	_, namespace, _ := seedRecognizedLegacyLeaseNamespace(t, []string{"one.lease", "two.lease"}, false)
	paths := []string{namespace, filepath.Join(namespace, "one.lease"), filepath.Join(namespace, "two.lease")}
	before := make(map[string]string, len(paths))
	for _, path := range paths {
		before[path] = windowsSDDLForTest(t, path)
	}
	previous := adoptLeaseNamespaceMigrationFailureHook
	adoptLeaseNamespaceMigrationFailureHook = func(stage string) error {
		if stage == "leaf-tightened" {
			return errors.New("rollback-canary")
		}
		return nil
	}
	t.Cleanup(func() { adoptLeaseNamespaceMigrationFailureHook = previous })

	report, err := MigrateLegacyAdoptLeaseNamespace(AdoptLeaseNamespaceMigrationOpts{Yes: true})
	if err == nil || !report.RollbackPerformed {
		t.Fatalf("rollback result=%+v err=%v", report, err)
	}
	if strings.Contains(err.Error(), "rollback-canary") {
		t.Fatalf("public migration error leaked raw cause: %v", err)
	}
	for _, path := range paths {
		if got := windowsSDDLForTest(t, path); got != before[path] {
			t.Fatalf("rollback did not restore DACL for %s", filepath.Base(path))
		}
	}
}

func TestAdoptLeaseNamespaceMigrationRefusesHostileShapesWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, namespace string)
	}{
		{"unknown-sibling", func(t *testing.T, namespace string) {
			if err := os.WriteFile(filepath.Join(namespace, "foreign.bin"), []byte("canary"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"nonzero-lease", func(t *testing.T, namespace string) {
			if err := os.WriteFile(filepath.Join(namespace, "bad.lease"), []byte("canary"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"broad-snapshot-sibling", func(t *testing.T, namespace string) {
			if err := os.Mkdir(filepath.Join(namespace, "broad-snapshot"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, namespace, _ := seedRecognizedLegacyLeaseNamespace(t, nil, false)
			tc.seed(t, namespace)
			beforeNames := readSortedNames(t, namespace)
			beforeSDDL := windowsSDDLForTest(t, namespace)
			report, err := MigrateLegacyAdoptLeaseNamespace(AdoptLeaseNamespaceMigrationOpts{Yes: true})
			if err == nil || report.State != AdoptLeaseNamespaceRefused {
				t.Fatalf("hostile migration report=%+v err=%v", report, err)
			}
			if got := readSortedNames(t, namespace); !reflect.DeepEqual(got, beforeNames) {
				t.Fatalf("refusal changed names: before=%v after=%v", beforeNames, got)
			}
			if got := windowsSDDLForTest(t, namespace); got != beforeSDDL {
				t.Fatal("refusal changed namespace DACL")
			}
		})
	}
}

func TestAdoptLeaseNamespaceMigrationRefusesExplicitBroadDACLAndBusyNamespace(t *testing.T) {
	t.Run("explicit-broad-unrecognized", func(t *testing.T) {
		statePathsHelper(t)
		stateDir := hardenedTempDir(t)
		daemonStateRootOverride = stateDir
		namespace := filepath.Join(stateDir, adoptProvenanceSnapshotSubdir)
		if err := os.Mkdir(namespace, 0o700); err != nil {
			t.Fatal(err)
		}
		applyFileDACLWithAuthUsersReadACE(t, namespace)
		before := windowsSDDLForTest(t, namespace)
		report, err := MigrateLegacyAdoptLeaseNamespace(AdoptLeaseNamespaceMigrationOpts{Yes: true})
		if err == nil || report.State != AdoptLeaseNamespaceRefused || report.ReasonID != AdoptLeaseReasonNamespaceDACLRefused {
			t.Fatalf("explicit broad report=%+v err=%v", report, err)
		}
		if got := windowsSDDLForTest(t, namespace); got != before {
			t.Fatal("explicit broad refusal changed DACL")
		}
	})

	t.Run("busy", func(t *testing.T) {
		_, namespace, _ := seedRecognizedLegacyLeaseNamespace(t, []string{"busy.lease"}, false)
		held := openDirHandleNoReparseForTest(t, namespace)
		defer windows.CloseHandle(held)
		before := windowsSDDLForTest(t, namespace)
		report, err := MigrateLegacyAdoptLeaseNamespace(AdoptLeaseNamespaceMigrationOpts{Yes: true})
		if err == nil || report.State != AdoptLeaseNamespaceRefused || report.ReasonID != AdoptLeaseReasonNamespaceBusy {
			t.Fatalf("busy report=%+v err=%v", report, err)
		}
		if got := windowsSDDLForTest(t, namespace); got != before {
			t.Fatal("busy refusal changed DACL")
		}
	})
}

func seedRecognizedLegacyLeaseNamespace(t *testing.T, leaves []string, safeSnapshot bool) (string, string, string) {
	statePathsHelper(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatalf("create state root: %v", err)
	}
	daemonStateRootOverride = stateDir
	current, err := currentUserSID()
	if err != nil {
		t.Fatalf("current user SID: %v", err)
	}
	authUsers, err := windows.StringToSid("S-1-5-11")
	if err != nil {
		t.Fatalf("authenticated users SID: %v", err)
	}
	entries := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(current)},
		},
		{
			AccessPermissions: windows.GENERIC_READ | windows.GENERIC_WRITE,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT | windows.INHERIT_ONLY,
			Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(authUsers)},
		},
	}
	applyProtectedDACLFromEntries(t, stateDir, entries)
	root := openDirHandleNoReparseForTest(t, stateDir)
	if err := verifyWindowsDACLFromHandle(root); err != nil {
		_ = windows.CloseHandle(root)
		t.Fatalf("fixture state root must remain strict: %v", err)
	}
	_ = windows.CloseHandle(root)

	namespace := filepath.Join(stateDir, adoptProvenanceSnapshotSubdir)
	if err := os.Mkdir(namespace, 0o700); err != nil {
		t.Fatalf("create inherited namespace: %v", err)
	}
	var firstLease string
	for _, leaf := range leaves {
		path := filepath.Join(namespace, leaf)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("create inherited lease %s: %v", leaf, err)
		}
		if firstLease == "" {
			firstLease = path
		}
	}
	if safeSnapshot {
		snapshot := filepath.Join(namespace, "existing-snapshot")
		if err := os.Mkdir(snapshot, 0o700); err != nil {
			t.Fatalf("create snapshot sibling: %v", err)
		}
		h := openWindowsPathForNamespaceTest(t, snapshot, windows.READ_CONTROL|windows.WRITE_DAC, true)
		if err := setRestrictiveDACL(h); err != nil {
			_ = windows.CloseHandle(h)
			t.Fatal(err)
		}
		_ = windows.CloseHandle(h)
	}
	return stateDir, namespace, firstLease
}

func openDirHandleNoReparseForTest(t *testing.T, path string) windows.Handle {
	t.Helper()
	h, err := openDirHandleNoReparse(path)
	if err != nil {
		t.Fatalf("open dir handle %s: %v", filepath.Base(path), err)
	}
	return h
}

func windowsSDDLForTest(t *testing.T, path string) string {
	t.Helper()
	h := openWindowsPathForNamespaceTest(t, path, windows.READ_CONTROL, true)
	defer windows.CloseHandle(h)
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd.String()
}

func windowsFileIdentityForTest(t *testing.T, path string) windows.ByHandleFileInformation {
	t.Helper()
	h := openWindowsPathForNamespaceTest(t, path, windows.FILE_READ_ATTRIBUTES, false)
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		t.Fatal(err)
	}
	return info
}

func assertWindowsPathDACLAllowlist(t *testing.T, path string, directory bool) {
	t.Helper()
	h := openWindowsPathForNamespaceTest(t, path, windows.READ_CONTROL, directory)
	defer windows.CloseHandle(h)
	if err := verifyWindowsDACLFromHandle(h); err != nil {
		t.Fatalf("DACL not restrictive for %s: %v", filepath.Base(path), err)
	}
}

func openWindowsPathForNamespaceTest(t *testing.T, path string, access uint32, directory bool) windows.Handle {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	h, err := windows.CreateFile(p, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func readSortedNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
