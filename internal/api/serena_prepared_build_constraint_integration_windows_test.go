//go:build windows

package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Original production owners pin the fixed constraint around a real inert
// child. This fixture does not install Serena or issue publisher authority.
// It receives the attempt path from its original owner and keeps work bounded.
func TestPreparedSerenaBuildConstraintAttemptLifecycle(t *testing.T) {
	r := preparedRequest()
	r.Namespace = hardenedTempDir(t)
	r.WorkLimit, r.CleanupLimit = time.Minute, time.Minute
	r.WorkDeadline = time.Now().Add(r.WorkLimit)
	r.AbsoluteCleanupDeadline = r.WorkDeadline.Add(r.CleanupLimit)
	work, cancelWork := context.WithDeadline(context.Background(), r.WorkDeadline)
	defer cancelWork()
	key, err := r.Identity()
	if err != nil {
		t.Fatal(err)
	}
	executable, _, err := qualifyPreparedSerenaFile(work, os.Getenv("COMSPEC"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	a := &preparedSerenaAcquisition{inputs: SerenaPreparationInputs{
		Request: r, execution: SerenaPreparationEnvironment{buildConstraint: true},
	}}
	defer func() {
		if err := a.closeOriginalRoots(); err != nil {
			t.Error("original tree close did not complete:", err)
		}
	}()
	roots, err := a.acquireRoots(work, r, key)
	if err != nil {
		t.Fatal(err)
	}
	attemptPath, err := a.attempt.WorkingDirectory(work)
	if err != nil {
		t.Fatal(err)
	}
	constraint := a.buildConstraint
	if constraint == nil || constraint.tree != a.attempt ||
		roots.Attempt.Path != attemptPath ||
		constraint.file.Path != filepath.Join(roots.Attempt.Path, preparedSerenaBuildConstraintName) ||
		constraint.file.SHA256 != preparedSerenaHash([]byte(preparedSerenaBuildConstraintContent)) {
		t.Fatal("fixed constraint did not come from the original attempt")
	}
	if raw, err := os.ReadFile(constraint.file.Path); err != nil || string(raw) != preparedSerenaBuildConstraintContent {
		t.Fatal("original constraint bytes differ:", err)
	}
	checkPreparedSerenaBuildConstraintPin(t, constraint.file.Path, true)
	if err := beginPreparedSerenaBuildConstraintUse(work, constraint); err != nil {
		t.Fatal("original begin-use admission failed:", err)
	}
	// Keep the actual command result until finish; Closure alone cannot
	// reconstruct the original native/I/O settlement.
	result, runErr := runPreparedSerenaInvocation(work, preparedSerenaInvocation{
		OperationID: r.OperationID, Executable: executable,
		Args: []string{"/d", "/c", "exit", "/b", "0"}, Environment: os.Environ(),
		WorkingDir: roots.Generation.Path, PolicySHA256: r.PolicySHA256,
		CleanupLimit: r.CleanupLimit, AbsoluteCleanupDeadline: r.AbsoluteCleanupDeadline,
		StdoutLimit: 4096, StderrLimit: 4096,
	})
	finishErr := finishPreparedSerenaBuildConstraintUse(constraint, result)
	if runErr != nil || finishErr != nil || !result.containedAttempted ||
		!result.Receipt.NativeSettlement.Complete() || !result.Receipt.IOSettlement.Complete() ||
		!result.Closure.complete(r.OperationID) || !result.Closure.RootExitKnown || result.Closure.RootExitCode != 0 {
		t.Fatalf("original inert command or pin finish did not settle: run=%v finish=%v", runErr, finishErr)
	}
	checkPreparedSerenaBuildConstraintPin(t, constraint.file.Path, false)
	if raw, err := os.ReadFile(constraint.file.Path); err != nil || string(raw) != preparedSerenaBuildConstraintContent {
		t.Fatal("pin use changed the original constraint:", err)
	}
	// Keep the original bounded work context live while this fixture
	// receives cleanup and the surviving generation inventory.
	removed, err := a.cleanupAttempt(work, roots, result.Closure)
	if err != nil || !removed.AttemptAbsent || !removed.LinksDischarged || !removed.NoPendingResources {
		t.Fatal("original attempt cleanup failed:", err)
	}
	if _, err := os.Stat(roots.Attempt.Path); !os.IsNotExist(err) {
		t.Fatal("completed attempt remains:", err)
	}
	inventory, err := a.generation.Inventory(work)
	if err != nil || inventory.RootIdentity.Device != roots.Generation.Device || inventory.RootIdentity.FileID != roots.Generation.FileID {
		t.Fatal("surviving generation lost original identity:", err)
	}
}
