package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// TestReconcileCmd_DryRunOutputFormatsTable verifies that the
// `mcphub reconcile` command (no --apply, no --json) prints a
// human-readable table with the expected header rows and per-entry
// columns.
func TestReconcileCmd_DryRunOutputFormatsTable(t *testing.T) {
	fakeResp := api.ReconcileResponse{
		DryRun:       true,
		DriftCount:   2,
		AppliedCount: 0,
		Drift: []api.DriftEntry{
			{
				TaskName:       `\mcp-local-hub-foo-default`,
				SchedulerState: api.ReconcileSchedulerStateStopped,
				IntentDesired:  api.ReconcileIntentDesiredRunning,
				SMState:        api.StIdle,
				Action:         api.ReconcileActionPostEvIntentUpdate,
			},
			{
				TaskName:       `\mcp-local-hub-bar-default`,
				SchedulerState: api.ReconcileSchedulerStateRunning,
				IntentDesired:  api.ReconcileIntentDesiredRunning,
				SMState:        api.StRunning,
				Action:         api.ReconcileActionNoOp,
			},
		},
	}
	uninstall := setReconcileDialFnForTest(func(ctx context.Context, apply bool) (api.ReconcileResponse, error) {
		if apply {
			t.Errorf("dial called with apply=true; want false (no --apply flag)")
		}
		return fakeResp, nil
	})
	defer uninstall()

	cmd := newReconcileCmdReal()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"mode: dry-run",
		"drift entries: 2",
		"applied transitions: 0",
		"TASK", "SCHED", "INTENT", "SM_STATE", "ACTION",
		`\mcp-local-hub-foo-default`,
		`\mcp-local-hub-bar-default`,
		"post_ev_intent_update",
		"no_op",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q; full:\n%s", want, out)
		}
	}
}

// TestReconcileCmd_ApplyFlagPropagates verifies the --apply flag is
// forwarded to the dial function.
func TestReconcileCmd_ApplyFlagPropagates(t *testing.T) {
	var sawApply bool
	uninstall := setReconcileDialFnForTest(func(ctx context.Context, apply bool) (api.ReconcileResponse, error) {
		sawApply = apply
		return api.ReconcileResponse{
			DryRun:              !apply,
			DriftCount:          0,
			AppliedCount:        0,
			SerenaRepairOutcome: api.SerenaIntentRepairOutcomeCompleted,
		}, nil
	})
	defer uninstall()

	cmd := newReconcileCmdReal()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--apply"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !sawApply {
		t.Errorf("dial was called with apply=false; want true (--apply was set)")
	}
	if !strings.Contains(buf.String(), "mode: apply") {
		t.Errorf("expected 'mode: apply' header; got %s", buf.String())
	}
}

// TestReconcileCmd_ReturnsNonZeroOnError verifies that a dial failure
// surfaces as a non-nil RunE return (which cobra propagates as a
// non-zero exit code).
func TestReconcileCmd_ReturnsNonZeroOnError(t *testing.T) {
	dialErr := errors.New("simulated supervisor unreachable")
	uninstall := setReconcileDialFnForTest(func(ctx context.Context, apply bool) (api.ReconcileResponse, error) {
		return api.ReconcileResponse{}, dialErr
	})
	defer uninstall()

	cmd := newReconcileCmdReal()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute returned nil; want non-nil error so cobra exits non-zero")
	}
	if !errors.Is(err, dialErr) && !strings.Contains(err.Error(), "simulated supervisor unreachable") {
		t.Errorf("error did not carry root cause; got %v", err)
	}
}

// TestReconcileCmd_JSONFlagEmitsValidJSON verifies that --json output
// is parseable JSON containing the response fields.
func TestReconcileCmd_JSONFlagEmitsValidJSON(t *testing.T) {
	fakeResp := api.ReconcileResponse{
		DryRun:       true,
		DriftCount:   1,
		AppliedCount: 0,
		Drift: []api.DriftEntry{
			{
				TaskName:       `\mcp-local-hub-foo-default`,
				SchedulerState: api.ReconcileSchedulerStateStopped,
				IntentDesired:  api.ReconcileIntentDesiredRunning,
				SMState:        api.StIdle,
				Action:         api.ReconcileActionPostEvIntentUpdate,
			},
		},
	}
	uninstall := setReconcileDialFnForTest(func(ctx context.Context, apply bool) (api.ReconcileResponse, error) {
		return fakeResp, nil
	})
	defer uninstall()

	cmd := newReconcileCmdReal()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var decoded api.ReconcileResponse
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("decode JSON output: %v; raw:\n%s", err, buf.String())
	}
	if decoded.DriftCount != 1 {
		t.Errorf("decoded.DriftCount = %d, want 1", decoded.DriftCount)
	}
	if len(decoded.Drift) != 1 || decoded.Drift[0].TaskName != `\mcp-local-hub-foo-default` {
		t.Errorf("decoded drift mismatch: %+v", decoded.Drift)
	}
}
