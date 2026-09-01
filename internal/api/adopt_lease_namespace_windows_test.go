//go:build windows

package api

import (
	"errors"
	"io"
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

func TestBuildAdoptPlanWithLegacyLeaseNamespaceDoesNotMutate(t *testing.T) {
	entry := "graphify-touchstone-dry-run"
	_, _, _ = setupAdoptTestEnv(t, entry, `[mcp_servers.graphify-touchstone-dry-run]
command = "go"
args = ["version"]
`)
	_, namespace, _ := seedRecognizedLegacyLeaseNamespace(t, []string{"existing.lease"}, false)
	beforeSDDL := windowsSDDLForTest(t, namespace)
	beforeNames := readSortedNames(t, namespace)

	if _, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: 9351,
	}); err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if got := windowsSDDLForTest(t, namespace); got != beforeSDDL {
		t.Fatal("dry-run plan changed legacy namespace DACL")
	}
	if got := readSortedNames(t, namespace); !reflect.DeepEqual(got, beforeNames) {
		t.Fatalf("dry-run plan changed legacy namespace entries: before=%v after=%v", beforeNames, got)
	}
}

func TestExecuteAdoptMigratesEligibleLegacyLeaseNamespaceAndRepeats(t *testing.T) {
	entry := "graphify-touchstone"
	_, manifestRoot, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.graphify-touchstone]
command = "go"
args = ["version"]
`)
	_, namespace, _ := seedRecognizedLegacyLeaseNamespace(t, []string{"existing.lease"}, false)

	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: 9352,
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if err := NewAPI().ExecuteAdopt(plan, io.Discard); err != nil {
		t.Fatalf("ExecuteAdopt: %v", err)
	}
	assertWindowsPathDACLAllowlist(t, namespace, true)
	if _, err := os.Stat(filepath.Join(manifestRoot, entry, "manifest.yaml")); err != nil {
		t.Fatalf("adopt did not write manifest: %v", err)
	}
	leasePath, err := adoptManifestLeasePath(entry)
	if err != nil {
		t.Fatalf("adopt lease path: %v", err)
	}
	if _, err := os.Lstat(leasePath); !os.IsNotExist(err) {
		t.Fatalf("adopt left an empty or live lease: %v", err)
	}
	beforeRepeat := readSortedNames(t, namespace)

	repeatPlan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: 9352,
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan repeat: %v", err)
	}
	if err := NewAPI().ExecuteAdopt(repeatPlan, io.Discard); err != nil {
		t.Fatalf("ExecuteAdopt repeat: %v", err)
	}
	if got := readSortedNames(t, namespace); !reflect.DeepEqual(got, beforeRepeat) {
		t.Fatalf("repeat changed lease namespace entries: before=%v after=%v", beforeRepeat, got)
	}
}

func TestExecuteAdoptLegacyLeaseMigrationFailureRollsBackWithoutLease(t *testing.T) {
	entry := "graphify-touchstone-rollback"
	_, manifestRoot, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.graphify-touchstone-rollback]
command = "go"
args = ["version"]
`)
	_, namespace, _ := seedRecognizedLegacyLeaseNamespace(t, []string{"existing.lease"}, false)
	beforeSDDL := windowsSDDLForTest(t, namespace)
	beforeNames := readSortedNames(t, namespace)
	previous := adoptLeaseNamespaceMigrationFailureHook
	called := false
	adoptLeaseNamespaceMigrationFailureHook = func(stage string) error {
		if stage == "leaf-tightened" {
			called = true
			return errors.New("migration-rollback-canary")
		}
		return nil
	}
	t.Cleanup(func() { adoptLeaseNamespaceMigrationFailureHook = previous })

	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: 9353,
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	err = NewAPI().ExecuteAdopt(plan, io.Discard)
	if err == nil {
		t.Fatal("ExecuteAdopt succeeded after injected migration failure")
	}
	if !called {
		t.Fatal("ExecuteAdopt did not invoke the lease namespace migration owner")
	}
	if strings.Contains(err.Error(), "migration-rollback-canary") {
		t.Fatalf("public error leaked migration cause: %v", err)
	}
	if got := windowsSDDLForTest(t, namespace); got != beforeSDDL {
		t.Fatal("failed migration did not restore namespace DACL")
	}
	if got := readSortedNames(t, namespace); !reflect.DeepEqual(got, beforeNames) {
		t.Fatalf("failed migration changed namespace entries: before=%v after=%v", beforeNames, got)
	}
	if _, statErr := os.Stat(filepath.Join(manifestRoot, entry, "manifest.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("failed migration wrote manifest: %v", statErr)
	}
	leasePath, pathErr := adoptManifestLeasePath(entry)
	if pathErr != nil {
		t.Fatalf("adopt lease path: %v", pathErr)
	}
	if _, statErr := os.Lstat(leasePath); !os.IsNotExist(statErr) {
		t.Fatalf("failed migration created a lease: %v", statErr)
	}
}

func TestExecuteAdoptDoesNotMigrateWhenStateRootIsRefused(t *testing.T) {
	entry := "graphify-touchstone-refused-root"
	_, manifestRoot, stateRoot := setupAdoptTestEnv(t, entry, `[mcp_servers.graphify-touchstone-refused-root]
command = "go"
args = ["version"]
`)
	applyFileDACLWithAuthUsersReadACE(t, stateRoot)
	previous := adoptLeaseNamespaceMigrationFailureHook
	called := false
	adoptLeaseNamespaceMigrationFailureHook = func(string) error {
		called = true
		return errors.New("migration-must-not-run")
	}
	t.Cleanup(func() { adoptLeaseNamespaceMigrationFailureHook = previous })

	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: 9354,
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	err = NewAPI().ExecuteAdopt(plan, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "reason=state-root-refused action=leave-unchanged") {
		t.Fatalf("ExecuteAdopt error=%v, want state-root refusal", err)
	}
	if called {
		t.Fatal("state-root refusal must not invoke child namespace migration")
	}
	if _, statErr := os.Stat(filepath.Join(manifestRoot, entry, "manifest.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("state-root refusal wrote manifest: %v", statErr)
	}
	leasePath, pathErr := adoptManifestLeasePath(entry)
	if pathErr != nil {
		t.Fatalf("adopt lease path: %v", pathErr)
	}
	if _, statErr := os.Lstat(leasePath); !os.IsNotExist(statErr) {
		t.Fatalf("state-root refusal created a lease: %v", statErr)
	}
}

func TestAdoptLeaseNamespaceLegacyStateRootMigrationDryRunApplyAndLease(t *testing.T) {
	stateRoot, namespace, lease := seedRecognizedLegacyStateRoot(t, []string{"existing.lease"})
	beforeRoot := windowsSDDLForTest(t, stateRoot)
	beforeNamespace := windowsSDDLForTest(t, namespace)

	report, err := MigrateLegacyAdoptLeaseNamespace(AdoptLeaseNamespaceMigrationOpts{})
	if err != nil || report.State != AdoptLeaseNamespaceLegacy || !report.MigrationEligible || report.ReasonID != AdoptLeaseReasonStateRootLegacyDACL || report.Action != AdoptLeaseActionMigrateLegacyStateRoot {
		t.Fatalf("root migration dry-run report=%+v err=%v", report, err)
	}
	if got := windowsSDDLForTest(t, stateRoot); got != beforeRoot {
		t.Fatal("root migration dry-run changed state-root DACL")
	}
	if got := windowsSDDLForTest(t, namespace); got != beforeNamespace {
		t.Fatal("root migration dry-run changed namespace DACL")
	}

	report, err = MigrateLegacyAdoptLeaseNamespace(AdoptLeaseNamespaceMigrationOpts{Yes: true})
	if err != nil || report.State != AdoptLeaseNamespaceReady || report.Action != AdoptLeaseActionRetryAdopt || !report.NamespaceChanged {
		t.Fatalf("root migration apply report=%+v err=%v", report, err)
	}
	assertWindowsPathDACLAllowlist(t, stateRoot, true)
	assertWindowsPathDACLAllowlist(t, namespace, true)
	assertWindowsPathDACLAllowlist(t, lease, false)
	acquired, ok, err := tryAcquireAdoptManifestLease("root-migrated")
	if err != nil || !ok {
		t.Fatalf("lease acquire after root migration: lease=%v acquired=%v err=%v", acquired != nil, ok, err)
	}
	if err := acquired.ReleaseAndRemove(); err != nil {
		t.Fatalf("settle lease after root migration: %v", err)
	}
}

func TestAdoptLeaseNamespaceLegacyStateRootMigrationRollsBackRootAndChild(t *testing.T) {
	stateRoot, namespace, _ := seedRecognizedLegacyStateRoot(t, []string{"existing.lease"})
	beforeRoot := windowsSDDLForTest(t, stateRoot)
	beforeNamespace := windowsSDDLForTest(t, namespace)
	previous := adoptLeaseNamespaceMigrationFailureHook
	adoptLeaseNamespaceMigrationFailureHook = func(stage string) error {
		if stage == "root-tightened" {
			return errors.New("root-rollback-canary")
		}
		return nil
	}
	t.Cleanup(func() { adoptLeaseNamespaceMigrationFailureHook = previous })

	report, err := MigrateLegacyAdoptLeaseNamespace(AdoptLeaseNamespaceMigrationOpts{Yes: true})
	if err == nil || !report.RollbackPerformed {
		t.Fatalf("root migration rollback report=%+v err=%v", report, err)
	}
	if strings.Contains(err.Error(), "root-rollback-canary") {
		t.Fatalf("public root migration error leaked cause: %v", err)
	}
	if got := windowsSDDLForTest(t, stateRoot); got != beforeRoot {
		t.Fatal("root migration rollback did not restore state-root DACL")
	}
	if got := windowsSDDLForTest(t, namespace); got != beforeNamespace {
		t.Fatal("root migration rollback did not restore namespace DACL")
	}
	leasePath, pathErr := adoptManifestLeasePath("root-rollback")
	if pathErr != nil {
		t.Fatalf("derive root rollback lease path: %v", pathErr)
	}
	if _, statErr := os.Lstat(leasePath); !os.IsNotExist(statErr) {
		t.Fatalf("root migration rollback created a lease: %v", statErr)
	}
}

func TestExecuteAdoptMigratesEligibleLegacyStateRoot(t *testing.T) {
	entry := "graphify-touchstone-legacy-root"
	_, manifestRoot, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.graphify-touchstone-legacy-root]
command = "go"
args = ["version"]
`)
	stateRoot, namespace, _ := seedRecognizedLegacyStateRoot(t, []string{"existing.lease"})
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: 9355,
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if err := NewAPI().ExecuteAdopt(plan, io.Discard); err != nil {
		t.Fatalf("ExecuteAdopt: %v", err)
	}
	assertWindowsPathDACLAllowlist(t, stateRoot, true)
	assertWindowsPathDACLAllowlist(t, namespace, true)
	if _, statErr := os.Stat(filepath.Join(manifestRoot, entry, "manifest.yaml")); statErr != nil {
		t.Fatalf("root migration apply did not write manifest: %v", statErr)
	}
	leasePath, pathErr := adoptManifestLeasePath(entry)
	if pathErr != nil {
		t.Fatalf("derive adopted lease path: %v", pathErr)
	}
	if _, statErr := os.Lstat(leasePath); !os.IsNotExist(statErr) {
		t.Fatalf("root migration apply left a lease: %v", statErr)
	}
}

func TestAdoptLeaseNamespaceRootMigrationRefusesHostileRootsWithoutMutation(t *testing.T) {
	t.Run("explicit-broad", func(t *testing.T) {
		stateRoot, namespace, _ := seedRecognizedLegacyStateRoot(t, []string{"existing.lease"})
		nsHandle := openWindowsPathForNamespaceTest(t, namespace, windows.READ_CONTROL|windows.WRITE_DAC, true)
		if err := setRestrictiveDACL(nsHandle); err != nil {
			_ = windows.CloseHandle(nsHandle)
			t.Fatal(err)
		}
		_ = windows.CloseHandle(nsHandle)
		beforeNamespace := windowsSDDLForTest(t, namespace)
		t.Cleanup(func() {
			h := openWindowsPathForNamespaceTest(t, stateRoot, windows.READ_CONTROL|windows.WRITE_DAC, true)
			defer windows.CloseHandle(h)
			if err := setRestrictiveDACL(h); err != nil {
				t.Errorf("restore root fixture DACL: %v", err)
			}
		})
		applyFileDACLWithAuthUsersReadACE(t, stateRoot)
		beforeRoot := windowsSDDLForTest(t, stateRoot)
		report, err := MigrateLegacyAdoptLeaseNamespace(AdoptLeaseNamespaceMigrationOpts{Yes: true})
		if err == nil || report.ReasonID != AdoptLeaseReasonStateRootRefused || report.Action != AdoptLeaseActionLeaveUnchanged {
			t.Fatalf("explicit broad root report=%+v err=%v", report, err)
		}
		if got := windowsSDDLForTest(t, stateRoot); got != beforeRoot {
			t.Fatal("explicit broad root refusal changed state root")
		}
		if got := windowsSDDLForTest(t, namespace); got != beforeNamespace {
			t.Fatal("explicit broad root refusal changed namespace")
		}
	})

	t.Run("reparse", func(t *testing.T) {
		statePathsHelper(t)
		parent := hardenedTempDir(t)
		foreign := hardenedTempDir(t)
		stateRoot := filepath.Join(parent, "state-root-link")
		if err := createJunctionForTest(stateRoot, foreign); err != nil {
			t.Skipf("junction creation unavailable; root reparse falsifier remains unrun: %v", err)
		}
		daemonStateRootOverride = stateRoot
		before := readSortedNames(t, foreign)
		report, err := MigrateLegacyAdoptLeaseNamespace(AdoptLeaseNamespaceMigrationOpts{Yes: true})
		if err == nil || report.ReasonID != AdoptLeaseReasonStateRootRefused || report.Action != AdoptLeaseActionLeaveUnchanged {
			t.Fatalf("reparse root report=%+v err=%v", report, err)
		}
		if got := readSortedNames(t, foreign); !reflect.DeepEqual(got, before) {
			t.Fatalf("reparse root refusal changed foreign target: before=%v after=%v", before, got)
		}
	})
}

func TestClassifyWindowsInheritedLegacyDACLRejectsSyntheticWrongOwnerBeforeMutation(t *testing.T) {
	stateRoot, _, _ := seedRecognizedLegacyStateRoot(t, []string{"existing.lease"})
	before := windowsSDDLForTest(t, stateRoot)
	h := openWindowsPathForNamespaceTest(t, stateRoot, windows.READ_CONTROL, true)
	defer windows.CloseHandle(h)
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("fixture DACL unavailable: dacl=%v err=%v", dacl != nil, err)
	}
	current, system, admin, err := allowlistSIDs()
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := windows.StringToSid("S-1-5-11")
	if err != nil {
		t.Fatal(err)
	}
	allowlist := []*windows.SID{current, system, admin}
	if eligible, err := classifyWindowsInheritedLegacyDACL(current, dacl, allowlist); err != nil || !eligible {
		t.Fatalf("recognized fixture was not classified as legacy: eligible=%v err=%v", eligible, err)
	}
	eligible, err := classifyWindowsInheritedLegacyDACL(foreign, dacl, allowlist)
	if err != nil || eligible {
		t.Fatalf("synthetic wrong owner accepted: eligible=%v err=%v", eligible, err)
	}
	if got := windowsSDDLForTest(t, stateRoot); got != before {
		t.Fatal("wrong-owner classification mutated state root")
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

// seedRecognizedLegacyStateRoot makes the exact inherited-DACL legacy shape:
// the state root is a child of a current-user-owned parent whose otherwise
// allowlisted DACL contributes one inherited Authenticated Users ACE.
func seedRecognizedLegacyStateRoot(t *testing.T, leaves []string) (string, string, string) {
	t.Helper()
	statePathsHelper(t)
	parent := filepath.Join(t.TempDir(), "legacy-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	current, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	authUsers, err := windows.StringToSid("S-1-5-11")
	if err != nil {
		t.Fatal(err)
	}
	entries := []windows.EXPLICIT_ACCESS{
		{AccessPermissions: windows.GENERIC_ALL, AccessMode: windows.GRANT_ACCESS, Inheritance: windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER, TrusteeValue: windows.TrusteeValueFromSID(current)}},
		{AccessPermissions: windows.GENERIC_READ | windows.GENERIC_WRITE, AccessMode: windows.GRANT_ACCESS, Inheritance: windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_WELL_KNOWN_GROUP, TrusteeValue: windows.TrusteeValueFromSID(authUsers)}},
	}
	applyProtectedDACLFromEntries(t, parent, entries)
	stateRoot := filepath.Join(parent, "state")
	if err := os.Mkdir(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	daemonStateRootOverride = stateRoot
	root := openDirHandleNoReparseForTest(t, stateRoot)
	legacy, err := windowsDACLIsRecognizedInheritedLegacy(root)
	_ = windows.CloseHandle(root)
	if err != nil || !legacy {
		t.Fatalf("fixture state root must be recognized inherited legacy: legacy=%v err=%v", legacy, err)
	}
	namespace := filepath.Join(stateRoot, adoptProvenanceSnapshotSubdir)
	if err := os.Mkdir(namespace, 0o700); err != nil {
		t.Fatal(err)
	}
	var firstLease string
	for _, leaf := range leaves {
		path := filepath.Join(namespace, leaf)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if firstLease == "" {
			firstLease = path
		}
	}
	return stateRoot, namespace, firstLease
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
