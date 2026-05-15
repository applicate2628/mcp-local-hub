package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/buildinfo"
	"mcp-local-hub/internal/config"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

// newInstallCmdReal is the concrete cobra.Command wired by root.go's stub
// newInstallCmd. It is a thin wrapper over api.Install — all behavior lives
// in internal/api so CLI and future GUI share one code path.
func newInstallCmdReal() *cobra.Command {
	var server string
	var daemonFilter string
	var clientsFlag string
	var dryRun bool
	var all bool
	var allClients bool
	var reconcileHubMode bool
	var upgrade bool
	c := &cobra.Command{
		Use:   "install",
		Short: "Install an MCP server as shared daemon(s)",
		Long: `Install an MCP server from a manifest under servers/<name>/manifest.yaml.

What install does:
  1. Verifies mcphub is on PATH (prompts to 'setup' if not, in a terminal)
  2. Creates Windows Task Scheduler tasks — one per daemon, plus an
     optional weekly-refresh task if the manifest declares one
  3. Starts the scheduler tasks immediately (won't wait for next logon)
  4. Writes a timestamped backup for each client config it touches
  5. Patches each client's config per the manifest's client_bindings list:
     default clients are Claude Code, Codex CLI, and Cursor; Gemini CLI,
     Qwen CLI, VS Code, and Antigravity are opt-in via --clients or --all-clients

Examples:
  mcphub install --server serena               # default clients: claude-code,codex-cli,cursor
  mcphub install --server serena --clients qwen-cli,vscode
  mcphub install --server serena --all-clients # every manifest client binding
  mcphub install --server serena --daemon codex # install only one daemon
  mcphub install --server serena --dry-run     # preview actions, change nothing
  mcphub install --all                         # install every shipped manifest
  mcphub install --upgrade                     # stop daemons, copy this binary, restart

Prerequisites:
  - First-time users: run 'mcphub setup' once to canonicalize the binary
    at ~/.local/bin and register it on user PATH
  - Secrets (wolfram, paper-search-mcp): 'mcphub secrets set <key>' first
  - Windows: Task Scheduler backend only. Linux/macOS ship compile-only stubs.

See also: status, restart, uninstall, rollback, scheduler upgrade.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Bug-bash A7 minimal closure (#4): --upgrade is the
			// one-shot binary replacement entry point. Pre-A7 the
			// operator had to stop daemons, run `mcphub setup` to
			// recopy the binary, then restart daemons by hand —
			// three steps, easy to skip the middle one, and
			// `mcphub setup` failed loudly with "target is in use"
			// when daemons were still up.
			if upgrade {
				// Bot r1 P2 closure on PR #181: --dry-run with --upgrade
				// would silently violate the dry-run contract ("print
				// planned actions without making changes") because
				// runInstallUpgrade ignores the flag and goes through
				// real Stop/Bootstrap/Restart. Reject the combo rather
				// than implementing a half-baked preview; the upgrade
				// flow is short enough that the operator can run
				// `mcphub stop --all && mcphub status` for a preview.
				if server != "" || daemonFilter != "" || all || strings.TrimSpace(clientsFlag) != "" || allClients || reconcileHubMode || dryRun {
					return fmt.Errorf("--upgrade is mutually exclusive with --server/--daemon/--all/--clients/--all-clients/--reconcile-hub-mode/--dry-run")
				}
				return runInstallUpgrade(cmd)
			}
			// codex bot phase5 r3 P1 closure on PR #160:
			// --reconcile-hub-mode runs the bidirectional install
			// reconciler (BuildHubReconcilePlan +
			// ApplyHubReconcileInOrder) for the current gate state.
			// Operator-explicit entry point per spec §"Bidirectional
			// install reconciler" — flip the gate in Settings,
			// restart the hub, then run this command to migrate
			// every client config to the chosen mode.
			if reconcileHubMode {
				if server != "" || daemonFilter != "" || all {
					return fmt.Errorf("--reconcile-hub-mode is mutually exclusive with --server/--daemon/--all")
				}
				// codex bot phase5 r6 P2 closure on PR #160:
				// --reconcile-hub-mode does NOT honor --clients /
				// --all-clients. The reconcile walks every (manifest,
				// client) tuple from disk; subsetting the reconcile
				// would be a destructive partial migration that leaves
				// the gate in an inconsistent state (e.g. only some
				// clients pointed at the hub aggregate, others still
				// on per-daemon URLs after a gate-ON toggle). Reject
				// the flags loudly rather than silently ignoring them.
				if strings.TrimSpace(clientsFlag) != "" || allClients {
					return fmt.Errorf("--reconcile-hub-mode is mutually exclusive with --clients/--all-clients; reconcile walks every (manifest, client) tuple from disk")
				}
				return runReconcileHubMode(cmd, dryRun)
			}
			// If mcphub is not on PATH, try to bootstrap before we hit
			// the API's preflight check. Three-tier fallback:
			//   1. ~/.local/bin already on PATH — silently copy there
			//      (no registry write, no prompt, non-interactive safe).
			//   2. Interactive terminal — prompt "bootstrap? [Y/n]".
			//   3. Non-interactive without canonical dir on PATH —
			//      return the guidance error (preflight would produce
			//      the same message).
			if _, err := exec.LookPath(mcphubShortName); err != nil {
				switch {
				case targetDirOnPath():
					if err := Bootstrap(cmd.OutOrStdout()); err != nil {
						return err
					}
				default:
					if err := maybeBootstrapInteractively(cmd.OutOrStdout(), os.Stdin); err != nil {
						return err
					}
				}
			}
			if all {
				if server != "" || daemonFilter != "" {
					return fmt.Errorf("--all is mutually exclusive with --server/--daemon")
				}
				a := api.NewAPI()
				include, err := parseInstallClientsFlag(clientsFlag, allClients)
				if err != nil {
					return err
				}
				results := a.InstallAllWithOpts(api.InstallAllOpts{
					ClientsInclude:    include,
					IncludeAllClients: allClients,
					DryRun:            dryRun,
					Writer:            cmd.OutOrStdout(),
				})
				failed := 0
				for _, r := range results {
					if r.Err != nil {
						failed++
						fmt.Fprintf(cmd.OutOrStderr(), "\u2717 %s: %v\n", r.Server, r.Err)
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "\u2713 %s\n", r.Server)
					}
				}
				if failed > 0 {
					return fmt.Errorf("%d of %d install(s) failed", failed, len(results))
				}
				return nil
			}
			if server == "" {
				return fmt.Errorf("--server is required")
			}
			include, err := parseInstallClientsFlag(clientsFlag, allClients)
			if err != nil {
				return err
			}
			a := api.NewAPI()
			return a.Install(api.InstallOpts{
				Server:            server,
				DaemonFilter:      daemonFilter,
				ClientsInclude:    include,
				IncludeAllClients: allClients,
				DryRun:            dryRun,
				Writer:            cmd.OutOrStdout(),
			})
		},
	}
	c.Flags().StringVar(&server, "server", "", "server name (matches servers/<name>/manifest.yaml)")
	c.Flags().StringVar(&daemonFilter, "daemon", "", "install only this daemon (+ its client bindings); omit to install all")
	c.Flags().StringVar(&clientsFlag, "clients", "", "comma-separated subset of clients (default: claude-code,codex-cli,cursor)")
	c.Flags().BoolVar(&allClients, "all-clients", false, "install into every client binding declared by the manifest")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print planned actions without making changes")
	c.Flags().BoolVar(&all, "all", false, "install every manifest under servers/")
	c.Flags().BoolVar(&reconcileHubMode, "reconcile-hub-mode", false,
		"run the bidirectional hub-endpoint reconciler against the current gui_server.hub_endpoint_enabled setting; "+
			"rewrites every client config to/from the mcphub-hub aggregate entry. "+
			"Run AFTER flipping the Settings toggle and restarting the hub.")
	c.Flags().BoolVar(&upgrade, "upgrade", false,
		"upgrade the canonical mcphub binary at ~/.local/bin/mcphub.exe to the currently-running build: "+
			"stop every mcp-local-hub-* daemon, copy this binary over the canonical path, "+
			"then restart every daemon from the new binary. Refuses when run from the canonical "+
			"path (run from your build directory, e.g. './mcphub install --upgrade' after 'go build').")
	return c
}

// runReconcileHubMode reads the current gate state from
// gui-preferences.yaml, builds the full-reconcile plan via
// BuildHubReconcilePlan, and applies it via
// ApplyHubReconcileInOrder. On DryRun, prints the plan without
// touching any client config.
//
// codex bot phase5 r3 P1 closure on PR #160 — the install flow
// must invoke the reconciler explicitly; without this entry point
// the Phase 5 Settings toggle would be a no-op.
func runReconcileHubMode(cmd *cobra.Command, dryRun bool) error {
	a := api.NewAPI()
	// 1. Read current gate state. False = OFF (default) → restore
	//    per-daemon entries + remove the aggregate. codex bot phase5
	//    r7 P2 closure on PR #160: a corrupt settings.yaml must NOT
	//    silently degrade to gate-OFF (which would tear down every
	//    mcphub-hub entry). Read errors are fatal here; only a
	//    genuinely missing file is treated as "first-run gate OFF".
	gateOn, gErr := readHubEndpointGateForReconcile()
	if gErr != nil {
		return fmt.Errorf("reconcile: gate state unreadable; refusing to default to OFF (would tear down mcphub-hub entries): %w", gErr)
	}

	// Issue #161 P2 closure (concurrency lane + endpoint/tokens
	// TOCTOU): acquire hub-mcp.lock for the WHOLE snapshot-to-apply
	// transaction. Two effects:
	//
	//   1. Concurrent reconciles serialize. One reconcile in gate-ON
	//      mode applies AddReplace, the next one waits, then applies
	//      its plan against the now-consistent snapshot. Pre-lock,
	//      AddReplace + Remove phases could interleave producing
	//      half-converged client configs that only re-run fixes.
	//   2. Endpoint / tokens cannot be mutated by a sibling
	//      regenerate-token / regenerate-instance-id during the
	//      reconcile transaction. Pre-lock, the snapshot loaded at
	//      step 2 could be stale by step 5 (apply) — clients would
	//      get 401s after a "successful" reconcile.
	//
	// Bot r1 P2 closure (PR #168): acquire the lock ONLY when
	// needed. AcquireHubMcpLock calls DaemonStateDir which creates
	// the state-dir on first run; pre-acquiring on every reconcile
	// invocation regresses the gate-OFF dry-run "no state side
	// effects" property that PR #160 r11 P3 carefully protected.
	//
	// Lock matrix:
	//   - gate-ON (any path): needs lock — endpoint/tokens snapshot
	//     must be consistent with the apply.
	//   - gate-OFF apply: needs lock — apply writes client configs
	//     that must serialize against concurrent reconciles.
	//   - gate-OFF dry-run: NO lock — reads nothing, applies nothing,
	//     so a state-dir creation here would be a behavior regression.
	if gateOn || !dryRun {
		lk, lockErr := api.AcquireHubMcpLock()
		if lockErr != nil {
			return fmt.Errorf("reconcile: acquire hub-mcp.lock: %w", lockErr)
		}
		defer func() { _ = lk.Unlock() }()
	}

	// 2. Load endpoint + tokens for plan header inputs (URL +
	//    Headers in the gate-ON branch). Read tokens from disk via
	//    ReloadHubTokens so a cold CLI process sees the table.
	//
	// codex bot phase5 r4 P1 closure on PR #160: in gate-ON mode,
	// endpoint and tokens are LOAD-BEARING inputs — Port and
	// InstanceID build the URL + X-Mcphub-Instance-Id header,
	// Tokens[client] builds the X-Mcphub-Hub-Token header. If any
	// of these are missing/corrupt, the planner would silently
	// produce `http://127.0.0.1:0/...` URLs and empty auth headers,
	// writing broken configs across every client. Fail fast and
	// tell the operator to start the hub at least once (which
	// generates endpoint.json + tokens.json) before re-running
	// the gate-ON reconcile.
	//
	// Gate-OFF mode does NOT use endpoint or tokens (URLs come
	// from manifest bindings; no auth headers), so the same
	// errors are tolerable there.
	// codex deep-sec phase5 r11 P3 closure on PR #160 (regression
	// lane): only the gate-ON branch consumes endpoint + tokens
	// (URL/headers in the AddReplace plan). Reading them
	// unconditionally would call DaemonStateDir() which creates the
	// state directory, even for a gate-OFF dry-run on a first-run
	// system. Move the load INSIDE the gate-ON branch so gate-OFF
	// reconcile (and its dry-run preview) leaves the state dir
	// untouched.
	var endpoint api.HubEndpoint
	var tokens api.HubTokenTable
	if gateOn {
		var epErr, tokErr error
		endpoint, epErr = api.LoadHubEndpoint()
		tokens, tokErr = api.ReloadHubTokens()
		if epErr != nil {
			return fmt.Errorf("gate-ON reconcile requires hub-mcp.endpoint.json (start the hub at least once to generate it): %w", epErr)
		}
		if endpoint.Port == 0 {
			return fmt.Errorf("gate-ON reconcile requires the hub to have bound at least once (endpoint.Port=0); start the hub then rerun")
		}
		if endpoint.InstanceID == "" {
			return fmt.Errorf("gate-ON reconcile requires a populated endpoint.InstanceID (corrupt endpoint state?); restart the hub or run `mcphub hub-mcp regenerate-instance-id`")
		}
		if tokErr != nil {
			return fmt.Errorf("gate-ON reconcile requires hub-mcp-tokens.json: %w", tokErr)
		}
		// codex deep-sec phase5 r11 P1 closure on PR #160 (protocol
		// lane): the state-file fail-fast above does not prove the
		// listener is actually up. A stale endpoint.json from a
		// previous hub run passes every check, and the reconcile
		// rewrites every client config to `http://127.0.0.1:<stale>
		// /clients/<id>/mcp` even though nothing is listening. Probe
		// the hub via the non-mutating HEAD /internal/reload-tokens
		// (added in r10) — only the live hub responds 204 to an
		// authenticated probe, so a stranger service or stale port
		// fails closed.
		if !hubProbeAlive(cmd, endpoint.Port) {
			return fmt.Errorf("gate-ON reconcile requires the hub-mcp listener to be live on port %d; start `mcphub gui` (with hub_endpoint_enabled=true in Settings) before running reconcile", endpoint.Port)
		}
	}

	// 3. Collect manifests for currently-installed servers.
	//
	// codex bot phase5 r6 P1 closure on PR #160 (initial fix):
	// switched from Scan().Entries to ManifestList because Scan
	// returns empty after gate-ON migration (clients hold
	// `mcphub-hub` only). ManifestList enumerates the on-disk +
	// embedded manifest registry.
	//
	// codex bot phase5 r8 P1 closure on PR #160 (refinement):
	// ManifestList alone over-includes — it returns EVERY shipped
	// manifest (including templates the user never installed). In
	// gate-OFF mode, that writes per-daemon AddReplace ops for
	// servers the user never ran, polluting client configs with
	// dead localhost URLs. Intersect ManifestList with the set of
	// servers that have at least one scheduled daemon task
	// (ListManagedTasks → ParseManagedTaskName) — those represent
	// "the user ran `mcphub install --server X`" intent. The
	// scheduler tasks survive gate transitions (reconcile only
	// rewrites client configs, never tears down tasks), so this
	// set is stable across gate ON ↔ OFF toggles.
	tasks, tErr := a.ListManagedTasks()
	if tErr != nil {
		return fmt.Errorf("list managed tasks: %w", tErr)
	}
	// codex bot phase5 r16 P1 closure on PR #160: do NOT derive the
	// installed-server set from `ParseManagedTaskName(task.Name)`.
	// ParseManagedTaskName splits on the LAST hyphen, so a daemon
	// name that contains '-' (e.g. `mcp-language-server` with
	// daemon `vscode-css`) parses as server=`mcp-language-server-vscode`
	// + daemon=`css` — the wrong attribution drops the real server
	// from the installed set, and reconcile skips that manifest
	// entirely.
	//
	// Build a normalized scheduler-task set, then walk every
	// manifest and check whether ANY of its expected task names
	// (`mcp-local-hub-<server>-<daemon>` for each daemon) is
	// registered. This is manifest-aware so hyphenated daemon
	// names work end-to-end.
	//
	// Pre-filter the task set to exclude known non-per-server
	// task families up-front. The most important one is
	// `mcp-local-hub-workspace-weekly-refresh`
	// (api.WeeklyRefreshTaskName): a manifest named "workspace"
	// with any daemon would falsely match against the per-server
	// `<server>-weekly-refresh` lookup in
	// manifestHasScheduledDaemon. Excluding it here keeps the
	// helper purely structural (byte-exact map lookup) without
	// requiring it to know about hub-wide task names.
	scheduledTasks := map[string]bool{}
	for _, t := range tasks {
		normalized := strings.TrimPrefix(t.Name, "\\")
		if normalized == strings.TrimPrefix(api.WatchdogTaskName, "\\") {
			continue
		}
		if normalized == api.WeeklyRefreshTaskName {
			continue
		}
		if api.IsLazyProxyTaskName(normalized) {
			continue
		}
		scheduledTasks[normalized] = true
	}
	names, mlErr := a.ManifestList()
	if mlErr != nil {
		return fmt.Errorf("list manifests: %w", mlErr)
	}
	var manifests []config.ServerManifest
	for _, name := range names {
		yaml, gErr := a.ManifestGet(name)
		if gErr != nil {
			if errors.Is(gErr, os.ErrNotExist) {
				continue // benign list-window race
			}
			return fmt.Errorf("read manifest %q: %w", name, gErr)
		}
		m, pErr := config.ParseManifest(strings.NewReader(yaml))
		if pErr != nil {
			return fmt.Errorf("parse manifest %q: %w", name, pErr)
		}
		if !manifestHasScheduledDaemon(m, scheduledTasks) {
			continue // not installed on this machine
		}
		manifests = append(manifests, *m)
	}

	plan, pErr := api.BuildHubReconcilePlan(manifests, endpoint, tokens, api.HubReconcileOpts{GateOn: gateOn})
	if pErr != nil {
		return fmt.Errorf("build reconcile plan: %w", pErr)
	}

	mode := "OFF (restore per-daemon URLs)"
	if gateOn {
		mode = "ON (route every client via http://127.0.0.1:<hub-port>/clients/<id>/mcp)"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Reconcile mode: gate %s\nPlan: %d op(s)\n", mode, len(plan))
	for _, op := range plan {
		fmt.Fprintf(cmd.OutOrStdout(), "  %s  client=%s  entry=%s  url=%s\n",
			op.Action, op.Client, op.EntryName, op.URL)
	}
	if dryRun {
		fmt.Fprintln(cmd.OutOrStdout(), "(--dry-run: no client config touched)")
		return nil
	}

	report := api.ApplyHubReconcileInOrder(plan)
	for _, ok := range report.Succeeded {
		fmt.Fprintf(cmd.OutOrStdout(), "✓ %s\n", ok)
	}
	// codex bot phase5 r4 P2 closure on PR #160: surface Skipped
	// clients to the operator. The reconciler skips clients whose
	// adapter requires fields the hub planner does not provide
	// (currently antigravity needs Relay{Server,Daemon,ExePath}).
	// Without this loop the operator gets a silent "success" verdict
	// while those clients remain on stale config. Stderr + a "manual
	// reinstall" hint keeps the operator-facing signal honest.
	for _, sk := range report.Skipped {
		fmt.Fprintf(cmd.OutOrStderr(),
			"⚠ %s: skipped (adapter not supported by hub reconciler — run `mcphub install --server <name> --clients %s` manually)\n",
			sk, sk)
	}
	for _, f := range report.Failed {
		fmt.Fprintf(cmd.OutOrStderr(), "✗ %s (%s): %s\n", f.Client, f.Phase, f.Err)
	}
	if len(report.Failed) > 0 {
		return fmt.Errorf("%d client reconcile failure(s) — see stderr; rerun to converge",
			len(report.Failed))
	}
	return nil
}

// manifestHasScheduledDaemon returns true iff at least one daemon
// declared in the manifest has a corresponding scheduler task in the
// supplied set. The expected task-name shape for a daemon is
// `mcp-local-hub-<server>-<daemon>` and (for per-server weekly
// refresh) `mcp-local-hub-<server>-weekly-refresh`. codex bot phase5
// r16 P1 closure on PR #160: use manifest-aware membership instead
// of ParseManagedTaskName, which splits on the last hyphen and
// mis-attributes daemons whose names contain '-' (e.g. `vscode-css`).
//
// `scheduledTasks` keys are the normalized form (leading `\` already
// trimmed by the caller in runReconcileHubMode). The check is byte-
// exact, no parsing, so hyphenated server AND hyphenated daemon
// names compose cleanly.
func manifestHasScheduledDaemon(m *config.ServerManifest, scheduledTasks map[string]bool) bool {
	const prefix = "mcp-local-hub-"
	for _, d := range m.Daemons {
		if scheduledTasks[prefix+m.Name+"-"+d.Name] {
			return true
		}
		// Per-server weekly-refresh task is also a valid signal
		// of installation. The CLI install path creates one
		// `mcp-local-hub-<server>-weekly-refresh` per server.
		if scheduledTasks[prefix+m.Name+"-weekly-refresh"] {
			return true
		}
	}
	return false
}

// readHubEndpointGateForReconcile returns the persisted gate state
// for the reconcile path. Distinct semantics from the gui-package
// helper readHubEndpointGateFromSettings (internal/gui/hub_listener.go),
// which silently defaults to false on every read/parse error so the
// hub-listener stays OFF when settings are unhealthy:
//
// codex bot phase5 r7 P2 closure on PR #160: --reconcile-hub-mode
// performs broad destructive rewrites (gate-OFF removes the
// `mcphub-hub` aggregate from every client). Silently defaulting
// to OFF when the settings file is corrupt could trigger that
// tear-down without operator intent. Distinguish:
//
//   - File missing: treat as gate OFF (default for a first-run
//     system that hasn't written settings.yaml yet — the reconcile
//     is a no-op because there's nothing to tear down).
//   - File present but unreadable / unparseable: fail with error.
//     Operator must repair or delete settings.yaml.
//   - File present, parseable, but EMPTY (zero-byte or whitespace-
//     only): fail with error. codex deep-sec phase5 r11 P2 closure
//     on PR #160 (concurrency lane): a zero-byte read could indicate
//     a torn write from a concurrent SettingsSet, and yaml.Unmarshal
//     returns nil for empty input — without this check we'd silently
//     default to gate-OFF and apply a destructive teardown.
//   - File present, parseable, key absent: treat as gate OFF (default
//     when the operator has never toggled the gate). codex bot phase5
//     r15 P2 closure on PR #160 raised the concern that a partial
//     write could parse as "key absent" — that race is now mitigated
//     by SettingsSetIn's tempfile+rename atomic-write path (added in
//     the same r15 commit), so cross-process readers see EITHER the
//     pre-write file (with the prior value) OR the post-write file
//     (with the new value), never a partial-truncate in between.
//   - File present and parseable with content: use the persisted value.
func readHubEndpointGateForReconcile() (bool, error) {
	path := api.SettingsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read settings %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return false, fmt.Errorf("settings %s is empty (concurrent write in progress, or file corrupted); refusing to default to OFF — wait for the writer to finish, or repair the file, then retry", path)
	}
	raw := map[string]string{}
	if uerr := yaml.Unmarshal(data, &raw); uerr != nil {
		return false, fmt.Errorf("parse settings %s (delete or repair before retrying reconcile): %w", path, uerr)
	}
	return raw["gui_server.hub_endpoint_enabled"] == "true", nil
}

func parseInstallClientsFlag(clientsFlag string, allClients bool) ([]string, error) {
	if allClients && strings.TrimSpace(clientsFlag) != "" {
		return nil, fmt.Errorf("--clients is mutually exclusive with --all-clients")
	}
	if strings.TrimSpace(clientsFlag) == "" {
		return nil, nil
	}
	parts := strings.Split(clientsFlag, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out, nil
}

// maybeBootstrapInteractively asks the user whether to bootstrap mcphub to
// ~/.local/bin when it is not yet on PATH. Returns nil if the user says yes
// (and bootstrap succeeds) or no-ops with a guidance error when the user
// declines. In non-terminal contexts it returns nil immediately; the API
// preflight check will then surface the "not on PATH" error with the same
// guidance, so automation never has its PATH mutated out from under it.
func maybeBootstrapInteractively(w io.Writer, in *os.File) error {
	if !term.IsTerminal(int(in.Fd())) {
		return nil
	}
	fmt.Fprintf(w, "%s not found on PATH. Bootstrap to ~/.local/bin? [Y/n] ", mcphubShortName)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		return fmt.Errorf("read prompt response: %w", err)
	}
	answer := strings.TrimSpace(line)
	if answer == "" || strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes") {
		return Bootstrap(w)
	}
	return fmt.Errorf("%s not found on PATH — run `mcphub setup` once to install to ~/.local/bin and register in PATH", mcphubShortName)
}

// ---------------------------------------------------------------------------
// `mcphub install --upgrade` — bug-bash A7 minimal closure (#4).
// ---------------------------------------------------------------------------

// upgradeStopAllFn, upgradeBootstrapFn, and upgradeRestartAllFn are
// test seams that let install_upgrade_test.go drive the orchestration
// without spawning real daemons or touching the filesystem. Production
// nil → fall through to the real implementations (a.StopAll, Bootstrap,
// a.RestartAll). Tests assign fakes inside the test setup.
var (
	upgradeStopAllFn    func() ([]api.RestartResult, error)
	upgradeBootstrapFn  func(io.Writer) error
	upgradeRestartAllFn func() ([]api.RestartResult, error)
	// upgradeExecutableFn / upgradeTargetPathFn carry the canonical-
	// path comparison for the self-replace guard. Tests inject any
	// path pair to drive the refusal branch without filesystem state.
	upgradeExecutableFn func() (string, error)
	upgradeTargetPathFn func() (string, error)
)

// runInstallUpgrade is the entry point behind `mcphub install --upgrade`.
//
// Flow:
//
//  1. Self-replace guard. Refuse if os.Executable() == canonical target
//     path (~/.local/bin/mcphub.exe). Running --upgrade FROM the
//     canonical binary would be a no-op at best (samePath in Bootstrap
//     skips the copy) and a confusing rename-failure at worst on Windows
//     (the running image cannot replace itself). The dual-binary
//     trampoline that lifts this restriction is bug #1, deferred.
//
//  2. StopAll. Kill every running mcp-local-hub-* daemon by port and
//     /End its scheduler task. This is what releases the Windows file
//     lock on ~/.local/bin/mcphub.exe so step 3 can replace it.
//     Scheduler task XML stays put — `sch.Stop` does not delete; the
//     task is just paused. Per-task stop failures are logged but do
//     NOT abort the upgrade; rare cases (Stuck Force-killed daemon
//     etc.) still need the binary copy to succeed.
//
//  3. Copy-only bootstrap. Copies the currently-running binary
//     (os.Executable()) to ~/.local/bin/mcphub.exe via tempfile +
//     atomic rename. Reuses the existing `mcphub setup` copy helper
//     but SKIPS PATH registration (bot r2 P1 closure on PR #181):
//     `Bootstrap` does both copy AND `ensureOnPath`, and a HKCU PATH
//     write hiccup during upgrade would propagate up, skip RestartAll,
//     and leave the daemon fleet down. PATH is a one-time setup
//     concern handled by `mcphub setup`, not upgrade.
//
//  4. RestartAll. /Run every paused task. The new tasks read XML that
//     references ~/.local/bin/mcphub.exe by absolute path, so they
//     pick up the NEW binary automatically.
//
// Watchdog interleaving. The watchdog scheduled task runs every 5 min.
// The upgrade window (Stop → Bootstrap → Restart) is sub-second in
// the steady state, so a watchdog tick landing inside it is rare. If
// one DOES land, the watchdog spawns `mcphub watchdog --once` from
// the OLD binary, restarts a daemon, re-locks the canonical path,
// and Bootstrap fails with `target in use`. The operator's recovery
// is identical to step 2 failure: stop the daemon manually, rerun
// --upgrade. A "disable watchdog during upgrade" dance would add
// two new failure modes (re-enable might fail) for a tiny edge case;
// not worth it for the minimal fix.
//
// Partial restart failure. If Bootstrap succeeds but RestartAll
// reports per-task failures, the operator is in a state where the
// binary is fresh but some daemons are down. RunE returns the
// aggregate error so the caller sees the failure; recovery is
// `mcphub restart --all` (idempotent). No rollback to the old
// binary — keeping a backup and reverting would add complexity
// and a new persistence surface; the operator explicitly opted into
// --upgrade, so they accept manual convergence in this rare case.
func runInstallUpgrade(cmd *cobra.Command) error {
	a := api.NewAPI()
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	// 1. Self-replace guard. Compare by filesystem identity (inode /
	// Windows FileID via os.SameFile) rather than cleaned path string:
	// aliases (symlinks, NTFS junctions, 8.3 short-names) can make
	// curExe and target differ as strings while pointing at the same
	// underlying file. Bot r3 P2 closure on PR #181: pre-fix, the
	// guard would let an aliased self-replace through, StopAll would
	// stop every daemon, and copyExe would then fail with "target in
	// use" (the running image still holds the file lock) — fleet down
	// for an avoidable reason.
	curExe, target, err := resolveUpgradeSelfPaths()
	if err != nil {
		return fmt.Errorf("resolve self-replace guard paths: %w", err)
	}
	if upgradeIsSelfReplace(curExe, target) {
		return fmt.Errorf(
			"refusing to --upgrade from the canonical binary at %s "+
				"(current executable %s resolves to the same file via path or symlink/junction/short-name alias): "+
				"the running image cannot replace itself on Windows. "+
				"Build a new binary (e.g. `go build ./cmd/mcphub`) and run "+
				"`./mcphub install --upgrade` (or `.\\mcphub.exe install --upgrade`) "+
				"from the build directory instead.",
			target, curExe)
	}

	// 1b. Dev-build guard (PR #188 / A8 closure): refuse to copy a
	// source binary that was built without the build scripts'
	// ldflags (`-X main.version=...` + `-H windowsgui` on Windows).
	// Such a binary shows `version=dev / commit=unknown` from
	// `mcphub version` and on Windows is a CONSOLE-subsystem
	// executable that spawns visible terminals for every Scheduler-
	// invoked daemon. The 2026-05-15 user session caught exactly
	// this: a plain `go build ./cmd/mcphub` had replaced the
	// canonical .local/bin/mcphub.exe, terminals flashed on every
	// daemon spawn, and tray failed to render. The repair was
	// `bash build.sh && ./bin/mcphub.exe install --upgrade` —
	// which this guard now enforces preemptively. Operators who
	// need to test an in-progress build path use `mcphub gui`
	// directly from `./bin/mcphub.exe` (running it in place); the
	// `install --upgrade` flow is for promoting a BUILT binary to
	// the canonical install location, so it should be picky.
	version := upgradeBuildVersion()
	if version == "dev" || version == "" {
		return fmt.Errorf(
			"refusing to --upgrade from a dev-build binary at %s: "+
				"current executable was built without the build scripts' ldflags "+
				"(version=%q, expected a semver like \"0.4.0\"). "+
				"On Windows this binary is also CONSOLE-subsystem (no `-H windowsgui` "+
				"linker flag), which would cause terminal flashes on every "+
				"Scheduler-invoked daemon and prevent the tray icon from rendering. "+
				"Recovery: rebuild via `bash build.sh` (or `pwsh build.ps1`), then "+
				"run `./bin/mcphub.exe install --upgrade` from the build directory.",
			curExe, version)
	}

	// 1c. Running-GUI guard (PR #188 / A8 closure): detect a
	// running `mcphub.exe gui` process whose image path equals
	// `target` and refuse with a clear stop-the-GUI hint. The
	// previous flow stopped daemons via StopAll but did NOT
	// touch the GUI — if a GUI was running on the canonical
	// install path, Bootstrap (step 3) would then fail with
	// "target in use" because the GUI holds an open handle on
	// the file. The 2026-05-15 smoke session walked into this
	// exactly: upgrade had to be re-run after manually killing
	// the GUI. Surface it as a refusal BEFORE StopAll runs so
	// the operator's daemon fleet stays up — pre-fix, daemons
	// were stopped and then Bootstrap failed, leaving the
	// fleet down until a manual recovery.
	//
	// Identity match: cmdline starts with the target path (so
	// daemons spawned from the same binary but from a build
	// dir don't false-trigger) AND argv[1] equals "gui" (or
	// argv has length 1 — the Explorer-double-click entry path
	// per cmd/mcphub/main.go shouldAutoLaunchGUI).
	guiProcs, guiErr := findRunningGUIsOnTarget(a, target)
	if guiErr != nil {
		// Best-effort: a wmic failure must not block the
		// upgrade — fall through and let Bootstrap surface the
		// "target in use" error if a GUI was actually running.
		fmt.Fprintf(errOut, "⚠ GUI detection failed (best-effort): %v\n", guiErr)
	} else if len(guiProcs) > 0 {
		pids := make([]string, 0, len(guiProcs))
		for _, p := range guiProcs {
			pids = append(pids, fmt.Sprintf("%d", p.PID))
		}
		return fmt.Errorf(
			"refusing to --upgrade with a running mcphub GUI on the target path %s "+
				"(PIDs: %s). The GUI process holds the binary file lock; "+
				"Bootstrap would fail with `target in use` and leave the "+
				"daemon fleet down (StopAll runs before Bootstrap). "+
				"Recovery: stop the GUI (tray menu → Quit, or "+
				"`Stop-Process -Id <PID> -Force` in PowerShell), then "+
				"rerun `./mcphub install --upgrade`",
			target, strings.Join(pids, ", "))
	}

	// 2. Stop all daemons (release the binary lock).
	fmt.Fprintln(out, "Stopping running daemons...")
	stopResults, err := upgradeStopAll(a)
	if err != nil {
		return fmt.Errorf("stop all: %w", err)
	}
	for _, r := range stopResults {
		if r.Err != "" {
			fmt.Fprintf(errOut, "⚠ stop %s: %s\n", r.TaskName, r.Err)
		} else {
			fmt.Fprintf(out, "✓ stopped %s\n", r.TaskName)
		}
	}

	// 3. Copy the new binary into the canonical path.
	//
	// Bot r3 P2 closure on PR #181: on copyExe failure (e.g., a
	// stuck daemon still holding the file lock after StopAll
	// reported success), the underlying error message hints at
	// `mcphub setup` for recovery. That's wrong in --upgrade
	// context: daemons are already stopped here, and `mcphub setup`
	// does NOT restart them. Wrap with upgrade-specific recovery:
	// re-run --upgrade (idempotent on the copy step, no harm if the
	// binary is already current) OR run `mcphub restart --all` to
	// converge if the binary is OK but daemons are still down.
	fmt.Fprintln(out, "Copying new binary...")
	if err := upgradeBootstrap(out); err != nil {
		return fmt.Errorf(
			"bootstrap (binary copy) failed after daemons were stopped: %w; "+
				"recovery: re-run `mcphub install --upgrade` (idempotent), "+
				"or `mcphub restart --all` to restart daemons without copying",
			err)
	}

	// 4. Restart every paused task from the new binary.
	fmt.Fprintln(out, "Restarting daemons...")
	restartResults, err := upgradeRestartAll(a)
	if err != nil {
		return fmt.Errorf("restart all: %w", err)
	}
	failed := 0
	for _, r := range restartResults {
		if r.Err != "" {
			failed++
			fmt.Fprintf(errOut, "✗ restart %s: %s\n", r.TaskName, r.Err)
		} else {
			fmt.Fprintf(out, "✓ restarted %s\n", r.TaskName)
		}
	}
	if failed > 0 {
		return fmt.Errorf(
			"%d daemon(s) failed to restart after upgrade; binary is updated, "+
				"run `mcphub restart --all` to converge",
			failed)
	}
	return nil
}

// findRunningGUIsOnTargetFn is the production seam for
// findRunningGUIsOnTarget; tests stub it to return a fixed slice
// instead of probing the live OS. nil = use real wmic-backed
// enumeration via api.ListMatchingProcesses.
var findRunningGUIsOnTargetFn func(target string) ([]api.ProcessInfo, error)

// upgradeBuildVersionFn is the production seam for the dev-build
// guard. Tests stub it to return a valid semver so the guard does
// NOT fire from `go test` (which inherits version="dev" from the
// non-ldflag'd test build). nil = consult buildinfo.Get().
var upgradeBuildVersionFn func() string

// upgradeBuildVersion routes through the test seam if set, else
// returns the buildinfo-store version. Centralized here so the guard
// in runInstallUpgrade has a single call site to mock.
func upgradeBuildVersion() string {
	if upgradeBuildVersionFn != nil {
		return upgradeBuildVersionFn()
	}
	v, _, _ := buildinfo.Get()
	return v
}

// findRunningGUIsOnTarget enumerates running mcphub.exe processes
// whose image path equals `target` AND whose first non-image argv
// element is "gui" (or no args at all — Explorer-double-click
// entry path per cmd/mcphub/main.go shouldAutoLaunchGUI).
//
// Returns ([], nil) on POSIX (the GUI lives only on Windows
// builds; the install --upgrade flow on POSIX is mostly a
// developer path).
//
// Tests can stub findRunningGUIsOnTargetFn to bypass wmic.
func findRunningGUIsOnTarget(a *api.API, target string) ([]api.ProcessInfo, error) {
	if findRunningGUIsOnTargetFn != nil {
		return findRunningGUIsOnTargetFn(target)
	}
	procs, err := a.ListMatchingProcesses([]string{"mcphub.exe"})
	if err != nil {
		return nil, err
	}
	var matched []api.ProcessInfo
	for _, p := range procs {
		if !cmdlineIsGUIOnTarget(p.Cmdline, target) {
			continue
		}
		matched = append(matched, p)
	}
	return matched, nil
}

// cmdlineIsGUIOnTarget reports whether a wmic/PowerShell
// CommandLine string represents a `mcphub.exe gui` invocation
// whose image is the same file as target.
//
// Match strategy (codex bot r2-r4 closures on PR #188):
//
//	1. Try case-insensitive prefix match: cmdline starts with
//	   `target` (literal path). Handles the common case and
//	   target paths containing spaces (r2/r3 closures).
//	2. If prefix match fails, extract the image path from
//	   cmdline (everything up to the first whitespace not
//	   inside quotes) and compare via os.SameFile (r4 closure).
//	   Catches 8.3 short paths, junctions/symlinks, and other
//	   canonicalization aliases that the prefix match misses
//	   because the cmdline holds the alias, not the canonical
//	   target string.
//
// After the image-path match (either path), the next non-space
// token must be "gui" (or end-of-string for Explorer-double-
// click launch per cmd/mcphub/main.go shouldAutoLaunchGUI).
//
// Rejects daemon invocations (`mcphub.exe daemon --server ...`),
// the tray child (`mcphub.exe tray`), watchdog ticks, and
// same-binary-different-path matches.
func cmdlineIsGUIOnTarget(cmdline, target string) bool {
	cmdline = strings.TrimSpace(cmdline)
	if cmdline == "" || target == "" {
		return false
	}
	// splitCSVLine strips quotes, but defensively handle a
	// still-quoted form from non-wmic input paths.
	cmdline = strings.TrimPrefix(cmdline, `"`)

	// Path 1: case-insensitive prefix match.
	if rest, ok := matchTargetPrefix(cmdline, target); ok {
		return firstArgIsGUI(rest)
	}

	// Path 2: file-identity match via os.SameFile. Handles
	// 8.3 short paths, junctions, symlinks, and any other path
	// alias whose string differs from `target` but resolves
	// to the same file. Requires both paths to be readable —
	// best-effort: a stat failure means "cannot prove file
	// identity, fall through to false" (the prefix match
	// already covered the literal path case).
	imagePath, rest := splitImageAndRest(cmdline)
	if imagePath == "" {
		return false
	}
	if !sameFileOrFalse(imagePath, target) {
		return false
	}
	return firstArgIsGUI(rest)
}

// matchTargetPrefix returns (rest, true) when cmdline starts
// with target (case-insensitive), and (rest, false) otherwise.
// rest is the cmdline content after the prefix, leading
// close-quote and whitespace stripped.
func matchTargetPrefix(cmdline, target string) (string, bool) {
	if !strings.HasPrefix(strings.ToLower(cmdline), strings.ToLower(target)) {
		return "", false
	}
	after := cmdline[len(target):]
	after = strings.TrimPrefix(after, `"`)
	after = strings.TrimLeft(after, " \t")
	return after, true
}

// splitImageAndRest extracts the image path and the rest from a
// cmdline. The image path is everything up to the first
// whitespace outside quotes; the rest is everything after,
// leading whitespace stripped. Handles a leading `"` (closing
// quote stripped if found before whitespace) but does NOT
// handle embedded-space paths whose quotes were stripped by
// splitCSVLine — that case is covered by matchTargetPrefix.
func splitImageAndRest(cmdline string) (image, rest string) {
	cmdline = strings.TrimPrefix(cmdline, `"`)
	// If a closing quote appears before the first whitespace,
	// use it as the image-boundary marker (preserves embedded
	// spaces in a quoted image path).
	if quoteIdx := strings.Index(cmdline, `"`); quoteIdx >= 0 {
		if spaceIdx := strings.IndexAny(cmdline, " \t"); spaceIdx < 0 || quoteIdx < spaceIdx {
			image = cmdline[:quoteIdx]
			rest = strings.TrimLeft(cmdline[quoteIdx+1:], " \t")
			return image, rest
		}
	}
	if spaceIdx := strings.IndexAny(cmdline, " \t"); spaceIdx >= 0 {
		image = cmdline[:spaceIdx]
		rest = strings.TrimLeft(cmdline[spaceIdx:], " \t")
		return image, rest
	}
	return cmdline, ""
}

// sameFileOrFalse stats both paths and returns true iff they
// resolve to the same file via os.SameFile. Stat failures (file
// missing, permission denied, etc.) yield false — best-effort
// behavior; the caller has already exhausted the prefix-match
// path so a failure here means "cannot prove same identity".
//
// sameFileOrFalseFn is the test seam; tests stub it to return
// a fixed bool without touching the filesystem.
var sameFileOrFalseFn func(path1, path2 string) bool

func sameFileOrFalse(path1, path2 string) bool {
	if sameFileOrFalseFn != nil {
		return sameFileOrFalseFn(path1, path2)
	}
	fi1, err := os.Stat(path1)
	if err != nil {
		return false
	}
	fi2, err := os.Stat(path2)
	if err != nil {
		return false
	}
	return os.SameFile(fi1, fi2)
}

// firstArgIsGUI reports whether the first whitespace-delimited
// token of rest is exactly "gui" (case-sensitive — Cobra
// subcommand routing is case-sensitive). Empty rest counts as
// "gui" (Explorer-double-click landing per cmd/mcphub/main.go
// shouldAutoLaunchGUI).
func firstArgIsGUI(rest string) bool {
	if rest == "" {
		return true
	}
	firstArg := rest
	if spaceIdx := strings.IndexAny(rest, " \t"); spaceIdx >= 0 {
		firstArg = rest[:spaceIdx]
	}
	return firstArg == "gui"
}

// resolveUpgradeSelfPaths returns (current executable, canonical
// target) for the self-replace guard, routing through the test seams
// when set. Errors propagate so the caller can surface a wrapped
// diagnostic; production paths only return error when os.Executable
// or os.UserHomeDir fail (extremely rare).
func resolveUpgradeSelfPaths() (curExe, target string, err error) {
	if upgradeExecutableFn != nil {
		curExe, err = upgradeExecutableFn()
	} else {
		curExe, err = os.Executable()
	}
	if err != nil {
		return "", "", fmt.Errorf("resolve current executable: %w", err)
	}
	if upgradeTargetPathFn != nil {
		target, err = upgradeTargetPathFn()
	} else {
		target, err = setupTargetPath()
	}
	if err != nil {
		return "", "", fmt.Errorf("resolve canonical target: %w", err)
	}
	return curExe, target, nil
}

// upgradeStopAll routes through the upgradeStopAllFn seam if set,
// otherwise a.StopAll. Kept as a thin wrapper so tests can replace
// either side independently.
func upgradeStopAll(a *api.API) ([]api.RestartResult, error) {
	if upgradeStopAllFn != nil {
		return upgradeStopAllFn()
	}
	return a.StopAll()
}

// upgradeBootstrap routes through upgradeBootstrapFn if set, otherwise
// the copy-only helper (bot r2 P1 closure on PR #181: skip ensureOnPath
// so a HKCU PATH write hiccup doesn't take down the daemon fleet during
// upgrade). PATH registration stays in `mcphub setup`'s purview.
func upgradeBootstrap(w io.Writer) error {
	if upgradeBootstrapFn != nil {
		return upgradeBootstrapFn(w)
	}
	return bootstrapCopyOnly(w)
}

// upgradeIsSelfReplace reports whether curExe and target reference the
// same underlying file by either:
//
//   - filesystem identity (os.SameFile on inode/FileID, which catches
//     symlinks, NTFS junctions, and 8.3 short-name aliases — bot r3 P2
//     closure on PR #181), OR
//   - cleaned absolute path string match (samePath; the legacy
//     comparison, kept as a fallback for the first-install case where
//     the target file does not exist yet and SameFile can't compare).
//
// The two-layer check is fail-closed: if SameFile is unavailable (e.g.,
// either path Stat fails because the target hasn't been created), the
// string-based check still catches the obvious "user typed the
// canonical path directly" case. The string-only legacy was the path
// the bot's r3 P2 finding flagged as bypassable.
func upgradeIsSelfReplace(curExe, target string) bool {
	if samePath(curExe, target) {
		return true
	}
	curInfo, err := os.Stat(curExe)
	if err != nil {
		return false
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		// Target doesn't exist yet (first install) — no self-replace
		// risk; string comparison already returned false above.
		return false
	}
	return os.SameFile(curInfo, targetInfo)
}

// upgradeRestartAll routes through the upgradeRestartAllFn seam if
// set, otherwise a.RestartAll.
func upgradeRestartAll(a *api.API) ([]api.RestartResult, error) {
	if upgradeRestartAllFn != nil {
		return upgradeRestartAllFn()
	}
	return a.RestartAll()
}
