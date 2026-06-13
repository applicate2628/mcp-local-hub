package api

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStopSupervisorManualReviewDriftReturnsError(t *testing.T) {
	kills, fake := stopSupervisorTestSetup(t, stopSupervisorTestIntent(), nil)

	restore := setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		return ReconcileResponse{
			Drift: []DriftEntry{
				{TaskName: stopSupervisorTestTask, Action: ReconcileActionNeedsManualReview},
			},
		}, nil
	})
	defer restore()

	results, err := NewAPI().Stop("time", "")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := atomic.LoadInt32(kills); got != 0 {
		t.Fatalf("legacy killByPortFn calls = %d, want 0 (manual-review row must be surfaced, not killed)", got)
	}
	if len(fake.stopNames) != 0 {
		t.Fatalf("scheduler Stop calls = %v, want none for supervisor-handled manual-review target", fake.stopNames)
	}
	if len(results) != 1 || results[0].TaskName != stopSupervisorTestTask {
		t.Fatalf("results = %+v, want one row for %s", results, stopSupervisorTestTask)
	}
	if results[0].Err == "" {
		t.Fatalf("manual-review drift returned empty Err row: %+v", results[0])
	}
	for _, want := range []string{"manual review", "stop not applied", "supervisor-events.log", "mcphub status"} {
		if !strings.Contains(results[0].Err, want) {
			t.Fatalf("manual-review error = %q, want substring %q", results[0].Err, want)
		}
	}
}

func TestStopSupervisorIPCUnavailableLiveOwnerReturnsRetryableErrorWithoutKill(t *testing.T) {
	kills, fake := stopSupervisorTestSetup(t, stopSupervisorTestIntent(), nil)

	restore := setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, fmt.Errorf("supervisor IPC reconcile: dial: %w", ErrSupervisorIPCUnavailable)
	})
	defer restore()

	origProbe := installSupervisorRunningProbeFn
	var probed bool
	installSupervisorRunningProbeFn = func(stateDir string) (bool, int, error) {
		if stateDir == "" {
			t.Fatal("state dir passed to supervisor running probe is empty")
		}
		probed = true
		return true, 43210, nil
	}
	t.Cleanup(func() { installSupervisorRunningProbeFn = origProbe })

	origForceKill := forceKillByPortFn
	forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
		t.Fatalf("forceKillByPortFn called for live-supervisor IPC-unavailable stop on port %d", port)
		return portKillNoListener, nil
	}
	t.Cleanup(func() { forceKillByPortFn = origForceKill })

	results, err := NewAPI().Stop("time", "")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !probed {
		t.Fatal("supervisor running probe was not called before handling IPC-unavailable stop")
	}
	if got := atomic.LoadInt32(kills); got != 0 {
		t.Fatalf("legacy killByPortFn calls = %d, want 0 under a live IPC-unavailable supervisor", got)
	}
	if len(fake.stopNames) != 0 {
		t.Fatalf("scheduler Stop calls = %v, want none under a live IPC-unavailable supervisor", fake.stopNames)
	}
	assertWedgedSupervisorRetryRow(t, results)
}

func TestStopSupervisorIPCUnavailableNoOwnerKeepsDescriptorKillFallback(t *testing.T) {
	stopSupervisorTestSetup(t, stopSupervisorTestIntent(), nil)

	restore := setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, fmt.Errorf("supervisor IPC reconcile: dial: %w", ErrSupervisorIPCUnavailable)
	})
	defer restore()

	origProbe := installSupervisorRunningProbeFn
	var probed bool
	installSupervisorRunningProbeFn = func(stateDir string) (bool, int, error) {
		probed = true
		return false, 0, nil
	}
	t.Cleanup(func() { installSupervisorRunningProbeFn = origProbe })

	var forceKillPorts []int
	origForceKill := forceKillByPortFn
	forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
		forceKillPorts = append(forceKillPorts, port)
		return portKillKilled, nil
	}
	t.Cleanup(func() { forceKillByPortFn = origForceKill })

	results, handled, err := stopSupervisorOwnedDaemons(context.Background(), "time", "")
	if err != nil {
		t.Fatalf("stopSupervisorOwnedDaemons: %v", err)
	}
	if !handled {
		t.Fatal("handled=false, want true for supervisor-owned IPC-unavailable target")
	}
	if !probed {
		t.Fatal("supervisor running probe was not called before descriptor kill fallback")
	}
	if len(forceKillPorts) != 1 || forceKillPorts[0] != 9128 {
		t.Fatalf("forceKillByPortFn ports = %v, want [9128]", forceKillPorts)
	}
	if len(results) != 1 || results[0].TaskName != stopSupervisorTestTask || results[0].Err != "" {
		t.Fatalf("results = %+v, want one descriptor kill success row", results)
	}
}

func TestStopForceSupervisorIPCUnavailableLiveOwnerReturnsRetryableErrorWithoutKill(t *testing.T) {
	stopSupervisorTestSetup(t, stopSupervisorTestIntent(), nil)

	origStatus := supervisorIPCStatusFn
	supervisorIPCStatusFn = func(ctx context.Context) ([]DaemonStatus, error) {
		return nil, fmt.Errorf("supervisor IPC status: dial: %w", ErrSupervisorIPCUnavailable)
	}
	t.Cleanup(func() { supervisorIPCStatusFn = origStatus })

	origProbe := installSupervisorRunningProbeFn
	var probed bool
	installSupervisorRunningProbeFn = func(stateDir string) (bool, int, error) {
		probed = true
		return true, 43210, nil
	}
	t.Cleanup(func() { installSupervisorRunningProbeFn = origProbe })

	origForceKill := forceKillByPortFn
	forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
		t.Fatalf("forceKillByPortFn called for live-supervisor IPC-unavailable force stop on port %d", port)
		return portKillNoListener, nil
	}
	t.Cleanup(func() { forceKillByPortFn = origForceKill })

	results, handled, err := stopForceKillSupervisorOwned(context.Background(), "time", "")
	if err != nil {
		t.Fatalf("stopForceKillSupervisorOwned: %v", err)
	}
	if !handled {
		t.Fatal("handled=false, want true for supervisor-owned force target")
	}
	if !probed {
		t.Fatal("supervisor running probe was not called before handling IPC-unavailable force stop")
	}
	assertWedgedSupervisorRetryRow(t, results)
}

func TestStopForceSupervisorIPCUnavailableNoOwnerKeepsDescriptorKillFallback(t *testing.T) {
	stopSupervisorTestSetup(t, stopSupervisorTestIntent(), nil)

	origStatus := supervisorIPCStatusFn
	supervisorIPCStatusFn = func(ctx context.Context) ([]DaemonStatus, error) {
		return nil, fmt.Errorf("supervisor IPC status: dial: %w", ErrSupervisorIPCUnavailable)
	}
	t.Cleanup(func() { supervisorIPCStatusFn = origStatus })

	origProbe := installSupervisorRunningProbeFn
	var probed bool
	installSupervisorRunningProbeFn = func(stateDir string) (bool, int, error) {
		probed = true
		return false, 0, nil
	}
	t.Cleanup(func() { installSupervisorRunningProbeFn = origProbe })

	var forceKillPorts []int
	origForceKill := forceKillByPortFn
	forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
		forceKillPorts = append(forceKillPorts, port)
		return portKillKilled, nil
	}
	t.Cleanup(func() { forceKillByPortFn = origForceKill })

	results, handled, err := stopForceKillSupervisorOwned(context.Background(), "time", "")
	if err != nil {
		t.Fatalf("stopForceKillSupervisorOwned: %v", err)
	}
	if !handled {
		t.Fatal("handled=false, want true for supervisor-owned force target")
	}
	if !probed {
		t.Fatal("supervisor running probe was not called before descriptor kill fallback")
	}
	if len(forceKillPorts) != 1 || forceKillPorts[0] != 9128 {
		t.Fatalf("forceKillByPortFn ports = %v, want [9128]", forceKillPorts)
	}
	if len(results) != 1 || results[0].TaskName != stopSupervisorTestTask || results[0].Err != "" {
		t.Fatalf("results = %+v, want one descriptor kill success row", results)
	}
}

func assertWedgedSupervisorRetryRow(t *testing.T, results []RestartResult) {
	t.Helper()
	if len(results) != 1 || results[0].TaskName != stopSupervisorTestTask {
		t.Fatalf("results = %+v, want one row for %s", results, stopSupervisorTestTask)
	}
	if results[0].Err == "" {
		t.Fatalf("IPC-unavailable live-owner path returned empty Err row: %+v", results[0])
	}
	for _, want := range []string{"IPC is unreachable", "mcphub restart", "kill the wedged process"} {
		if !strings.Contains(results[0].Err, want) {
			t.Fatalf("retryable error = %q, want substring %q", results[0].Err, want)
		}
	}
}
