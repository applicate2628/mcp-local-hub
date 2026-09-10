//go:build windows

package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// TestAdoptLeaseGuardOwnsAcquireAndSettle makes the guard-to-slot ordering
// observable: the owner must not open the manifest leaf until its exact
// namespace guard has been locked. Returning from the hook also proves that a
// pre-slot failure cannot hand out a half-acquired lease.
func TestAdoptLeaseGuardOwnsAcquireAndSettle(t *testing.T) {
	stateDir := isolateStateDir(t)
	bootstrapSecureAdoptLeaseNamespace(t)
	const manifest = "guard-before-slot"
	leasePath, err := adoptManifestLeasePath(manifest)
	if err != nil {
		t.Fatalf("derive lease path: %v", err)
	}
	previous := adoptLeaseBeforeSlotOpenHook
	called := false
	adoptLeaseBeforeSlotOpenHook = func() error {
		called = true
		if _, err := os.Lstat(leasePath); !os.IsNotExist(err) {
			return errors.New("manifest slot opened before namespace guard")
		}
		return errors.New("injected guard-held pre-slot stop")
	}
	t.Cleanup(func() { adoptLeaseBeforeSlotOpenHook = previous })
	lease, acquired, err := tryAcquireAdoptManifestLease(manifest)
	if lease != nil {
		t.Cleanup(func() { _ = lease.Unlock() })
	}
	if !called || acquired || err == nil {
		t.Fatalf("guard-before-slot acquisition = lease:%v acquired:%v err:%v; want hook failure before any slot handoff", lease != nil, acquired, err)
	}
	if _, err := os.Lstat(leasePath); !os.IsNotExist(err) {
		t.Fatalf("pre-slot failure left a manifest lease leaf: %v", err)
	}
	_ = stateDir // keeps the state-root assertion explicit in this platform test.
}

// TestAdoptLeaseNamespaceWindowsRejectsJunction verifies the normalised root
// assertion: the retained state-root handle, not a path re-walk, is the only
// authority to create or open adopt-provenance. A junction must never receive a
// lease leaf in its foreign target.
func TestAdoptLeaseNamespaceWindowsRejectsJunction(t *testing.T) {
	statePathsHelper(t)
	stateDir := hardenedTempDir(t)
	daemonStateRootOverride = stateDir
	foreign := hardenedTempDir(t)
	namespace := filepath.Join(stateDir, adoptProvenanceSnapshotSubdir)
	if err := createJunctionForTest(namespace, foreign); err != nil {
		t.Skipf("junction creation unavailable; live reparse falsifier remains unrun: %v", err)
	}

	lease, acquired, err := tryAcquireAdoptManifestLease("junction-namespace")
	if lease != nil {
		t.Cleanup(func() { _ = lease.Unlock() })
	}
	if err == nil || acquired {
		t.Fatalf("junction namespace accepted: acquired=%v err=%v", acquired, err)
	}
	if _, statErr := os.Stat(filepath.Join(foreign, "junction-namespace"+adoptManifestLeaseSuffix)); !os.IsNotExist(statErr) {
		t.Fatalf("junction target gained a lease leaf: %v", statErr)
	}
}

// TestAdoptLeaseLeafWindowsRejectsDirectoryAndBroadDACLBeforeLock covers two
// hostile final-slot forms. The canary leaf must remain in place; acquisition
// is not allowed to repair, lock, or delete it.
func TestAdoptLeaseLeafWindowsRejectsDirectoryAndBroadDACLBeforeLock(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, path string)
	}{
		{
			name: "directory",
			seed: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatalf("seed directory leaf: %v", err)
				}
			},
		},
		{
			name: "broad-dacl",
			seed: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("foreign-lease-canary"), 0o600); err != nil {
					t.Fatalf("seed broad-DACL leaf: %v", err)
				}
				applyFileDACLWithAuthUsersReadACE(t, path)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := isolateStateDir(t)
			bootstrapSecureAdoptLeaseNamespace(t)
			entry := "hostile-leaf-" + tc.name
			leasePath, err := adoptManifestLeasePath(entry)
			if err != nil {
				t.Fatalf("derive leaf path: %v", err)
			}
			tc.seed(t, leasePath)

			lease, acquired, err := tryAcquireAdoptManifestLease(entry)
			if lease != nil {
				t.Cleanup(func() { _ = lease.Unlock() })
			}
			if err == nil || acquired {
				t.Fatalf("hostile %s leaf accepted: acquired=%v err=%v", tc.name, acquired, err)
			}
			if _, statErr := os.Lstat(leasePath); statErr != nil {
				t.Fatalf("rejected hostile %s leaf was removed: %v", tc.name, statErr)
			}
			if tc.name == "broad-dacl" {
				if got, readErr := os.ReadFile(leasePath); readErr != nil || string(got) != "foreign-lease-canary" {
					t.Fatalf("rejected broad-DACL leaf changed: bytes=%q err=%v", got, readErr)
				}
			}
			_ = stateDir
		})
	}
}

// TestAdoptLeaseLeafWindowsRejectsJunctionBeforeLock is the final-component
// reparse falsifier. The exact leaf name must not be followed into a foreign
// directory, and the junction itself must survive the refusal unchanged.
func TestAdoptLeaseLeafWindowsRejectsJunctionBeforeLock(t *testing.T) {
	_ = isolateStateDir(t)
	bootstrapSecureAdoptLeaseNamespace(t)
	entry := "junction-leaf"
	leasePath, err := adoptManifestLeasePath(entry)
	if err != nil {
		t.Fatalf("derive leaf path: %v", err)
	}
	foreign := hardenedTempDir(t)
	if err := createJunctionForTest(leasePath, foreign); err != nil {
		t.Skipf("junction creation unavailable; final-leaf reparse falsifier remains unrun: %v", err)
	}

	lease, acquired, err := tryAcquireAdoptManifestLease(entry)
	if lease != nil {
		t.Cleanup(func() { _ = lease.Unlock() })
	}
	if err == nil || acquired {
		t.Fatalf("junction leaf accepted: acquired=%v err=%v", acquired, err)
	}
	if info, statErr := os.Lstat(leasePath); statErr != nil {
		t.Fatalf("rejected junction leaf changed or vanished: info=%v err=%v", info, statErr)
	}
	if entries, readErr := os.ReadDir(foreign); readErr != nil || len(entries) != 0 {
		t.Fatalf("junction target changed by refusal: entries=%v err=%v", entries, readErr)
	}
}

// TestAdoptLeaseLeafWindowsRejectsWrongOwnerNilAndCallbackDACLBeforeLock
// drives the three owner/DACL cases that cannot be inferred from a basic broad
// ALLOW ACE: foreign owner, NULL DACL (allow-all), and callback ALLOW ACE.
// Hosts that deny owner mutation or callback-ACE installation report a skip so
// the release gate can retain those as explicit live-privilege probes.
func TestAdoptLeaseLeafWindowsRejectsWrongOwnerNilAndCallbackDACLBeforeLock(t *testing.T) {
	for _, kind := range []string{"wrong-owner", "nil-dacl", "callback-allow"} {
		t.Run(kind, func(t *testing.T) {
			_ = isolateStateDir(t)
			bootstrapSecureAdoptLeaseNamespace(t)
			entry := "hostile-acl-" + kind
			leaf, err := adoptManifestLeasePath(entry)
			if err != nil {
				t.Fatalf("derive leaf: %v", err)
			}
			if err := os.WriteFile(leaf, []byte("foreign-acl-canary"), 0o600); err != nil {
				t.Fatalf("seed leaf: %v", err)
			}
			h := openWindowsFileForDACLTest(t, leaf, windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER)
			switch kind {
			case "wrong-owner":
				foreign, sidErr := windows.StringToSid("S-1-5-11")
				if sidErr != nil {
					t.Fatalf("Authenticated Users SID: %v", sidErr)
				}
				if setErr := windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, foreign, nil, nil, nil); setErr != nil {
					_ = windows.CloseHandle(h)
					t.Skipf("foreign-owner fixture needs unavailable privilege: %v", setErr)
				}
			case "nil-dacl":
				if setErr := windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, nil, nil); setErr != nil {
					_ = windows.CloseHandle(h)
					t.Skipf("NULL-DACL fixture unavailable: %v", setErr)
				}
			case "callback-allow":
				current, sidErr := currentUserSID()
				if sidErr != nil {
					_ = windows.CloseHandle(h)
					t.Fatalf("current SID: %v", sidErr)
				}
				sd, parseErr := windows.SecurityDescriptorFromString("D:(A;;GA;;;" + current.String() + ")(XA;;GR;;;AU;(TRUE))")
				if parseErr != nil {
					_ = windows.CloseHandle(h)
					t.Skipf("callback-ACE fixture unsupported by this Windows policy/runtime: %v", parseErr)
				}
				dacl, _, daclErr := sd.DACL()
				if daclErr != nil {
					_ = windows.CloseHandle(h)
					t.Fatalf("callback DACL: %v", daclErr)
				}
				if setErr := windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); setErr != nil {
					_ = windows.CloseHandle(h)
					t.Skipf("callback-ACE installation unavailable: %v", setErr)
				}
			}
			if err := windows.CloseHandle(h); err != nil {
				t.Fatalf("close hostile fixture handle: %v", err)
			}

			lease, acquired, err := tryAcquireAdoptManifestLease(entry)
			if lease != nil {
				t.Cleanup(func() { _ = lease.Unlock() })
			}
			if err == nil || acquired {
				t.Fatalf("hostile %s leaf accepted: acquired=%v err=%v", kind, acquired, err)
			}
			if got, readErr := os.ReadFile(leaf); readErr != nil || string(got) != "foreign-acl-canary" {
				t.Fatalf("rejected %s leaf changed: bytes=%q err=%v", kind, got, readErr)
			}
		})
	}
}

// TestAdoptLeaseWindowsSyntheticWrongOwnerPolicy is the unprivileged companion
// to the live foreign-owner fixture. Windows may refuse assigning an arbitrary
// SID as an owner without SeRestorePrivilege, but the predicate used by the
// same handle-DACL verifier is deterministic: only the current user, SYSTEM,
// and Builtin Administrators can own a lease namespace or leaf. The callback
// ALLOW branch above is a live constructed-DACL test and does not require that
// privilege.
func TestAdoptLeaseWindowsSyntheticWrongOwnerPolicy(t *testing.T) {
	current, err := currentUserSID()
	if err != nil {
		t.Fatalf("current SID: %v", err)
	}
	system, err := windows.StringToSid("S-1-5-18")
	if err != nil {
		t.Fatalf("SYSTEM SID: %v", err)
	}
	admins, err := windows.StringToSid("S-1-5-32-544")
	if err != nil {
		t.Fatalf("Builtin Administrators SID: %v", err)
	}
	foreign, err := windows.StringToSid("S-1-5-11")
	if err != nil {
		t.Fatalf("Authenticated Users SID: %v", err)
	}
	allowlist := []*windows.SID{current, system, admins}
	if ownerSIDAllowed(foreign, allowlist) {
		t.Fatal("synthetic foreign owner passed the lease-owner allowlist")
	}
	for _, allowed := range allowlist {
		if !ownerSIDAllowed(allowed, allowlist) {
			t.Fatalf("allowlisted lease owner %s was rejected", allowed.String())
		}
	}
}

// TestAdoptLeaseNamespaceWindowsRejectsBroadDACL keeps a hostile pre-existing
// namespace as an explicit refusal case. The owner must not repair it, descend
// into it, or create a manifest leaf through it.
func TestAdoptLeaseNamespaceWindowsRejectsBroadDACL(t *testing.T) {
	statePathsHelper(t)
	stateDir := hardenedTempDir(t)
	daemonStateRootOverride = stateDir
	namespace := filepath.Join(stateDir, adoptProvenanceSnapshotSubdir)
	if err := os.Mkdir(namespace, 0o700); err != nil {
		t.Fatalf("seed namespace: %v", err)
	}
	applyFileDACLWithAuthUsersReadACE(t, namespace)
	lease, acquired, err := tryAcquireAdoptManifestLease("broad-namespace")
	if lease != nil {
		t.Cleanup(func() { _ = lease.Unlock() })
	}
	if err == nil || acquired {
		t.Fatalf("broad-DACL namespace accepted: acquired=%v err=%v", acquired, err)
	}
	if _, statErr := os.Stat(filepath.Join(namespace, "broad-namespace"+adoptManifestLeaseSuffix)); !os.IsNotExist(statErr) {
		t.Fatalf("refused namespace gained a lease leaf: %v", statErr)
	}
}

// TestAdoptLeaseCleanupFailureMatrix forces every handle-owning acquisition and
// settlement operation to report failure only after its real syscall. Each
// branch must return its typed primary, release every exact handle/lock, allow a
// clean reacquisition, and leave no normal-result lease leaf.
func TestAdoptLeaseCleanupFailureMatrix(t *testing.T) {
	type cleanupCase struct {
		phase, stage, wantID string
	}
	for _, tc := range []cleanupCase{
		{"acquire", "namespace-open", adoptLeaseFailureNamespaceRefused},
		{"acquire", "guard-open", adoptLeaseFailureCleanup},
		{"acquire", "guard-verify", adoptLeaseFailureCleanup},
		{"acquire", "guard-lock", adoptLeaseFailureCleanup},
		{"acquire", "leaf-open", adoptLeaseFailureCleanup},
		{"acquire", "leaf-verify", adoptLeaseFailureCleanup},
		{"acquire", "leaf-lock", adoptLeaseFailureCleanup},
		{"settle", "guard-open", adoptLeaseFailureCleanup},
		{"settle", "guard-verify", adoptLeaseFailureCleanup},
		{"settle", "guard-lock", adoptLeaseFailureCleanup},
		{"settle", "guard-unlock", adoptLeaseFailureCleanup},
		{"settle", "guard-close", adoptLeaseFailureCleanup},
		{"settle", "leaf-open", adoptLeaseFailureSlotReplaced},
		{"settle", "leaf-verify", adoptLeaseFailureSlotReplaced},
		{"settle", "lease-unlock", adoptLeaseFailureCleanup},
		{"settle", "delete-mark", adoptLeaseFailureCleanup},
		{"settle", "close", adoptLeaseFailureCleanup},
		{"settle", "absence-readback", adoptLeaseFailureCleanup},
	} {
		t.Run(tc.phase+"-"+tc.stage, func(t *testing.T) {
			_ = isolateStateDir(t)
			bootstrapSecureAdoptLeaseNamespace(t)
			entry := "cleanup-" + tc.phase + "-" + tc.stage
			previous := adoptLeaseWindowsFailureHook
			cause := errors.New("injected-" + tc.phase + "-" + tc.stage)
			called := false
			installFailure := func() {
				adoptLeaseWindowsFailureHook = func(got string) error {
					if got == tc.stage {
						called = true
						return cause
					}
					return nil
				}
			}
			t.Cleanup(func() { adoptLeaseWindowsFailureHook = previous })
			var err error
			if tc.phase == "acquire" {
				installFailure()
				lease, acquired, acquireErr := tryAcquireAdoptManifestLease(entry)
				if lease != nil {
					t.Cleanup(func() { _ = lease.Unlock() })
				}
				if acquired {
					t.Fatalf("acquire succeeded after injected %s", tc.stage)
				}
				err = acquireErr
			} else {
				lease, acquired, acquireErr := tryAcquireAdoptManifestLease(entry)
				if acquireErr != nil || !acquired {
					t.Fatalf("setup acquire: acquired=%v err=%v", acquired, acquireErr)
				}
				installFailure()
				err = lease.ReleaseAndRemove()
			}
			if err == nil {
				t.Fatalf("%s succeeded, want cleanup failure for stage %q", tc.phase, tc.stage)
			}
			if !called {
				t.Fatalf("%s did not reach injected stage %q", tc.phase, tc.stage)
			}
			{
				var leaseFailure *LeaseFailure
				if !errors.As(err, &leaseFailure) || leaseFailure.FailureID != tc.wantID || !errors.Is(err, cause) {
					t.Fatalf("%s %s error=%v, want typed %s plus injected cause", tc.phase, tc.stage, err, tc.wantID)
				}
			}
			adoptLeaseWindowsFailureHook = previous
			reacquired, ok, err := tryAcquireAdoptManifestLease(entry)
			if err != nil || !ok {
				t.Fatalf("reacquire after %s %s failure: ok=%v err=%v", tc.phase, tc.stage, ok, err)
			}
			if err := reacquired.ReleaseAndRemove(); err != nil {
				t.Fatalf("clean settlement after %s %s failure: %v", tc.phase, tc.stage, err)
			}
			leasePath, err := adoptManifestLeasePath(entry)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(leasePath); !os.IsNotExist(err) {
				t.Fatalf("residual leaf after %s %s matrix case: %v", tc.phase, tc.stage, err)
			}
		})
	}
}

// TestAdoptLeaseWindowsLateReadbackReplacementIsRecoveryRequired catches a
// replacement created only after the retained original handle has closed. The
// final readback owns detection, so SLOT_REPLACED may appear behind an earlier
// typed cleanup cause and must still dominate the public classification.
func TestAdoptLeaseWindowsLateReadbackReplacementIsRecoveryRequired(t *testing.T) {
	_ = isolateStateDir(t)
	const entry = "late-readback-replacement"
	lease, acquired, err := tryAcquireAdoptManifestLease(entry)
	if err != nil || !acquired {
		t.Fatalf("acquire original lease: acquired=%v err=%v", acquired, err)
	}
	leasePath, err := adoptManifestLeasePath(entry)
	if err != nil {
		t.Fatalf("derive lease path: %v", err)
	}
	const foreign = "late-replacement-must-survive"
	previous := adoptLeaseWindowsFailureHook
	adoptLeaseWindowsFailureHook = func(stage string) error {
		if stage != "close" {
			return nil
		}
		if err := os.WriteFile(leasePath, []byte(foreign), 0o600); err != nil {
			return err
		}
		return nil
	}
	t.Cleanup(func() { adoptLeaseWindowsFailureHook = previous })

	err = lease.ReleaseAndRemove()
	var leaseFailure *LeaseFailure
	if !errors.As(err, &leaseFailure) || leaseFailure.FailureID != adoptLeaseFailureSlotReplaced || !leaseFailure.RecoveryRequired {
		t.Fatalf("late replacement error=%v, want recovery-required %s", err, adoptLeaseFailureSlotReplaced)
	}
	if got, readErr := os.ReadFile(leasePath); readErr != nil || string(got) != foreign {
		t.Fatalf("late foreign replacement changed: bytes=%q err=%v", got, readErr)
	}
}

// TestAdoptLeaseRootRefusalDiagnosticCategories keeps the public refusal
// contract stable while making the three root-admission refusal boundaries
// distinguishable to in-process diagnostics. Each hostile fixture must remain
// untouched: acquisition may not create a namespace or a manifest lease.
func TestAdoptLeaseRootRefusalDiagnosticCategories(t *testing.T) {
	for _, tc := range []struct {
		name           string
		configure      func(t *testing.T) func() error
		wantCategory   AdoptLeaseNamespaceFailureCategory
		wantNativeCode bool
		wantPublic     string
	}{
		{
			name: "root-open",
			configure: func(t *testing.T) func() error {
				t.Helper()
				stateRoot := isolateStateDir(t)
				previous := adoptLeaseWindowsFailureHook
				adoptLeaseWindowsFailureHook = func(stage string) error {
					if stage == "root-open" {
						return syscall.Errno(windows.ERROR_ACCESS_DENIED)
					}
					return nil
				}
				t.Cleanup(func() { adoptLeaseWindowsFailureHook = previous })
				return func() error {
					if _, err := os.Stat(filepath.Join(stateRoot, adoptProvenanceSnapshotSubdir)); !os.IsNotExist(err) {
						return err
					}
					return nil
				}
			},
			wantCategory:   AdoptLeaseNamespaceFailureRootOpen,
			wantNativeCode: true,
			wantPublic:     "E_ADOPT_LEASE_NAMESPACE_REFUSED reason=state-root-refused action=leave-unchanged category=root-open native_error_code=5",
		},
		{
			name: "root-kind-reparse",
			configure: func(t *testing.T) func() error {
				t.Helper()
				statePathsHelper(t)
				parent := hardenedTempDir(t)
				foreign := hardenedTempDir(t)
				stateRoot := filepath.Join(parent, "state-root-link")
				if err := createJunctionForTest(stateRoot, foreign); err != nil {
					t.Skipf("junction creation unavailable; root reparse category remains unrun: %v", err)
				}
				daemonStateRootOverride = stateRoot
				before := readSortedNames(t, foreign)
				return func() error {
					if got := readSortedNames(t, foreign); !reflect.DeepEqual(got, before) {
						return fmt.Errorf("foreign target changed: before=%v after=%v", before, got)
					}
					return nil
				}
			},
			wantCategory: AdoptLeaseNamespaceFailureRootKindReparse,
			wantPublic:   "E_ADOPT_LEASE_NAMESPACE_REFUSED reason=state-root-refused action=leave-unchanged category=root-kind-reparse",
		},
		{
			name: "root-security",
			configure: func(t *testing.T) func() error {
				t.Helper()
				statePathsHelper(t)
				stateRoot := hardenedTempDir(t)
				daemonStateRootOverride = stateRoot
				applyFileDACLWithAuthUsersReadACE(t, stateRoot)
				beforeDACL := windowsSDDLForTest(t, stateRoot)
				beforeNames := readSortedNames(t, stateRoot)
				return func() error {
					if got := windowsSDDLForTest(t, stateRoot); got != beforeDACL {
						return fmt.Errorf("state-root DACL changed")
					}
					if got := readSortedNames(t, stateRoot); !reflect.DeepEqual(got, beforeNames) {
						return fmt.Errorf("state-root entries changed: before=%v after=%v", beforeNames, got)
					}
					return nil
				}
			},
			wantCategory: AdoptLeaseNamespaceFailureRootSecurity,
			wantPublic:   "E_ADOPT_LEASE_NAMESPACE_REFUSED reason=state-root-refused action=leave-unchanged category=root-security",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unchanged := tc.configure(t)
			lease, acquired, err := acquireAdoptManifestLeasePlatform("diagnostic-" + tc.name)
			if lease != nil {
				t.Cleanup(func() { _ = lease.unlock() })
			}
			if err == nil || acquired {
				t.Fatalf("refusal result: acquired=%v err=%v", acquired, err)
			}
			var failure *LeaseFailure
			if !errors.As(err, &failure) || failure.Category != tc.wantCategory {
				t.Fatalf("lease failure category=%q err=%v, want %q", failure.Category, err, tc.wantCategory)
			}
			if got := failure.PublicMessage(); got != tc.wantPublic {
				t.Fatalf("public refusal=%q, want %q", got, tc.wantPublic)
			}
			if got := newAdoptStageError("lease-acquire", "uncommitted", err).Error(); got != tc.wantPublic {
				t.Fatalf("stage public refusal=%q, want %q", got, tc.wantPublic)
			}
			if (failure.NativeErrorCode != 0) != tc.wantNativeCode {
				t.Fatalf("lease failure native code=%d, want present=%v", failure.NativeErrorCode, tc.wantNativeCode)
			}
			if tc.wantNativeCode && !errors.Is(err, syscall.Errno(windows.ERROR_ACCESS_DENIED)) {
				t.Fatalf("lease failure did not preserve the native cause: %v", err)
			}
			var namespaceFailure *LeaseNamespaceFailure
			if !errors.As(err, &namespaceFailure) || namespaceFailure.Category != tc.wantCategory {
				t.Fatalf("namespace failure category=%q err=%v, want %q", namespaceFailure.Category, err, tc.wantCategory)
			}
			if err := unchanged(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAdoptLeaseNamespaceFailurePublicProjectionDropsUnknownCategory(t *testing.T) {
	const want = "E_ADOPT_LEASE_NAMESPACE_REFUSED reason=state-root-refused action=leave-unchanged"
	const untrusted = AdoptLeaseNamespaceFailureCategory("root-secret-canary")
	for _, tc := range []struct {
		name string
		got  string
	}{
		{
			name: "lease-owner",
			got: (&LeaseFailure{
				FailureID:       adoptLeaseFailureNamespaceRefused,
				ReasonID:        AdoptLeaseReasonStateRootRefused,
				Action:          AdoptLeaseActionLeaveUnchanged,
				Category:        untrusted,
				NativeErrorCode: 12345,
			}).PublicMessage(),
		},
		{
			name: "namespace-owner",
			got: (&LeaseNamespaceFailure{
				FailureID:       adoptLeaseFailureNamespaceRefused,
				ReasonID:        AdoptLeaseReasonStateRootRefused,
				Action:          AdoptLeaseActionLeaveUnchanged,
				Category:        untrusted,
				NativeErrorCode: 12345,
			}).Error(),
		},
		{
			name: "stage-projection",
			got: newAdoptStageError("lease-acquire", "uncommitted", &LeaseFailure{
				FailureID:       adoptLeaseFailureNamespaceRefused,
				ReasonID:        AdoptLeaseReasonStateRootRefused,
				Action:          AdoptLeaseActionLeaveUnchanged,
				Category:        untrusted,
				NativeErrorCode: 12345,
			}).Error(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != want {
				t.Fatalf("untrusted category leaked into public projection: %q", tc.got)
			}
		})
	}
}
