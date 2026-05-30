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
// Sequence (design §6 + §7.1):
//
//  1. Source-state detect (parent plan §D.3 table) from the EMBEDDED/effective
//     serena manifest: legacy-2-daemon / intermediate-unified → migrate;
//     already-migrated → idempotent exit-0; malformed → error.
//  2. Build the dynamic-pool manifest in memory (NOT written to disk).
//  3. Snapshot + preserve the serena workspace registry rows (the OUTER
//     rollback scope per §D.3).
//  4. api.InstallParsedManifest materializes per-workspace RuntimeSpec rows into
//     supervisor-intent.json (the INNER rollback scope — scheduler/client/intent
//     undos live there; the outer stack never re-runs them, so no double-undo).
//  5. Client-reconcile to the constant /serena/mcp router BEFORE the legacy 9121
//     endpoints are removed (a per-client failure leaves that client on the
//     still-functional legacy endpoint).
//  6. §7.1 supervisor upgrade/restart gate: when the intent write introduced
//     runtime_spec rows, drive the existing cold-restart upgrade flow
//     (install_upgrade.go: quiesce → exit{graceful} → force-kill fallback →
//     start-new-supervisor) so no OLD supervisor binary keeps reading the new
//     runtime_spec intent. If the prior supervisor cannot be quiesced/exited the
//     migrate FAILS LOUD rather than committing an intent a stuck old supervisor
//     would ignore.
//
// Rollback composition (parent plan §D.3 "Outer/inner rollback composition"):
// this driver owns the OUTER stack covering ONLY the registry alloc/save.
// api.InstallParsedManifest owns the INNER stack for scheduler tasks + per-client
// config + intent write. When InstallParsedManifest returns an error, its inner
// stack has ALREADY undone its own steps; the outer stack then undoes only the
// registry. The outer stack never pushes the inner's undos, so there is no
// double-undo. (Unlike the removed predecessor, there is NO manifest-write step,
// so the outer stack does not snapshot/restore a manifest.)
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// migrateSerenaRestartFn drives the §7.1 supervisor cold-restart after a
// spec-bearing intent write + client reconcile. Default is the per-platform
// production binding (defaultMigrateSerenaRestart): on Windows it builds the
// real UpgradeDeps and calls RunInstallUpgrade; on other platforms it returns
// errSupervisorRestartUnsupported (the supervisor cold-restart wiring is
// Windows-only in v0.5.0 — release scope is Windows GA / Linux beta / macOS
// preview). Tests override it to assert it fires after the intent write and to
// exercise the fail-loud path.
var migrateSerenaRestartFn = defaultMigrateSerenaRestart

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

	// 2. already-migrated → idempotent no-op, exit 0, zero writes.
	if state == serenaSourceAlreadyMigrated {
		fmt.Fprintln(w, "serena is already migrated to the dynamic pool; nothing to do.")
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
	// ports + task names.
	allocated := reg.SerenaEntries()

	// 6. Install the parsed (in-memory) manifest. The inner stack owns
	//    scheduler/client/intent undos; on error here those are ALREADY undone
	//    and the outer stack fires for the registry only. The dynamic-pool
	//    manifest carries no client_bindings (the /serena/mcp router owns
	//    routing), so the only side effect is the per-workspace
	//    supervisor-intent fan-out driven by opts.Workspaces. A non-nil (possibly
	//    empty) Workspaces snapshot is required: nil would drop the server's
	//    existing rows.
	if allocated == nil {
		allocated = []api.WorkspaceEntry{}
	}
	intentPath, ierr := installParsedManifestFn(ctx, api.NewAPI(), dynamicManifest, api.InstallParsedManifestOpts{
		Writer:     w,
		Workspaces: allocated,
	})
	if ierr != nil {
		err = ierr
		return err
	}

	// 7. Client-reconcile to the constant /serena/mcp router URL BEFORE legacy
	//    9121 removal (the reconcile itself removes the legacy endpoint only
	//    AFTER each client's router rewrite succeeds). A whole-run blocker (GUI
	//    not live) fails the migrate; per-client failures are reported and
	//    leave that client on the still-functional legacy endpoint.
	report, rerr := reconcileSerenaClientsFn(ctx, w)
	if rerr != nil {
		err = fmt.Errorf("client-reconcile to /serena/mcp router: %w", rerr)
		return err
	}
	printSerenaReconcileReport(w, report)

	// 8. §7.1 supervisor upgrade/restart gate. Only required when the intent
	//    write introduced runtime_spec rows: an OLD supervisor binary's intent
	//    watcher uses DisallowUnknownFields and would reject the new field,
	//    silently no-op'ing the migration at runtime. Drive the cold-restart so
	//    the binary that next reads the intent is the new one. A NO-spec write
	//    (zero workspaces — claim #7) needs no restart: an old supervisor reads a
	//    no-runtime_spec intent fine.
	specBearing, scanErr := intentHasRuntimeSpecRow(intentPath)
	if scanErr != nil {
		err = fmt.Errorf("verify supervisor-intent runtime_spec rows after write: %w", scanErr)
		return err
	}
	if specBearing {
		fmt.Fprintln(w, "Restarting the supervisor so the new runtime_spec intent is read by the current binary…")
		if rsErr := migrateSerenaRestartFn(ctx, w); rsErr != nil {
			// FAIL LOUD: a stuck old supervisor would ignore the new intent. Do
			// NOT report success.
			err = fmt.Errorf(
				"supervisor upgrade/restart gate (§7.1) failed after the runtime_spec intent write: %w; "+
					"the new serena dynamic-pool intent is committed but the supervisor that reads it was not "+
					"restarted — stop any running supervisor and re-run `mcphub install --upgrade` (or the migrate) "+
					"so the current binary reconciles the new intent", rsErr)
			return err
		}
	} else {
		fmt.Fprintln(w, "No registered serena workspaces — installed the dynamic-pool intent with zero daemon rows; no supervisor restart required.")
	}

	// 9. Emit the success audit event.
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
			},
		})
	}

	// 10. Success — clear the rollback so the deferred undo is a no-op.
	rollback = nil
	fmt.Fprintln(w, "serena dynamic-pool migration complete.")
	return nil
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
