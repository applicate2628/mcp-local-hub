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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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
// Version 3 makes recovery authority per exact row. Versions 1 and 2 lack the
// immutable `(surface, client, language, entry_name)` row, active plan,
// pre/intended attempt, and effective applied receipt needed to distinguish a
// successful write from a retry that did not write. They are read-only refused
// with `legacy-ownership-unproven`; neither is upgraded in place.
const mcpFrontReconcileReportVersion = 3

const mcpFrontReconcileVersion2 = 2

// mcpFrontReconcileLegacyReportVersion is the pre-pinning artifact schema.
const mcpFrontReconcileLegacyReportVersion = 1

// mcpFrontReconcileReport is the persisted pre-reconcile record.
//
// Rows own the durable contract. Each row keeps its first baseline immutable,
// its current generation's exact prepared attempt, the latest proven applied
// receipt for that row, and its durable rollback disposition. ActivePlan is
// one complete captured population for Generation; it is published before the
// first write and cannot be replaced while any attempt is uncertain.
//
// No top-level compatibility projection is persisted: Rows are the only
// mutation and rollback authority. A rollback retires the artifact only after
// a durable re-read shows every row in a terminal disposition.
type mcpFrontReconcileReport struct {
	Version          int                             `json:"version"`
	SnapshotComplete bool                            `json:"snapshot_complete"`
	Generation       int                             `json:"generation,omitempty"`
	Rows             map[string]mcpFrontReconcileRow `json:"rows,omitempty"`
	ActivePlan       *mcpFrontReconcilePlan          `json:"active_plan,omitempty"`
}

type mcpFrontReconcileRow struct {
	Surface     string                       `json:"surface"`
	Client      string                       `json:"client"`
	Language    string                       `json:"language,omitempty"`
	EntryName   string                       `json:"entry_name"`
	Baseline    mcpFrontEntryState           `json:"baseline"`
	BaselineSet bool                         `json:"baseline_set"`
	Pin         *mcpFrontSerenaPin           `json:"pin,omitempty"`
	Attempt     *mcpFrontReconcileAttempt    `json:"attempt,omitempty"`
	Applied     *mcpFrontAppliedReceipt      `json:"applied,omitempty"`
	Disposition *mcpFrontRollbackDisposition `json:"disposition,omitempty"`
}

type mcpFrontReconcilePlan struct {
	Generation int                       `json:"generation"`
	Port       int                       `json:"port"`
	Rows       []string                  `json:"rows"`
	Operations []mcpFrontReconcilePlanOp `json:"operations"`
}

type mcpFrontReconcilePlanOp struct {
	RowKey        string             `json:"row_key"`
	Operation     string             `json:"operation"`
	PreState      mcpFrontEntryState `json:"pre_state"`
	IntendedState mcpFrontEntryState `json:"intended_state"`
}

type mcpFrontEntryState struct {
	Present     bool                        `json:"present"`
	Fingerprint string                      `json:"fingerprint,omitempty"`
	LSP         *api.LSPRouterEntrySnapshot `json:"lsp,omitempty"`
}

type mcpFrontReconcileAttempt struct {
	Generation    int                `json:"generation"`
	Operation     string             `json:"operation"`
	PreState      mcpFrontEntryState `json:"pre_state"`
	IntendedState mcpFrontEntryState `json:"intended_state"`
	State         string             `json:"state"`
}

type mcpFrontAppliedReceipt struct {
	Generation int                `json:"generation"`
	Port       int                `json:"port"`
	PostState  mcpFrontEntryState `json:"post_state"`
}

type mcpFrontRollbackDisposition struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

const (
	mcpFrontSurfaceSerena = "serena"
	mcpFrontSurfaceLSP    = "lsp"

	mcpFrontAttemptPrepared             = "prepared"
	mcpFrontAttemptConfirmedNoWrite     = "confirmed-no-write"
	mcpFrontAttemptApplied              = "applied"
	mcpFrontAttemptConflict             = "conflict"
	mcpFrontAttemptPreconditionConflict = "precondition-conflict"

	mcpFrontDispositionBaselineOnly = "baseline-only"
	mcpFrontDispositionRestored     = "restored"
	mcpFrontDispositionConflict     = "skipped-conflict"
	mcpFrontDispositionPending      = "pending"
	mcpFrontDispositionFailed       = "failed"
)

func mcpFrontReconcileRowKey(surface, client, language, entryName string) string {
	return surface + "\x00" + client + "\x00" + language + "\x00" + entryName
}

func mcpFrontRowKey(row mcpFrontReconcileRow) string {
	return mcpFrontReconcileRowKey(row.Surface, row.Client, row.Language, row.EntryName)
}

func snapshotEntryNameCLI(row api.LSPRouterEntrySnapshot) string {
	if row.EntryName != "" {
		return row.EntryName
	}
	return api.LSPRouterEntryName(row.Language)
}

func mcpFrontLSPState(row api.LSPRouterEntrySnapshot) mcpFrontEntryState {
	copy := row
	return mcpFrontEntryState{Present: row.Present, LSP: &copy}
}

func mcpFrontSerenaState(fingerprint string) mcpFrontEntryState {
	return mcpFrontEntryState{Present: fingerprint != "", Fingerprint: fingerprint}
}

func mcpFrontStateEqual(a, b mcpFrontEntryState) bool {
	return a.Present == b.Present &&
		a.Fingerprint == b.Fingerprint &&
		reflect.DeepEqual(a.LSP, b.LSP)
}

func effectiveMCPFrontAppliedReceipt(row mcpFrontReconcileRow) (*mcpFrontAppliedReceipt, bool) {
	if row.Attempt != nil {
		switch row.Attempt.State {
		case mcpFrontAttemptPrepared, mcpFrontAttemptConflict:
			return nil, true
		case mcpFrontAttemptPreconditionConflict:
			return nil, false
		case mcpFrontAttemptApplied:
			if row.Applied == nil {
				return nil, true
			}
		case mcpFrontAttemptConfirmedNoWrite:
			// The previous successful receipt remains the effective owner.
		default:
			return nil, true
		}
	}
	return row.Applied, false
}

// settleMCPFrontReconcileAttempts refuses every prepared attempt that survived
// process re-entry. Current value equality is not mutation causation: only the
// same-call ConditionalEntryMutator observation may create an applied receipt.
func settleMCPFrontReconcileAttempts(reportPath string, report *mcpFrontReconcileReport) error {
	if report == nil {
		return nil
	}
	changed := false
	var uncertain []string
	for key, row := range report.Rows {
		if row.Attempt == nil || row.Attempt.State != mcpFrontAttemptPrepared {
			continue
		}
		if row.Disposition == nil ||
			row.Disposition.State != mcpFrontDispositionPending ||
			row.Disposition.Reason != "pending-ownership-unknown" {
			row.Disposition = &mcpFrontRollbackDisposition{
				State: mcpFrontDispositionPending, Reason: "pending-ownership-unknown",
			}
			report.Rows[key] = row
			changed = true
		}
		uncertain = append(uncertain, key)
	}
	if changed {
		if err := api.WriteStateFileAtomic(reportPath, report); err != nil {
			return fmt.Errorf("settle recovery attempts: %w", err)
		}
	}
	if len(uncertain) > 0 {
		return fmt.Errorf("forward-previous-attempt-uncertain: %d prepared row(s) have no same-call mutation receipt", len(uncertain))
	}
	return nil
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
	if prior != nil {
		if settleErr := settleMCPFrontReconcileAttempts(reportPath, prior); settleErr != nil {
			return fmt.Errorf("reconcile-mcp-front: %w", settleErr)
		}
	}

	// Freeze the complete eligible LSP population and every exact pre-state
	// once. Apply uses only this plan and never re-enumerates clients.
	lspPlan, planErr := a.PlanLSPRouterClientEntries(api.LSPClientRouterOpts{
		GUIPort:     port,
		BackupKeepN: keepN,
	})
	if planErr != nil {
		return fmt.Errorf("reconcile-mcp-front: capture-incomplete: %w (nothing was written)", planErr)
	}
	if len(lspPlan.CaptureFailures) > 0 {
		return fmt.Errorf("reconcile-mcp-front: capture-incomplete: %d lsp row(s) could not be captured (nothing was written)", len(lspPlan.CaptureFailures))
	}
	journal, journalErr := newMCPFrontV3Journal(reportPath, prior, port, lspPlan)
	if journalErr != nil {
		return fmt.Errorf("reconcile-mcp-front: build recovery plan: %w", journalErr)
	}
	// The complete LSP plan is durable before the first Serena or LSP mutation.
	if persistErr := journal.persist(); persistErr != nil {
		return fmt.Errorf("reconcile-mcp-front: persist active recovery plan: %w (nothing was written)", persistErr)
	}

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
		Port:              port,
		BackupKeepN:       keepN,
		OnAttemptPrepared: journal.prepareSerenaAttempt,
		OnAttemptFinished: journal.finishSerenaAttempt,
	})
	if serr != nil {
		if errors.Is(serr, api.ErrSerenaReconcileRouteNotLive) {
			return fmt.Errorf("reconcile-mcp-front: refusing to write any client config: %w", serr)
		}
		return fmt.Errorf("reconcile-mcp-front: serena client reconcile: %w", serr)
	}

	lspReport, lerr := a.ApplyLSPRouterClientPlan(lspPlan, api.LSPRouterApplyCallbacks{
		OnPrepared:             journal.prepareLSPOperation,
		OnFinished:             journal.finishLSPOperation,
		OnPreconditionConflict: journal.finishLSPOperation,
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

func persistMCPFrontDisposition(
	reportPath string,
	report *mcpFrontReconcileReport,
	key, state, reason string,
) error {
	row, ok := report.Rows[key]
	if !ok {
		return fmt.Errorf("unknown recovery row %q", key)
	}
	row.Disposition = &mcpFrontRollbackDisposition{State: state, Reason: reason}
	report.Rows[key] = row
	if err := api.WriteStateFileAtomic(reportPath, report); err != nil {
		return fmt.Errorf("persist rollback disposition for %q: %w", key, err)
	}
	return nil
}

func terminalMCPFrontDisposition(disposition *mcpFrontRollbackDisposition) bool {
	if disposition == nil {
		return false
	}
	switch disposition.State {
	case mcpFrontDispositionBaselineOnly, mcpFrontDispositionRestored, mcpFrontDispositionConflict:
		return true
	default:
		return false
	}
}

func canRetireMCPFrontReconcileReport(report *mcpFrontReconcileReport) bool {
	if report == nil || len(report.Rows) == 0 {
		return false
	}
	for _, row := range report.Rows {
		if !terminalMCPFrontDisposition(row.Disposition) {
			return false
		}
	}
	return true
}

func describePendingMCPFrontRows(report *mcpFrontReconcileReport) string {
	if report == nil {
		return ""
	}
	var labels []string
	for _, row := range report.Rows {
		if terminalMCPFrontDisposition(row.Disposition) {
			continue
		}
		label := row.Client
		if row.Language != "" {
			label += "/" + row.Language
		} else {
			label += "/" + row.EntryName
		}
		labels = append(labels, label)
	}
	sort.Strings(labels)
	return strings.Join(labels, ", ")
}

func conflictMCPFrontRowLabels(report *mcpFrontReconcileReport) []string {
	if report == nil {
		return nil
	}
	var labels []string
	for _, row := range report.Rows {
		if row.Disposition == nil || row.Disposition.State != mcpFrontDispositionConflict {
			continue
		}
		labels = append(labels, strings.Join([]string{
			row.Surface, row.Client, row.Language, row.EntryName,
		}, "/"))
	}
	sort.Strings(labels)
	return labels
}

func sortedMCPFrontRowKeys(report *mcpFrontReconcileReport, surface string) []string {
	var keys []string
	for key, row := range report.Rows {
		if row.Surface == surface {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func runRollbackMCPFront(cmd *cobra.Command, a *api.API, reportPath string) error {
	raw, rerr := api.ReadStateFileInodeAnchored(reportPath)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return fmt.Errorf("reconcile-mcp-front --rollback: no prior reconcile report at %s; nothing to roll back (run `mcphub install --reconcile-mcp-front` first)", reportPath)
		}
		return fmt.Errorf("reconcile-mcp-front --rollback: read %s: %w", reportPath, rerr)
	}
	decoded, decodeErr := decodeMCPFrontReconcileReport(raw, reportPath)
	if decodeErr != nil {
		return fmt.Errorf("reconcile-mcp-front --rollback: parse %s: %w", reportPath, decodeErr)
	}
	persisted := *decoded

	// EVERY required section is validated, and every pinned input is proven
	// present and intact, BEFORE the first restoration write. A rollback that
	// starts writing and only then discovers a missing section or a vanished
	// backup leaves the host half-restored across two surfaces, which is a
	// worse state than either endpoint.
	if verr := validateMCPFrontReconcileReport(&persisted, reportPath); verr != nil {
		return fmt.Errorf("reconcile-mcp-front --rollback: %w", verr)
	}
	if settleErr := settleMCPFrontReconcileAttempts(reportPath, &persisted); settleErr != nil {
		return fmt.Errorf("reconcile-mcp-front --rollback: %w", settleErr)
	}
	if verr := verifyMCPFrontSerenaPins(&persisted, reportPath); verr != nil {
		return fmt.Errorf("reconcile-mcp-front --rollback: %w", verr)
	}
	serenaRestored := 0
	for _, key := range sortedMCPFrontRowKeys(&persisted, mcpFrontSurfaceSerena) {
		row := persisted.Rows[key]
		if terminalMCPFrontDisposition(row.Disposition) {
			continue
		}
		receipt, uncertain := effectiveMCPFrontAppliedReceipt(row)
		if uncertain {
			if err := persistMCPFrontDisposition(reportPath, &persisted, key, mcpFrontDispositionPending, "pending-ownership-unknown"); err != nil {
				return fmt.Errorf("reconcile-mcp-front --rollback: %w", err)
			}
			continue
		}
		if receipt == nil {
			if err := persistMCPFrontDisposition(reportPath, &persisted, key, mcpFrontDispositionBaselineOnly, "no-effective-applied-receipt"); err != nil {
				return fmt.Errorf("reconcile-mcp-front --rollback: %w", err)
			}
			continue
		}
		results, restoreErr := api.RestoreSerenaReconcileAppliedOwned(
			[]api.SerenaOwnedRestoreRequest{{
				Client:                     row.Client,
				BackupPath:                 row.Pin.Path,
				ExpectedAppliedFingerprint: receipt.PostState.Fingerprint,
				BaselinePresent:            row.Baseline.Present,
			}},
			nil,
		)
		if restoreErr != nil {
			if err := persistMCPFrontDisposition(reportPath, &persisted, key, mcpFrontDispositionPending, "rollback-write-failed"); err != nil {
				return fmt.Errorf("reconcile-mcp-front --rollback: %w", err)
			}
			continue
		}
		if len(results) != 1 {
			if err := persistMCPFrontDisposition(reportPath, &persisted, key, mcpFrontDispositionPending, "rollback-result-missing"); err != nil {
				return fmt.Errorf("reconcile-mcp-front --rollback: %w", err)
			}
			continue
		}
		switch results[0].Status {
		case api.SerenaOwnedRestoreRestored:
			live, liveErr := api.SerenaClientEntryFingerprint(row.Client, nil)
			if liveErr != nil || !mcpFrontStateEqual(mcpFrontSerenaState(live), row.Baseline) {
				if err := persistMCPFrontDisposition(reportPath, &persisted, key, mcpFrontDispositionPending, "rollback-verify-failed"); err != nil {
					return fmt.Errorf("reconcile-mcp-front --rollback: %w", err)
				}
				continue
			}
			if err := persistMCPFrontDisposition(reportPath, &persisted, key, mcpFrontDispositionRestored, "inverse-verified"); err != nil {
				return fmt.Errorf("reconcile-mcp-front --rollback: %w", err)
			}
			serenaRestored++
		case api.SerenaOwnedRestoreConflict:
			if err := persistMCPFrontDisposition(reportPath, &persisted, key, mcpFrontDispositionConflict, "rollback-cas-conflict"); err != nil {
				return fmt.Errorf("reconcile-mcp-front --rollback: %w", err)
			}
		default:
			if err := persistMCPFrontDisposition(reportPath, &persisted, key, mcpFrontDispositionPending, "rollback-write-failed"); err != nil {
				return fmt.Errorf("reconcile-mcp-front --rollback: %w", err)
			}
		}
	}

	var recoveryRows []api.LSPRouterRecoveryRow
	for _, key := range sortedMCPFrontRowKeys(&persisted, mcpFrontSurfaceLSP) {
		row := persisted.Rows[key]
		receipt, uncertain := effectiveMCPFrontAppliedReceipt(row)
		recovery := api.LSPRouterRecoveryRow{Baseline: *row.Baseline.LSP, Uncertain: uncertain}
		if row.Disposition != nil {
			recovery.Disposition = row.Disposition.State
			recovery.DispositionReason = row.Disposition.Reason
		}
		if receipt != nil && receipt.PostState.LSP != nil {
			applied := *receipt.PostState.LSP
			recovery.Applied = &applied
		}
		recoveryRows = append(recoveryRows, recovery)
	}
	lspReport, _, lerr := a.RestoreLSPRouterRecoveryRows(
		recoveryRows,
		api.LSPClientRouterOpts{BackupKeepN: effectiveBackupKeepN()},
		api.LSPRouterRestoreCallbacks{
			BeforeMutation: func(result api.LSPRouterRestoreRowResult) error {
				key := mcpFrontReconcileRowKey(mcpFrontSurfaceLSP, result.Client, result.Language, result.EntryName)
				return persistMCPFrontDisposition(reportPath, &persisted, key, mcpFrontDispositionPending, result.Reason)
			},
			OnDisposition: func(result api.LSPRouterRestoreRowResult) error {
				key := mcpFrontReconcileRowKey(mcpFrontSurfaceLSP, result.Client, result.Language, result.EntryName)
				switch result.Status {
				case api.LSPRouterRestoreBaselineOnly:
					return persistMCPFrontDisposition(reportPath, &persisted, key, mcpFrontDispositionBaselineOnly, result.Reason)
				case api.LSPRouterRestoreRestored:
					return persistMCPFrontDisposition(reportPath, &persisted, key, mcpFrontDispositionRestored, result.Reason)
				case api.LSPRouterRestoreConflict:
					return persistMCPFrontDisposition(reportPath, &persisted, key, mcpFrontDispositionConflict, result.Reason)
				default:
					return persistMCPFrontDisposition(reportPath, &persisted, key, mcpFrontDispositionPending, result.Reason)
				}
			},
		},
	)
	lspRestored := len(lspReport.Applied) + len(lspReport.Restored)
	if lerr != nil && len(lspReport.Pending) == 0 && len(lspReport.Failed) == 0 {
		return fmt.Errorf("reconcile-mcp-front --rollback: lsp rollback: %w", lerr)
	}

	durable, durableErr := mcpFrontReadReportForRetirementFn(reportPath)
	if durableErr != nil {
		return fmt.Errorf("reconcile-mcp-front --rollback: re-read durable report: %w", durableErr)
	}
	if !canRetireMCPFrontReconcileReport(durable) {
		fmt.Fprintf(cmd.OutOrStdout(), "mcp-front rollback: serena(restored=%d) lsp(restored=%d removed=%d pending=%d failed=%d)\n",
			serenaRestored, lspRestored, len(lspReport.Removed), len(lspReport.Pending), len(lspReport.Failed))
		return fmt.Errorf("reconcile-mcp-front --rollback: recovery remains pending; rows not reachable right now or with unresolved ownership/write evidence: %s; the active report %s was kept",
			describePendingMCPFrontRows(durable), reportPath)
	}
	conflictRows := conflictMCPFrontRowLabels(durable)

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
		"port":            persisted.ActivePlan.Port,
		"serena_restored": serenaRestored,
		"lsp_restored":    lspRestored,
		"lsp_removed":     len(lspReport.Removed),
		"lsp_failed":      len(lspReport.Failed),
	})

	fmt.Fprintf(cmd.OutOrStdout(), "mcp-front rollback: serena(restored=%d) lsp(restored=%d removed=%d failed=%d)\n",
		serenaRestored, lspRestored, len(lspReport.Removed), len(lspReport.Failed))
	if len(conflictRows) > 0 {
		// A skipped-conflict row is the compare-and-swap inverse refusing to
		// write (I7): the live entry no longer equals what the forward run
		// recorded writing, so somebody EDITED it after the cutover and
		// restoring would silently discard that edit. Naming the row key alone
		// left the operator to infer all of that, so say it.
		return fmt.Errorf("reconcile-mcp-front --rollback: rollback completed, but %d entr%s left untouched because %s edited after the reconcile ran: %s. "+
			"Restoring would have discarded that edit, so the current value was kept and the rest of the rollback completed normally. "+
			"If the pre-reconcile value is the one you want, set it by hand; if the current value is correct, nothing more to do",
			len(conflictRows), map[bool]string{true: "y was", false: "ies were"}[len(conflictRows) == 1],
			map[bool]string{true: "it was", false: "they were"}[len(conflictRows) == 1],
			strings.Join(conflictRows, ", "))
	}
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
	// Defence in depth: every production caller decodes first, and the decoder
	// already refuses a foreign version. Routing this through the same owner
	// keeps ONE refusal wording for the operator instead of two that disagree
	// about what to do next.
	if persisted.Version != mcpFrontReconcileReportVersion {
		return mcpFrontArtifactRefusal(reportPath, persisted.Version,
			fmt.Sprintf("it declares version %d", persisted.Version))
	}
	if !persisted.SnapshotComplete {
		return fmt.Errorf("%s is not marked snapshot-complete: the run that wrote it did not finish capturing every recovery section, so consuming it would restore only part of the host and then delete the record of the rest. Re-run `mcphub install --reconcile-mcp-front` (which merges into this record without overwriting it) so the missing section is captured, or move the file aside and restore by hand", reportPath)
	}
	if persisted.Generation <= 0 {
		return fmt.Errorf("%s has no valid generation", reportPath)
	}
	if persisted.Rows == nil {
		return fmt.Errorf("%s carries no version-3 row map", reportPath)
	}
	if persisted.ActivePlan == nil || persisted.ActivePlan.Generation <= 0 {
		return fmt.Errorf("%s carries no active generation plan", reportPath)
	}
	if persisted.ActivePlan.Generation != persisted.Generation {
		return fmt.Errorf("%s active plan generation %d does not match artifact generation %d",
			reportPath, persisted.ActivePlan.Generation, persisted.Generation)
	}
	if persisted.ActivePlan.Port <= 0 {
		return fmt.Errorf("%s active generation plan has no valid port", reportPath)
	}
	if len(persisted.ActivePlan.Rows) != len(persisted.ActivePlan.Operations) {
		return fmt.Errorf("%s active generation plan has %d row references but %d operations",
			reportPath, len(persisted.ActivePlan.Rows), len(persisted.ActivePlan.Operations))
	}
	planRows := make(map[string]bool, len(persisted.ActivePlan.Rows))
	for _, key := range persisted.ActivePlan.Rows {
		if key == "" || planRows[key] {
			return fmt.Errorf("%s active generation plan has an empty or duplicate row reference %q", reportPath, key)
		}
		if _, ok := persisted.Rows[key]; !ok {
			return fmt.Errorf("%s active generation plan references missing row %q", reportPath, key)
		}
		planRows[key] = true
	}
	planOps := make(map[string]mcpFrontReconcilePlanOp, len(persisted.ActivePlan.Operations))
	for _, op := range persisted.ActivePlan.Operations {
		row, ok := persisted.Rows[op.RowKey]
		if !ok || !planRows[op.RowKey] {
			return fmt.Errorf("%s active generation operation references unplanned row %q", reportPath, op.RowKey)
		}
		if _, duplicate := planOps[op.RowKey]; duplicate {
			return fmt.Errorf("%s active generation has duplicate operation for row %q", reportPath, op.RowKey)
		}
		if err := validateMCPFrontOperation(row.Surface, op.Operation); err != nil {
			return fmt.Errorf("%s active generation row %q: %w", reportPath, op.RowKey, err)
		}
		if err := validateMCPFrontEntryState(row, op.PreState); err != nil {
			return fmt.Errorf("%s active generation row %q has invalid pre-state: %w", reportPath, op.RowKey, err)
		}
		if err := validateMCPFrontEntryState(row, op.IntendedState); err != nil {
			return fmt.Errorf("%s active generation row %q has invalid intended state: %w", reportPath, op.RowKey, err)
		}
		if op.Operation == "add" && !op.IntendedState.Present {
			return fmt.Errorf("%s active generation add row %q has an absent intended state", reportPath, op.RowKey)
		}
		if op.Operation == "remove" && op.IntendedState.Present {
			return fmt.Errorf("%s active generation remove row %q has a present intended state", reportPath, op.RowKey)
		}
		planOps[op.RowKey] = op
	}
	for key, row := range persisted.Rows {
		if key != mcpFrontRowKey(row) {
			return fmt.Errorf("%s row %q does not match its exact identity", reportPath, key)
		}
		switch row.Surface {
		case mcpFrontSurfaceSerena:
			if row.Client == "" || row.Language != "" || row.EntryName != "serena" || row.Pin == nil {
				return fmt.Errorf("%s row %q has an incomplete serena baseline", reportPath, key)
			}
		case mcpFrontSurfaceLSP:
			if row.Client == "" || row.Language == "" || row.EntryName == "" || row.Pin != nil {
				return fmt.Errorf("%s row %q has an incomplete lsp baseline", reportPath, key)
			}
		default:
			return fmt.Errorf("%s row %q has unknown surface %q", reportPath, key, row.Surface)
		}
		if !row.BaselineSet {
			return fmt.Errorf("%s row %q has no immutable baseline marker", reportPath, key)
		}
		if err := validateMCPFrontEntryState(row, row.Baseline); err != nil {
			return fmt.Errorf("%s row %q has invalid immutable baseline: %w", reportPath, key, err)
		}
		if row.Attempt != nil {
			if row.Attempt.Generation <= 0 || row.Attempt.Generation > persisted.Generation {
				return fmt.Errorf("%s row %q has an incomplete attempt", reportPath, key)
			}
			if err := validateMCPFrontOperation(row.Surface, row.Attempt.Operation); err != nil {
				return fmt.Errorf("%s row %q attempt: %w", reportPath, key, err)
			}
			if err := validateMCPFrontEntryState(row, row.Attempt.PreState); err != nil {
				return fmt.Errorf("%s row %q has invalid attempt pre-state: %w", reportPath, key, err)
			}
			if err := validateMCPFrontEntryState(row, row.Attempt.IntendedState); err != nil {
				return fmt.Errorf("%s row %q has invalid attempt intended state: %w", reportPath, key, err)
			}
			switch row.Attempt.State {
			case mcpFrontAttemptPrepared, mcpFrontAttemptConfirmedNoWrite,
				mcpFrontAttemptApplied, mcpFrontAttemptConflict,
				mcpFrontAttemptPreconditionConflict:
			default:
				return fmt.Errorf("%s row %q has unknown attempt state %q", reportPath, key, row.Attempt.State)
			}
			if row.Attempt.Generation == persisted.ActivePlan.Generation {
				op, ok := planOps[key]
				if !ok {
					return fmt.Errorf("%s current-generation attempt row %q is not referenced by the active plan", reportPath, key)
				}
				if op.Operation != row.Attempt.Operation ||
					!mcpFrontStateEqual(op.PreState, row.Attempt.PreState) ||
					!mcpFrontStateEqual(op.IntendedState, row.Attempt.IntendedState) {
					return fmt.Errorf("%s current-generation attempt row %q differs from its active-plan operation", reportPath, key)
				}
			}
			if (row.Attempt.State == mcpFrontAttemptPrepared || row.Attempt.State == mcpFrontAttemptConflict) &&
				row.Attempt.Generation != persisted.ActivePlan.Generation {
				return fmt.Errorf("%s uncertain row %q belongs to generation %d, not the active generation %d",
					reportPath, key, row.Attempt.Generation, persisted.ActivePlan.Generation)
			}
		}
		if row.Applied != nil {
			if row.Applied.Generation <= 0 || row.Applied.Generation > persisted.Generation || row.Applied.Port <= 0 {
				return fmt.Errorf("%s row %q has an invalid applied receipt", reportPath, key)
			}
			if err := validateMCPFrontEntryState(row, row.Applied.PostState); err != nil {
				return fmt.Errorf("%s row %q has an invalid applied post-state: %w", reportPath, key, err)
			}
			if row.Attempt != nil && row.Attempt.State == mcpFrontAttemptApplied &&
				(row.Applied.Generation != row.Attempt.Generation ||
					!mcpFrontStateEqual(row.Applied.PostState, row.Attempt.IntendedState)) {
				return fmt.Errorf("%s row %q applied receipt differs from its applied attempt", reportPath, key)
			}
		}
		if row.Disposition != nil {
			switch row.Disposition.State {
			case mcpFrontDispositionBaselineOnly, mcpFrontDispositionRestored,
				mcpFrontDispositionConflict, mcpFrontDispositionPending, mcpFrontDispositionFailed:
			default:
				return fmt.Errorf("%s row %q has unknown rollback disposition %q",
					reportPath, key, row.Disposition.State)
			}
		}
	}
	return nil
}

func validateMCPFrontOperation(surface, operation string) error {
	switch surface {
	case mcpFrontSurfaceSerena:
		if operation != "add" {
			return fmt.Errorf("serena operation %q is not supported", operation)
		}
	case mcpFrontSurfaceLSP:
		if operation != "add" && operation != "remove" {
			return fmt.Errorf("lsp operation %q is not supported", operation)
		}
	default:
		return fmt.Errorf("unknown surface %q", surface)
	}
	return nil
}

func validateMCPFrontEntryState(row mcpFrontReconcileRow, state mcpFrontEntryState) error {
	switch row.Surface {
	case mcpFrontSurfaceSerena:
		if state.LSP != nil {
			return errors.New("serena state carries an lsp projection")
		}
		if state.Present != (state.Fingerprint != "") {
			return errors.New("serena presence and fingerprint disagree")
		}
	case mcpFrontSurfaceLSP:
		if state.Fingerprint != "" || state.LSP == nil {
			return errors.New("lsp state lacks its exact projection")
		}
		if state.Present != state.LSP.Present {
			return errors.New("lsp presence and projection disagree")
		}
		if state.LSP.Client != row.Client ||
			state.LSP.Language != row.Language ||
			snapshotEntryNameCLI(*state.LSP) != row.EntryName {
			return errors.New("lsp projection identity differs from its recovery row")
		}
	default:
		return fmt.Errorf("unknown surface %q", row.Surface)
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
	pinRoot, rootErr := filepath.Abs(mcpFrontReconcilePinDir(reportPath))
	if rootErr != nil {
		return fmt.Errorf("%s: resolve pin root: %w", reportPath, rootErr)
	}
	seenPaths := map[string]string{}
	for key, row := range persisted.Rows {
		if row.Surface == mcpFrontSurfaceLSP {
			if row.Pin != nil {
				return fmt.Errorf("%s lsp row %q illegally carries a serena pin", reportPath, key)
			}
			continue
		}
		if row.Pin == nil {
			return fmt.Errorf("%s serena row %q has no row-owned pin", reportPath, key)
		}
		pin := *row.Pin
		if pin.Client != row.Client || pin.Path == "" || pin.Origin == "" || pin.SHA256 == "" {
			return fmt.Errorf("%s serena row %q has an incomplete or disagreeing row-owned pin", reportPath, key)
		}
		absolutePin, absErr := filepath.Abs(pin.Path)
		if absErr != nil {
			return fmt.Errorf("%s serena row %q pin path: %w", reportPath, key, absErr)
		}
		rel, relErr := filepath.Rel(pinRoot, absolutePin)
		if relErr != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." ||
			strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%s serena row %q pin %q escapes the report pin directory", reportPath, key, pin.Path)
		}
		cleanPin := filepath.Clean(absolutePin)
		if owner, duplicate := seenPaths[cleanPin]; duplicate {
			return fmt.Errorf("%s serena rows %q and %q share duplicate pin path %q", reportPath, owner, key, pin.Path)
		}
		seenPaths[cleanPin] = key
		sum, err := fileSHA256(cleanPin)
		if err != nil {
			return fmt.Errorf("%s: pinned pre-reconcile backup for %q is unreadable at %s: %w; no client is touched", reportPath, row.Client, pin.Path, err)
		}
		if sum != pin.SHA256 {
			return fmt.Errorf("%s: pinned pre-reconcile backup for %q at %s has changed (sha256 %s, recorded %s)", reportPath, row.Client, pin.Path, sum, pin.SHA256)
		}
	}
	return nil
}

// mcpFrontReadReportForRetirementFn is the narrow retirement-gate test seam.
// Production always resolves it to the canonical report reader. Focused tests
// use it to make the durable re-read remain pending after the rollback's local
// copy became terminal, proving that the caller gates retirement on durable
// evidence rather than its stale in-memory copy.
var mcpFrontReadReportForRetirementFn = readMCPFrontReconcileReport

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
	out, decodeErr := decodeMCPFrontReconcileReport(raw, path)
	if decodeErr != nil {
		return nil, decodeErr
	}
	if err := validateMCPFrontReconcileReport(out, path); err != nil {
		return nil, err
	}
	return out, nil
}

// mcpFrontLegacyBodyKeys are the top-level keys of the version-1/version-2
// journal body. Version 3 persists none of them (I2: the `rows` map is the only
// authority, and no top-level compatibility projection is written), so seeing
// any of them proves the bytes on disk are a pre-version-3 body regardless of
// what the `version` field claims.
//
// This exists because the two ways an operator meets a legacy artifact are NOT
// the same failure: a genuine version-1/2 file declares its own version, while
// a file written by a pre-release interim build can declare version 3 and still
// carry the old body. The strict decoder reports the second one as
// `json: unknown field "lsp"`, which names neither the file's real problem nor
// anything the operator can do about it.
var mcpFrontLegacyBodyKeys = []string{"serena", "lsp", "pins", "port"}

// mcpFrontLegacyBodyKeysPresent returns the version-1/2 body keys carried by
// raw, in mcpFrontLegacyBodyKeys order. An unparseable body returns nothing:
// the caller then falls back to the exact decoder error, which is the honest
// diagnostic for bytes that are not a journal at all.
func mcpFrontLegacyBodyKeysPresent(raw []byte) []string {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil
	}
	var found []string
	for _, key := range mcpFrontLegacyBodyKeys {
		if _, ok := top[key]; ok {
			found = append(found, key)
		}
	}
	return found
}

// mcpFrontArtifactRefusal is the SINGLE OWNER of the operator-facing refusal
// for an mcp-front reconcile journal this binary cannot consume.
//
// THE MIGRATION DECISION IT IMPLEMENTS: version 3 REFUSES a foreign-version
// artifact; it never upgrades, downgrades, or guesses at one
// (work-items/decisions/2026-07-27-mcp-front-reconcile-v3-row-journal.md,
// "Compatibility and migration"). The reason is not schema tidiness. A
// version-1/2 journal records which entries were CAPTURED, never which client
// write actually landed — it has no per-row attempt, no same-call applied
// receipt, and no generation. Synthesising version-3 rows from it would
// manufacture rollback authority that was never proven, and the first thing
// that authority does is overwrite a live client entry. Refusing costs the
// operator one manual step; upgrading can silently destroy an entry this hub
// never wrote.
//
// What the refusal MUST carry, and what a bare `json: unknown field "lsp"` or a
// bare "artifact version 2 cannot be consumed by version 3" did not: the file's
// absolute path, why the upgrade is refused rather than attempted, the fact
// that nothing was read and no client was touched, and the concrete moves that
// get the operator out of it.
//
// TWO ARMS, BECAUSE THE REMEDY IS OPPOSITE. An OLDER artifact means the file
// was written before this binary and the recovery binary is the older one; a
// NEWER artifact means the operator downgraded (or ran a different install) and
// the recovery binary is the newer one. Telling a version-4 holder to "roll
// back with the older mcphub" sends them to the exact binary that cannot read
// their file. `declared` therefore selects the arm; it is never assumed.
func mcpFrontArtifactRefusal(path string, declared int, detail string) error {
	if declared > mcpFrontReconcileReportVersion {
		return fmt.Errorf(
			"legacy-ownership-unproven: %s carries artifact version %d and this mcphub understands version %d — the file was written by a NEWER mcphub than the one you are running. "+
				"This binary will not guess at a format it does not know: it would be building rollback authority out of fields it cannot interpret, and the first thing that authority does is overwrite a live client entry. "+
				"Nothing was read from the file and no client config was touched. "+
				"Run `mcphub install --reconcile-mcp-front --rollback` with the NEWER mcphub that wrote it (upgrade this install, or use the binary you ran before), "+
				"or move the file aside (rename it to %s.unsupported) and restore the entries by hand before re-running this command",
			path, declared, mcpFrontReconcileReportVersion, path)
	}
	return fmt.Errorf(
		"legacy-ownership-unproven: %s is a pre-version-%d mcp-front reconcile journal (%s). "+
			"This binary will NOT upgrade it in place: the older format records which client entries were captured, "+
			"never which client write actually landed, so building version-%d rollback authority from it would let "+
			"`--rollback` overwrite an entry this hub never wrote. Nothing was read from the file and no client config was touched. "+
			"Do ONE of these: (1) run `mcphub install --reconcile-mcp-front --rollback` with the OLDER mcphub binary that wrote "+
			"this file — it understands the format and will put your clients back; or (2) move the file aside "+
			"(rename it to %s.legacy) and restore the entries by hand — it is plain JSON, and its `serena` and `lsp` sections "+
			"name every client together with its pre-reconcile URL. Then re-run `mcphub install --reconcile-mcp-front` to start "+
			"a fresh version-%d journal",
		path, mcpFrontReconcileReportVersion, detail,
		mcpFrontReconcileReportVersion, path, mcpFrontReconcileReportVersion)
}

func decodeMCPFrontReconcileReport(raw []byte, path string) (*mcpFrontReconcileReport, error) {
	var envelope struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if envelope.Version != mcpFrontReconcileReportVersion {
		return nil, mcpFrontArtifactRefusal(path, envelope.Version,
			fmt.Sprintf("it declares version %d", envelope.Version))
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var out mcpFrontReconcileReport
	if err := decoder.Decode(&out); err != nil {
		// A declared version 3 carrying the OLD body is the interim-build case:
		// same refusal, same remedy, but the version field cannot be trusted to
		// say so, and the raw decoder error names only the first stray key.
		if legacy := mcpFrontLegacyBodyKeysPresent(raw); len(legacy) > 0 {
			return nil, mcpFrontArtifactRefusal(path, envelope.Version, fmt.Sprintf(
				"it declares version %d but carries the pre-version-%d body shape: top-level %s",
				envelope.Version, mcpFrontReconcileReportVersion, strings.Join(legacy, ", ")))
		}
		return nil, fmt.Errorf("decode strict version-3 row journal %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("unexpected trailing JSON value")
		}
		return nil, fmt.Errorf("decode strict version-3 row journal %s: %w", path, err)
	}
	return &out, nil
}

// mcpFrontReconcileJournal owns the recovery record for ONE forward run.
//
// It exists so the record can be made durable INCREMENTALLY, ahead of each
// mutation, instead of once at the end. Its state is the accumulating record
// plus its exact row map. The constructor and prepare methods preserve every
// earlier immutable baseline and replace only the current attempt/receipt.
type mcpFrontReconcileJournal struct {
	reportPath string
	port       int
	record     mcpFrontReconcileReport
}

func newMCPFrontV3Journal(
	reportPath string,
	prior *mcpFrontReconcileReport,
	port int,
	plan *api.LSPRouterClientPlan,
) (*mcpFrontReconcileJournal, error) {
	record := mcpFrontReconcileReport{
		Version: mcpFrontReconcileReportVersion, SnapshotComplete: true,
		Rows: map[string]mcpFrontReconcileRow{},
	}
	if prior != nil {
		record = *prior
		if record.Rows == nil {
			record.Rows = map[string]mcpFrontReconcileRow{}
		}
	}
	record.Generation++
	active := &mcpFrontReconcilePlan{Generation: record.Generation, Port: port}
	if plan != nil {
		if len(plan.CaptureFailures) > 0 {
			return nil, fmt.Errorf("capture-incomplete: %d lsp plan row(s) failed", len(plan.CaptureFailures))
		}
		for _, op := range plan.Operations {
			key := mcpFrontReconcileRowKey(mcpFrontSurfaceLSP, op.Client, op.Language, op.EntryName)
			if existing, ok := record.Rows[key]; ok {
				if existing.Surface != mcpFrontSurfaceLSP || existing.Baseline.LSP == nil {
					return nil, fmt.Errorf("row %q has no immutable lsp baseline", key)
				}
			} else {
				baseline := op.PreState
				record.Rows[key] = mcpFrontReconcileRow{
					Surface: mcpFrontSurfaceLSP, Client: op.Client, Language: op.Language,
					EntryName: op.EntryName, Baseline: mcpFrontLSPState(baseline), BaselineSet: true,
				}
			}
			active.Rows = append(active.Rows, key)
			active.Operations = append(active.Operations, mcpFrontReconcilePlanOp{
				RowKey: key, Operation: op.Operation,
				PreState: mcpFrontLSPState(op.PreState), IntendedState: mcpFrontLSPState(op.IntendedState),
			})
		}
	}
	record.ActivePlan = active
	return &mcpFrontReconcileJournal{reportPath: reportPath, port: port, record: record}, nil
}

func (j *mcpFrontReconcileJournal) persist() error {
	return api.WriteStateFileAtomic(j.reportPath, j.record)
}

func (j *mcpFrontReconcileJournal) prepareSerenaAttempt(result api.SerenaReconcileAttemptResult) error {
	key := mcpFrontReconcileRowKey(mcpFrontSurfaceSerena, result.Client, "", "serena")
	row, ok := j.record.Rows[key]
	var createdPin *mcpFrontSerenaPin
	if ok {
		if row.Surface != mcpFrontSurfaceSerena || row.Pin == nil {
			return fmt.Errorf("journal-prepare-failed: serena row %q has no row-owned pinned baseline", key)
		}
	} else {
		pin, pinErr := j.pinBackup(result.Client, result.BackupPath)
		if pinErr != nil {
			return fmt.Errorf("journal-prepare-failed: pin serena baseline for %s: %w", result.Client, pinErr)
		}
		createdPin = &pin
		row = mcpFrontReconcileRow{
			Surface: mcpFrontSurfaceSerena, Client: result.Client, EntryName: "serena",
			Baseline: mcpFrontSerenaState(result.PreFingerprint), BaselineSet: true, Pin: &pin,
		}
	}
	row.Attempt = &mcpFrontReconcileAttempt{
		Generation: j.record.Generation, Operation: "add",
		PreState:      mcpFrontSerenaState(result.PreFingerprint),
		IntendedState: mcpFrontSerenaState(result.IntendedFingerprint),
		State:         mcpFrontAttemptPrepared,
	}
	row.Disposition = nil
	j.record.Rows[key] = row
	found := false
	for _, planKey := range j.record.ActivePlan.Rows {
		if planKey == key {
			found = true
			break
		}
	}
	if !found {
		j.record.ActivePlan.Rows = append(j.record.ActivePlan.Rows, key)
		j.record.ActivePlan.Operations = append(j.record.ActivePlan.Operations, mcpFrontReconcilePlanOp{
			RowKey: key, Operation: "add",
			PreState: row.Attempt.PreState, IntendedState: row.Attempt.IntendedState,
		})
	}
	if err := j.persist(); err != nil {
		if createdPin != nil {
			delete(j.record.Rows, key)
			if cleanupErr := reclaimOrphanedPin(*createdPin, j.reportPath); cleanupErr != nil {
				return fmt.Errorf("journal-prepare-failed: persist row: %w; reclaim unreferenced pin: %v", err, cleanupErr)
			}
		}
		return fmt.Errorf("journal-prepare-failed: persist row: %w", err)
	}
	return nil
}

func (j *mcpFrontReconcileJournal) finishSerenaAttempt(result api.SerenaReconcileAttemptResult) error {
	key := mcpFrontReconcileRowKey(mcpFrontSurfaceSerena, result.Client, "", "serena")
	return j.finishAttempt(
		key,
		mcpFrontSerenaState(result.ObservedFingerprint),
		result.Invoked,
		result.PreconditionConflict,
		result.ObservationErr,
	)
}

func (j *mcpFrontReconcileJournal) prepareLSPOperation(op api.LSPRouterPlannedOperation) error {
	key := mcpFrontReconcileRowKey(mcpFrontSurfaceLSP, op.Client, op.Language, op.EntryName)
	row, ok := j.record.Rows[key]
	if !ok || row.Baseline.LSP == nil {
		return fmt.Errorf("journal-prepare-failed: lsp row %q has no immutable baseline", key)
	}
	row.Attempt = &mcpFrontReconcileAttempt{
		Generation: j.record.Generation, Operation: op.Operation,
		PreState: mcpFrontLSPState(op.PreState), IntendedState: mcpFrontLSPState(op.IntendedState),
		State: mcpFrontAttemptPrepared,
	}
	row.Disposition = nil
	j.record.Rows[key] = row
	return j.persist()
}

func (j *mcpFrontReconcileJournal) finishLSPOperation(obs api.LSPRouterMutationObservation) error {
	key := mcpFrontReconcileRowKey(
		mcpFrontSurfaceLSP, obs.Operation.Client, obs.Operation.Language, obs.Operation.EntryName)
	return j.finishAttempt(
		key,
		mcpFrontLSPState(obs.ObservedState),
		obs.Invoked,
		obs.PreconditionConflict,
		obs.ObservationErr,
	)
}

func (j *mcpFrontReconcileJournal) finishAttempt(
	key string,
	observed mcpFrontEntryState,
	invoked bool,
	preconditionConflict bool,
	observationErr error,
) error {
	row, ok := j.record.Rows[key]
	if !ok {
		// A first-generation Serena precondition conflict produced no mutation
		// and therefore needs no inverse row or pin.
		if !invoked && preconditionConflict {
			return nil
		}
		return fmt.Errorf("promotion-not-durable: row %q is absent", key)
	}
	if !invoked {
		if !preconditionConflict {
			return nil
		}
		op, found := activeMCPFrontPlanOperation(j.record.ActivePlan, key)
		if !found {
			return fmt.Errorf("forward-plan-precondition-conflict-not-durable: row %q is not in the active plan", key)
		}
		row.Attempt = &mcpFrontReconcileAttempt{
			Generation:    j.record.Generation,
			Operation:     op.Operation,
			PreState:      op.PreState,
			IntendedState: op.IntendedState,
			State:         mcpFrontAttemptPreconditionConflict,
		}
		row.Disposition = &mcpFrontRollbackDisposition{
			State: mcpFrontDispositionConflict, Reason: "forward-plan-precondition-conflict",
		}
		j.record.Rows[key] = row
		if err := j.persist(); err != nil {
			return fmt.Errorf("forward-plan-precondition-conflict-not-durable: %w", err)
		}
		return nil
	}
	if row.Attempt == nil || row.Attempt.State != mcpFrontAttemptPrepared {
		return fmt.Errorf("promotion-not-durable: row %q has no durable prepared attempt", key)
	}
	if observationErr != nil {
		row.Disposition = &mcpFrontRollbackDisposition{State: mcpFrontDispositionPending, Reason: "forward-ownership-unknown"}
		j.record.Rows[key] = row
		if err := j.persist(); err != nil {
			return err
		}
		return fmt.Errorf("forward-ownership-unknown: %s", key)
	}
	switch {
	case mcpFrontStateEqual(observed, row.Attempt.IntendedState):
		row.Attempt.State = mcpFrontAttemptApplied
		row.Applied = &mcpFrontAppliedReceipt{
			Generation: row.Attempt.Generation, Port: j.port, PostState: row.Attempt.IntendedState,
		}
		row.Disposition = nil
	case mcpFrontStateEqual(observed, row.Attempt.PreState):
		row.Attempt.State = mcpFrontAttemptConfirmedNoWrite
		row.Disposition = nil
	default:
		row.Attempt.State = mcpFrontAttemptConflict
		row.Disposition = &mcpFrontRollbackDisposition{State: mcpFrontDispositionPending, Reason: "forward-ownership-unknown"}
	}
	j.record.Rows[key] = row
	if err := j.persist(); err != nil {
		return fmt.Errorf("promotion-not-durable: %w", err)
	}
	if row.Attempt.State == mcpFrontAttemptConflict {
		return fmt.Errorf("forward-ownership-unknown: %s", key)
	}
	return nil
}

func activeMCPFrontPlanOperation(plan *mcpFrontReconcilePlan, key string) (mcpFrontReconcilePlanOp, bool) {
	if plan == nil {
		return mcpFrontReconcilePlanOp{}, false
	}
	for _, op := range plan.Operations {
		if op.RowKey == key {
			return op, true
		}
	}
	return mcpFrontReconcilePlanOp{}, false
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
