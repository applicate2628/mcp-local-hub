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
//	1  — IPC dial / read / decode failure (supervisor unreachable or wire error),
//	     OR (--apply only) the supervisor's serena registry/intent self-heal
//	     failed, was skipped for lock contention, or returned an unavailable /
//	     unknown outcome — see errSerenaRepairFailed and
//	     errSerenaRepairIncomplete below
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
)

// errSerenaRepairFailed is the sentinel `mcphub reconcile --apply` returns when
// the supervisor answered normally but its serena registry/intent self-heal did
// NOT complete (ReconcileResponse.SerenaRepairError non-empty).
//
// Exiting 0 there was the defect: a failed repair never lands the orphan in
// supervisor-intent.json, so it never appears in the drift report either, and
// the command printed `no drift — scheduler state and intent are already
// aligned` over a workspace the operator had just registered and could not use.
// Dry-run keeps exit 0 (it mutates nothing and a report is all it promised) but
// still PRINTS the failure.
var errSerenaRepairFailed = errors.New("serena registry/intent self-heal failed on the supervisor")

// errSerenaRepairIncomplete is the retryable `mcphub reconcile --apply`
// sentinel for a lock-skipped, absent, or unrecognized repair outcome. The
// supervisor's IPC frame and ordinary drift result remain valid, but Serena
// classification did not reach a verdict and cannot certify alignment.
var errSerenaRepairIncomplete = errors.New("serena registry/intent self-heal classification is incomplete on the supervisor")

type serenaRepairClassification struct {
	outcome    api.SerenaIntentRepairOutcome
	incomplete bool
	detail     string
}

// classifySerenaRepairOutcome is the sole CLI interpretation of the additive
// repair outcome wire field. Both human output and apply-mode exit policy use
// it so an old or future supervisor value cannot be presented as alignment by
// one surface and failure by another.
func classifySerenaRepairOutcome(resp api.ReconcileResponse) serenaRepairClassification {
	if resp.SerenaRepairError != "" {
		return serenaRepairClassification{
			outcome:    api.SerenaIntentRepairOutcomeError,
			incomplete: true,
			detail:     resp.SerenaRepairError,
		}
	}
	switch resp.SerenaRepairOutcome {
	case api.SerenaIntentRepairOutcomeCompleted:
		return serenaRepairClassification{outcome: api.SerenaIntentRepairOutcomeCompleted}
	case api.SerenaIntentRepairOutcomeSkippedRegistryLock:
		return serenaRepairClassification{
			outcome:    resp.SerenaRepairOutcome,
			incomplete: true,
			detail:     "the workspace registry lock remained contended",
		}
	case api.SerenaIntentRepairOutcomeSkippedIntentLock:
		return serenaRepairClassification{
			outcome:    resp.SerenaRepairOutcome,
			incomplete: true,
			detail:     "the supervisor-intent lock remained contended",
		}
	case api.SerenaIntentRepairOutcomeSkippedRemovalFenceProbe:
		return serenaRepairClassification{
			outcome:    resp.SerenaRepairOutcome,
			incomplete: true,
			detail:     "a pending Serena-removal liveness fence could not be probed",
		}
	case api.SerenaIntentRepairOutcomeError:
		return serenaRepairClassification{
			outcome:    resp.SerenaRepairOutcome,
			incomplete: true,
			detail:     "the supervisor reported a Serena repair error without causal text",
		}
	case "":
		return serenaRepairClassification{
			incomplete: true,
			detail:     "the Serena repair outcome is unavailable (restart or upgrade the supervisor)",
		}
	default:
		return serenaRepairClassification{
			outcome:    resp.SerenaRepairOutcome,
			incomplete: true,
			detail:     fmt.Sprintf("the Serena repair outcome %q is unknown (restart or upgrade the supervisor)", resp.SerenaRepairOutcome),
		}
	}
}

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
		// VISIBLE, under Maintenance. The supervisor reconciles continuously
		// on its own, so this is a manual inspection and repair hatch rather
		// than a normal operator step — but it is an operator-facing one,
		// and no runtime error names it, so the top-level listing and shell
		// completion are its only discovery surfaces (cobra drops hidden
		// commands from tab-completion as well as from --help). Same
		// rationale as `scheduler`; root.go groups it under Maintenance.
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
				if encErr := enc.Encode(resp); encErr != nil {
					return encErr
				}
			} else if printErr := printReconcileTable(cmd.OutOrStdout(), resp); printErr != nil {
				return printErr
			}
			// Report printed either way; now decide the exit. An apply-mode run
			// whose Serena classification did not reach a completed verdict must
			// not exit 0: the ordinary drift report cannot show an orphan that
			// never reached supervisor-intent.json.
			classification := classifySerenaRepairOutcome(resp)
			if apply && classification.incomplete {
				if classification.outcome == api.SerenaIntentRepairOutcomeError {
					return fmt.Errorf("%w: %s (the drift report above is still valid; "+
						"re-run `mcphub reconcile --apply` once the cause is resolved, and see "+
						"supervisor-events.log `serena-intent-repair-*` entries)",
						errSerenaRepairFailed, classification.detail)
				}
				return fmt.Errorf("%w: %s; re-run `mcphub reconcile --apply` after the contention or version skew is resolved",
					errSerenaRepairIncomplete, classification.detail)
			}
			return nil
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
	classification := classifySerenaRepairOutcome(resp)
	// A self-heal that never reached a verdict is printed FIRST and
	// unconditionally: the counts below and the drift table underneath are both
	// blind to the orphan it failed on, so this line is the only place the
	// operator can learn the report is incomplete.
	if classification.incomplete && classification.outcome == api.SerenaIntentRepairOutcomeError {
		verb := "serena orphan repair FAILED"
		if resp.DryRun {
			verb = "serena orphan repair PREVIEW failed"
		}
		if _, err := fmt.Fprintf(w, "%s: %s\n  (this pass could not materialize orphaned serena "+
			"workspaces; they are absent from the drift table below because they never reached "+
			"supervisor-intent.json)\n", verb, classification.detail); err != nil {
			return err
		}
	} else if classification.incomplete {
		verb := "serena orphan repair skipped"
		if resp.DryRun {
			verb = "serena orphan repair PREVIEW skipped"
		}
		if _, err := fmt.Fprintf(w, "%s: %s\n  (Serena classification is incomplete; re-run `mcphub reconcile --apply` after the lock holder finishes or the supervisor is upgraded)\n", verb, classification.detail); err != nil {
			return err
		}
	}
	// Serena orphan-repair preview/result (BLOCKING 3 fix): shown in BOTH
	// modes so a dry-run reconcile predicts exactly what the next --apply
	// would materialize, instead of hiding it as an apply-only side effect.
	if resp.SerenaOrphansRepaired > 0 || len(resp.SerenaOrphansDeferred) > 0 {
		verb := "would repair"
		if !resp.DryRun {
			verb = "repaired"
		}
		if _, err := fmt.Fprintf(w, "serena orphans %s: %d\n", verb, resp.SerenaOrphansRepaired); err != nil {
			return err
		}
		if len(resp.SerenaOrphansDeferred) > 0 {
			if _, err := fmt.Fprintf(w, "serena orphans deferred (run `mcphub migrate serena legacy-to-dynamic-pool`): %s\n",
				strings.Join(resp.SerenaOrphansDeferred, ", ")); err != nil {
				return err
			}
		}
	}
	if resp.DriftCount == 0 {
		// Never claim alignment when the self-heal above did not finish: the
		// orphan it failed on is exactly the drift this line would be denying.
		if classification.incomplete {
			_, err := fmt.Fprintf(w, "no drift against intent as read; Serena classification incomplete: %s\n", classification.detail)
			return err
		}
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
