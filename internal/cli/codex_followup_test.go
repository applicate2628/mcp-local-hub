// Tests for the Codex deep-security parallel review on PR #135 — CLI side.
//
// Finding 2 (HIGH): runUninstall must NOT call runUninstallWatchdog when
// api.Uninstall returned (report, nil) but report.TaskDeleteWarns is
// non-empty. Surviving scheduler tasks (those that failed to delete) need
// the watchdog to keep running so auto-recovery is preserved.
package cli

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// TestRunUninstall_TaskDeleteWarns_KeepsWatchdog covers Finding 2 (HIGH).
// Before the fix, the per-server uninstall returning a non-nil report with
// per-task delete warnings (TasksDeleted=N, TaskDeleteWarns=[...]) but a
// nil error caused runUninstall to proceed to runUninstallWatchdog,
// stripping the watchdog. Surviving tasks (those that failed to delete)
// were left without auto-recovery. The fix gates the watchdog teardown on
// `len(report.TaskDeleteWarns) == 0`.
func TestRunUninstall_TaskDeleteWarns_KeepsWatchdog(t *testing.T) {
	// Stubbed report mirrors api.Uninstall's contract on a partial failure:
	//   - TasksDeleted carries the names that DID delete cleanly.
	//   - TaskDeleteWarns carries the failures (per-task scheduler.Delete
	//     errors).
	//   - The function returns (report, nil) — the warnings ride inside
	//     the report, not the error channel.
	doUninstall := func(s string) (*api.UninstallReport, error) {
		return &api.UninstallReport{
			Server:          s,
			TasksDeleted:    []string{"\\mcp-local-hub-time-default"},
			TaskDeleteWarns: []string{"delete \\mcp-local-hub-time-extra: scheduler busy"},
		}, nil
	}
	watchdogCalled := false
	doWatchdogUninstall := func(_ io.Writer, _ string) error {
		watchdogCalled = true
		return nil
	}

	out := &bytes.Buffer{}
	err := runUninstall(out, "time", doUninstall, doWatchdogUninstall)
	// runUninstall MUST NOT call the watchdog teardown.
	if watchdogCalled {
		t.Errorf("runUninstallWatchdog was invoked despite TaskDeleteWarns; surviving tasks would lose auto-recovery")
	}
	// The user-facing output must still surface the warnings so the
	// operator knows survivors remain.
	if !strings.Contains(out.String(), "scheduler busy") {
		t.Errorf("warning text missing from stdout; got %q", out.String())
	}
	// The user-facing output must NOT claim a clean uninstall — partial
	// failure should not be silently swallowed as success.
	if strings.Contains(out.String(), "Uninstall complete") {
		t.Errorf("'Uninstall complete' must not render on partial failure; got %q", out.String())
	}
	// The error channel surfaces a non-nil error so the cobra wrapper
	// returns a non-zero exit (operator-visible signal).
	if err == nil {
		t.Error("runUninstall: want non-nil error on partial-delete report, got nil")
	}
}

// TestRunUninstall_NoTaskDeleteWarns_RemovesWatchdog regression-guards the
// happy path: when the report carries zero TaskDeleteWarns, the watchdog
// teardown is invoked exactly once after the per-server uninstall.
func TestRunUninstall_NoTaskDeleteWarns_RemovesWatchdog(t *testing.T) {
	doUninstall := func(s string) (*api.UninstallReport, error) {
		return &api.UninstallReport{
			Server:       s,
			TasksDeleted: []string{"\\mcp-local-hub-time-default"},
			// TaskDeleteWarns intentionally nil — clean delete.
		}, nil
	}
	watchdogCalls := 0
	doWatchdogUninstall := func(_ io.Writer, _ string) error {
		watchdogCalls++
		return nil
	}

	out := &bytes.Buffer{}
	if err := runUninstall(out, "time", doUninstall, doWatchdogUninstall); err != nil {
		t.Fatalf("runUninstall: %v", err)
	}
	if watchdogCalls != 1 {
		t.Errorf("watchdog teardown invocations = %d, want 1 on clean uninstall", watchdogCalls)
	}
	if !strings.Contains(out.String(), "Uninstall complete") {
		t.Errorf("'Uninstall complete' missing on clean uninstall; got %q", out.String())
	}
}

// silenceErrorsIs guards against accidental import pruning when the file
// drops references to errors.Is during refactors.
var _ = errors.Is
