package cli

import (
	"fmt"
	"io"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

// newUninstallCmdReal is the concrete cobra.Command wired by root.go. It is
// a thin wrapper over api.Uninstall: the api does the work and returns a
// structured report; the CLI renders that report to cmd.OutOrStdout().
//
// Ordering (post-bot-P1.1 fix):
//
//  1. api.Uninstall(server) — per-server flow runs FIRST. Marks
//     intents + emits server-uninstalled audit entries via
//     recordUninstallIntentForTasks (Task 10), then deletes each
//     `mcp-local-hub-<server>-*` task.
//  2. runUninstallWatchdog (only on per-server success) — disables/
//     deletes the global watchdog scheduled task IF this was the
//     last managed server. The partial-uninstall gate inside
//     runUninstallWatchdog already keeps the watchdog installed when
//     other servers remain (Codex bot P2). On per-server FAILURE the
//     watchdog stays installed regardless so it can keep recovering
//     daemons whose tasks may still be present (bot P1.1 fix).
//  3. Render the per-server report to stdout.
func newUninstallCmdReal() *cobra.Command {
	var server string
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove an installed MCP server (scheduler + client bindings)",
		Long: `Reverse of 'install': deletes the scheduler tasks and removes the
server's entry from every managed client config declared by the manifest.

What uninstall does:
  1. Reads the manifest to know which tasks + which clients to touch.
  2. Marks each 'mcp-local-hub-<server>-*' task as Desired=stopped +
     Reason=uninstalled (plan Task 10) BEFORE deleting.
  3. Deletes each 'mcp-local-hub-<server>-*' Task Scheduler task.
  4. Removes the server's entry from each client config.
  5. ONLY on success of (1)-(4): if this was the last managed server,
     removes the watchdog scheduled task and the Windows EventLog source
     'mcp-local-hub' (plan §60; POSIX no-op). When other managed servers
     remain, the watchdog stays installed for them. When the per-server
     uninstall failed, the watchdog also stays installed so auto-recovery
     keeps running for whatever tasks remain (bot P1.1 fix).
  6. Does NOT delete .bak-mcp-local-hub-* backup files — they remain on disk.
  7. Does NOT delete live daemon processes — Task Scheduler's task delete
     only removes task metadata; existing processes keep running until they
     exit naturally. Use 'mcphub stop --server <n>' first to kill them.

Examples:
  mcphub uninstall --server wolfram

Recovery:
  'mcphub rollback' restores the latest client config backup
  'mcphub rollback --original' restores the pristine pre-hub-ever sentinel

See also: install, stop, rollback, backups list.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			return runUninstall(cmd.OutOrStdout(), server, a.Uninstall, runUninstallWatchdog)
		},
	}
	c.Flags().StringVar(&server, "server", "", "server name")
	return c
}

// runUninstall is the testable orchestration body for `mcphub uninstall`.
// Function-parameter injection lets unit tests substitute fake
// implementations of `a.Uninstall(server)` and runUninstallWatchdog so the
// ordering contract can be exercised without spinning up the real
// scheduler / manifest pipeline.
//
// Ordering invariant (bot P1.1 fix):
//
//   - Per-server uninstall runs FIRST.
//   - Watchdog teardown is ONLY invoked when the per-server uninstall
//     returned no error. If api.Uninstall fails — even partially with
//     scheduler.Delete warnings rolled into the report — the watchdog
//     stays installed. The previous ordering (watchdog FIRST) would
//     remove auto-recovery wiring even when scheduler tasks survived,
//     leaving the user with installed-but-unrecoverable daemons.
//
// The race-during-teardown rationale that originally motivated the
// "watchdog FIRST" plan is preserved by the partial-uninstall gate
// inside runUninstallWatchdog: when other servers remain, the gate
// keeps the watchdog installed; when this is the last server, the
// per-server uninstall succeeds first and THEN the watchdog is
// removed (so a brief overlap with stale tasks cannot occur).
func runUninstall(
	out io.Writer,
	server string,
	doUninstall func(string) (*api.UninstallReport, error),
	doWatchdogUninstall func(io.Writer, string) error,
) error {
	if server == "" {
		return fmt.Errorf("--server is required")
	}
	// Step 1: per-server uninstall.
	report, err := doUninstall(server)
	if err != nil {
		// Bot P1.1: do NOT touch the watchdog when the per-server
		// uninstall failed. Daemons / scheduler tasks may still be
		// installed; auto-recovery must keep running for them.
		return err
	}
	// Step 2: render the per-server report.
	for _, name := range report.TasksDeleted {
		fmt.Fprintf(out, "✓ Deleted task: %s\n", name)
	}
	for _, warn := range report.TaskDeleteWarns {
		fmt.Fprintf(out, "⚠ %s\n", warn)
	}
	for _, client := range report.ClientsUpdated {
		fmt.Fprintf(out, "✓ Removed %s from %s\n", report.Server, client)
	}
	for _, warn := range report.ClientWarns {
		fmt.Fprintf(out, "⚠ %s\n", warn)
	}
	// Step 3: watchdog teardown. The helper's internal partial-uninstall
	// gate (Codex bot P2) keeps the watchdog installed when other
	// managed servers remain.
	if err := doWatchdogUninstall(out, server); err != nil {
		return err
	}
	fmt.Fprintln(out, "Uninstall complete. Client config backups (.bak-mcp-local-hub-*) remain on disk.")
	return nil
}
