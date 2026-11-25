//go:build windows

package api

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestObservedSupervisorInstallBypassBindsOneBorrow(t *testing.T) {
	root := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	lease, err := TryAcquireStateFileObservedLease(t.Context(), filepath.Join(root, "supervisor.lock"))
	if lease != nil {
		t.Cleanup(func() {
			closed, closeErr := lease.CloseObserved()
			if closeErr != nil || closed == nil || closed.Validate(lease) != nil {
				t.Errorf("original interlock close: %v", closeErr)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	var escaped InstallParsedManifestBypass
	if err := lease.WithSupervisorLockBypass(t.Context(), root, func(token InstallParsedManifestBypass) error {
		escaped = token
		if !token.holdsSupervisorLock(root) || token.holdsSupervisorLock(t.TempDir()) {
			t.Error("borrow token did not bind the original held state path")
		}
		if path, err := runtimeMcphubExecutableUnderSupervisorLock(token); err != nil || path != testCanonicalMcphubPathOverride {
			t.Errorf("observed interlock lost the existing runtime-image route: %q %v", path, err)
		}
		opts := InstallParsedManifestOpts{ManifestHash: testExactManifestHash, Writer: io.Discard,
			Workspaces:           []WorkspaceEntry{{WorkspaceKey: WorkspaceKey(workspace), WorkspacePath: workspace, Language: SerenaLanguageSentinel, Backend: SerenaServerName, Port: 9401}},
			SupervisorLockBypass: token}
		if prepared, err := NewAPI().PrepareSerenaParsedInstall(t.Context(), serenaTemplateManifest(), opts); prepared != nil || err == nil || err.Error() != "SERENA_PARSED_PREPARATION_REQUIRES_UNLOCKED_CALLER" {
			t.Errorf("observed token bypassed pre-lock preparation: prepared=%v err=%v", prepared != nil, err)
		}
		if path, err := NewAPI().InstallParsedManifest(t.Context(), serenaTemplateManifest(), opts); path != "" || err == nil || err.Error() != "SERENA_PARSED_PREACQUIRED_INVOCATION_REQUIRED" {
			t.Errorf("observed token bypassed the original prepared invocation: path=%q err=%v", path, err)
		}
		for _, name := range []string{"supervisor-intent.json", "supervisor-intent.json.lock"} {
			if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
				t.Errorf("refused unlocked/preacquired entry created %s: %v", name, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if escaped.holdsSupervisorLock(root) {
		t.Fatal("escaped token remained active after its borrow")
	}
	if err := lease.WithSupervisorLockBypass(t.Context(), root, func(current InstallParsedManifestBypass) error {
		if current == escaped || escaped.holdsSupervisorLock(root) || !current.holdsSupervisorLock(root) {
			t.Error("an expired token revived during a later borrow")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	closed, err := lease.CloseObserved()
	if err != nil || closed == nil || closed.Validate(lease) != nil {
		t.Fatalf("original observed close: %v", err)
	}
	if escaped.holdsSupervisorLock(root) {
		t.Fatal("closed lease's token retained authority")
	}
	entered := false
	if err := lease.WithSupervisorLockBypass(t.Context(), root, func(InstallParsedManifestBypass) error { entered = true; return nil }); err == nil || entered {
		t.Fatal("closed original lease entered another install borrow")
	}
	// The pre-existing reservation API must still not mint this new capability.
	reserved, _, err := AcquireSupervisorLockQuietObserved(t.Context(), filepath.Join(root, "supervisor.lock"))
	if reserved != nil {
		t.Cleanup(func() {
			if _, err := reserved.ReleaseQuietObserved(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	if reserved.AllowSpecBearingWriteBypass() != (InstallParsedManifestBypass{}) {
		t.Fatal("a quiet observed reservation acquired install authority")
	}
}

func TestObservedSupervisorInstallBypassCloseWaitsForEnteredBorrow(t *testing.T) {
	root := daemonIntentTestHelper(t)
	lease, err := TryAcquireStateFileObservedLease(t.Context(), filepath.Join(root, "supervisor.lock"))
	if lease != nil {
		t.Cleanup(func() {
			closed, closeErr := lease.CloseObserved()
			if closeErr != nil || closed == nil || closed.Validate(lease) != nil {
				t.Errorf("original close: %v", closeErr)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan InstallParsedManifestBypass, 1)
	allowReturn := make(chan struct{})
	exited := make(chan struct{})
	result := make(chan error, 1)
	var allowOnce sync.Once
	allow := func() { allowOnce.Do(func() { close(allowReturn) }) }
	// Join this original worker before the original lease's cleanup above.
	t.Cleanup(func() { allow(); <-exited })
	primary := errors.New("entered install refusal")
	go func() {
		defer close(exited)
		result <- lease.WithSupervisorLockBypass(t.Context(), root, func(token InstallParsedManifestBypass) error {
			entered <- token
			<-allowReturn
			return primary
		})
	}()
	var token InstallParsedManifestBypass
	select {
	case token = <-entered:
	case err := <-result:
		t.Fatalf("borrow did not enter: %v", err)
	}
	if !token.holdsSupervisorLock(root) {
		t.Fatal("entered original borrow is not held")
	}
	closed, closeErr := lease.CloseObserved()
	if closed != nil || !errors.Is(closeErr, ErrStateFileLeaseBorrowActive) || !lease.owner.Locked() {
		t.Fatalf("concurrent close unlocked beneath original borrow: proof=%v err=%v", closed, closeErr)
	}
	if token.holdsSupervisorLock(root) {
		t.Fatal("close-requested permit still authorizes a new check")
	}
	allow()
	<-exited
	if err := <-result; !errors.Is(err, primary) {
		t.Fatalf("original callback cause lost: %v", err)
	}
	closed, closeErr = lease.CloseObserved() // receive the original cached result
	if closeErr != nil || closed == nil || closed.Validate(lease) != nil {
		t.Fatalf("borrow did not receive pending close: %v", closeErr)
	}
	again, againErr := lease.CloseObserved()
	if again != closed || againErr != nil {
		t.Fatal("original close receipt was replaced or retried")
	}
	if token.holdsSupervisorLock(root) {
		t.Fatal("expired borrow regained authority after close")
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := lease.WithSupervisorLockBypass(cancelled, root, func(InstallParsedManifestBypass) error { t.Error("cancelled borrow entered"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled borrowed entry changed cause: %v", err)
	}
}
