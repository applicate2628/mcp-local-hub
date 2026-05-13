// internal/cli/hubmcp.go — Phase 5 Task 5.2.
//
// `mcphub hub-mcp` command tree: status / regenerate-token /
// regenerate-instance-id. Operator-facing surface for the G4 unified
// hub MCP endpoint.
//
// Spec: docs/superpowers/plans/2026-05-12-g4-unified-hub-mcp.md
// §"Settings + CLI surface" + Task 5.2.

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
)

// newHubMcpCmd returns the cobra parent command tree. Wired into
// rootCmd.AddCommand by NewRootCmd in root.go.
func newHubMcpCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "hub-mcp",
		Short: "Hub MCP endpoint operations",
		Long: `Operate on the G4 unified hub MCP endpoint.

The endpoint is the single HTTP listener (default port from
hub-mcp.endpoint.json) that proxies per-client MCP traffic to the
underlying daemons. Each client (claude-code, codex-cli, cursor, ...)
authenticates with its own 64-hex token + the per-process instance_id.

Subcommands:
  status                   Show endpoint state (presence-only — no tokens).
  regenerate-token         Rotate one client's hub-mcp token.
  regenerate-instance-id   Rotate the persistent instance id (invalidates
                           every client config — every client must reinstall).`,
	}
	c.AddCommand(newHubMcpStatusCmd(), newHubMcpRegenTokenCmd(), newHubMcpRegenInstanceIDCmd())
	return c
}

func newHubMcpStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show hub-mcp endpoint state (presence-only, no token bytes)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ep, epErr := api.LoadHubEndpoint()
			// codex bot phase5 r3 P2 closure on PR #160: read tokens
			// from DISK, not the in-memory cache. A fresh CLI process
			// has a nil cache until Ensure/Rotate/Reload runs, so
			// CurrentTokenTable would report empty tokens even when
			// hub-mcp-tokens.json exists on disk. ReloadHubTokens
			// reads + parses + publishes — exactly what status needs
			// for an accurate snapshot, even on cold CLI invocations.
			// The side-effect of publishing the cache is harmless:
			// the next process that calls CurrentTokenTable just sees
			// the same table that's already on disk.
			tbl, tblErr := api.ReloadHubTokens()
			events, _ := api.RecentHubMcpEvents(8)

			perClient := map[string]string{}
			for client, tok := range tbl.Tokens {
				if tok == "" {
					perClient[client] = "ABSENT"
				} else {
					perClient[client] = "PRESENT"
				}
			}

			out := map[string]any{
				"port":              ep.Port,
				"pid":               ep.PID,
				"started_at":        ep.StartedAt,
				"instance_id":       presenceTag(ep.InstanceID),
				"per_client_tokens": perClient,
				"recent_events":     events,
			}
			if epErr != nil {
				out["endpoint_error"] = epErr.Error()
			}
			if tblErr != nil {
				out["token_table_error"] = tblErr.Error()
			}
			raw, err := json.MarshalIndent(out, "", "  ")
			if err != nil {
				return fmt.Errorf("marshal status: %w", err)
			}
			// Defense-in-depth: pipe through RedactToken even though
			// every emit path already redacts at write time. A late
			// regression somewhere else cannot leak a 64-hex via the
			// CLI surface.
			fmt.Fprintln(cmd.OutOrStdout(), api.RedactToken(string(raw)))
			return nil
		},
	}
}

func newHubMcpRegenTokenCmd() *cobra.Command {
	var client string
	var yes bool
	c := &cobra.Command{
		Use:   "regenerate-token",
		Short: "Rotate one client's hub-mcp token",
		Long: `Generate a new 64-hex token for the named client and persist it
to hub-mcp-tokens.json. The live hub picks up the new tokens via the
internal /reload-tokens endpoint within ms.

After a successful rotation, the existing client config is stale if
the hub-endpoint gate is ON (client config carries the per-client
token in its header). Run ` + "`mcphub install --reconcile-hub-mode`" + `
to refresh every client's aggregate entry with the new token. Gate
OFF: no client refresh needed (per-daemon URLs don't carry this
token).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if client == "" {
				return fmt.Errorf("--client is required")
			}
			// codex bot phase5 r12 P1 closure on PR #160: validate
			// --client against the supported-adapter allowlist BEFORE
			// rotating. RotateHubToken creates a fresh entry when the
			// key is absent (used by EnsureHubTokens), so a typo like
			// `--client claud-code` would create a new token entry for
			// the typo and exit "successfully" while the actual
			// compromised client's token stays untouched. Operator
			// reads "Rotated token for client claud-code" as success
			// even though no real rotation happened — defeats the
			// security intent of rotation.
			supported := false
			for _, name := range clients.SupportedClientNames() {
				if name == client {
					supported = true
					break
				}
			}
			if !supported {
				return fmt.Errorf("unknown client %q (expected %s)", client, strings.Join(clients.SupportedClientNames(), " | "))
			}
			if !yes && !inputIsTerminal(cmd.InOrStdin()) {
				fmt.Fprintln(cmd.ErrOrStderr(), "non-TTY input requires --yes to confirm rotation")
				return &forceExitError{code: 6}
			}

			// 1. Rotate on disk under flock.
			if _, err := api.RotateHubToken(client); err != nil {
				return fmt.Errorf("rotate token: %w", err)
			}

			// 2. Read control token + POST /internal/reload-tokens
			//    so the live hub picks up the new table.
			//
			// codex bot phase5 r4 P2 closure on PR #160: a missing
			// endpoint.json (first-run, after `mcphub gui --reset-port`,
			// or after a clean state-dir wipe) is the same operational
			// case as ep.Port==0 — there is no live hub to reload.
			// Persistence is already complete, so exit 0 with the
			// "next start picks it up" message instead of failing
			// with exit 1 / the rotate-but-not-applied warning.
			ep, epErr := api.LoadHubEndpoint()
			if epErr != nil {
				if os.IsNotExist(epErr) || errors.Is(epErr, os.ErrNotExist) {
					printRotationOK(cmd, client, "(hub not running, endpoint state absent — next start picks up the new token)")
					return nil
				}
				return fallbackReloadWarning(cmd, fmt.Errorf("load endpoint: %w", epErr))
			}
			if ep.Port == 0 {
				// Hub never bound — no live process to reload. Persistence
				// is enough; the next hub start will pick up the new table.
				printRotationOK(cmd, client, "(hub not running — next start picks up the new token)")
				return nil
			}
			controlTok, ctErr := api.ReadHubMcpControlToken()
			if ctErr != nil {
				return fallbackReloadWarning(cmd, fmt.Errorf("read control token: %w", ctErr))
			}
			if err := postReloadTokens(cmd, ep.Port, controlTok); err != nil {
				return fallbackReloadWarning(cmd, err)
			}

			printRotationOK(cmd, client, "")
			return nil
		},
	}
	c.Flags().StringVar(&client, "client", "", "client adapter id (required, e.g. claude-code)")
	c.Flags().BoolVar(&yes, "yes", false, "confirm in non-TTY contexts (required when stdin is not a terminal)")
	_ = c.MarkFlagRequired("client")
	return c
}

func newHubMcpRegenInstanceIDCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "regenerate-instance-id",
		Short: "Rotate the persistent hub-mcp instance id",
		Long: `Generate a new 64-hex instance_id. Every existing client config
becomes stale — every client must rerun ` + "`mcphub install`" + ` to
refresh their X-Mcphub-Instance-Id header against the new value.

The running hub server caches the instance_id in memory at startup
and has no live-reload protocol for this field (tokens have one via
/internal/reload-tokens; the endpoint state does not). So:

  - If the hub is running when you rotate, the command will print
    a CRITICAL warning + exit non-zero. Stop the hub, rerun the
    rotation if needed, then start the hub fresh.
  - If the hub is not running, the rotation succeeds cleanly and
    the next start picks up the new id.

Use this when the instance_id may have been compromised (e.g. leaked
client config snapshot). For routine per-client token compromise, use
` + "`mcphub hub-mcp regenerate-token --client X`" + ` instead.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes && !inputIsTerminal(cmd.InOrStdin()) {
				fmt.Fprintln(cmd.ErrOrStderr(), "non-TTY input requires --yes to confirm rotation")
				return &forceExitError{code: 6}
			}
			// codex deep-sec phase5 r11 P1 closure on PR #160 (protocol
			// + concurrency lanes agreed): probe liveness BEFORE
			// mutating disk state. The pre-r11 ordering rotated the
			// instance_id on disk first, then checked liveness — if
			// the hub was alive, we ended up with a split-brain state
			// (disk = NEW id, in-memory hub = OLD id) AND a cli-error
			// exit. An operator interrupting the rotation (Ctrl-C
			// between the write and the probe) would leave the same
			// split state without seeing the warning. Probe first,
			// refuse on live hub, then rotate — disk only ever gets
			// the new id when the hub is actually stopped.
			ep, epErr := api.LoadHubEndpoint()
			if epErr == nil && ep.Port > 0 && hubProbeAlive(cmd, ep.Port) {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"CRITICAL: refusing to rotate instance_id while the hub is running.")
				fmt.Fprintln(cmd.ErrOrStderr(),
					"  The live hub caches instance_id in-memory at startup and has no live-reload for the endpoint state.")
				fmt.Fprintln(cmd.ErrOrStderr(),
					"  Rotating now would leave disk on the NEW id while the running hub keeps accepting the OLD id —")
				fmt.Fprintln(cmd.ErrOrStderr(),
					"  any client reinstalled with the new id would get 401 until the hub restarts.")
				fmt.Fprintln(cmd.ErrOrStderr(),
					"  Stop the hub (close the GUI / kill the daemon) first, then rerun this command.")
				return &forceExitError{code: 1}
			}
			if _, err := api.RotateHubInstanceID(); err != nil {
				return fmt.Errorf("rotate instance id: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(),
				"Rotated hub-mcp instance_id. Every client config is now stale.\n"+
					"Rerun `mcphub install --reconcile-hub-mode` for every client to refresh the live config.")
			return nil
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "confirm in non-TTY contexts (required when stdin is not a terminal)")
	return c
}

// hubProbeAlive returns true if the hub at <port> answers a
// reload-tokens probe in the hub-specific shape: HEAD to
// /internal/reload-tokens with a VALID control token must yield
// 204 — only the live hub-mcp control handler emits that status
// for HEAD after the loopback + auth gates pass. Any other status,
// connect-refused, or unreadable control token is treated as "hub
// not running" so a stale endpoint.Port reused by an unrelated
// local HTTP service does NOT false-positive.
//
// codex bot phase5 r2 P2 closure on PR #160: pre-r2 probe accepted
// 401/403/405/204 from ANY HTTP responder — fail-loud branch would
// fire on a port inherited by another local service. r2 fix sent
// POST + control token + accepted 204|429.
//
// codex bot phase5 r9 P2 closure on PR #160: the r2 POST probe
// performs a real token reload as a side effect and bumps the
// 5s cooldown timer. That breaks chained rotation flows — e.g.,
// `mcphub hub-mcp regenerate-instance-id` (which probes liveness
// to refuse on a live hub) followed by `mcphub hub-mcp
// regenerate-token` (which POSTs /internal/reload-tokens to push
// the new table) would get 429 on the second call because the
// probe consumed the cooldown window. Switch to HEAD — handler
// short-circuits to 204 after auth without touching reloadMutex
// or lastReload. POST stays the mutating live-apply path.
//
// If the control token file is missing (e.g., hub crashed mid-
// startup before writing it), we cannot prove the hub is alive →
// return false. The CLI will exit clean, and any actual live hub
// will surface the staleness via 401 on the next client call.
func hubProbeAlive(cmd *cobra.Command, port int) bool {
	controlTok, err := api.ReadHubMcpControlToken()
	if err != nil {
		return false // no control token = no way to prove hub-specific
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/internal/reload-tokens", port)
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodHead, url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("X-Mcphub-Control-Token", controlTok)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// 204 on HEAD after auth → confirmed hub. A stranger service
	// holding the port would either 405 the unknown route or 401
	// against an arbitrary 64-hex token (no in-memory match).
	return resp.StatusCode == http.StatusNoContent
}

// postReloadTokens fires the live-reload POST against the running hub.
// 204 = success. Any other status or transport error → returned error
// so the caller can surface the rotate-persisted-but-not-applied state.
func postReloadTokens(cmd *cobra.Command, port int, controlTok string) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/internal/reload-tokens", port)
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, url, nil)
	if err != nil {
		return fmt.Errorf("build reload request: %w", err)
	}
	req.Header.Set("X-Mcphub-Control-Token", controlTok)
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("POST /internal/reload-tokens: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("POST /internal/reload-tokens: HTTP %d", resp.StatusCode)
	}
	return nil
}

func fallbackReloadWarning(cmd *cobra.Command, cause error) error {
	fmt.Fprintln(cmd.ErrOrStderr(),
		"rotate persisted to disk but live hub did not confirm; "+
			"restart the hub to apply, or rerun this command once the hub is reachable")
	fmt.Fprintln(cmd.ErrOrStderr(), "  cause:", cause)
	return &forceExitError{code: 1}
}

func printRotationOK(cmd *cobra.Command, client, suffix string) {
	// codex bot phase5 r5+r6 P2 closures on PR #160:
	// r5 caught `--client` (singular) doesn't exist (flag is
	// `--clients` plural). r6 caught that `mcphub install` still
	// requires `--server` or `--all` even with `--clients`, so
	// `mcphub install --clients X` alone fails with "--server is
	// required" and leaves the rotated token unapplied in client
	// config. The runnable + gate-aware path is
	// `--reconcile-hub-mode` (which walks every manifest from disk
	// and rewrites every client entry with the new token). Gate-OFF
	// client configs carry no hub token, so refresh is a no-op there.
	msg := fmt.Sprintf(
		"Rotated token for client %s. If the hub-endpoint gate is ON, run `mcphub install --reconcile-hub-mode` to refresh client configs with the new token. Gate OFF: no client refresh needed.",
		client,
	)
	if suffix != "" {
		msg = msg + " " + suffix
	}
	fmt.Fprintln(cmd.OutOrStdout(), msg)
}

// presenceTag converts a sensitive string into a presence-only token
// for the status surface. Empty → ABSENT. Anything else → PRESENT.
// Never echoes the actual value, even after RedactToken.
func presenceTag(s string) string {
	if s == "" {
		return "ABSENT"
	}
	return "PRESENT"
}

// hubMcpExitCodeForTest is a test seam — production code never reads
// it. Kept here so a future test that needs to inject a non-default
// exit shape can do so without breaking the production paths.
var _ = os.Stdin
