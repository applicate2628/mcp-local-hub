// Package cli — register / unregister / workspaces commands for the
// workspace-scoped lazy-proxy flow (Phase 3, M3 of the 2026-04-20 plan).
//
// These three commands are thin wrappers over api.Register / api.Unregister
// and direct reads of the registry. All behavior lives in internal/api so
// the CLI and future GUI frontends share one code path.
package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

// newRegisterCmdReal is the concrete cobra.Command wired by root.go's stub.
// Usage: `mcphub register <workspace> [language...] [--no-weekly-refresh]`.
// Zero positional language args = default-all (every language declared in
// the shipped mcp-language-server manifest).
func newRegisterCmdReal() *cobra.Command {
	var noWeekly bool
	c := &cobra.Command{
		Use:   "register <workspace> [language...]",
		Short: "Register workspace-scoped mcp-language-server daemons (lazy-mode)",
		Long: `Allocate one lazy proxy per (workspace, language), create the scheduler
task that launches it, and write managed entries into every installed MCP
client config (codex-cli, claude-code, gemini-cli).

Lazy mode:
  - No LSP binary preflight at register time. A missing binary surfaces
    later at first tools/call via the LifecycleMissing state shown in
    ` + "`mcphub workspaces`" + `.
  - Scheduler task args: ` + "`daemon workspace-proxy --port <p> --workspace <ws> --language <lang>`" + `.
  - Entry names are ` + "`mcp-language-server-<lang>`" + `; a cross-workspace
    collision appends ` + "`-<4hex>`" + ` from the workspace key.

Examples:
  mcphub register D:\projects\foo                         # every language
  mcphub register D:\projects\foo python typescript rust  # three languages
  mcphub register /home/u/web typescript --no-weekly-refresh

See also: unregister, workspaces, status.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			workspace := args[0]
			var languages []string
			if len(args) > 1 {
				languages = args[1:]
			}
			// nil slice = default-all semantics inside api.Register.
			a := api.NewAPI()
			report, err := a.Register(workspace, languages, api.RegisterOpts{
				WeeklyRefresh: !noWeekly,
				Writer:        cmd.OutOrStdout(),
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"\nRegistered %d language(s) for workspace %s (key %s):\n",
				len(report.Entries), report.Workspace, report.WorkspaceKey)
			for _, e := range report.Entries {
				fmt.Fprintf(cmd.OutOrStdout(), "  %-12s port=%-5d task=%s\n",
					e.Language, e.Port, e.TaskName)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&noWeekly, "no-weekly-refresh", false,
		"set weekly_refresh=false for every new registry entry")
	return c
}

// newUnregisterCmdReal: `mcphub unregister <workspace> [language...]`.
// With no language args, removes every registered language for the
// workspace. With one or more, removes only those.
func newUnregisterCmdReal() *cobra.Command {
	c := &cobra.Command{
		Use:   "unregister <workspace> [language...]",
		Short: "Remove workspace-scoped daemons (full or per-language)",
		Long: `Remove scheduler tasks, client-config entries, and registry rows for a
workspace. With no language arguments, every registered language for the
workspace is removed. With one or more language names, only those are
removed (others stay intact).

Examples:
  mcphub unregister D:\projects\foo                     # remove all
  mcphub unregister D:\projects\foo python typescript   # remove two

See also: register, workspaces.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			workspace := args[0]
			var langs []string
			if len(args) > 1 {
				langs = args[1:]
			}
			a := api.NewAPI()
			report, err := a.Unregister(workspace, langs)
			if err != nil {
				return err
			}
			if len(langs) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(),
					"Removed %s (key %s): %d language(s)\n",
					report.Workspace, report.WorkspaceKey, len(report.Removed))
			} else {
				fmt.Fprintf(cmd.OutOrStdout(),
					"Removed %d language(s) from %s (key %s): %v\n",
					len(report.Removed), report.Workspace, report.WorkspaceKey, report.Removed)
			}
			for _, warn := range report.Warnings {
				fmt.Fprintf(cmd.OutOrStderr(), "warning: %s\n", warn)
			}
			return nil
		},
	}
	return c
}

// newWorkspacesCmdReal: `mcphub workspaces [--json]`. Lists the registry.
// Columns: WORKSPACE, LANG, PORT, BACKEND, LIFECYCLE, LAST_USED, PATH.
// LAST_USED is a relative time (e.g. "5m ago") or "-" when the daemon has
// not yet served a tools/call.
func newWorkspacesCmdReal() *cobra.Command {
	var jsonOut bool
	c := &cobra.Command{
		Use:   "workspaces",
		Short: "List registered workspaces and their languages",
		Long: `Enumerate every (workspace, language) tuple in the registry. Default
output is a human-readable table; --json emits the full WorkspaceEntry
array verbatim (including LIFECYCLE, LAST_ERROR, and timestamp fields).

Lifecycle states:
  configured  — proxy scheduled; backend NOT materialized yet
  starting    — materialization in-flight (singleflight active)
  active      — backend materialized and healthy
  missing     — materialization attempted; LSP binary not on PATH
  failed      — materialization attempted; failed for any other reason`,
		RunE: func(cmd *cobra.Command, args []string) error {
			regPath, err := api.DefaultRegistryPath()
			if err != nil {
				return err
			}
			reg := api.NewRegistry(regPath)
			if err := reg.Load(); err != nil {
				if os.IsNotExist(err) {
					reg = api.NewRegistry(regPath)
				} else {
					return err
				}
			}
			entries := append([]api.WorkspaceEntry(nil), reg.Workspaces...)
			sort.Slice(entries, func(i, j int) bool {
				if entries[i].WorkspacePath != entries[j].WorkspacePath {
					return entries[i].WorkspacePath < entries[j].WorkspacePath
				}
				return entries[i].Language < entries[j].Language
			})
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				if entries == nil {
					entries = []api.WorkspaceEntry{}
				}
				return enc.Encode(entries)
			}
			return printWorkspacesTable(cmd, entries)
		},
	}
	c.Flags().BoolVar(&jsonOut, "json", false, "machine-readable JSON output")
	return c
}

// printWorkspacesTable renders the table form of the registry. Extracted so
// tests can exercise the exact column layout independent of the cobra
// dispatch path.
func printWorkspacesTable(cmd *cobra.Command, entries []api.WorkspaceEntry) error {
	fmt.Fprintf(cmd.OutOrStdout(), "%-12s %-12s %-6s %-20s %-11s %-10s %s\n",
		"WORKSPACE", "LANG", "PORT", "BACKEND", "LIFECYCLE", "LAST_USED", "PATH")
	for _, e := range entries {
		fmt.Fprintf(cmd.OutOrStdout(), "%-12s %-12s %-6d %-20s %-11s %-10s %s\n",
			e.WorkspaceKey, e.Language, e.Port, e.Backend,
			stateOrDash(e.Lifecycle),
			relativeLastUsed(e.LastToolsCallAt),
			e.WorkspacePath)
	}
	return nil
}

// stateOrDash returns "-" when s is empty, else s verbatim. Lifecycle
// strings are short enough to show unmodified; the empty value is an
// artifact of legacy YAML without the field, rendered as "-" so the
// column stays self-describing.
func stateOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// relativeLastUsed formats a LastToolsCallAt timestamp relative to now.
// Zero time → "-". Positive elapsed → "Xs ago" / "Xm ago" / "Xh ago" /
// "Xd ago". Capped at day granularity; anything older shows the date.
func relativeLastUsed(t time.Time) string {
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
