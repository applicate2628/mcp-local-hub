// Package cli — register / unregister / workspaces commands for the
// workspace-scoped lazy-proxy flow (Phase 3, M3 of the 2026-04-20 plan).
//
// These three commands are thin wrappers over api.Register / api.Unregister
// and direct reads of the registry. All behavior lives in internal/api so
// the CLI and future GUI frontends share one code path.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/buildinfo"
	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/gui"
	"mcp-local-hub/internal/scheduler"

	"github.com/spf13/cobra"
)

// relayStdioClientNamesForHelp renders the relay-stdio client ids for
// `mcphub register --help`, DERIVED from the client registry rather than
// re-typed. The carve-out the help describes is enforced by
// clients.IsRelayStdio (api.defaultClientBindingsNow filters on it), so
// enumerating the names by hand here would silently go stale the first time a
// relay-stdio adapter is added or reclassified — help that contradicts the
// behavior is worse than help that omits it. Order follows
// clients.SupportedClientNames() (registry order), matching every other
// client-ordered surface.
func relayStdioClientNamesForHelp() string {
	var names []string
	for _, name := range clients.SupportedClientNames() {
		if clients.IsRelayStdio(name) {
			names = append(names, name)
		}
	}
	return strings.Join(names, ", ")
}

// newRegisterCmdReal is the concrete cobra.Command wired by root.go's stub.
// Usage: `mcphub register <workspace> [language...] [--no-weekly-refresh]`.
// Zero positional language args = default-all (every language declared in
// the shipped mcp-language-server manifest).
func newRegisterCmdReal() *cobra.Command {
	var weekly bool
	var noWeekly bool
	var supervised bool
	c := &cobra.Command{
		Use:   "register <workspace> [language...]",
		Short: "Register workspace-scoped mcp-language-server daemons (lazy-mode)",
		Long: `Allocate one lazy proxy per (workspace, language), create the launch
surface, and write managed entries into every default-install MCP client config
(claude-code, codex-cli by default). Cursor and the other clients are opt-in:
add them to your default-install set in the GUI under Settings -> Clients
(persisted as ` + "`clients.default_install`" + ` in gui-preferences.yaml) and register
will bind them too. There is no --clients flag on register, and editing the
shipped mcp-language-server manifest does not work — it is embedded in the
binary.

One carve-out versus ` + "`mcphub install`" + `: register SKIPS relay-stdio clients
(` + relayStdioClientNamesForHelp() + `) even when your default-install set
selects them, and prints a warning naming each one. A workspace LSP proxy is
reached by URL, and those clients accept only a stdio relay entry — not the
URL-only binding register writes. Reach them with ` + "`mcphub install`" + `, which
does emit the stdio relay form. If your default-install set names ONLY
relay-stdio clients, register still creates the proxy but binds no client at
all, and says so.

Lazy mode:
  - No LSP binary preflight at register time. A missing binary surfaces
    later at first tools/call via the LifecycleMissing state shown in
    ` + "`mcphub workspaces`" + `.
  - On scheduler-capable hosts, the default launch surface is the legacy
    per-language scheduled task.
  - On schedulerless Linux/macOS builds, plain register automatically uses
    the supervised proxy path so no Task Scheduler backend is required.
  - --supervised writes a supervisor-intent daemon row and asks the running
    supervisor to start the proxy as a Job-protected child process.
  - Proxy args: ` + "`daemon workspace-proxy --port <p> --workspace <ws> --language <lang>`" + `.
  - Entry names are ` + "`mcp-language-server-<lang>`" + `; a cross-workspace
    collision appends ` + "`-<4hex>`" + ` from the workspace key.

Weekly refresh enrollment:
  - --weekly-refresh         force-enroll new entries (override knob).
  - --no-weekly-refresh      force-skip new entries (override knob).
  - (neither)                read daemons.weekly_refresh_default from
                             settings (default: false). Memo D1.

Examples:
  mcphub register D:\projects\foo
  mcphub register D:\projects\foo python typescript rust --weekly-refresh
  mcphub register /home/u/web typescript --no-weekly-refresh # supervised on schedulerless hosts

See also: unregister, workspaces, status.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			workspace := args[0]
			var languages []string
			if len(args) > 1 {
				languages = args[1:]
			}
			if weekly && noWeekly {
				return fmt.Errorf("cannot use both --weekly-refresh and --no-weekly-refresh")
			}
			if err := ensureSupervisorForSchedulerlessRegister(cmd); err != nil {
				return err
			}
			explicit := weekly || noWeekly
			a := api.NewAPI()
			report, err := a.Register(workspace, languages, api.RegisterOpts{
				WeeklyRefreshExplicit:   explicit,
				WeeklyRefresh:           weekly,
				SupervisedProxy:         supervised,
				ManagedRouterAuthorizer: registerManagedRouterAuthorizer(),
				Writer:                  cmd.OutOrStdout(),
			})
			if err != nil {
				return err
			}
			printRegisterReport(cmd, report)
			return nil
		},
	}
	c.Flags().BoolVar(&weekly, "weekly-refresh", false,
		"force-enroll new entries in weekly refresh (override daemons.weekly_refresh_default)")
	c.Flags().BoolVar(&noWeekly, "no-weekly-refresh", false,
		"force-skip new entries from weekly refresh (override daemons.weekly_refresh_default)")
	c.Flags().BoolVar(&supervised, "supervised", false,
		"start LSP proxies through supervisor-intent as Job-protected supervisor children")
	return c
}

// registerManagedRouterAuthorizer is the CLI composition boundary. Discovery
// is read-only: PidportPathNoCreate resolves a candidate path without creating
// the GUI state directory, while all trust decisions remain in internal/gui.
func registerManagedRouterAuthorizer() api.ManagedRouterAuthorizer {
	pidportPath, _ := gui.PidportPathNoCreate()
	currentExecutable, _ := os.Executable()
	version, _, _ := buildinfo.Get()
	return gui.NewManagedRouterAuthorizer(pidportPath, currentExecutable, version)
}

var registerSchedulerUnavailableForHost = func() (bool, error) {
	if _, err := scheduler.New(); err != nil {
		if api.SchedulerUnavailableError(err) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

var registerResolveMCPHubBinary = resolveMCPHubBinary
var registerEnsureSupervisorRunning = ensureSupervisorRunning

func ensureSupervisorForSchedulerlessRegister(cmd *cobra.Command) error {
	schedulerless, err := registerSchedulerUnavailableForHost()
	if err != nil {
		return fmt.Errorf("check scheduler availability before LSP register: %w", err)
	}
	if !schedulerless {
		return nil
	}
	supervisorBin, err := registerResolveMCPHubBinary()
	if err != nil {
		return fmt.Errorf("schedulerless LSP register requires a running supervisor, but mcphub binary resolution failed: %w; run `mcphub supervise` from another shell and retry", err)
	}
	ctx := context.Background()
	if cmd != nil && cmd.Context() != nil {
		ctx = cmd.Context()
	}
	owner, err := registerEnsureSupervisorRunning(ctx, supervisorBin, false, 15*time.Second)
	if err != nil {
		return fmt.Errorf("schedulerless LSP register requires a running supervisor: %w; run `mcphub supervise` from another shell and retry", err)
	}
	if owner != nil {
		var out io.Writer = os.Stdout
		if cmd != nil {
			out = cmd.OutOrStdout()
		}
		if owner.Spawned() {
			fmt.Fprintf(out, "supervisor: spawned PID %d for schedulerless LSP register\n", owner.Pid())
		} else {
			fmt.Fprintln(out, "supervisor: adopted for schedulerless LSP register")
		}
	}
	return nil
}

func printRegisterReport(cmd *cobra.Command, report *api.RegisterReport) {
	fmt.Fprintf(cmd.OutOrStdout(),
		"\nRegistered %d language(s) for workspace %s (key %s):\n",
		len(report.Entries), report.Workspace, report.WorkspaceKey)
	for _, e := range report.Entries {
		fmt.Fprintf(cmd.OutOrStdout(), "  %-12s port=%-5d task=%s\n",
			e.Language, e.Port, e.TaskName)
	}
	for _, warn := range report.Warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warn)
	}
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
