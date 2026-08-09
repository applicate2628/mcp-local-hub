package api

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestRegistrationTransaction_RunsAllCompensationsLIFOAndJoinsErrors(t *testing.T) {
	primaryErr := errors.New("primary")
	undoAErr := errors.New("undo-a")
	undoBErr := errors.New("undo-b")
	closeAErr := errors.New("close-a")
	closeBErr := errors.New("close-b")
	var calls []string

	tx := newRegistrationTransaction()
	tx.AddCompensation("a", func() error {
		calls = append(calls, "undo-a")
		return undoAErr
	})
	tx.AddCompensation("b", func() error {
		calls = append(calls, "undo-b")
		return undoBErr
	})
	tx.AddFinalizer("a", func() error {
		calls = append(calls, "close-a")
		return closeAErr
	})
	tx.AddFinalizer("b", func() error {
		calls = append(calls, "close-b")
		return closeBErr
	})

	outcome := tx.Fail(primaryErr)
	if outcome.State != registrationTransactionRolledBack {
		t.Fatalf("state = %q, want rolled-back", outcome.State)
	}
	for _, want := range []error{primaryErr, undoAErr, undoBErr, closeAErr, closeBErr} {
		if !errors.Is(outcome.Err, want) {
			t.Errorf("outcome error does not retain %v: %v", want, outcome.Err)
		}
	}
	wantCalls := []string{"undo-b", "undo-a", "close-b", "close-a"}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", calls, wantCalls)
	}
}

func TestRegistrationTransaction_CommitFinalizerFailureRollsBack(t *testing.T) {
	closeErr := errors.New("close")
	var calls []string
	tx := newRegistrationTransaction()
	tx.AddCompensation("state", func() error {
		calls = append(calls, "undo")
		return nil
	})
	tx.AddFinalizer("lease", func() error {
		calls = append(calls, "close")
		return closeErr
	})

	outcome := tx.Commit()
	if outcome.State != registrationTransactionRolledBack || !errors.Is(outcome.Err, closeErr) {
		t.Fatalf("outcome = %+v, want rolled-back close failure", outcome)
	}
	if want := []string{"close", "undo"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestRegistrationTransaction_CommitForwardRetainsFinalizerErrorWithoutCompensation(t *testing.T) {
	tx := newRegistrationTransaction()
	compensationCalls := 0
	var observers []string
	closeErr := errors.New("forward finalizer release failed")
	tx.AddCompensation("active-a", func() error {
		compensationCalls++
		return nil
	})
	tx.AddCompensation("active-b", func() error {
		compensationCalls++
		return nil
	})
	tx.AddFinalizer("forward resource", func() error { return closeErr })
	tx.AddAfterCommit("forward observer a", func() error {
		observers = append(observers, "a")
		return nil
	})
	tx.AddAfterCommit("forward observer b", func() error {
		observers = append(observers, "b")
		return nil
	})

	outcome := tx.CommitForward()
	if outcome.State != registrationTransactionCommitted || !errors.Is(outcome.Err, closeErr) {
		t.Fatalf("CommitForward outcome = %+v, want committed with finalizer error", outcome)
	}
	if compensationCalls != 0 {
		t.Fatalf("CommitForward ran %d active compensations, want zero", compensationCalls)
	}
	if want := []string{"a", "b"}; !reflect.DeepEqual(observers, want) {
		t.Fatalf("CommitForward observers = %v, want %v", observers, want)
	}
}

func TestRegistrationTransaction_PostCommitObserversRunAllAndJoinErrors(t *testing.T) {
	observerAErr := errors.New("observer-a")
	observerCErr := errors.New("observer-c")
	var calls []string
	tx := newRegistrationTransaction()
	tx.AddAfterCommit("a", func() error {
		calls = append(calls, "a")
		return observerAErr
	})
	tx.AddAfterCommit("b", func() error {
		calls = append(calls, "b")
		return nil
	})
	tx.AddAfterCommit("c", func() error {
		calls = append(calls, "c")
		return observerCErr
	})

	outcome := tx.Commit()
	if !outcome.Committed() || outcome.Err != nil {
		t.Fatalf("outcome = %+v, want committed state", outcome)
	}
	for _, want := range []error{observerAErr, observerCErr} {
		if !errors.Is(outcome.ObserverErr, want) {
			t.Errorf("observer error does not retain %v: %v", want, outcome.ObserverErr)
		}
	}
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("observer calls = %v, want %v", calls, want)
	}
}

type deterministicFailingWriter struct {
	err    error
	writes int
	calls  *[]string
}

func (w *deterministicFailingWriter) Write([]byte) (int, error) {
	w.writes++
	if w.calls != nil {
		*w.calls = append(*w.calls, "failing-writer")
	}
	return 0, w.err
}

func TestRegistrationTransaction_PostCommitWriterFailureKeepsCommittedStateAndContinuesObservers(t *testing.T) {
	writerErr := errors.New("injected post-commit writer failure")
	var calls []string
	compensationCalls := 0
	finalizerCalls := 0

	tx := newRegistrationTransaction()
	tx.AddCompensation("must remain disarmed after commit", func() error {
		compensationCalls++
		calls = append(calls, "compensation")
		return nil
	})
	tx.AddFinalizer("release before post-commit observers", func() error {
		finalizerCalls++
		calls = append(calls, "finalizer")
		return nil
	})
	tx.AddAfterCommit("observer before failing writer", func() error {
		calls = append(calls, "before")
		return nil
	})
	failing := &deterministicFailingWriter{err: writerErr, calls: &calls}
	tx.AddSuccessOutput("operator output", failing, "committed output\n")
	tx.AddAfterCommit("observer after failing writer", func() error {
		calls = append(calls, "after")
		return nil
	})
	var laterOutput bytes.Buffer
	tx.AddSuccessOutput("later operator output", &laterOutput, "later output\n")
	tx.AddAfterCommit("tail observer", func() error {
		calls = append(calls, "tail")
		return nil
	})

	outcome := tx.Commit()
	if !outcome.Committed() || outcome.State != registrationTransactionCommitted || outcome.Err != nil {
		t.Fatalf("outcome = %+v, want committed state with no settlement error", outcome)
	}
	if !errors.Is(outcome.ObserverErr, writerErr) {
		t.Fatalf("observer error %v does not retain writer failure", outcome.ObserverErr)
	}
	if got := outcome.ObserverErr.Error(); !strings.Contains(got, "after-commit observer operator output") {
		t.Fatalf("observer error %q does not identify the failing output participant", got)
	}
	if failing.writes != 1 {
		t.Fatalf("failing writer calls = %d, want exactly 1", failing.writes)
	}
	if laterOutput.String() != "later output\n" {
		t.Fatalf("later output = %q, want sibling output after writer failure", laterOutput.String())
	}
	if compensationCalls != 0 {
		t.Fatalf("post-commit writer failure re-entered compensation %d time(s)", compensationCalls)
	}
	if finalizerCalls != 1 {
		t.Fatalf("finalizer calls = %d, want exactly 1", finalizerCalls)
	}
	if want := []string{"finalizer", "before", "failing-writer", "after", "tail"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	if tx.state != registrationTransactionCommitted ||
		len(tx.steps) != 0 || len(tx.resources) != 0 || len(tx.observers) != 0 {
		t.Fatalf(
			"transaction cleanup after observer failure: state=%q steps=%d resources=%d observers=%d",
			tx.state,
			len(tx.steps),
			len(tx.resources),
			len(tx.observers),
		)
	}
}

func TestRegisterCommittedObservation_FailureInjectionMatrix(t *testing.T) {
	successClasses := []string{
		"cleanup-backup",
		"cleanup-removal",
		"client-entry-written",
		"scheduler-task-created",
		"scheduler-task-started",
		"supervised-proxy-started",
	}
	for _, failureAfter := range successClasses {
		t.Run(failureAfter, func(t *testing.T) {
			var output bytes.Buffer
			observed := 0
			tx := newRegistrationTransaction()
			for _, class := range successClasses {
				class := class
				tx.AddSuccessOutput(class, &output, "success:%s\n", class)
				tx.AddAfterCommit("audit "+class, func() error {
					observed++
					return nil
				})
				if class == failureAfter {
					break
				}
			}
			injected := errors.New("injected failure after " + failureAfter)
			outcome := tx.Fail(injected)
			if !errors.Is(outcome.Err, injected) {
				t.Fatalf("outcome error = %v, want injected failure", outcome.Err)
			}
			if output.Len() != 0 || observed != 0 {
				t.Fatalf("rollback exposed committed observations: output=%q audits=%d", output.String(), observed)
			}
		})
	}
}

func TestRegistrationTransaction_RegistryReleaseFailureFailsClosed(t *testing.T) {
	releaseErr := errors.New("registry release")
	var releaseCalls, undoCalls int
	tx := newRegistrationTransaction()
	tx.AddCompensation("registry row", func() error {
		undoCalls++
		return nil
	})
	tx.AddFinalizer("registry lock", func() error {
		releaseCalls++
		return releaseErr
	})

	outcome := tx.Commit()
	if outcome.State != registrationTransactionRolledBack || !errors.Is(outcome.Err, releaseErr) {
		t.Fatalf("outcome = %+v, want release failure with rolled-back state", outcome)
	}
	if releaseCalls != 1 || undoCalls != 1 {
		t.Fatalf("release/undo calls = %d/%d, want exactly 1/1", releaseCalls, undoCalls)
	}
}

func TestSupervisorIntentUndo_AllCallersPropagateErrors(t *testing.T) {
	causeErr := errors.New("injected downstream registration failure")
	undoErr := errors.New("injected supervisor-intent undo failure")

	for _, caller := range []string{
		"supervised-register",
		"ensure-existing-promotion",
		"ensure-new-row",
	} {
		t.Run(caller, func(t *testing.T) {
			transaction := newRegistrationTransaction()
			calls := 0
			enrollSupervisorIntentUndo(transaction, "restore "+caller+" supervisor intent", func() error {
				calls++
				return undoErr
			})

			outcome := transaction.Fail(causeErr)
			if calls != 1 {
				t.Fatalf("undo calls = %d, want 1", calls)
			}
			if !errors.Is(outcome.Err, causeErr) {
				t.Fatalf("transaction error %v does not retain downstream cause", outcome.Err)
			}
			if !errors.Is(outcome.Err, undoErr) {
				t.Fatalf("transaction error %v does not retain supervisor-intent undo failure", outcome.Err)
			}
		})
	}
}

type fakeRegistryRollbackStore struct {
	lockErr    error
	locked     bool
	loadErr    error
	putErr     error
	saveErr    error
	releaseErr error
}

func (s *fakeRegistryRollbackStore) TryLock() (func() error, bool, error) {
	if s.lockErr != nil {
		return nil, false, s.lockErr
	}
	return func() error { return s.releaseErr }, s.locked, nil
}
func (s *fakeRegistryRollbackStore) Load() error                 { return s.loadErr }
func (s *fakeRegistryRollbackStore) PutLSP(WorkspaceEntry) error { return s.putErr }
func (s *fakeRegistryRollbackStore) Remove(string, string)       {}
func (s *fakeRegistryRollbackStore) Save() error                 { return s.saveErr }

func TestRemoveLSPRegistryRow_ReturnsLockLoadSaveReleaseErrors(t *testing.T) {
	lockErr := errors.New("lock")
	loadErr := errors.New("load")
	saveErr := errors.New("save")
	releaseErr := errors.New("release")
	tests := []struct {
		name  string
		store *fakeRegistryRollbackStore
		want  []error
	}{
		{name: "lock", store: &fakeRegistryRollbackStore{lockErr: lockErr}, want: []error{lockErr}},
		{name: "busy", store: &fakeRegistryRollbackStore{}, want: nil},
		{name: "load and release", store: &fakeRegistryRollbackStore{locked: true, loadErr: loadErr, releaseErr: releaseErr}, want: []error{loadErr, releaseErr}},
		{name: "save and release", store: &fakeRegistryRollbackStore{locked: true, saveErr: saveErr, releaseErr: releaseErr}, want: []error{saveErr, releaseErr}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := restoreRegistryRowWithStore(tt.store, "workspace", "go", WorkspaceEntry{}, false)
			if tt.name == "busy" {
				if err == nil || err.Error() != "try-lock registry for rollback: lock is busy" {
					t.Fatalf("busy error = %v", err)
				}
				return
			}
			for _, want := range tt.want {
				if !errors.Is(err, want) {
					t.Errorf("error %v does not retain %v", err, want)
				}
			}
		})
	}
}
