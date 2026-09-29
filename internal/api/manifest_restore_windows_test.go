//go:build windows

package api

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func restoreTestLinkCount(t *testing.T, path string) uint32 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		t.Fatal(err)
	}
	return info.NumberOfLinks
}

func TestRestoreManifestCompleteNoReplace(t *testing.T) {
	root := t.TempDir()
	name := "restore-test"
	raw := renderStdioBridgeManifestYAML(name, "node", []string{"server.js"}, nil, 9137, nil)
	a := NewAPI()
	if err := a.manifestRestoreExactInWithAdoptLeaseHeld(root, name, raw); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, name, "manifest.yaml")
	got, err := os.ReadFile(target)
	if err != nil || string(got) != raw {
		t.Fatalf("restored manifest mismatch: err=%v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, name))
	if err != nil || len(entries) != 1 || entries[0].Name() != "manifest.yaml" {
		t.Fatalf("owned temporary residue: entries=%v err=%v", entries, err)
	}
	if err := a.manifestRestoreExactInWithAdoptLeaseHeld(root, name, raw); err == nil {
		t.Fatal("existing canonical manifest was accepted as a new publication")
	}
	got, err = os.ReadFile(target)
	if err != nil || string(got) != raw {
		t.Fatalf("existing manifest changed: err=%v", err)
	}
}

func TestRestoreManifestConcurrentTargetCreationNeverReplaces(t *testing.T) {
	root := t.TempDir()
	name := "restore-race"
	raw := []byte(renderStdioBridgeManifestYAML(name, "node", []string{"server.js"}, nil, 19371, nil))
	final := filepath.Join(root, name, "manifest.yaml")
	foreign := []byte("foreign-existing-target")
	err := restoreExactManifestNoReplaceWithHook(root, name, raw, func() {
		if writeErr := os.WriteFile(final, foreign, 0600); writeErr != nil {
			t.Error(writeErr)
		}
	})
	var refusal *ManifestRestoreError
	if !errors.As(err, &refusal) || refusal.Code != "ATTACH_MANIFEST_RESTORE_CONFLICT" {
		t.Fatalf("concurrent target was not a typed conflict: %v", err)
	}
	got, err := os.ReadFile(final)
	if err != nil || string(got) != string(foreign) {
		t.Fatalf("concurrent target was replaced: bytes=%q err=%v", got, err)
	}
	entries, err := os.ReadDir(filepath.Join(root, name))
	if err != nil || len(entries) != 1 || entries[0].Name() != "manifest.yaml" {
		t.Fatalf("restore left temporary residue: entries=%v err=%v", entries, err)
	}
}

func TestRestoreManifestReaderSeesOnlyAbsentOrCompleteFinal(t *testing.T) {
	root := t.TempDir()
	name := "restore-reader"
	raw := []byte(renderStdioBridgeManifestYAML(name, "node", []string{"server.js"}, nil, 19379, nil) + strings.Repeat("# complete comment\n", 1024))
	final := filepath.Join(root, name, "manifest.yaml")
	stop := make(chan struct{})
	done := make(chan struct{})
	firstRead := make(chan struct{})
	readerErr := make(chan error, 1)
	started := false
	err := restoreExactManifestNoReplaceWithHook(root, name, raw, func() {
		started = true
		go func() {
			defer close(done)
			first := true
			for {
				got, readErr := os.ReadFile(final)
				if readErr != nil && !os.IsNotExist(readErr) || readErr == nil && string(got) != string(raw) {
					select {
					case readerErr <- errors.New("reader saw partial or unverified final file"):
					default:
					}
				}
				if first {
					first = false
					close(firstRead)
				}
				select {
				case <-stop:
					return
				default:
				}
			}
		}()
		<-firstRead // at least one absent read before Link
	})
	if started {
		time.Sleep(20 * time.Millisecond)
		close(stop)
		<-done
	}
	if err != nil {
		t.Fatal(err)
	}
	select {
	case readErr := <-readerErr:
		t.Fatal(readErr)
	default:
	}
	got, readErr := os.ReadFile(final)
	if readErr != nil || string(got) != string(raw) {
		t.Fatalf("published final was not complete: err=%v", readErr)
	}
}

func TestRestoreManifestConcurrentTargetAndCleanupFailureIsUnverified(t *testing.T) {
	root := t.TempDir()
	name := "restore-cleanup-fault"
	raw := []byte(renderStdioBridgeManifestYAML(name, "node", []string{"server.js"}, nil, 19375, nil))
	final := filepath.Join(root, name, "manifest.yaml")
	foreign := []byte("foreign-existing-target")
	foreignRecovery := filepath.Join(root, name, ".manifest-recovery", "foreign-entry")
	err := restoreExactManifestNoReplaceWithHook(root, name, raw, func() {
		if writeErr := os.WriteFile(final, foreign, 0600); writeErr != nil {
			t.Fatal(writeErr)
		}
		if writeErr := os.WriteFile(foreignRecovery, []byte("retain"), 0600); writeErr != nil {
			t.Fatal(writeErr)
		}
	})
	var refusal *ManifestRestoreError
	if !errors.As(err, &refusal) || refusal.Code != "ATTACH_MANIFEST_RESTORE_UNVERIFIED" {
		t.Errorf("cleanup failure must be typed uncertainty, not a settled conflict: %v", err)
	}
	got, readErr := os.ReadFile(final)
	if readErr != nil || string(got) != string(foreign) {
		t.Fatalf("concurrent target was replaced: bytes=%q err=%v", got, readErr)
	}
	if got, readErr := os.ReadFile(foreignRecovery); readErr != nil || string(got) != "retain" {
		t.Fatalf("foreign recovery leaf was lost: bytes=%q err=%v", got, readErr)
	}
}

func TestRestoreManifestUnsupportedLinkCleanupFailureIsUnverified(t *testing.T) {
	root := t.TempDir()
	name := "restore-unsupported-cleanup"
	raw := []byte(renderStdioBridgeManifestYAML(name, "node", []string{"server.js"}, nil, 19378, nil))
	foreignRecovery := filepath.Join(root, name, ".manifest-recovery", "foreign-entry")
	err := restoreExactManifestNoReplaceWithLinkFn(root, name, raw, func() {
		if writeErr := os.WriteFile(foreignRecovery, []byte("retain"), 0600); writeErr != nil {
			t.Fatal(writeErr)
		}
	}, func(string, string) error { return errors.ErrUnsupported })
	var refusal *ManifestRestoreError
	if !errors.As(err, &refusal) || refusal.Code != "ATTACH_MANIFEST_RESTORE_UNVERIFIED" {
		t.Errorf("unsupported Link with failed cleanup was falsely settled: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, name, "manifest.yaml")); !os.IsNotExist(err) {
		t.Fatalf("unsupported Link created canonical source: %v", err)
	}
	if got, err := os.ReadFile(foreignRecovery); err != nil || string(got) != "retain" {
		t.Fatalf("foreign recovery leaf was changed: err=%v", err)
	}
}

func TestRestoreManifestUnsupportedLinkWithSettledCleanupIsTyped(t *testing.T) {
	root := t.TempDir()
	name := "restore-unsupported-settled"
	raw := []byte(renderStdioBridgeManifestYAML(name, "node", []string{"server.js"}, nil, 19380, nil))
	err := restoreExactManifestNoReplaceWithLinkFn(root, name, raw, nil, func(string, string) error { return errors.ErrUnsupported })
	var refusal *ManifestRestoreError
	if !errors.As(err, &refusal) || refusal.Code != "ATTACH_MANIFEST_RESTORE_UNSUPPORTED_FS" {
		t.Fatalf("settled unsupported Link has wrong code: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, name, ".manifest-recovery")); !os.IsNotExist(err) {
		t.Fatalf("settled unsupported Link left owned residue: %v", err)
	}
}

func TestRestoreManifestCrashAfterLinkReentryCleansOnlyVerifiedTemp(t *testing.T) {
	root := t.TempDir()
	name := "restore-reentry"
	raw := []byte(renderStdioBridgeManifestYAML(name, "node", []string{"server.js"}, nil, 19372, nil))
	hash := ManifestHashContent(raw)
	recoveryDir := filepath.Join(root, name, ".manifest-recovery")
	if err := os.MkdirAll(recoveryDir, 0700); err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(recoveryDir, "manifest-"+hash+".tmp")
	final := filepath.Join(root, name, "manifest.yaml")
	if err := os.WriteFile(temp, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(temp, final); err != nil {
		t.Fatal(err)
	}
	if count := restoreTestLinkCount(t, final); count != 2 {
		t.Fatalf("published pair has %d links, want 2", count)
	}
	if err := NewAPI().settlePublishedRestoreUnderLease(root, name, hash); err != nil {
		t.Fatal(err)
	}
	if count := restoreTestLinkCount(t, final); count != 1 {
		t.Fatalf("settled canonical file has %d links, want 1", count)
	}
	if _, err := os.Lstat(temp); !os.IsNotExist(err) {
		t.Fatalf("verified temp not cleaned: %v", err)
	}
	got, err := os.ReadFile(final)
	if err != nil || string(got) != string(raw) {
		t.Fatalf("canonical source changed during cleanup: err=%v", err)
	}
}

func TestRestoreManifestCrashAfterTempRemovalAcceptsEmptyOwnedRecoveryDir(t *testing.T) {
	root := t.TempDir()
	name := "restore-empty-recovery"
	raw := []byte(renderStdioBridgeManifestYAML(name, "node", []string{"server.js"}, nil, 19376, nil))
	serverDir := filepath.Join(root, name)
	recoveryDir := filepath.Join(serverDir, ".manifest-recovery")
	if err := os.MkdirAll(recoveryDir, 0700); err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(serverDir, "manifest.yaml")
	if err := os.WriteFile(final, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := NewAPI().settlePublishedRestoreUnderLease(root, name, ManifestHashContent(raw)); err != nil {
		t.Errorf("empty owned directory blocked exact present-manifest re-entry: %v", err)
	}
	if _, err := os.Lstat(recoveryDir); !os.IsNotExist(err) {
		t.Errorf("empty owned recovery directory remained after re-entry: %v", err)
	}
	got, err := os.ReadFile(final)
	if err != nil || string(got) != string(raw) {
		t.Fatalf("exact canonical manifest changed: err=%v", err)
	}
}

func TestRestoreManifestCrashAfterLinkThirdAliasRefused(t *testing.T) {
	root := t.TempDir()
	name := "restore-third-link"
	raw := []byte(renderStdioBridgeManifestYAML(name, "node", []string{"server.js"}, nil, 19377, nil))
	hash := ManifestHashContent(raw)
	recoveryDir := filepath.Join(root, name, ".manifest-recovery")
	if err := os.MkdirAll(recoveryDir, 0700); err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(recoveryDir, "manifest-"+hash+".tmp")
	final := filepath.Join(root, name, "manifest.yaml")
	foreignAlias := filepath.Join(root, "foreign-alias.yaml")
	if err := os.WriteFile(temp, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(temp, final); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(temp, foreignAlias); err != nil {
		t.Fatal(err)
	}
	var refusal *ManifestRestoreError
	if err := NewAPI().settlePublishedRestoreUnderLease(root, name, hash); !errors.As(err, &refusal) || refusal.Code != "ATTACH_MANIFEST_RESTORE_UNVERIFIED" {
		t.Errorf("third alias was accepted as an exclusive owned pair: %v", err)
	}
	for _, path := range []string{temp, final, foreignAlias} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != string(raw) {
			t.Errorf("ambiguous alias was removed or changed at %s: err=%v", filepath.Base(path), err)
		}
	}
}

func TestRestoreManifestCrashBeforeLinkReentryPublishesExactBytes(t *testing.T) {
	root := t.TempDir()
	name := "restore-orphan"
	raw := []byte(renderStdioBridgeManifestYAML(name, "node", []string{"server.js"}, nil, 19373, nil))
	recoveryDir := filepath.Join(root, name, ".manifest-recovery")
	if err := os.MkdirAll(recoveryDir, 0700); err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(recoveryDir, "manifest-"+ManifestHashContent(raw)+".tmp")
	if err := os.WriteFile(temp, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := restoreExactManifestNoReplace(root, name, raw); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, name, "manifest.yaml"))
	if err != nil || string(got) != string(raw) {
		t.Fatalf("orphan re-entry failed: err=%v", err)
	}
	if _, err := os.Lstat(recoveryDir); !os.IsNotExist(err) {
		t.Fatalf("orphan recovery directory remained: %v", err)
	}
}

func TestRestoreManifestExistingLinksNeverReplaced(t *testing.T) {
	root := t.TempDir()
	name := "restore-links"
	raw := []byte(renderStdioBridgeManifestYAML(name, "node", []string{"server.js"}, nil, 19374, nil))
	serverDir := filepath.Join(root, name)
	if err := os.Mkdir(serverDir, 0700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(root, "foreign.yaml")
	foreignBytes := []byte("foreign exact bytes")
	if err := os.WriteFile(foreign, foreignBytes, 0600); err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(serverDir, "manifest.yaml")
	for _, kind := range []string{"hardlink", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			var err error
			if kind == "hardlink" {
				err = os.Link(foreign, final)
			} else {
				err = os.Symlink(foreign, final)
				if err != nil {
					t.Skipf("test host cannot make symlink: %v", err)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Remove(final) })
			var refusal *ManifestRestoreError
			if err := restoreExactManifestNoReplace(root, name, raw); !errors.As(err, &refusal) || refusal.Code != "ATTACH_MANIFEST_RESTORE_CONFLICT" {
				t.Fatalf("existing %s target was not a conflict: %v", kind, err)
			}
			got, err := os.ReadFile(foreign)
			if err != nil || string(got) != string(foreignBytes) {
				t.Fatalf("existing %s target changed foreign bytes: err=%v", kind, err)
			}
			if err := os.Remove(final); err != nil {
				t.Fatal(err)
			}
		})
	}
}
