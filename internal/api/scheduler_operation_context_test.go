package api

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"mcp-local-hub/internal/scheduler"
)

// Use the existing register harness so the real transaction/rollback dispatch
// runs while native tasks, process control, client files and state stay isolated.
func TestSchedulerUnknownMutationDoesNotTriggerRegistrationInverse(t *testing.T) {
	for _, mode := range []string{"legacy", "supervised", "automatic-lsp"} {
		t.Run(mode, func(t *testing.T) {
			h := newRegisterHarness(t)
			defer h.restore()
			previous := schedulerFactoryFn
			schedulerFactoryFn = func() (scheduler.Scheduler, error) { return h.fakeSch, nil }
			t.Cleanup(func() { schedulerFactoryFn = previous })
			ws := t.TempDir()
			canonical, err := CanonicalWorkspacePath(ws)
			if err != nil {
				t.Fatal(err)
			}
			wsKey := WorkspaceKey(canonical)
			task := LSPTaskNameForWorkspaceLanguage(wsKey, "python")
			original := []byte(`<Task name="` + task + `"/>`)
			h.fakeSch.tasks[task] = true
			h.fakeSch.xml[task] = append([]byte(nil), original...)
			cause := errors.Join(scheduler.ErrMutationOutcomeUnknown, context.DeadlineExceeded)
			h.fakeSch.failDeleteErr = cause
			a := mustNewAPI(t)
			if mode == "automatic-lsp" {
				reg := NewRegistry(h.regPath)
				if err := reg.PutLSP(WorkspaceEntry{WorkspaceKey: wsKey, WorkspacePath: canonical, Language: "python", Backend: "mcp-language-server", Port: 9242, TaskName: task, Lifecycle: LifecycleActive}); err != nil {
					t.Fatal(err)
				}
				if err := reg.Save(); err != nil {
					t.Fatal(err)
				}
				_, err = a.EnsureLSPRegistered(t.Context(), wsKey, canonical, "python")
			} else {
				_, err = a.registerWithManifest(nineLanguageManifest(), ws, []string{"python"}, RegisterOpts{Writer: &bytes.Buffer{}, SupervisedProxy: mode == "supervised"})
			}
			if !errors.Is(err, scheduler.ErrMutationOutcomeUnknown) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("original unknown outcome/cause lost: %v", err)
			}
			// Legacy registration creates the independent weekly-refresh task
			// before it reaches this LSP deletion. It is not an inverse operation.
			wantCreates := 0
			if mode == "legacy" {
				wantCreates = 1
			}
			if len(h.fakeSch.deleteNames) != 1 || len(h.fakeSch.importNames) != 0 || len(h.fakeSch.runNames) != 0 || h.fakeSch.createCount != wantCreates {
				t.Fatalf("unconfirmed deletion triggered another task mutation: delete=%v import=%v run=%v create=%d", h.fakeSch.deleteNames, h.fakeSch.importNames, h.fakeSch.runNames, h.fakeSch.createCount)
			}
			for _, spec := range h.fakeSch.createdSpecs {
				if spec.Name != WeeklyRefreshTaskName {
					t.Fatalf("unconfirmed LSP deletion triggered Create(%s)", spec.Name)
				}
			}
			if !bytes.Equal(h.fakeSch.xml[task], original) {
				t.Fatal("unknown registration occupant changed")
			}
		})
	}
}

type operationBindingSchedulerFixture struct {
	scheduler.Scheduler
	received context.Context
}

func (s *operationBindingSchedulerFixture) BindContext(ctx context.Context) (scheduler.Scheduler, error) {
	s.received = ctx
	return s, nil
}

func TestInstallSchedulerFactoryBindsOriginalOperationContext(t *testing.T) {
	fixture := &operationBindingSchedulerFixture{Scheduler: newInstallFakeScheduler()}
	previous := schedulerFactoryFn
	schedulerFactoryFn = func() (scheduler.Scheduler, error) { return fixture, nil }
	t.Cleanup(func() { schedulerFactoryFn = previous })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if _, err := newSchedulerContext(ctx); err != nil {
		t.Fatal(err)
	}
	if fixture.received != ctx {
		t.Fatal("factory replaced the caller lifetime")
	}
	cancel()
	if _, err := newSchedulerContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled scheduler admitted: %v", err)
	}
}

type schedulerContextCall struct {
	operation string
	ctx       context.Context
}

// This fixture enforces the production bound-context contract, including
// immutable rebinding. Embedding the old context-free fake alone would hide
// the cancellation failure in the real transaction's compensation closure.
type restorationContextSchedulerFixture struct {
	scheduler.Scheduler
	ctx      context.Context
	bindings *[]context.Context
	calls    *[]schedulerContextCall
}

func (s *restorationContextSchedulerFixture) BindContext(ctx context.Context) (scheduler.Scheduler, error) {
	bound := *s
	bound.ctx = ctx
	*s.bindings = append(*s.bindings, ctx)
	return &bound, nil
}

func (s *restorationContextSchedulerFixture) observe(operation string) error {
	*s.calls = append(*s.calls, schedulerContextCall{operation: operation, ctx: s.ctx})
	if s.ctx == nil {
		return errors.New("fixture operation lacks bound context")
	}
	return s.ctx.Err()
}

func (s *restorationContextSchedulerFixture) ExportXML(name string) ([]byte, error) {
	if err := s.observe("export"); err != nil {
		return nil, err
	}
	return s.Scheduler.ExportXML(name)
}

func (s *restorationContextSchedulerFixture) Delete(name string) error {
	if err := s.observe("delete"); err != nil {
		return err
	}
	return s.Scheduler.Delete(name)
}

func (s *restorationContextSchedulerFixture) ImportXML(name string, raw []byte) error {
	if err := s.observe("import"); err != nil {
		return err
	}
	return s.Scheduler.ImportXML(name, raw)
}

func (s *restorationContextSchedulerFixture) Run(name string) error {
	if err := s.observe("run"); err != nil {
		return err
	}
	return s.Scheduler.Run(name)
}

func TestEnsureLSPRegisteredCanceledReconcileRestoresLegacyTask(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	var bindings []context.Context
	var calls []schedulerContextCall
	fixture := &restorationContextSchedulerFixture{Scheduler: h.fakeSch, bindings: &bindings, calls: &calls}
	previousFactory := schedulerFactoryFn
	schedulerFactoryFn = func() (scheduler.Scheduler, error) { return fixture, nil }
	t.Cleanup(func() { schedulerFactoryFn = previousFactory })
	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatal(err)
	}
	wsKey := WorkspaceKey(canonical)
	task := LSPTaskNameForWorkspaceLanguage(wsKey, "python")
	prior := WorkspaceEntry{WorkspaceKey: wsKey, WorkspacePath: canonical, Language: "python", Backend: "mcp-language-server", Port: 9242, TaskName: task, Lifecycle: LifecycleActive}
	reg := NewRegistry(h.regPath)
	if err := reg.PutLSP(prior); err != nil {
		t.Fatal(err)
	}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	original := []byte(`<Task name="` + task + `"/>`)
	h.fakeSch.tasks[task] = true
	h.fakeSch.xml[task] = append([]byte(nil), original...)
	type contextValueKey struct{}
	ctx, cancel := context.WithCancelCause(context.WithValue(t.Context(), contextValueKey{}, "original-operation"))
	defer cancel(context.Canceled)
	cause := errors.New("caller canceled during reconcile")
	previousReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(reconcileCtx context.Context, apply bool) (ReconcileResponse, error) {
		if !apply || len(h.fakeSch.deleteNames) != 1 || h.fakeSch.tasks[task] {
			t.Fatal("reconcile did not follow the known successful legacy deletion")
		}
		cancel(cause)
		return ReconcileResponse{}, errors.Join(reconcileCtx.Err(), context.Cause(reconcileCtx))
	}
	t.Cleanup(func() { registerSupervisorReconcileFn = previousReconcile })
	_, err = NewAPI().EnsureLSPRegistered(ctx, wsKey, canonical, "python")
	if !errors.Is(err, cause) || !errors.Is(err, context.Canceled) || errors.Is(err, scheduler.ErrMutationOutcomeUnknown) {
		t.Fatalf("known deletion/canceled reconcile lost its cause: %v", err)
	}
	if len(h.fakeSch.deleteNames) != 1 || len(h.fakeSch.importNames) != 1 || len(h.fakeSch.runNames) != 1 || !bytes.Equal(h.fakeSch.xml[task], original) {
		t.Fatalf("known deletion was not restored after caller cancellation: delete=%v import=%v run=%v", h.fakeSch.deleteNames, h.fakeSch.importNames, h.fakeSch.runNames)
	}
	if len(bindings) != 2 || bindings[0] != ctx || bindings[1] == ctx || fixture.ctx != nil {
		t.Fatalf("restoration did not bind one independent private backend: bindings=%d", len(bindings))
	}
	cleanupCtx := bindings[1]
	deadline, bounded := cleanupCtx.Deadline()
	if !bounded || deadline.Sub(time.Now()) > lspLegacyTaskRestoreTimeout || cleanupCtx.Value(contextValueKey{}) != "original-operation" {
		t.Fatal("restoration lost its shared bound or original context values")
	}
	if len(calls) != 4 || calls[0].operation != "export" || calls[1].operation != "delete" || calls[2].operation != "import" || calls[3].operation != "run" {
		t.Fatalf("unexpected scheduler sequence: %+v", calls)
	}
	for i, call := range calls {
		wantCtx := ctx
		if i >= 2 {
			wantCtx = cleanupCtx
		}
		if call.ctx != wantCtx {
			t.Fatalf("%s renewed or replaced its operation lifetime", call.operation)
		}
	}
	if !errors.Is(cleanupCtx.Err(), context.Canceled) {
		t.Fatal("completed restoration retained a live cleanup timer")
	}
}
