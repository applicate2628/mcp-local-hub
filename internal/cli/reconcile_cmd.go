// Package cli — Phase A.3 (plan v10 §A.3, 2026-05-20) cobra command
// for `mcphub reconcile`.
//
// Usage:
//
//	mcphub reconcile           # dry-run: print drift report, no mutations
//	mcphub reconcile --apply   # apply: trigger SM transitions to align scheduler state with intent
//
// The command sends an IPC `reconcile` verb to the running supervisor
// via api.DialSupervisorIPCReconcile, prints a human-readable drift
// table to stdout, and (in --apply mode) also reports the count of
// EvIntentUpdate transitions the supervisor dispatched.
//
// Exit codes:
//
//	0  — success (drift report printed; in --apply mode, transitions dispatched)
//	1  — IPC dial / read / decode failure (supervisor unreachable or wire error)
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
)

// reconcileDialFn is the package-private indirection for
// api.DialSupervisorIPCReconcile. Tests swap this with a fake that
// returns a canned api.ReconcileResponse without spinning up a real
// supervisor.
var reconcileDialFn = api.DialSupervisorIPCReconcile

// setReconcileDialFnForTest installs a test dial closure. Returns an
// uninstall function tests defer to restore the production wiring.
// Production callers never invoke this.
func setReconcileDialFnForTest(fn func(ctx context.Context, apply bool) (api.ReconcileResponse, error)) func() {
	prev := reconcileDialFn
	reconcileDialFn = fn
	return func() { reconcileDialFn = prev }
}

func newReconcileCmdReal() *cobra.Command {
	var apply bool
	var jsonOut bool
	c := &cobra.Command{
		Use:   "reconcile",
		Short: "Surface and (optionally) apply intent/scheduler drift to the running supervisor",
		Long: `Walks the supervisor-intent.json daemons, the scheduler-registered
mcp-local-hub-* tasks, and the per-task daemon-intent.json desired
state; surfaces every (task, drift-class) pair as a row in the drift
table; and (with --apply) posts EvIntentUpdate per actionable drift
entry so the supervisor's state machine drives the corrective
Run/Stop/Delete transitions WITHOUT a supervisor cold-restart.

By default the command is a dry-run: it prints the drift table and
exits without dispatching any state-machine events. Pass --apply to
have the supervisor act on the drift.

Drift action vocabulary:
  post_ev_intent_update  — fixable in-place; --apply dispatches EvIntentUpdate
  no_op                  — scheduler and intent already aligned; informational
  needs_manual_review    — orphan scheduler task, missing scheduler entry, or
                           an unrecognized scheduler state. Operator must
                           investigate (e.g. 'mcphub install --upgrade'
                           re-registers a missing scheduler entry).

Examples:
  mcphub reconcile               # print drift report
  mcphub reconcile --apply       # apply actionable drift
  mcphub reconcile --json        # machine-readable output for scripting

See also: status, install --upgrade.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := reconcileDialFn(cmd.Context(), apply)
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(resp)
			}
			return printReconcileTable(cmd.OutOrStdout(), resp)
		},
	}
	c.Flags().BoolVar(&apply, "apply", false,
		"dispatch EvIntentUpdate per drift entry whose Action is post_ev_intent_update (default: dry-run)")
	c.Flags().BoolVar(&jsonOut, "json", false, "machine-readable JSON output")
	return c
}

// printReconcileTable formats the reconcile response for human reading.
// Header summary first (mode, drift count, applied count), then the
// per-entry table.
func printReconcileTable(w io.Writer, resp api.ReconcileResponse) error {
	mode := "dry-run"
	if !resp.DryRun {
		mode = "apply"
	}
	if _, err := fmt.Fprintf(w, "mode: %s\ndrift entries: %d\napplied transitions: %d\n",
		mode, resp.DriftCount, resp.AppliedCount); err != nil {
		return err
	}
	if resp.DriftCount == 0 {
		_, err := fmt.Fprintln(w, "no drift — scheduler state and intent are already aligned")
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	headerFmt := "%-45s %-10s %-10s %-15s %s\n"
	if _, err := fmt.Fprintf(w, headerFmt,
		"TASK", "SCHED", "INTENT", "SM_STATE", "ACTION"); err != nil {
		return err
	}
	for _, e := range resp.Drift {
		sm := string(e.SMState)
		if sm == "" {
			sm = "-"
		}
		if _, err := fmt.Fprintf(w, headerFmt,
			e.TaskName, e.SchedulerState, e.IntentDesired, sm, e.Action); err != nil {
			return err
		}
	}
	return nil
}
