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
//     §D.3 table) classifies legacy-2-daemon / intermediate-unified / malformed,
//     but it is NEVER rewritten, so it alone cannot mark "already migrated"
//     (catalog-shape would re-trigger forever). The AUTHORITATIVE
//     already-migrated signal is the committed supervisor-intent.json carrying a
//     dynamic-pool serena row (a serena daemon with a materialized runtime_spec).
//     Either signal → idempotent exit-0 (no reap, no write, no restart — never
//     bounce a healthy supervisor).
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
// drives the detached per-OS supervisor spawn; non-Windows fails loud. A
// non-nil return AFTER the intent commit is fail-loud-with-guidance only — the
// driver does NOT roll back the registry (the intent is the commit point;
// rolling back would create split-state per finding #2). Tests override it.
var migrateSerenaStartFn = defaultMigrateSerenaStart

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

	// 2. already-migrated → idempotent no-op, exit 0, zero writes. TWO signals
	//    are checked, EITHER of which means "nothing to do":
	//
	//    (a) catalog shape — the embedded/effective manifest is the dynamic-pool
	//        daemon_template form (detectSerenaSourceState above). Only relevant
	//        if the on-disk manifest was ever updated by hand; the migrate never
	//        rewrites it.
	//    (b) RUNTIME shape (finding #5, the authoritative one) — the committed
	//        supervisor-intent.json already carries a serena dynamic-pool row (a
	//        serena daemon with a materialized runtime_spec). The migrate never
	//        touches the catalog, so catalog-shape ALONE would re-trigger the
	//        migration forever; the runtime intent is the truth for "did the
	//        cutover already happen". When it is already dynamic-pool we MUST NOT
	//        reap/write/restart — bouncing a healthy supervisor and re-reaping is
	//        the regression #5 closes.
	if state == serenaSourceAlreadyMigrated {
		fmt.Fprintln(w, "serena is already migrated to the dynamic pool (catalog shape); nothing to do.")
		return nil
	}
	alreadyMigrated, amErr := serenaRuntimeIntentIsDynamicPool()
	if amErr != nil {
		return fmt.Errorf("inspect supervisor-intent.json for an existing serena dynamic-pool migration: %w", amErr)
	}
	if alreadyMigrated {
		fmt.Fprintln(w, "serena is already migrated to the dynamic pool (runtime intent already carries the workspace-scoped serena descriptor); nothing to do.")
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
	//    held across the whole snapshot → install → (rollback) sequence so no
	//    concurrent writer interleaves.
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		return err
	}
	reg := api.NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		return err
	}
	defer unlock()
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

	// 5. Allocate a serena pool port per workspace + Save, pushing the registry
	//    undo BEFORE the mutation. Snapshot the rows first so the undo restores
	//    the exact prior rows and re-persists them under the still-held lock.
	//    Zero workspaces (claim #7) is legitimate: the loop is a no-op, Save is
	//    a no-op-equivalent, and the install proceeds with zero daemon rows.
	portsBefore := snapshotSerenaRegistry(reg)
	if err := allocateSerenaMigratePorts(reg, dynamicManifest); err != nil {
		return err
	}
	rollback = append(rollback, func() error { return restoreSerenaRegistry(reg, portsBefore) })
	if err := reg.Save(); err != nil {
		return err
	}

	// Re-read the freshly-allocated serena rows so the fan-out sees the assigned
	// ports + task names. A non-nil (possibly empty) Workspaces snapshot is
	// required for the install: nil would drop the server's existing rows.
	allocated := reg.SerenaEntries()
	if allocated == nil {
		allocated = []api.WorkspaceEntry{}
	}

	// willReap is the PRE-write predicate for "this migrate is a cutover that
	// must reap-then-restart the supervisor". At least one candidate workspace
	// → the install will (modulo stale-workspace pruning) materialize a
	// runtime_spec row, so an OLD supervisor must be reaped before the write
	// and a fresh one started after. Zero candidates (claim #7) → the install
	// writes a no-runtime_spec intent an old supervisor reads fine; no reap, no
	// restart. NOTE: even if every candidate is pruned as stale at install
	// time (so the written intent ends up non-spec-bearing), once willReap
	// fired we have committed to the reap → we ALWAYS start a successor
	// afterward (recovery invariant: never leave no-supervisor-running).
	willReap := len(allocated) > 0

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

	// 8. Write the spec-bearing intent. The §7.1 gate inside InstallParsedManifest
	//    now passes NATURALLY — after the reap no supervisor holds the lock, so the
	//    spec-bearing write is the safe state (the NEXT supervisor start is THIS
	//    binary). The inner stack owns scheduler/client/intent undos; on error here
	//    those are ALREADY undone and the deferred outer stack restores the
	//    reconcile + registry. The dynamic-pool manifest carries no client_bindings
	//    (the /serena/mcp router owns routing), so the only side effect is the
	//    per-workspace supervisor-intent fan-out driven by opts.Workspaces.
	intentPath, ierr := installParsedManifestFn(ctx, api.NewAPI(), dynamicManifest, api.InstallParsedManifestOpts{
		Writer:     w,
		Workspaces: allocated,
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
		// The intent IS committed; surface the verify failure but do NOT roll back.
		return fmt.Errorf("verify supervisor-intent runtime_spec rows after the committed write: %w "+
			"(the intent is committed; start the supervisor with `mcphub supervise` if it is not already running)", scanErr)
	}
	if willReap {
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

// snapshotSerenaRegistry returns a copy of the registry's workspace rows for
// rollback restore. The caller holds the registry flock across the whole
// snapshot → mutate → save → restore sequence, so no concurrent writer can
// interleave; the only state the rollback reconstructs is this process's own
// port allocations.
func snapshotSerenaRegistry(reg *api.Registry) []api.WorkspaceEntry {
	return append([]api.WorkspaceEntry(nil), reg.Workspaces...)
}

// restoreSerenaRegistry resets the registry rows to the snapshot and persists.
func restoreSerenaRegistry(reg *api.Registry, snapshot []api.WorkspaceEntry) error {
	reg.Workspaces = append([]api.WorkspaceEntry(nil), snapshot...)
	if err := reg.Save(); err != nil {
		return fmt.Errorf("restore registry: %w", err)
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
