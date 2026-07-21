// Package cli — operator-facing dry-run preview for the Phase 4-E1
// dual-intent collapse (adversarial review P3-3).
//
// Usage:
//
//	mcphub intent-collapse --check   # dry-run: print the per-task merge
//	                                 # report against the LIVE state-dir;
//	                                 # NO write, NO backup.
//
// The collapse itself (the destructive in-place merge that folds
// daemon-intent.json stop overrides into supervisor-intent.json's stops
// sub-block) runs as part of the E-deploy flow — it is NOT exposed as a
// mutating operator command in this PR. Only the read-only --check preview
// is wired so an operator can validate the merge against live state BEFORE
// deploying E (spec §15 P1-c (i) + §12 Phase 4).
//
// Exit codes:
//
//	0  — success (report printed)
//	1  — state-dir resolution / read / merge-compute failure
package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
)

// intentCollapseCheckFn is the package-private indirection for
// api.CheckDaemonIntentCollapse. Tests swap this with a fake so the command
// can be exercised without a populated state directory.
var intentCollapseCheckFn = api.CheckDaemonIntentCollapse

// intentCollapseStateDirFn is the package-private indirection for the read-only
// state-dir resolver so a test can point the command at a t.TempDir without
// touching the real per-user state directory or creating a first-run dir.
var intentCollapseStateDirFn = api.DaemonStateDirReadOnly

// setIntentCollapseCheckFnForTest installs a test check closure. Returns an
// uninstall function tests defer to restore production wiring.
func setIntentCollapseCheckFnForTest(fn func(stateDir string, now time.Time) (api.DaemonIntentCollapseResult, error)) func() {
	prev := intentCollapseCheckFn
	intentCollapseCheckFn = fn
	return func() { intentCollapseCheckFn = prev }
}

// setIntentCollapseStateDirFnForTest installs a test state-dir resolver.
// Returns an uninstall function tests defer to restore production wiring.
func setIntentCollapseStateDirFnForTest(fn func() (string, error)) func() {
	prev := intentCollapseStateDirFn
	intentCollapseStateDirFn = fn
	return func() { intentCollapseStateDirFn = prev }
}

func newIntentCollapseCmdReal() *cobra.Command {
	var check bool
	c := &cobra.Command{
		Use:   "intent-collapse",
		Short: "Preview the dual-intent (daemon-intent → supervisor-intent stops) merge",
		// Hidden: deploy-time preview tool for the Phase 4-E1 collapse.
		// Read-only developer/deployer diagnostic, not an operator command.
		//
		// Discoverability loss ACCEPTED (2026-07-20): no runtime error names
		// this command, so hiding removes it from tab-completion as well as
		// from --help, leaving it with no discovery surface. That is correct
		// here because the audience is a deployer running a known migration
		// step from the spec, not an operator hunting for a tool. Contrast
		// `scheduler` / `reconcile`, which are operator-facing repair tools
		// and were therefore kept visible under Maintenance.
		Hidden: true,
		Long: `Computes the Phase 4-E1 dual-intent collapse against the LIVE
state directory and prints the per-task merge decisions WITHOUT touching
disk. Use this to validate what the deploy-time collapse would do before
deploying E.

The collapse folds the legacy daemon-intent.json stop overrides into the
supervisor-intent.json stops sub-block. The preview re-evaluates each
stop's IsActiveStop(now) so expired / stale / re-enabled (Desired=running)
stops are reported as dropped, exactly as the real merge would persist.

Per-task action vocabulary:
  add          — daemon-intent.json carries an active stop the stops
                 sub-block did not have; the merge would add it.
  update       — both sources carry the task but the record differs; the
                 merge would overwrite the sub-block with the live value.
  drop-expired — the baseline had the stop but it is no longer active at
                 now (TTL expired, stale-bound passed, or Desired=running);
                 the merge would drop it.

Only the read-only --check preview is exposed here; the destructive merge
runs as part of the E-deploy flow.

Examples:
  mcphub intent-collapse --check   # dry-run preview against live state`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !check {
				return fmt.Errorf("intent-collapse: only the read-only preview is available; pass --check to print the dry-run merge report")
			}
			stateDir, err := intentCollapseStateDirFn()
			if err != nil {
				return fmt.Errorf("intent-collapse: resolve state dir: %w", err)
			}
			// now=zero → CheckDaemonIntentCollapse uses time.Now().UTC().
			res, err := intentCollapseCheckFn(stateDir, time.Time{})
			if err != nil {
				return err
			}
			return printIntentCollapseReport(cmd.OutOrStdout(), stateDir, res)
		},
	}
	c.Flags().BoolVar(&check, "check", false,
		"print the dry-run merge report against the live state-dir (no write, no backup); currently required")
	return c
}

// printIntentCollapseReport formats the dry-run merge result for human
// reading: a header line with the state-dir + counts, then one row per
// per-task decision. A no-delta result prints an explicit "already collapsed"
// line so an operator can distinguish "nothing to do" from "no output".
func printIntentCollapseReport(w io.Writer, stateDir string, res api.DaemonIntentCollapseResult) error {
	if _, err := fmt.Fprintf(w, "state-dir: %s\nmode: check (dry-run, no write)\nmerge changes: %t\nstops after merge: %d\nper-task decisions: %d\n",
		stateDir, res.Changed, len(res.MergedStops), len(res.Entries)); err != nil {
		return err
	}
	if len(res.Entries) == 0 {
		if res.Changed {
			_, err := fmt.Fprintln(w, "bookkeeping compaction: pruning redundant legacy-stop watermarks (no daemon lifecycle change)")
			return err
		}
		_, err := fmt.Fprintln(w, "no per-task changes — daemon-intent.json stops already reflected in supervisor-intent.json")
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	headerFmt := "%-45s %-14s %s\n"
	if _, err := fmt.Fprintf(w, headerFmt, "TASK", "ACTION", "REASON"); err != nil {
		return err
	}
	for _, e := range res.Entries {
		reason := e.Reason
		if reason == "" {
			reason = "-"
		}
		if _, err := fmt.Fprintf(w, headerFmt, e.TaskName, string(e.Action), reason); err != nil {
			return err
		}
	}
	return nil
}
