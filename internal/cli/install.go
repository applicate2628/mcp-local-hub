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

Prerequisites:
  - First-time users: run 'mcphub setup' once to canonicalize the binary
    at ~/.local/bin and register it on user PATH
  - Secrets (wolfram, paper-search-mcp): 'mcphub secrets set <key>' first
  - Windows: Task Scheduler backend only. Linux/macOS ship compile-only stubs.

See also: status, restart, uninstall, rollback, scheduler upgrade.`,
		RunE: func(cmd *cobra.Command, args []string) error {
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
	//    per-daemon entries + remove the aggregate.
	gateOn := readHubEndpointGateForReconcile()

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
	endpoint, epErr := api.LoadHubEndpoint()
	tokens, tokErr := api.ReloadHubTokens()
	if gateOn {
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
	}

	// 3. Collect manifests for every supported server. The Scan
	//    surface lists every server with a manifest; ManifestGet
	//    returns the YAML; config.ParseManifest decodes into the
	//    typed struct the reconcile planner expects.
	scan, sErr := a.Scan()
	if sErr != nil {
		return fmt.Errorf("scan manifests: %w", sErr)
	}
	var manifests []config.ServerManifest
	for _, entry := range scan.Entries {
		if !entry.ManifestExists {
			continue
		}
		yaml, gErr := a.ManifestGet(entry.Name)
		if gErr != nil {
			if errors.Is(gErr, os.ErrNotExist) {
				continue // benign scan-window race
			}
			return fmt.Errorf("read manifest %q: %w", entry.Name, gErr)
		}
		m, pErr := config.ParseManifest(strings.NewReader(yaml))
		if pErr != nil {
			return fmt.Errorf("parse manifest %q: %w", entry.Name, pErr)
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
	for _, f := range report.Failed {
		fmt.Fprintf(cmd.OutOrStderr(), "✗ %s (%s): %s\n", f.Client, f.Phase, f.Err)
	}
	if len(report.Failed) > 0 {
		return fmt.Errorf("%d client reconcile failure(s) — see stderr; rerun to converge",
			len(report.Failed))
	}
	return nil
}

// readHubEndpointGateForReconcile mirrors the gui-package private
// helper readHubEndpointGateFromSettings (internal/gui/hub_listener.go)
// — fail-closed when settings file is missing / malformed. Inlined
// here so the cli package doesn't depend on gui or need an
// api-package export for a 4-line yaml lookup.
func readHubEndpointGateForReconcile() bool {
	path := api.SettingsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	raw := map[string]string{}
	if uerr := yaml.Unmarshal(data, &raw); uerr != nil {
		return false
	}
	return raw["gui_server.hub_endpoint_enabled"] == "true"
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
