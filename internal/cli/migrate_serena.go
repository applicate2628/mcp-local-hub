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
	"time"

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
var loadSerenaManifestForMigrateFn = func() (*config.ServerManifest, string, error) {
	raw, err := api.NewAPI().ManifestGet(serenaMigrateServerName)
	if err != nil {
		return nil, "", fmt.Errorf("load serena manifest: %w", err)
	}
	manifestHash := api.ManifestHashContent([]byte(raw))
	m, err := config.ParseManifest(strings.NewReader(raw))
	if err != nil {
		return nil, "", fmt.Errorf("parse serena manifest: %w", err)
	}
	return m, manifestHash, nil
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
	target, err := api.NewAPI().ResolveClientRoutingTarget()
	if err != nil {
		return nil, fmt.Errorf("resolve client routing target: %w", err)
	}
	pidportPath, err := gui.PidportPath()
	if err != nil {
		return nil, fmt.Errorf("resolve gui pidport path: %w", err)
	}
	return api.ReconcileSerenaClientsToRouter(ctx, api.SerenaReconcileOpts{
		RoutingTarget: &target,
		PidportPath:   pidportPath,
		ReadPidport:   gui.ReadPidport,
		VerifyIdentity: func(verifyCtx context.Context, pid, port int) error {
			return gui.VerifyGUIOwnerListener(verifyCtx, pidportPath, pid, port)
		},
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

// ErrMigrateSerenaReconcileReadyTimeout is the TYPED sentinel
// defaultMigrateSerenaStart wraps ONLY when the freshly-spawned supervisor was
// launched successfully (StartSupervisor returned nil) but did not report
// reconcile_ready=true over IPC within the bounded poll window. It distinguishes
// a benign post-commit readiness-poll timeout from a HARD start failure (the
// detached spawn itself failed) so the POST-COMMIT start (step 10) can downgrade
// the timeout to a warning while a real spawn failure still fails loud.
//
// Why the post-commit timeout is benign: by step 10 the dynamic-pool intent is
// already committed (rollback == nil) and the registry is intentionally NOT
// rolled back. The 30s/60s poll does NOT wait for daemon spawn — reconcile_ready
// flips true when the reconcile goroutine is SCHEDULED (supervise.go reconcile-
// ready event), not when daemons bind their ports. The single most common cause
// of the timeout is the known-benign release→child-acquire supervisor.lock
// hand-off window: the migrate releases the interlock immediately before the
// start, and the fresh supervisor must re-acquire the lock and bind its IPC pipe
// before `status` can answer — a DialPipe against a not-yet-bound pipe. The
// supervisor reconciles eventually regardless (and the GUI's
// ensureSupervisorRunning brings one up if the detached spawn lost a race), so
// the timeout cannot corrupt: there is nothing to roll back and no split-brain.
//
// SCOPE: this sentinel is honored ONLY at the post-commit start (step 10). Every
// pre-commit / recovery start site fails loud on ANY non-nil start error,
// including this one — they wrap it unconditionally and return, so they are
// unaffected by the existence of the sentinel.
var ErrMigrateSerenaReconcileReadyTimeout = errors.New("serena migrate: supervisor started but did not report reconcile-ready within the bounded window")

// migrateSerenaReconcileReadyTimeout bounds how long the Windows START driver
// (defaultMigrateSerenaStart) waits for the freshly-started supervisor to reach
// reconcile_ready=true via IPC `status`. Set to 60s as defence-in-depth against
// the known-benign release→child-acquire supervisor.lock hand-off window (the
// migrate releases the interlock immediately before the start, so the fresh
// supervisor must re-acquire the lock and bind its IPC pipe before `status` can
// answer). The poll's cost is ~constant in pool size (it polls one IPC endpoint,
// not per daemon), so widening the window is cheap. Even on a timeout the
// POST-COMMIT start (step 10) downgrades to a warning rather than a scary exit-1
// (ErrMigrateSerenaReconcileReadyTimeout), but the wider window makes that
// downgrade rarely necessary. The serena_auto_register.go reconcile-ready poll
// (see serena_auto_register.go:647) uses a 30s budget; this cutover path widens
// it to 60s to absorb the interlock release→re-acquire→IPC-bind hand-off.
//
// Defined here (cross-platform) — NOT under //go:build windows — because the
// driver's POST-COMMIT downgrade messaging in this file (step 10) references it,
// and that code compiles on every GOOS (Linux is shipping beta scope). The
// Windows-only START driver consumes it via waitReconcileReadyViaIPC.
const migrateSerenaReconcileReadyTimeout = 60 * time.Second

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

// migrateSerenaStartSupportedFn reports whether the platform's supervisor START
// primitive is wired (bot PR #250 finding #3 preflight). The detached
// supervisor spawn (defaultMigrateSerenaStart) is Windows-only production wiring
// in v0.5.0 (release scope: Windows GA / Linux beta / macOS preview); on other
// platforms it fails loud. When a cutover WILL require a start (willStart) AND
// this reports false, the driver FAILS LOUD BEFORE the intent write / client
// rewrite — refusing to commit an intent the platform cannot bring live (which
// would otherwise leave a committed intent + rewritten clients with no
// supervisor: a worse half-state than failing upfront). Default is the
// per-platform binding (defaultMigrateSerenaStartSupported: true on Windows,
// false elsewhere); tests override it to drive the preflight without a real
// platform split.
var migrateSerenaStartSupportedFn = defaultMigrateSerenaStartSupported

// acquireSupervisorInterlockFn acquires the supervisor.lock interlock the migrate
// HOLDS across its reap→write→start critical section (Phase 2 of
// .plans/2026-06/plan-serena-lock-interlock-2026-06-09.md). While held, no other
// actor can ACQUIRE supervisor.lock, so no foreign supervisor can START
// (every supervisor-starter calls api.AcquireSupervisorLock and fails fast on a
// held lock → its child exits) AND no concurrent serena auto-register cutover can
// force-kill the migrate's lock-holding process (Revision 2 / Starter A — the two
// reaping flows are mutually exclusive). It returns the live lock HANDLE (so the
// caller can mint the typed §7.1 bypass token via AllowSpecBearingWriteBypass) and
// an idempotent release closure. A non-nil error means a foreign supervisor or
// another serena cutover already holds the lock → the migrate fails loud (acquire
// is post-reap-PRE-write, so the intent is not yet committed; §2b).
//
// Default is the per-platform binding (defaultAcquireSupervisorInterlock): on
// Windows it acquires the real lock on api.DaemonStateDir()'s supervisor.lock leaf
// — the gate's EXACT resolver — so the held-lock path equals the §7.1 gate-probed
// path; on non-Windows it is a no-op (nil handle + no-op release + nil error),
// since the spec-bearing path is unreachable there (the reap stub fails loud
// first). Tests override it to drive a concurrent acquirer deterministically.
var acquireSupervisorInterlockFn = defaultAcquireSupervisorInterlock

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
type migrateSerenaRunDeps struct {
	isAppliedInstallError  func(error) bool
	acquireInitialRegistry func(*api.Registry) (func() error, error)
}

func runMigrateSerenaDynamicPool(ctx context.Context, w io.Writer) error {
	return runMigrateSerenaDynamicPoolWithDeps(ctx, w, migrateSerenaRunDeps{
		isAppliedInstallError: api.IsAppliedLockReleaseUnconfirmed,
		acquireInitialRegistry: func(reg *api.Registry) (func() error, error) {
			return reg.Lock()
		},
	})
}

func runMigrateSerenaDynamicPoolWithDeps(ctx context.Context, w io.Writer, deps migrateSerenaRunDeps) (err error) {
	if deps.isAppliedInstallError == nil {
		deps.isAppliedInstallError = api.IsAppliedLockReleaseUnconfirmed
	}
	if deps.acquireInitialRegistry == nil {
		deps.acquireInitialRegistry = func(reg *api.Registry) (func() error, error) {
			return reg.Lock()
		}
	}
	// 1. Load + parse the serena manifest; detect source state. The manifest
	//    is READ ONLY — it is the catalog input to the in-memory builder and
	//    the source-state classifier. It is never written.
	src, manifestHash, err := loadSerenaManifestForMigrateFn()
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
		// The committed intent is ALREADY dynamic-pool. Before declaring a no-op,
		// reconcile the intent against the CURRENT serena registry (finding #2 —
		// bot PR #250). A `mcphub workspace register --backend serena` that lands
		// AFTER the initial cutover updates only the registry (register saves
		// workspaces.yaml; the supervisor reconciles from supervisor-intent.json),
		// so the new workspace is router-resolvable but has no daemon row and is
		// never spawned. Re-running the migrate is the natural fan-out path, but an
		// unconditional already-migrated no-op blocks it.
		//
		//   - DRIFT (a current serena registry workspace missing from the intent)
		//     → do NOT no-op: fall through to the main migration flow, which re-runs
		//     the install over the CURRENT registry (build manifest →
		//     reReadAndAllocateSerenaForInstall → InstallParsedManifest) and
		//     reaps/starts as needed so the missing workspace lands in the intent
		//     and comes live.
		//   - NO DRIFT (every current serena workspace already has an intent row) →
		//     the Fix 5 idempotency branch below: GENUINE no-op if the supervisor is
		//     healthy, recovery-start otherwise.
		drift, driftErr := serenaIntentRegistryDrift()
		if driftErr != nil {
			return fmt.Errorf("reconcile committed serena intent against the registry before the already-migrated no-op: %w", driftErr)
		}
		if !drift {
			// Fix 5 (PR #250 deeper review — consultant Q2): "already migrated by
			// runtime, no drift" is NOT unconditionally a no-op. Two sub-cases:
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
		// DRIFT: a current serena registry workspace is missing from the committed
		// intent. Fall through (do NOT return) to the main migration flow to
		// re-fan-out the install over the current registry. The §7.1 reap gate means
		// a running supervisor is reaped before the spec-bearing re-write, then
		// restarted so it reconciles the intent that now covers the drifted
		// workspace.
		fmt.Fprintln(w, "serena is already migrated to the dynamic pool, but the registry has a serena workspace not present in the committed intent (drift — a workspace registered after the initial cutover) — re-fanning out the install over the current registry to cover it…")
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
	unlock, err := deps.acquireInitialRegistry(reg)
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
	var regReleaseErr error
	releaseRegistryLock := func() error {
		if regLockReleased {
			return regReleaseErr
		}
		regLockReleased = true
		if releaseErr := unlock(); releaseErr != nil {
			regReleaseErr = releaseErr
		}
		return regReleaseErr
	}
	defer func() {
		if !regLockReleased {
			if deferredReleaseErr := releaseRegistryLock(); deferredReleaseErr != nil {
				err = errors.Join(err, fmt.Errorf("migrate serena: migration outcome above stands, but could not release the registry lock: %w", deferredReleaseErr))
			}
		}
	}()
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
	// The first-allocation rows are ALL in serenaBefore (snapshotted just above),
	// so the snapshot rollback (restoreSerenaRegistryRelocking) already reverts
	// them; the newly-allocated set is needed only for the re-read path (finding
	// #2), whose rows are NOT in serenaBefore. Discard it here.
	if _, allocErr := allocateSerenaMigratePorts(reg, dynamicManifest); allocErr != nil {
		return allocErr
	}
	if err := reg.Save(); err != nil {
		// Save is atomic (tempfile + rename): a failure leaves the prior
		// on-disk registry intact, so there is nothing to roll back. The
		// deferred releaseRegistryLock frees the still-held lock. Crucially we
		// do NOT push the relocking undo here — it would deadlock trying to
		// re-acquire the lock this same goroutine still holds.
		return err
	}
	// The registry mutation is committed on disk. Release it BEFORE arming the
	// relocking undo: an unconfirmed release poisons this leaf, so a deferred
	// restore would otherwise re-acquire our own retained handle.
	// Nothing after this point (reconcile, reap,
	// intent write, start) touches the registry, so holding the lock across the
	// multi-second reap would only block concurrent registry ops. Ordering is
	// load-bearing: releaseRegistryLock MUST run before any deferred rollback
	// fires (it does — the explicit call below executes during the function
	// body, the deferred rollback runs after the body returns).
	if releaseErr := settleMigrateRegistryRelease(releaseRegistryLock, func() {
		// Only a confirmed release makes later rollback physically possible.
		rollback = append(rollback, func() error {
			return restoreSerenaRegistryRelocking(regPath, serenaBefore)
		})
	}); releaseErr != nil {
		return releaseErr
	}

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

	// PREFLIGHT (finding #3 — bot PR #250): if a cutover WILL require a supervisor
	// start (willStart) but the platform's start primitive is NOT wired (non-Windows
	// in v0.5.0), FAIL LOUD NOW — BEFORE the reconcile (client rewrite), the reap, or
	// the intent write. Round-4 made willReap false on non-Windows when no supervisor
	// is running (skip the unsupported reap stub), but willStart stayed true, so the
	// path reached the non-Windows START stub only AFTER InstallParsedManifest had
	// already committed the intent + rewritten the clients — leaving a committed
	// intent the platform can never bring live (a worse half-state than failing
	// upfront). Failing here, before any commit/client rewrite, keeps legacy fully
	// untouched. (A Windows host reports supported and is unaffected.) A second,
	// post-re-read copy of this guard (below) covers the finding-#1 window where the
	// first snapshot was empty but a workspace registers during the unlocked window.
	if willStart && !migrateSerenaStartSupportedFn() {
		return errMigrateSerenaStartUnsupportedPreflight()
	}

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

	// Interlock plumbing (Phase 2 / Revision 3 + bot PR #276 finding 2). The
	// supervisor.lock interlock is HELD across reap→write→start so no foreign
	// supervisor can start and no concurrent serena auto-register cutover can
	// force-kill THIS process. Per Revision 3 the release is armed ONLY at the
	// moment of a successful acquire — NOT at function entry — because the start
	// sites BEFORE any acquire (the early recovery-start at the top of the function,
	// which returns before this point) never held the interlock and must not
	// release a never-acquired lock. Until armed, releaseInterlock is a no-op; every
	// start site calls it (the pre-acquire ones are harmless no-ops), keeping one
	// release discipline. The acquire swaps in the real idempotent release (the
	// underlying (*SupervisorLock).Release nils its flock, so a second call is a
	// no-op too). Mirrors releaseRegistryLock's idempotent-closure shape.
	//
	// finding 2 (bot PR #276): the acquire was historically deferred to step 7e —
	// AFTER the step-7 reap, the registry re-read, the start-supported re-check, and
	// the late-reap decision — leaving an UNLOCKED post-reap gap in which a foreign
	// supervisor could take supervisor.lock and beat the migrate to it. The fix is
	// to acquire IMMEDIATELY after each successful reap (and, when there is no reap
	// at all, at the step-7e spec-bearing-write boundary), via acquireInterlockOnce
	// below, so the lock covers the whole reap→write window with no gap.
	var interlockBypass api.InstallParsedManifestBypass
	releaseInterlock := func() {}
	interlockHeld := false
	// acquireInterlockOnce acquires the supervisor.lock interlock the FIRST time it
	// is called (subsequent calls are no-ops — the lock is a singleton held across
	// the critical section) and, on success, arms releaseInterlock + mints the §7.1
	// bypass token from the held handle. It is invoked immediately after each reap
	// (finding 2) and at the no-reap spec-bearing boundary, so the lock is taken at
	// the earliest post-reap instant rather than after the post-reap work. A non-nil
	// return is the fail-loud signal (a foreign supervisor / concurrent cutover won
	// the window); each call site drives the willReap recovery-start and returns.
	acquireInterlockOnce := func() error {
		if interlockHeld {
			return nil
		}
		lock, release, acqErr := acquireSupervisorInterlockFn()
		if acqErr != nil {
			return acqErr
		}
		interlockHeld = true
		releaseInterlock = release
		interlockBypass = lock.AllowSpecBearingWriteBypass()
		return nil
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
		// finding 2 (bot PR #276): acquire the interlock IMMEDIATELY after the reap,
		// before the registry re-read / start-supported re-check / late-reap below,
		// so no foreign supervisor can take supervisor.lock in the post-reap gap. The
		// quiet acquire does NOT overwrite the owner sidecar (it still names the
		// just-reaped old supervisor), so any later reader targets the right PID. On
		// acquire-FAIL we already reaped → drive the recovery start (restore the
		// still-on-disk legacy intent) and fail loud; the deferred outer stack
		// restores the reconcile + registry.
		if acqErr := acquireInterlockOnce(); acqErr != nil {
			err = fmt.Errorf(
				"acquire the supervisor.lock interlock immediately after the reap: %w; "+
					"a supervisor (or another serena cutover) started during the migrate window and now holds supervisor.lock — "+
					"the new dynamic-pool intent was NOT written and legacy serena is untouched. "+
					"Wait for it to settle (`mcphub status`) and re-run the migrate", acqErr)
			releaseInterlock() // no-op (acquire failed, nothing held)
			fmt.Fprintln(w, "could not acquire the supervisor.lock interlock after the reap — restarting a supervisor to restore the prior (legacy) intent…")
			if startErr := migrateSerenaStartFn(ctx, w); startErr != nil {
				err = fmt.Errorf("%w; AND the recovery supervisor start ALSO failed: %v — "+
					"NO supervisor is running and the prior (legacy) intent is on disk; "+
					"run `mcphub supervise` from a shell to restore the legacy serena daemons", err, startErr)
			}
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
	installWorkspaces, reReadNewlyAllocated, reReadErr := reReadAndAllocateSerenaForInstall(regPath, dynamicManifest)
	if reReadErr != nil {
		err = fmt.Errorf("re-read registry under a re-acquired lock before the intent write: %w", reReadErr)
		if willReap {
			// Release the interlock before the recovery start (no-op here: the
			// interlock is acquired AFTER step 7d, so this pre-acquire path never
			// held it — the call keeps one release discipline). The recovered
			// supervisor must AcquireSupervisorLock itself.
			releaseInterlock()
			fmt.Fprintln(w, "registry re-read failed after the supervisor reap — restarting a supervisor to restore the prior (legacy) intent…")
			if startErr := migrateSerenaStartFn(ctx, w); startErr != nil {
				err = fmt.Errorf("%w; AND the recovery supervisor start ALSO failed: %v — "+
					"NO supervisor is running and the prior (legacy) intent is on disk; "+
					"run `mcphub supervise` from a shell to restore the legacy serena daemons", err, startErr)
			}
		}
		return err
	}
	// Push the finding-#2 surgical undo for the ports the re-read just allocated to
	// the concurrently-added serena rows (NEW rows the snapshot rollback leaves
	// untouched). On any PRE-COMMIT abort below (the second start-supported
	// preflight, the late reap, or the §7.1 install gate), this reverts exactly
	// those rows to their pre-re-read state so the registry/router is never left
	// pointing a workspace at a dead, un-spawned port. The instant the install
	// commits, the outer stack is disarmed (rollback = nil), so this undo never
	// runs once the committed intent owns those ports. revertSerenaReReadAllocations
	// is a no-op when the re-read allocated nothing (the common, no-concurrent-register
	// case), so the push is unconditional and harmless.
	rollback = append(rollback, func() error {
		return revertSerenaReReadAllocations(regPath, reReadNewlyAllocated)
	})

	// 7c. RECOMPUTE the start decision from the RE-READ install snapshot (finding
	//     #1 — bot PR #250). willStart was fixed from the FIRST snapshot
	//     (len(allocated)>0). But reReadAndAllocateSerenaForInstall above can pick
	//     up a workspace registered during the released-lock window and return a
	//     spec-bearing install snapshot even when the first snapshot was EMPTY. In
	//     that case the spec-bearing intent commits below but, with willStart fixed
	//     false, the post-commit start would be SKIPPED → clients on the /serena/mcp
	//     router, the spec-bearing intent on disk, but no supervisor started to
	//     spawn the newly-registered daemon. So re-evaluate willStart against the
	//     authoritative install snapshot: ≥1 workspace ⇒ a start is required.
	//     willStart only ever flips false→true here (the re-read can add rows that
	//     the first snapshot missed, never drop the candidates the first snapshot saw
	//     and the realloc preserved). willReap is NOT recomputed HERE — but the late
	//     reap in step 7d below DOES perform (and record) a reap when this recompute
	//     flips willStart true and a supervisor turns out to be running, because the
	//     first-snapshot-empty path skipped the willReap probe entirely. After 7d,
	//     willReap is again the HISTORICAL fact of whether a reap happened, which the
	//     recovery-message branches below key on.
	willStart = willStart || len(installWorkspaces) > 0

	// PREFLIGHT (finding #3 + #1 interaction): the recompute above can flip
	// willStart true when the first snapshot was empty (so Preflight A passed) but a
	// workspace registered in the window. On a platform whose start is unwired, that
	// would otherwise commit an intent below and then fail at the unwired start stub
	// — the exact half-state Preflight A prevents for the common case. Re-check here,
	// BEFORE the install commit. This fires only when Preflight A passed (hasWorkspaces
	// was false), so no reap ran (supervisorRunning is probed only when hasWorkspaces)
	// → willReap is false and no recovery start is owed; the deferred outer stack
	// restores the reconcile + registry, leaving legacy untouched and NO intent on disk.
	if willStart && !migrateSerenaStartSupportedFn() {
		err = errMigrateSerenaStartUnsupportedPreflight()
		return err
	}

	// 7d. LATE REAP (finding #1 — bot PR #250). The 7c recompute can flip willStart
	//     true when the FIRST snapshot was EMPTY (so the earlier willReap probe at
	//     step 5 was skipped — supervisorRunning is only probed when hasWorkspaces),
	//     but a workspace registered during the released-lock window made the re-read
	//     install snapshot spec-bearing. InstallParsedManifest's §7.1 gate refuses a
	//     spec-bearing write WHILE A SUPERVISOR IS RUNNING (an old binary's intent
	//     watcher uses DisallowUnknownFields and would reject runtime_spec). Without a
	//     reap here the cutover would FAIL at that gate EVEN ON WINDOWS — the supervisor
	//     was never reaped because the empty first snapshot never armed willReap. So
	//     re-probe liveness now and reap EXACTLY ONCE: only when a spec-bearing write is
	//     actually required (len(installWorkspaces) > 0), the earlier reap did NOT run
	//     (!willReap — the first-snapshot-NON-empty cutover already reaped above, and
	//     re-reaping would be a double-reap of an already-gone supervisor), and a
	//     supervisor is genuinely running. This runs AFTER the unsupported-start
	//     preflight (so an unwired platform fails loud BEFORE touching the live
	//     supervisor — legacy stays fully up) and BEFORE the install write (so the
	//     §7.1 gate then passes naturally). On success we set willReap = true so this
	//     late reap folds into the same historical fact the recovery-message and audit
	//     branches below key on (a post-write failure must drive the recovery start).
	//     A reap FAILURE here is fail-loud (§7.1 acceptance #2): the intent is NOT
	//     written and the deferred outer stack restores the reconcile + registry +
	//     the re-read allocations (legacy untouched); no recovery start is owed because
	//     no spec-bearing intent committed.
	if !willReap && len(installWorkspaces) > 0 {
		running, probeErr := migrateSerenaSupervisorRunningFn()
		if probeErr != nil {
			fmt.Fprintf(w, "warning: could not determine whether a supervisor is running before the spec-bearing intent write (%v); assuming one is and reaping it.\n", probeErr)
			running = true
		}
		if running {
			fmt.Fprintln(w, "NOTE: this cutover briefly takes serena OFFLINE while the supervisor is reaped and restarted (tens of seconds); clients reconnect once the new dynamic-pool daemons are reconcile-ready.")
			fmt.Fprintln(w, "Reaping the running supervisor before the runtime_spec intent write (a workspace registered during the released-lock window made this a spec-bearing cutover)…")
			if reapErr := migrateSerenaReapFn(ctx, w); reapErr != nil {
				err = fmt.Errorf(
					"supervisor reap (§7.1) failed BEFORE the runtime_spec intent write: %w; "+
						"the new serena dynamic-pool intent was NOT written and legacy serena is untouched — "+
						"stop any running supervisor and re-run the migrate", reapErr)
				return err
			}
			// The late reap succeeded: record it so the post-write recovery branches
			// (and the success audit) treat this run as having reaped a supervisor.
			willReap = true
			// finding 2 (bot PR #276): acquire the interlock IMMEDIATELY after this
			// late reap too — there is no post-reap work between here and the step-7e
			// acquire today, but acquiring here keeps the "lock the instant the reap
			// completes" invariant uniform across both reap sites (and is robust if
			// future work lands between this reap and the write). acquireInterlockOnce
			// no-ops if step 7 already took the lock (it never does on THIS branch —
			// this branch only runs when !willReap at step 7 — but the guard is cheap
			// and keeps the helper the single acquire authority).
			if acqErr := acquireInterlockOnce(); acqErr != nil {
				err = fmt.Errorf(
					"acquire the supervisor.lock interlock immediately after the late reap: %w; "+
						"a supervisor (or another serena cutover) started during the migrate window and now holds supervisor.lock — "+
						"the new dynamic-pool intent was NOT written and legacy serena is untouched. "+
						"Wait for it to settle (`mcphub status`) and re-run the migrate", acqErr)
				releaseInterlock() // no-op (acquire failed, nothing held)
				fmt.Fprintln(w, "could not acquire the supervisor.lock interlock after the late reap — restarting a supervisor to restore the prior (legacy) intent…")
				if startErr := migrateSerenaStartFn(ctx, w); startErr != nil {
					err = fmt.Errorf("%w; AND the recovery supervisor start ALSO failed: %v — "+
						"NO supervisor is running and the prior (legacy) intent is on disk; "+
						"run `mcphub supervise` from a shell to restore the legacy serena daemons", err, startErr)
				}
				return err
			}
		}
	}

	// 7e. ENSURE the supervisor.lock interlock is held before the step-8 spec-bearing
	//     write. In the reap paths above acquireInterlockOnce ALREADY took the lock
	//     immediately after the reap (finding 2 — closing the post-reap gap), so this
	//     is a no-op there. The remaining case is a spec-bearing cutover with NO reap
	//     (no supervisor was running, so neither step-7 nor step-7d reaped): the lock
	//     is taken HERE so the reap→write window's foreign-start exclusion still
	//     holds for the write. The lock is HELD across write→start. Holding it means
	//     (a) no foreign supervisor can START in the window — every starter calls
	//     api.AcquireSupervisorLock and fails fast on a held lock (supervise.go) → its
	//     child exits; AND (b) no concurrent serena auto-register cutover can read
	//     supervisor.lock.owner.json and force-kill the migrate (Revision 2 / Starter
	//     A — the two reaping flows are mutually exclusive; the QUIET acquire leaves
	//     the sidecar intact for whichever reap is in flight).
	//
	//     Acquire ONLY when this run will actually do a spec-bearing write that the
	//     §7.1 gate guards — i.e. there are install workspaces (the same condition
	//     the late reap + the post-commit start key on). A zero-workspace install
	//     writes a non-spec intent an old supervisor reads fine; it neither reaped
	//     nor needs the interlock. On non-Windows the binding is a no-op (nil
	//     handle); the reap stub already failed loud above, so this is unreachable
	//     in practice there.
	//
	//     acquireInterlockOnce arms the release + mints the typed §7.1 bypass token
	//     from the held handle (Revision 3) so the step-8 gate does not misread our
	//     OWN lock as a foreign supervisor. A nil handle (non-Windows no-op) yields a
	//     zero-value token (no bypass) — harmless because that path never reaches a
	//     real spec-bearing write.
	//
	//     ACQUIRE-FAIL is FAIL-LOUD (do NOT block): a foreign supervisor — or a
	//     concurrent serena auto-register/upgrade cutover — won the window and now
	//     holds supervisor.lock. The acquire is post-reap-PRE-write, so the intent
	//     is NOT yet committed and legacy serena is the reaped-but-restartable
	//     prior state; the deferred outer stack restores the reconcile + registry +
	//     re-read allocations, and — because willReap is already the historical
	//     fact of whether a reap ran — the recovery-start owed for our OWN reap is
	//     driven below via that same fact. The operator waits for the racer to
	//     settle and re-runs. (In practice this no-reap branch never set willReap, so
	//     no recovery start is owed; the willReap guard keeps one code shape.)
	if len(installWorkspaces) > 0 {
		if acqErr := acquireInterlockOnce(); acqErr != nil {
			// Release the interlock before any recovery start (no-op: acquire
			// failed, nothing held). If we reaped our own supervisor, restart one
			// from the still-on-disk OLD (legacy) intent so we never leave
			// no-supervisor-running; the deferred outer stack restores the rest.
			releaseInterlock()
			err = fmt.Errorf(
				"acquire the supervisor.lock interlock before the runtime_spec intent write: %w; "+
					"a supervisor (or another serena cutover) started during the migrate window and now holds supervisor.lock — "+
					"the new dynamic-pool intent was NOT written and legacy serena is untouched. "+
					"Wait for it to settle (`mcphub status`) and re-run the migrate", acqErr)
			if willReap {
				fmt.Fprintln(w, "could not acquire the supervisor.lock interlock after the reap — restarting a supervisor to restore the prior (legacy) intent…")
				if startErr := migrateSerenaStartFn(ctx, w); startErr != nil {
					err = fmt.Errorf("%w; AND the recovery supervisor start ALSO failed: %v — "+
						"NO supervisor is running and the prior (legacy) intent is on disk; "+
						"run `mcphub supervise` from a shell to restore the legacy serena daemons", err, startErr)
				}
			}
			return err
		}
	}

	// 8. Write the spec-bearing intent. The §7.1 gate inside InstallParsedManifest
	//    now passes NATURALLY — after the reap no supervisor holds the lock, so the
	//    spec-bearing write is the safe state (the NEXT supervisor start is THIS
	//    binary). The interlock we hold IS supervisor.lock, so the gate's probe
	//    would see it held; the minted bypass token (interlockBypass) tells the
	//    gate the held lock is our OWN handle (verified-identity check, Phase 1) so
	//    the spec-bearing write proceeds. The inner stack owns scheduler/client/
	//    intent undos; on error here those are ALREADY undone and the deferred
	//    outer stack restores the reconcile + registry. The dynamic-pool manifest
	//    carries no client_bindings (the /serena/mcp router owns routing), so the
	//    only side effect is the per-workspace supervisor-intent fan-out driven by
	//    opts.Workspaces (the finding-#2 re-read snapshot).
	intentPath, ierr := installParsedManifestFn(ctx, api.NewAPI(), dynamicManifest, api.InstallParsedManifestOpts{
		ManifestHash:         manifestHash,
		Writer:               w,
		Workspaces:           installWorkspaces,
		SupervisorLockBypass: interlockBypass,
	})
	var postCommitErr error
	if ierr != nil {
		if deps.isAppliedInstallError(ierr) {
			// The intent is durable. Retain the cause and enter the canonical
			// post-commit continuation below; willStart, verification, interlock
			// handoff, timeout/liveness policy, and audit projection stay single-owned.
			postCommitErr = ierr
		} else {
			// Recovery invariant: if we reaped (step 7) but the write failed, NO
			// supervisor is running and the OLD intent is still on disk. Restart a
			// supervisor so it reads the still-on-disk old intent (legacy restored)
			// rather than leaving no-supervisor-running silently. The outer stack
			// still restores the reconcile + registry (legacy is the source of truth).
			err = ierr
			if willReap {
				// Release the interlock before the recovery start — the started
				// supervisor must AcquireSupervisorLock itself (the held lock would
				// block it). Idempotent.
				releaseInterlock()
				fmt.Fprintln(w, "intent write failed after the supervisor reap — restarting a supervisor to restore the prior (legacy) intent…")
				if startErr := migrateSerenaStartFn(ctx, w); startErr != nil {
					err = fmt.Errorf("%w; AND the recovery supervisor start ALSO failed: %v — "+
						"NO supervisor is running and the prior (legacy) intent is on disk; "+
						"run `mcphub supervise` from a shell to restore the legacy serena daemons", err, startErr)
				}
			} else {
				// No reap (and thus no recovery start) on this path, but we may still
				// hold the interlock (acquired when installWorkspaces>0). Release it so
				// it never leaks past this error return.
				releaseInterlock()
			}
			return err
		}
	}

	// 9. DISARM the outer rollback (finding #2). InstallParsedManifest has
	//    committed the new intent — that IS the commit point. A later failure
	//    (the start below) must NOT roll the registry/reconcile back: doing so
	//    would leave the committed intent with daemon rows + ports while the
	//    registry reverts to port 0, so the /serena/mcp router could not resolve
	//    a workspace (split-state). Clear the stack so the deferred undo is a
	//    no-op from here on.
	rollback = nil

	// Wire the Revision 4 hand-off-window observer for the duration of the start
	// (both the scanErr-recovery start and the normal start below). The START
	// primitive calls migrateSerenaHandoffWindowFn when the benign
	// release→child-acquire window actually materializes (a >1-retry reconcile or a
	// duplicate-spawn singleton exit); we emit the named event through this run's
	// audit log so an operator can tell a known-benign window apart from a
	// recurrence of the original bare-IPC-timeout bug. Reset to the no-op on return
	// so a later stray call is inert. Best-effort: emit failure is non-fatal.
	migrateSerenaHandoffWindowFn = func(phase string) {
		emitSerenaInterlockHandoffWindowEvent(events, phase)
	}
	defer func() { migrateSerenaHandoffWindowFn = func(string) {} }()

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
		// Release the interlock before any start (the started supervisor must
		// AcquireSupervisorLock itself) and on the no-start branch too (avoid a
		// leak). Idempotent.
		releaseInterlock()
		if willStart {
			if willReap {
				fmt.Fprintln(w, "intent-verify re-read failed after the supervisor reap — starting the supervisor anyway so the committed intent is reconciled…")
			} else {
				fmt.Fprintln(w, "intent-verify re-read failed after the committed write — starting the supervisor so the committed intent is reconciled…")
			}
			if startErr := migrateSerenaStartFn(ctx, w); startErr != nil {
				err = fmt.Errorf("%w; AND the supervisor start ALSO failed: %w — "+
					"the new serena dynamic-pool intent is committed on disk but no supervisor is running; "+
					"run `mcphub supervise` from a shell so the current binary reconciles it", err, startErr)
			}
		} else {
			err = fmt.Errorf("%w — start the supervisor with `mcphub supervise` if it is not already running", err)
		}
		return errors.Join(postCommitErr, err)
	}
	// Release the interlock before the (normal or no-op) start. The just-started
	// supervisor must AcquireSupervisorLock itself; holding it here would block the
	// child's own acquire. Releasing right before the start opens the benign
	// release→child-acquire hand-off window (Revision 4): the intent is already
	// committed (the §7.1 write-blocking property is past), the singleton makes a
	// racing duplicate exit, an old-decoder winner self-crashes with nothing
	// re-spawning it, and the start's own reconcile-ready poll covers the pre-bind
	// pipe race. Idempotent (a later belt-and-suspenders release is a no-op).
	releaseInterlock()
	if willStart {
		fmt.Fprintln(w, "Starting the supervisor so it reconciles the new serena dynamic-pool intent…")
		if startErr := migrateSerenaStartFn(ctx, w); startErr != nil {
			// POST-COMMIT downgrade (ONLY at this site): a reconcile-ready-timeout
			// after a SUCCESSFUL spawn whose supervisor is DEMONSTRABLY STILL ALIVE
			// (lock-liveness gate below) is benign. The intent is committed and the
			// registry is intentionally NOT rolled back; the 30s/60s poll does not
			// wait for daemon spawn, and the supervisor reconciles eventually (the
			// GUI's ensureSupervisorRunning also brings one up). The most common
			// cause is the known-benign release→child-acquire supervisor.lock
			// hand-off window (DialPipe finds no pipe yet). Turning that into a
			// scary exit-1 falsely told operators the migration failed. Downgrade
			// to a warning and return nil — the migration IS committed.
			if errors.Is(startErr, ErrMigrateSerenaReconcileReadyTimeout) {
				// Bot PR #278 P1: the sentinel proves the detached SPAWN succeeded,
				// NOT that the spawned supervisor is still alive — it fires equally
				// when the child died right after the spawn (immediate exit, crash
				// before binding IPC). Downgrading THAT case would report exit-0
				// with a committed intent and NO supervisor running. Gate the
				// downgrade on the lightweight supervisor.lock liveness probe: in
				// the benign hand-off window the child HOLDS the lock while it
				// warms up; a dead child leaves it free. Conservative polarity here
				// is the OPPOSITE of the pre-reap probe (which assumes running on a
				// probe error so a needed reap is never skipped): an undeterminable
				// probe must NOT certify success → fail loud.
				//
				// The verdict is POINT-IN-TIME: a child that holds the lock at the
				// probe instant but dies right after still exits 0 — a bounded
				// residual window every spot liveness check has (the gate narrows
				// the false-success surface; it cannot close it). Self-healing: the
				// committed intent is durable, and the GUI's ensureSupervisorRunning
				// / the next reconcile brings a supervisor back up; the warning's
				// "run `mcphub status`" is the operator-side confirmation step.
				running, probeErr := migrateSerenaSupervisorRunningFn()
				if probeErr != nil || !running {
					liveness := "no process is holding supervisor.lock"
					if probeErr != nil {
						liveness = fmt.Sprintf("supervisor liveness probe failed: %v", probeErr)
					}
					hardStartErr := fmt.Errorf(
						"supervisor start (§7.1) failed after the runtime_spec intent was committed: %w (%s — "+
							"the spawned supervisor is not demonstrably alive, so this reconcile-ready timeout is NOT the benign lock hand-off window); "+
							"the new serena dynamic-pool intent is on disk but no supervisor is confirmed running — "+
							"run `mcphub supervise` from a shell (or re-run the migrate) so the current binary reconciles it. "+
							"The registry is intentionally NOT rolled back: the intent is the commit point", startErr, liveness)
					return errors.Join(postCommitErr, hardStartErr, probeErr)
				}
				fmt.Fprintf(w, "warning: serena dynamic-pool intent committed and a supervisor spawned, but it did not report reconcile-ready within %s — likely still binding its IPC pipe. Run \"mcphub status\" to confirm; the migration is committed (registry intentionally NOT rolled back).\n", migrateSerenaReconcileReadyTimeout)
				if events != nil {
					_ = events.Emit(api.SupervisorEvent{
						SchemaVersion: api.SupervisorEventSchemaVersion,
						Severity:      api.SupervisorEventSeverityWarn,
						Source:        "migration",
						Event:         "serena-migrate-post-commit-reconcile-ready-timeout",
						Body: map[string]any{
							"timeout":              migrateSerenaReconcileReadyTimeout.String(),
							"detail":               startErr.Error(),
							"reason":               "post-commit supervisor spawn succeeded but reconcile-ready was not observed within the bounded window; the intent is committed and the registry is NOT rolled back — the supervisor reconciles eventually",
							"action":               "run `mcphub status` to confirm the supervisor came up; no operator action is required if it is reconcile-ready",
							"committed":            true,
							"supervisor_lock_live": true,
						},
					})
				}
				// Fall through to the success audit event below — the migration
				// is committed; this is success-with-warning, exit 0.
			} else {
				// HARD start failure (the detached spawn itself failed) — FAIL
				// LOUD with guidance. Intent committed, registry NOT rolled back (#2).
				return errors.Join(postCommitErr, fmt.Errorf(
					"supervisor start (§7.1) failed after the runtime_spec intent was committed: %w; "+
						"the new serena dynamic-pool intent is on disk but no supervisor is running — "+
						"run `mcphub supervise` from a shell (or re-run the migrate) so the current binary reconciles it. "+
						"The registry is intentionally NOT rolled back: the intent is the commit point", startErr))
			}
		}
	} else {
		fmt.Fprintln(w, "No registered serena workspaces — installed the dynamic-pool intent with zero daemon rows; no supervisor reap/restart required.")
	}

	if postCommitErr != nil {
		return postCommitErr
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

// settleMigrateRegistryRelease is the migration's release-before-rollback
// boundary. The caller owns the one-shot release closure and rollback stack;
// this helper makes their ordering explicit and permits a local, deterministic
// test without adding a process-global lock hook.
func settleMigrateRegistryRelease(release func() error, armRollback func()) error {
	if releaseErr := release(); releaseErr != nil {
		return fmt.Errorf("migrate serena: registry allocations are committed, but could not release the registry lock before re-read, reconcile, reap, install, or start: %w", releaseErr)
	}
	armRollback()
	return nil
}

// serenaIntentRegistryDrift reports whether the current serena registry contains
// a workspace that the committed supervisor-intent.json does NOT cover with a
// daemon row (bot PR #250 finding #2). It exists for the runtime-already-migrated
// branch: a `mcphub workspace register --backend serena` that lands AFTER the
// initial cutover updates only the registry (register saves workspaces.yaml; the
// supervisor reconciles from supervisor-intent.json), so the new workspace is
// router-resolvable but has no daemon row and is never spawned. Re-running the
// migrate is the natural fan-out path, but the unconditional already-migrated
// no-op would block it. This predicate lets the driver distinguish a GENUINE
// no-op (every current serena workspace already has an intent row) from DRIFT (a
// registry serena workspace missing from the intent) so it can re-fan-out only on
// drift.
//
// It mirrors the install's own row filter (api.filterExistingWorkspaceRows /
// workspacePathStale, install_parsed_manifest.go): the install drops a serena
// workspace whose path no longer exists on disk (stale) BEFORE writing the
// intent, so a stale registry row absent from the intent is an INTENTIONAL skip,
// NOT drift — counting it as drift would force an endless re-fan-out loop (the
// install keeps dropping it; drift keeps re-detecting it). So only a serena row
// with a non-empty, non-stale path that is missing from the intent counts as
// drift. The intent join key is the canonical SerenaTaskNameForWorkspace the
// install writes (install_parsed_manifest.go:619).
//
// A missing intent file reports (false, nil) — there is nothing to drift against
// (and this predicate is only reached when serenaRuntimeIntentIsDynamicPool
// already reported the intent dynamic-pool, so the file exists in practice). Only
// a genuine read/parse error propagates.
func serenaIntentRegistryDrift() (bool, error) {
	stateDir, err := stateDirFunc()
	if err != nil {
		return false, fmt.Errorf("resolve state dir for serena intent/registry drift check: %w", err)
	}
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	intent, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	intentSerenaTasks := map[string]bool{}
	if intent != nil {
		for i := range intent.Daemons {
			if intent.Daemons[i].Server == serenaMigrateServerName {
				intentSerenaTasks[intent.Daemons[i].TaskName] = true
			}
		}
	}

	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		return false, fmt.Errorf("resolve registry path for serena intent/registry drift check: %w", err)
	}
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		return false, fmt.Errorf("load registry for serena intent/registry drift check: %w", err)
	}
	for _, e := range reg.SerenaEntries() {
		// SerenaEntries already filters to the @serena sentinel; additionally skip
		// rows the install itself would drop (empty/stale path) so a stale row
		// missing from the intent is not mistaken for drift.
		if e.WorkspacePath == "" || serenaMigrateWorkspacePathStale(e.WorkspacePath) {
			continue
		}
		if !intentSerenaTasks[api.SerenaTaskNameForWorkspace(e.WorkspacePath)] {
			return true, nil
		}
	}
	return false, nil
}

// serenaMigrateWorkspacePathStale mirrors api.workspacePathStale
// (internal/api/install_parsed_manifest.go:517), which is package-private to api:
// a non-empty path that does not exist on disk is stale. Kept byte-for-byte
// equivalent so the drift check (serenaIntentRegistryDrift) classifies a serena
// row exactly as the install fan-out's own stale filter does.
func serenaMigrateWorkspacePathStale(path string) bool {
	if path == "" {
		return false
	}
	if _, statErr := os.Stat(path); statErr != nil && os.IsNotExist(statErr) {
		return true
	}
	return false
}

// errMigrateSerenaStartUnsupportedPreflight is the finding-#3 preflight error
// returned when a cutover would require a supervisor start but the platform's
// start primitive is not wired. It names the v0.5.0 release scope so the operator
// runs the cutover on a Windows host. Both preflight points (pre-reconcile and
// post-re-read pre-install) return it so the operator sees identical guidance.
func errMigrateSerenaStartUnsupportedPreflight() error {
	return fmt.Errorf(
		"serena dynamic-pool cutover requires (re)starting the supervisor to bring the new daemons live, " +
			"but the supervisor start primitive is not wired on this platform (v0.5.0 ships the supervisor " +
			"spawn/restart flow on Windows only; Linux is beta and macOS preview) — refusing to commit a " +
			"supervisor-intent this platform cannot bring live. NO intent was written and legacy serena is " +
			"untouched; run the cutover on a Windows host")
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

// migrateSerenaHandoffWindowFn is the Revision 4 observability seam. After the
// migrate releases supervisor.lock immediately before starting the successor (the
// benign release→child-acquire hand-off window), the START primitive calls this
// when that window actually exercises its tolerance — the pre-bind pipe race
// materialized but resolved (waitReconcileReadyViaIPC needed >1 retry), or a
// duplicate-spawn singleton exit was observed. The driver wires it to an emit
// closure (over the run's audit log) around the start and resets it after; the
// default is a no-op so a stray call off the migrate path is inert. It is
// observability ONLY — never a gate; the driver's emit is best-effort.
var migrateSerenaHandoffWindowFn = func(phase string) {}

// emitSerenaInterlockHandoffWindowEvent writes the Revision 4
// `supervisor-interlock-handoff-window` info event to the supplied audit log
// (best-effort; a nil log or emit failure is silently non-fatal — mirrors
// emitWorkspaceAutoRegisteredEvent). phase is "reconcile-ready-retry" (the
// pre-bind pipe race resolved after >1 poll) or "duplicate-spawn-exit" (a racing
// duplicate supervisor exited via the singleton). The note documents WHY the
// window is known-benign so an operator can tell it apart from a recurrence of
// the original bare-IPC-timeout bug.
func emitSerenaInterlockHandoffWindowEvent(events *api.SupervisorEventLog, phase string) {
	if events == nil {
		return
	}
	_ = events.Emit(api.SupervisorEvent{
		SchemaVersion: api.SupervisorEventSchemaVersion,
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Severity:      api.SupervisorEventSeverityInfo,
		Source:        "migration",
		Event:         "supervisor-interlock-handoff-window",
		Body: map[string]any{
			"phase": phase,
			"note": "known-benign hand-off window: the migrate released supervisor.lock before " +
				"starting the successor; a racing duplicate exits via the singleton, an old-decoder " +
				"winner self-crashes, and reconcile-ready retries cover the pre-bind pipe race",
		},
	})
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
func restoreSerenaRegistryRelocking(regPath string, serenaSnapshot []api.WorkspaceEntry) (err error) {
	reg := api.NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		return fmt.Errorf("restore registry: re-acquire lock: %w", err)
	}
	defer api.ReleaseAndJoin(&err, unlock, "restore registry: restore above stands, but could not release the registry lock")
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
//
// It returns the PRIOR state (a copy of each row exactly as it stood BEFORE the
// allocation) of every serena row it newly assigned a port/task-name to (finding
// #2, bot PR #250). The re-read caller pushes a surgical undo that reverts those
// exact rows on a pre-commit abort, so a port allocated for a concurrently-added
// serena row during the released-lock window is not left dangling (the snapshot
// rollback restoreSerenaRegistryRelocking deliberately leaves non-snapshotted rows
// untouched, so it cannot revert them). Each returned entry carries the row's
// pre-allocation Port (always 0, since only port-less rows are assigned) and
// TaskName, so the undo restores the EXACT prior state.
func allocateSerenaMigratePorts(reg *api.Registry, dynamicManifest *config.ServerManifest) ([]api.WorkspaceEntry, error) {
	if dynamicManifest.DaemonTemplate == nil || dynamicManifest.DaemonTemplate.PortPool == nil {
		return nil, fmt.Errorf("internal: in-memory dynamic-pool manifest has no daemon_template.port_pool")
	}
	pool := *dynamicManifest.DaemonTemplate.PortPool
	var newlyAllocated []api.WorkspaceEntry
	for _, ws := range reg.SerenaEntries() {
		cur, ok := reg.Get(ws.WorkspaceKey, api.SerenaLanguageSentinel)
		if !ok {
			continue
		}
		if cur.Port > 0 {
			continue
		}
		// Snapshot the row's PRIOR state before mutating it, so the re-read
		// caller can surgically revert exactly this row on a pre-commit abort.
		newlyAllocated = append(newlyAllocated, cur)
		port, err := reg.AllocateSerenaPort(pool)
		if err != nil {
			return nil, fmt.Errorf("allocate serena port for workspace %s: %w", cur.WorkspacePath, err)
		}
		cur.Port = port
		if cur.TaskName == "" {
			cur.TaskName = api.SerenaTaskNameForWorkspace(cur.WorkspacePath)
		}
		if err := reg.PutSerena(cur); err != nil {
			return nil, fmt.Errorf("persist allocated port for workspace %s: %w", cur.WorkspacePath, err)
		}
	}
	return newlyAllocated, nil
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
// It ALSO returns reReadNewlyAllocated — the PRIOR state of every serena row this
// re-read newly assigned a port to (the concurrently-added port-less rows; the
// originally-allocated rows already carry a port and are skipped, so they are NOT
// in this set). The driver pushes a surgical undo over exactly these rows
// (revertSerenaReReadAllocations), so a PRE-COMMIT abort after this re-read
// (the second start-supported preflight, the late reap, or the §7.1 install gate)
// clears the re-read port/task-name from the concurrently-added row instead of
// leaving it pointing the registry/router at a dead port. The snapshot rollback
// (restoreSerenaRegistryRelocking) deliberately leaves non-snapshotted rows
// untouched (round-4 #1, to avoid clobbering concurrent registrations), so it
// cannot revert these; the two undos cover disjoint key sets (snapshot keys vs
// concurrently-added keys) and are independent.
//
// The lock is held ONLY across this fast reload/realloc/Save and released before
// returning (no leaked lock), so it never overlaps the slow reap. The returned
// slice is always non-nil (an empty registry yields an empty slice) because
// InstallParsedManifest requires a non-nil Workspaces snapshot.
func reReadAndAllocateSerenaForInstall(regPath string, dynamicManifest *config.ServerManifest) (installWorkspaces, reReadNewlyAllocated []api.WorkspaceEntry, err error) {
	reg := api.NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		return nil, nil, fmt.Errorf("re-acquire registry lock: %w", err)
	}
	defer api.ReleaseAndJoin(&err, unlock, "re-read serena rows: allocations above stand, but could not release the registry lock")
	if err := reg.Load(); err != nil {
		return nil, nil, fmt.Errorf("reload registry: %w", err)
	}
	newlyAllocated, err := allocateSerenaMigratePorts(reg, dynamicManifest)
	if err != nil {
		return nil, nil, err
	}
	if err := reg.Save(); err != nil {
		return nil, nil, fmt.Errorf("save registry after re-allocation: %w", err)
	}
	entries := reg.SerenaEntries()
	if entries == nil {
		entries = []api.WorkspaceEntry{}
	}
	return entries, newlyAllocated, nil
}

// revertSerenaReReadAllocations is the finding-#2 (bot PR #250) surgical undo for
// the port/task-name allocations reReadAndAllocateSerenaForInstall assigned to the
// concurrently-added serena rows. It re-acquires the registry lock, reloads the
// CURRENT on-disk registry, and restores each newly-allocated row to its PRIOR
// (pre-re-read) state via PutSerena — but ONLY when that workspace key still
// exists in the reloaded registry. A key that a concurrent
// `mcphub workspace unregister --backend serena` removed during the abort window
// is SKIPPED, not resurrected (the same dual guard restoreSerenaRegistryRelocking
// uses, round-5 #1). It touches ONLY the supplied (concurrently-added) keys and
// leaves the snapshotted rows to restoreSerenaRegistryRelocking, so the two undos
// never double-touch a row. The lock is always released before returning.
//
// It is pushed onto the OUTER rollback stack, so it runs ONLY on a pre-commit
// abort: the instant InstallParsedManifest commits, the driver clears the stack
// (rollback = nil), making this undo a no-op from the commit point on (the
// committed intent now owns those rows' ports — reverting them would strand the
// router).
func revertSerenaReReadAllocations(regPath string, newlyAllocated []api.WorkspaceEntry) (err error) {
	if len(newlyAllocated) == 0 {
		return nil
	}
	reg := api.NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		return fmt.Errorf("revert re-read allocations: re-acquire lock: %w", err)
	}
	defer api.ReleaseAndJoin(&err, unlock, "revert re-read allocations: revert above stands, but could not release the registry lock")
	if err := reg.Load(); err != nil {
		return fmt.Errorf("revert re-read allocations: reload: %w", err)
	}
	for _, prior := range newlyAllocated {
		if _, present := reg.GetSerena(prior.WorkspaceKey); !present {
			// Concurrently unregistered during the abort window — do not re-add a
			// row the user removed and the migrate no longer owns.
			continue
		}
		if err := reg.PutSerena(prior); err != nil {
			return fmt.Errorf("revert re-read allocations: restore prior serena row for workspace_key %s: %w", prior.WorkspaceKey, err)
		}
	}
	if err := reg.Save(); err != nil {
		return fmt.Errorf("revert re-read allocations: save: %w", err)
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
