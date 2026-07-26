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

// mcpFrontReconcileLegacyReportVersion is the pre-pinning artifact schema.
//
// It is REFUSED for forward migration (readMCPFrontReconcileReport) — the
// properties version 2 added cannot be reconstructed after the fact, so a v1
// record must never be merged into and re-blessed as trustworthy. It IS
// admitted for ROLLBACK, but only after its completeness is VERIFIED rather
// than assumed: see admitLegacyMCPFrontRecordForRollback for the distinction
// and for why a blanket rollback refusal was the worse failure.
const mcpFrontReconcileLegacyReportVersion = 1

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
//   - Port: the mcp_front.port the LATEST forward generation actually wrote
//     into the live client configs. Recording it (rather than re-resolving the
//     live setting at rollback time) closes the footgun of an operator
//     changing the setting between the forward run and the rollback — "the hub
//     remembers, not the human". A Port of 0 means "not recorded" and the
//     rollback falls back to re-resolving the live setting.
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
//
//   - Applied: the fingerprint of each rewritten serena entry AS THIS COMMAND
//     LEFT IT, so the rollback can tell "still exactly what I wrote" from "the
//     operator has edited this since". See mcpFrontSerenaApplied.
//
// # First-generation vs latest-generation fields
//
// The record spans an arbitrary number of forward generations, and its fields
// split cleanly by which one they belong to (codex bot PR #588):
//
//   - WHAT WAS HERE BEFORE — the serena Applied rows, their Pins, and the LSP
//     pre-state — is FIRST-generation and immutable. That is the state
//     `--rollback` must restore, and a later generation's view of it is
//     already the post-cutover state.
//
//   - WHAT THIS COMMAND LEFT BEHIND — Port and Applied — tracks the LATEST
//     generation. Both describe the live host as the most recent forward run
//     left it, and a re-run at a different mcp_front.port genuinely moves both.
//     Pinning them to the first generation is what made the record lie: the
//     rollback compared the live entries against a port the forward pass had
//     since stopped writing, so its ABSENT-row removal guard
//     (entryMatchesLSPRouter against LSPRouterURL(port, language)) no longer
//     matched, and the entries the cutover created were left behind pointing
//     at a retired port instead of being removed.
type mcpFrontReconcileReport struct {
	Version          int                           `json:"version"`
	Port             int                           `json:"port"`
	SnapshotComplete bool                          `json:"snapshot_complete"`
	Serena           *api.MigrateReport            `json:"serena"`
	LSP              *[]api.LSPRouterEntrySnapshot `json:"lsp"`
	Pins             []mcpFrontSerenaPin           `json:"pins"`
	Applied          []mcpFrontSerenaApplied       `json:"applied"`
}

// mcpFrontSerenaPin is one client's retention-immune pre-reconcile backup.
type mcpFrontSerenaPin struct {
	Client string `json:"client"`
	Origin string `json:"origin"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// mcpFrontSerenaApplied is the post-write fingerprint of one client's serena
// entry: what the most recent forward generation left in the live config.
//
// It is the serena counterpart of a guard the LSP side already had. A recovery
// record can stay active indefinitely — the forward run and its `--rollback`
// are separate operator actions — so by rollback time the operator may have
// edited that entry themselves. RestoreSerenaReconcileApplied overwrites it
// unconditionally, silently discarding their work. Comparing this fingerprint
// against the live entry before any restoration write turns that silent
// clobber into an explicit refusal.
//
// SHA256 empty means the entry was ABSENT when the forward run finished (a
// real state, distinct from "not recorded" — see Recorded).
//
// Recorded=false means the forward run could not compute a fingerprint for
// this client at all (its adapter or config was unreachable at commit time).
// Such a row carries no baseline, so the rollback cannot judge divergence for
// it and says so rather than either refusing or silently overwriting.
type mcpFrontSerenaApplied struct {
	Client   string `json:"client"`
	SHA256   string `json:"sha256"`
	Recorded bool   `json:"recorded"`
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

// mcpFrontReconcileLockLeaf is the basename (sans the `.lock` /
// `.owner.json` suffixes api.AcquireSupervisorLock appends) of the flock that
// serializes ONE recovery transaction — a whole forward run, or a whole
// rollback — against any other invocation of this command.
//
// WHY (codex bot PR #588). The transaction is read-modify-write across several
// files: read the prior generation, capture the LSP pre-state, journal each
// serena backup, rewrite client configs, and finally retire the record by
// rename. Two concurrent invocations interleave on all of it. Both read the
// same prior generation and each writes a record missing the other's rows; a
// forward run racing a rollback can journal rows into a record the rollback is
// about to retire, so those clients end up rewritten with their recovery rows
// in a retired file. Every one of those outcomes leaves a client mutated with
// no usable way back — the exact property the whole record lifecycle exists to
// guarantee.
//
// It reuses api.AcquireSupervisorLock, the repo's existing state-dir flock
// primitive (internal/api/supervisor_lock.go, the same one the serena
// migrate/cutover interlock borrows), rather than inventing a second
// mechanism. It is a NEW basename, deliberately not supervisor.lock and not
// migration.lock: this command must not contend with the supervisor's own
// singleton (api.AssertMCPFrontPortSupervisorOwned PROBES supervisor.lock to
// prove a supervisor is live — holding it ourselves would make our own gate
// report no supervisor) and must not block, or be blocked by, a serena migrate
// holding migration.lock.
//
// NO DEADLOCK IS POSSIBLE: AcquireSupervisorLock is a TRY-lock. A second
// invocation fails immediately with the holder's PID rather than waiting, so a
// concurrent reader is never parked behind this one — and this command spawns
// no child process and is bounded by mcpFrontReconcileTimeout, so the hold is
// short and self-limiting. It is also the only lock this command takes, so it
// participates in no lock-ordering cycle.
const mcpFrontReconcileLockLeaf = "mcp-front-reconcile"

// runReconcileMCPFront dispatches to the forward reconcile or its rollback,
// with the WHOLE transaction serialized under one flock (see
// mcpFrontReconcileLockLeaf).
func runReconcileMCPFront(cmd *cobra.Command, rollback bool) error {
	a := api.NewAPI()
	reportPath, perr := mcpFrontReconcileSerenaReportPathFn()
	if perr != nil {
		return fmt.Errorf("reconcile-mcp-front: %w", perr)
	}
	// The lock lives beside the record it protects, so it follows the report
	// path seam automatically and a test is isolated without extra wiring.
	lock, lerr := api.AcquireSupervisorLock(filepath.Join(filepath.Dir(reportPath), mcpFrontReconcileLockLeaf))
	if lerr != nil {
		return fmt.Errorf("reconcile-mcp-front: another `mcphub install --reconcile-mcp-front` invocation is already running (%v); refusing to run two recovery transactions at once — they would interleave on the same pre-reconcile record and could leave a client rewritten with no usable rollback row. Wait for it to finish, then re-run", lerr)
	}
	defer lock.Release()

	if rollback {
		return runRollbackMCPFront(cmd, a, reportPath)
	}
	return runForwardReconcileMCPFront(cmd, a, reportPath)
}

// preflightMCPFrontReconcile establishes EVERY precondition the forward pass
// is about to depend on, before it mutates anything.
//
// WHY THIS IS ONE STEP (codex bot PR #588). Two of that round's findings were
// the same structural mistake seen from different sides: the command's only
// liveness proof lived INSIDE its first mutating call. ReconcileSerenaClients-
// ToRouter probes /serena/mcp as part of doing the serena rewrite, and that
// probe was treated as the whole command's gate — so the LSP leg, which points
// clients at a completely different route (/lsp/<language>/mcp) served by an
// independently-wired router, ran with no proof of its own. A gate that is a
// side effect of one surface's mutation can only ever cover that surface, and
// silently under-covers every surface added later.
//
// So the proof is hoisted out of the mutation and made explicit and total: one
// side-effect-free block that establishes ownership and liveness for EVERY
// endpoint this run will write into a client config. Adding a third surface
// later means adding its probe here, which is a visible, reviewable omission
// rather than an invisible one.
//
// Ordering is cheapest-and-most-decisive first: the two ownership assertions
// are local file/OS reads, the two liveness probes are loopback round-trips.
// Every failure returns before any write, so a refused run leaves ZERO side
// effects (in particular, no recovery record is created).
func preflightMCPFrontReconcile(ctx context.Context, a *api.API, port int) error {
	if oerr := assertMCPFrontPortNotForeignOwned(a, port); oerr != nil {
		return oerr
	}
	if serr := api.AssertMCPFrontPortSupervisorOwned(port); serr != nil {
		return serr
	}
	// Serena route. ReconcileSerenaClientsToRouter re-proves this internally
	// before its own writes; that inner proof is deliberately left in place as
	// defense-in-depth, but it is no longer this command's gate.
	if perr := api.AssertSerenaRouterRouteLive(ctx, port); perr != nil {
		return perr
	}
	// LSP route. Never probed before this round — see AssertLSPRouterRouteLive.
	if perr := api.AssertLSPRouterRouteLive(ctx, port); perr != nil {
		return perr
	}
	return nil
}

func runForwardReconcileMCPFront(cmd *cobra.Command, a *api.API, reportPath string) error {
	port, err := a.ResolveMCPFrontPort()
	if err != nil {
		return fmt.Errorf("reconcile-mcp-front: resolve mcp_front.port: %w", err)
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), mcpFrontReconcileTimeout)
	defer cancel()

	if gerr := preflightMCPFrontReconcile(ctx, a, port); gerr != nil {
		return fmt.Errorf("reconcile-mcp-front: refusing to write any client config: %w", gerr)
	}

	keepN := effectiveBackupKeepN()
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
	// on every addition. Nothing has been written yet, and nothing will be
	// unless a client is actually about to be mutated.
	journal := newMCPFrontReconcileJournal(reportPath, prior, port, lspSnapshot)

	// Serena first. Its own port-liveness proof (Port>0 path,
	// serena_client_reconcile.go's resolveSerenaReconcilePort) still runs
	// inside this call as defense-in-depth, but it is NO LONGER this command's
	// gate — preflightMCPFrontReconcile above established both routes before
	// anything here could write (codex bot PR #588; a gate embedded in one
	// surface's mutation cannot cover the other surface). RemoveLegacy is
	// deliberately false: the pre-dynamic-pool legacy port-9121 daemon is an
	// unrelated lifecycle concern this reconcile does not touch.
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

func runRollbackMCPFront(cmd *cobra.Command, a *api.API, reportPath string) error {
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
	// Operator-edit gate. Also BEFORE the first write, for the same reason as
	// the two gates above: a refusal must leave the host exactly as it was.
	if verr := verifyMCPFrontSerenaNotEdited(cmd, &persisted, reportPath); verr != nil {
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
	// A version-1 record is ACCEPTED FOR ROLLBACK when — and only when — the
	// completeness version 2 records as a marker can be VERIFIED directly from
	// its contents. See admitLegacyMCPFrontRecordForRollback.
	if persisted.Version == mcpFrontReconcileLegacyReportVersion {
		return admitLegacyMCPFrontRecordForRollback(persisted, reportPath)
	}
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

// admitLegacyMCPFrontRecordForRollback decides whether a version-1 record may
// drive a ROLLBACK.
//
// The v1 refusal exists because v2 added three properties that make a record's
// trustworthiness checkable, and a v1 record cannot be UPGRADED in place — its
// serena rows point at rolling backups that retention may already have pruned,
// and it cannot distinguish "no LSP rows" from "no LSP section". Refusing to
// TRUST those properties by assumption is right, and forward migration stays
// refused for exactly that reason (readMCPFrontReconcileReport).
//
// But a blanket refusal of ROLLBACK was too strong, and its failure mode is
// the worst one available: a host cut over by the immediately preceding build
// can hold a v1 record with an explicit LSP section and every serena backup
// still on disk — demonstrably sufficient recovery inputs — and the new binary
// refuses both the rollback and the forward retry, stranding the operator cut
// over with no way back. That is strictly worse than the defect the refusal
// protects against.
//
// So the distinction is TRUST vs VERIFY. Every property v2 asserts by marker,
// this admits only after checking it directly on the record in hand:
//
//   - the LSP section must be EXPLICITLY PRESENT (a non-nil pointer). This is
//     the one v1 could not express, so it is checked rather than assumed; a v1
//     record whose `lsp` key is absent is still refused, because "no LSP rows"
//     and "LSP section lost" are genuinely indistinguishable there.
//   - the serena section must be present.
//   - every referenced backup must exist and be readable RIGHT NOW. v2 gets
//     this from pinning + checksums; v1 gets it from a direct probe of the
//     rolling backups it names, immediately before any write. Retention may
//     have pruned them — if it has, this refuses, which is the honest answer.
//
// A v1 record has no pins, so verifyMCPFrontSerenaPins would refuse it on the
// missing-pin branch; that check is therefore satisfied HERE for v1 by the
// direct backup probe below, and skipped there (it keys off Version).
func admitLegacyMCPFrontRecordForRollback(persisted *mcpFrontReconcileReport, reportPath string) error {
	if persisted.Serena == nil {
		return fmt.Errorf("%s is a version-%d record with no serena section; a rollback cannot be driven from it (restore those clients by hand or move the file aside)", reportPath, mcpFrontReconcileLegacyReportVersion)
	}
	if persisted.LSP == nil {
		return fmt.Errorf("%s is a version-%d record whose lsp section is ABSENT (not merely empty). Version %d records mark completeness explicitly; a version-%d record cannot, so this one is accepted for rollback only when it carries the section outright — and it does not. Consuming it would restore serena and delete the only evidence the LSP router entries are still on the front port. Restore those entries by hand (each client's `mcp-language-server-<language>` entry), then move %s aside",
			reportPath, mcpFrontReconcileLegacyReportVersion, mcpFrontReconcileReportVersion, mcpFrontReconcileLegacyReportVersion, reportPath)
	}
	var missing []string
	for _, row := range persisted.Serena.Applied {
		if row.BackupPath == "" {
			continue // dry-run row: nothing to restore, nothing to verify
		}
		if _, err := os.Stat(row.BackupPath); err != nil {
			missing = append(missing, fmt.Sprintf("%s (%s)", row.Client, row.BackupPath))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s is a version-%d record whose recorded serena backup(s) are no longer on disk: %s. Version-%d records pin a retention-immune copy; version-%d ones name the rolling `.bak-mcp-local-hub-<ts>` file, which backup retention prunes. Those clients cannot be restored, so no client is touched — restore them by hand and move %s aside",
			reportPath, mcpFrontReconcileLegacyReportVersion, strings.Join(missing, ", "), mcpFrontReconcileReportVersion, mcpFrontReconcileLegacyReportVersion, reportPath)
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
	if persisted.Version == mcpFrontReconcileLegacyReportVersion {
		// A version-1 record predates pinning entirely, so "no pin for this
		// row" is its normal shape, not evidence of a careless producer. Its
		// equivalent proof — every named rolling backup still present and
		// readable — was performed by admitLegacyMCPFrontRecordForRollback
		// before this ran.
		return nil
	}
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
		//
		// The PIN, however, was already published (write-ahead ordering puts it
		// on disk before the record that references it). Nothing now references
		// it and nothing ever will: this generation is refusing, and the pin
		// directory is only ever cleaned wholesale by a successful rollback. A
		// pin is a byte copy of a WHOLE client config, so it can carry header
		// tokens and stdio `env` secrets, and the retry this failure invites
		// would publish another one each time. Reclaim it here, and report a
		// reclaim failure rather than swallowing it — an operator told only
		// about the record write would never learn a secret-bearing file was
		// left behind (codex bot PR #588).
		werr = fmt.Errorf("persist to %s: %w", j.reportPath, werr)
		if cerr := reclaimOrphanedPin(pin, j.reportPath); cerr != nil {
			return fmt.Errorf("%w; additionally, the now-unreferenced pinned copy of this client's config could NOT be removed: %v. It holds a verbatim copy of %s (which may contain tokens or env secrets) and nothing references it — delete it by hand", werr, cerr, pin.Origin)
		}
		return werr
	}
	j.record = next
	return nil
}

// reclaimOrphanedPin removes a pin that was published but never referenced by
// a durable record, plus the per-client directory it lived in when that leaves
// the directory empty.
//
// Deliberately conservative about directories: only the client-scoped leaf and
// the pin root are considered, and only when they are already empty, so a
// concurrent generation's pins (or an unrelated file an operator dropped in)
// are never removed as collateral.
func reclaimOrphanedPin(pin mcpFrontSerenaPin, reportPath string) error {
	if pin.Path == "" {
		return nil
	}
	if err := os.Remove(pin.Path); err != nil && !os.IsNotExist(err) {
		return err
	}
	// The hardened atomic writer leaves a `<path>.lock` flock sidecar next to
	// every file it publishes. Removing only the payload would still leave the
	// per-client directory non-empty (so it could never be reclaimed) and leave
	// a growing trail of lock leaves across retries.
	if err := os.Remove(pin.Path + ".lock"); err != nil && !os.IsNotExist(err) {
		return err
	}
	// os.Remove on a directory succeeds only when it is empty, which is exactly
	// the condition we want — a non-empty directory fails and is left alone.
	pinRoot := mcpFrontReconcilePinDir(reportPath)
	clientDir := filepath.Dir(pin.Path)
	if clientDir != pinRoot && strings.HasPrefix(clientDir, pinRoot) {
		_ = os.Remove(clientDir)
	}
	_ = os.Remove(pinRoot)
	return nil
}

// commit re-writes the accumulated record and re-asserts the invariant that
// every mutated client was journaled ahead of its mutation.
//
// The re-write is what puts the LSP pre-state on disk on a host with zero
// serena clients (where recordSerenaBackup never fires) — the LSP reconcile
// runs next, and its recovery record must be durable first.
// It also re-derives the post-write serena fingerprints (Applied) for EVERY
// recorded client, not just the ones this generation rewrote. A re-run at a
// changed mcp_front.port rewrites clients an earlier generation had already
// migrated, so a fingerprint set that only covered this run's new clients
// would leave the earlier ones baselined against a URL the hub itself has
// since replaced — and the rollback would then refuse them as
// operator-modified. Both this and the Port field track the latest generation
// for the same reason: they describe the live host, not its history.
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
	j.record.Applied = j.captureAppliedFingerprints()
	return api.WriteStateFileAtomic(j.reportPath, j.record)
}

// captureAppliedFingerprints records what this command left in each recorded
// client's serena entry, so a later rollback can detect operator edits.
//
// A client whose fingerprint cannot be computed right now is recorded with
// Recorded=false rather than omitted: "we have no baseline for this client" is
// a fact the rollback must be able to state, and an omitted row is
// indistinguishable from a record written by a producer that had no
// fingerprints at all.
func (j *mcpFrontReconcileJournal) captureAppliedFingerprints() []mcpFrontSerenaApplied {
	if j.record.Serena == nil {
		return nil
	}
	out := make([]mcpFrontSerenaApplied, 0, len(j.record.Serena.Applied))
	for _, row := range j.record.Serena.Applied {
		if row.BackupPath == "" {
			continue // dry-run row: nothing was written, nothing to baseline
		}
		sum, err := api.SerenaClientEntryFingerprint(row.Client, nil)
		if err != nil {
			out = append(out, mcpFrontSerenaApplied{Client: row.Client})
			continue
		}
		out = append(out, mcpFrontSerenaApplied{Client: row.Client, SHA256: sum, Recorded: true})
	}
	return out
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

// verifyMCPFrontSerenaNotEdited refuses to overwrite a serena entry the
// OPERATOR has changed since the forward run wrote it.
//
// RestoreSerenaReconcileApplied writes the recorded backup over the live entry
// unconditionally. Forward and rollback are separate operator actions with no
// bound between them, so "the entry is still what we wrote" is an assumption,
// not a fact — and when it is wrong, the rollback silently destroys whatever
// the operator put there (a repointed remote serena, added headers, a
// deliberate disable). The LSP half of this same rollback already refuses that
// case; this is the missing serena half (codex bot PR #588).
//
// Refusal is whole-run and pre-write, matching verifyMCPFrontSerenaPins: a
// rollback that restored the untouched clients and then stopped at the edited
// one would leave the host split across two generations, which is worse than
// either endpoint and much harder to reason about.
//
// A row with no recorded baseline (Recorded=false, or a record written before
// the field existed) cannot be judged. Those are REPORTED to the operator and
// allowed through rather than blocking a legitimate rollback on missing
// evidence — stated plainly, because the operator is the one who knows whether
// they edited it.
func verifyMCPFrontSerenaNotEdited(cmd *cobra.Command, persisted *mcpFrontReconcileReport, reportPath string) error {
	baseline := map[string]mcpFrontSerenaApplied{}
	for _, row := range persisted.Applied {
		baseline[row.Client] = row
	}
	var diverged []string
	var unjudgeable []string
	for _, row := range persisted.Serena.Applied {
		if row.BackupPath == "" {
			continue // dry-run row: nothing was written, so nothing was replaced
		}
		recorded, ok := baseline[row.Client]
		if !ok || !recorded.Recorded {
			unjudgeable = append(unjudgeable, row.Client)
			continue
		}
		live, err := api.SerenaClientEntryFingerprint(row.Client, nil)
		if err != nil {
			// Unreadable right now. RestoreSerenaReconcileApplied will surface
			// its own error for this client; do not convert a transient read
			// failure into an operator-edit accusation.
			unjudgeable = append(unjudgeable, row.Client)
			continue
		}
		if live != recorded.SHA256 {
			diverged = append(diverged, row.Client)
		}
	}
	if len(unjudgeable) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(),
			"reconcile-mcp-front --rollback: note: no post-reconcile baseline for %s; cannot tell whether they were edited since the forward run, and they will be restored from the recorded backup\n",
			strings.Join(unjudgeable, ", "))
	}
	if len(diverged) > 0 {
		return fmt.Errorf("the serena entry for %s no longer matches what the forward reconcile wrote — it has been edited since (by hand, or by another tool). Restoring would overwrite that change with the pre-reconcile backup and discard it. No client was touched. Either put those entries back the way this command left them and re-run `--rollback`, or, if the current content is what you want to keep, move %s aside and restore the other clients by hand",
			strings.Join(diverged, ", "), reportPath)
	}
	return nil
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
		// NOTE (codex bot PR #588): out.Port is deliberately NOT inherited from
		// prior. Port describes what the LATEST forward generation wrote into
		// the live client configs, and a re-run at a changed mcp_front.port
		// really did move them — keeping the first generation's value left the
		// record naming a port the command had stopped writing, so the rollback
		// judged the live entries against the wrong URL. The pre-state fields
		// below (rows, pins, LSP snapshot) keep the opposite, first-generation
		// polarity for the opposite reason. Applied is likewise re-derived per
		// generation, by the journal's commit, and so is not merged here.
		//
		// A prior.Port that is still meaningful is not lost: this function is
		// only ever called with THIS run's port, and a run that reaches it has
		// already proven that port live and supervisor-owned.
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
