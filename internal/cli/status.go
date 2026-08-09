package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

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
		Short: "Show the state of every mcp-local-hub daemon",
		Long: `Print a table of every 'mcp-local-hub-*' daemon with state, port, PID,
RAM, uptime, and next-run time (where one applies).

Since v0.5 the supervisor owns daemon lifecycle, so state is read over
the supervisor IPC status seam. The legacy Task Scheduler scan remains
only as the fallback for a host running without that wiring.

State column:
  Running    — port is bound, daemon process alive
  Starting   — mid-launch; port not yet bound
  Scheduled  — no live daemon, but a future trigger will fire
               (e.g. -weekly-refresh tasks)
  Stopped    — no future trigger, no daemon. Run 'restart' to revive.
  Disabled   — marked disabled (rare)

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
			// Default `mcphub status` (no --health / --force-materialize
			// / --workspace-scoped flags) routes through StatusContext
			// which prefers the supervisor IPC seam (canonical v0.5.0
			// view: 13 daemons). The --health / --force-materialize /
			// --workspace-scoped paths need the legacy scheduler-scan
			// enrichment (port-listen probe, manifest merge, registry
			// load for Lifecycle/LastToolsCallAt/LastError, MCP
			// initialize round-trip) so they keep using StatusWithOpts.
			// PR #215 fix: before this routing, even the bare
			// `mcphub status` invocation returned only the supervisor
			// scheduler row because StatusWithOpts queries
			// scheduler.List which only has 1 mcp-local-hub-* task in
			// v0.5.0 (the supervisor LogonTrigger entry).
			//
			// PR #215 r2 fix (codex review):
			//   Finding 1: workspaceScoped added to the routing gate.
			//     The IPC path does not populate Lifecycle /
			//     LastToolsCallAt / LastError, so
			//     `mcphub status --workspace-scoped` (without --health)
			//     pre-r2 silently rendered empty LIFECYCLE / LAST_USED
			//     / LAST_ERROR columns. Routing it through
			//     StatusWithOpts restores enrichment.
			//   Finding 2: a.Status() → a.StatusContext(cmd.Context())
			//     so the cobra command's ctx (Ctrl+C / shutdown)
			//     propagates into the supervisor IPC dial.
			var rows []api.DaemonStatus
			var err error
			if probeHealth || forceMaterialize || workspaceScoped {
				rows, err = a.StatusWithOpts(api.StatusOpts{
					ProbeHealth:      probeHealth,
					ForceMaterialize: forceMaterialize,
				})
			} else {
				rows, err = a.StatusContext(cmd.Context())
			}
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
			var printErr error
			if workspaceScoped {
				printErr = printWorkspaceScopedTable(cmd, rows, probeHealth)
			} else {
				printErr = printDefaultStatusTable(cmd, rows, probeHealth)
			}
			if printErr != nil {
				return printErr
			}
			warnUnmanagedStdioFromScan(cmd, a)
			return nil
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
// lazy-proxy `mcp-local-hub-lsp-<key>-<lang>` pattern. The predicate is
// structural (task-name) rather than field-based: Lifecycle and Language
// are registry-derived and may be empty if registry load or enrichment
// failed, which must not silently drop a real workspace-scoped row.
func filterWorkspaceScoped(rows []api.DaemonStatus) []api.DaemonStatus {
	out := rows[:0]
	for _, r := range rows {
		if api.IsLazyProxyTaskName(r.TaskName) {
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
		name := statusDisplayName(r)
		if probeHealth {
			health := renderHealthCell(r)
			fmt.Fprintf(cmd.OutOrStdout(), headerFmt,
				name, r.State, port, pid, ram, uptime, health, r.NextRun)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), headerFmt,
				name, r.State, port, pid, ram, uptime, r.NextRun)
		}
	}
	printSpawnHoldNotice(cmd, rows)
	return nil
}

// printSpawnHoldNotice explains, in plain words, any daemon the pre-spawn
// existence gate (P1.1) is holding because a path it needs is absent.
//
// Without this the table shows a column of "Stopped" rows and no reason — which
// is exactly the experience that cost an operator a working day: everything
// red, no explanation, the actual remedy (reinstall mcphub) invisible. The
// STATE column cannot carry it; a real sentence after the table can.
//
// When every held daemon shares one missing path — the incident's shape, since
// all daemons are started from one mcphub.exe — the shared cause is stated ONCE
// instead of repeated per row.
func printSpawnHoldNotice(cmd *cobra.Command, rows []api.DaemonStatus) {
	out := cmd.OutOrStdout()
	// The message embeds SpawnHoldPath, which is a filesystem path taken from
	// the supervisor descriptor — a workspace basename or install directory.
	// POSIX basenames may legally contain ESC / OSC / BEL bytes (see
	// stripTerminalControls below), so a deleted directory whose name carries
	// control sequences would otherwise emit them raw here and spoof rows in
	// the operator's terminal. spawnHoldDaemonLabel already strips; the message
	// must too, or the hardening has a hole exactly where attacker-influenced
	// text is most likely to appear.
	if fleet := DeriveFleetWideSpawnHold(rows); fleet != nil {
		fmt.Fprintf(out, "\n! %d servers cannot start, all for the same reason: %s\n",
			fleet.Count, stripTerminalControls(fleet.Message))
		return
	}
	for _, r := range rows {
		if r.SpawnHoldReason == "" {
			continue
		}
		fmt.Fprintf(out, "\n! %s cannot start: %s\n",
			spawnHoldDaemonLabel(r),
			stripTerminalControls(spawnHoldOperatorMessage(r.SpawnHoldReason, r.SpawnHoldPath)))
	}
}

// spawnHoldDaemonLabel names the daemon in a hold notice, never blank.
//
// statusDisplayName falls back to TaskName, which can be empty; an
// unidentifiable "! cannot start" line would tell the operator something is
// broken without saying WHAT — the failure this whole feature exists to
// prevent. Server/daemon is always populated and is the identity the operator
// recognizes anyway ("memory", "fetch"). Terminal controls are stripped for the
// same reason the rest of this file strips them: these names come from
// client-config keys and workspace basenames, which may legally contain ESC/BEL
// bytes that would otherwise spoof rows in a terminal.
func spawnHoldDaemonLabel(r api.DaemonStatus) string {
	if name := statusDisplayName(r); name != "" {
		return name
	}
	label := r.Server
	if r.Daemon != "" && r.Daemon != "default" {
		label += "/" + r.Daemon
	}
	if label == "" {
		return "(unnamed daemon)"
	}
	return stripTerminalControls(label)
}

var statusScanFn = func(a *api.API) (*api.ScanResult, error) {
	return a.Scan()
}

func warnUnmanagedStdioFromScan(cmd *cobra.Command, a *api.API) {
	result, err := statusScanFn(a)
	if err != nil || result == nil {
		return
	}
	names := api.UnmanagedStdioNames(result.Entries)
	if len(names) == 0 {
		return
	}
	for i := range names {
		names[i] = stripTerminalControls(names[i])
	}
	fmt.Fprintf(
		cmd.ErrOrStderr(),
		"⚠ %d unmanaged stdio MCP server(s) detected (configured directly in a client, bypassing the hub): %s\n  Adopt them via 'mcphub adopt' or the GUI Discovery screen to route them through the hub and stop orphan-process accumulation.\n",
		len(names),
		strings.Join(names, ", "),
	)
}

// statusDisplayName returns the friendly human-readable name for the table's
// NAME column: DaemonStatus.DisplayName when the backend resolved one
// ("serena · <project>", "<lang> @ <workspace>"), else the raw task name.
// The raw task_name always remains available verbatim via `mcphub status
// --json` for ops/scripting that key on the canonical identifier.
func statusDisplayName(r api.DaemonStatus) string {
	if r.DisplayName != "" {
		return stripTerminalControls(r.DisplayName)
	}
	return stripTerminalControls(r.TaskName)
}

// stripTerminalControls removes terminal control AND bidi/format characters from
// text about to be rendered in the human-readable status tables / warnings.
// DisplayName and scan-derived server names can include a workspace basename or a
// client-config key, and POSIX basenames / config keys may legally contain bytes
// such as newlines, ESC, OSC, or BEL (CONTROL, category Cc) — printing those
// verbatim would let a malicious name spoof rows or trigger terminal side effects.
// It ALSO strips Unicode FORMAT / separator runes (Cf: bidi overrides U+202E/U+202D,
// isolates U+2066-2069, zero-width ZWSP; Zl/Zp line/paragraph separators) which
// IsControl (Cc only) misses but which visually reorder or hide the rest of a line
// in bidi-rendering terminals (commission fable P3). JSON output is unchanged
// (emitted through json.Encoder).
func stripTerminalControls(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return -1
		}
		return r
	}, s)
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
		name := statusDisplayName(r)
		if probeHealth {
			health := renderHealthCell(r)
			fmt.Fprintf(cmd.OutOrStdout(), headerFmt,
				name, r.State, port, lifecycle, lastUsed, health, lastErr)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), headerFmt,
				name, r.State, port, lifecycle, lastUsed, lastErr)
		}
	}
	// Same notice as printDefaultStatusTable. Its absence here was an
	// asymmetry, not a decision: `--workspace-scoped` is the view most likely
	// to be showing a daemon held for a MISSING WORKSPACE, so it is the last
	// table that should stay silent about it.
	printSpawnHoldNotice(cmd, rows)
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
	// The built-in route front daemon's probe proves the MCP lifecycle on
	// /serena/mcp and never enumerates tools (see api.RouteFrontHealthSource),
	// so its ToolCount is structurally 0 — printing "OK (0)" would read as
	// "healthy but exposes nothing".
	if r.Health.Source == api.RouteFrontHealthSource {
		return "OK (lifecycle)"
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
