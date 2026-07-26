package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/buildinfo"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/gui"
	"mcp-local-hub/internal/scheduler"

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
	var check bool
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
     default clients are Claude Code and Codex CLI; Cursor, Gemini CLI,
     Qwen CLI, VS Code, Antigravity, Zed, Kiro, Windsurf, Cline, Kilo Code,
     OpenCode, Hermes, and OpenClaw are opt-in via --clients or --all-clients

Examples:
  mcphub install --server serena               # default clients: claude-code,codex-cli
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
  - Windows: Task Scheduler backend only. Linux/macOS manifest installs ship
    compile-only scheduler stubs.
  - Workspace LSP registration uses 'mcphub register <workspace> [language...]';
    on schedulerless Linux/macOS builds, that path automatically uses the
    supervised proxy instead of legacy scheduled tasks.

See also: status, restart, uninstall, rollback, scheduler upgrade.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// A3 PR-2: in an INTERACTIVE terminal, a client-config write that
			// hits a SYMLINKED destination prompts the operator [y/N] (default
			// N) before following the symlink to its resolved target. The port
			// is consulted at the single client-config write choke point inside
			// package api and covers BOTH the install write branches below and
			// the --reconcile-hub-mode reconciler. Non-interactive runs install
			// nothing → the existing refusal stands (automation never
			// redirected). The restore defer clears the process-level port.
			defer installInteractiveSymlinkConsent(cmd.OutOrStdout(), os.Stdin)()

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
				// v0.6 Phase F: the v0.4.x→v0.5.0 forward-migration engine and
				// the `--rollback-to-legacy` demotion path are deleted (there
				// is no v0.4.x scheduler model to migrate from or roll back to
				// anymore). --upgrade now routes between just two sinks based
				// on machine state: the v0.5.x→v0.5.x cold-restart upgrade
				// (supervisor-intent.json present — rename-aside + IPC handoff)
				// and the fresh-install binary-copy fallback (no state on
				// disk). Decision tree in dispatchUpgrade.
				return dispatchUpgrade(cmd)
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
			// --check: read-only readiness probe. Print the report (blockers +
			// optional advisories) for --server and exit 0 WITHOUT installing,
			// bootstrapping, or mutating anything. Handled BEFORE the bootstrap
			// block so a pure diagnostic never copies the binary or prompts.
			if check {
				if server == "" {
					return fmt.Errorf("--check requires --server")
				}
				if all || daemonFilter != "" {
					return fmt.Errorf("--check is mutually exclusive with --all/--daemon")
				}
				// --check must report on the SAME client scope the install it
				// previews would use, so an explicitly targeted opt-in client's
				// broken binding is surfaced here instead of only at install time.
				include, err := parseInstallClientsFlag(clientsFlag, allClients)
				if err != nil {
					return err
				}
				rep, rerr := api.CheckServerReadinessByNameWithScope(server, api.AdmissionScope{
					ClientsInclude:    include,
					IncludeAllClients: allClients,
				})
				if rerr != nil {
					return rerr
				}
				renderReadinessReport(cmd.OutOrStdout(), rep)
				return nil
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
					GUIPort:           resolveInstallGUIPort(),
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
			// Readiness report — surface the already-built actionable findings
			// BEFORE the install mutates anything (the --check-only branch ran
			// earlier, pre-bootstrap). This runs AFTER bootstrap so the
			// `mcphub binary` requirement reflects the just-bootstrapped state.
			// Print blockers (with guided fixes) and STOP if any; print
			// unset-optional-secret advisories and PROCEED to install.
			// Parse the client selection BEFORE readiness: readiness must
			// validate the bindings this very install will apply, not the
			// default-install set. An explicitly targeted opt-in client (e.g.
			// `--clients cursor`) whose binding the planner rejects has to show
			// up as a readiness blocker, not as a surprise install failure.
			include, err := parseInstallClientsFlag(clientsFlag, allClients)
			if err != nil {
				return err
			}
			rep, rerr := api.CheckServerReadinessByNameWithScope(server, api.AdmissionScope{
				DaemonFilter:      daemonFilter,
				ClientsInclude:    include,
				IncludeAllClients: allClients,
			})
			if rerr != nil {
				return rerr
			}
			if blocked := renderReadinessReport(cmd.OutOrStdout(), rep); blocked {
				return fmt.Errorf("%s is not ready to install — fix the blocker(s) above (or run `mcphub install --server %s --check` to re-print them)", server, server)
			}
			a := api.NewAPI()
			return a.Install(api.InstallOpts{
				Server:            server,
				DaemonFilter:      daemonFilter,
				ClientsInclude:    include,
				IncludeAllClients: allClients,
				DryRun:            dryRun,
				Writer:            cmd.OutOrStdout(),
				GUIPort:           resolveInstallGUIPort(),
			})
		},
	}
	c.Flags().StringVar(&server, "server", "", "server name (matches servers/<name>/manifest.yaml)")
	c.Flags().StringVar(&daemonFilter, "daemon", "", "install only this daemon (+ its client bindings); omit to install all")
	c.Flags().StringVar(&clientsFlag, "clients", "", "comma-separated subset of clients (default: claude-code,codex-cli)")
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
	c.Flags().BoolVar(&check, "check", false,
		"print the readiness report for --server (missing dependencies/launchers as blockers, "+
			"unset optional secrets as advisories) and exit WITHOUT installing or mutating anything")
	return c
}

// renderReadinessReport prints a server's readiness report to w and reports
// whether any BLOCKING requirement (non-optional + not OK) is present.
//
// Surfacing contract (install-and-it-works Area 2, Phase 1):
//   - BLOCKERS (non-optional unmet) print with their guided Fix; the caller
//     hard-stops the install when blocked is true.
//   - OPTIONAL unmet (e.g. an unset `secret:` ref) print as a NON-BLOCKING
//     advisory and the install PROCEEDS. The Optional flag is the single owner
//     of "does this block" — this render never flips an optional requirement
//     into a blocker (the SECRETS-OPTIONAL invariant).
//
// Security: Reason/Fix are rendered VERBATIM. They are already path-redacted at
// the source (CheckServerReadiness builds them via redactErrorDetail / basename
// normalization), and a `secret:` requirement names only the KEY, never a value
// — so the render must not re-derive or reformat them in a way that bypasses
// that redaction.
func renderReadinessReport(w io.Writer, rep *api.ReadinessReport) (blocked bool) {
	if rep == nil {
		return false
	}
	var blockers, advisories []api.ReadinessRequirement
	for _, r := range rep.Requirements {
		if r.OK {
			continue
		}
		if r.Optional {
			advisories = append(advisories, r)
		} else {
			blockers = append(blockers, r)
		}
	}

	if len(blockers) > 0 {
		fmt.Fprintf(w, "%s is not ready to install — %d blocker(s):\n", rep.Server, len(blockers))
		for _, r := range blockers {
			fmt.Fprintf(w, "  ✗ %s", r.Name)
			if r.Reason != "" {
				fmt.Fprintf(w, ": %s", r.Reason)
			}
			fmt.Fprintln(w)
			if r.Fix != "" {
				fmt.Fprintf(w, "      Fix: %s\n", r.Fix)
			}
		}
	}

	if len(advisories) > 0 {
		fmt.Fprintf(w, "%s — %d optional item(s) not set (install will proceed without them):\n", rep.Server, len(advisories))
		for _, r := range advisories {
			fmt.Fprintf(w, "  ℹ %s", r.Name)
			if r.Reason != "" {
				fmt.Fprintf(w, ": %s", r.Reason)
			}
			fmt.Fprintln(w)
			if r.Fix != "" {
				fmt.Fprintf(w, "      %s\n", r.Fix)
			}
		}
	}

	if len(blockers) == 0 && len(advisories) == 0 {
		fmt.Fprintf(w, "%s is ready to install.\n", rep.Server)
	}

	return len(blockers) > 0
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
	// dead localhost URLs. Intersect ManifestList with install
	// signals from either legacy scheduler rows or v0.6
	// supervisor-intent daemon rows. Fresh supervisor-owned installs
	// deliberately have no per-daemon scheduler tasks, so
	// supervisor-intent `Daemons[].server` is now an equivalent
	// "the user ran `mcphub install --server X`" signal.
	supervisorInstalledServers, siErr := supervisorIntentManagedServerSignals()
	if siErr != nil {
		return fmt.Errorf("read supervisor-intent installed servers: %w", siErr)
	}
	tasks, tErr := a.ListManagedTasks()
	if tErr != nil && !api.SchedulerUnavailableError(tErr) {
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
		// v0.6 Phase D: the `\mcp-local-hub-watchdog` skip was dropped with
		// the watchdog engine. A leftover watchdog (or the liveness) task
		// cannot false-match a real manifest's `<server>-<daemon>` lookup in
		// manifestHasScheduledDaemon, so no per-task skip is needed for them.
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
		if !manifestHasInstallSignal(m, scheduledTasks, supervisorInstalledServers) {
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
	// The whole apply runs under hub-mcp.lock. Clear the endpoint-owned durable
	// marker only after every non-skipped client operation succeeded, while the
	// same lock still prevents a newer InstanceID rotation from racing this ack.
	if err := api.ClearHubReconcilePendingLocked(); err != nil {
		return fmt.Errorf("client configs reconciled, but durable reconcile state could not be cleared: %w", err)
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

func manifestHasInstallSignal(m *config.ServerManifest, scheduledTasks map[string]bool, supervisorInstalledServers map[string]struct{}) bool {
	if m == nil {
		return false
	}
	if _, ok := supervisorInstalledServers[m.Name]; ok {
		return true
	}
	return manifestHasScheduledDaemon(m, scheduledTasks)
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
	data, err := api.ReadStateFileInodeAnchored(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			parent := filepath.Dir(path)
			if info, statErr := os.Lstat(parent); statErr != nil {
				if errors.Is(statErr, os.ErrNotExist) {
					return false, nil
				}
				return false, fmt.Errorf("settings parent %s unreadable: %w", parent, statErr)
			} else if !info.IsDir() {
				return false, fmt.Errorf("settings parent %s is not a directory", parent)
			}
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
	upgradeStopAllFn       func() ([]api.RestartResult, error)
	upgradeBootstrapFn     func(io.Writer) error
	upgradeRestartAllFn    func() ([]api.RestartResult, error)
	upgradeRestartTasksFn  func([]string) ([]api.RestartResult, error)
	upgradeInstallServerFn func(server string, w io.Writer) error
	// upgradeExecutableFn / upgradeTargetPathFn carry the canonical-
	// path comparison for the self-replace guard. Tests inject any
	// path pair to drive the refusal branch without filesystem state.
	upgradeExecutableFn func() (string, error)
	upgradeTargetPathFn func() (string, error)
)

func runInstallUpgradePreflightGuards(cmd *cobra.Command) (curExe, target string, err error) {
	errOut := cmd.ErrOrStderr()
	a := api.NewAPI()

	curExe, target, err = resolveUpgradeSelfPaths()
	if err != nil {
		return "", "", fmt.Errorf("resolve self-replace guard paths: %w", err)
	}
	if upgradeIsSelfReplace(curExe, target) {
		return "", "", fmt.Errorf(
			"refusing to --upgrade from the canonical binary at %s "+
				"(current executable %s resolves to the same file via path or symlink/junction/short-name alias): "+
				"the running image cannot replace itself on Windows. "+
				"Build a new binary (e.g. `go build ./cmd/mcphub`) and run "+
				"`./mcphub install --upgrade` (or `.\\mcphub.exe install --upgrade`) "+
				"from the build directory instead.",
			target, curExe)
	}

	if runtime.GOOS == "windows" {
		version := upgradeBuildVersion()
		if version == "dev" || version == "" {
			return "", "", fmt.Errorf(
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
	}

	guiProcs, guiErr := findRunningGUIsOnTarget(a, target)
	if guiErr != nil {
		// Best-effort: a wmic failure must not block the upgrade — fall through
		// and let the binary-copy/rename step surface "target in use" if a GUI
		// is actually running.
		fmt.Fprintf(errOut, "⚠ GUI detection failed (best-effort): %v\n", guiErr)
	} else if len(guiProcs) > 0 {
		pids := make([]string, 0, len(guiProcs))
		for _, p := range guiProcs {
			pids = append(pids, fmt.Sprintf("%d", p.PID))
		}
		return "", "", fmt.Errorf(
			"refusing to --upgrade with a running mcphub GUI on the target path %s "+
				"(PIDs: %s). The GUI process holds the binary file lock; "+
				"Bootstrap would fail with `target in use` and leave the "+
				"daemon fleet down (StopAll runs before Bootstrap). "+
				"Recovery: stop the GUI (tray menu → Quit, or "+
				"`Stop-Process -Id <PID> -Force` in PowerShell), then "+
				"rerun `./mcphub install --upgrade`",
			target, strings.Join(pids, ", "))
	}
	return curExe, target, nil
}

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
// (Historical note: pre-v0.6 the watchdog scheduled task ran every 5
// min and could interleave with this upgrade window, re-locking the
// canonical path from the OLD binary. The v0.6 redesign deleted the
// watchdog engine, so that interleaving race is gone; the supervisor
// owns daemon revival and the liveness task owns owner-relaunch.)
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

	if _, _, err := runInstallUpgradePreflightGuards(cmd); err != nil {
		return err
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
// element is "gui" (or no args at all — the no-arg launch, i.e. an
// Explorer double-click OR a bare `mcphub` typed at a terminal, both
// of which cmd/mcphub/main.go shouldAutoLaunchGUI routes to gui).
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
//  1. Try case-insensitive prefix match: cmdline starts with
//     `target` (literal path). Handles the common case and
//     target paths containing spaces (r2/r3 closures).
//  2. If prefix match fails, extract the image path from
//     cmdline (everything up to the first whitespace not
//     inside quotes) and compare via os.SameFile (r4 closure).
//     Catches 8.3 short paths, junctions/symlinks, and other
//     canonicalization aliases that the prefix match misses
//     because the cmdline holds the alias, not the canonical
//     target string.
//
// After the image-path match (either path), the next non-space
// token must be "gui" (or end-of-string for a no-arg launch —
// Explorer double-click OR a bare `mcphub` typed at a terminal —
// per cmd/mcphub/main.go shouldAutoLaunchGUI).
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
	// to the same file.
	//
	// Codex bot r7 P1 closure on PR #188: a fixed "first
	// whitespace = image boundary" extraction fails when the
	// ALIAS path itself contains spaces (Windows profile dirs
	// with spaces, ALIASed through a junction whose source path
	// is unquoted in the wmic-stripped cmdline). The image
	// extraction must try progressively longer prefixes,
	// calling sameFileOrFalse at each whitespace boundary,
	// stopping at the first match.
	//
	// Quoted-image case (rare — splitCSVLine usually strips
	// quotes, but defensive for PS or direct-CLI input where
	// quotes survive): if a closing `"` appears before the
	// first whitespace, treat that as the image boundary
	// explicitly (preserves embedded spaces in a quoted path).
	cmdline = strings.TrimPrefix(cmdline, `"`)
	if quoteIdx := strings.Index(cmdline, `"`); quoteIdx >= 0 {
		if spaceIdx := strings.IndexAny(cmdline, " \t"); spaceIdx < 0 || quoteIdx < spaceIdx {
			image := cmdline[:quoteIdx]
			rest := strings.TrimLeft(cmdline[quoteIdx+1:], " \t")
			if !sameFileOrFalse(image, target) {
				return false
			}
			return firstArgIsGUI(rest)
		}
	}
	return matchByProgressiveImageBoundary(cmdline, target)
}

// matchByProgressiveImageBoundary tries every whitespace
// position in cmdline as a candidate image/args boundary,
// stopping at the first one where sameFileOrFalse(candidate,
// target) returns true. Handles the codex bot r7 P1 case:
// cmdline holds an alias path containing spaces and no quotes
// (wmic strips quotes before passing to ListMatchingProcesses).
//
// Capped at maxImageBoundaryAttempts whitespace positions to
// bound the os.Stat fan-out — real cmdlines almost never have
// 10+ whitespace boundaries even with path-with-spaces, and
// past that horizon the chance of a true match has practically
// dropped to zero.
const maxImageBoundaryAttempts = 10

func matchByProgressiveImageBoundary(cmdline, target string) bool {
	pos := 0
	for attempt := 0; attempt < maxImageBoundaryAttempts; attempt++ {
		spaceIdx := strings.IndexAny(cmdline[pos:], " \t")
		if spaceIdx < 0 {
			// No more whitespace — try whole remaining string
			// (Explorer-launch + alias path with embedded spaces
			// and no args).
			if sameFileOrFalse(cmdline, target) {
				return firstArgIsGUI("")
			}
			return false
		}
		end := pos + spaceIdx
		candidate := cmdline[:end]
		if sameFileOrFalse(candidate, target) {
			rest := strings.TrimLeft(cmdline[end:], " \t")
			return firstArgIsGUI(rest)
		}
		pos = end + 1
	}
	return false
}

// matchTargetPrefix returns (rest, true) when cmdline starts
// with target (case-insensitive, rune-aware), and (rest, false)
// otherwise. rest is the cmdline content after the prefix,
// leading close-quote and whitespace stripped.
//
// Codex bot r6 P2 closure on PR #188: must NOT slice on
// len(target) after a strings.ToLower roundtrip — Unicode
// case-folding can change byte length (Turkish dotless `ı` ↔
// `İ`, Greek `ς` ↔ `σ`, etc.), so the lowered prefix may not
// align with target's UTF-8 byte positions in the original
// cmdline. A Windows profile path containing such a character
// would mis-slice and `firstArgIsGUI` evaluate the wrong tail.
// Walk the strings rune-by-rune via unicode.ToLower instead,
// counting bytes from the ORIGINAL cmdline so the slice
// boundary is correct.
func matchTargetPrefix(cmdline, target string) (string, bool) {
	sLen := 0
	pLen := 0
	for {
		if pLen == len(target) {
			return cmdlineTailAfterImage(cmdline[sLen:]), true
		}
		if sLen == len(cmdline) {
			return "", false
		}
		sr, sw := utf8.DecodeRuneInString(cmdline[sLen:])
		pr, pw := utf8.DecodeRuneInString(target[pLen:])
		if unicode.ToLower(sr) != unicode.ToLower(pr) {
			return "", false
		}
		sLen += sw
		pLen += pw
	}
}

// cmdlineTailAfterImage strips a single optional close-quote and
// leading whitespace from the cmdline content after the image
// boundary, returning the args portion (which firstArgIsGUI
// consumes).
func cmdlineTailAfterImage(after string) string {
	after = strings.TrimPrefix(after, `"`)
	return strings.TrimLeft(after, " \t")
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
// "gui": a no-arg launch (Explorer double-click OR a bare `mcphub`
// typed at a terminal) lands on gui per cmd/mcphub/main.go
// shouldAutoLaunchGUI.
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

func upgradeRestartTasks(taskNames []string) ([]api.RestartResult, error) {
	if upgradeRestartTasksFn != nil {
		return upgradeRestartTasksFn(taskNames)
	}
	if len(taskNames) == 0 {
		return nil, nil
	}
	sch, err := scheduler.New()
	if err != nil {
		return nil, err
	}
	results := make([]api.RestartResult, 0, len(taskNames))
	for _, taskName := range taskNames {
		name := strings.TrimSpace(taskName)
		if name == "" {
			continue
		}
		if err := sch.Run(name); err != nil {
			results = append(results, api.RestartResult{TaskName: name, Err: err.Error()})
			continue
		}
		results = append(results, api.RestartResult{TaskName: name})
	}
	return results, nil
}

// upgradeNoClientWriteSentinel is a single empty-string entry passed as
// InstallOpts.ClientsInclude to materialize a server's supervisor-intent rows
// WITHOUT rewriting any client config during the legacy-scheduler upgrade
// migration.
//
// bot r33 P2 closure on PR #288: in the legacy migration path,
// upgradeInstallServer is used ONLY to absorb matched legacy scheduler daemons
// into supervisor intent AFTER the binary copy — it must NOT touch client
// configs (the pre-v0.6 upgrade only stopped/copied/restarted daemons). An
// empty/nil ClientsInclude makes api.installClientPredicate fall back to
// clients.DefaultInstallClientNames() (claude-code, codex-cli), so the
// migration would ADD/OVERWRITE those clients' entries even for an operator who
// installed only an opt-in client or hand-customized those configs.
//
// installClientPredicate's contract (internal/api/install.go) is the in-scope
// lever: when ClientsInclude is NON-empty it is used verbatim, and each entry
// that trims to "" is silently dropped BEFORE the unknown-client check — the
// same empty-entry tolerance parseInstallClientsFlag relies on. A single ""
// entry therefore yields a non-empty slice (len 1 → not the default branch)
// whose selected-client set is empty → zero ClientUpdates in the plan, while
// the supervisor-intent / scheduler / daemon materialization (built
// unconditionally before the client loop) still runs.
//
// Scope note: the cleanest fix would be a named api-side knob (e.g.
// InstallOpts.SkipClientConfig bool); that change lives in internal/api which
// is out of this lane's file scope, so this preserve-no-client-writes sentinel
// is the in-scope equivalent. Tracked as a follow-up to replace the sentinel
// with an explicit option.
var upgradeNoClientWriteSentinel = []string{""}

// upgradeServerInstallFn is a narrow test seam ONE level below
// upgradeInstallServerFn: it intercepts the api.InstallOpts the production
// upgradeInstallServer body constructs, so a test can assert the call site
// passes the no-client-write sentinel (FIX 3, bot r33 P2 on PR #288) without
// driving a real install. nil → the real api.NewAPI().Install.
var upgradeServerInstallFn func(api.InstallOpts) error

func resolveInstallGUIPort() int {
	// NoCreate: this runs at InstallOpts construction (before the dry-run gate),
	// so it must not create the per-user GUI dir as a side effect of a dry-run.
	pidportPath, err := gui.PidportPathNoCreate()
	if err != nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	v := gui.Probe(ctx, pidportPath)
	if v.Class != gui.VerdictHealthy || v.Port <= 0 {
		return 0
	}
	return v.Port
}

func upgradeInstallServer(server string, w io.Writer) error {
	if upgradeInstallServerFn != nil {
		return upgradeInstallServerFn(server, w)
	}
	opts := api.InstallOpts{
		Server:         server,
		ClientsInclude: upgradeNoClientWriteSentinel,
		Writer:         w,
		GUIPort:        resolveInstallGUIPort(),
	}
	if upgradeServerInstallFn != nil {
		return upgradeServerInstallFn(opts)
	}
	return api.NewAPI().Install(opts)
}

// ---------------------------------------------------------------------------
// `mcphub install --upgrade` routing (v0.6 Phase F).
//
// Phase F deleted the v0.4.x→v0.5.0 forward-migration engine and the
// `--rollback-to-legacy` demotion path (no v0.4.x scheduler model survives to
// migrate from or roll back to). --upgrade now routes between just two sinks
// based on machine state:
//
//   1. v0.5.x present (supervisor-intent.json on disk) → cli.RunInstallUpgrade,
//      which drives the rename-aside + IPC handoff per spec §"Upgrade sequence".
//   2. Fresh install (no supervisor-intent.json) → the legacy
//      runInstallUpgrade body so the first-time binary-copy + restart still
//      works from a build directory.
//
// Production deps wiring lives in install_migration_wiring_windows.go
// (Windows-only) — the rename-aside / IPC / supervisor-spawn surfaces are all
// Windows-specific. POSIX builds leave v5UpgradeFn nil and fall back to the
// legacy runInstallUpgrade body.
// ---------------------------------------------------------------------------

// upgradeDispatcher is the function-pointer seam tests inject to override the
// production routing. nil → use the real dispatchUpgradeReal helper. Tests set
// it in setup and clear it in teardown to keep production logic out of their
// assertion path.
var upgradeDispatcher func(cmd *cobra.Command) error

// dispatchUpgrade routes the --upgrade command per the machine-state decision
// tree above. The upgradeDispatcher seam lets tests stub it without driving
// the real Windows-only wiring.
func dispatchUpgrade(cmd *cobra.Command) error {
	if upgradeDispatcher != nil {
		return upgradeDispatcher(cmd)
	}
	return dispatchUpgradeReal(cmd)
}

// hasSupervisorIntent reports whether the state-dir's supervisor-intent.json
// names an actual v0.5.x supervisor — i.e. carries at least one
// supervisor-owned DAEMON DESCRIPTOR row (len(intent.Daemons) > 0 after the
// existing one-shot/maintenance filtering in api.ReadSupervisorIntent).
//
//   - ≥1 daemon row     → v0.5.x machine (cold-restart upgrade path).
//   - file absent        → fresh install (binary-copy fallback).
//   - descriptor-less    → "no v0.5 supervisor". A stops-only / daemon-less
//     intent file (e.g. a v0.4 scheduler-only host where someone ran a
//     `mcphub ... stop`, minting a supervisor-intent.json that carries ONLY
//     Stops and no Daemons) must NOT be treated as a v0.5 install. Routing it
//     as v0.5 takes runV5UpgradeReal and SKIPS the legacy-scheduler migration,
//     so existing legacy scheduler tasks are never materialized into
//     supervisor intent (bot r32 P2). Returning false here routes such files
//     down the legacy-scheduler probe instead.
//
// Returns (false, nil) on os.ErrNotExist for both the file and its parent
// directory — both mean "no supervisor intent on disk" and the caller treats
// them identically.
func hasSupervisorIntent() (bool, error) {
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		// Treat "cannot resolve state-dir" as "no supervisor intent" rather
		// than aborting upgrade routing; for the routing decision a missing
		// state-dir means "fresh install".
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("resolve state-dir: %w", err)
	}
	path := filepath.Join(stateDir, "supervisor-intent.json")
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		// Corrupt state-dir: a directory named supervisor-intent.json.
		// Round-4 fix (codex-r4-a/c-p1): the prior silent (false, nil)
		// branch let the dispatcher fall through to legacy
		// runInstallUpgrade even though os.Stat reports the path
		// exists — the round-3 unreadable-intent guard inside
		// runV5UpgradeWindows then never fires. Surface a non-nil
		// error so the routing dispatcher fails closed and the
		// operator sees the corruption shape.
		return false, fmt.Errorf("hasSupervisorIntent: %s is a directory (corrupt state-dir; rename/delete and re-run)", path)
	}
	if !info.Mode().IsRegular() {
		// Defense-in-depth for symlink / named pipe / socket / device
		// entries that resolved through os.Stat to a non-regular kind.
		// Same fail-closed rationale as the IsDir branch above.
		return false, fmt.Errorf("hasSupervisorIntent: %s is not a regular file (mode %v)", path, info.Mode())
	}
	// Mere regular-file presence is NOT enough (bot r32 P2): a stops-only /
	// descriptor-less file would otherwise route as a v0.5 install and skip
	// the legacy-scheduler migration. Read the intent and require at least one
	// daemon row. api.ReadSupervisorIntent strips legacy one-shot daemons
	// first, so the count reflects real long-lived supervisor descriptors.
	//
	// A read error is a hard, wrapped, fail-closed error (NOT a silent false):
	// the file the routing discriminator relied on is unreadable (corrupt
	// JSON, EBUSY race, permission drift). Failing closed makes the dispatcher
	// surface the real cause instead of silently mis-routing.
	intent, err := api.ReadSupervisorIntent(path)
	if err != nil {
		return false, fmt.Errorf("hasSupervisorIntent: read %s: %w", path, err)
	}
	if intent == nil {
		return false, fmt.Errorf("hasSupervisorIntent: %s decoded to nil (corrupt envelope)", path)
	}
	return len(intent.Daemons) > 0, nil
}

// dispatchUpgradeReal implements the production routing branch. It asks
// api.DaemonStateDir + os.Stat about supervisor-intent.json, then dispatches
// to one of three sinks:
//
//   - cli.RunInstallUpgrade (v0.5.x → v0.5.x cold-restart upgrade)
//   - binary-copy + per-server install materialization (v0.4 scheduler-only)
//   - runInstallUpgrade legacy body (fresh install)
//
// Any state-probe failure short-circuits with the wrapped error so the
// operator sees the real cause instead of a confused fallback.
func dispatchUpgradeReal(cmd *cobra.Command) error {
	supervisorPresent, err := hasSupervisorIntent()
	if err != nil {
		return fmt.Errorf("upgrade routing: probe supervisor-intent.json: %w", err)
	}
	if supervisorPresent {
		return runV5UpgradeReal(cmd)
	}
	legacyProbe, err := probeLegacySchedulerUpgradeServers(api.NewAPI())
	if err != nil {
		return fmt.Errorf("upgrade routing: probe legacy scheduler tasks: %w", err)
	}
	if len(legacyProbe.legacyTasks) > 0 {
		if len(legacyProbe.servers) == 0 {
			return fmt.Errorf(
				"legacy v0.4 scheduler daemon tasks exist but none match a shipped manifest; refusing to silently restart the deleted legacy task model. "+
					"Run `mcphub setup`, then `mcphub install --server <server>` for each installed server. Legacy tasks: %s",
				strings.Join(legacyProbe.legacyTasks, ", "))
		}
		return runLegacySchedulerUpgradeMigration(cmd, legacyProbe)
	}
	// Fresh install: no supervisor state on disk. Fall through to the legacy
	// runInstallUpgrade body so first-time copy + restart still works.
	return runInstallUpgrade(cmd)
}

type legacyUpgradeProbe struct {
	servers     []string
	legacyTasks []string
	unmatched   []string
}

func probeLegacySchedulerUpgradeServers(a *api.API) (legacyUpgradeProbe, error) {
	tasks, err := a.ListManagedTasks()
	if err != nil {
		if api.SchedulerUnavailableError(err) {
			return legacyUpgradeProbe{}, nil
		}
		return legacyUpgradeProbe{}, err
	}

	scheduledTasks := make(map[string]bool, len(tasks))
	legacyTasks := make([]string, 0, len(tasks))
	for _, task := range tasks {
		name := strings.TrimPrefix(task.Name, "\\")
		if !legacyUpgradeTaskLooksDaemon(name) {
			continue
		}
		scheduledTasks[name] = true
		legacyTasks = append(legacyTasks, name)
	}
	sort.Strings(legacyTasks)
	if len(legacyTasks) == 0 {
		return legacyUpgradeProbe{}, nil
	}

	manifestNames, err := a.ManifestList()
	if err != nil {
		return legacyUpgradeProbe{}, err
	}
	servers := map[string]struct{}{}
	matchedTasks := map[string]struct{}{}
	const prefix = "mcp-local-hub-"
	for _, name := range manifestNames {
		raw, err := a.ManifestGet(name)
		if err != nil {
			return legacyUpgradeProbe{}, fmt.Errorf("read manifest %s: %w", name, err)
		}
		m, err := config.ParseManifest(strings.NewReader(raw))
		if err != nil {
			return legacyUpgradeProbe{}, fmt.Errorf("parse manifest %s: %w", name, err)
		}
		if m == nil || m.Kind != config.KindGlobal {
			continue
		}
		for _, d := range m.Daemons {
			taskName := prefix + m.Name + "-" + d.Name
			if scheduledTasks[taskName] {
				servers[m.Name] = struct{}{}
				matchedTasks[taskName] = struct{}{}
			}
		}
	}

	out := legacyUpgradeProbe{
		servers:     make([]string, 0, len(servers)),
		legacyTasks: legacyTasks,
	}
	for server := range servers {
		out.servers = append(out.servers, server)
	}
	sort.Strings(out.servers)
	for _, taskName := range legacyTasks {
		if _, ok := matchedTasks[taskName]; !ok {
			out.unmatched = append(out.unmatched, taskName)
		}
	}
	return out, nil
}

func legacyUpgradeTaskLooksDaemon(name string) bool {
	const prefix = "mcp-local-hub-"
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	// bot r33 P2 closure on PR #288: workspace-scoped LSP and serena
	// per-workspace tasks ALSO match the `mcp-local-hub-<rest-with-hyphen>`
	// shape below (`mcp-local-hub-lsp-<wsKey>-<lang>` and
	// `mcp-local-hub-serena-<wsKey>`), so classifying them as migratable
	// global daemons makes `legacyTasks` non-empty while `servers` stays
	// empty on a workspace-only host — and dispatchUpgradeReal then ABORTS
	// with "none match a shipped manifest" instead of doing the normal
	// binary-copy/restart. Those tasks are NOT global daemons (they belong
	// to the per-workspace `mcphub register` flow, not `mcphub install
	// --server X`), so exclude them up-front. Reuse the canonical structural
	// predicates from internal/api (both accept the bare, leading-backslash-
	// stripped form this function receives) rather than re-deriving the
	// task-name shapes here.
	if api.IsLazyProxyTaskName(name) || api.IsSerenaTaskName(name) {
		return false
	}
	rest := strings.TrimPrefix(name, prefix)
	if rest == "" ||
		rest == "liveness" ||
		rest == "watchdog" ||
		rest == "supervisor" ||
		strings.HasSuffix(rest, "-weekly-refresh") {
		return false
	}
	return strings.Contains(rest, "-")
}

func runLegacySchedulerUpgradeMigration(cmd *cobra.Command, probe legacyUpgradeProbe) error {
	a := api.NewAPI()
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	if _, _, err := runInstallUpgradePreflightGuards(cmd); err != nil {
		return err
	}

	// Lock-safety verdict for the v0.4 scheduler migration route: this path
	// still uses upgradeBootstrap's plain copy-only helper, not the v0.5
	// rename-aside handoff. Any running legacy daemon can hold the canonical
	// binary lock on Windows, including unmatched custom/workspace tasks. Stop
	// the whole legacy scheduler fleet for the copy, then explicitly re-run the
	// unmatched task names so only matched shipped manifests are absorbed into
	// supervisor intent.
	fmt.Fprintln(out, "Stopping legacy scheduler daemons...")
	stopResults, err := upgradeStopAll(a)
	if err != nil {
		return fmt.Errorf("stop legacy scheduler daemons: %w", err)
	}
	for _, r := range stopResults {
		if r.Err != "" {
			fmt.Fprintf(errOut, "⚠ stop %s: %s\n", r.TaskName, r.Err)
		} else {
			fmt.Fprintf(out, "✓ stopped %s\n", r.TaskName)
		}
	}

	fmt.Fprintln(out, "Copying new binary...")
	if err := upgradeBootstrap(out); err != nil {
		return fmt.Errorf(
			"bootstrap (binary copy) failed after legacy daemons were stopped: %w; "+
				"recovery: re-run `mcphub install --upgrade` (idempotent), "+
				"or run `mcphub setup` then `mcphub install --server <server>` for each legacy server",
			err)
	}

	if len(probe.unmatched) > 0 {
		fmt.Fprintln(out, "Restarting unmatched legacy scheduler daemons...")
		restartResults, err := upgradeRestartTasks(probe.unmatched)
		if err != nil {
			return fmt.Errorf("restart unmatched legacy scheduler daemons after binary copy: %w", err)
		}
		var failed []string
		for _, r := range restartResults {
			if r.Err != "" {
				failed = append(failed, r.TaskName)
				fmt.Fprintf(errOut, "✗ restart unmatched %s: %s\n", r.TaskName, r.Err)
			} else {
				fmt.Fprintf(out, "✓ restarted unmatched %s\n", r.TaskName)
			}
		}
		if len(failed) > 0 {
			return fmt.Errorf(
				"%d unmatched legacy scheduler task(s) failed to restart after binary copy (%s); "+
					"recovery: run `schtasks /Run /TN <task>` for each failed task, then rerun `mcphub install --upgrade` if migrated servers are still missing",
				len(failed), strings.Join(failed, ", "))
		}
		fmt.Fprintf(errOut,
			"⚠ legacy scheduler tasks without matching shipped manifests were left for manual review: %s\n",
			strings.Join(probe.unmatched, ", "))
	}
	fmt.Fprintln(out, "Materializing supervisor intent for legacy servers...")
	for _, server := range probe.servers {
		if err := upgradeInstallServer(server, out); err != nil {
			return fmt.Errorf(
				"install migrated server %s: %w; recovery: run `mcphub setup`, then `mcphub install --server %s`",
				server, err, server)
		}
		fmt.Fprintf(out, "✓ installed %s into supervisor intent\n", server)
	}
	return nil
}

// runV5UpgradeReal wires the rename-aside + IPC handoff path through
// cli.RunInstallUpgrade. Production wiring lives in
// install_migration_wiring_windows.go.
func runV5UpgradeReal(cmd *cobra.Command) error {
	if v5UpgradeFn == nil {
		// POSIX or unwired production path. The cold-restart upgrade flow
		// targets a Windows supervisor; POSIX builds reach this branch only
		// via misuse and the legacy runInstallUpgrade is the closest safe
		// fallback.
		return runInstallUpgrade(cmd)
	}
	if _, _, err := runInstallUpgradePreflightGuards(cmd); err != nil {
		return err
	}
	return v5UpgradeFn(cmd)
}

// v5UpgradeFn is the production-wiring seam set by the init function in
// install_migration_wiring_windows.go (Windows) or left nil on POSIX. Tests
// override the higher-level upgradeDispatcher seam instead.
var v5UpgradeFn func(cmd *cobra.Command) error
