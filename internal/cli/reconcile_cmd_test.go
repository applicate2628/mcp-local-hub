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

type reconcilePartialOutputFailure struct{ cause error }

func (w reconcilePartialOutputFailure) Write([]byte) (int, error) { return 0, w.cause }

func TestReconcileCmdRetainsPartialResultOnError(t *testing.T) {
	remote := errors.New("controlled remote reconcile failure")
	output := errors.New("controlled partial output failure")
	response := api.ReconcileResponse{SerenaOrphansRepaired: 1, SerenaRepairOutcome: api.SerenaIntentRepairOutcomeError, SerenaRepairError: "original close failure"}
	restore := setReconcileDialFnForTest(func(context.Context, bool) (api.ReconcileResponse, error) { return response, remote })
	defer restore()
	for _, mode := range []string{"human", "json", "output-failure"} {
		t.Run(mode, func(t *testing.T) {
			cmd := newReconcileCmdReal()
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			var buf bytes.Buffer
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			args := []string{"--apply"}
			if mode == "json" {
				args = append(args, "--json")
			}
			if mode == "output-failure" {
				cmd.SetOut(reconcilePartialOutputFailure{cause: output})
			}
			cmd.SetArgs(args)
			err := cmd.Execute()
			if !errors.Is(err, remote) {
				t.Fatalf("remote cause lost: %v", err)
			}
			var retained *reconcileResultError
			if !errors.As(err, &retained) || retained.response.SerenaOrphansRepaired != 1 {
				t.Fatalf("caller dropped partial result: %v", err)
			}
			if mode == "json" {
				var got api.ReconcileResponse
				if json.Unmarshal(buf.Bytes(), &got) != nil || got.SerenaOrphansRepaired != 1 {
					t.Fatalf("partial JSON unavailable: %s", buf.String())
				}
			} else if mode == "human" && (!strings.Contains(buf.String(), "partial Serena repair result: repaired=1") || !strings.Contains(buf.String(), "readiness remains unconfirmed")) {
				t.Fatalf("partial facts hidden/promoted: %s", buf.String())
			}
			if mode == "output-failure" && !errors.Is(err, output) {
				t.Fatalf("secondary output failure lost: %v", err)
			}
		})
	}
}
