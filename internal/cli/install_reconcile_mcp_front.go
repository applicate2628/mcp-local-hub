// internal/cli/install_reconcile_mcp_front.go
//
// `mcphub install --reconcile-mcp-front[/--rollback]` — sub-increment 2a of
// the MCP front-daemon decision
// (work-items/decisions/2026-07-25-increment2-mcp-front-port-ownership.md).
//
// This is the ONE operator-gated write path that rewrites in-scope client
// serena/LSP entries from the GUI's own port to the settings-owned
// mcp_front.port (the supervisor-managed `mcphub route` front daemon's
// port). It is a THIN composition over the existing, tested
// api.ReconcileSerenaClientsToRouter + api.EnsureLSPRouterClientEntries
// (forward) and api.RestoreSerenaReconcileApplied +
// api.RollbackLSPRouterClientEntries (--rollback) — no reconcile/backup/
// rollback logic is reimplemented here.
//
// Fail-closed gate: ReconcileSerenaClientsToRouter's own port-liveness proof
// (resolveSerenaReconcilePort -> defaultRouterReadinessPing, which performs
// BOTH the HEAD/405 same-router-shape check AND a real MCP `initialize`
// round-trip) runs UNCONDITIONALLY before any client write, regardless of
// how many serena clients are actually present — so calling the serena
// reconcile FIRST doubles as the whole command's shared liveness gate. If
// it returns ErrSerenaReconcileRouteNotLive (a whole-run blocker, distinct
// from a per-client Failed row), this file aborts BEFORE touching LSP at
// all — nothing is written on either surface when the route isn't proven
// live.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

// mcpFrontReconcileSerenaReportLeaf is the state-file basename the forward
// reconcile persists its serena MigrateReport under, so a LATER, separate
// `--rollback` invocation can restore each rewritten client from the exact
// backup the forward run captured (api.RestoreSerenaReconcileApplied needs
// that report — it is not self-contained the way api.
// RollbackLSPRouterClientEntries is). Lives alongside the other install-side
// state-bookkeeping files under DaemonStateDir(), written through the same
// hardened WriteStateFileAtomic pipeline every other state file uses.
const mcpFrontReconcileSerenaReportLeaf = "mcp-front-reconcile-serena-report.json"

// mcpFrontReconcileTimeout bounds the whole reconcile-mcp-front run
// (liveness probe + every in-scope client write for both serena and LSP).
// Generous relative to the two per-request 2s budgets
// defaultRouterReadinessPing/mcpInitializeProbe use internally, since this
// wraps a full client-config walk across the fixed client set, not a
// single HTTP round-trip.
const mcpFrontReconcileTimeout = 30 * time.Second

// mcpFrontReconcileSerenaReportPathFn is a package-level test seam (mirrors
// resolveMCPFrontPortFn / reconcileSpawnFn's style elsewhere in this
// package). api.DaemonStateDir() resolves the Windows path via
// SHGetKnownFolderPath in a production build, which does NOT honor
// LOCALAPPDATA / MCPHUB_STATE_DIR_OVERRIDE unless the whole test binary is
// compiled with -tags test_state_path_env — an in-process test of this
// command that does not control its own build tags must not rely on that.
// Overriding this var lets a test point report persistence at a t.TempDir()
// directly, with zero risk of touching a real operator's state dir
// regardless of how the test binary was built.
var mcpFrontReconcileSerenaReportPathFn = mcpFrontReconcileSerenaReportPath

func mcpFrontReconcileSerenaReportPath() (string, error) {
	dir, err := api.DaemonStateDir()
	if err != nil {
		return "", fmt.Errorf("resolve state dir: %w", err)
	}
	return filepath.Join(dir, mcpFrontReconcileSerenaReportLeaf), nil
}

// runReconcileMCPFront dispatches to the forward reconcile or its rollback.
func runReconcileMCPFront(cmd *cobra.Command, rollback bool) error {
	a := api.NewAPI()
	if rollback {
		return runRollbackMCPFront(cmd, a)
	}
	return runForwardReconcileMCPFront(cmd, a)
}

func runForwardReconcileMCPFront(cmd *cobra.Command, a *api.API) error {
	port, err := a.ResolveMCPFrontPort()
	if err != nil {
		return fmt.Errorf("reconcile-mcp-front: resolve mcp_front.port: %w", err)
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), mcpFrontReconcileTimeout)
	defer cancel()

	keepN := effectiveBackupKeepN()

	// Serena FIRST — its own port-liveness proof (Port>0 path,
	// serena_client_reconcile.go's resolveSerenaReconcilePort) is the shared
	// fail-closed gate for this whole command. RemoveLegacy is deliberately
	// false: the pre-dynamic-pool legacy port-9121 daemon is an unrelated
	// lifecycle concern this reconcile does not touch.
	serenaReport, serr := api.ReconcileSerenaClientsToRouter(ctx, api.SerenaReconcileOpts{
		Port:        port,
		BackupKeepN: keepN,
	})
	if serr != nil {
		if errors.Is(serr, api.ErrSerenaReconcileRouteNotLive) {
			return fmt.Errorf("reconcile-mcp-front: refusing to write any client config: %w", serr)
		}
		return fmt.Errorf("reconcile-mcp-front: serena client reconcile: %w", serr)
	}

	// Persist the serena report BEFORE the LSP reconcile runs, so a crash or
	// a subsequent LSP-side failure still leaves a rollback-capable record of
	// the serena writes that already landed on disk.
	reportPath, perr := mcpFrontReconcileSerenaReportPathFn()
	if perr != nil {
		return fmt.Errorf("reconcile-mcp-front: %w (serena client configs were already rewritten; re-run `mcphub install --reconcile-mcp-front --rollback` will not find a report to restore from until this is fixed)", perr)
	}
	if werr := api.WriteStateFileAtomic(reportPath, serenaReport); werr != nil {
		return fmt.Errorf("reconcile-mcp-front: persist serena reconcile report to %s: %w (serena client configs were already rewritten; rollback needs this file)", reportPath, werr)
	}

	lspReport, lerr := a.EnsureLSPRouterClientEntries(api.LSPClientRouterOpts{
		GUIPort:     port,
		BackupKeepN: keepN,
	})
	if lerr != nil {
		// The serena rewrite already committed (each client individually
		// backed up) — an LSP-side failure does NOT roll back serena. Report
		// both surfaces so the operator can see exactly what landed before
		// deciding whether to retry or roll back.
		fmt.Fprintf(cmd.OutOrStdout(), "serena: %d applied, %d failed (port %d)\n", len(serenaReport.Applied), len(serenaReport.Failed), port)
		return fmt.Errorf("reconcile-mcp-front: lsp client router reconcile: %w", lerr)
	}

	_ = api.LogHubMcpEvent("info", "mcp-front-reconciled", map[string]any{
		"action":         "reconcile",
		"port":           port,
		"serena_applied": len(serenaReport.Applied),
		"serena_failed":  len(serenaReport.Failed),
		"lsp_applied":    len(lspReport.Applied),
		"lsp_removed":    len(lspReport.Removed),
		"lsp_failed":     len(lspReport.Failed),
	})

	fmt.Fprintf(cmd.OutOrStdout(),
		"mcp-front reconcile: port=%d serena(applied=%d failed=%d) lsp(applied=%d removed=%d failed=%d)\n",
		port, len(serenaReport.Applied), len(serenaReport.Failed), len(lspReport.Applied), len(lspReport.Removed), len(lspReport.Failed))
	if len(serenaReport.Failed) > 0 || len(lspReport.Failed) > 0 {
		return fmt.Errorf("reconcile-mcp-front: %d serena + %d lsp per-client failure(s); re-run to retry the failed clients (successful ones are unaffected)",
			len(serenaReport.Failed), len(lspReport.Failed))
	}
	return nil
}

func runRollbackMCPFront(cmd *cobra.Command, a *api.API) error {
	reportPath, perr := mcpFrontReconcileSerenaReportPathFn()
	if perr != nil {
		return fmt.Errorf("reconcile-mcp-front --rollback: %w", perr)
	}
	raw, rerr := api.ReadStateFileInodeAnchored(reportPath)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return fmt.Errorf("reconcile-mcp-front --rollback: no prior reconcile report at %s; nothing to roll back (run `mcphub install --reconcile-mcp-front` first)", reportPath)
		}
		return fmt.Errorf("reconcile-mcp-front --rollback: read %s: %w", reportPath, rerr)
	}
	var serenaReport api.MigrateReport
	if uerr := json.Unmarshal(raw, &serenaReport); uerr != nil {
		return fmt.Errorf("reconcile-mcp-front --rollback: parse %s: %w", reportPath, uerr)
	}

	if rerr := api.RestoreSerenaReconcileApplied(&serenaReport, nil); rerr != nil {
		return fmt.Errorf("reconcile-mcp-front --rollback: restore serena clients: %w", rerr)
	}

	// LSP rollback is self-contained (api.RollbackLSPRouterClientEntries
	// reconstructs from the workspace registry + each client's own backup
	// history — it does not need the persisted serena report). It DOES need
	// to know which port to recognize as "owned by this reconcile" so it can
	// identify and remove those entries.
	//
	// ASSUMPTION (UNVERIFIED): re-resolving the CURRENT mcp_front.port
	// setting is correct only if the operator has not changed the setting
	// between the forward run and this rollback. This command does not
	// persist the forward run's port separately from the serena report's
	// already-written URLs (which do encode it, but LSPClientRouterOpts.GUIPort
	// is consulted for ownership recognition, not re-derived from the serena
	// report). If a mismatch ever proves to matter in practice, the fix is to
	// persist the forward-run port alongside the serena report and read it
	// back here instead of re-resolving the live setting.
	port, err := a.ResolveMCPFrontPort()
	if err != nil {
		return fmt.Errorf("reconcile-mcp-front --rollback: resolve mcp_front.port: %w", err)
	}
	lspReport, lerr := a.RollbackLSPRouterClientEntries(api.LSPClientRouterOpts{
		GUIPort:     port,
		BackupKeepN: effectiveBackupKeepN(),
	})
	if lerr != nil {
		return fmt.Errorf("reconcile-mcp-front --rollback: lsp rollback: %w", lerr)
	}

	if rmErr := os.Remove(reportPath); rmErr != nil && !os.IsNotExist(rmErr) {
		fmt.Fprintf(cmd.OutOrStdout(), "reconcile-mcp-front --rollback: warning: could not remove persisted report %s: %v\n", reportPath, rmErr)
	}

	_ = api.LogHubMcpEvent("info", "mcp-front-reconciled", map[string]any{
		"action":          "rollback",
		"port":            port,
		"serena_restored": len(serenaReport.Applied),
		"lsp_restored":    len(lspReport.Restored),
		"lsp_removed":     len(lspReport.Removed),
		"lsp_failed":      len(lspReport.Failed),
	})

	fmt.Fprintf(cmd.OutOrStdout(), "mcp-front rollback: serena(restored=%d) lsp(restored=%d removed=%d failed=%d)\n",
		len(serenaReport.Applied), len(lspReport.Restored), len(lspReport.Removed), len(lspReport.Failed))
	if len(lspReport.Failed) > 0 {
		return fmt.Errorf("reconcile-mcp-front --rollback: %d lsp per-client failure(s)", len(lspReport.Failed))
	}
	return nil
}
