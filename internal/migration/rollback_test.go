package migration

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/scheduler"
)

// ---------------------------------------------------------------------------
// Rollback fixtures.
// ---------------------------------------------------------------------------

// setupCommittedJournalFixture creates a state-dir that already carries
// a committed migration journal — i.e. simulates the state immediately
// after a successful forward migration. The journal contains
// legacy-tasks/<task>.xml for two tasks, all four forward markers,
// classification + intent files, and supervisor-intent.json /
// supervisor-state.json in the state-dir.
func setupCommittedJournalFixture(t *testing.T) *testFixture {
	t.Helper()
	tx := setupV04xFixture(t)

	// Run a real forward migration to populate the journal end-to-end.
	if err := RunForward(tx.State, fakeForwardOptions(t, tx)); err != nil {
		t.Fatalf("seed forward: %v", err)
	}
	// Re-zero scheduler tracking so rollback assertions can count only
	// rollback-side calls.
	tx.Scheduler.DeletedTasks = nil
	tx.Scheduler.CreatedTasks = nil
	tx.Scheduler.RunTasks = nil
	tx.Scheduler.CreateXMLPayloads = make(map[string]string)
	return tx
}

// setupPartialJournalFixture creates a journal with markers up to and
// including `lastMarker` (and the prior markers).
func setupPartialJournalFixture(t *testing.T, lastMarker string) *testFixture {
	t.Helper()
	tx := setupV04xFixture(t)
	// Mint a journal dir + touch the requested marker chain.
	journalDir := tx.State.journalDirForTime(tx.State.Now())
	if err := os.MkdirAll(journalDir, 0700); err != nil {
		t.Fatal(err)
	}
	// Touch markers up to lastMarker inclusive.
	for _, m := range allForwardMarkers {
		if err := touchMarker(journalDir, m); err != nil {
			t.Fatal(err)
		}
		if m == lastMarker {
			break
		}
	}
	// Always populate one legacy-tasks XML so rollback has something
	// to restore.
	legacyDir := filepath.Join(journalDir, "legacy-tasks")
	if err := os.MkdirAll(legacyDir, 0700); err != nil {
		t.Fatal(err)
	}
	xmlBody := cleanV04xXML(t, "memory", "default", tx.CurrentUser, tx.State.InstallDir)
	// Inject the URI element so extractTag finds the canonical task name.
	xmlBody = strings.Replace(xmlBody, "<Description>memory daemon for default</Description>",
		"<URI>\\mcp-local-hub-memory-default</URI><Description>memory daemon for default</Description>", 1)
	if err := os.WriteFile(filepath.Join(legacyDir, "mcp-local-hub-memory-default.xml"), []byte(xmlBody), 0600); err != nil {
		t.Fatal(err)
	}
	return tx
}

// fakeRollbackOptions wires the rollback callbacks to the fixture.
func fakeRollbackOptions(t *testing.T, tx *testFixture) RollbackOptions {
	t.Helper()
	return RollbackOptions{
		Scheduler: tx.Scheduler,
		SupervisorIPC: func(cmd string, args map[string]any, timeout time.Duration) error {
			return tx.IPC.Send(cmd, args, timeout)
		},
		ProbeSupervisorTokenMismatch: func() error { return nil },
		ForceKillSupervisor: func() error {
			tx.forceKillCalled++
			return nil
		},
		PortBindWait: func(port int, timeout time.Duration) error {
			tx.portWaitMu.Lock()
			defer tx.portWaitMu.Unlock()
			if tx.portWaitIdx < len(tx.portWaitReturns) {
				err := tx.portWaitReturns[tx.portWaitIdx]
				tx.portWaitIdx++
				return err
			}
			return nil
		},
		PortBindWaitBound: func(port int, timeout time.Duration) error {
			tx.portWaitBoundMu.Lock()
			defer tx.portWaitBoundMu.Unlock()
			if tx.portWaitBoundIdx < len(tx.portWaitBoundReturns) {
				err := tx.portWaitBoundReturns[tx.portWaitBoundIdx]
				tx.portWaitBoundIdx++
				return err
			}
			return nil
		},
		LookupProcessIdentity: func(pid int) (ProcessIdentity, error) {
			if id, ok := tx.identityByPID[pid]; ok {
				return id, nil
			}
			return ProcessIdentity{}, errors.New("not found")
		},
		QuarantineTranslator: func(_ State) error {
			tx.quarantineCalled++
			return nil
		},
		ShimUninstaller: func() error {
			tx.shimUninstalled++
			return nil
		},
		TimeWaitSettle: 10 * time.Millisecond, // fast test
	}
}

// ---------------------------------------------------------------------------
// Rollback tests.
// ---------------------------------------------------------------------------

// TestRollback_RestoresLegacyTasks runs a forward+rollback round trip.
// After rollback: supervisor-intent.json removed, fakeScheduler recorded
// CreateXML calls for the legacy tasks, Run was called for each.
func TestRollback_RestoresLegacyTasks(t *testing.T) {
	tx := setupCommittedJournalFixture(t)

	opts := fakeRollbackOptions(t, tx)
	// Tell rollback what daemons to expect (read from the committed intent).
	intent, err := api.ReadSupervisorIntent(filepath.Join(tx.State.StateDir, "supervisor-intent.json"))
	if err != nil {
		t.Fatalf("read intent: %v", err)
	}
	opts.ExpectedDaemons = intent.Daemons

	if err := RunRollback(tx.State, opts); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tx.State.StateDir, "supervisor-intent.json")); !os.IsNotExist(err) {
		t.Fatal("supervisor-intent.json not deleted on rollback")
	}
	if len(tx.Scheduler.CreatedTasks) == 0 {
		t.Fatal("legacy tasks not re-registered")
	}
	if len(tx.Scheduler.RunTasks) == 0 {
		t.Fatal("legacy tasks not invoked via Run")
	}
	// Quarantine translator + shim uninstaller fired.
	if tx.quarantineCalled != 1 {
		t.Fatalf("quarantineTranslator: want 1 call, got %d", tx.quarantineCalled)
	}
	if tx.shimUninstalled != 1 {
		t.Fatalf("shimUninstaller: want 1 call, got %d", tx.shimUninstalled)
	}
}

// TestRollback_ExitCode13TokenMismatch: token probe returns access-
// denied → exit code 13.
func TestRollback_ExitCode13TokenMismatch(t *testing.T) {
	tx := setupCommittedJournalFixture(t)

	opts := fakeRollbackOptions(t, tx)
	opts.ProbeSupervisorTokenMismatch = func() error {
		return errors.New("OpenProcess: ERROR_ACCESS_DENIED")
	}
	err := RunRollback(tx.State, opts)
	if err == nil {
		t.Fatal("expected token-mismatch error, got nil")
	}
	var ec *ExitCodeError
	if !errors.As(err, &ec) {
		t.Fatalf("expected *ExitCodeError, got: %T %v", err, err)
	}
	if ec.Code != ExitRollbackTokenMismatch {
		t.Fatalf("expected exit %d, got %d", ExitRollbackTokenMismatch, ec.Code)
	}
}

// TestRollback_WarningsExitZero: port-bind fails for one task → warning
// recorded, RunRollback returns nil (exit 0).
func TestRollback_WarningsExitZero(t *testing.T) {
	tx := setupCommittedJournalFixture(t)
	intent, _ := api.ReadSupervisorIntent(filepath.Join(tx.State.StateDir, "supervisor-intent.json"))
	// Assign a non-zero port to one daemon so the wait actually runs.
	for i := range intent.Daemons {
		intent.Daemons[i].Port = 9128 + i
	}

	opts := fakeRollbackOptions(t, tx)
	opts.ExpectedDaemons = intent.Daemons
	opts.PortBindTimeout = 50 * time.Millisecond

	// Step 3 (wait-until-unbound) and step 10 (wait-until-BOUND) now
	// have separate hooks (PortBindWait vs. PortBindWaitBound). Step 3
	// should pass — leave portWaitReturns empty so the fake defaults
	// to nil. Step 10's FIRST call returns "bind timeout" → warning.
	tx.portWaitBoundReturns = append(tx.portWaitBoundReturns, errors.New("bind timeout"))

	if err := RunRollback(tx.State, opts); err != nil {
		t.Fatalf("rollback: want nil with warnings, got: %v", err)
	}
	// Find the journal dir.
	journalDir, _ := FindLatestJournal(tx.State.StateDir)
	raw, err := os.ReadFile(filepath.Join(journalDir, "rollback-warnings.json"))
	if err != nil {
		t.Fatalf("rollback-warnings.json missing: %v", err)
	}
	var w rollbackWarningsFile
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatalf("parse warnings: %v", err)
	}
	if w.Version != 1 {
		t.Fatalf("warnings version: want 1, got %d", w.Version)
	}
	if len(w.Warnings) == 0 {
		t.Fatal("expected at least one warning entry")
	}
	// reason starts with the canonical "port-not-bound-after-" prefix.
	if !strings.HasPrefix(w.Warnings[0].Reason, "port-not-bound-after-") {
		t.Fatalf("unexpected warning reason: %s", w.Warnings[0].Reason)
	}
}

// TestRollback_OrphanDaemonsRemain: step-3 port verification times out
// → ErrRollbackOrphanDaemonsRemain.
func TestRollback_OrphanDaemonsRemain(t *testing.T) {
	tx := setupCommittedJournalFixture(t)
	intent, _ := api.ReadSupervisorIntent(filepath.Join(tx.State.StateDir, "supervisor-intent.json"))
	for i := range intent.Daemons {
		intent.Daemons[i].Port = 9200 + i
	}
	opts := fakeRollbackOptions(t, tx)
	opts.ExpectedDaemons = intent.Daemons
	// First (step-3) port wait fails — orphan daemon path.
	tx.portWaitReturns = []error{errors.New("still bound after 10s")}

	// Force exit{graceful} to time out so force-kill fires.
	tx.IPC.Returns["exit"] = errors.New("timeout")

	err := RunRollback(tx.State, opts)
	if err == nil {
		t.Fatal("expected orphan-daemons error, got nil")
	}
	if !errors.Is(err, ErrRollbackOrphanDaemonsRemain) {
		t.Fatalf("expected ErrRollbackOrphanDaemonsRemain, got: %v", err)
	}
}

// TestRollback_DeletesStateFiles: supervisor-state.json /
// supervisor-events.log are deleted at step 11 even when they pre-exist.
func TestRollback_DeletesStateFiles(t *testing.T) {
	tx := setupCommittedJournalFixture(t)
	// Pre-write supervisor-state.json + supervisor-events.log.
	for _, fname := range []string{"supervisor-state.json", "supervisor-events.log"} {
		_ = os.WriteFile(filepath.Join(tx.State.StateDir, fname), []byte("seed"), 0600)
	}

	opts := fakeRollbackOptions(t, tx)
	intent, _ := api.ReadSupervisorIntent(filepath.Join(tx.State.StateDir, "supervisor-intent.json"))
	opts.ExpectedDaemons = intent.Daemons

	if err := RunRollback(tx.State, opts); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	for _, fname := range []string{"supervisor-state.json", "supervisor-events.log", "supervisor-intent.json"} {
		if _, err := os.Stat(filepath.Join(tx.State.StateDir, fname)); !os.IsNotExist(err) {
			t.Fatalf("%s should have been deleted", fname)
		}
	}
}

// TestRollback_RemovesRollbackInProgressMarker: step 12 cleanup.
func TestRollback_RemovesRollbackInProgressMarker(t *testing.T) {
	tx := setupCommittedJournalFixture(t)
	opts := fakeRollbackOptions(t, tx)
	intent, _ := api.ReadSupervisorIntent(filepath.Join(tx.State.StateDir, "supervisor-intent.json"))
	opts.ExpectedDaemons = intent.Daemons

	if err := RunRollback(tx.State, opts); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	journalDir, _ := FindLatestJournal(tx.State.StateDir)
	if _, err := os.Stat(filepath.Join(journalDir, MarkerRollbackInProgress)); !os.IsNotExist(err) {
		t.Fatal("rollback-in-progress marker should be deleted")
	}
}

// TestRollback_LegacyDirUnreadableAborts pins codex r2 Lane C P0 (fix
// 1): when os.ReadDir(legacyDir) returns a non-IsNotExist error
// (permission denied, ENOTDIR, corrupt entry), rollback MUST abort
// with a corrupt-journal error. The previous behavior treated ANY
// ReadDir error as "zero-daemon migration" when the committed marker
// was present, which would have silently deleted supervisor state
// without proving the legacy scheduler tasks could be replayed.
//
// Implementation note: the test injects the unreadable-dir error via
// the rollbackReadLegacyDirFn package-level seam because
// reproducing a portable non-IsNotExist ReadDir error across POSIX
// (where chmod 0 + ENOTDIR are easy) and Windows (where the analogous
// constructs map back to IsNotExist) requires either elevated ACL
// manipulation or platform-conditional skips. Injecting at the seam
// keeps the test deterministic on every supported OS.
func TestRollback_LegacyDirUnreadableAborts(t *testing.T) {
	tx := setupCommittedJournalFixture(t)
	intent, _ := api.ReadSupervisorIntent(filepath.Join(tx.State.StateDir, "supervisor-intent.json"))
	opts := fakeRollbackOptions(t, tx)
	opts.ExpectedDaemons = intent.Daemons

	// Sanity: committed marker IS present (the prior failure mode
	// silently accepted any-error purely because committed was set).
	journalDir, err := FindLatestJournal(tx.State.StateDir)
	if err != nil {
		t.Fatalf("find journal: %v", err)
	}
	if !markerExists(journalDir, MarkerCommitted) {
		t.Fatal("setup invariant: committed marker should exist")
	}

	// Inject a permission-denied ReadDir result via the seam. The
	// production code MUST reject this (only IsNotExist is accepted
	// alongside committed).
	origReader := rollbackReadLegacyDirFn
	t.Cleanup(func() { rollbackReadLegacyDirFn = origReader })
	injectedErr := &os.PathError{Op: "open", Path: filepath.Join(journalDir, "legacy-tasks"), Err: os.ErrPermission}
	rollbackReadLegacyDirFn = func(path string) ([]os.DirEntry, error) {
		return nil, injectedErr
	}

	err = RunRollback(tx.State, opts)
	if err == nil {
		t.Fatal("expected error from unreadable legacy-tasks/, got nil")
	}
	if !strings.Contains(err.Error(), "legacy-tasks dir unreadable") {
		t.Errorf("error should name the corrupt-journal cause; got %v", err)
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("error should wrap the underlying permission failure; got %v", err)
	}
	// Verify the rollback aborted BEFORE deleting supervisor state.
	for _, fname := range []string{"supervisor-intent.json"} {
		if _, statErr := os.Stat(filepath.Join(tx.State.StateDir, fname)); statErr != nil {
			t.Errorf("%s deleted on abort — rollback should not have reached step 11; got err %v", fname, statErr)
		}
	}
}

// TestRollback_LegacyDirMissingAfterCommittedAcceptedAsZeroDaemon pins
// codex r2 Lane C P0 (fix 1): the ONLY accepted ReadDir error is
// os.IsNotExist + committed marker present. That pair means a clean
// zero-daemon migration committed without ever creating
// legacy-tasks/. RunRollback must succeed, record a warning, and
// proceed through state-file cleanup.
func TestRollback_LegacyDirMissingAfterCommittedAcceptedAsZeroDaemon(t *testing.T) {
	tx := setupCommittedJournalFixture(t)
	intent, _ := api.ReadSupervisorIntent(filepath.Join(tx.State.StateDir, "supervisor-intent.json"))
	opts := fakeRollbackOptions(t, tx)
	opts.ExpectedDaemons = nil // zero-daemon path: no ports to verify at step 3
	_ = intent

	// Delete the legacy-tasks directory entirely so os.ReadDir
	// returns os.IsNotExist. The committed marker stays in place.
	journalDir, err := FindLatestJournal(tx.State.StateDir)
	if err != nil {
		t.Fatalf("find journal: %v", err)
	}
	legacyDir := filepath.Join(journalDir, "legacy-tasks")
	if err := os.RemoveAll(legacyDir); err != nil {
		t.Fatalf("remove legacy dir: %v", err)
	}
	if !markerExists(journalDir, MarkerCommitted) {
		t.Fatal("setup invariant: committed marker should exist")
	}

	if err := RunRollback(tx.State, opts); err != nil {
		t.Fatalf("rollback: zero-daemon committed migration should succeed; got %v", err)
	}
	// Warning recorded for the zero-daemon edge case.
	raw, err := os.ReadFile(filepath.Join(journalDir, "rollback-warnings.json"))
	if err != nil {
		t.Fatalf("rollback-warnings.json missing: %v", err)
	}
	var w rollbackWarningsFile
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatalf("parse warnings: %v", err)
	}
	if len(w.Warnings) == 0 {
		t.Fatal("expected at least one warning entry for zero-daemon committed migration")
	}
	foundZeroDaemonReason := false
	for _, entry := range w.Warnings {
		if strings.Contains(entry.Reason, "zero-daemon migration") {
			foundZeroDaemonReason = true
			break
		}
	}
	if !foundZeroDaemonReason {
		t.Errorf("expected a zero-daemon warning entry; got %+v", w.Warnings)
	}
	// supervisor-intent.json was cleaned up at step 11.
	if _, err := os.Stat(filepath.Join(tx.State.StateDir, "supervisor-intent.json")); !os.IsNotExist(err) {
		t.Errorf("supervisor-intent.json should be deleted on zero-daemon committed rollback")
	}
}

// TestRunRollback_PortBindWaitBoundSemantic pins codex r2 Lanes C+F P1
// (fix 2): step 10 must wire through RollbackOptions.PortBindWaitBound
// (NEW field — wait-until-BOUND), NOT RollbackOptions.PortBindWait
// (which is wait-until-UNBOUND for step 3). The two have opposite
// semantics and the previous wiring sent the step-10 wait through the
// step-3 helper, which would have returned "success" the moment the
// post-kill window closed — i.e., before any restored daemon could
// possibly have bound.
//
// Assertions: PortBindWaitBound IS called at step 10 for every
// restored task with a non-zero port; PortBindWait is NOT called at
// step 10 (only at step 3); a bound-wait error lands in
// rollback-warnings.json under the wait-not-bound reason.
func TestRunRollback_PortBindWaitBoundSemantic(t *testing.T) {
	tx := setupCommittedJournalFixture(t)
	intent, _ := api.ReadSupervisorIntent(filepath.Join(tx.State.StateDir, "supervisor-intent.json"))
	// Assign non-zero ports so both step-3 and step-10 wait loops
	// have something to iterate over.
	for i := range intent.Daemons {
		intent.Daemons[i].Port = 9300 + i
	}

	opts := fakeRollbackOptions(t, tx)
	opts.ExpectedDaemons = intent.Daemons
	opts.PortBindTimeout = 50 * time.Millisecond

	// Track which hook was called with which port + timeout.
	var unboundCalls []struct {
		port    int
		timeout time.Duration
	}
	var boundCalls []struct {
		port    int
		timeout time.Duration
	}
	opts.PortBindWait = func(port int, timeout time.Duration) error {
		unboundCalls = append(unboundCalls, struct {
			port    int
			timeout time.Duration
		}{port, timeout})
		return nil // step 3 ports are unbound
	}
	opts.PortBindWaitBound = func(port int, timeout time.Duration) error {
		boundCalls = append(boundCalls, struct {
			port    int
			timeout time.Duration
		}{port, timeout})
		// First step-10 call fails — exercises the warning path
		// AND confirms the new hook is in use (the old hook would
		// have received this error).
		if len(boundCalls) == 1 {
			return errors.New("daemon did not bind")
		}
		return nil
	}

	if err := RunRollback(tx.State, opts); err != nil {
		t.Fatalf("rollback: want nil with warnings, got: %v", err)
	}
	// Step 3 (wait-until-unbound) called for every expected daemon.
	if len(unboundCalls) != len(intent.Daemons) {
		t.Errorf("PortBindWait (step 3) call count = %d; want %d", len(unboundCalls), len(intent.Daemons))
	}
	for _, c := range unboundCalls {
		if c.timeout != 10*time.Second {
			t.Errorf("PortBindWait step-3 timeout = %s; want 10s (rollback hard budget)", c.timeout)
		}
	}
	// Step 10 (wait-until-BOUND) called for every restored task.
	if len(boundCalls) == 0 {
		t.Fatal("PortBindWaitBound was never called — production must be wired to the new hook for step 10")
	}
	for _, c := range boundCalls {
		if c.timeout != opts.PortBindTimeout {
			t.Errorf("PortBindWaitBound step-10 timeout = %s; want %s", c.timeout, opts.PortBindTimeout)
		}
	}
	// Warning for the first failing bound-wait.
	journalDir, _ := FindLatestJournal(tx.State.StateDir)
	raw, err := os.ReadFile(filepath.Join(journalDir, "rollback-warnings.json"))
	if err != nil {
		t.Fatalf("rollback-warnings.json missing: %v", err)
	}
	var w rollbackWarningsFile
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatalf("parse warnings: %v", err)
	}
	foundPortWarning := false
	for _, entry := range w.Warnings {
		if strings.HasPrefix(entry.Reason, "port-not-bound-after-") {
			foundPortWarning = true
			break
		}
	}
	if !foundPortWarning {
		t.Errorf("expected port-not-bound warning; got %+v", w.Warnings)
	}
}

// ---------------------------------------------------------------------------
// Resume classification tests.
// ---------------------------------------------------------------------------

// TestResume_PreOsMutatingMarker matches the verbatim plan contract:
// pre-os-mutating present → operator-choice-forward-or-rollback.
func TestResume_PreOsMutatingMarker(t *testing.T) {
	tx := setupPartialJournalFixture(t, MarkerPreOsMutating)
	journalDir, _ := FindLatestJournal(tx.State.StateDir)
	verdict := ClassifyResume(journalDir)
	if verdict.Action != "operator-choice-forward-or-rollback" {
		t.Fatalf("got %s", verdict.Action)
	}
}

// TestResume_PreparedOnly: prepared marker only → safe-abort-delete-journal.
func TestResume_PreparedOnly(t *testing.T) {
	tx := setupPartialJournalFixture(t, MarkerPrepared)
	journalDir, _ := FindLatestJournal(tx.State.StateDir)
	verdict := ClassifyResume(journalDir)
	if verdict.Action != "safe-abort-delete-journal" {
		t.Fatalf("got %s", verdict.Action)
	}
}

// TestResume_OsMutatingComplete: os-mutating-complete (no committed) →
// operator-choice-forward-or-rollback.
func TestResume_OsMutatingComplete(t *testing.T) {
	tx := setupPartialJournalFixture(t, MarkerOsMutatingComplete)
	journalDir, _ := FindLatestJournal(tx.State.StateDir)
	verdict := ClassifyResume(journalDir)
	if verdict.Action != "operator-choice-forward-or-rollback" {
		t.Fatalf("got %s", verdict.Action)
	}
}

// TestResume_Committed: committed marker → already-committed.
func TestResume_Committed(t *testing.T) {
	tx := setupPartialJournalFixture(t, MarkerCommitted)
	journalDir, _ := FindLatestJournal(tx.State.StateDir)
	verdict := ClassifyResume(journalDir)
	if verdict.Action != "already-committed-no-resume-needed" {
		t.Fatalf("got %s", verdict.Action)
	}
}

// TestResume_RollbackInProgress: rollback-in-progress marker → rollback-must-complete.
func TestResume_RollbackInProgress(t *testing.T) {
	tx := setupPartialJournalFixture(t, MarkerCommitted)
	journalDir, _ := FindLatestJournal(tx.State.StateDir)
	// Also touch rollback-in-progress.
	if err := touchMarker(journalDir, MarkerRollbackInProgress); err != nil {
		t.Fatal(err)
	}
	verdict := ClassifyResume(journalDir)
	if verdict.Action != "rollback-must-complete" {
		t.Fatalf("got %s", verdict.Action)
	}
	// Markers list includes rollback-in-progress + the forward marker.
	foundRollback := false
	foundCommitted := false
	for _, m := range verdict.Markers {
		if m == MarkerRollbackInProgress {
			foundRollback = true
		}
		if m == MarkerCommitted {
			foundCommitted = true
		}
	}
	if !foundRollback || !foundCommitted {
		t.Fatalf("expected both rollback-in-progress and committed in markers: %v", verdict.Markers)
	}
}

// TestResume_NoJournal: missing dir → no-journal verdict.
func TestResume_NoJournal(t *testing.T) {
	verdict := ClassifyResume("")
	if verdict.Action != "no-journal" {
		t.Fatalf("empty path: got %s", verdict.Action)
	}
	verdict = ClassifyResume(filepath.Join(t.TempDir(), "nonexistent"))
	if verdict.Action != "no-journal" {
		t.Fatalf("nonexistent path: got %s", verdict.Action)
	}
}

// TestResume_EmptyDirNoMarkers: dir exists but contains no markers →
// no-journal verdict (so a stray empty dir does not block classification).
func TestResume_EmptyDirNoMarkers(t *testing.T) {
	dir := t.TempDir()
	verdict := ClassifyResume(dir)
	if verdict.Action != "no-journal" {
		t.Fatalf("empty dir: got %s", verdict.Action)
	}
}

// ---------------------------------------------------------------------------
// Bonus: lock-set sanity tests (the lock-acquire helper file).
// ---------------------------------------------------------------------------

// TestLockSet_LIFORelease verifies Release() unwinds both locks in
// LIFO order without panicking.
func TestLockSet_LIFORelease(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	ls, err := AcquireMigrationLocks(dir)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	ls.Release()
	// Idempotent re-release.
	ls.Release()
}

// TestLockSet_DoubleAcquireRejected verifies the second acquire fails
// with ErrMigrationLockHeld.
func TestLockSet_DoubleAcquireRejected(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	ls1, err := AcquireMigrationLocks(dir)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer ls1.Release()

	_, err = AcquireMigrationLocks(dir)
	if err == nil {
		t.Fatal("expected second acquire to fail")
	}
	if !errors.Is(err, ErrMigrationLockHeld) {
		t.Fatalf("expected ErrMigrationLockHeld, got: %v", err)
	}
}

// TestLockSet_OnceLockHeldUnwinds verifies migration.lock is released
// when --once.lock acquisition fails.
func TestLockSet_OnceLockHeldUnwinds(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	// Pre-acquire --once.lock to force the second-step failure.
	preOnce, err := api.AcquireSupervisorLock(filepath.Join(dir, "--once"))
	if err != nil {
		t.Fatalf("pre-acquire once: %v", err)
	}
	defer preOnce.Release()

	_, err = AcquireMigrationLocks(dir)
	if err == nil {
		t.Fatal("expected --once.lock-held error")
	}
	if !errors.Is(err, ErrOnceLockHeld) {
		t.Fatalf("expected ErrOnceLockHeld, got: %v", err)
	}
	// Verify migration.lock is now free (would have been released on
	// unwind).
	mig, err := api.AcquireSupervisorLock(filepath.Join(dir, "migration"))
	if err != nil {
		t.Fatalf("migration.lock should be free after unwind: %v", err)
	}
	mig.Release()
}

// silence unused-import warnings when the scheduler package is only
// referenced via SchedulerBackend interface satisfaction.
var _ = scheduler.ErrTaskNotFound
