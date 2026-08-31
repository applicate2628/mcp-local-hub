package api

import (
	"fmt"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/scheduler"
)

type statusObservationScheduler struct {
	listErr error
}

func (s *statusObservationScheduler) Create(scheduler.TaskSpec) error { return nil }
func (s *statusObservationScheduler) Delete(string) error             { return nil }
func (s *statusObservationScheduler) Run(string) error                { return nil }
func (s *statusObservationScheduler) Stop(string) error               { return nil }
func (s *statusObservationScheduler) Status(string) (scheduler.TaskStatus, error) {
	return scheduler.TaskStatus{}, nil
}
func (s *statusObservationScheduler) List(string) ([]scheduler.TaskStatus, error) {
	return nil, s.listErr
}
func (s *statusObservationScheduler) ExportXML(string) ([]byte, error) {
	return nil, scheduler.ErrTaskNotFound
}
func (s *statusObservationScheduler) ImportXML(string, []byte) error { return nil }

func TestStatusWithOptsDetailed_SchedulerUnavailableContinuesRegistryRows(t *testing.T) {
	t.Cleanup(SetDaemonStateRootForTest(t.TempDir()))

	origScheduler := statusSchedulerFactory
	statusSchedulerFactory = func() (scheduler.Scheduler, error) {
		return nil, fmt.Errorf("scheduler bridge: %w: protocol", scheduler.ErrUnavailable)
	}
	t.Cleanup(func() { statusSchedulerFactory = origScheduler })

	regPath := filepath.Join(t.TempDir(), "workspaces.yaml")
	origRegPath := defaultRegistryPathFn
	defaultRegistryPathFn = func() (string, error) { return regPath, nil }
	t.Cleanup(func() { defaultRegistryPathFn = origRegPath })

	origBatch := lookupProcessBatch
	origLookup := lookupProcess
	lookupProcessBatch = nil
	lookupProcess = nil
	t.Cleanup(func() {
		lookupProcessBatch = origBatch
		lookupProcess = origLookup
	})

	origPortLive := registryOnlyStatusPortLiveFn
	registryOnlyStatusPortLiveFn = func(int) bool { return false }
	t.Cleanup(func() { registryOnlyStatusPortLiveFn = origPortLive })

	reg := NewRegistry(regPath)
	reg.Put(WorkspaceEntry{
		WorkspaceKey:  "abcd1234",
		WorkspacePath: "workspace",
		Language:      "python",
		Backend:       "mcp-language-server",
		Port:          9217,
		TaskName:      "mcp-local-hub-lsp-abcd1234-python",
		Lifecycle:     LifecycleActive,
	})
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}

	detailed, err := NewAPI().StatusWithOptsDetailed(StatusOpts{})
	if err != nil {
		t.Fatalf("StatusWithOptsDetailed: %v", err)
	}
	if detailed.Observation != SchedulerObservationUnavailable {
		t.Fatalf("scheduler observation = %q, want %q", detailed.Observation, SchedulerObservationUnavailable)
	}
	if len(detailed.Rows) != 1 || detailed.Rows[0].TaskName != "mcp-local-hub-lsp-abcd1234-python" {
		t.Fatalf("detailed rows = %+v, want registry row after unavailable scheduler", detailed.Rows)
	}

	legacyRows, err := NewAPI().StatusWithOpts(StatusOpts{})
	if err != nil {
		t.Fatalf("legacy StatusWithOpts: %v", err)
	}
	if len(legacyRows) != 1 || legacyRows[0].TaskName != detailed.Rows[0].TaskName {
		t.Fatalf("legacy rows = %+v, want detailed rows %+v", legacyRows, detailed.Rows)
	}
}

func TestStatusWithOptsDetailed_ObservationClassesKeepUnexpectedFailuresFatal(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want SchedulerObservation
	}{
		{name: "not implemented", err: fmt.Errorf("scheduler disabled: %w", scheduler.ErrNotImplemented), want: SchedulerObservationUnsupported},
		{name: "unavailable", err: fmt.Errorf("scheduler bridge: %w", scheduler.ErrUnavailable), want: SchedulerObservationUnavailable},
		{name: "untyped", err: fmt.Errorf("scheduler bridge protocol")},
		{name: "permission", err: fmt.Errorf("scheduler access: %w", scheduler.ErrPermissionDenied)},
		{name: "corrupt", err: fmt.Errorf("scheduler task: %w", scheduler.ErrTaskCorrupt)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origScheduler := statusSchedulerFactory
			statusSchedulerFactory = func() (scheduler.Scheduler, error) { return nil, tc.err }
			t.Cleanup(func() { statusSchedulerFactory = origScheduler })
			t.Cleanup(SetDaemonStateRootForTest(t.TempDir()))

			detailed, err := NewAPI().StatusWithOptsDetailed(StatusOpts{})
			if tc.want == "" {
				if err == nil {
					t.Fatalf("StatusWithOptsDetailed returned nil error for %v", tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("StatusWithOptsDetailed: %v", err)
			}
			if detailed.Observation != tc.want {
				t.Fatalf("scheduler observation = %q, want %q", detailed.Observation, tc.want)
			}
		})
	}
}

func TestStatusWithOptsDetailed_SchedulerListUnavailableContinues(t *testing.T) {
	origScheduler := statusSchedulerFactory
	statusSchedulerFactory = func() (scheduler.Scheduler, error) {
		return &statusObservationScheduler{listErr: fmt.Errorf("scheduler bridge: %w: protocol", scheduler.ErrUnavailable)}, nil
	}
	t.Cleanup(func() { statusSchedulerFactory = origScheduler })
	t.Cleanup(SetDaemonStateRootForTest(t.TempDir()))

	detailed, err := NewAPI().StatusWithOptsDetailed(StatusOpts{})
	if err != nil {
		t.Fatalf("StatusWithOptsDetailed: %v", err)
	}
	if detailed.Observation != SchedulerObservationUnavailable {
		t.Fatalf("scheduler observation = %q, want %q", detailed.Observation, SchedulerObservationUnavailable)
	}
}
