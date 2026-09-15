//go:build windows

package api

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/windows"
)

func acquireReleasedProviderSuffixLeaseForReview(t *testing.T, name string) (string, string) {
	t.Helper()
	statePathsHelper(t)
	stateDir := hardenedTempDir(t)
	daemonStateRootOverride = stateDir
	lease, acquired, err := tryAcquireManifestMutationLease(name)
	if lease != nil {
		t.Cleanup(func() {
			if err := lease.Unlock(); err != nil {
				t.Errorf("cleanup ordinary lease: %v", err)
			}
		})
	}
	if err != nil || !acquired || lease == nil {
		t.Fatalf("ordinary acquire %q: acquired=%v err=%v", name, acquired, err)
	}
	if err := lease.Unlock(); err != nil {
		t.Fatalf("ordinary unlock %q: %v", name, err)
	}
	namespace := filepath.Join(stateDir, adoptProvenanceSnapshotSubdir)
	path := filepath.Join(namespace, name+adoptManifestLeaseSuffix)
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
		t.Fatalf("ordinary lease was not retained as an empty regular file: info=%v err=%v", info, err)
	}
	return namespace, path
}

func TestProviderLeaseSuffixReviewOrdinaryReleaseInspectMigrate(t *testing.T) {
	for _, name := range []string{"foo", "foo.lease", "foo.lock", "foo.lease.lease", "foo.lock.lease"} {
		t.Run(name, func(t *testing.T) {
			namespace, path := acquireReleasedProviderSuffixLeaseForReview(t, name)
			beforeNames := readSortedNames(t, namespace)
			beforeID := windowsFileIdentityForTest(t, path)
			beforeSDDL := windowsSDDLForTest(t, path)
			for _, operation := range []struct {
				name string
				run  func() (AdoptLeaseNamespaceReport, error)
			}{
				{"inspect", InspectAdoptLeaseNamespace},
				{"dry-run", func() (AdoptLeaseNamespaceReport, error) {
					return MigrateLegacyAdoptLeaseNamespace(AdoptLeaseNamespaceMigrationOpts{})
				}},
				{"migrate", func() (AdoptLeaseNamespaceReport, error) {
					return MigrateLegacyAdoptLeaseNamespace(AdoptLeaseNamespaceMigrationOpts{Yes: true})
				}},
			} {
				report, err := operation.run()
				if err != nil || report.State != AdoptLeaseNamespaceReady || report.LeaseLeafCount != 2 ||
					report.SnapshotDirCount != 0 || report.MigrationEligible || report.ChangedLeafCount != 0 || report.NamespaceChanged {
					t.Errorf("%s after ordinary acquire/unlock %q: report=%+v err=%v", operation.name, name, report, err)
				}
			}
			if got := readSortedNames(t, namespace); !reflect.DeepEqual(got, beforeNames) {
				t.Fatalf("namespace entries changed: before=%v after=%v", beforeNames, got)
			}
			if got := windowsFileIdentityForTest(t, path); !sameWindowsAdoptLeaseIdentity(beforeID, got) {
				t.Fatal("lease identity changed")
			}
			if got := windowsSDDLForTest(t, path); got != beforeSDDL {
				t.Fatal("ready lease DACL changed")
			}
		})
	}
}

func TestProviderLeaseSuffixReviewLegacyMigration(t *testing.T) {
	for _, name := range []string{"foo.lease", "foo.lock", "foo.lease.lease"} {
		t.Run(name, func(t *testing.T) {
			_, namespace, path := seedRecognizedLegacyLeaseNamespace(t, []string{name + adoptManifestLeaseSuffix}, true)
			beforeNames := readSortedNames(t, namespace)
			beforeID := windowsFileIdentityForTest(t, path)
			snapshot := filepath.Join(namespace, "existing-snapshot")
			beforeSnapshotSDDL := windowsSDDLForTest(t, snapshot)
			beforeNamespaceSDDL := windowsSDDLForTest(t, namespace)
			beforeLeafSDDL := windowsSDDLForTest(t, path)
			report, err := InspectAdoptLeaseNamespace()
			if err != nil || report.State != AdoptLeaseNamespaceLegacy || !report.MigrationEligible ||
				report.LeaseLeafCount != 1 || report.SnapshotDirCount != 1 {
				t.Errorf("inspect legacy suffix lease: report=%+v err=%v", report, err)
			}
			if windowsSDDLForTest(t, namespace) != beforeNamespaceSDDL || windowsSDDLForTest(t, path) != beforeLeafSDDL {
				t.Fatal("inspection changed legacy DACLs")
			}
			report, err = MigrateLegacyAdoptLeaseNamespace(AdoptLeaseNamespaceMigrationOpts{Yes: true})
			if err != nil || report.State != AdoptLeaseNamespaceReady || report.ChangedLeafCount != 1 ||
				!report.NamespaceChanged || report.RollbackPerformed {
				t.Fatalf("migrate legacy suffix lease: report=%+v err=%v", report, err)
			}
			assertWindowsPathDACLAllowlist(t, namespace, true)
			assertWindowsPathDACLAllowlist(t, path, false)
			if windowsSDDLForTest(t, snapshot) != beforeSnapshotSDDL {
				t.Fatal("safe snapshot DACL changed")
			}
			if got := readSortedNames(t, namespace); !reflect.DeepEqual(got, beforeNames) {
				t.Fatalf("migration changed entries: before=%v after=%v", beforeNames, got)
			}
			if got := windowsFileIdentityForTest(t, path); !sameWindowsAdoptLeaseIdentity(beforeID, got) || got.FileSizeHigh != 0 || got.FileSizeLow != 0 {
				t.Fatal("migration changed lease identity or size")
			}
		})
	}
}

func TestProviderLeaseSuffixReviewRefusesUnsafeLeaves(t *testing.T) {
	for _, mutation := range []string{"nonempty", "hardlink", "broad-dacl", "directory"} {
		t.Run(mutation, func(t *testing.T) {
			namespace, path := acquireReleasedProviderSuffixLeaseForReview(t, "foo.lease")
			switch mutation {
			case "nonempty":
				if err := os.WriteFile(path, []byte("suffix-lease-canary"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(path, filepath.Join(filepath.Dir(namespace), "outside-link")); err != nil {
					t.Fatal(err)
				}
			case "broad-dacl":
				applyFileDACLWithAuthUsersReadACE(t, path)
			case "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			beforeNames := readSortedNames(t, namespace)
			beforeNamespaceSDDL := windowsSDDLForTest(t, namespace)
			beforeLeafSDDL := windowsSDDLForTest(t, path)
			for _, apply := range []bool{false, true} {
				report, err := MigrateLegacyAdoptLeaseNamespace(AdoptLeaseNamespaceMigrationOpts{Yes: apply})
				if err == nil || report.State != AdoptLeaseNamespaceRefused || report.ReasonID != AdoptLeaseReasonNamespaceUnrecognized ||
					report.Action != AdoptLeaseActionLeaveUnchanged || report.ChangedLeafCount != 0 || report.NamespaceChanged {
					t.Errorf("unsafe %s apply=%v: report=%+v err=%v", mutation, apply, report, err)
				}
				if windowsSDDLForTest(t, namespace) != beforeNamespaceSDDL || windowsSDDLForTest(t, path) != beforeLeafSDDL {
					t.Fatal("refusal changed DACLs")
				}
				if got := readSortedNames(t, namespace); !reflect.DeepEqual(got, beforeNames) {
					t.Fatalf("refusal changed entries: before=%v after=%v", beforeNames, got)
				}
			}
			if mutation == "nonempty" {
				if raw, err := os.ReadFile(path); err != nil || string(raw) != "suffix-lease-canary" {
					t.Fatalf("refusal changed content: raw=%q err=%v", raw, err)
				}
			}
		})
	}
}

func TestProviderLeaseSuffixReviewRejectsMalformedNamesAndSnapshotCollision(t *testing.T) {
	namespace, _ := acquireReleasedProviderSuffixLeaseForReview(t, "foo")
	entries, err := allowlistExplicitAccess()
	if err != nil {
		t.Fatal(err)
	}
	// These invalid basenames are real, empty, secure files: absence or a
	// hostile DACL must not be what makes the name checks pass.
	for _, name := range []string{".lease", "..lease", "foo .lease", "foo..lease"} {
		path := filepath.Join(namespace, name)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		applyProtectedDACLFromEntries(t, path, entries)
		assertWindowsPathDACLAllowlist(t, path, false)
	}
	ns := openDirHandleNoReparseForTest(t, namespace)
	defer windows.CloseHandle(ns)
	for _, name := range []string{"", ".lease", "..lease", "../foo.lease", `foo\bar.lease`, "foo:stream.lease", "foo .lease", "foo..lease", "FOO.lease", "con.lease", "nul.lock.lease"} {
		h, kind, _, err := openAndValidateWindowsLeaseNamespaceEntry(ns, name, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
		if h != windows.InvalidHandle {
			_ = windows.CloseHandle(h)
		}
		if err == nil || h != windows.InvalidHandle || kind != "" {
			t.Errorf("malformed name %q admitted: kind=%q err=%v", name, kind, err)
		}
	}
	for _, name := range []string{"foo.lease", "foo.lease.lease", "foo.lock.lease"} {
		if lease, acquired, err := tryAcquireAdoptManifestLease(name); err == nil || acquired || lease != nil {
			if lease != nil {
				_ = lease.Unlock()
			}
			t.Errorf("snapshot-owning adopt accepted reserved suffix %q: acquired=%v err=%v", name, acquired, err)
		}
	}
}
