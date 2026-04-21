package cli

import (
	"encoding/json"
	"fmt"
	"time"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newStatusCmdReal() *cobra.Command {
	var jsonOut bool
	var probeHealth bool
	var workspaceScoped bool
	var forceMaterialize bool
	c := &cobra.Command{
		Use:   "status",
		Short: "Show state of all mcp-local-hub scheduler tasks",
		Long: `Print a table of every 'mcp-local-hub-*' Task Scheduler task with state,
port, PID, RAM, uptime, and next-run time (for scheduled tasks).

State column:
  Running    — port is bound, daemon process alive
  Starting   — scheduler is mid-launch; port not yet bound
  Scheduled  — task idle, no live daemon, but a future trigger will fire
               (e.g. -weekly-refresh tasks)
  Stopped    — task idle, no future trigger, no daemon. Run 'restart' to revive.
  Disabled   — scheduler marked task as disabled (rare)

--health adds an MCP protocol smoke test per Running daemon. The
column shows either the tool count returned by tools/list ("OK N")
or the error the MCP server returned ("ERR ..."). Running + port
bound is NOT the same as 'MCP protocol responds' — a daemon can
be alive while its upstream backend (e.g. gdb/lldb binaries) is
missing, or while the subprocess is wedged. --health adds ~1-3 s
of round-trip time per daemon; use it for diagnostic passes, not
routine polling.

--workspace-scoped filters to the lazy-proxy daemons created by
'mcphub register' and prints extra columns: LIFECYCLE (one of
configured/starting/active/missing/failed) and LAST_USED (relative
time since last tools/call). LAST_ERROR (truncated) shows the most
recent materialization failure on missing/failed rows.

--force-materialize (requires --health) sends a real tools/call to
each workspace-scoped proxy to probe the heavy backend. Without this
flag --health preserves the lazy contract (synthetic handshake only;
backend stays cold). Use when you specifically want to verify that
LSP binaries are installed and the proxy can materialize them.

Examples:
  mcphub status                                 # pretty table
  mcphub status --json                          # machine-readable
  mcphub status --health                        # + MCP-level probe (synthetic)
  mcphub status --workspace-scoped              # lazy-proxy rows only
  mcphub status --health --force-materialize    # probe LSP backends

Troubleshooting:
  - All tasks showing Stopped? The mcphub binary may have moved.
    'mcphub setup' + 'mcphub scheduler upgrade' fixes that in one pass.
  - Some tasks Stopped, others Running? Restart the Stopped ones:
    'mcphub restart --server <name>'.

See also: restart, stop, logs, scheduler upgrade.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			rows, err := a.StatusWithOpts(api.StatusOpts{
				ProbeHealth:      probeHealth,
				ForceMaterialize: forceMaterialize,
			})
			if err != nil {
				return err
			}
			if workspaceScoped {
				rows = filterWorkspaceScoped(rows)
			}
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			if workspaceScoped {
				return printWorkspaceScopedTable(cmd, rows, probeHealth)
			}
			return printDefaultStatusTable(cmd, rows, probeHealth)
		},
	}
	c.Flags().BoolVar(&jsonOut, "json", false, "machine-readable JSON output")
	c.Flags().BoolVar(&probeHealth, "health", false, "MCP-level smoke test: initialize + tools/list per Running daemon")
	c.Flags().BoolVar(&workspaceScoped, "workspace-scoped", false,
		"show only workspace-scoped lazy-proxy daemons with LIFECYCLE / LAST_USED / LAST_ERROR columns")
	c.Flags().BoolVar(&forceMaterialize, "force-materialize", false,
		"for workspace-scoped daemons with --health: send a real tools/call to probe the heavy backend (triggers materialization). Default --health stays at proxy-alive only.")
	return c
}

// filterWorkspaceScoped returns only rows whose TaskName matches the
// lazy-proxy `mcp-local-hub-lsp-<key>-<lang>` pattern. Tested indirectly
// via the CLI test harness; the test relies on the DaemonStatus.Lifecycle
// field being non-empty for workspace-scoped rows after enrichment.
func filterWorkspaceScoped(rows []api.DaemonStatus) []api.DaemonStatus {
	out := rows[:0]
	for _, r := range rows {
		if r.Language != "" || r.Lifecycle != "" {
			out = append(out, r)
		}
	}
	return out
}

// printDefaultStatusTable renders the original (pre-M5) status layout.
// Kept intact so global-daemon operators see stable output.
func printDefaultStatusTable(cmd *cobra.Command, rows []api.DaemonStatus, probeHealth bool) error {
	headerFmt := "%-45s %-10s %-6s %-8s %-8s %-10s %s\n"
	headerArgs := []any{"NAME", "STATE", "PORT", "PID", "RAM(MB)", "UPTIME", "NEXT RUN"}
	if probeHealth {
		headerFmt = "%-45s %-10s %-6s %-8s %-8s %-10s %-20s %s\n"
		headerArgs = []any{"NAME", "STATE", "PORT", "PID", "RAM(MB)", "UPTIME", "HEALTH", "NEXT RUN"}
	}
	fmt.Fprintf(cmd.OutOrStdout(), headerFmt, headerArgs...)
	for _, r := range rows {
		ram := ""
		if r.RAMBytes > 0 {
			ram = fmt.Sprintf("%d", r.RAMBytes/(1024*1024))
		}
		uptime := ""
		if r.UptimeSec > 0 {
			uptime = fmt.Sprintf("%dh%dm", r.UptimeSec/3600, (r.UptimeSec%3600)/60)
		}
		port := ""
		if r.Port > 0 {
			port = fmt.Sprintf("%d", r.Port)
		}
		pid := ""
		if r.PID > 0 {
			pid = fmt.Sprintf("%d", r.PID)
		}
		if probeHealth {
			health := renderHealthCell(r)
			fmt.Fprintf(cmd.OutOrStdout(), headerFmt,
				r.TaskName, r.State, port, pid, ram, uptime, health, r.NextRun)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), headerFmt,
				r.TaskName, r.State, port, pid, ram, uptime, r.NextRun)
		}
	}
	return nil
}

// printWorkspaceScopedTable renders the --workspace-scoped layout. Columns:
// NAME, STATE, PORT, LIFECYCLE, LAST_USED, LAST_ERROR (+ HEALTH when
// --health is set).
func printWorkspaceScopedTable(cmd *cobra.Command, rows []api.DaemonStatus, probeHealth bool) error {
	headerFmt := "%-45s %-10s %-6s %-11s %-10s %s\n"
	headerArgs := []any{"NAME", "STATE", "PORT", "LIFECYCLE", "LAST_USED", "LAST_ERROR"}
	if probeHealth {
		headerFmt = "%-45s %-10s %-6s %-11s %-10s %-20s %s\n"
		headerArgs = []any{"NAME", "STATE", "PORT", "LIFECYCLE", "LAST_USED", "HEALTH", "LAST_ERROR"}
	}
	fmt.Fprintf(cmd.OutOrStdout(), headerFmt, headerArgs...)
	for _, r := range rows {
		port := ""
		if r.Port > 0 {
			port = fmt.Sprintf("%d", r.Port)
		}
		lifecycle := r.Lifecycle
		if lifecycle == "" {
			lifecycle = "-"
		}
		lastUsed := relativeStatusLastUsed(r.LastToolsCallAt)
		lastErr := firstN(r.LastError, 40)
		if probeHealth {
			health := renderHealthCell(r)
			fmt.Fprintf(cmd.OutOrStdout(), headerFmt,
				r.TaskName, r.State, port, lifecycle, lastUsed, health, lastErr)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), headerFmt,
				r.TaskName, r.State, port, lifecycle, lastUsed, lastErr)
		}
	}
	return nil
}

// renderHealthCell formats a DaemonStatus.Health probe outcome. For
// workspace-scoped rows (Source=="proxy-synthetic") the cell annotates
// whether the probe only confirmed the synthetic handshake or also
// observed a materialized backend, so operators don't mistake a live
// lazy proxy for a healthy LSP binary.
//
// Decoding table for workspace-scoped rows:
//
//	Health.OK + Lifecycle==active              → "OK (materialized)"
//	Health.OK + Lifecycle==missing             → "OK (synth); backend missing"
//	Health.OK + Lifecycle==failed              → "OK (synth); backend ERR: ..."
//	Health.OK + Lifecycle∈{configured,starting,""} → "OK (synth)"
//	Health.OK==false                           → "ERR: <probe error>"
//
// Global-daemon rows (Source=="") render the original OK (<count>)
// format so upstream output stays unchanged.
func renderHealthCell(r api.DaemonStatus) string {
	if r.Health == nil {
		return "—"
	}
	if !r.Health.OK {
		return "ERR: " + firstN(r.Health.Err, 40)
	}
	if r.Health.Source != "proxy-synthetic" {
		return fmt.Sprintf("OK (%d)", r.Health.ToolCount)
	}
	// Workspace-scoped: compose with Lifecycle to distinguish proxy-only
	// vs backend-materialized success paths.
	switch r.Lifecycle {
	case "active":
		return "OK (materialized)"
	case "missing":
		return "OK (synth); backend missing"
	case "failed":
		return "OK (synth); backend ERR: " + firstN(r.LastError, 30)
	default:
		return "OK (synth)"
	}
}

// relativeStatusLastUsed is the status-table variant of LAST_USED
// formatting. Zero time → "-"; elapsed rendered coarsely.
func relativeStatusLastUsed(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	elapsed := time.Since(t)
	switch {
	case elapsed < time.Minute:
		return fmt.Sprintf("%ds ago", int(elapsed.Seconds()))
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm ago", int(elapsed.Minutes()))
	case elapsed < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(elapsed.Hours()))
	case elapsed < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(elapsed.Hours()/24))
	}
	return t.UTC().Format("2006-01-02")
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
