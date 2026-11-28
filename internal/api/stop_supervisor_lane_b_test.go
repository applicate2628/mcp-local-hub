package api

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStopSupervisorStopBatchRefusalReturnsError(t *testing.T) {
	kills, fake := stopSupervisorTestSetup(t, stopSupervisorTestIntent(), nil)

	restore := setSupervisorStopBatchHookForTest(func(context.Context, StopBatchCommandV1) (StopBatchResultV1, error) {
		return StopBatchResultV1{}, fmt.Errorf("stop_batch manual review required")
	})
	defer restore()

	results, err := NewAPI().Stop("time", "")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := atomic.LoadInt32(kills); got != 0 {
		t.Fatalf("legacy killByPortFn calls = %d, want 0 (stop_batch refusal must be surfaced, not killed)", got)
	}
	if len(fake.stopNames) != 0 {
		t.Fatalf("scheduler Stop calls = %v, want none for supervisor-handled stop_batch target", fake.stopNames)
	}
	if len(results) != 1 || results[0].TaskName != stopSupervisorTestTask {
		t.Fatalf("results = %+v, want one row for %s", results, stopSupervisorTestTask)
	}
	if results[0].Err == "" {
		t.Fatalf("stop_batch refusal returned empty Err row: %+v", results[0])
	}
	for _, want := range []string{"manual review", "stop_batch"} {
		if !strings.Contains(results[0].Err, want) {
			t.Fatalf("stop_batch error = %q, want substring %q", results[0].Err, want)
		}
	}
}

func TestStopSupervisorIPCUnavailableLiveOwnerReturnsRetryableErrorWithoutKill(t *testing.T) {
	kills, fake := stopSupervisorTestSetup(t, stopSupervisorTestIntent(), nil)

	restore := setSupervisorStopBatchHookForTest(func(context.Context, StopBatchCommandV1) (StopBatchResultV1, error) {
		return StopBatchResultV1{}, fmt.Errorf("supervisor IPC stop_batch: dial: %w", ErrSupervisorIPCUnavailable)
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

	restore := setSupervisorStopBatchHookForTest(func(context.Context, StopBatchCommandV1) (StopBatchResultV1, error) {
		return StopBatchResultV1{}, fmt.Errorf("supervisor IPC stop_batch: dial: %w", ErrSupervisorIPCUnavailable)
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

func TestStopForceSupervisorIPCUnavailableNeverFallsBackToObservedOwner(t *testing.T) {
	for _, ownerLive := range []bool{false, true} {
		t.Run(fmt.Sprintf("observed_owner_live_%t", ownerLive), func(t *testing.T) {
			stopSupervisorTestSetup(t, stopSupervisorTestIntent(), nil)
			sealForceStopNumericalFallbacks(t)
			installForceStopReceipt(t, func(context.Context, string, int) (ForceStopResultV1, error) {
				return ForceStopResultV1{}, fmt.Errorf("force_stop dial: %w", ErrSupervisorIPCUnavailable)
			})
			previousProbe := installSupervisorRunningProbeFn
			installSupervisorRunningProbeFn = func(string) (bool, int, error) {
				t.Error("force-stop used an observer liveness probe as kill authority")
				return ownerLive, 43210, nil
			}
			t.Cleanup(func() { installSupervisorRunningProbeFn = previousProbe })
			rows, handled, err := stopForceKillSupervisorOwned(context.Background(), "time", "")
			if err != nil || !handled || len(rows) != 1 || rows[0].Code != ForceStopOwnerUnavailable || rows[0].Err == "" {
				t.Fatalf("unavailable owner: handled=%t rows=%+v err=%v", handled, rows, err)
			}
		})
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
