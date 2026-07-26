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
// # Fail-closed gate, in three parts, ALL before any client write
//
//  1. Port ownership by a KNOWN OTHER owner (codex bot PR #588 P2). The
//     mcp_front.port setting accepts any value in [1024,65535], including the
//     GUI's own port. The liveness probe below only proves "SOMETHING at this
//     port speaks the /serena/mcp protocol shape" — and the GUI speaks it too,
//     so a port set to gui_server.port would satisfy the probe while the route
//     child could never bind it, leaving every reconciled client on a
//     GUI-dependent endpoint that dies with the GUI (exactly the guarantee this
//     cutover exists to provide). assertMCPFrontPortNotForeignOwned refuses
//     that and every other known-owner collision BEFORE the probe runs.
//
//  2. Port ownership by the SUPERVISOR (adversarial review finding 3).
//     Refusing every known other owner is not the same as proving the right
//     owner. A bare `mcphub route --port N` typed in a terminal is the real
//     route server: it collides with nothing and answers the probe perfectly,
//     yet nothing restarts it, so the cutover would rewrite every client onto
//     a port that goes dark when that shell closes. api.AssertMCPFrontPort-
//     SupervisorOwned requires the canonical built-in route descriptor at this
//     exact port AND an OS-level binding from the port to the supervisor's own
//     recorded child PID. Protocol shape is not ownership; only ownership
//     carries the survival guarantee this whole increment is for.
//
//  3. Liveness. ReconcileSerenaClientsToRouter's own port-liveness proof
//     (resolveSerenaReconcilePort -> defaultRouterReadinessPing, which
//     performs BOTH the HEAD/405 same-router-shape check AND a real MCP
//     `initialize` round-trip) runs UNCONDITIONALLY before any client write,
//     regardless of how many serena clients are actually present — so calling
//     the serena reconcile FIRST doubles as the whole command's shared
//     liveness gate. If it returns ErrSerenaReconcileRouteNotLive (a whole-run
//     blocker, distinct from a per-client Failed row), this file aborts BEFORE
//     touching LSP at all — nothing is written on either surface when the
//     route isn't proven live.
//
// # The recovery record's lifecycle (adversarial review findings 1, 2, 5, 7)
//
// Those four findings are one question wearing four hats: WHEN IS THE RECOVERY
// RECORD TRUSTWORTHY? A record that is written after the mutation it protects,
// that points at files a retention sweep may already have deleted, that can be
// consumed with a whole section missing, or that stays active after a rollback
// believes it consumed it, is not a recovery record — it is a note that
// usually happens to be right. So the record is treated as ONE artifact with
// one lifecycle, and all four are properties of that lifecycle rather than
// four independent patches:
//
//   - WRITE-AHEAD (finding 2). Nothing is mutated before its recovery row is
//     durable. The LSP pre-state is captured and persisted before the LSP
//     reconcile; each serena client's backup is journaled through
//     SerenaReconcileOpts.OnBackupCaptured — which fires after that client's
//     backup exists and before its config is touched — so a failed record
//     write PREVENTS the mutation instead of orphaning it.
//
//   - PINNED INPUTS (finding 1). The record's serena rows point at a byte-copy
//     of each first-generation backup, pinned under mcp-front-reconcile-pins/
//     and checksummed, not at the rolling `.bak-mcp-local-hub-<ts>` file. The
//     rolling backups are retention-managed (BackupKeep prunes to
//     backups.keep_n, and a single forward run takes TWO of them per client —
//     one in the serena leg, one in the LSP leg), so the file the record
//     depends on is otherwise deleted after a few re-runs of the retry this
//     command itself recommends. A record must not depend on state governed by
//     a policy that does not know the record exists.
//
//   - COMPLETENESS MARKER (finding 5). SnapshotComplete plus a POINTER-typed
//     LSP section make "this record has every section" checkable rather than
//     inferable. Go marshals a nil slice and an empty one to `null` and `[]`,
//     which the pre-fix reader could not tell apart — so a record with no LSP
//     section at all validated, restored serena, performed a zero-row LSP
//     restore, and was deleted while the LSP clients sat on the front port.
//     Every required section is now validated BEFORE any restoration write.
//
//   - ATOMIC RETIREMENT (finding 7). A consumed record leaves the active
//     namespace by rename, and a rename that cannot complete FAILS the
//     rollback. Warning about a failed delete and returning success left a
//     completed generation active, so the next forward run merged against a
//     stale baseline and ITS rollback restored the pre-PREVIOUS state.
//
// The unifying rule the four share: a generation is consumed only when its
// restoration is COMPLETE (no failed rows, no pending rows) and its record has
// atomically left the active namespace. Anything less keeps the record.
package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
// and the pinned serena backups too — see mcpFrontReconcileReport.)
const mcpFrontReconcileSerenaReportLeaf = "mcp-front-reconcile-serena-report.json"

// mcpFrontReconcilePinDirLeaf is the directory, sibling to the report, holding
// the pinned first-generation client-config backups the record's serena rows
// restore from. It deliberately lives OUTSIDE the client-config directory and
// carries none of the `.bak-mcp-local-hub-` naming, so no rolling-retention
// pass can see it (pruneOldTimestamped keys on that prefix next to the live
// config).
//
// It holds whole client-config copies, which can contain header tokens and
// stdio `env` secrets, so every pin is written through
// api.WriteStateFileBytesAtomic (handle-bound owner-only DACL) — the same
// posture as the adopt-provenance snapshots documented in CLAUDE.md, and for
// the same reason.
const mcpFrontReconcilePinDirLeaf = "mcp-front-reconcile-pins"

// mcpFrontReconcileReportVersion is the artifact schema version. A rollback
// REFUSES an artifact carrying any other value: the file's whole purpose is
// to record the pre-reconcile state, and a record this build cannot fully
// interpret must never be used to drive writes into client configs. Bump on
// any change to what the fields mean.
//
// Version 2 added the three fields that make the record's trustworthiness
// CHECKABLE rather than assumed — SnapshotComplete, the pointer-typed LSP
// section, and Pins. A version-1 artifact is refused (not upgraded in place)
// because the properties it is missing cannot be reconstructed after the fact:
// its serena rows point at rolling backups that retention may already have
// deleted, and nothing in it distinguishes "no LSP rows" from "no LSP
// section".
const mcpFrontReconcileReportVersion = 2

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
// overwritten, and the whole artifact is retired only by a COMPLETE rollback.
//
// Fields:
//
//   - Port: the FIRST generation's mcp_front.port. Recording it (rather than
//     re-resolving the live setting at rollback time) closes the footgun of an
//     operator changing the setting between the forward run and the rollback —
//     "the hub remembers, not the human". A Port of 0 means "not recorded" and
//     the rollback falls back to re-resolving the live setting.
//
//   - SnapshotComplete: the completeness marker. True only on a record whose
//     writer had BOTH recovery sections in hand. The rollback refuses a record
//     without it before performing any restoration write, so a record that
//     covers one surface can never be consumed (and deleted) while the other
//     surface is still on the front port.
//
//   - Serena: the merged Applied rows, each carrying the per-client backup
//     path api.RestoreSerenaReconcileApplied restores from — which for a
//     version-2 record is the PINNED copy, not the rolling one. Failed rows
//     are deliberately NOT persisted: a client whose rewrite failed was never
//     mutated, so it has nothing to restore.
//
//   - LSP: the per-(client, language) pre-state of each canonical
//     `mcp-language-server-<language>` entry, captured before the forward LSP
//     write. See api.LSPRouterEntrySnapshot for why this exists rather than
//     reusing api.RollbackLSPRouterClientEntries. It is a POINTER so a missing
//     section (`null`) is distinguishable from a present-but-empty one (`[]`);
//     conflating the two is what let an LSP-less record be consumed.
//
//   - Pins: provenance and integrity for each pinned backup. Origin is the
//     rolling backup the pin copies (diagnostics only — it may well be gone by
//     rollback time, which is the entire point), SHA256 is verified before the
//     pin is allowed to drive a client write.
type mcpFrontReconcileReport struct {
	Version          int                           `json:"version"`
	Port             int                           `json:"port"`
	SnapshotComplete bool                          `json:"snapshot_complete"`
	Serena           *api.MigrateReport            `json:"serena"`
	LSP              *[]api.LSPRouterEntrySnapshot `json:"lsp"`
	Pins             []mcpFrontSerenaPin           `json:"pins"`
}

// mcpFrontSerenaPin is one client's retention-immune pre-reconcile backup.
type mcpFrontSerenaPin struct {
	Client string `json:"client"`
	Origin string `json:"origin"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
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
//
// The pin directory is derived from the resolved report path, so it follows
// the seam automatically and a test never scatters pinned client configs
// outside its own temp dir.
var mcpFrontReconcileSerenaReportPathFn = mcpFrontReconcileSerenaReportPath

func mcpFrontReconcileSerenaReportPath() (string, error) {
	dir, err := api.DaemonStateDir()
	if err != nil {
		return "", fmt.Errorf("resolve state dir: %w", err)
	}
	return filepath.Join(dir, mcpFrontReconcileSerenaReportLeaf), nil
}

func mcpFrontReconcilePinDir(reportPath string) string {
	return filepath.Join(filepath.Dir(reportPath), mcpFrontReconcilePinDirLeaf)
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
	if serr := api.AssertMCPFrontPortSupervisorOwned(port); serr != nil {
		return fmt.Errorf("reconcile-mcp-front: refusing to write any client config: %w", serr)
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

	// The journal holds the accumulating record in memory and makes it durable
	// on every addition. It is NOT written yet: the liveness gate inside the
	// serena reconcile has not run, and a refused run must leave no artifact
	// behind at all.
	journal := newMCPFrontReconcileJournal(reportPath, prior, port, lspSnapshot)

	// Serena FIRST — its own port-liveness proof (Port>0 path,
	// serena_client_reconcile.go's resolveSerenaReconcilePort) is the shared
	// fail-closed gate for this whole command. RemoveLegacy is deliberately
	// false: the pre-dynamic-pool legacy port-9121 daemon is an unrelated
	// lifecycle concern this reconcile does not touch.
	//
	// OnBackupCaptured is the write-ahead hand-off: it fires once per client,
	// after that client's backup exists and before its config is mutated, and
	// a failure there aborts THAT CLIENT'S rewrite rather than leaving it
	// mutated with no durable way back.
	serenaReport, serr := api.ReconcileSerenaClientsToRouter(ctx, api.SerenaReconcileOpts{
		Port:             port,
		BackupKeepN:      keepN,
		OnBackupCaptured: journal.recordSerenaBackup,
	})
	if serr != nil {
		if errors.Is(serr, api.ErrSerenaReconcileRouteNotLive) {
			return fmt.Errorf("reconcile-mcp-front: refusing to write any client config: %w", serr)
		}
		return fmt.Errorf("reconcile-mcp-front: serena client reconcile: %w", serr)
	}

	// Make the record durable before the LSP reconcile runs. For a host with
	// at least one serena client this is already true (the journal wrote it),
	// and this call is a no-op re-write; for a host with none it is the write
	// that puts the LSP pre-state on disk ahead of the LSP mutation. It also
	// re-asserts the every-applied-row-was-journaled invariant.
	if werr := journal.commit(serenaReport); werr != nil {
		return fmt.Errorf("reconcile-mcp-front: persist reconcile report to %s: %w (serena client configs were already rewritten; rollback needs this file)", reportPath, werr)
	}

	lspReport, lerr := a.EnsureLSPRouterClientEntries(api.LSPClientRouterOpts{
		GUIPort:     port,
		BackupKeepN: keepN,
	})
	if lerr != nil {
		// The serena rewrite already committed (each client individually
		// backed up and journaled) — an LSP-side failure does NOT roll back
		// serena. Report both surfaces so the operator can see exactly what
		// landed before deciding whether to retry or roll back.
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

	// EVERY required section is validated, and every pinned input is proven
	// present and intact, BEFORE the first restoration write. A rollback that
	// starts writing and only then discovers a missing section or a vanished
	// backup leaves the host half-restored across two surfaces, which is a
	// worse state than either endpoint.
	if verr := validateMCPFrontReconcileReport(&persisted, reportPath); verr != nil {
		return fmt.Errorf("reconcile-mcp-front --rollback: %w", verr)
	}
	if verr := verifyMCPFrontSerenaPins(&persisted, reportPath); verr != nil {
		return fmt.Errorf("reconcile-mcp-front --rollback: %w", verr)
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
	lspReport, lerr := a.RestoreLSPRouterClientEntriesSnapshot(*persisted.LSP, api.LSPClientRouterOpts{
		GUIPort:     port,
		BackupKeepN: effectiveBackupKeepN(),
	})
	if lerr != nil {
		return fmt.Errorf("reconcile-mcp-front --rollback: lsp rollback: %w", lerr)
	}

	// RestoreLSPRouterClientEntriesSnapshot reports a URL rewrite as an "add"
	// op (Applied); Restored is the backup-file restore kind applyLSPRouterOps
	// shares with the demotion path. Both are "put back" from this command's
	// point of view, so the operator-facing count is their sum.
	lspRestored := len(lspReport.Applied) + len(lspReport.Restored)

	// CONSUMPTION GATE. The record is retired only when the restoration it
	// describes is COMPLETE. A pending row (a recorded client whose adapter or
	// config file is not reachable right now) is the case that motivated this:
	// the pre-fix restore silently skipped such a client, the caller saw a
	// clean report, deleted the record, and that client's rollback row was
	// gone for good the moment it came back.
	if len(lspReport.Pending) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "mcp-front rollback: serena(restored=%d) lsp(restored=%d removed=%d pending=%d)\n",
			len(serenaReport.Applied), lspRestored, len(lspReport.Removed), len(lspReport.Pending))
		return fmt.Errorf("reconcile-mcp-front --rollback: %d recorded lsp entr(ies) could not be restored because their client is not reachable right now (%s); the reconcile report %s has been KEPT so re-running `--rollback` after the client is back finishes the job. To abandon those rows deliberately, move that file aside by hand",
			len(lspReport.Pending), describeLSPChanges(lspReport.Pending), reportPath)
	}

	if len(lspReport.Failed) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "mcp-front rollback: serena(restored=%d) lsp(restored=%d removed=%d failed=%d)\n",
			len(serenaReport.Applied), lspRestored, len(lspReport.Removed), len(lspReport.Failed))
		return fmt.Errorf("reconcile-mcp-front --rollback: %d lsp per-client failure(s); the reconcile report %s has been KEPT so the rollback can be re-run once the cause is cleared", len(lspReport.Failed), reportPath)
	}

	// ATOMIC RETIREMENT. Move the record out of the active namespace before
	// reporting success, and FAIL if that transition cannot complete. The
	// pre-fix code warned on a failed delete and returned success, which left
	// a fully-consumed generation active: the next forward run then merged
	// against that stale baseline and its own rollback restored the
	// pre-PREVIOUS state instead of the new generation's pre-state.
	retiredPath, terr := mcpFrontRetireReportFn(reportPath)
	if terr != nil {
		return fmt.Errorf("reconcile-mcp-front --rollback: every client was restored, but the reconcile report %s could NOT be retired: %w. It is still ACTIVE, so a later `--reconcile-mcp-front` would merge against this already-consumed generation and its rollback would restore the wrong state. Remove or move that file aside before running the forward command again", reportPath, terr)
	}
	// Best-effort cleanup, deliberately AFTER the rename: the active namespace
	// is already clear, so neither of these can leave a stale record behind.
	// Leftovers here are bounded, owner-only, and operator-removable.
	if rmErr := os.Remove(retiredPath); rmErr != nil && !os.IsNotExist(rmErr) {
		fmt.Fprintf(cmd.OutOrStdout(), "reconcile-mcp-front --rollback: note: retired report kept at %s (%v)\n", retiredPath, rmErr)
	}
	if rmErr := os.RemoveAll(mcpFrontReconcilePinDir(reportPath)); rmErr != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "reconcile-mcp-front --rollback: note: pinned backups kept at %s (%v)\n", mcpFrontReconcilePinDir(reportPath), rmErr)
	}

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
	return nil
}

// describeLSPChanges renders a short, operator-readable "client/language" list.
func describeLSPChanges(changes []api.LSPClientRouterChange) string {
	parts := make([]string, 0, len(changes))
	for _, c := range changes {
		parts = append(parts, c.Client+"/"+c.Language)
	}
	return strings.Join(parts, ", ")
}

// validateMCPFrontReconcileReport is the rollback's complete-record gate. It
// runs BEFORE any restoration write and refuses anything it cannot fully
// interpret.
//
// Each check answers a question the pre-fix reader simply did not ask, and the
// answers are independent — which is why they are enumerated rather than
// collapsed into one "looks fine" predicate.
func validateMCPFrontReconcileReport(persisted *mcpFrontReconcileReport, reportPath string) error {
	if persisted.Version != mcpFrontReconcileReportVersion {
		return fmt.Errorf("%s carries artifact version %d, this build understands version %d — refusing to drive client-config writes from a pre-reconcile record it cannot fully interpret (the file was written by an incompatible build; restore those clients by hand or move the file aside)", reportPath, persisted.Version, mcpFrontReconcileReportVersion)
	}
	if !persisted.SnapshotComplete {
		return fmt.Errorf("%s is not marked snapshot-complete: the run that wrote it did not finish capturing every recovery section, so consuming it would restore only part of the host and then delete the record of the rest. Re-run `mcphub install --reconcile-mcp-front` (which merges into this record without overwriting it) so the missing section is captured, or move the file aside and restore by hand", reportPath)
	}
	if persisted.Serena == nil {
		return fmt.Errorf("%s carries no serena section (corrupt or unrecognized artifact); refusing to restore half the host and delete the record of the other half", reportPath)
	}
	if persisted.LSP == nil {
		return fmt.Errorf("%s carries no lsp section (the key is absent, which is NOT the same as an empty one); a record with no LSP pre-state cannot put the LSP router entries back, and consuming it would delete the only evidence they are still on the front port. Refusing", reportPath)
	}
	return nil
}

// verifyMCPFrontSerenaPins proves every serena row's restore input is present
// and byte-intact BEFORE the first client write.
//
// This is the check that makes finding 1 observable instead of silent. The
// rolling `.bak-mcp-local-hub-<ts>` backups a forward run takes are pruned to
// backups.keep_n by the very next runs, so a record that pointed at them was
// one retry away from naming a deleted file — and the failure surfaced as a
// mid-rollback error after serena had already been partly restored. Pinning
// removes the cause; verifying the pin up front removes the class.
func verifyMCPFrontSerenaPins(persisted *mcpFrontReconcileReport, reportPath string) error {
	pins := map[string]mcpFrontSerenaPin{}
	for _, pin := range persisted.Pins {
		pins[pin.Client] = pin
	}
	for _, row := range persisted.Serena.Applied {
		if row.BackupPath == "" {
			// No snapshot to restore from (a dry-run row). Nothing to verify;
			// RestoreSerenaReconcileApplied skips it for the same reason.
			continue
		}
		pin, ok := pins[row.Client]
		if !ok {
			return fmt.Errorf("%s records a restorable serena row for %q but no pinned backup for it; this record was written by a producer that did not pin its inputs, so the file it points at is subject to rolling backup retention and may already be gone. Refusing to start a restore that could fail halfway", reportPath, row.Client)
		}
		if pin.Path != row.BackupPath {
			return fmt.Errorf("%s records serena backup %q for %q but its pin points at %q; the record is inconsistent and must not drive client-config writes", reportPath, row.BackupPath, row.Client, pin.Path)
		}
		sum, err := fileSHA256(pin.Path)
		if err != nil {
			return fmt.Errorf("%s: pinned pre-reconcile backup for %q is unreadable at %s: %w; the client cannot be restored from it, so no client is touched", reportPath, row.Client, pin.Path, err)
		}
		if sum != pin.SHA256 {
			return fmt.Errorf("%s: pinned pre-reconcile backup for %q at %s has changed since it was pinned (sha256 %s, recorded %s); refusing to write a client config from a backup whose contents are no longer the ones this record vouched for", reportPath, row.Client, pin.Path, sum, pin.SHA256)
		}
	}
	return nil
}

// mcpFrontRetireReportFn is the retirement test seam (same package-level style
// as mcpFrontReconcileSerenaReportPathFn).
//
// It exists because the behavior finding 7 is about is the CALLER's reaction
// to a retirement that cannot complete — the pre-fix code printed a warning
// and returned success — and the only portable way to produce a failing rename
// is to inject one. The retirement mechanism itself is covered directly by
// TestMCPFrontReview_RetirementClearsTheActiveNamespace.
var mcpFrontRetireReportFn = retireMCPFrontReconcileReport

// retireMCPFrontReconcileReport moves a fully-consumed record out of the
// active namespace by rename, returning the retired path.
//
// Rename (not unlink) is the operation that matters: it is the single
// filesystem step that makes the active name stop resolving, so either the
// generation is consumed or it is not — there is no state in which the
// rollback reported success while the record it consumed is still the one the
// next forward run will merge into.
//
// The timestamp carries nanoseconds AND a disambiguating counter because
// Windows rename fails when the destination exists, and a caller must never be
// told "retirement failed" over a name collision.
func retireMCPFrontReconcileReport(reportPath string) (string, error) {
	base := reportPath + ".retired-" + time.Now().UTC().Format("20060102-150405.000000000")
	for i := 0; i < 1000; i++ {
		candidate := base
		if i > 0 {
			candidate = fmt.Sprintf("%s-%03d", base, i)
		}
		if _, err := os.Stat(candidate); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("probe retirement path %s: %w", candidate, err)
		}
		if err := os.Rename(reportPath, candidate); err != nil {
			return "", fmt.Errorf("rename %s -> %s: %w", reportPath, candidate, err)
		}
		return candidate, nil
	}
	return "", fmt.Errorf("could not find a free retirement path next to %s after 1000 attempts", reportPath)
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
		return nil, fmt.Errorf("artifact version %d, this build understands version %d (a version-1 record cannot be upgraded in place: its serena rows point at rolling backups that retention may already have pruned)", out.Version, mcpFrontReconcileReportVersion)
	}
	return &out, nil
}

// mcpFrontReconcileJournal owns the recovery record for ONE forward run.
//
// It exists so the record can be made durable INCREMENTALLY, ahead of each
// mutation, instead of once at the end. Its state is the accumulating record
// plus the merge bookkeeping; every mutation of that state goes back through
// mergeMCPFrontReconcileReport, so the "never overwrite a row an earlier
// generation captured" invariant has exactly one owner.
type mcpFrontReconcileJournal struct {
	reportPath string
	port       int
	record     mcpFrontReconcileReport
}

// newMCPFrontReconcileJournal seeds the record with the prior generation's
// rows plus THIS run's LSP pre-state. Nothing is written yet — the liveness
// gate has not run, and a refused run must leave no artifact at all.
func newMCPFrontReconcileJournal(reportPath string, prior *mcpFrontReconcileReport, port int, lsp []api.LSPRouterEntrySnapshot) *mcpFrontReconcileJournal {
	return &mcpFrontReconcileJournal{
		reportPath: reportPath,
		port:       port,
		record:     mergeMCPFrontReconcileReport(prior, port, nil, lsp, nil),
	}
}

// recordSerenaBackup is the SerenaReconcileOpts.OnBackupCaptured hook: it runs
// after this client's pre-rewrite backup exists and before its config is
// mutated. Returning an error prevents that mutation.
//
// A client an earlier generation already recorded is deliberately a no-op: the
// pinned backup that generation took is the true pre-state, and the backup
// this run just captured is the ALREADY-REWRITTEN config (or, for a client
// that generation failed on, an identical copy of a state that was never
// mutated). Either way the recorded row is the one to keep.
func (j *mcpFrontReconcileJournal) recordSerenaBackup(client, backupPath string) error {
	if j.serenaRecorded(client) {
		return nil
	}
	pin, err := j.pinBackup(client, backupPath)
	if err != nil {
		return err
	}
	row := &api.MigrateReport{Applied: []api.AppliedMigration{{
		Server:     "serena",
		Client:     client,
		URL:        fmt.Sprintf("http://127.0.0.1:%d%s", j.port, api.SerenaRouterURLPath),
		BackupPath: pin.Path,
	}}}
	next := mergeMCPFrontReconcileReport(&j.record, j.port, row, nil, []mcpFrontSerenaPin{pin})
	if werr := api.WriteStateFileAtomic(j.reportPath, next); werr != nil {
		// The mutation has NOT happened yet, so refusing here costs the
		// operator one retryable client and costs the record nothing.
		return fmt.Errorf("persist to %s: %w", j.reportPath, werr)
	}
	j.record = next
	return nil
}

// commit re-writes the accumulated record and re-asserts the invariant that
// every mutated client was journaled ahead of its mutation.
//
// The re-write is what puts the LSP pre-state on disk on a host with zero
// serena clients (where recordSerenaBackup never fires) — the LSP reconcile
// runs next, and its recovery record must be durable first.
func (j *mcpFrontReconcileJournal) commit(serena *api.MigrateReport) error {
	if serena != nil {
		for _, row := range serena.Applied {
			if row.BackupPath == "" {
				continue // dry-run row: nothing was backed up or written
			}
			if !j.serenaRecorded(row.Client) {
				return fmt.Errorf("internal invariant: client %q was rewritten without a write-ahead recovery row; the OnBackupCaptured journal hook did not run for it", row.Client)
			}
		}
	}
	return api.WriteStateFileAtomic(j.reportPath, j.record)
}

func (j *mcpFrontReconcileJournal) serenaRecorded(client string) bool {
	if j.record.Serena == nil {
		return false
	}
	for _, row := range j.record.Serena.Applied {
		if row.Client == client {
			return true
		}
	}
	return false
}

// pinBackup copies a rolling client-config backup into the record-owned pin
// directory and returns its provenance + checksum.
//
// The copy is what takes the record's input out of rolling-retention's reach.
// It is written through the hardened owner-only state-file pipeline because a
// client config can carry header tokens and stdio `env` secrets — the same
// reason the adopt-provenance snapshots use it (CLAUDE.md, "Adopt
// provenance").
func (j *mcpFrontReconcileJournal) pinBackup(client, backupPath string) (mcpFrontSerenaPin, error) {
	if backupPath == "" {
		return mcpFrontSerenaPin{}, errors.New("no backup path was captured for this client")
	}
	segment, err := safeClientPathSegment(client)
	if err != nil {
		return mcpFrontSerenaPin{}, err
	}
	raw, rerr := os.ReadFile(backupPath)
	if rerr != nil {
		return mcpFrontSerenaPin{}, fmt.Errorf("read pre-reconcile backup %s: %w", backupPath, rerr)
	}
	pinPath := filepath.Join(mcpFrontReconcilePinDir(j.reportPath), segment, filepath.Base(backupPath))
	if werr := api.WriteStateFileBytesAtomic(pinPath, raw); werr != nil {
		return mcpFrontSerenaPin{}, fmt.Errorf("pin pre-reconcile backup to %s: %w", pinPath, werr)
	}
	sum := sha256.Sum256(raw)
	return mcpFrontSerenaPin{
		Client: client,
		Origin: backupPath,
		Path:   pinPath,
		SHA256: hex.EncodeToString(sum[:]),
	}, nil
}

// safeClientPathSegment refuses a client name that could escape or collide
// inside the pin directory. Client names come from the compile-time
// clients.AllClients() key set, so this is defense-in-depth against a future
// caller (or a hand-edited record) rather than a live threat — but a pin
// directory is written with operator-secret contents, so "cannot be talked
// into writing elsewhere" is worth the six lines.
func safeClientPathSegment(client string) (string, error) {
	trimmed := strings.TrimSpace(client)
	if trimmed == "" || trimmed == "." || trimmed == ".." {
		return "", fmt.Errorf("client name %q cannot be used as a pin directory name", client)
	}
	if strings.ContainsAny(trimmed, `/\:`) || strings.Contains(trimmed, "..") {
		return "", fmt.Errorf("client name %q contains path separators; refusing to pin outside the pin directory", client)
	}
	return trimmed, nil
}

func fileSHA256(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
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
// Serena rows key on Client; LSP rows key on (Client, Language); pins key on
// Client alongside the serena row they belong to. Port keeps the first
// generation's value once recorded. SnapshotComplete is asserted here because
// this function is the only producer of a record, and it always writes both
// sections: LSP is materialized to a non-nil slice so the artifact carries an
// explicit empty section rather than a missing one.
func mergeMCPFrontReconcileReport(
	prior *mcpFrontReconcileReport,
	port int,
	serena *api.MigrateReport,
	lsp []api.LSPRouterEntrySnapshot,
	pins []mcpFrontSerenaPin,
) mcpFrontReconcileReport {
	out := mcpFrontReconcileReport{Version: mcpFrontReconcileReportVersion, Port: port, SnapshotComplete: true}
	mergedSerena := &api.MigrateReport{}
	mergedLSP := []api.LSPRouterEntrySnapshot{}
	seenClient := map[string]bool{}
	seenLSP := map[string]bool{}
	seenPin := map[string]bool{}

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
		if prior.LSP != nil {
			for _, row := range *prior.LSP {
				mergedLSP = append(mergedLSP, row)
				seenLSP[lspSnapshotKey(row)] = true
			}
		}
		for _, pin := range prior.Pins {
			out.Pins = append(out.Pins, pin)
			seenPin[pin.Client] = true
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
		mergedLSP = append(mergedLSP, row)
	}
	for _, pin := range pins {
		if seenPin[pin.Client] {
			continue
		}
		seenPin[pin.Client] = true
		out.Pins = append(out.Pins, pin)
	}

	out.Serena = mergedSerena
	out.LSP = &mergedLSP
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
// This gate looks for a NEGATIVE (no known other owner), which is why each
// probe is best-effort on its own read errors — an unreadable
// gui-preferences.yaml / pidport / supervisor-intent is not evidence of a
// collision and must not block a legitimate reconcile — while a POSITIVE
// collision is always fatal. Its mirror image,
// api.AssertMCPFrontPortSupervisorOwned, looks for a POSITIVE (the supervisor
// owns this port) and therefore fails closed on the same read errors. Both
// run; they are not redundant, and they cannot be collapsed because their
// error polarity is opposite.
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
