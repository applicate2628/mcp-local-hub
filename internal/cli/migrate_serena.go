// Package cli — `mcphub migrate serena legacy-to-dynamic-pool` (Phase 4 of
// docs/superpowers/plans/2026-05-29-serena-migrate-redesign.md).
//
// The driver migrates the serena server from its legacy 2-daemon (claude+codex)
// or unified-intermediate `kind: global` manifest to the dynamic-pool
// `kind: workspace-scoped` + daemon_template shape, fanning out one
// supervisor-intent daemon per registered serena workspace — WITHOUT rewriting
// the disk manifest. The original (removed in a7dcbcd) command's defect was a
// disk-manifest rewrite that the embed-first read path never read; this redesign
// builds the dynamic-pool manifest IN MEMORY (Phase 2 builder
// BuildInMemorySerenaDynamicPoolManifest) and materializes it via
// api.InstallParsedManifest (Phase 1 seam), so the on-disk manifest is never
// touched.
//
// Sequence (design §6 + §7.1; REAP-FIRST ordering — bot PR #250):
//
//  1. Source-state detect — catalog AND runtime. The catalog shape (parent plan
//     §D.3 table) classifies legacy-2-daemon / intermediate-unified /
//     already-migrated / malformed, but it is NEVER rewritten, so it CANNOT
//     short-circuit to a no-op — it only classifies the SOURCE for building the
//     manifest (finding #2). The AUTHORITATIVE "did the cutover happen" signal is
//     the committed supervisor-intent.json carrying a dynamic-pool serena row (a
//     serena daemon with a materialized runtime_spec): runtime dynamic-pool →
//     idempotent exit-0 if the supervisor is healthy, else recovery-start (Fix 5);
//     runtime legacy/missing → PROCEED with the cutover REGARDLESS of catalog
//     shape (a dynamic-pool catalog can ship while this host's runtime is still
//     legacy). A malformed catalog still hard-errors at detect time.
//  2. Build the dynamic-pool manifest in memory (NOT written to disk) + allocate
//     a pool port per workspace + register (OUTER rollback armed: registry).
//  3. Client-reconcile to the constant /serena/mcp router. FAIL on partial: if
//     the report has ANY failed client, roll the reconcile back (restore the
//     already-rewritten clients to legacy) + the registry, and abort — BEFORE
//     the reap, while legacy 9121 is still up so every client keeps working.
//     Reconcile must FULLY succeed before the point of no return (§7.1: "legacy
//     endpoints removed only after the router rewrite succeeds").
//  4. REAP the OLD supervisor via the cold-restart primitives DIRECTLY
//     (ReapSupervisorForRestart: quiesce → exit{graceful} → force-kill fallback →
//     verify ports unbound). NO binary swap (same binary — RunInstallUpgrade's
//     rename-aside would abort), NO successor start yet. If the prior supervisor
//     cannot be reaped → FAIL LOUD (do not write an intent a stuck old supervisor
//     would ignore — §7.1 acceptance #2). THIS is the point of no return.
//  5. Write the spec-bearing intent (api.InstallParsedManifest). The §7.1 gate
//     now passes NATURALLY — no supervisor is running after the reap.
//  6. DISARM the outer rollback: once InstallParsedManifest commits the intent,
//     the install is the commit point. A later (start) failure must NOT roll the
//     registry/reconcile back — that would create split-state (intent has daemon
//     rows with ports, registry reverted to 0 → router can't resolve). Finding #2.
//  7. START the new supervisor (it cold-reconciles the new intent → dynamic-pool
//     daemons come up). A start failure AFTER the commit is fail-loud-with-guidance
//     only; the registry is NOT rolled back (#2).
//
// Recovery invariant: if the reap (4) succeeds but the intent write (5) fails, no
// supervisor is running and the OLD intent is still on disk → the driver attempts
// a best-effort supervisor restart (it reads the still-on-disk old intent →
// legacy restored), else fails loud with explicit operator guidance. It NEVER
// leaves no-supervisor-running silently, and reap-first structurally prevents a
// committed new-intent coexisting with the old supervisor.
//
// Rollback composition (parent plan §D.3 "Outer/inner rollback composition"):
// this driver owns the OUTER stack covering the registry alloc/save AND the
// client-reconcile restore. api.InstallParsedManifest owns the INNER stack for
// scheduler tasks + per-client config + intent write. The outer stack is DISARMED
// the instant InstallParsedManifest commits (step 6), so a post-commit start
// failure never re-runs it. (Unlike the removed predecessor, there is NO
// manifest-write step, so the outer stack does not snapshot/restore a manifest.)
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/gui"
)

// serenaMigrateServerName is the canonical serena server name read by the
// source-state detector and passed to the in-memory builder.
const serenaMigrateServerName = "serena"

// serenaSourceState classifies the detected source shape of the serena
// manifest. The migration is only defined for the legacy and intermediate
// shapes; already-migrated is a no-op and malformed is a hard error.
type serenaSourceState int

const (
	serenaSourceMalformed serenaSourceState = iota
	serenaSourceLegacy2Daemon
	serenaSourceUnifiedIntermediate
	serenaSourceAlreadyMigrated
)

func (s serenaSourceState) String() string {
	switch s {
	case serenaSourceLegacy2Daemon:
		return "legacy-2-daemon"
	case serenaSourceUnifiedIntermediate:
		return "unified-intermediate"
	case serenaSourceAlreadyMigrated:
		return "already-migrated"
	default:
		return "malformed"
	}
}

// detectSerenaSourceState classifies a parsed serena manifest per the parent
// plan §D.3 source-state table:
//
//	| Legacy 2-daemon       | daemons[] == {claude, codex}, daemon_template absent |
//	| Intermediate unified  | daemons[] == {unified},        daemon_template absent |
//	| Already migrated      | daemons[] empty/absent,        daemon_template present |
//	| Malformed / partial   | anything else → error (manual reconciliation)        |
//
// Refuse-on-malformed: any shape outside the three recognized states returns
// serenaSourceMalformed with an explicit error.
func detectSerenaSourceState(m *config.ServerManifest) (serenaSourceState, error) {
	switch {
	case len(m.Daemons) == 0 && m.DaemonTemplate != nil:
		return serenaSourceAlreadyMigrated, nil
	case m.DaemonTemplate == nil && len(m.Daemons) == 2 &&
		daemonNamed(m.Daemons, "claude") && daemonNamed(m.Daemons, "codex"):
		return serenaSourceLegacy2Daemon, nil
	case m.DaemonTemplate == nil && len(m.Daemons) == 1 && daemonNamed(m.Daemons, "unified"):
		return serenaSourceUnifiedIntermediate, nil
	default:
		return serenaSourceMalformed, fmt.Errorf(
			"serena manifest is in an unrecognized state (daemons=%d, daemon_template_present=%t) — "+
				"manual reconciliation required: the migration only supports the legacy 2-daemon "+
				"(claude+codex), unified-intermediate (single `unified`), or already-migrated "+
				"(daemon_template only) shapes",
			len(m.Daemons), m.DaemonTemplate != nil)
	}
}

// daemonNamed reports whether daemons contains an entry with the given name.
func daemonNamed(daemons []config.DaemonSpec, name string) bool {
	for _, d := range daemons {
		if d.Name == name {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Test seams. Production binds each to its real implementation; tests override
// them to drive the driver deterministically without a real scheduler / GUI /
// supervisor.
// ---------------------------------------------------------------------------

// loadSerenaManifestForMigrateFn loads + parses the serena manifest used for
// source-state detection AND as the catalog input to the in-memory builder.
// Default: api.ManifestGet("serena") (honors the MCPHUB_MANIFEST_DIR_OVERRIDE
// test seam → reads a seeded temp manifest with no embed leakage).
var loadSerenaManifestForMigrateFn = func() (*config.ServerManifest, error) {
	raw, err := api.NewAPI().ManifestGet(serenaMigrateServerName)
	if err != nil {
		return nil, fmt.Errorf("load serena manifest: %w", err)
	}
	m, err := config.ParseManifest(strings.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse serena manifest: %w", err)
	}
	return m, nil
}

// installParsedManifestFn is the package-level seam over
// api.InstallParsedManifest. Tests override it to inject an install failure
// (exercising the outer registry rollback) without reaching the real
// scheduler / state-file pipeline.
var installParsedManifestFn = func(ctx context.Context, a *api.API, m *config.ServerManifest, opts api.InstallParsedManifestOpts) (string, error) {
	return a.InstallParsedManifest(ctx, m, opts)
}

// reconcileSerenaClientsFn is the package-level seam over
// api.ReconcileSerenaClientsToRouter. Default wires the gui-package pidport
// primitives (which the api package cannot import without a cycle) so the
// reconcile discovers the live GUI router port; tests override it to assert
// ordering (reconcile runs before legacy removal) without a live GUI.
var reconcileSerenaClientsFn = func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
	pidportPath, err := gui.PidportPath()
	if err != nil {
		return nil, fmt.Errorf("resolve gui pidport path: %w", err)
	}
	return api.ReconcileSerenaClientsToRouter(ctx, api.SerenaReconcileOpts{
		PidportPath:  pidportPath,
		ReadPidport:  gui.ReadPidport,
		RemoveLegacy: true,
		BackupKeepN:  effectiveBackupKeepN(),
	})
}

// restoreReconcileFn is the package-level seam over
// api.RestoreSerenaReconcileApplied — the outer-rollback compensator that
// restores the already-rewritten clients to their legacy entry when the
// reconcile fails on some clients (finding #3) OR when a later pre-commit step
// errors. Default restores against the live clients.AllClients(); tests override
// it to assert the restore fires without touching real client configs.
var restoreReconcileFn = func(report *api.MigrateReport) error {
	return api.RestoreSerenaReconcileApplied(report, nil)
}

// migrateSerenaReapFn reaps the OLD supervisor BEFORE the spec-bearing intent
// write (§7.1 reap-first ordering). It runs the IPC quiesce → exit{graceful} →
// force-kill fallback → verify-ports-unbound sub-sequence (ReapSupervisorForRestart)
// WITHOUT a binary swap and WITHOUT starting a successor. Default is the
// per-platform production binding (defaultMigrateSerenaReap): on Windows it
// builds the real UpgradeDeps and resolves expected ports from the still-on-disk
// OLD supervisor-intent.json; on other platforms it returns
// errSupervisorRestartUnsupported (the supervisor cold-restart primitives are
// Windows-only in v0.5.0 — release scope is Windows GA / Linux beta / macOS
// preview). A non-nil return means the prior supervisor could not be reaped, so
// the migrate FAILS LOUD before committing an intent a stuck old supervisor
// would silently ignore (§7.1 acceptance criterion 2). Tests override it to
// assert it fires BEFORE the intent write and to exercise the fail-loud path.
var migrateSerenaReapFn = defaultMigrateSerenaReap

// migrateSerenaStartFn starts the NEW supervisor AFTER the spec-bearing intent
// write commits. The fresh supervisor cold-reconciles from the just-written
// intent (re-materializing nil-spec serena rows before spawning). Default is
// the per-platform production binding (defaultMigrateSerenaStart): Windows
// drives the detached per-OS supervisor spawn THEN polls IPC reconcile-ready
// (Fix 4); non-Windows fails loud. A non-nil return AFTER the intent commit is
// fail-loud-with-guidance only — the driver does NOT roll back the registry
// (the intent is the commit point; rolling back would create split-state per
// finding #2). Tests override it.
var migrateSerenaStartFn = defaultMigrateSerenaStart

// migrateSerenaSupervisorHealthyFn reports whether a supervisor is currently
// running AND reconcile-ready (Fix 5, PR #250 deeper review — consultant Q2).
// The idempotency-recovery branch uses it to distinguish a GENUINE
// already-migrated no-op (healthy supervisor → do nothing) from a recovery
// situation (the dynamic-pool intent is committed but no reconcile-ready
// supervisor is running → drive the start without re-reaping / re-writing).
// Default is the per-platform binding (defaultMigrateSerenaSupervisorHealthy):
// it checks the supervisor lock cross-platform and, on Windows, additionally
// probes IPC reconcile-ready; a (false, err) return means health could not be
// confirmed, which the caller treats as a recovery situation. Tests override it.
var migrateSerenaSupervisorHealthyFn = defaultMigrateSerenaSupervisorHealthy

// migrateSerenaSupervisorRunningFn reports whether a supervisor process is
// currently RUNNING (bot PR #250 finding #3). Unlike the heavier
// migrateSerenaSupervisorHealthyFn (which also probes IPC reconcile-ready), this
// is the lightweight lock-only liveness signal — the same one
// SupervisorRunningUnderStateDir and the §7.1 install gate read — used PRE-reap
// to decide whether a cutover reap is even needed. When NO supervisor is running
// (the operator stopped it per §7.1 guidance, or a fresh host), the reap is a
// no-op-equivalent whose production stub fails loud on non-Windows, so the
// cutover would be blocked even though the §7.1 install liveness gate is already
// satisfied (nothing to reap). Gating willReap on this probe lets such a migrate
// proceed straight to the intent write + supervisor start.
//
// A (_, err) return means liveness is UNDETERMINABLE (e.g. a lock-probe failure
// on a hardened host); the caller treats that conservatively as "running" and
// still attempts the reap, so an undeterminable probe never silently skips a
// needed reap (the §7.1 install gate is the fail-closed backstop either way).
// Default is the cross-platform lock probe; tests override it.
var migrateSerenaSupervisorRunningFn = defaultMigrateSerenaSupervisorRunning

// defaultMigrateSerenaSupervisorRunning is the production binding for
// migrateSerenaSupervisorRunningFn: a cross-platform supervisor-lock liveness
// probe (the lock primitive is platform-neutral via flock). It resolves the
// state dir through stateDirFunc so it honors MCPHUB_STATE_DIR_OVERRIDE exactly
// like serenaRuntimeIntentIsDynamicPool.
func defaultMigrateSerenaSupervisorRunning() (bool, error) {
	stateDir, err := stateDirFunc()
	if err != nil {
		return false, fmt.Errorf("resolve state dir for supervisor liveness probe: %w", err)
	}
	running, _, perr := api.SupervisorRunningUnderStateDir(stateDir)
	if perr != nil {
		return false, fmt.Errorf("probe supervisor liveness: %w", perr)
	}
	return running, nil
}

// ---------------------------------------------------------------------------
// Command wiring.
// ---------------------------------------------------------------------------

// newMigrateSerenaCmd builds the `serena` subcommand of `migrate`.
//
// With no further subcommand, `mcphub migrate serena [more] [flags]` behaves
// exactly like the leaf `mcphub migrate serena [more] [flags]` did before this
// command existed (stdio→HTTP rewrite), via a delegating RunE that re-prepends
// the consumed `serena` token. The dynamic-pool migration lives under the
// `legacy-to-dynamic-pool` child.
func newMigrateSerenaCmd() *cobra.Command {
	var clientsFlag string
	var dryRun, jsonOut bool
	c := &cobra.Command{
		Use:   "serena [server]...",
		Short: "serena-specific migration helpers",
		Long: `Subcommand group for serena migrations.

With no further subcommand, ` + "`mcphub migrate serena [more] [flags]`" + ` rewrites
the serena (and any additional listed servers') client entries from stdio to hub
HTTP — exactly as ` + "`mcphub migrate serena [more]`" + ` did before this command
existed. The dynamic-pool migration lives under the
` + "`legacy-to-dynamic-pool`" + ` subcommand.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Re-prepend the consumed "serena" token so the delegate
			// migrates the serena server plus any additional servers.
			servers := append([]string{serenaMigrateServerName}, args...)
			return runStdioMigrate(cmd, servers, clientsFlag, dryRun, jsonOut)
		},
	}
	c.Flags().StringVar(&clientsFlag, "clients", "", "comma-separated subset of clients (default: every binding in the manifest)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "show what would change, don't write")
	c.Flags().BoolVar(&jsonOut, "json", false, "machine-readable JSON output")
	c.AddCommand(newMigrateSerenaDynamicPoolCmd())
	return c
}

// newMigrateSerenaDynamicPoolCmd builds
// `mcphub migrate serena legacy-to-dynamic-pool`.
func newMigrateSerenaDynamicPoolCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "legacy-to-dynamic-pool",
		Short: "Migrate serena from legacy 2-daemon/unified to the dynamic per-workspace pool",
		Long: `Switch serena from its legacy global 2-daemon (claude+codex) or
unified-intermediate shape to the dynamic-pool kind=workspace-scoped +
daemon_template form, fanning out one supervisor-intent daemon per registered
serena workspace. The on-disk manifest is NOT rewritten — the dynamic-pool
manifest is built in memory and materialized via the supervisor intent.

Idempotent: re-running against an already-migrated serena is a no-op
(exit 0, no writes).

After a spec-bearing migrate, the supervisor is cold-restarted (the existing
upgrade flow) so the binary that next reads the new runtime_spec intent is the
new one; if the prior supervisor cannot be exited, the migrate fails loud.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMigrateSerenaDynamicPool(cmd.Context(), cmd.OutOrStdout())
		},
	}
}

// runMigrateSerenaDynamicPool is the dynamic-pool migration driver.
//
// err is named so the deferred outer-rollback closure can inspect and rewrite
// it (composite-error contract per §D.3 defense layer 3).
func runMigrateSerenaDynamicPool(ctx context.Context, w io.Writer) (err error) {
	// 1. Load + parse the serena manifest; detect source state. The manifest
	//    is READ ONLY — it is the catalog input to the in-memory builder and
	//    the source-state classifier. It is never written.
	src, err := loadSerenaManifestForMigrateFn()
	if err != nil {
		return err
	}
	state, err := detectSerenaSourceState(src)
	if err != nil {
		return err
	}

	// 2. already-migrated decision — RUNTIME-AUTHORITATIVE (finding #2 + #5).
	//    The committed supervisor-intent.json is the SINGLE source of truth for
	//    "did the cutover already happen". The catalog shape (detectSerenaSourceState
	//    above) classifies the SOURCE for BUILDING the dynamic-pool manifest, but
	//    it MUST NOT short-circuit to a no-op: the migrate never rewrites the
	//    catalog, so a dynamic-pool-shaped catalog can legitimately coexist with a
	//    still-legacy runtime — e.g. a future embedded-manifest update ships the
	//    dynamic-pool catalog, or an operator edits only the manifest, while THIS
	//    host's committed intent is still legacy/missing. A catalog-only no-op
	//    there would strand the operator: the legacy clients/supervisor stay in
	//    place with NO way to run the cutover. So:
	//
	//      - runtime intent already dynamic-pool → the Fix-5 health-check branch
	//        below (no-op if the supervisor is healthy; recovery-start otherwise).
	//      - runtime intent legacy/missing → PROCEED with the migration REGARDLESS
	//        of catalog shape (the cutover has not happened on this host).
	alreadyMigrated, amErr := serenaRuntimeIntentIsDynamicPool()
	if amErr != nil {
		return fmt.Errorf("inspect supervisor-intent.json for an existing serena dynamic-pool migration: %w", amErr)
	}
	if alreadyMigrated {
		// The committed intent is ALREADY dynamic-pool. Fix 5 (PR #250 deeper
		// review — consultant Q2): "already migrated by runtime" is NOT
		// unconditionally a no-op. Two sub-cases:
		//
		//   (i)  supervisor running + reconcile-ready → GENUINE no-op. Do not
		//        re-reap / re-write / bounce a healthy supervisor.
		//   (ii) supervisor NOT running / not reconcile-ready → this is a
		//        RECOVERY situation: a prior run committed the dynamic-pool
		//        intent then FAILED the start (Fix 4's readiness poll failed,
		//        or the host crashed). Re-running would otherwise no-op and
		//        leave the operator stuck — clients on the /serena/mcp router,
		//        no daemons. Drive the start (with the Fix 4 readiness poll) so
		//        the re-run brings the already-committed intent live. Do NOT
		//        re-reap or re-write: the intent is already correct; only the
		//        supervisor is missing.
		healthy, hErr := migrateSerenaSupervisorHealthyFn()
		if hErr != nil {
			fmt.Fprintf(w, "warning: could not determine supervisor health (%v); treating as a recovery situation and starting the supervisor.\n", hErr)
		}
		if healthy {
			fmt.Fprintln(w, "serena is already migrated to the dynamic pool (runtime intent already carries the workspace-scoped serena descriptor) and the supervisor is running and reconcile-ready; nothing to do.")
			return nil
		}
		fmt.Fprintln(w, "serena is already migrated to the dynamic pool but no reconcile-ready supervisor is running — recovering by starting the supervisor against the already-committed intent (no re-reap, no re-write)…")
		if startErr := migrateSerenaStartFn(ctx, w); startErr != nil {
			return fmt.Errorf(
				"serena dynamic-pool intent is already committed but the recovery supervisor start failed: %w; "+
					"the intent on disk is correct (no re-reap / re-write needed) — "+
					"run `mcphub supervise` from a shell so the current binary reconciles it", startErr)
		}
		fmt.Fprintln(w, "recovery complete: the supervisor is reconciling the already-committed serena dynamic-pool intent.")
		return nil
	}

	// 3. Build the dynamic-pool manifest IN MEMORY (the core regression guard:
	//    the disk manifest is never rewritten). BuildInMemorySerenaDynamicPoolManifest
	//    sources command + base_args from the catalog manifest and the EFFECTIVE
	//    daemon_template (port_pool/context/extra_args) from the Phase 2 owner.
	dynamicManifest, err := api.BuildInMemorySerenaDynamicPoolManifest(src)
	if err != nil {
		return err
	}

	// 4. Acquire the registry flock; snapshot serena workspaces. The flock is
	//    held ONLY across the alloc → Save mutation (Fix 3, PR #250 deeper
	//    review): the reconcile + reap that follow run for tens of seconds
	//    (quiesce 30s + exit 5s + port-verify 10s) and touch NOTHING in the
	//    registry, so holding the lock across them needlessly blocks every
	//    concurrent `mcphub` registry op (install/register) for that whole
	//    window. We release the lock the instant Save returns (below). The
	//    registry rollback closure re-acquires the lock for its restore.
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		return err
	}
	reg := api.NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		return err
	}
	// releaseRegistryLock is the single idempotent unlock path. It is called
	// explicitly right after the registry Save (the only registry mutation)
	// and is also deferred as a belt-and-suspenders guard for the error paths
	// BEFORE that explicit release. Idempotency (regLockReleased guard) means
	// the deferred call is a harmless no-op once the explicit release fired —
	// no double-unlock.
	regLockReleased := false
	releaseRegistryLock := func() {
		if regLockReleased {
			return
		}
		regLockReleased = true
		unlock()
	}
	defer releaseRegistryLock()
	if err := reg.Load(); err != nil {
		return err
	}

	// Open the audit log up front so the success and rollback paths can emit. A
	// nil log (open failure) degrades to no audit but does not abort.
	events := openMigrateSerenaAuditLog(w)

	// OUTER rollback stack — owns ONLY the registry alloc/save. The inner
	// InstallParsedManifest stack owns scheduler/client/intent undos; the outer
	// stack never pushes those, so there is no double-undo (§D.3 composition).
	var rollback []func() error
	defer func() {
		if err == nil {
			return
		}
		var rbErrs []error
		for i := len(rollback) - 1; i >= 0; i-- {
			if e := rollback[i](); e != nil {
				rbErrs = append(rbErrs, e)
			}
		}
		if len(rbErrs) > 0 {
			failed := make([]string, 0, len(rbErrs))
			for _, e := range rbErrs {
				failed = append(failed, e.Error())
			}
			if events != nil {
				_ = events.Emit(api.SupervisorEvent{
					SchemaVersion: api.SupervisorEventSchemaVersion,
					Severity:      api.SupervisorEventSeverityError,
					Source:        "migration",
					Event:         "rollback-incomplete",
					Body: map[string]any{
						"primary_error": err.Error(),
						"failed_undos":  failed,
					},
				})
			}
			err = fmt.Errorf("%w; rollback also failed: %v", err, errors.Join(rbErrs...))
		}
	}()

	// 5. Allocate a serena pool port per workspace + Save. Snapshot the serena
	//    rows first so the undo restores the exact prior serena state. Zero
	//    workspaces (claim #7) is legitimate: the loop is a no-op, Save is a
	//    no-op-equivalent, and the install proceeds with zero daemon rows.
	serenaBefore := snapshotSerenaRows(reg)
	if err := allocateSerenaMigratePorts(reg, dynamicManifest); err != nil {
		return err
	}
	if err := reg.Save(); err != nil {
		// Save is atomic (tempfile + rename): a failure leaves the prior
		// on-disk registry intact, so there is nothing to roll back. The
		// deferred releaseRegistryLock frees the still-held lock. Crucially we
		// do NOT push the relocking undo here — it would deadlock trying to
		// re-acquire the lock this same goroutine still holds.
		return err
	}
	// The registry mutation is committed on disk. NOW push the relocking undo
	// (it re-acquires the lock itself, so it must run only after we release)
	// and release the lock (Fix 3). Nothing after this point (reconcile, reap,
	// intent write, start) touches the registry, so holding the lock across the
	// multi-second reap would only block concurrent registry ops. Ordering is
	// load-bearing: releaseRegistryLock MUST run before any deferred rollback
	// fires (it does — the explicit call below executes during the function
	// body, the deferred rollback runs after the body returns).
	rollback = append(rollback, func() error {
		return restoreSerenaRegistryRelocking(regPath, serenaBefore)
	})
	releaseRegistryLock()

	// Re-read the freshly-allocated serena rows so the PRE-reap predicates see
	// the assigned ports + task names. This is the count used to decide willReap
	// / willStart; the AUTHORITATIVE Workspaces snapshot the install fans out
	// from is RE-READ under a re-acquired lock immediately before the install
	// (finding #2 — captures any workspace registered during the released-lock
	// window). A non-nil (possibly empty) slice is required: nil would drop the
	// server's existing rows.
	allocated := reg.SerenaEntries()
	if allocated == nil {
		allocated = []api.WorkspaceEntry{}
	}

	// hasWorkspaces: at least one candidate workspace → the install will (modulo
	// stale-workspace pruning) materialize a runtime_spec row, so the cutover
	// must (re)start a supervisor to bring the dynamic-pool daemons live. Zero
	// candidates (claim #7) → the install writes a no-runtime_spec intent an old
	// supervisor reads fine; no reap, no restart.
	hasWorkspaces := len(allocated) > 0

	// supervisorRunning (finding #3): probe supervisor liveness BEFORE deciding
	// to reap. A reap is only meaningful when a supervisor is actually running;
	// when none is (the operator stopped it per §7.1 guidance, or a fresh host),
	// the production reap stub fails loud on non-Windows, so reaping a
	// non-existent supervisor would needlessly block an otherwise-valid migrate
	// — the §7.1 install liveness gate is already satisfied (nothing to reap). We
	// only probe when there are candidate workspaces (a zero-workspace install
	// never reaps regardless). An undeterminable probe is treated conservatively
	// as running so a needed reap is never silently skipped.
	supervisorRunning := false
	if hasWorkspaces {
		running, probeErr := migrateSerenaSupervisorRunningFn()
		if probeErr != nil {
			fmt.Fprintf(w, "warning: could not determine whether a supervisor is running (%v); assuming one is and reaping it before the intent write.\n", probeErr)
			supervisorRunning = true
		} else {
			supervisorRunning = running
		}
	}

	// willReap is the PRE-write predicate for "reap a RUNNING supervisor before
	// the spec-bearing write" (reap-first ordering). It requires BOTH a candidate
	// workspace AND a running supervisor (finding #3): a cutover with no
	// supervisor running skips the reap and goes straight to the intent write
	// (the §7.1 gate passes — no supervisor), then starts one. willStart is the
	// PRE-write predicate for "(re)start a supervisor after the write so the new
	// dynamic-pool intent comes live": it fires whenever there are daemon rows,
	// whether we reaped a running supervisor or there was none to reap. NOTE:
	// even if every candidate is pruned as stale at install time (so the written
	// intent ends up non-spec-bearing), once willReap fired we have committed to
	// the reap → we ALWAYS start a successor afterward (recovery invariant: never
	// leave no-supervisor-running).
	willReap := hasWorkspaces && supervisorRunning
	willStart := hasWorkspaces

	// Fix 6 (PR #250 deeper review — consultant nice-to-have): warn the operator
	// up front that a cutover briefly takes serena offline. The reap → intent
	// write → supervisor-start window (tens of seconds: quiesce up to 30s + exit
	// up to 5s + port-verify up to 10s + cold reconcile) is when the legacy
	// serena daemons are gone and the dynamic-pool ones are not yet ready. Only
	// printed for an actual reap (willReap) — a zero-workspace install or a
	// no-supervisor-running cutover never reaps an online daemon to take offline.
	if willReap {
		fmt.Fprintln(w, "NOTE: this cutover briefly takes serena OFFLINE while the supervisor is reaped and restarted (tens of seconds); clients reconnect once the new dynamic-pool daemons are reconcile-ready.")
	}

	// 6. Client-reconcile to the constant /serena/mcp router URL — BEFORE the
	//    reap, so legacy 9121 is still up and a per-client failure leaves that
	//    client on its still-functional legacy endpoint. The reconcile records a
	//    per-client backup; on a PARTIAL failure (any report.Failed row) we MUST
	//    NOT proceed to the irreversible reap with only SOME clients on the
	//    router — restore the rewritten clients to legacy + roll the registry
	//    back, and abort (finding #3; §7.1 "legacy removed only after the router
	//    rewrite succeeds" → the rewrite must FULLY succeed first). A whole-run
	//    blocker (GUI not live) also fails the migrate. The reconcile-restore is
	//    pushed onto the OUTER rollback so the deferred stack runs it on ANY
	//    pre-commit error after this point (the reap-fail path included).
	report, rerr := reconcileSerenaClientsFn(ctx, w)
	if rerr != nil {
		err = fmt.Errorf("client-reconcile to /serena/mcp router: %w", rerr)
		return err
	}
	printSerenaReconcileReport(w, report)
	rollback = append(rollback, func() error { return restoreReconcileFn(report) })
	if len(report.Failed) > 0 {
		err = fmt.Errorf(
			"client-reconcile to /serena/mcp router failed on %d client(s) — refusing to reap the supervisor with a partially-migrated client set; "+
				"the already-rewritten clients are being restored to their legacy endpoint and the registry rolled back. "+
				"Resolve the per-client failures above and re-run the migrate (legacy serena is untouched)", len(report.Failed))
		return err
	}

	// 7. REAP the OLD supervisor BEFORE the spec-bearing intent write (reap-first
	//    ordering — bot PR #250). Only for a cutover (willReap): an OLD supervisor
	//    binary's intent watcher uses DisallowUnknownFields and would reject the
	//    new runtime_spec field, silently no-op'ing the migration at runtime. The
	//    reap runs quiesce → exit{graceful} → force-kill fallback → verify ports
	//    unbound (NO binary swap — same binary; NO successor start yet). If the
	//    prior supervisor cannot be reaped → FAIL LOUD (the deferred outer stack
	//    restores the reconcile + registry; legacy stays the source of truth) —
	//    §7.1 acceptance #2. THIS is the point of no return for the cutover.
	if willReap {
		fmt.Fprintln(w, "Reaping the running supervisor before the runtime_spec intent write (so the current binary is the one that reconciles it)…")
		if reapErr := migrateSerenaReapFn(ctx, w); reapErr != nil {
			err = fmt.Errorf(
				"supervisor reap (§7.1) failed BEFORE the runtime_spec intent write: %w; "+
					"the new serena dynamic-pool intent was NOT written and legacy serena is untouched — "+
					"stop any running supervisor and re-run the migrate", reapErr)
			return err
		}
	}

	// 7b. RE-READ the registry under a re-acquired lock immediately before the
	//     install (finding #2 — bot PR #250). The lock was released after the
	//     migrate's Save (Fix 3) so the multi-second reconcile + reap did not
	//     block concurrent registry ops. During that window a concurrent
	//     `mcphub workspace register --backend serena` may have committed a NEW
	//     serena row. If we fed the install the pre-release `allocated` snapshot,
	//     that workspace would be present in the registry + clients but ABSENT
	//     from supervisor-intent.json — the router would resolve a workspace whose
	//     daemon the restarted supervisor never spawns. So we reload, re-run port
	//     allocation over the CURRENT rows (allocating a pool port for any
	//     concurrently-added port-less row — allocateSerenaMigratePorts is
	//     idempotent for rows that already carry one), Save, and read the
	//     authoritative snapshot the install fans out from. The lock is held ONLY
	//     across this fast reload/realloc/Save — never across the slow reap. This
	//     re-acquire is fully nested and released before the install; the install
	//     acquires a DISTINCT lock (supervisor-intent.json.lock), and the deferred
	//     registry rollback re-acquires the registry lock only after this function
	//     returns, so there is no double-hold / deadlock on any path.
	//
	//     A failure here lands AFTER the reap (the point of no return), so it is
	//     handled exactly like an install failure: the recovery start restores a
	//     running supervisor from the still-on-disk OLD intent when we reaped, and
	//     the deferred outer stack restores the reconcile + registry.
	installWorkspaces, reReadErr := reReadAndAllocateSerenaForInstall(regPath, dynamicManifest)
	if reReadErr != nil {
		err = fmt.Errorf("re-read registry under a re-acquired lock before the intent write: %w", reReadErr)
		if willReap {
			fmt.Fprintln(w, "registry re-read failed after the supervisor reap — restarting a supervisor to restore the prior (legacy) intent…")
			if startErr := migrateSerenaStartFn(ctx, w); startErr != nil {
				err = fmt.Errorf("%w; AND the recovery supervisor start ALSO failed: %v — "+
					"NO supervisor is running and the prior (legacy) intent is on disk; "+
					"run `mcphub supervise` from a shell to restore the legacy serena daemons", err, startErr)
			}
		}
		return err
	}

	// 8. Write the spec-bearing intent. The §7.1 gate inside InstallParsedManifest
	//    now passes NATURALLY — after the reap no supervisor holds the lock, so the
	//    spec-bearing write is the safe state (the NEXT supervisor start is THIS
	//    binary). The inner stack owns scheduler/client/intent undos; on error here
	//    those are ALREADY undone and the deferred outer stack restores the
	//    reconcile + registry. The dynamic-pool manifest carries no client_bindings
	//    (the /serena/mcp router owns routing), so the only side effect is the
	//    per-workspace supervisor-intent fan-out driven by opts.Workspaces (the
	//    finding-#2 re-read snapshot).
	intentPath, ierr := installParsedManifestFn(ctx, api.NewAPI(), dynamicManifest, api.InstallParsedManifestOpts{
		Writer:     w,
		Workspaces: installWorkspaces,
	})
	if ierr != nil {
		// Recovery invariant: if we reaped (step 7) but the write failed, NO
		// supervisor is running and the OLD intent is still on disk. Restart a
		// supervisor so it reads the still-on-disk old intent (legacy restored)
		// rather than leaving no-supervisor-running silently. The outer stack
		// still restores the reconcile + registry (legacy is the source of truth).
		err = ierr
		if willReap {
			fmt.Fprintln(w, "intent write failed after the supervisor reap — restarting a supervisor to restore the prior (legacy) intent…")
			if startErr := migrateSerenaStartFn(ctx, w); startErr != nil {
				err = fmt.Errorf("%w; AND the recovery supervisor start ALSO failed: %v — "+
					"NO supervisor is running and the prior (legacy) intent is on disk; "+
					"run `mcphub supervise` from a shell to restore the legacy serena daemons", err, startErr)
			}
		}
		return err
	}

	// 9. DISARM the outer rollback (finding #2). InstallParsedManifest has
	//    committed the new intent — that IS the commit point. A later failure
	//    (the start below) must NOT roll the registry/reconcile back: doing so
	//    would leave the committed intent with daemon rows + ports while the
	//    registry reverts to port 0, so the /serena/mcp router could not resolve
	//    a workspace (split-state). Clear the stack so the deferred undo is a
	//    no-op from here on.
	rollback = nil

	// 10. START the new supervisor (only after a cutover reap). It cold-reconciles
	//     from the just-written intent, re-materializing any nil-spec serena rows
	//     BEFORE spawning, and the dynamic-pool daemons come up. A start failure
	//     AFTER the intent commit is fail-loud-with-guidance ONLY — the intent is
	//     committed, so the registry is NOT rolled back (#2); the operator re-runs
	//     or starts the supervisor by hand.
	specBearing, scanErr := intentHasRuntimeSpecRow(intentPath)
	if scanErr != nil {
		// The intent IS committed; the verify re-read failed (a real Windows
		// file-handle / ACL contention race right after the write). Do NOT roll
		// back — the intent is the commit point. But if there are daemon rows to
		// bring live (willStart), the just-committed intent must still be
		// reconciled: drive the recovery start so we never leave
		// no-supervisor-running silently (the same invariant the ierr != nil
		// path upholds). The message names the reap only when we actually reaped
		// (willReap); a no-supervisor-running cutover (finding #3) starts a fresh
		// one without one. The start failing too surfaces BOTH errors with
		// operator guidance.
		err = fmt.Errorf("verify supervisor-intent runtime_spec rows after the committed write: %w "+
			"(the intent is committed; the registry is NOT rolled back)", scanErr)
		if willStart {
			if willReap {
				fmt.Fprintln(w, "intent-verify re-read failed after the supervisor reap — starting the supervisor anyway so the committed intent is reconciled…")
			} else {
				fmt.Fprintln(w, "intent-verify re-read failed after the committed write — starting the supervisor so the committed intent is reconciled…")
			}
			if startErr := migrateSerenaStartFn(ctx, w); startErr != nil {
				err = fmt.Errorf("%w; AND the supervisor start ALSO failed: %v — "+
					"the new serena dynamic-pool intent is committed on disk but no supervisor is running; "+
					"run `mcphub supervise` from a shell so the current binary reconciles it", err, startErr)
			}
		} else {
			err = fmt.Errorf("%w — start the supervisor with `mcphub supervise` if it is not already running", err)
		}
		return err
	}
	if willStart {
		fmt.Fprintln(w, "Starting the supervisor so it reconciles the new serena dynamic-pool intent…")
		if startErr := migrateSerenaStartFn(ctx, w); startErr != nil {
			// FAIL LOUD with guidance; intent committed, registry NOT rolled back (#2).
			return fmt.Errorf(
				"supervisor start (§7.1) failed after the runtime_spec intent was committed: %w; "+
					"the new serena dynamic-pool intent is on disk but no supervisor is running — "+
					"run `mcphub supervise` from a shell (or re-run the migrate) so the current binary reconciles it. "+
					"The registry is intentionally NOT rolled back: the intent is the commit point", startErr)
		}
	} else {
		fmt.Fprintln(w, "No registered serena workspaces — installed the dynamic-pool intent with zero daemon rows; no supervisor reap/restart required.")
	}

	// 11. Emit the success audit event.
	if events != nil {
		_ = events.Emit(api.SupervisorEvent{
			SchemaVersion: api.SupervisorEventSchemaVersion,
			Severity:      api.SupervisorEventSeverityInfo,
			Source:        "migration",
			Event:         "serena-dynamic-pool-migration",
			Body: map[string]any{
				"source_state":      state.String(),
				"target_workspaces": serenaWorkspacePaths(allocated),
				"allocated_ports":   serenaWorkspacePorts(allocated),
				"spec_bearing":      specBearing,
				"reaped":            willReap,
			},
		})
	}

	fmt.Fprintln(w, "serena dynamic-pool migration complete.")
	return nil
}

// serenaRuntimeIntentIsDynamicPool reports whether the committed
// supervisor-intent.json already carries a serena dynamic-pool descriptor — a
// daemon row with Server=="serena" and a materialized RuntimeSpec. This is the
// AUTHORITATIVE runtime "already migrated" signal (finding #5): the migrate
// never rewrites the catalog manifest, so the catalog shape alone would
// re-trigger the migration forever; the runtime intent is the truth for whether
// the cutover already happened. A missing intent file (cutover never ran) reports
// (false, nil) — there is nothing to migrate away from yet, but a legacy supervisor
// may still be running, so the caller proceeds with the migrate. Only a genuine
// read/parse error (corrupt envelope, insecure parent) propagates.
func serenaRuntimeIntentIsDynamicPool() (bool, error) {
	stateDir, err := stateDirFunc()
	if err != nil {
		return false, fmt.Errorf("resolve state dir: %w", err)
	}
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	intent, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No committed intent yet — the cutover has not run. Not already
			// migrated; proceed (a legacy supervisor may still be running).
			return false, nil
		}
		return false, err
	}
	if intent == nil {
		return false, nil
	}
	for i := range intent.Daemons {
		if intent.Daemons[i].Server == serenaMigrateServerName && intent.Daemons[i].RuntimeSpec != nil {
			return true, nil
		}
	}
	return false, nil
}

// intentHasRuntimeSpecRow re-reads the written supervisor-intent.json and
// reports whether any row carries a materialized RuntimeSpec. This is the
// authoritative signal for whether the §7.1 restart gate must fire (the write
// fanned out ≥1 live workspace) versus a zero-workspace install (claim #7) that
// needs no restart. An empty intentPath (e.g. a dry-run path that returns "")
// reports false with no error.
func intentHasRuntimeSpecRow(intentPath string) (bool, error) {
	if intentPath == "" {
		return false, nil
	}
	intent, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		return false, err
	}
	if intent == nil {
		return false, nil
	}
	return intent.HasRuntimeSpecRow(), nil
}

// openMigrateSerenaAuditLog resolves the supervisor-events.log path under the
// active state dir and opens it. A failure degrades to nil (no audit) +
// a warning; it never aborts the migration.
func openMigrateSerenaAuditLog(w io.Writer) *api.SupervisorEventLog {
	stateDir, err := stateDirFunc()
	if err != nil {
		fmt.Fprintf(w, "warning: cannot resolve state dir for audit log: %v\n", err)
		return nil
	}
	events, err := api.OpenSupervisorEventLog(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
	if err != nil {
		fmt.Fprintf(w, "warning: cannot open supervisor-events.log: %v\n", err)
		return nil
	}
	return events
}

// printSerenaReconcileReport prints the per-(client, server) reconcile rows.
func printSerenaReconcileReport(w io.Writer, report *api.MigrateReport) {
	if report == nil {
		return
	}
	for _, app := range report.Applied {
		fmt.Fprintf(w, "✓ %s/%s → %s\n", app.Server, app.Client, app.URL)
	}
	for _, f := range report.Failed {
		fmt.Fprintf(w, "✗ %s/%s: %s (client left on its legacy endpoint; retry the migrate)\n", f.Server, f.Client, f.Err)
	}
}

// ---------------------------------------------------------------------------
// Registry snapshot/restore + port allocation (the OUTER §D.3 rollback scope).
// ---------------------------------------------------------------------------

// snapshotSerenaRows returns a copy of just the registry's serena rows for the
// rollback restore. Only serena rows are snapshotted because the migrate only
// mutates serena rows (allocateSerenaMigratePorts) — the relocking restore
// reloads the rest of the registry from disk so concurrent non-serena writes
// during the (Fix 3) unlocked window survive a rollback.
func snapshotSerenaRows(reg *api.Registry) []api.WorkspaceEntry {
	return append([]api.WorkspaceEntry(nil), reg.SerenaEntries()...)
}

// restoreSerenaRegistryRelocking is the registry rollback compensator. Because
// Fix 3 releases the registry lock right after the migrate's Save (so the
// multi-second reconcile + reap do not block concurrent registry ops), this
// restore must RE-ACQUIRE the lock itself. It then RELOADS the current on-disk
// registry and restores ONLY the serena rows the migrate actually snapshotted —
// it is SURGICAL (finding #1), not a blanket drop-all-serena-rows reset.
//
// The migrate's only serena mutation is allocateSerenaMigratePorts, which
// iterates the serena rows that existed when serenaSnapshot was taken and
// assigns each a pool port + task name (PutSerena upsert) — it NEVER adds a
// serena row for a workspace key absent from the snapshot. So the exact set of
// serena keys the migrate could have changed IS the snapshot's key set.
// Restoring is therefore an upsert of each snapshot row back to its
// pre-migrate port/fields.
//
// Crucially, any serena row whose workspace key is NOT in the snapshot is a
// row the migrate never touched — most importantly a CONCURRENT
// `mcphub workspace register` for serena that committed a NEW row during the
// released-lock window. The old blanket "drop every serena row, re-add the
// snapshot" reset DELETED that concurrent registration even though the migrate
// never touched it. The surgical restore leaves every non-snapshotted serena
// row (and every non-serena row) intact and reverts only the snapshotted keys.
//
// DUAL of that surgical guard (finding #1, bot PR #250): a snapshot key may have
// DISAPPEARED from the reloaded registry — a concurrent
// `mcphub workspace unregister --backend serena` removed the workspace during
// the released-lock window. Blindly re-PutSerena-ing it would RESURRECT a row the
// user just removed (and which the migrate no longer owns). So we restore a
// snapshot key's prior port ONLY IF that workspace still exists in the reloaded
// registry; a key gone from disk is SKIPPED (left unregistered). Restoring the
// concurrent-register case (key present, migrate changed its port) and skipping
// the concurrent-unregister case (key absent) are the two halves of keeping the
// rollback consistent with whatever concurrent registry writes landed in the
// window.
//
// The lock is always released before returning (no leaked lock).
func restoreSerenaRegistryRelocking(regPath string, serenaSnapshot []api.WorkspaceEntry) error {
	reg := api.NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		return fmt.Errorf("restore registry: re-acquire lock: %w", err)
	}
	defer unlock()
	if err := reg.Load(); err != nil {
		return fmt.Errorf("restore registry: reload: %w", err)
	}
	// Upsert each snapshotted serena row back to its pre-migrate state, but ONLY
	// if the workspace still exists in the reloaded registry. PutSerena is an
	// in-place upsert keyed on (WorkspaceKey, @serena), so for a still-present key
	// this reverts the port/task-name the migrate assigned; for a key that a
	// concurrent unregister removed during the released-lock window, GetSerena
	// reports absent and we SKIP it rather than resurrect the removed workspace.
	// Every other row — non-serena rows AND concurrent new serena registrations
	// not in the snapshot — stays untouched on the reloaded disk state.
	for _, e := range serenaSnapshot {
		if _, present := reg.GetSerena(e.WorkspaceKey); !present {
			// Concurrently unregistered during the released-lock window — do not
			// re-add a row the user removed and the migrate no longer owns.
			continue
		}
		if err := reg.PutSerena(e); err != nil {
			return fmt.Errorf("restore registry: put serena row for workspace_key %s: %w", e.WorkspaceKey, err)
		}
	}
	if err := reg.Save(); err != nil {
		return fmt.Errorf("restore registry: save: %w", err)
	}
	return nil
}

// allocateSerenaMigratePorts assigns a serena pool port to every serena
// workspace row that does not already have one, writing the assignment back via
// PutSerena. The pool is the EFFECTIVE dynamic-pool port pool (from the built
// in-memory manifest's daemon_template). Idempotent for rows that already carry
// a port. Zero serena rows → no-op (claim #7).
func allocateSerenaMigratePorts(reg *api.Registry, dynamicManifest *config.ServerManifest) error {
	if dynamicManifest.DaemonTemplate == nil || dynamicManifest.DaemonTemplate.PortPool == nil {
		return fmt.Errorf("internal: in-memory dynamic-pool manifest has no daemon_template.port_pool")
	}
	pool := *dynamicManifest.DaemonTemplate.PortPool
	for _, ws := range reg.SerenaEntries() {
		cur, ok := reg.Get(ws.WorkspaceKey, api.SerenaLanguageSentinel)
		if !ok {
			continue
		}
		if cur.Port > 0 {
			continue
		}
		port, err := reg.AllocateSerenaPort(pool)
		if err != nil {
			return fmt.Errorf("allocate serena port for workspace %s: %w", cur.WorkspacePath, err)
		}
		cur.Port = port
		if cur.TaskName == "" {
			cur.TaskName = api.SerenaTaskNameForWorkspace(cur.WorkspacePath)
		}
		if err := reg.PutSerena(cur); err != nil {
			return fmt.Errorf("persist allocated port for workspace %s: %w", cur.WorkspacePath, err)
		}
	}
	return nil
}

// reReadAndAllocateSerenaForInstall re-acquires the registry lock, reloads the
// CURRENT on-disk registry, re-runs the serena port allocation over it, Saves,
// and returns the authoritative serena-row snapshot the install fans out from
// (finding #2, bot PR #250). It exists because the migrate releases the registry
// lock after its first Save (Fix 3) so the slow reconcile + reap do not block
// concurrent registry ops; a concurrent `mcphub workspace register --backend
// serena` may therefore have committed a NEW serena row during that window. By
// reloading + re-allocating here (idempotent for rows that already carry a port,
// so the originally-allocated rows are untouched and any concurrently-added
// port-less row gets a pool port), the returned snapshot reflects every current
// serena row — the install then writes a supervisor-intent daemon row for each,
// so the registry/clients and the intent stay consistent.
//
// The lock is held ONLY across this fast reload/realloc/Save and released before
// returning (no leaked lock), so it never overlaps the slow reap. The returned
// slice is always non-nil (an empty registry yields an empty slice) because
// InstallParsedManifest requires a non-nil Workspaces snapshot.
func reReadAndAllocateSerenaForInstall(regPath string, dynamicManifest *config.ServerManifest) ([]api.WorkspaceEntry, error) {
	reg := api.NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		return nil, fmt.Errorf("re-acquire registry lock: %w", err)
	}
	defer unlock()
	if err := reg.Load(); err != nil {
		return nil, fmt.Errorf("reload registry: %w", err)
	}
	if err := allocateSerenaMigratePorts(reg, dynamicManifest); err != nil {
		return nil, err
	}
	if err := reg.Save(); err != nil {
		return nil, fmt.Errorf("save registry after re-allocation: %w", err)
	}
	entries := reg.SerenaEntries()
	if entries == nil {
		entries = []api.WorkspaceEntry{}
	}
	return entries, nil
}

// serenaWorkspacePaths extracts the workspace paths for the audit body.
func serenaWorkspacePaths(workspaces []api.WorkspaceEntry) []string {
	out := make([]string, 0, len(workspaces))
	for _, ws := range workspaces {
		out = append(out, ws.WorkspacePath)
	}
	return out
}

// serenaWorkspacePorts extracts the allocated ports for the audit body.
func serenaWorkspacePorts(workspaces []api.WorkspaceEntry) []int {
	out := make([]int, 0, len(workspaces))
	for _, ws := range workspaces {
		out = append(out, ws.Port)
	}
	return out
}
