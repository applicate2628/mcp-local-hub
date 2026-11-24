//go:build windows

package scheduler

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"testing/synctest"
	"time"

	"mcp-local-hub/internal/process"
)

func TestSchedulerBoundOperationPreservesDeadlineAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		deadline := time.Now().Add(10 * time.Second)
		ctx, cancel := context.WithDeadline(t.Context(), deadline)
		defer cancel()
		original := &windowsScheduler{}
		backend, err := BindContext(ctx, original)
		if err != nil {
			t.Fatal(err)
		}
		if original.ctx != nil || backend == original {
			t.Fatal("binding mutated shared scheduler")
		}
		calls := 0
		withCOMRunner(t, func(received context.Context, request schedulerCOMRequest) (schedulerCOMResponse, error) {
			calls++
			got, ok := received.Deadline()
			if !ok || !got.Equal(deadline) {
				t.Fatalf("caller deadline renewed: %v", got)
			}
			zero := int32(0)
			return schedulerCOMResponse{OK: true, Task: &schedulerCOMTask{Name: "fixture", State: 3, LastResult: &zero, NextRun: new(string), XML: "<Task/>"}}, nil
		})
		if _, err := backend.Status(`\fixture`); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Second) // virtual time, not a native-operation timing assertion
		if _, err := backend.ExportXML(`\fixture`); err != nil {
			t.Fatal(err)
		}
		cancel()
		if err := backend.Stop(`\fixture`); !errors.Is(err, context.Canceled) || errors.Is(err, ErrMutationOutcomeUnknown) {
			t.Fatalf("pre-dispatch cancellation became an attempted mutation: %v", err)
		}
		if calls != 2 {
			t.Fatalf("canceled operation dispatched: calls=%d", calls)
		}
	})
}

func TestSchedulerContainedCommandRefusesCanceledAdmission(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cmd := exec.Command("fixture-must-never-start.exe")
	result, err := runSchedulerCommand(ctx, cmd, nil)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrUnavailable) || cmd.Process != nil || result.SettledExit != nil {
		t.Fatalf("canceled command acquired a child or lost its cause: err=%v", err)
	}
	publicErr := schedulerCommandMutationError("/Create", result, err)
	if !errors.Is(publicErr, context.Canceled) || !errors.Is(publicErr, ErrUnavailable) || errors.Is(publicErr, ErrMutationOutcomeUnknown) {
		t.Fatalf("proven pre-dispatch refusal became an unknown mutation: %v", publicErr)
	}
	if _, err := NewContext(nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil context accepted: %v", err)
	}
}

func TestSchedulerCOMCancellationBeforeExecutorAdmissionRemainsKnown(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(context.Canceled)
	cause := errors.New("operation retired before executor admission")
	realRunner := schedulerCOMRun
	calls := 0
	withCOMRunner(t, func(received context.Context, request schedulerCOMRequest) (schedulerCOMResponse, error) {
		calls++
		// The public COM boundary has admitted the request, but its contained
		// executor has not. Cancel exactly in that window without native work.
		cancel(cause)
		return realRunner(received, request)
	})
	err := (&windowsScheduler{ctx: ctx}).Delete(`\fixture-never-dispatched`)
	if calls != 1 || !errors.Is(err, cause) || !errors.Is(err, context.Canceled) || !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrMutationOutcomeUnknown) {
		t.Fatalf("composed public cancellation classification: calls=%d err=%v", calls, err)
	}
}

func TestSchedulerCOMPostCreateFailureRemainsUnknown(t *testing.T) {
	cause := errors.New("native handle acquisition failed")
	withCOMRunner(t, func(context.Context, schedulerCOMRequest) (schedulerCOMResponse, error) {
		return schedulerCOMResponse{}, errors.Join(process.ErrSpawnPostCreate, cause)
	})
	err := (&windowsScheduler{ctx: t.Context()}).Delete(`\fixture-post-create`)
	if !errors.Is(err, ErrMutationOutcomeUnknown) || !errors.Is(err, process.ErrSpawnPostCreate) || !errors.Is(err, cause) {
		t.Fatalf("post-create evidence was downgraded to not-dispatched: %v", err)
	}
}

func TestSchedulerForeignNoDispatchCauseCannotDowngradeCurrentMutation(t *testing.T) {
	previous, stopPrevious := context.WithCancel(t.Context())
	stopPrevious()
	_, previousErr := runSchedulerCommand(previous, exec.Command("fixture-never-started.exe"), nil)
	if !schedulerCommandWasNotDispatched(previousErr) {
		t.Fatal("previous command did not return direct refusal evidence")
	}

	t.Run("schtasks-settled-timeout", func(t *testing.T) {
		current, cancel := context.WithCancelCause(t.Context())
		cancel(previousErr)
		// Model the existing runner's post-dispatch error and settlement; the
		// inherited cause is real evidence, but belongs to the earlier command.
		err := schedulerCommandMutationError("/Create", process.StrictRunResult{
			SettledExit: &process.StrictRunSettledExit{ExitCode: 1, TimedOut: true},
		}, errors.Join(ErrUnavailable, current.Err(), context.Cause(current)))
		if !errors.Is(err, ErrMutationOutcomeUnknown) || !errors.Is(err, previousErr) || !errors.Is(err, context.Canceled) {
			t.Fatalf("foreign no-dispatch cause downgraded current mutation: %v", err)
		}
	})
	t.Run("COM-post-create", func(t *testing.T) {
		current, cancel := context.WithCancelCause(t.Context())
		defer cancel(context.Canceled)
		withCOMRunner(t, func(received context.Context, _ schedulerCOMRequest) (schedulerCOMResponse, error) {
			cancel(previousErr)
			return schedulerCOMResponse{}, errors.Join(process.ErrSpawnPostCreate, received.Err(), context.Cause(received))
		})
		err := (&windowsScheduler{ctx: current}).Delete(`\fixture-post-create-inherited-cause`)
		if !errors.Is(err, ErrMutationOutcomeUnknown) || !errors.Is(err, process.ErrSpawnPostCreate) || !errors.Is(err, previousErr) {
			t.Fatalf("foreign no-dispatch cause downgraded COM mutation: %v", err)
		}
	})
}

func TestSchedulerCommandMutationOutcomePreservesExecutionCause(t *testing.T) {
	cause := errors.New("fixture execution cause")
	for _, tc := range []struct {
		name    string
		result  process.StrictRunResult
		extra   error
		unknown bool
	}{
		{"settled-command-failure", process.StrictRunResult{SettledExit: &process.StrictRunSettledExit{ExitCode: 7}}, nil, false},
		{"missing-settlement", process.StrictRunResult{}, nil, true},
		{"post-create-without-process-handle", process.StrictRunResult{}, process.ErrSpawnPostCreate, true},
		{"overflow-with-execution-error", process.StrictRunResult{Stdout: process.StrictRunCapture{Prefix: []byte("bounded"), Truncated: true}, SettledExit: &process.StrictRunSettledExit{ExitCode: 7}}, errSchedulerOutputLimit, true},
		{"canceled", process.StrictRunResult{SettledExit: &process.StrictRunSettledExit{ExitCode: 1, TimedOut: true}}, context.Canceled, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := schedulerCommandMutationError("/Create", tc.result, errors.Join(ErrUnavailable, cause, tc.extra))
			if !errors.Is(err, cause) || !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrMutationOutcomeUnknown) != tc.unknown {
				t.Fatalf("mutation classification/cause lost: %v", err)
			}
			if tc.extra != nil && !errors.Is(err, tc.extra) {
				t.Fatalf("secondary execution cause lost: %v", err)
			}
		})
	}
}

func TestEntrypointUnknownMutationDoesNotCompensate(t *testing.T) {
	backend := newEntrypointTaskBackendFake(map[string][]byte{`\mcp-local-hub-fixture`: []byte("original")}, "fixture-owner")
	backend.exportHook = func(string) error { t.Fatal("unknown mutation triggered recovery by name"); return nil }
	txn := &OwnedEntrypointTaskTxn{backend: backend, ctx: t.Context(), inventory: map[string]ownedEntrypointTaskSnapshot{`\mcp-local-hub-fixture`: {name: `\mcp-local-hub-fixture`}}}
	cause := errors.Join(ErrMutationOutcomeUnknown, context.DeadlineExceeded)
	err := txn.compensateExcluded(nil, nil, cause)
	var partial *EntrypointTaskPartialProgressError
	if !errors.As(err, &partial) || !errors.Is(err, ErrMutationOutcomeUnknown) || !errors.Is(err, context.DeadlineExceeded) || backend.importCalls != 0 {
		t.Fatalf("unknown mutation was replayed or cause lost: %v", err)
	}
}

func TestEntrypointTransitionObservesCreatorCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		creator, cancelCreator := context.WithCancelCause(t.Context())
		defer cancelCreator(context.Canceled)
		txn := &OwnedEntrypointTaskTxn{ctx: creator}
		ctx, cancel := txn.transitionContext(t.Context())
		defer cancel()
		cause := errors.New("original operation retired")
		cancelCreator(cause)
		synctest.Wait()
		if !errors.Is(context.Cause(ctx), cause) {
			t.Fatalf("original creator cancellation lost: %v", context.Cause(ctx))
		}
	})
}
