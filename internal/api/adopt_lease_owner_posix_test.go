//go:build !windows

package api

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPosixAdoptLeaseSettlementRetainsNoncooperativeReplacement(t *testing.T) {
	_ = isolateStateDir(t)
	const entry = "posix-noncooperative-replacement"
	lease, acquired, err := tryAcquireAdoptManifestLease(entry)
	if err != nil || !acquired {
		t.Fatalf("acquire original lease: acquired=%v err=%v", acquired, err)
	}

	owner, ok := lease.impl.(*posixAdoptLease)
	if !ok {
		t.Fatalf("lease implementation is %T, want *posixAdoptLease", lease.impl)
	}
	const original = "original-owner-inode"
	if _, err := unix.Pwrite(owner.handle, []byte(original), 0); err != nil {
		t.Fatalf("seed original owner inode: %v", err)
	}
	leasePath, err := adoptManifestLeasePath(entry)
	if err != nil {
		t.Fatalf("derive lease path: %v", err)
	}
	originalDev, originalIno := posixLeaseIdentity(t, leasePath)
	movedOriginal := filepath.Join(filepath.Dir(leasePath), "noncooperative-original")
	const replacement = "noncooperative-replacement"

	previous := adoptLeaseBeforeSettlementHook
	adoptLeaseBeforeSettlementHook = func() error {
		if err := os.Rename(leasePath, movedOriginal); err != nil {
			return err
		}
		return os.WriteFile(leasePath, []byte(replacement), 0o600)
	}
	t.Cleanup(func() { adoptLeaseBeforeSettlementHook = previous })

	err = lease.ReleaseAndRemove()
	var failure *LeaseFailure
	if !errors.As(err, &failure) || failure.FailureID != adoptLeaseFailureSlotReplaced || !failure.RecoveryRequired {
		t.Fatalf("replacement settlement error=%v, want recovery-required %s", err, adoptLeaseFailureSlotReplaced)
	}
	if got, readErr := os.ReadFile(movedOriginal); readErr != nil || string(got) != original {
		t.Fatalf("settlement unlinked or changed original inode: bytes=%q err=%v", got, readErr)
	}
	if got, readErr := os.ReadFile(leasePath); readErr != nil || string(got) != replacement {
		t.Fatalf("settlement unlinked or changed canonical replacement: bytes=%q err=%v", got, readErr)
	}
	if gotDev, gotIno := posixLeaseIdentity(t, movedOriginal); gotDev != originalDev || gotIno != originalIno {
		t.Fatalf("moved original identity=(%d,%d), want retained owner identity=(%d,%d)", gotDev, gotIno, originalDev, originalIno)
	}
	if gotDev, gotIno := posixLeaseIdentity(t, leasePath); gotDev == originalDev && gotIno == originalIno {
		t.Fatalf("canonical replacement retained original identity=(%d,%d)", gotDev, gotIno)
	}
}

func TestPosixAdoptLeaseReusesOneCanonicalInodeAcross100Settlements(t *testing.T) {
	stateDir := isolateStateDir(t)
	const entry = "posix-persistent-owner-inode"
	leasePath, err := adoptManifestLeasePath(entry)
	if err != nil {
		t.Fatalf("derive lease path: %v", err)
	}
	namespace := filepath.Join(stateDir, adoptProvenanceSnapshotSubdir)
	baseline := posixLeaseNamespaceEntryNames(t, namespace)

	var wantDev, wantIno uint64
	for cycle := 0; cycle != 100; cycle++ {
		lease, acquired, err := tryAcquireAdoptManifestLease(entry)
		if err != nil || !acquired {
			t.Fatalf("cycle %d acquire: acquired=%v err=%v", cycle, acquired, err)
		}
		gotDev, gotIno := posixLeaseIdentity(t, leasePath)
		if cycle == 0 {
			wantDev, wantIno = gotDev, gotIno
		} else if gotDev != wantDev || gotIno != wantIno {
			t.Fatalf("cycle %d acquired identity=(%d,%d), want persistent=(%d,%d)", cycle, gotDev, gotIno, wantDev, wantIno)
		}
		if err := lease.ReleaseAndRemove(); err != nil {
			t.Fatalf("cycle %d settlement: %v", cycle, err)
		}
		if gotDev, gotIno = posixLeaseIdentity(t, leasePath); gotDev != wantDev || gotIno != wantIno {
			t.Fatalf("cycle %d settlement changed canonical identity=(%d,%d), want=(%d,%d)", cycle, gotDev, gotIno, wantDev, wantIno)
		}
	}

	names := posixLeaseNamespaceEntryNames(t, namespace)
	want := append([]string(nil), baseline...)
	for _, required := range []string{adoptLeaseNamespaceLockLeaf, entry + adoptManifestLeaseSuffix} {
		present := false
		for _, existing := range want {
			if existing == required {
				present = true
				break
			}
		}
		if !present {
			want = append(want, required)
		}
	}
	sort.Strings(want)
	if len(names) != len(want) {
		t.Fatalf("persistent lease namespace entries=%q, want exactly %q", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("persistent lease namespace entries=%q, want exactly %q", names, want)
		}
	}
}

func posixLeaseNamespaceEntryNames(t *testing.T, namespace string) []string {
	t.Helper()
	entries, err := os.ReadDir(namespace)
	if err != nil {
		t.Fatalf("read persistent lease namespace: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("unexpected directory in persistent lease namespace: %q", entry.Name())
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func TestPosixAdoptLeaseSettlementGuardRejectsConcurrentAcquirer(t *testing.T) {
	_ = isolateStateDir(t)
	const entry = "posix-settlement-guard"
	lease, acquired, err := tryAcquireAdoptManifestLease(entry)
	if err != nil || !acquired {
		t.Fatalf("acquire owner: acquired=%v err=%v", acquired, err)
	}

	entered := make(chan struct{})
	continueSettlement := make(chan struct{})
	previous := adoptLeaseBeforeSettlementHook
	adoptLeaseBeforeSettlementHook = func() error {
		close(entered)
		<-continueSettlement
		return nil
	}
	t.Cleanup(func() { adoptLeaseBeforeSettlementHook = previous })
	settled := make(chan error, 1)
	go func() { settled <- lease.ReleaseAndRemove() }()
	<-entered

	other, acquired, err := tryAcquireAdoptManifestLease(entry)
	if other != nil {
		_ = other.Unlock()
		t.Fatal("concurrent acquirer received a lease while settlement guard was held")
	}
	var failure *LeaseFailure
	if !errors.As(err, &failure) || acquired || failure.FailureID != adoptLeaseFailureGuardBusy || !failure.Retryable {
		t.Fatalf("concurrent acquire=(lease=%T acquired=%v err=%v), want retryable %s", other, acquired, err, adoptLeaseFailureGuardBusy)
	}
	close(continueSettlement)
	if err := <-settled; err != nil {
		t.Fatalf("owner settlement after guard check: %v", err)
	}
	adoptLeaseBeforeSettlementHook = previous

	reacquired, acquired, err := tryAcquireAdoptManifestLease(entry)
	if err != nil || !acquired {
		t.Fatalf("acquire after settled guard: acquired=%v err=%v", acquired, err)
	}
	if err := reacquired.ReleaseAndRemove(); err != nil {
		t.Fatalf("settle reacquired lease: %v", err)
	}
}

func TestPosixAdoptLeaseSettlementFailureIsTypedAndReleasesOwnerLock(t *testing.T) {
	_ = isolateStateDir(t)
	const entry = "posix-typed-cleanup"
	lease, acquired, err := tryAcquireAdoptManifestLease(entry)
	if err != nil || !acquired {
		t.Fatalf("acquire owner: acquired=%v err=%v", acquired, err)
	}
	previous := adoptLeaseBeforeSettlementHook
	adoptLeaseBeforeSettlementHook = func() error { return errors.New("injected POSIX settlement failure") }
	t.Cleanup(func() { adoptLeaseBeforeSettlementHook = previous })

	err = lease.ReleaseAndRemove()
	var failure *LeaseFailure
	if !errors.As(err, &failure) || failure.FailureID != adoptLeaseFailureCleanup {
		t.Fatalf("settlement error=%v, want typed %s", err, adoptLeaseFailureCleanup)
	}
	adoptLeaseBeforeSettlementHook = previous
	reacquired, acquired, err := tryAcquireAdoptManifestLease(entry)
	if err != nil || !acquired {
		t.Fatalf("typed cleanup error stranded owner lock: acquired=%v err=%v", acquired, err)
	}
	if err := reacquired.ReleaseAndRemove(); err != nil {
		t.Fatalf("settle reacquired lease: %v", err)
	}
}

func TestPosixAdoptLeaseSettlementContainsNoPathRemovalOrRename(t *testing.T) {
	_, sourcePath, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate POSIX lease test source")
	}
	ownerPath := filepath.Join(filepath.Dir(sourcePath), "adopt_lease_owner_posix.go")
	parsed, err := parser.ParseFile(token.NewFileSet(), ownerPath, nil, 0)
	if err != nil {
		t.Fatalf("parse POSIX lease owner: %v", err)
	}
	var forbidden []string
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		packageName, ok := selector.X.(*ast.Ident)
		if !ok || (packageName.Name != "unix" && packageName.Name != "os") {
			return true
		}
		switch selector.Sel.Name {
		case "Unlink", "Unlinkat", "Remove", "RemoveAll", "Rename", "Renameat":
			forbidden = append(forbidden, packageName.Name+"."+selector.Sel.Name)
		}
		return true
	})
	if len(forbidden) != 0 {
		t.Fatalf("POSIX lease settlement must not delete, tombstone, or rename canonical paths; found %q", forbidden)
	}
}

func posixLeaseIdentity(t *testing.T, path string) (uint64, uint64) {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	return uint64(st.Dev), st.Ino
}
