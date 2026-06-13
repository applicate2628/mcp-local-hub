//go:build !windows

package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/process"
)

func TestFinishProductionTerminate_EscalationAbortReturnsError(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)

	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	cmd := exec.Command(copyCurrentTestBinaryAsReconcileMcphub(t), "-test.run=TestProductionTerminateFn_HelperSleep")
	cmd.Env = append(os.Environ(), "MCPHUB_PRODUCTION_TERMINATE_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper child: %v", err)
	}
	if cmd.Process == nil {
		t.Fatal("helper child started without Process")
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	pid := cmd.Process.Pid
	err = finishProductionTerminate(process.PIDIdentityProof{
		PID:            pid,
		ExecutablePath: canonicalMcphubPath(),
		StartedAt:      "2000-01-01T00:00:00Z",
	}, api.SupervisorDaemon{TaskName: reconcileWiringTestTaskName}, events)
	if err == nil {
		t.Fatal("finishProductionTerminate returned nil after PID identity mismatch at escalation")
	}
	if !strings.Contains(err.Error(), "terminate escalation aborted") {
		t.Fatalf("finishProductionTerminate error = %v, want escalation abort", err)
	}

	logRaw, readErr := os.ReadFile(eventsPath)
	if readErr != nil {
		t.Fatalf("read events log: %v", readErr)
	}
	if !strings.Contains(string(logRaw), `"event":"daemon-terminate-escalation-aborted-pid-reuse"`) {
		t.Fatalf("daemon-terminate-escalation-aborted-pid-reuse event missing from audit log:\n%s", string(logRaw))
	}
}

// TestProductionTerminateFn_EscalationAbortIsNotGone is the #2316 (pr302 r4)
// falsifier. The production terminate closure must NOT classify a
// finishProductionTerminate ESCALATION-ABORT as errTerminateTargetGone: on POSIX an
// escalation abort means the process survived SIGTERM AND its identity could not be
// verified for SIGKILL — it MAY STILL BE ALIVE. The r3 code wrapped EVERY
// finishProductionTerminate error as gone, which would make the orphan reap classify
// a still-alive daemon confirmed-dead and clear the supervisor handle.
//
// This drives the FULL production terminate closure (makeProductionTerminateFnWithStatePath)
// against a REAL surviving helper child: query→alive, verify→nil, terminate-issue→nil
// (the fake does not actually kill, so the child survives the grace period),
// finishProductionTerminate then escalates and aborts on a stale-StartedAt identity
// proof. The returned error must NOT be errTerminateTargetGone (so the reap preserves
// + retries rather than losing the PID).
func TestProductionTerminateFn_EscalationAbortIsNotGone(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()
	statePath := filepath.Join(tmpHome, "supervisor-state.json")

	// A REAL child that survives SIGTERM long enough that the grace-period poll in
	// finishProductionTerminate sees it ALIVE and escalates.
	cmd := exec.Command(copyCurrentTestBinaryAsReconcileMcphub(t), "-test.run=TestProductionTerminateFn_HelperSleep")
	cmd.Env = append(os.Environ(), "MCPHUB_PRODUCTION_TERMINATE_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper child: %v", err)
	}
	if cmd.Process == nil {
		t.Fatal("helper child started without Process")
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	const taskName = `\mcp-local-hub-memory-default`
	tracker := NewDaemonRuntimeTracker()
	// Record the live PID with a STALE StartedAt so the escalation-time identity
	// verify (real VerifyPIDIdentity) mismatches and aborts the SIGKILL.
	tracker.MarkSpawned(taskName, pid, time.Unix(1, 0).UTC())

	prevQuery := productionQueryPIDStateFn
	prevVerify := productionVerifyPIDIdentityFn
	prevTerminate := productionTerminatePIDWithIdentityFn
	productionQueryPIDStateFn = func(int) (process.PIDState, error) { return process.PIDStateAlive, nil }
	productionVerifyPIDIdentityFn = func(process.PIDIdentityProof) error { return nil }
	// Terminate-issue returns nil (the SIGTERM is "sent") but does NOT actually kill
	// the helper child, so it survives the grace period and finishProductionTerminate
	// escalates → real VerifyPIDIdentity against the stale StartedAt proof mismatches
	// → escalation abort.
	productionTerminatePIDWithIdentityFn = func(process.PIDIdentityProof) error { return nil }
	t.Cleanup(func() {
		productionQueryPIDStateFn = prevQuery
		productionVerifyPIDIdentityFn = prevVerify
		productionTerminatePIDWithIdentityFn = prevTerminate
	})

	terminateFn := makeProductionTerminateFnWithStatePath(events, map[string]runningProcessIdentity{}, tracker, statePath)
	err = terminateFn(api.SupervisorDaemon{TaskName: taskName, Command: canonicalMcphubPath()})
	if err == nil {
		t.Fatal("#2316: a finishProductionTerminate escalation-abort must surface as a terminate ERROR, not nil success")
	}
	if errors.Is(err, errTerminateTargetGone) {
		t.Fatalf("#2316: an escalation-abort (process MAY STILL BE ALIVE) must NOT be wrapped errTerminateTargetGone (which would make the reap clear a live daemon's handle); got %v", err)
	}
	if !strings.Contains(err.Error(), "terminate escalation aborted") {
		t.Fatalf("#2316: expected the escalation-abort error to propagate unchanged; got %v", err)
	}
}
