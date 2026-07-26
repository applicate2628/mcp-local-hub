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
// api.RestoreLSPRouterClientEntriesSnapshot (--rollback) — no reconcile/
// backup/rollback logic is reimplemented here.
//
// Fail-closed gate, in two parts, BOTH before any client write:
//
//  1. Port ownership (codex bot PR #588 P2). The mcp_front.port setting
//     accepts any value in [1024,65535], including the GUI's own port. The
//     liveness probe below only proves "SOMETHING at this port speaks the
//     /serena/mcp protocol shape" — and the GUI speaks it too, so a port set
//     to gui_server.port would satisfy the probe while the route child could
//     never bind it, leaving every reconciled client on a GUI-dependent
//     endpoint that dies with the GUI (exactly the guarantee this cutover
//     exists to provide). assertMCPFrontPortNotForeignOwned refuses that and
//     every other known-owner collision BEFORE the probe runs.
//
//  2. Liveness. ReconcileSerenaClientsToRouter's own port-liveness proof
//     (resolveSerenaReconcilePort -> defaultRouterReadinessPing, which
//     performs BOTH the HEAD/405 same-router-shape check AND a real MCP
//     `initialize` round-trip) runs UNCONDITIONALLY before any client write,
//     regardless of how many serena clients are actually present — so calling
//     the serena reconcile FIRST doubles as the whole command's shared
//     liveness gate. If it returns ErrSerenaReconcileRouteNotLive (a whole-run
//     blocker, distinct from a per-client Failed row), this file aborts BEFORE
//     touching LSP at all — nothing is written on either surface when the
//     route isn't proven live.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/gui"

	"github.com/spf13/cobra"
)

// mcpFrontReconcileSerenaReportLeaf is the state-file basename the forward
// reconcile persists its pre-reconcile record under, so a LATER, separate
// `--rollback` invocation can restore each rewritten client from the exact
// state the forward run captured. Lives alongside the other install-side
// state-bookkeeping files under DaemonStateDir(), written through the same
// hardened WriteStateFileAtomic pipeline every other state file uses.
//
// (The basename still says "serena-report" for byte-compatibility with the
// path this branch already wrote; the artifact now carries the LSP pre-state
// too — see mcpFrontReconcileReport.)
const mcpFrontReconcileSerenaReportLeaf = "mcp-front-reconcile-serena-report.json"

// mcpFrontReconcileReportVersion is the artifact schema version. A rollback
// REFUSES an artifact carrying any other value: the file's whole purpose is
// to record the pre-reconcile state, and a record this build cannot fully
// interpret must never be used to drive writes into client configs. Bump on
// any change to what the fields mean.
const mcpFrontReconcileReportVersion = 1

// mcpFrontReconcileReport is the persisted pre-reconcile record.
//
// It is a CUMULATIVE record of the FIRST generation's pre-reconcile state,
// not a snapshot of the most recent run (codex bot PR #588 P1). The forward
// command is explicitly re-runnable — its own per-client-failure message tells
// the operator to re-run to retry the failed clients — and the pre-fix code
// re-captured and re-wrote this artifact wholesale on every run. On the second
// run the already-migrated clients were backed up AGAIN, so their recorded
// backups then contained the FRONT-port URL rather than the original one, and
// `--rollback` restored the front URL: the original state was destroyed by the
// very retry the command recommends. mergeMCPFrontReconcileReport therefore
// only ever ADDS rows; a row an earlier generation recorded is never
// overwritten, and the whole artifact is deleted only by a successful
// rollback.
//
// Fields:
//
//   - Port: the FIRST generation's mcp_front.port. Recording it (rather than
//     re-resolving the live setting at rollback time) closes the footgun of an
//     operator changing the setting between the forward run and the rollback —
//     "the hub remembers, not the human". A Port of 0 means "not recorded" and
//     the rollback falls back to re-resolving the live setting.
//
//   - Serena: the merged Applied rows, each carrying the per-client backup
//     path api.RestoreSerenaReconcileApplied restores from. Failed rows are
//     deliberately NOT persisted: a client whose rewrite failed was never
//     mutated, so it has nothing to restore.
//
//   - LSP: the per-(client, language) pre-state of each canonical
//     `mcp-language-server-<language>` entry, captured before the forward LSP
//     write. See api.LSPRouterEntrySnapshot for why this exists rather than
//     reusing api.RollbackLSPRouterClientEntries.
type mcpFrontReconcileReport struct {
	Version int                          `json:"version"`
	Port    int                          `json:"port"`
	Serena  *api.MigrateReport           `json:"serena"`
	LSP     []api.LSPRouterEntrySnapshot `json:"lsp"`
}

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
	if oerr := assertMCPFrontPortNotForeignOwned(a, port); oerr != nil {
		return fmt.Errorf("reconcile-mcp-front: refusing to write any client config: %w", oerr)
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), mcpFrontReconcileTimeout)
	defer cancel()

	keepN := effectiveBackupKeepN()

	reportPath, perr := mcpFrontReconcileSerenaReportPathFn()
	if perr != nil {
		return fmt.Errorf("reconcile-mcp-front: %w", perr)
	}
	// Read any prior, not-yet-rolled-back generation BEFORE anything is
	// written. A prior artifact that exists but cannot be read/parsed is a
	// HARD refusal, not a "start fresh": overwriting it would destroy the
	// only record of the pre-reconcile state, which is precisely the failure
	// mode this merge exists to prevent.
	prior, priorErr := readMCPFrontReconcileReport(reportPath)
	if priorErr != nil {
		return fmt.Errorf("reconcile-mcp-front: a prior reconcile report exists at %s but could not be read: %w; refusing to run (overwriting it would destroy the pre-reconcile state `--rollback` restores from) — roll back or move that file aside first", reportPath, priorErr)
	}

	// Capture the LSP pre-state BEFORE any write, so an unreadable client
	// config aborts the run with ZERO side effects rather than after the
	// serena rewrite has already committed. Read-only.
	lspSnapshot, snapErr := a.SnapshotLSPRouterClientEntries(api.LSPClientRouterOpts{})
	if snapErr != nil {
		return fmt.Errorf("reconcile-mcp-front: capture lsp pre-state: %w (nothing was written)", snapErr)
	}

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

	// Persist the merged record BEFORE the LSP reconcile runs, so a crash or
	// a subsequent LSP-side failure still leaves a rollback-capable record of
	// the serena writes that already landed on disk AND of the LSP pre-state
	// the next step is about to overwrite.
	persisted := mergeMCPFrontReconcileReport(prior, port, serenaReport, lspSnapshot)
	if werr := api.WriteStateFileAtomic(reportPath, persisted); werr != nil {
		return fmt.Errorf("reconcile-mcp-front: persist reconcile report to %s: %w (serena client configs were already rewritten; rollback needs this file)", reportPath, werr)
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
		return fmt.Errorf("reconcile-mcp-front: %d serena + %d lsp per-client failure(s); re-run to retry the failed clients (successful ones are unaffected, and the pre-reconcile record `--rollback` restores from is preserved across re-runs)",
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
	var persisted mcpFrontReconcileReport
	if uerr := json.Unmarshal(raw, &persisted); uerr != nil {
		return fmt.Errorf("reconcile-mcp-front --rollback: parse %s: %w", reportPath, uerr)
	}
	if persisted.Version != mcpFrontReconcileReportVersion {
		return fmt.Errorf("reconcile-mcp-front --rollback: %s carries artifact version %d, this build understands version %d — refusing to drive client-config writes from a pre-reconcile record it cannot fully interpret (the file was written by an incompatible build; restore those clients by hand or move the file aside)", reportPath, persisted.Version, mcpFrontReconcileReportVersion)
	}
	if persisted.Serena == nil {
		return fmt.Errorf("reconcile-mcp-front --rollback: %s carries no serena report (corrupt or unrecognized artifact)", reportPath)
	}
	serenaReport := persisted.Serena

	if rerr := api.RestoreSerenaReconcileApplied(serenaReport, nil); rerr != nil {
		return fmt.Errorf("reconcile-mcp-front --rollback: restore serena clients: %w", rerr)
	}

	// LSP rollback drives each canonical `mcp-language-server-<language>`
	// entry back to the URL the forward run recorded for it (or removes it
	// when the forward run recorded it as absent).
	//
	// It deliberately does NOT call api.RollbackLSPRouterClientEntries: that
	// is the router-to-LEGACY demotion routine (reconstruct per-workspace
	// entries from the registry, then delete the canonical router entry), not
	// the inverse of this command's port rewrite. On an already-migrated host
	// the forward pass only changes the canonical entry's PORT, so its
	// inverse is "put the previous port back" — the demotion routine instead
	// removed the shared router entry outright, leaving the client with no
	// LSP route at all (codex bot PR #588 P1).
	//
	// The port passed here is ownership evidence for the pre-write guard, and
	// comes from the PERSISTED forward-run record rather than the live
	// mcp_front.port setting, so an operator changing that setting between the
	// forward run and this rollback cannot strand the entries the forward run
	// actually wrote. Port==0 means an artifact that predates the field — fall
	// back to the live setting rather than failing.
	port := persisted.Port
	if port <= 0 {
		var err error
		port, err = a.ResolveMCPFrontPort()
		if err != nil {
			return fmt.Errorf("reconcile-mcp-front --rollback: resolve mcp_front.port (no port recorded in %s): %w", reportPath, err)
		}
	}
	lspReport, lerr := a.RestoreLSPRouterClientEntriesSnapshot(persisted.LSP, api.LSPClientRouterOpts{
		GUIPort:     port,
		BackupKeepN: effectiveBackupKeepN(),
	})
	if lerr != nil {
		return fmt.Errorf("reconcile-mcp-front --rollback: lsp rollback: %w", lerr)
	}

	if rmErr := os.Remove(reportPath); rmErr != nil && !os.IsNotExist(rmErr) {
		fmt.Fprintf(cmd.OutOrStdout(), "reconcile-mcp-front --rollback: warning: could not remove persisted report %s: %v\n", reportPath, rmErr)
	}

	// RestoreLSPRouterClientEntriesSnapshot reports a URL rewrite as an "add"
	// op (Applied); Restored is the backup-file restore kind applyLSPRouterOps
	// shares with the demotion path. Both are "put back" from this command's
	// point of view, so the operator-facing count is their sum.
	lspRestored := len(lspReport.Applied) + len(lspReport.Restored)

	_ = api.LogHubMcpEvent("info", "mcp-front-reconciled", map[string]any{
		"action":          "rollback",
		"port":            port,
		"serena_restored": len(serenaReport.Applied),
		"lsp_restored":    lspRestored,
		"lsp_removed":     len(lspReport.Removed),
		"lsp_failed":      len(lspReport.Failed),
	})

	fmt.Fprintf(cmd.OutOrStdout(), "mcp-front rollback: serena(restored=%d) lsp(restored=%d removed=%d failed=%d)\n",
		len(serenaReport.Applied), lspRestored, len(lspReport.Removed), len(lspReport.Failed))
	if len(lspReport.Failed) > 0 {
		return fmt.Errorf("reconcile-mcp-front --rollback: %d lsp per-client failure(s)", len(lspReport.Failed))
	}
	return nil
}

// readMCPFrontReconcileReport loads a previously persisted record.
//
// (nil, nil) means "no prior generation" — the only absence this function
// treats as benign. Every other read/parse failure is returned as an error so
// the caller can refuse rather than silently start a fresh record on top of an
// artifact it could not read.
func readMCPFrontReconcileReport(path string) (*mcpFrontReconcileReport, error) {
	raw, err := api.ReadStateFileInodeAnchored(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out mcpFrontReconcileReport
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return nil, uerr
	}
	if out.Version != mcpFrontReconcileReportVersion {
		return nil, fmt.Errorf("artifact version %d, this build understands version %d", out.Version, mcpFrontReconcileReportVersion)
	}
	return &out, nil
}

// mergeMCPFrontReconcileReport folds THIS forward generation into the persisted
// record WITHOUT ever overwriting a row an earlier generation captured.
//
// The invariant: for each key, the record holds the state as it was BEFORE the
// FIRST generation touched it — because that is the state `--rollback` must
// restore. A re-run of the forward command (the documented retry for per-client
// failures) re-derives a "pre-state" that is really the POST-first-run state
// for every client the first run already migrated; adopting it would overwrite
// the original with the front-port value and make rollback a no-op.
//
// A client the first run FAILED on was never mutated, so the row this run
// contributes for it genuinely is its original pre-state — which is exactly
// why "add rows not yet recorded, never replace recorded ones" is the correct
// merge and not merely the conservative one.
//
// Serena rows key on Client; LSP rows key on (Client, Language). Port keeps
// the first generation's value once recorded.
func mergeMCPFrontReconcileReport(
	prior *mcpFrontReconcileReport,
	port int,
	serena *api.MigrateReport,
	lsp []api.LSPRouterEntrySnapshot,
) mcpFrontReconcileReport {
	out := mcpFrontReconcileReport{Version: mcpFrontReconcileReportVersion, Port: port}
	mergedSerena := &api.MigrateReport{}
	seenClient := map[string]bool{}
	seenLSP := map[string]bool{}

	if prior != nil {
		if prior.Port > 0 {
			out.Port = prior.Port
		}
		if prior.Serena != nil {
			for _, row := range prior.Serena.Applied {
				mergedSerena.Applied = append(mergedSerena.Applied, row)
				seenClient[row.Client] = true
			}
		}
		for _, row := range prior.LSP {
			out.LSP = append(out.LSP, row)
			seenLSP[lspSnapshotKey(row)] = true
		}
	}

	if serena != nil {
		for _, row := range serena.Applied {
			if seenClient[row.Client] {
				continue
			}
			seenClient[row.Client] = true
			mergedSerena.Applied = append(mergedSerena.Applied, row)
		}
	}
	for _, row := range lsp {
		if seenLSP[lspSnapshotKey(row)] {
			continue
		}
		seenLSP[lspSnapshotKey(row)] = true
		out.LSP = append(out.LSP, row)
	}

	out.Serena = mergedSerena
	return out
}

// lspSnapshotKey is the (client, language) identity of one LSP pre-state row.
// The NUL separator cannot appear in either component, so distinct pairs can
// never collide into one key.
func lspSnapshotKey(row api.LSPRouterEntrySnapshot) string {
	return row.Client + "\x00" + row.Language
}

// assertMCPFrontPortNotForeignOwned refuses a mcp_front.port that a DIFFERENT
// listener already owns (codex bot PR #588 P2).
//
// The liveness probe this command relies on proves only that something at the
// port speaks the /serena/mcp protocol shape — and the GUI serves that exact
// route, so a mcp_front.port set to the GUI's port passes the probe while the
// route child can never bind it. Every client would then be reconciled onto a
// GUI-dependent endpoint that dies with the GUI, silently defeating the whole
// point of moving the data plane off the GUI process. The probe cannot
// distinguish the two listeners; ownership can, so it is checked here first.
//
// Three known owners are consulted, each independently sufficient to refuse:
//
//  1. the gui_server.port SETTING — catches a collision even while the GUI is
//     down;
//  2. the GUI's LIVE bound port from the pidport file — catches a `--port 0`
//     or explicit-flag GUI launch whose actual port differs from the setting;
//  3. any OTHER supervisor-intent descriptor claiming the port — catches a
//     collision with a managed daemon (the built-in route row itself is
//     skipped: that IS the intended owner).
//
// Deliberately NOT implemented as "verify through supervisor state that the
// port is owned by the built-in route task": the command's own not-live error
// tells the operator to start EITHER `mcphub route` OR `mcphub supervise`, so
// requiring a running supervisor would break the documented standalone path.
// Each probe is best-effort on its own read errors — an unreadable
// gui-preferences.yaml / pidport / supervisor-intent must not block a
// legitimate reconcile — but a POSITIVE collision is always fatal.
func assertMCPFrontPortNotForeignOwned(a *api.API, port int) error {
	if raw, err := a.SettingsGet("gui_server.port"); err == nil {
		if n, cerr := strconv.Atoi(strings.TrimSpace(raw)); cerr == nil && n == port {
			return fmt.Errorf("mcp_front.port %d is the GUI's own gui_server.port; the `mcphub route` front daemon could never bind it, and the liveness probe would be satisfied by the GUI itself — leaving every reconciled client on an endpoint that dies with the GUI. Set mcp_front.port to a free port (`mcphub settings set %s <port>`; default %d) and re-run", port, api.MCPFrontPortSettingKey, api.DefaultMCPFrontPort)
		}
	}
	if pidportPath, err := gui.PidportPathNoCreate(); err == nil {
		if _, boundPort, rerr := gui.ReadPidport(pidportPath); rerr == nil && boundPort == port {
			return fmt.Errorf("mcp_front.port %d is the port the running GUI is bound to right now (per %s); the `mcphub route` front daemon could never bind it, and the liveness probe would be satisfied by the GUI itself. Set mcp_front.port to a free port (`mcphub settings set %s <port>`; default %d) and re-run", port, pidportPath, api.MCPFrontPortSettingKey, api.DefaultMCPFrontPort)
		}
	}
	if intentPath, err := api.DefaultSupervisorIntentPath(); err == nil {
		if intent, rerr := api.ReadSupervisorIntent(intentPath); rerr == nil && intent != nil {
			for _, d := range intent.Daemons {
				if d.Port != port {
					continue
				}
				if strings.TrimPrefix(d.TaskName, `\`) == strings.TrimPrefix(api.BuiltinRouteTaskName, `\`) {
					continue // the built-in route row IS the intended owner
				}
				return fmt.Errorf("mcp_front.port %d is already claimed by supervisor-managed daemon %q (server %q, daemon %q) in %s; the `mcphub route` front daemon could never bind it. Set mcp_front.port to a free port (`mcphub settings set %s <port>`; default %d) and re-run", port, d.TaskName, d.Server, d.Daemon, intentPath, api.MCPFrontPortSettingKey, api.DefaultMCPFrontPort)
			}
		}
	}
	return nil
}
