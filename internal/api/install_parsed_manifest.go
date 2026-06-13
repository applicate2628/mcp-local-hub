// Package api - Phase D.3 in-process install seam for pre-parsed manifests.
//
// InstallParsedManifest is the WORKSPACE-SCOPED-ONLY sister to
// (*API).Install. Where Install loads a manifest by name and refuses
// workspace-scoped kinds, InstallParsedManifest accepts an already-parsed
// manifest (the caller owns parsing) and is restricted to
// kind: workspace-scoped manifests: it BYPASSES
// refuseWorkspaceScopedInstall (workspace-scoped dynamic-pool manifests
// are its intended input) but REJECTS global and any other non-workspace-
// scoped kind up front. Global manifests go through (*API).Install, which
// owns the per-daemon scheduler-task + immediate-start path this seam
// deliberately does not. The seam never starts daemons in-process — its
// per-workspace serena daemons start via the supervisor reconciler once it
// observes the new intent — so it always defers the start (there is no
// StartAfterWrite knob; the deferred-start contract is structural).
//
// It shares the materialization core (audit-first emission +
// executeInstallTo) with Install via the unexported installPlan helper,
// and folds the supervisor-intent.json write into executeInstallTo's
// rollback stack so scheduler tasks, per-client configs, and the intent
// write commit-or-roll-back as one unit.
//
// Plan ref: docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md D.3.
package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/autostart"
	"mcp-local-hub/internal/config"
)

// installAutostartBackendFactoryFn is the install-path seam for ensuring the
// supervisor logon owner after global supervisor-intent installs. Production
// uses the OS backend; tests inject a fake so no real Task Scheduler,
// systemd-user, or launchctl state is touched.
var installAutostartBackendFactoryFn = autostart.New
var installAutostartOwnerStartFn = autostart.StartOwner
var installSupervisorRunningProbeFn = SupervisorRunningUnderStateDir

// InstallParsedManifestOpts controls an InstallParsedManifest invocation.
//
// There is no StartAfterWrite field: this seam is workspace-scoped-only
// (a global manifest is rejected up front) and never starts daemons
// in-process. The per-workspace serena daemons start via the supervisor
// reconciler on its next tick once it observes the new intent, so the
// seam always passes StartTasks=false into the materialization core. The
// deferred-start contract is structural, not a runtime knob.
type InstallParsedManifestOpts struct {
	Writer            io.Writer
	ClientsInclude    []string
	IncludeAllClients bool
	// Snapshot of registered serena workspaces; consumed by the
	// per-workspace fan-out; populated by future callers (migrate redesign
	// / E.2 auto-register).
	Workspaces []WorkspaceEntry
	DryRun     bool
	// RequireWorkspaceKey, when non-empty, makes the install FAIL PRE-COMMIT
	// (before the supervisor-intent write) if the merged intent does not carry a
	// serena per-workspace daemon row for this key — i.e. buildMergedSupervisorIntent's
	// stale-row filter dropped it (its dir vanished between the caller's registry
	// Save and this build). serena auto-register-on-miss passes its triggering
	// workspace key here so a dropped workspace fails BEFORE the write, leaving the
	// prior intent intact on disk for the caller's recovery-restart (bot PR #253 r6
	// P2) — instead of committing an intent that drops it, which on the FIRST
	// introduce would also replace the legacy serena rows with nothing. Empty = no
	// requirement (legacy/migrate callers that legitimately install a filtered set).
	RequireWorkspaceKey string
	// SupervisorLockBypass, when carrying a non-nil lock that is STILL HELD and
	// whose path matches THIS install's gate (filepath.Join(stateDir,
	// "supervisor.lock")), authorizes the §7.1 spec-bearing write gate below to
	// SKIP its SupervisorRunningUnderStateDir probe — the caller has proven it is
	// the supervisor-lock holder, so the held lock is its own handle, not a
	// foreign (possibly-old) supervisor. The token is opaque and
	// constructor-enforced: only a holder of a real *SupervisorLock can mint one
	// via (*SupervisorLock).AllowSpecBearingWriteBypass(). The zero value (no
	// lock) is the default and preserves the original fail-closed behavior for
	// every existing call site. The migrate / serena auto-register flows (Phase
	// 2) acquire the lock around their reap+rewrite and pass the minted token
	// here. A nil, released, or path-mismatched token is treated as NO bypass
	// (the probe runs → fail-closed), folding the path-mismatch check into the
	// gate itself.
	SupervisorLockBypass InstallParsedManifestBypass
}

// InstallParsedManifest installs a pre-parsed manifest in-process and
// returns the absolute path of the supervisor-intent.json it wrote.
//
// Sequence:
//
//  1. Preflight (fail-fast, check-only, no mutation): LookPath of the daemon
//     command, ensureCanonicalMcphubPresent, and checkSecretRefs on m.Env —
//     the same gate the three legacy install paths run. A missing `secret:`
//     env var or a missing command fails HERE, before any intent or
//     scheduler mutation, instead of surfacing later at daemon start. Port
//     checks do not apply to a workspace-scoped DaemonTemplate manifest
//     (its m.Daemons is empty). Preflight is check-only so it runs on the
//     dry-run path too, matching api.Install (Preflight precedes its
//     dry-run short-circuit).
//  2. Pre-flight intent write (fail-fast, no mutation): dry-write the intent
//     file to a temp path via WriteStateFileAtomic. A failure here (disk
//     full, permission denied, parent-dir DACL gate refusal under
//     MCPHUB_REQUIRE_SINGLE_USER_HOME=1) returns BEFORE any other mutation
//     so the end-state is pristine. SKIPPED on dry-run — a dry run must not
//     touch disk.
//  3. BuildPlanWithOpts on the parsed manifest.
//  4. installPlan -> executeInstallTo: scheduler-task creation + per-client
//     config writes, then the supervisor-intent write as the intermediate
//     step INSIDE executeInstallTo's rollback scope. Pass B (immediate
//     daemon start) is always gated OFF — the seam defers every start to the
//     supervisor reconciler — so installPlan receives StartTasks=false. On
//     dry-run, installPlan prints the plan and returns; this function then
//     returns ("", nil) because nothing was written and there is no intent
//     path for the caller to dereference.
//
// The manifest MUST be kind: workspace-scoped with a daemon_template (a nil,
// global, or template-less manifest is rejected up front — those go through
// (*API).Install), and a non-dry install MUST pass a non-nil Workspaces
// snapshot (a nil snapshot would silently drop the server's existing daemon
// rows; pass an empty non-nil slice for an intentional zero-workspace install).
// On any sub-failure inside step 4, the shared rollback stack runs and the
// function returns the error with every side effect undone.
func (a *API) InstallParsedManifest(ctx context.Context, m *config.ServerManifest, opts InstallParsedManifestOpts) (intentPath string, err error) {
	w := opts.Writer
	if w == nil {
		w = os.Stderr
	}

	// 1. Contract validation — runs FIRST, before Preflight (which dereferences
	// m) and before any mutation. This seam installs ONLY kind: workspace-scoped
	// dynamic-pool manifests that carry a daemon_template: it materializes
	// supervisor-intent daemon rows and defers every daemon start to the
	// supervisor reconciler. A nil manifest, a global manifest, or a legacy
	// workspace-scoped manifest WITHOUT a daemon_template belongs to
	// (*API).Install, which owns the per-daemon scheduler-task + immediate-start
	// path. Rejecting here keeps such a manifest from silently taking a
	// deferred-start path that would never start its daemons, and moots the
	// per-server weekly-cadence machinery a global manifest would otherwise need
	// from this seam (only workspace-scoped serena flows reach it, and serena
	// sets weekly_refresh=false).
	if m == nil {
		return "", fmt.Errorf("InstallParsedManifest: nil manifest")
	}
	if m.Kind != config.KindWorkspaceScoped || m.DaemonTemplate == nil {
		return "", fmt.Errorf("InstallParsedManifest only installs kind=%q dynamic-pool manifests with a daemon_template (manifest %q is kind=%q, has daemon_template=%t); install global or legacy manifests through (*API).Install", config.KindWorkspaceScoped, m.Name, m.Kind, m.DaemonTemplate != nil)
	}
	// native-http gate (design §3.1). The serena-proxy no longer re-validates
	// transport at runtime (it reads a materialized RuntimeSpec, not the
	// manifest). A stdio-bridge + daemon_template + kind:workspace-scoped
	// manifest PASSES config.ServerManifest.Validate today (Validate rejects
	// daemon_template only for kind!=workspace-scoped or transport=remote-http
	// — internal/config/manifest.go), so this is the only thing stopping a
	// non-native-http dynamic-pool manifest from reaching the
	// HTTP-reverse-proxy spawn path. Reject BEFORE any mutation, same
	// fail-loud style as the kind+template gate above. Additive admission
	// tightening only — the write/rollback/deferred-start shape is unchanged.
	if m.Transport != config.TransportNativeHTTP {
		return "", fmt.Errorf("InstallParsedManifest only installs transport=%q dynamic-pool manifests (manifest %q is transport=%q); a daemon_template manifest spawns an HTTP reverse-proxy, which requires native-http", config.TransportNativeHTTP, m.Name, m.Transport)
	}
	// Empty-context gate (bot PR #246 P2). The per-workspace fan-out
	// (BuildSupervisorDaemonsForSerena → buildSerenaChildArgs, design §5)
	// APPENDS `--context <DaemonTemplate.Context>` into every materialized
	// RuntimeSpec.ChildArgs. config.ServerManifest.Validate does NOT check
	// Context (only port_pool + extra_args_template — internal/config/manifest.go),
	// so a manifest with an absent/blank daemon_template.context PASSES Validate
	// but would materialize `--context ""` and the supervisor would respawn a
	// serena child with an invalid empty context. Reject BEFORE any mutation,
	// same fail-loud style as the kind+template+transport gates above. This is
	// the install-time mirror of the build-time skip in
	// BuildSupervisorDaemonsForSerena. Additive admission tightening only.
	if strings.TrimSpace(m.DaemonTemplate.Context) == "" {
		return "", fmt.Errorf("InstallParsedManifest: manifest %q has an empty daemon_template.context; the per-workspace serena proxy materializes --context <value> for the child, so a non-empty context is required (set daemon_template.context in the manifest)", m.Name)
	}

	// Duplicate-context gate (bot PR #246 r2 P2). The fan-out APPENDS
	// `--context <DaemonTemplate.Context>`, so a --context token already in
	// base_args / extra_args_template would double the flag. FAIL LOUD here,
	// before any mutation: BuildSupervisorDaemonsForSerena returns nil for this
	// shape (defense-in-depth), but a nil fan-out is merged as "this server has
	// no daemons" and would SILENTLY remove the server's existing per-workspace
	// rows (bot finding). config.ServerManifest.Validate also rejects it, but this
	// seam accepts a PRE-PARSED manifest that may not have been revalidated.
	if config.ArgsContainContextFlag(m.BaseArgs) || config.ArgsContainContextFlag(m.DaemonTemplate.ExtraArgsTemplate) {
		return "", fmt.Errorf("InstallParsedManifest: manifest %q places --context in base_args or extra_args_template; the per-workspace serena proxy appends --context <daemon_template.context> at spawn, so a token here would duplicate the flag — remove it (context comes solely from daemon_template.context)", m.Name)
	}

	// 1a. Preflight: check-only gate shared with the legacy install paths.
	// Before the pre-flight intent write and before any mutation, so a missing
	// command or unresolved secret: ref fails fast.
	if err := Preflight(m, ""); err != nil {
		return "", fmt.Errorf("preflight: %w", err)
	}

	// 1b. Dry-run short-circuit — runs BEFORE the state-dir resolve, the
	// intent flock, and the read-merge. A dry run must print the plan and make
	// ZERO disk changes: it must not create/touch supervisor-intent.json.lock,
	// must not read or parse the existing supervisor-intent.json (so a corrupt
	// or unreadable existing intent never fails a dry run), and must not touch
	// the state dir. Mirrors api.Install, whose dry-run short-circuit also
	// precedes any intent/scheduler I/O. We build the plan and route it through
	// installPlan with DryRun=true (which prints via printPlanTo and returns
	// before any mutation or the intermediate hook), then return an empty path
	// — nothing was committed, so the caller must not dereference a path.
	if opts.DryRun {
		plan, err := BuildPlanWithOpts(m, BuildPlanOpts{
			ClientsInclude:    opts.ClientsInclude,
			IncludeAllClients: opts.IncludeAllClients,
		})
		if err != nil {
			return "", err
		}
		if err := a.installPlan(ctx, m, plan, installPlanOpts{Writer: w, DryRun: true}); err != nil {
			return "", err
		}
		// The legacy plan above carries ZERO scheduler tasks for a
		// DaemonTemplate manifest, so for the MAIN fan-out case it reports no
		// planned changes. Print a preview of the per-workspace
		// supervisor-intent daemon rows the real (non-dry) path would write —
		// one line per workspace. This preview MUST stay corrupt-safe: it does
		// NOT read/parse the existing supervisor-intent.json, acquires no
		// flock, writes no file, and emits no supervisor-events.log entry. The
		// stale label reuses the SAME pure liveness predicate the non-dry path
		// uses (workspacePathStale) but emits nothing here — label only.
		previewWorkspaceFanOut(w, m, opts.Workspaces)
		return "", nil
	}

	// 1c. Non-dry install requires the workspace snapshot. A nil snapshot would
	// fall through serenaOrPlanDaemons to the plan-derived (nil) daemon set and
	// SILENTLY DROP every existing supervisor-intent daemon row for this server
	// (buildMergedSupervisorIntent replaces m's rows with whatever
	// serenaOrPlanDaemons returns). Distinguish a forgotten snapshot (nil →
	// error) from an intentional zero-workspace install (empty non-nil slice →
	// proceed, legitimately clearing m's rows). Dry runs are exempt: they write
	// nothing, so a nil snapshot there is non-destructive.
	if opts.Workspaces == nil {
		return "", fmt.Errorf("InstallParsedManifest: a non-nil Workspaces snapshot is required for a non-dry install of %q (pass an empty non-nil slice to install with zero registered workspaces; a nil snapshot would drop the server's existing supervisor-intent daemon rows)", m.Name)
	}

	stateDir, err := DaemonStateDir()
	if err != nil {
		return "", fmt.Errorf("resolve state dir: %w", err)
	}
	intentPath = joinStateFilePath(stateDir, supervisorIntentFileLeaf)

	// Capture live PIDs before taking the supervisor-intent flock. This is a
	// preliminary superset: the lock-held merge below recomputes the
	// authoritative removed set, and any row that appeared after this pre-read
	// simply has no captured PID and falls back to the port classifier.
	ownershipScope := supervisorIntentOwnershipScopeForManifest(m, opts.Workspaces, "")
	preCaptureTargets, err := preliminarySupervisorTargetsForServerScope(intentPath, m.Name, ownershipScope)
	if err != nil {
		return "", err
	}
	removedSupervisorPIDByTask := supervisorOwnedLivePIDsForTargets(ctx, preCaptureTargets)

	// FIX 1 — atomic read-merge-write of supervisor-intent.json.
	//
	// The read (buildMergedSupervisorIntent) → merge → write sequence must be
	// serialized as ONE critical section against every other supervisor-intent
	// writer; otherwise two concurrent InstallParsedManifest calls for different
	// servers each read a stale snapshot, each merge in only THEIR rows, and the
	// later writer clobbers the earlier writer's sibling-server daemon rows.
	//
	// We acquire the canonical per-file flock (supervisor-intent.json.lock — the
	// same `<path>.lock` leaf WriteStateFileAtomic uses, so this also serializes
	// against migration / autostart / post-success intent writers across the
	// process AND across processes) and hold it across the read-merge AND the
	// commit inside executeInstallTo's intermediate hook. Because we already
	// hold the lock, the inner commit + rollback restore use the LOCK-FREE
	// secure-write body (writeSupervisorIntentLockHeld) — re-entering
	// WriteSupervisorIntent (which re-acquires the same flock) would deadlock,
	// exactly the readIntentLocked/writeIntentLocked split daemon_intent.go uses.
	lock := flock.New(intentPath + supervisorIntentLockSuffix)
	if err := lock.Lock(); err != nil {
		return "", fmt.Errorf("supervisor-intent flock %s: %w", intentPath+supervisorIntentLockSuffix, err)
	}
	defer func() { _ = lock.Unlock() }()

	// Build the intent file we intend to write up front (under the held lock)
	// so the pre-flight dry-write exercises the same payload and the same
	// secure-write pipeline the real write will use. priorIntent + priorExisted
	// drive the rollback restore.
	// InstallParsedManifest is the workspace-scoped / serena path and always
	// installs the full set (no per-daemon filter), so pass "" — and serena's
	// manifest has weekly_refresh=false, so mergeServerWeeklyRefreshTimer's
	// materialization condition never fires here (prior timers carry verbatim).
	desiredIntent, priorIntent, priorExisted, err := a.buildMergedSupervisorIntent(m, intentPath, opts.Workspaces, "", w)
	if err != nil {
		return "", err
	}
	removedSupervisorTargetsAfterInstall := removedSupervisorTargetsForServerMergeScope(priorIntent, desiredIntent, m.Name, ownershipScope)
	desiredIntent.Stops = pruneStopsForRemovedSupervisorTargets(desiredIntent.Stops, removedSupervisorTargetsAfterInstall)

	// Required-workspace pre-commit guard (bot PR #253 r6 P2). A caller that
	// auto-registers a SPECIFIC triggering workspace passes opts.RequireWorkspaceKey.
	// If buildMergedSupervisorIntent's stale-row filter dropped that workspace (its
	// dir vanished between the caller's registry Save and this build), committing
	// would write an intent MISSING the triggering daemon — and on the FIRST introduce
	// also REPLACE the legacy serena rows with nothing, so the caller's recovery-restart
	// would reconcile an emptied intent. Fail HERE, before the §7.1 gate and the intent
	// write, so the prior intent stays intact on disk and the recovery-restart restores
	// it. This is the pre-commit form the bot asked for (vs a post-commit restore). Empty
	// key = no requirement.
	if opts.RequireWorkspaceKey != "" && !desiredIntent.HasSerenaDaemonForWorkspaceKey(opts.RequireWorkspaceKey) {
		return "", fmt.Errorf("InstallParsedManifest: required workspace key %q is not present in the merged supervisor intent for %q (its directory may have been removed mid-install); refusing to commit an intent that drops it", opts.RequireWorkspaceKey, m.Name)
	}

	// §7.1 spec-bearing supervisor-intent write gate (bot PR #246 r2 P2).
	// desiredIntent may carry runtime_spec rows (materializeSerenaRuntimeSpec).
	// An ALREADY-RUNNING supervisor MAY be an OLD binary whose
	// ReadSupervisorIntent uses DisallowUnknownFields: its watcher would reject a
	// file carrying the new runtime_spec field, keep its stale in-memory intent,
	// and leave split-brain (this new CLI believes the descriptor was
	// re-materialized; the old supervisor never sees it). The only safe state for
	// a spec-bearing write is NO supervisor running — the NEXT supervisor start is
	// THIS binary, which reads runtime_spec. So this Phase-1 gate refuses while a
	// supervisor is running OR while liveness is undeterminable (FAIL CLOSED,
	// consultant PR #246 r2 #1 — assuming "not running" on a probe error would
	// silently disable the gate on hardened hosts). Non-spec installs (no
	// runtime_spec rows) are NOT gated — an old supervisor reads legacy/global
	// rows fine. The cutover phase (design §7.1 / Phase 4) UPGRADES this refuse to
	// an automatic drive of the cold-restart flow; until then the operator must
	// STOP the running supervisor (NOT merely `install --upgrade`, which leaves a
	// supervisor running — the any-running gate would still refuse). The check
	// runs under the held supervisor-intent flock, immediately before the
	// preflight + commit; the supervisor lock is a distinct primitive, so an old
	// binary cold-starting in the tiny probe→commit window is an extreme residual
	// edge — but it self-heals: a fresh old binary decoding the new file fails
	// LOUD at supervisor cold start (supervise.go intent-decode hard abort), not
	// silently.
	//
	// Phase-5 live-add refinement (auto-register-on-miss): the gate fires ONLY
	// when this write INTRODUCES runtime_spec — i.e. desiredIntent carries it but
	// the prior on-disk intent does NOT. A write that ADDS a workspace to an
	// intent that ALREADY carries runtime_spec is safe even while a supervisor
	// runs, because that running supervisor is PROVABLY the new binary: the only
	// path to a runtime_spec on-disk intent is the Phase-4 cutover, which reaps
	// the old supervisor BEFORE the first runtime_spec write (this very gate
	// blocks writing runtime_spec while an old supervisor runs). So
	// priorIntent.HasRuntimeSpecRow() ⟹ the cutover already happened ⟹ any
	// running supervisor understands runtime_spec ⟹ a live-add cannot split-brain.
	// (Verified airtight against every runtime_spec writer: InstallParsedManifest
	// is gated here; migrate reaps first; strict-mode only PRESERVES, never
	// INTRODUCES, runtime_spec; the supervisor reconciler never writes it back.)
	// The introduction case keeps the original FAIL-CLOSED behavior (refuse on
	// running OR undeterminable liveness).
	if desiredIntent.HasRuntimeSpecRow() && !priorIntent.HasRuntimeSpecRow() {
		// Verified-identity bypass (Phase 1, plan "Revision 1"). The migrate /
		// serena auto-register flows acquire THIS gate's supervisor.lock around
		// their reap+rewrite, so the lock the probe would see held is their OWN
		// handle, not a foreign (possibly-old) supervisor. Skip the probe ONLY
		// when the caller proves it holds the matching lock: a non-nil token
		// whose lock is STILL HELD (lk.fl != nil; Release() nils it,
		// supervisor_lock.go) AND whose lk.path equals this gate's own
		// supervisor.lock path. A nil, released, or path-mismatched token is NO
		// bypass → the probe runs → fail-closed. Folding the path check INTO the
		// gate means a misconfigured Phase-2 call site (wrong resolver) cannot
		// silently re-open the split-brain.
		gateLockPath := filepath.Join(stateDir, "supervisor.lock")
		if bp := opts.SupervisorLockBypass.lk; bp != nil && bp.fl != nil && bp.path == gateLockPath {
			// Verified bypass: the caller holds this gate's exact lock. Emit the
			// info audit row (mirrors emitSpecBearingInstallRefusedEvent) and skip
			// the probe so the spec-bearing write proceeds.
			emitSpecBearingInstallAllowedUnderLockEvent(m.Name, gateLockPath)
		} else {
			running, pid, probeErr := SupervisorRunningUnderStateDir(stateDir)
			switch {
			case probeErr != nil:
				emitSpecBearingInstallRefusedEvent(m.Name, "supervisor-liveness-undeterminable", 0)
				return "", fmt.Errorf("InstallParsedManifest: refusing to write spec-bearing serena dynamic-pool descriptors (runtime_spec) for %q: could not determine whether a supervisor is running (%v) — failing closed to avoid split-brain with an older supervisor whose intent watcher uses DisallowUnknownFields. Stop any running supervisor and resolve the probe error, then re-run the install (design §7.1)", m.Name, probeErr)
			case running:
				emitSpecBearingInstallRefusedEvent(m.Name, "supervisor-running", pid)
				pidNote := ""
				if pid > 0 {
					pidNote = fmt.Sprintf(" (pid %d)", pid)
				}
				return "", fmt.Errorf("InstallParsedManifest: refusing to write spec-bearing serena dynamic-pool descriptors (runtime_spec) for %q while a supervisor is running%s: an older supervisor binary's intent watcher uses DisallowUnknownFields and would reject the new field, leaving split-brain. STOP the running supervisor first (the next start will be this binary, which reads runtime_spec) — note `mcphub install --upgrade` alone leaves a supervisor running, so the Phase-4 cold-restart driver or a manual stop is required — then re-run the install (design §7.1)", m.Name, pidNote)
			}
		}
	}

	// 2. Pre-flight intent-write gate: dry-write to a temp path. No rollback
	// push — this is a read-only-ish probe that leaves no committed side
	// effect (the temp file is removed immediately). The probe targets a
	// DISTINCT ".preflight" path with its OWN ".preflight.lock" leaf, so it
	// never re-acquires the supervisor-intent.json.lock we hold here. (Dry-run
	// short-circuited above before reaching this flock-guarded section.)
	if err := preflightSupervisorIntentWrite(stateDir, desiredIntent); err != nil {
		return "", fmt.Errorf("pre-flight supervisor-intent write: %w", err)
	}

	// 3. Build the plan. NOTE: refuseWorkspaceScopedInstall is intentionally
	// NOT called — workspace-scoped is the intended input here.
	plan, err := BuildPlanWithOpts(m, BuildPlanOpts{
		ClientsInclude:    opts.ClientsInclude,
		IncludeAllClients: opts.IncludeAllClients,
	})
	if err != nil {
		return "", err
	}

	// 4. Audit-first + execute, with the supervisor-intent write folded into
	// executeInstallTo's rollback stack via the intermediate hook. On dry-run
	// installPlan prints the plan and returns without invoking the hook. The
	// write + rollback restore run LOCK-FREE because the supervisor-intent
	// flock is already held by this function (see FIX 1 above).
	var intermediate intentWriteStep = func() (func(), error) {
		// The supervisor-intent write below is the irreversible commit point. Honor a
		// caller cancellation HERE — the last instant before the write — so serena
		// auto-register's session watcher (which cancels ctx on a mid-register session
		// DELETE/idle-sweep) prevents a terminated session from landing a committed
		// daemon row (bot PR #253 r6 P2). For a DaemonTemplate plan executeInstallTo has
		// run no other mutation (zero scheduler tasks), so this IS the true pre-commit
		// gate; the caller's failPreCommit then rolls the registry row back (and, if it
		// reaped, recovery-restarts on the still-intact prior intent).
		if cerr := ctx.Err(); cerr != nil {
			return nil, fmt.Errorf("supervisor-intent commit canceled before write: %w", cerr)
		}
		if werr := writeSupervisorIntentLockHeld(intentPath, desiredIntent); werr != nil {
			return nil, fmt.Errorf("write supervisor intent %s: %w", intentPath, werr)
		}
		// Compensating undo: restore the prior file content verbatim, or
		// remove the file entirely if it did not exist before this install.
		undo := func() {
			if priorExisted {
				if rerr := writeSupervisorIntentLockHeld(intentPath, priorIntent); rerr != nil {
					fmt.Fprintf(w, "  rollback: restore prior supervisor-intent failed: %v\n", rerr)
				} else {
					fmt.Fprintf(w, "  rollback: restored prior supervisor-intent.json\n")
				}
				return
			}
			if rerr := os.Remove(intentPath); rerr != nil && !os.IsNotExist(rerr) {
				fmt.Fprintf(w, "  rollback: remove supervisor-intent failed: %v\n", rerr)
			} else {
				fmt.Fprintf(w, "  rollback: removed supervisor-intent.json\n")
			}
		}
		return undo, nil
	}

	// FIX 2 — skip scheduler prune for workspace-scoped (DaemonTemplate)
	// installs. A workspace-scoped manifest yields ZERO SchedulerTasks, but
	// executeInstallTo's full-install reconcile would then prune EVERY existing
	// mcp-local-hub-<server>-* scheduler task against an empty planned set —
	// destroying registered serena workspace tasks before the intent is even
	// written. Workspace-scoped daemons live in supervisor-intent.json, not in
	// scheduler tasks, so there is nothing for this seam to reconcile. Legacy
	// callers (api.Install et al.) pass non-DaemonTemplate manifests and leave
	// SkipSchedulerPrune false, so their reconcile behavior is unchanged.
	if err := a.installPlan(ctx, m, plan, installPlanOpts{
		Writer:       w,
		DaemonFilter: "",
		DryRun:       false, // dry-run short-circuited above; this path always mutates
		// StartTasks is always false: this seam defers every daemon start to
		// the supervisor reconciler (workspace-scoped-only contract). Pass B
		// never runs here.
		StartTasks:         false,
		Intermediate:       intermediate,
		SkipSchedulerPrune: m.DaemonTemplate != nil,
		AuditTaskNames:     fanOutAuditTaskNames(m, desiredIntent),
	}); err != nil {
		return "", err
	}
	if len(removedSupervisorTargetsAfterInstall) > 0 {
		nudgeResult := nudgeSupervisorReconcileAfterRemovedTargets(context.Background())
		killRemovedSupervisorTargetsAfterGlobalInstall(
			removedSupervisorTargetsAfterInstall,
			removedSupervisorPIDByTask,
			nudgeResult,
			fmt.Sprintf("re-run install or `mcphub stop --force %s`", m.Name),
			w,
		)
	}
	return intentPath, nil
}

// installPlanCore is the shared install body for the three global-manifest
// entrypoints — (*API).Install, installUsingEmbedFirst, installFromManifestDir
// — so the v0.6 Phase F "global daemons → supervisor-intent (not scheduler)"
// decision lives in ONE owner rather than being duplicated across all three.
//
// Decision:
//   - DRY RUN → print the plan and return (no mutation, no intent).
//   - GLOBAL manifest with ≥1 daemon (m.Kind != workspace-scoped &&
//     len(plan.SupervisorIntent) > 0) → SUPERVISOR-INTENT path: write the
//     daemon descriptor rows into supervisor-intent.json (under the per-file
//     flock, folded into executeInstallTo's rollback stack via the
//     intermediate hook) and DEFER every daemon spawn to the supervisor
//     reconcile loop. NO per-daemon `\mcp-local-hub-<server>-<daemon>` Task
//     Scheduler task is created.
//   - GLOBAL manifest with ZERO daemons (e.g. transport=remote-http) → the
//     legacy client-config-only path (no tasks to create, no intent rows to
//     write); StartTasks stays false because there is nothing to start.
//
// After a non-dry-run success it records the Desired=running re-enable intent
// for each planned task exactly as the legacy path did
// (recordInstallIntentPostSuccess).
func (a *API) installPlanCore(ctx context.Context, m *config.ServerManifest, plan *Plan, daemonFilter string, dryRun bool, w io.Writer) error {
	// Phase F: a global manifest's daemon lifecycle is owned by
	// supervisor-intent.json, not by per-daemon scheduler tasks. Full global
	// installs must enter this merge even when the NEW manifest contributes zero
	// rows (daemonless remote-http): the full install still owns removal of this
	// server's prior descriptor rows and per-server weekly timer.
	superviseGlobal := m.Kind != config.KindWorkspaceScoped && (len(plan.SupervisorIntent) > 0 || daemonFilter == "")
	if dryRun {
		if superviseGlobal {
			return a.printSupervisorGlobalInstallDryRun(m, plan, daemonFilter, w)
		}
		return a.installPlan(ctx, m, plan, installPlanOpts{
			Writer:       w,
			DaemonFilter: daemonFilter,
			DryRun:       true,
		})
	}

	var nudgeAfterInstall bool
	var ensureAutostartAfterInstall bool
	var autostartStrictMode bool
	var removedSupervisorTargetsAfterInstall []SupervisorDaemon
	var removedSupervisorPIDByTask map[string]int
	nudgeResult := supervisorReconcileNudgeResult{status: supervisorReconcileNudgeSucceeded}

	if superviseGlobal {
		stateDir, err := DaemonStateDir()
		if err != nil {
			return fmt.Errorf("resolve state dir: %w", err)
		}
		intentPath := joinStateFilePath(stateDir, supervisorIntentFileLeaf)
		ownershipScope := supervisorIntentOwnershipScopeForManifest(m, nil, "")
		if daemonFilter == "" {
			// Pre-capture a superset before the intent flock. The locked merge
			// below recomputes the authoritative removed targets; rows that were
			// not present in this pre-read have no captured PID and use the port
			// path after the reconcile nudge.
			preCaptureTargets, err := preliminarySupervisorTargetsForServerScope(intentPath, m.Name, ownershipScope)
			if err != nil {
				return err
			}
			removedSupervisorPIDByTask = supervisorOwnedLivePIDsForTargets(ctx, preCaptureTargets)
		}

		// The supervisor-intent flock is held ONLY across the read-merge-write
		// critical section below. It MUST be released before the fall-through to
		// recordInstallIntentPostSuccess at the end of this function:
		// recordInstallIntentPostSuccess → WriteStopIntent → mutateStopSubBlock
		// re-acquires the SAME supervisor-intent.json.lock leaf with a BLOCKING
		// Lock(), which is non-reentrant on Windows LockFileEx and would
		// self-deadlock if this function still held it. Wrapping the locked work
		// in an inline closure makes the deferred Unlock fire at the closure's
		// return — i.e. while still inside installPlanCore but BEFORE the
		// post-success stop-subblock write — rather than at installPlanCore's own
		// return.
		runLocked := func() error {
			// Hold the canonical per-file flock across the read-merge AND the
			// commit inside executeInstallTo's intermediate hook, exactly as
			// InstallParsedManifest does (FIX 1 there) — otherwise two concurrent
			// installs for different servers each read a stale snapshot and the
			// later writer clobbers the earlier writer's sibling-server rows.
			// Because we hold the lock, the inner write uses the LOCK-FREE
			// writeSupervisorIntentLockHeld body (re-entering WriteSupervisorIntent
			// would deadlock).
			lock := flock.New(intentPath + supervisorIntentLockSuffix)
			if err := lock.Lock(); err != nil {
				return fmt.Errorf("supervisor-intent flock %s: %w", intentPath+supervisorIntentLockSuffix, err)
			}
			defer func() { _ = lock.Unlock() }()

			// Build the merged intent under the held lock. For a global manifest
			// serenaOrPlanDaemons takes the supervisorDaemonsFromPlan branch (no
			// workspaces, no runtime_spec), so this carries NO spec-bearing rows —
			// the §7.1 supervisor-running gate does not apply. daemonFilter is
			// threaded so a global manifest's weekly_refresh materializes a
			// server-weekly-refresh MaintenanceTimer on a FULL install (the Phase F
			// successor to the deleted per-server scheduler task at install.go:1230).
			desiredIntent, priorIntent, priorExisted, err := a.buildMergedSupervisorIntent(m, intentPath, nil, daemonFilter, w)
			if err != nil {
				return err
			}
			removedSupervisorTargetsAfterInstall = removedSupervisorTargetsForFullInstallScope(priorIntent, desiredIntent, m.Name, daemonFilter, ownershipScope)
			desiredIntent.Stops = pruneStopsForRemovedSupervisorTargets(desiredIntent.Stops, removedSupervisorTargetsAfterInstall)
			intentWriteNeeded := len(plan.SupervisorIntent) > 0 ||
				(daemonFilter == "" && supervisorIntentHasServerLifecycleArtifactsScope(priorIntent, m.Name, ownershipScope))
			nudgeAfterInstall = len(plan.SupervisorIntent) > 0 ||
				(daemonFilter == "" && supervisorIntentHasServerDaemonRowsScope(priorIntent, m.Name, ownershipScope))
			if len(plan.SupervisorIntent) > 0 {
				ensureAutostartAfterInstall = true
				autostartStrictMode = desiredIntent.StrictMode
			}
			// Defense-in-depth: a global manifest must never materialize a
			// runtime_spec row. If one ever appeared it would need the §7.1
			// supervisor-running gate (which lives in InstallParsedManifest), so
			// refuse rather than write a spec-bearing intent through this gate-less
			// path. Scope the check to THIS server's rows: the merged intent
			// legitimately carries OTHER servers' spec-bearing rows (the serena
			// dynamic-pool rows after `mcphub migrate serena
			// legacy-to-dynamic-pool`), and a whole-file check made EVERY global
			// install fail on such hosts (live-host fetch install, 2026-06-12).
			for _, d := range desiredIntent.Daemons {
				if d.RuntimeSpec != nil && supervisorIntentRowOwnedByScope(d, m.Name, ownershipScope) {
					return fmt.Errorf("installPlanCore: global manifest %q unexpectedly produced a runtime_spec-bearing supervisor-intent row; global installs must not carry runtime_spec (that path is InstallParsedManifest's)", m.Name)
				}
			}

			if intentWriteNeeded {
				// Pre-flight the secure-write so a doomed install fails fast with a
				// pristine end-state (mirrors InstallParsedManifest step 2).
				if err := preflightSupervisorIntentWrite(stateDir, desiredIntent); err != nil {
					return fmt.Errorf("pre-flight supervisor-intent write: %w", err)
				}
			}

			var intermediate intentWriteStep
			if intentWriteNeeded {
				intermediate = func() (func(), error) {
					if cerr := ctx.Err(); cerr != nil {
						return nil, fmt.Errorf("supervisor-intent commit canceled before write: %w", cerr)
					}
					if werr := writeSupervisorIntentLockHeld(intentPath, desiredIntent); werr != nil {
						return nil, fmt.Errorf("write supervisor intent %s: %w", intentPath, werr)
					}
					undo := func() {
						if priorExisted {
							if rerr := writeSupervisorIntentLockHeld(intentPath, priorIntent); rerr != nil {
								fmt.Fprintf(w, "  rollback: restore prior supervisor-intent failed: %v\n", rerr)
							} else {
								fmt.Fprintf(w, "  rollback: restored prior supervisor-intent.json\n")
							}
							return
						}
						if rerr := os.Remove(intentPath); rerr != nil && !os.IsNotExist(rerr) {
							fmt.Fprintf(w, "  rollback: remove supervisor-intent failed: %v\n", rerr)
						} else {
							fmt.Fprintf(w, "  rollback: removed supervisor-intent.json\n")
						}
					}
					return undo, nil
				}
			}

			if err := a.installPlan(ctx, m, plan, installPlanOpts{
				Writer:       w,
				DaemonFilter: daemonFilter,
				DryRun:       false,
				// Defer every daemon spawn to the supervisor reconcile loop; create
				// no per-daemon scheduler task; the daemon set is owned by
				// supervisor-intent.json, so the legacy scheduler cleanup below is
				// a best-effort handoff delete rather than executeInstallTo's
				// rollback-scoped replacement/prune.
				StartTasks:         false,
				Intermediate:       intermediate,
				SkipSchedulerTasks: true,
				SkipSchedulerPrune: true,
			}); err != nil {
				return err
			}
			return nil
		}
		// Run the locked critical section; its deferred Unlock fires here, so the
		// supervisor-intent flock is released BEFORE recordInstallIntentPostSuccess
		// below re-acquires the same leaf (which would otherwise self-deadlock).
		if err := runLocked(); err != nil {
			return err
		}
		cleanupLegacySchedulerTasksForSupervisorInstall(m, daemonFilter, w)
		if ensureAutostartAfterInstall {
			ensureGlobalInstallAutostartOwner(w, autostartStrictMode)
		}
	} else {
		// Global manifest with no daemons (e.g. remote-http): client-config
		// writes only — no scheduler tasks, no supervisor-intent rows.
		if err := a.installPlan(ctx, m, plan, installPlanOpts{
			Writer:             w,
			DaemonFilter:       daemonFilter,
			DryRun:             false,
			StartTasks:         false,
			SkipSchedulerTasks: true,
			SkipSchedulerPrune: true,
		}); err != nil {
			return err
		}
	}

	// Post-success: record Desired=running re-enable intent for each planned
	// task (clears any prior stop tombstone). Failures are logged + tolerated —
	// the install already happened.
	a.recordInstallIntentPostSuccess(m, daemonFilter, w)

	// v0.6 Phase F (bot PR #288 F2): a fresh global install only WRITES the
	// supervisor-intent descriptor rows; nothing in this path spawns the
	// daemon. A running supervisor's IntentWatcher does NOT pick up a NEW
	// descriptor — its onChange handler posts EvIntentUpdate only for tasks
	// that appear in the STOPS delta (resolveWatcherDaemonIntent →
	// UnifiedStopsFile → diffIntentSnapshots), and a freshly-installed daemon
	// has no stop entry, so it is never in that delta and never spawns until
	// the next manual reconcile / GUI restart / supervisor cold-restart. Nudge
	// the supervisor to reconcile now (it re-reads intent from disk and the
	// drift classifier maps a sched-less running descriptor to a spawn —
	// classifyDriftAction !hasSched + running → post_ev_intent_update). When no
	// supervisor is running, print the operator hint. Only fires for the
	// supervise-global branch that actually wrote descriptor rows.
	if nudgeAfterInstall {
		nudgeResult = nudgeSupervisorReconcileAfterGlobalInstall(context.Background(), w)
	}
	// A live supervisor may still hold the removed descriptor rows in its
	// in-memory intent cache until the reconcile nudge above refreshes it. Kill
	// only after the nudge has had a chance to drop those rows, otherwise the
	// child exit can look like a crash of a still-desired daemon and be respawned.
	killRemovedSupervisorTargetsAfterGlobalInstall(
		removedSupervisorTargetsAfterInstall,
		removedSupervisorPIDByTask,
		nudgeResult,
		fmt.Sprintf("re-run install or `mcphub stop --force %s`", m.Name),
		w,
	)
	return nil
}

func (a *API) printSupervisorGlobalInstallDryRun(m *config.ServerManifest, plan *Plan, daemonFilter string, w io.Writer) error {
	if w == nil {
		w = os.Stderr
	}
	stateDir, err := daemonStateDirReadOnly()
	if err != nil {
		return fmt.Errorf("resolve state dir: %w", err)
	}
	intentPath := joinStateFilePath(stateDir, supervisorIntentFileLeaf)
	desiredIntent, priorIntent, priorExisted, mergeErr := a.buildMergedSupervisorIntent(m, intentPath, nil, daemonFilter, io.Discard)
	var priorDiffUnavailable string
	if mergeErr != nil {
		desiredIntent = &SupervisorIntentFile{
			Version:           1,
			Daemons:           supervisorDaemonsFromPlan(m, daemonFilter),
			MaintenanceTimers: mergeServerWeeklyRefreshTimer(m, daemonFilter, nil),
		}
		priorDiffUnavailable = fmt.Sprintf("unable to read prior supervisor intent at %s: %v", intentPath, mergeErr)
	} else if !priorExisted {
		priorDiffUnavailable = fmt.Sprintf("prior supervisor intent not found at %s", intentPath)
	}
	var removed []SupervisorDaemon
	if mergeErr == nil && priorExisted {
		ownershipScope := supervisorIntentOwnershipScopeForManifest(m, nil, "")
		removed = removedSupervisorTargetsForFullInstallScope(priorIntent, desiredIntent, m.Name, daemonFilter, ownershipScope)
	}
	legacyTasks, legacyErr := legacySchedulerTasksForSupervisorInstallDryRun(m, daemonFilter)

	fmt.Fprintf(w, "Install plan for server %q (dry-run):\n\n", plan.Server)
	daemonRows := supervisorDaemonsFromPlan(m, daemonFilter)
	fmt.Fprintf(w, "  Supervisor intent rows to write (%d):\n", len(daemonRows))
	for _, d := range daemonRows {
		fmt.Fprintf(w, "    \u2022 %s  [port %d]\n        %s %v\n", strings.TrimPrefix(d.TaskName, `\`), d.Port, d.Command, d.Args)
	}

	var timers []MaintenanceTimer
	if desiredIntent != nil {
		for _, tm := range desiredIntent.MaintenanceTimers {
			if maintenanceTimerOwnedBy(tm, m.Name) {
				timers = append(timers, tm)
			}
		}
	}
	fmt.Fprintf(w, "\n  Maintenance timers to ensure (%d):\n", len(timers))
	for _, tm := range timers {
		fmt.Fprintf(w, "    \u2022 %s  [%s]\n        %s %v\n", strings.TrimPrefix(tm.Name, `\`), tm.Kind, tm.Command, tm.Args)
	}

	autostartAction := "no-op"
	if len(plan.SupervisorIntent) > 0 {
		autostartAction = "ensure supervisor owner autostart is enabled"
	}
	fmt.Fprintf(w, "\n  Autostart owner to ensure: %s\n", autostartAction)

	fmt.Fprintf(w, "\n  Legacy scheduler tasks to clean (%d):\n", len(legacyTasks))
	if legacyErr != nil {
		fmt.Fprintf(w, "    \u2022 unable to preview legacy cleanup: %v\n", legacyErr)
	} else {
		for _, name := range legacyTasks {
			fmt.Fprintf(w, "    \u2022 %s\n", strings.TrimPrefix(name, `\`))
		}
	}

	fmt.Fprintf(w, "\n  Removed supervisor targets to kill (%d):\n", len(removed))
	if priorDiffUnavailable != "" {
		fmt.Fprintf(w, "    \u2022 Prior supervisor-intent diff unavailable: %s; removed-targets / legacy cleanup preview omitted.\n", priorDiffUnavailable)
	}
	for _, d := range removed {
		fmt.Fprintf(w, "    \u2022 %s  [port %d]\n", strings.TrimPrefix(d.TaskName, `\`), d.Port)
	}

	printClientUpdatesTo(w, plan)
	fmt.Fprintln(w, "\nNo changes made.")
	return nil
}

func legacySchedulerTasksForSupervisorInstallDryRun(m *config.ServerManifest, daemonFilter string) ([]string, error) {
	if m == nil || m.Name == "" {
		return nil, nil
	}
	if daemonFilter != "" {
		return []string{"mcp-local-hub-" + m.Name + "-" + daemonFilter}, nil
	}
	sch, err := newScheduler()
	if err != nil {
		if schedulerUnavailableError(err) {
			return nil, nil
		}
		return nil, err
	}
	prefix := "mcp-local-hub-" + m.Name + "-"
	tasks, err := sch.List(prefix)
	if err != nil {
		if schedulerUnavailableError(err) {
			return nil, nil
		}
		return nil, err
	}
	// r36-2: this dry-run preview MIRRORS cleanupLegacySchedulerTasksForSupervisorInstall's
	// full-server arm; it must apply the SAME longest-installed-prefix filter so
	// the preview never claims it will delete a hyphen-sibling's task the real
	// cleanup now spares (otherwise the dry-run LIES). Read the installed catalog
	// once; an empty set (read failure) claims any prefix-matching task, matching
	// the real cleanup's fallback.
	var installed map[string]struct{}
	if names, lerr := listManifestNamesEmbedFirst(); lerr == nil {
		installed = make(map[string]struct{}, len(names))
		for _, n := range names {
			installed[n] = struct{}{}
		}
	}
	out := make([]string, 0, len(tasks))
	for _, task := range tasks {
		name := strings.TrimPrefix(task.Name, `\`)
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if manifestDeclaresSchedulerTask(m, task.Name) {
			out = append(out, name)
			continue
		}
		if !blankServerRowOwnedByLongestInstalledPrefix(task.Name, m.Name, installed) {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

func ensureGlobalInstallAutostartOwner(w io.Writer, strictMode bool) {
	backend, err := installAutostartBackendFactoryFn()
	if err != nil {
		warnAutostartOwner(w, "resolve backend", err)
		return
	}
	opts := autostart.Options{StrictMode: strictMode}
	if canonical, err := canonicalMcphubPath(); err == nil {
		opts.MCPHubPath = canonical
	}
	state, err := backend.Status(opts)
	if err != nil {
		warnAutostartOwner(w, "read status", err)
		return
	}
	switch state {
	case autostart.StateAbsent, autostart.StateDrifted:
		if err := backend.Enable(opts); err != nil {
			warnAutostartOwner(w, "enable", err)
		}
	case autostart.StateEnabledRunning, autostart.StateEnabledStopped:
		return
	default:
		if w != nil {
			fmt.Fprintf(w, "  warning: supervisor autostart owner is %s; run `mcphub autostart enable` to reconcile\n", state)
		}
	}
}

func warnAutostartOwner(w io.Writer, action string, err error) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, "  warning: supervisor autostart owner %s failed: %v (manual: mcphub autostart enable)\n", action, err)
}

type supervisorReconcileNudgeStatus int

const (
	supervisorReconcileNudgeSucceeded supervisorReconcileNudgeStatus = iota
	supervisorReconcileNudgeIPCUnavailable
	supervisorReconcileNudgeFailed
)

type supervisorReconcileNudgeResult struct {
	status supervisorReconcileNudgeStatus
	err    error
}

func (r supervisorReconcileNudgeResult) allowsRemovedTargetKill() bool {
	return r.status == supervisorReconcileNudgeSucceeded || r.status == supervisorReconcileNudgeIPCUnavailable
}

func nudgeSupervisorReconcileApply(ctx context.Context) supervisorReconcileNudgeResult {
	if _, err := supervisorReconcileApplyFn(ctx, true); err != nil {
		if errors.Is(err, ErrSupervisorIPCUnavailable) {
			return supervisorReconcileNudgeResult{status: supervisorReconcileNudgeIPCUnavailable, err: err}
		}
		return supervisorReconcileNudgeResult{status: supervisorReconcileNudgeFailed, err: err}
	}
	return supervisorReconcileNudgeResult{status: supervisorReconcileNudgeSucceeded}
}

// nudgeSupervisorReconcileAfterGlobalInstall asks a running supervisor to
// reconcile so a freshly-installed global daemon spawns immediately instead of
// waiting up to 60s for the IntentWatcher poll (which only feeds stops, not new
// descriptors — see installPlanCore F2 note). Best-effort: when no supervisor
// is running (ErrSupervisorIPCUnavailable) it tries to start the autostart owner
// now, falling back to the operator hint if that best-effort start fails. Any
// other reconcile error is non-fatal — the descriptor is durably on disk, so the
// next supervisor start spawns it via the startup-path reconcile. The returned
// outcome lets removed-target cleanup avoid killing a child while the
// supervisor still has the old descriptor in memory.
func nudgeSupervisorReconcileAfterGlobalInstall(ctx context.Context, w io.Writer) supervisorReconcileNudgeResult {
	result := nudgeSupervisorReconcileApply(ctx)
	if result.status == supervisorReconcileNudgeIPCUnavailable {
		recovery := startAutostartOwnerAfterUnavailableSupervisor(w, result.err)
		if recovery.status == supervisorReconcileNudgeFailed {
			return recovery
		}
		return result
	}
	// Any other reconcile error is non-fatal to the durable install, but callers
	// must treat it as unsafe for removed-target force-kill.
	return result
}

func nudgeSupervisorReconcileAfterRemovedTargets(ctx context.Context) supervisorReconcileNudgeResult {
	return demoteIPCUnavailableWhenOwnerAlive(nudgeSupervisorReconcileApply(ctx))
}

// demoteIPCUnavailableWhenOwnerAlive guards the removed-target force-kill path
// against a live-but-wedged supervisor. nudgeSupervisorReconcileApply maps an
// unreachable IPC endpoint to supervisorReconcileNudgeIPCUnavailable, which
// allowsRemovedTargetKill() treats as kill-OK on the assumption that an
// unreachable endpoint means no supervisor is spawning the child. That
// assumption only holds when NO process owns supervisor.lock: a supervisor that
// is ALIVE (holds the lock) but whose IPC listener is wedged still tracks the
// child in memory and its reaper will respawn / quarantine a child we force-kill
// here. So on IPC-unavailable we run the SAME flock-authoritative live-owner
// probe the global-install nudge uses (startAutostartOwnerAfterUnavailableSupervisor):
//   - lock owner ALIVE + IPC unreachable → demote to supervisorReconcileNudgeFailed
//     (allowsRemovedTargetKill() == false), so the kill is skipped and the
//     operator gets a retry hint instead of a respawn fight.
//   - NO live owner (or any probe error) → keep IPCUnavailable; the kill is safe
//     because nothing will respawn the now-descriptorless child.
//
// Unlike the global-install nudge this never STARTS the autostart owner: the
// removed-target / uninstall path is terminating a daemon, not bringing a fresh
// descriptor online, so there is no owner to start here.
func demoteIPCUnavailableWhenOwnerAlive(result supervisorReconcileNudgeResult) supervisorReconcileNudgeResult {
	if result.status != supervisorReconcileNudgeIPCUnavailable {
		return result
	}
	stateDir, err := DaemonStateDir()
	if err != nil {
		// Cannot resolve the state dir → cannot probe the lock owner. Fail
		// closed against the kill: a live wedged supervisor is the dangerous
		// case, so treat the unknown as owner-alive and skip the kill.
		return supervisorReconcileNudgeResult{
			status: supervisorReconcileNudgeFailed,
			err:    fmt.Errorf("resolve state dir before removed-target owner probe: %w (original: %v)", err, result.err),
		}
	}
	running, pid, probeErr := installSupervisorRunningProbeFn(stateDir)
	if probeErr != nil {
		// Probe failed → unknown owner liveness. Fail closed against the kill.
		return supervisorReconcileNudgeResult{
			status: supervisorReconcileNudgeFailed,
			err:    fmt.Errorf("probe supervisor before removed-target kill: %w (original: %v)", probeErr, result.err),
		}
	}
	if running {
		return supervisorReconcileNudgeResult{
			status: supervisorReconcileNudgeFailed,
			err:    fmt.Errorf("supervisor lock is held by pid=%d but IPC is unreachable; removed-target force-kill skipped (a live supervisor still tracks the child): %w", pid, result.err),
		}
	}
	// No live owner — IPC really is unavailable because no supervisor is
	// running; the kill is safe because nothing will respawn the child.
	return result
}

func startAutostartOwnerAfterUnavailableSupervisor(w io.Writer, ipcErr error) supervisorReconcileNudgeResult {
	stateDir, err := DaemonStateDir()
	if err != nil {
		warnAutostartOwner(w, "resolve state dir before start", err)
		warnNoRunningSupervisor(w)
		return supervisorReconcileNudgeResult{status: supervisorReconcileNudgeIPCUnavailable, err: ipcErr}
	}
	running, pid, err := installSupervisorRunningProbeFn(stateDir)
	if err != nil {
		warnAutostartOwner(w, "probe supervisor before start", err)
		warnNoRunningSupervisor(w)
		return supervisorReconcileNudgeResult{status: supervisorReconcileNudgeIPCUnavailable, err: ipcErr}
	}
	if running {
		if w != nil {
			fmt.Fprintf(w, "  warning: supervisor lock is held by pid=%d, but IPC is unreachable; likely a hung listener or a supervisor started with --no-ipc. Run `mcphub restart`, or kill the wedged process if restart cannot reach it.\n", pid)
		}
		err := fmt.Errorf("supervisor lock is held by pid=%d but IPC is unreachable: %w", pid, ipcErr)
		return supervisorReconcileNudgeResult{status: supervisorReconcileNudgeFailed, err: err}
	}
	if err := installAutostartOwnerStartFn(); err != nil {
		warnAutostartOwner(w, "start", err)
		warnNoRunningSupervisor(w)
		return supervisorReconcileNudgeResult{status: supervisorReconcileNudgeIPCUnavailable, err: ipcErr}
	}
	if w != nil {
		fmt.Fprintln(w, "  supervisor autostart owner started; installed daemons will reconcile shortly.")
	}
	return supervisorReconcileNudgeResult{status: supervisorReconcileNudgeIPCUnavailable, err: ipcErr}
}

func warnNoRunningSupervisor(w io.Writer) {
	if w != nil {
		fmt.Fprintln(w, "Note: no running supervisor — the installed daemon will start on the next `mcphub supervise` (or via the autostart task on next logon).")
	}
}

func supervisorIntentHasServerDaemonRows(intent *SupervisorIntentFile, server string) bool {
	return supervisorIntentHasServerDaemonRowsScope(intent, server, nil)
}

func supervisorIntentHasServerDaemonRowsScope(intent *SupervisorIntentFile, server string, scope *supervisorIntentOwnershipScope) bool {
	if intent == nil {
		return false
	}
	for _, d := range intent.Daemons {
		if supervisorIntentRowOwnedByScope(d, server, scope) {
			return true
		}
	}
	return false
}

func supervisorIntentHasServerLifecycleArtifacts(intent *SupervisorIntentFile, server string) bool {
	return supervisorIntentHasServerLifecycleArtifactsScope(intent, server, nil)
}

func supervisorIntentHasServerLifecycleArtifactsScope(intent *SupervisorIntentFile, server string, scope *supervisorIntentOwnershipScope) bool {
	if supervisorIntentHasServerDaemonRowsScope(intent, server, scope) {
		return true
	}
	if intent == nil {
		return false
	}
	for _, tm := range intent.MaintenanceTimers {
		if serverWeeklyRefreshTimerOwnedBy(tm, server) {
			return true
		}
	}
	return false
}

func removedSupervisorTargetsForFullInstall(prior, merged *SupervisorIntentFile, server, daemonFilter string) []SupervisorDaemon {
	return removedSupervisorTargetsForFullInstallScope(prior, merged, server, daemonFilter, nil)
}

func removedSupervisorTargetsForFullInstallScope(prior, merged *SupervisorIntentFile, server, daemonFilter string, scope *supervisorIntentOwnershipScope) []SupervisorDaemon {
	if daemonFilter != "" {
		return nil
	}
	return removedSupervisorTargetsForServerMergeScope(prior, merged, server, scope)
}

func removedSupervisorTargetsForServerMerge(prior, merged *SupervisorIntentFile, server string) []SupervisorDaemon {
	return removedSupervisorTargetsForServerMergeScope(prior, merged, server, nil)
}

func removedSupervisorTargetsForServerMergeScope(prior, merged *SupervisorIntentFile, server string, scope *supervisorIntentOwnershipScope) []SupervisorDaemon {
	if prior == nil || merged == nil || server == "" {
		return nil
	}
	freshTasks := make(map[string]struct{})
	for _, d := range merged.Daemons {
		if supervisorIntentRowOwnedByScope(d, server, scope) {
			freshTasks[canonicalIntentTaskKey(d.TaskName)] = struct{}{}
		}
	}

	var removed []SupervisorDaemon
	for _, d := range prior.Daemons {
		if !supervisorIntentRowOwnedByScope(d, server, scope) {
			continue
		}
		if _, replaced := freshTasks[canonicalIntentTaskKey(d.TaskName)]; replaced {
			continue
		}
		removed = append(removed, d)
	}
	return removed
}

func pruneStopsForRemovedSupervisorTargets(stops map[string]DaemonIntent, removed []SupervisorDaemon) map[string]DaemonIntent {
	if len(stops) == 0 || len(removed) == 0 {
		return stops
	}
	removedTasks := make(map[string]struct{}, len(removed))
	for _, d := range removed {
		removedTasks[canonicalIntentTaskKey(d.TaskName)] = struct{}{}
	}
	kept := make(map[string]DaemonIntent, len(stops))
	for taskName, intent := range stops {
		if _, drop := removedTasks[canonicalIntentTaskKey(taskName)]; drop {
			continue
		}
		kept[taskName] = intent
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

func preliminarySupervisorTargetsForServer(intentPath, server string) ([]SupervisorDaemon, error) {
	return preliminarySupervisorTargetsForServerScope(intentPath, server, nil)
}

func preliminarySupervisorTargetsForServerScope(intentPath, server string, scope *supervisorIntentOwnershipScope) ([]SupervisorDaemon, error) {
	prior, existed, err := readSupervisorIntentForMerge(intentPath)
	if err != nil {
		return nil, err
	}
	if !existed || prior == nil || server == "" {
		return nil, nil
	}
	targets := make([]SupervisorDaemon, 0, len(prior.Daemons))
	for _, d := range prior.Daemons {
		if supervisorIntentRowOwnedByScope(d, server, scope) {
			targets = append(targets, d)
		}
	}
	return targets, nil
}

func supervisorOwnedLivePIDsForTargets(ctx context.Context, targets []SupervisorDaemon) map[string]int {
	if len(targets) == 0 {
		return nil
	}
	livePIDs := supervisorOwnedLivePIDs(ctx)
	if len(livePIDs) == 0 {
		return nil
	}
	out := make(map[string]int, len(targets))
	for _, d := range targets {
		taskName := strings.TrimPrefix(d.TaskName, `\`)
		if pid := livePIDs[taskName]; pid > 0 {
			out[taskName] = pid
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func killRemovedSupervisorTargetsAfterGlobalInstall(targets []SupervisorDaemon, pidByTask map[string]int, nudgeResult supervisorReconcileNudgeResult, retryPath string, w io.Writer) {
	killRemovedSupervisorTargetsAfterNudge(targets, pidByTask, nudgeResult, retryPath, func(format string, args ...any) {
		if w != nil {
			fmt.Fprintf(w, "  warning: "+format+"\n", args...)
		}
	})
}

func killRemovedSupervisorTargetsAfterNudge(targets []SupervisorDaemon, pidByTask map[string]int, nudgeResult supervisorReconcileNudgeResult, retryPath string, warnf func(string, ...any)) {
	if len(targets) == 0 {
		return
	}
	if !nudgeResult.allowsRemovedTargetKill() {
		if warnf != nil {
			errText := "unknown reconcile failure"
			if nudgeResult.err != nil {
				errText = nudgeResult.err.Error()
			}
			warnf("force kill removed supervisor targets skipped because supervisor reconcile nudge failed: %s (retry: %s)", errText, retryPath)
		}
		return
	}
	for _, d := range targets {
		result := forceKillOneSupervisorTarget(d, pidByTask)
		if result.Err != "" && warnf != nil {
			warnf("force kill removed supervisor target %s: %s", d.TaskName, result.Err)
		}
	}
}

func cleanupLegacySchedulerTasksForSupervisorInstall(m *config.ServerManifest, daemonFilter string, w io.Writer) {
	if m == nil || m.Name == "" {
		return
	}
	sch, err := newScheduler()
	if err != nil {
		if schedulerUnavailableError(err) {
			return
		}
		if w != nil {
			fmt.Fprintf(w, "⚠ Legacy scheduler cleanup skipped: %v\n", err)
		}
		return
	}
	if daemonFilter != "" {
		name := "mcp-local-hub-" + m.Name + "-" + daemonFilter
		tasks, err := sch.List(name)
		if err != nil {
			if schedulerUnavailableError(err) {
				return
			}
			if w != nil {
				fmt.Fprintf(w, "⚠ Legacy scheduler cleanup skipped: list task %s: %v\n", name, err)
			}
			return
		}
		exists := false
		for _, task := range tasks {
			if strings.TrimPrefix(task.Name, "\\") == name {
				exists = true
				break
			}
		}
		if !exists {
			return
		}
		if deleteLegacySchedulerTaskBestEffort(sch, name, w) {
			killLegacySchedulerTaskDaemonByPortBestEffort(name, daemonPortForLegacySchedulerTask(m, name), w)
		}
		return
	}
	prefix := "mcp-local-hub-" + m.Name + "-"
	tasks, err := sch.List(prefix)
	if err != nil {
		if schedulerUnavailableError(err) {
			return
		}
		if w != nil {
			fmt.Fprintf(w, "⚠ Legacy scheduler cleanup skipped: list tasks for %s: %v\n", m.Name, err)
		}
		return
	}
	// r36-2: List(prefix) returns every task whose name STARTS WITH
	// "mcp-local-hub-<m.Name>-", which over-matches a hyphen-sibling server:
	// installing `demo` lists `mcp-local-hub-demo-alpha-beta`, which is server
	// demo-alpha's daemon `beta`, not a demo task. The pre-fix loop deleted it
	// unconditionally — wiping a sibling's scheduler task on every demo install.
	// (The KILL half was already exact-name port-guarded via
	// daemonPortForLegacySchedulerTask returning 0 for a foreign name; only the
	// DELETE was unguarded.) Gate each task through the longest-installed-prefix
	// disambiguator so demo only deletes tasks it actually owns. The installed
	// catalog is read once; on a read failure the set is empty, which makes the
	// disambiguator claim any prefix-matching task (no sibling proof available) —
	// the same outcome as the pre-fix unconditional delete, but only when the
	// catalog is genuinely unreadable.
	var installed map[string]struct{}
	if names, lerr := listManifestNamesEmbedFirst(); lerr == nil {
		installed = make(map[string]struct{}, len(names))
		for _, n := range names {
			installed[n] = struct{}{}
		}
	}
	for _, task := range tasks {
		name := strings.TrimPrefix(task.Name, "\\")
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		// Exact manifest task ownership beats the longest-prefix fallback. The
		// fallback's sibling protection is for ambiguous legacy rows; it must not
		// preserve a stale task for this manifest just because a longer-prefix
		// manifest is available in the catalog.
		if manifestDeclaresSchedulerTask(m, task.Name) {
			if deleteLegacySchedulerTaskBestEffort(sch, name, w) {
				killLegacySchedulerTaskDaemonByPortBestEffort(name, daemonPortForLegacySchedulerTask(m, name), w)
			}
			continue
		}
		// Skip a hyphen-sibling's task: only delete tasks this server is the
		// longest installed prefix of (mirrors the supervisor-intent ownership
		// arm at supervisorIntentRowOwnedByScope's Arm 2).
		if !blankServerRowOwnedByLongestInstalledPrefix(task.Name, m.Name, installed) {
			continue
		}
		// Full installs only delete/kill tasks returned by List(prefix), so
		// existence is proven before Delete's idempotent missing-task success.
		if deleteLegacySchedulerTaskBestEffort(sch, name, w) {
			killLegacySchedulerTaskDaemonByPortBestEffort(name, daemonPortForLegacySchedulerTask(m, name), w)
		}
	}
}

func manifestDeclaresSchedulerTask(m *config.ServerManifest, taskName string) bool {
	if m == nil || m.Name == "" {
		return false
	}
	canonical := canonicalIntentTaskKey(taskName)
	for _, d := range m.Daemons {
		if d.Name == "" {
			continue
		}
		if canonical == canonicalIntentTaskKey("mcp-local-hub-"+m.Name+"-"+d.Name) {
			return true
		}
	}
	return false
}

func deleteLegacySchedulerTaskBestEffort(sch interface{ Delete(string) error }, name string, w io.Writer) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	if err := sch.Delete(name); err != nil {
		if schedulerUnavailableError(err) {
			return false
		}
		if w != nil {
			fmt.Fprintf(w, "⚠ Failed to remove legacy scheduler task %s: %v\n", name, err)
		}
		return false
	}
	if w != nil {
		fmt.Fprintf(w, "✓ Scheduler task removed (legacy supervisor handoff): %s\n", name)
	}
	return true
}

func daemonPortForLegacySchedulerTask(m *config.ServerManifest, taskName string) int {
	if m == nil {
		return 0
	}
	name := strings.TrimPrefix(taskName, "\\")
	if name == "" {
		return 0
	}
	const prefix = "mcp-local-hub-"
	for _, d := range m.Daemons {
		if name == prefix+m.Name+"-"+d.Name {
			return d.Port
		}
	}
	return 0
}

func killLegacySchedulerTaskDaemonByPortBestEffort(taskName string, port int, w io.Writer) {
	if port == 0 {
		if w != nil {
			fmt.Fprintf(w, "⚠ Legacy scheduler cleanup skipped daemon kill for %s: daemon port unknown\n", taskName)
		}
		return
	}
	if legacySchedulerTaskPortOwnedBySupervisor(taskName, port) {
		return
	}
	outcome, err := forceKillByPortFn(port, 5*time.Second)
	if outcome == portKillIdentityMismatch {
		if w != nil {
			fmt.Fprintf(w, "⚠ Legacy scheduler cleanup skipped daemon kill for %s on port %d: port owned by foreign process, not killing: %v\n", taskName, port, err)
		}
		return
	}
	if err != nil {
		if w != nil {
			fmt.Fprintf(w, "⚠ Failed to stop legacy daemon after removing scheduler task %s on port %d: %v\n", taskName, port, err)
		}
	}
}

func legacySchedulerTaskPortOwnedBySupervisor(taskName string, port int) bool {
	portPID, havePortPID := supervisorOwnedPortPID(port)
	if !havePortPID {
		return false
	}
	livePIDs, reachable := supervisorOwnedLivePIDsWithReachability(context.Background())
	if !reachable {
		// Without both PID proof surfaces we cannot distinguish a legacy orphan
		// from a supervised child. Keep the handoff progressing: delete prevents
		// relaunch, and a bounded port kill is safe on a rerun because the
		// supervisor's restart policy respawns its desired child after an external
		// kill during upgrade.
		return false
	}
	taskKey := strings.TrimPrefix(canonicalIntentTaskKey(taskName), `\`)
	return livePIDs[taskKey] == portPID
}

// supervisorIntentFileLeaf is the canonical basename of the supervisor
// intent file under the per-user state directory.
const supervisorIntentFileLeaf = "supervisor-intent.json"

// buildMergedSupervisorIntent loads the existing supervisor-intent.json (if
// any), removes the daemons this install owns for m.Name (all rows on a full
// install, or only daemonFilter's row on a partial install), appends the
// daemons this install plans for m, and returns the merged file plus the prior raw bytes
// (for rollback). Ownership-preserving: daemons for OTHER servers are left
// untouched.
//
// The daemon set this install contributes for m.Name is chosen by
// serenaOrPlanDaemons:
//
//   - Workspace-scoped dynamic-pool manifest (m.DaemonTemplate != nil) WITH a
//     non-empty workspaces snapshot -> the D.2 per-workspace serena fan-out
//     (one SupervisorDaemon per registered serena workspace), keyed by the
//     canonical SerenaTaskNameForWorkspace task name.
//   - Otherwise (a workspace-scoped manifest with no registered workspaces)
//     -> supervisorDaemonsFromPlan(m, daemonFilter): the static per-daemon descriptors
//     (empty for a template-only manifest with no workspaces). This keeps the
//     template-only-no-workspaces path byte-identical to the pre-D.3b
//     behavior.
//
// MaintenanceTimers are merged (not blindly carried verbatim) by
// mergeServerWeeklyRefreshTimer so a GLOBAL manifest's `weekly_refresh`
// setting is honored on the supervisor model:
//
//   - For a GLOBAL full install (m.Kind != KindWorkspaceScoped &&
//     m.WeeklyRefresh && daemonFilter == ""), this server's prior
//     server-weekly-refresh timer is REPLACED by a freshly-materialized one
//     (`restart --server <m.Name>`, Kind "server-weekly-refresh" — the cadence
//     the supervisor maintenance scheduler already dispatches on, supervise_-
//     maintenance.go:333). Pre-Phase-F this was the per-server
//     `mcp-local-hub-<server>-weekly-refresh` scheduler task (install.go:1230);
//     Phase F routes global installs through SkipSchedulerTasks=true, so the
//     timer must materialize HERE or the weekly restart is silently dropped.
//     A prior timer's operator off-switch (Enabled=&false) is preserved across
//     the replace so a deliberately-disabled timer is not silently re-enabled.
//   - For a GLOBAL full install with weekly_refresh=false, this server's prior
//     server-weekly-refresh timer is DROPPED, mirroring the legacy scheduler
//     path's prune of `mcp-local-hub-<server>-weekly-refresh` when the manifest
//     flag flips off.
//   - Otherwise (workspace-scoped manifest — serena sets weekly_refresh=false;
//     or a filtered/per-daemon install) the prior set is carried through
//     VERBATIM (this server's timers AND every sibling's). Preserving the prior
//     set untouched means an operator's deliberately-disabled timer survives
//     every re-install of an unrelated/partial manifest.
//
// Siblings' timers are ALWAYS preserved untouched; only THIS server's
// server-weekly-refresh timer is replaced, mirroring the Daemons replace-by-
// server logic above for full installs. Filtered installs replace only the
// selected daemon row and preserve this server's other daemon rows verbatim.
func (a *API) buildMergedSupervisorIntent(m *config.ServerManifest, intentPath string, workspaces []WorkspaceEntry, daemonFilter string, w io.Writer) (merged, prior *SupervisorIntentFile, priorExisted bool, err error) {
	prior, existed, err := readSupervisorIntentForMerge(intentPath)
	if err != nil {
		return nil, nil, false, err
	}
	ownershipScope := supervisorIntentOwnershipScopeForManifest(m, workspaces, "")

	// On a filtered install, the fresh row supervisorDaemonsFromPlan
	// materializes is keyed by this canonical task name. A prior row that
	// names the SAME task must be dropped here regardless of how its Server /
	// Daemon fields are populated, or it survives the field-based filter and
	// the fresh row appends alongside it -> DUPLICATE task_name entries
	// (duplicate status rows, ambiguous stop/restart selection). Legacy /
	// older-writer rows can carry a blank or stale Daemon (other intent
	// readers tolerate that by re-deriving from the task name via
	// ParseManagedTaskName), so the field match alone is not sufficient (bot
	// PR #284 P2). Empty daemonFilter -> empty filteredTaskName -> the
	// task-name fallback is inert (the full-install branch already drops every
	// row for m.Name).
	var filteredTaskName string
	if daemonFilter != "" {
		filteredTaskName = canonicalIntentTaskKey("mcp-local-hub-" + m.Name + "-" + daemonFilter)
	}
	kept := make([]SupervisorDaemon, 0, len(prior.Daemons))
	for _, d := range prior.Daemons {
		// Full install: drop every row this server owns. The Server-field
		// match alone is not sufficient — a legacy / older-writer row can
		// carry a BLANK Server while its canonical TaskName belongs to this
		// server (e.g. `\mcp-local-hub-demo-alpha` with Server=""). Such a
		// row survives the field-only filter and the fresh row appends
		// alongside it -> DUPLICATE task_name entries (the #284 fix added the
		// TaskName fallback only for the FILTERED path; the full path needs
		// the same robustness, bot PR #288 F4). supervisorIntentRowOwnedBy
		// re-derives ownership from the task name via ParseManagedTaskName so
		// a blank-Server row keyed to this server is dropped for replacement.
		if daemonFilter == "" {
			if supervisorIntentRowOwnedByScope(d, m.Name, ownershipScope) {
				continue // replaced below
			}
			kept = append(kept, d)
			continue
		}
		// Filtered install: drop the selected daemon's row. Match on the
		// populated fields first, then fall back to the canonical task name so
		// a blank-/stale-Daemon legacy row for the same task is not duplicated.
		if (d.Server == m.Name && d.Daemon == daemonFilter) ||
			canonicalIntentTaskKey(d.TaskName) == filteredTaskName {
			continue // replaced below
		}
		kept = append(kept, d)
	}
	kept = append(kept, serenaOrPlanDaemons(m, workspaces, daemonFilter, w)...)

	merged = &SupervisorIntentFile{
		Version:           1,
		UpdatedAt:         time.Now().UTC().Format(time.RFC3339Nano),
		Daemons:           kept,
		MaintenanceTimers: mergeServerWeeklyRefreshTimer(m, daemonFilter, prior.MaintenanceTimers),
		StrictMode:        prior.StrictMode,
		// Stops is the E2 sub-block — the SOLE per-daemon stop source. The
		// install merge does not own stops; dropping it here would wipe
		// every operator stop across ALL servers on any install (bot PR
		// #284 P2 — cross-phase E2×F regression). The post-success
		// recordStopIntentAs / ClearStopIntent writers adjust ONLY the
		// installed daemons' entries afterwards.
		Stops: prior.Stops,
	}
	return merged, prior, existed, nil
}

type supervisorIntentOwnershipScope struct {
	taskNames  map[string]struct{}
	daemonKeys map[string]struct{}

	// legacyPrefixFallback enables the sibling-safe legacy-prefix arm in
	// supervisorIntentRowOwnedByScope for FULL-SERVER-cleanup callers (full
	// reinstall replace + manifest-backed uninstall). It is set ONLY by
	// supervisorIntentOwnershipScopeForManifest, whose callers are exactly
	// those full-cleanup paths. A nil scope or a scope built any other way
	// leaves it false, so additive / per-daemon paths never start claiming
	// legacy sibling rows.
	//
	// r31-F1 ↔ r33-2 tension: the exact-name arm (taskNames) was added by
	// r31-F1 so a blank-Server row `\mcp-local-hub-demo-alpha-beta` is NOT
	// claimed by server `demo` when it belongs to server `demo-alpha` /
	// daemon `beta`. But that exact-ONLY match has the opposite failure
	// (bot r33-2): a blank-Server row for a daemon that was RENAMED or
	// REMOVED from the manifest is no longer in taskNames, so a full
	// reinstall / manifest uninstall of the server cannot claim it and the
	// stale descriptor survives forever. The legacy-prefix fallback below
	// reclaims those rows while staying sibling-safe via installedServers.
	legacyPrefixFallback bool

	// installedServers is the set of ALL currently-known server names
	// (manifest catalog) when legacyPrefixFallback is enabled. The
	// disambiguator claims a blank-Server prefix row for `server` ONLY when
	// no OTHER installed server name is a LONGER prefix of the same task —
	// so if both `demo` and `demo-alpha` are installed, `demo-alpha` (the
	// longest matching installed prefix) owns `\mcp-local-hub-demo-alpha-beta`
	// and `demo` must not claim it (preserving r31-F1).
	installedServers map[string]struct{}
}

func (s *supervisorIntentOwnershipScope) addTaskName(taskName string) {
	if s.taskNames == nil {
		s.taskNames = make(map[string]struct{})
	}
	s.taskNames[canonicalIntentTaskKey(taskName)] = struct{}{}
}

func (s *supervisorIntentOwnershipScope) addDaemonKey(daemon string) {
	if daemon == "" {
		return
	}
	if s.daemonKeys == nil {
		s.daemonKeys = make(map[string]struct{})
	}
	s.daemonKeys[daemon] = struct{}{}
}

func (s *supervisorIntentOwnershipScope) addInstalledServer(server string) {
	if server == "" {
		return
	}
	if s.installedServers == nil {
		s.installedServers = make(map[string]struct{})
	}
	s.installedServers[server] = struct{}{}
}

func supervisorIntentOwnershipScopeForManifest(m *config.ServerManifest, workspaces []WorkspaceEntry, daemonFilter string) *supervisorIntentOwnershipScope {
	scope := &supervisorIntentOwnershipScope{}
	if m == nil || m.Name == "" {
		return scope
	}
	// Every caller of this builder is a FULL-SERVER-cleanup path (full
	// reinstall replace, global full install, dry-run preview, manifest-backed
	// uninstall), so enable the sibling-safe legacy-prefix fallback and capture
	// the installed-server catalog the disambiguator needs. The catalog read is
	// best-effort: on a read failure the set is left empty, which makes the
	// fallback claim any blank-Server prefix row for this server (no sibling
	// proof available) — the same outcome as the pre-fix prefix match for the
	// scope==nil residual, and strictly better than the exact-only arm that
	// stranded the stale descriptor (r33-2). The owning server itself is NOT
	// excluded here; supervisorIntentRowOwnedByScope only consults OTHER
	// installed names (S != server) for the longer-prefix check.
	scope.legacyPrefixFallback = true
	if names, err := listManifestNamesEmbedFirst(); err == nil {
		for _, n := range names {
			scope.addInstalledServer(n)
		}
	}
	if m.Kind != config.KindWorkspaceScoped {
		for _, d := range m.Daemons {
			if daemonFilter != "" && d.Name != daemonFilter {
				continue
			}
			if d.Name == "" {
				continue
			}
			scope.addTaskName("mcp-local-hub-" + m.Name + "-" + d.Name)
			scope.addDaemonKey(d.Name)
		}
	}
	if m.DaemonTemplate != nil {
		for _, ws := range workspaces {
			if ws.WorkspacePath == "" {
				continue
			}
			scope.addTaskName(SerenaTaskNameForWorkspace(ws.WorkspacePath))
			daemonKey := string(ws.WorkspaceKey)
			if daemonKey == "" {
				daemonKey = string(WorkspaceKey(ws.WorkspacePath))
			}
			scope.addDaemonKey(daemonKey)
		}
	}
	return scope
}

// supervisorIntentRowOwnedBy reports whether a supervisor-intent daemon row
// belongs to the named server when the caller has only the server string. It
// matches on the populated Server field first, then falls back to the legacy
// prefix match for a BLANK Server row. Residual: that fallback can still claim
// prefix-colliding blank-Server sibling rows; install/uninstall paths that have
// manifest or workspace context must call supervisorIntentRowOwnedByScope.
func supervisorIntentRowOwnedBy(d SupervisorDaemon, server string) bool {
	return supervisorIntentRowOwnedByScope(d, server, nil)
}

// supervisorIntentRowOwnedByScope is the manifest-aware ownership predicate.
// Populated Server rows remain exact. Blank-Server rows are decided in two
// arms:
//
//  1. EXACT (r31-F1, always on for a non-nil scope) — the row's canonical task
//     name is in the current manifest's task set; when the row carries a
//     populated Daemon field, that daemon must also be in the manifest/workspace
//     set, so demo/alpha-beta cannot claim demo-alpha/beta solely by task-prefix
//     shape.
//
//  2. LEGACY-PREFIX FALLBACK (r33-2, gated on scope.legacyPrefixFallback — set
//     ONLY by supervisorIntentOwnershipScopeForManifest, i.e. the full-server-
//     cleanup callers) — for a blank-Server row NOT in the exact set, claim it
//     when `server` is the MOST-SPECIFIC (longest) INSTALLED-server prefix of
//     the task's `mcp-local-hub-<X>` portion: `<X>` starts with `server+"-"`
//     AND no OTHER installed server S (S != server, len(S) > len(server)) is
//     also a prefix (`<X>` starts with `S+"-"`). This reclaims a renamed/removed
//     daemon's stale descriptor on a full reinstall / manifest uninstall while
//     keeping r31-F1's sibling preservation: if demo-alpha is ALSO installed it
//     is the longer prefix and owns the row, so demo does not claim it.
//
// The scope==nil path keeps the documented legacy prefix residual (used by the
// server-string-only assertion helper and the non-manifest uninstall wrapper).
func supervisorIntentRowOwnedByScope(d SupervisorDaemon, server string, scope *supervisorIntentOwnershipScope) bool {
	if server == "" {
		return false
	}
	if d.Server == server {
		return true
	}
	if d.Server != "" {
		return false
	}
	if scope != nil {
		// Arm 1 — exact manifest-derived task membership (r31-F1, precise).
		if _, ok := scope.taskNames[canonicalIntentTaskKey(d.TaskName)]; ok {
			if d.Daemon != "" && len(scope.daemonKeys) > 0 {
				if _, ok := scope.daemonKeys[d.Daemon]; !ok {
					return false
				}
			}
			return true
		}
		// Arm 2 — sibling-safe legacy-prefix fallback (r33-2, full-cleanup only).
		if scope.legacyPrefixFallback {
			return blankServerRowOwnedByLongestInstalledPrefix(d.TaskName, server, scope.installedServers)
		}
		return false
	}
	prefix := canonicalIntentTaskKey("mcp-local-hub-" + server + "-")
	return strings.HasPrefix(canonicalIntentTaskKey(d.TaskName), prefix)
}

// ServerOwningTaskByLongestInstalledPrefix returns the installed server name that
// owns taskName under the longest-installed-prefix rule (the sibling-safe
// disambiguator for blank-Server legacy rows: \mcp-local-hub-demo-alpha-beta is
// owned by demo-alpha if installed, else by demo). ok=false when no installed
// server name is a prefix of the task's <server>-... portion.
//
// This is the SINGLE owner of the longest-installed-prefix scan. The boolean
// predicate blankServerRowOwnedByLongestInstalledPrefix below (and its cli-side
// mirror) compose this function so the longer-sibling disambiguation has exactly
// one implementation. The scan only considers a server name S a prefix when the
// task portion starts with `S+"-"` (so `demo` does not match `demonstration-x`,
// and a bare `\mcp-local-hub-demo` with no daemon segment yields ok=false), and
// returns the LONGEST such installed S. An empty/nil installedServers set yields
// ok=false (no installed name can be a prefix) — callers that want the safe
// "claim-any" full-cleanup fallback handle ok=false explicitly.
func ServerOwningTaskByLongestInstalledPrefix(taskName string, installedServers map[string]struct{}) (string, bool) {
	const taskPrefix = `\mcp-local-hub-`
	canonical := canonicalIntentTaskKey(taskName)
	portion, ok := strings.CutPrefix(canonical, taskPrefix)
	if !ok {
		return "", false
	}
	owner := ""
	for s := range installedServers {
		if s == "" {
			continue
		}
		// `<X>` must be `s-<daemon...>`: s followed by a hyphen (a bare `s`
		// with no daemon segment is degenerate and not a daemon row this
		// prefix server owns).
		if !strings.HasPrefix(portion, s+"-") {
			continue
		}
		if len(s) > len(owner) {
			owner = s
		}
	}
	if owner == "" {
		return "", false
	}
	return owner, true
}

// blankServerRowOwnedByLongestInstalledPrefix decides whether a blank-Server
// supervisor-intent row whose task name is `mcp-local-hub-<X>` is owned by
// `server` under the longest-installed-prefix disambiguator (r33-2). It returns
// true IFF `<X>` starts with `server+"-"` AND no OTHER installed server name S
// (S != server, len(S) > len(server)) is also a prefix of `<X>` in the same
// `S+"-"` form. installedServers may be empty (catalog read failed) — then the
// sibling check is vacuous and any prefix-matching row is claimed, which is the
// safe full-cleanup outcome (no sibling proof exists to defer to).
//
// The longer-installed-sibling scan is delegated to the single-owner
// ServerOwningTaskByLongestInstalledPrefix; `server` itself need NOT be
// installed (a removed/renamed daemon's prefix server may be absent from the
// catalog), so the candidate-specific portion+prefix check stays local here.
func blankServerRowOwnedByLongestInstalledPrefix(taskName, server string, installedServers map[string]struct{}) bool {
	const taskPrefix = `\mcp-local-hub-`
	canonical := canonicalIntentTaskKey(taskName)
	portion, ok := strings.CutPrefix(canonical, taskPrefix)
	if !ok {
		return false
	}
	// `<X>` must be `server-<daemon...>`: starts with server followed by a
	// hyphen (a bare `server` with no daemon segment is degenerate and not a
	// daemon row this prefix server should claim).
	if !strings.HasPrefix(portion, server+"-") {
		return false
	}
	// A strictly-LONGER installed server name that is also a prefix owns the
	// row, so `server` must not claim it (preserves r31-F1). Because the owner
	// returned is the LONGEST installed prefix, `len(owner) > len(server)`
	// holds iff some installed prefix is strictly longer than `server`; an
	// owner equal in length is `server` itself (a portion has exactly one
	// prefix of any given length), and an empty catalog yields ok=false →
	// claim (the safe no-sibling-proof fallback).
	if owner, ok := ServerOwningTaskByLongestInstalledPrefix(taskName, installedServers); ok && len(owner) > len(server) {
		return false
	}
	return true
}

// maintenanceTimerOwnedBy reports whether a MaintenanceTimer belongs to the
// named server. Like supervisorIntentRowOwnedBy it matches the populated
// Server field first, then falls back to the timer's canonical Name (the
// `\mcp-local-hub-<server>-weekly-refresh` task name) via ParseManagedTaskName
// so a blank-Server legacy timer is still recognized. Used by the uninstall
// cleanup to drop this server's server-weekly-refresh timer while preserving
// every sibling's.
func maintenanceTimerOwnedBy(tm MaintenanceTimer, server string) bool {
	if server == "" {
		return false
	}
	if tm.Server == server {
		return true
	}
	parsedServer, _ := ParseManagedTaskName(tm.Name)
	return parsedServer == server
}

// removeServerFromSupervisorIntent removes every supervisor-intent artifact a
// v0.6 GLOBAL uninstall must clear for the named server: its Daemons rows, its
// server-weekly-refresh MaintenanceTimer, and its Stops sub-block entries —
// all under the canonical supervisor-intent flock, re-reading fresh and
// writing back so EVERY other server's rows / timers / stops are preserved
// verbatim (the same read-merge-fresh + write contract install uses, 4ed263d).
//
// Why this is needed (bot PR #288 F1): Phase F routes a global install through
// supervisor-intent.json descriptor rows (no per-daemon scheduler task), so
// the uninstall path's scheduler-task deletion (sch.List(prefix) + sch.Delete)
// removes NOTHING for a v0.6 global — the descriptor survives and the
// supervisor keeps (re)spawning the daemon forever. This is the symmetric
// cleanup: install writes the rows, uninstall removes them.
//
// Ownership is matched via supervisorIntentRowOwnedBy / maintenanceTimerOwnedBy
// / parsed Stops key, so blank-Server legacy rows keyed to this server are
// caught too (same robustness as the F4 full-install replace loop).
//
// A missing intent file is a no-op (nothing to clean). Best-effort by
// contract: the caller (Uninstall) treats a cleanup error as a warning, never
// a hard failure — uninstall is idempotent and must remove the scheduler
// tasks + client entries regardless. The function reports whether it changed
// anything so the caller can skip the reconcile nudge when there was nothing
// to remove.
func (a *API) removeServerFromSupervisorIntent(server string) (changed bool, err error) {
	changed, _, _, err = a.removeServerFromSupervisorIntentCore(context.Background(), server, nil, false)
	return changed, err
}

func (a *API) removeServerFromSupervisorIntentCore(ctx context.Context, server string, ownershipScope *supervisorIntentOwnershipScope, captureLivePIDs bool) (changed bool, removedDaemons []SupervisorDaemon, removedPIDByTask map[string]int, err error) {
	if server == "" {
		return false, nil, nil, fmt.Errorf("removeServerFromSupervisorIntent: empty server")
	}
	stateDir, err := DaemonStateDir()
	if err != nil {
		return false, nil, nil, fmt.Errorf("resolve state dir: %w", err)
	}
	intentPath := joinStateFilePath(stateDir, supervisorIntentFileLeaf)

	if captureLivePIDs {
		// Capture before taking the supervisor-intent flock. The locked cleanup
		// below recomputes the authoritative removed rows; rows that appear after
		// this pre-read have no captured PID and fall back to their descriptor
		// port after the reconcile nudge.
		preCaptureTargets, err := preliminarySupervisorTargetsForServerScope(intentPath, server, ownershipScope)
		if err != nil {
			return false, nil, nil, err
		}
		removedPIDByTask = supervisorOwnedLivePIDsForTargets(ctx, preCaptureTargets)
	}

	// Serialize the read-merge-write as ONE critical section against every
	// other supervisor-intent writer (install / migrate / autostart / stop),
	// exactly as buildMergedSupervisorIntent's callers do. Holding the flock
	// across the read AND the write means a concurrent install for a sibling
	// server cannot interleave and clobber the other's rows. Because we hold
	// the lock, the write uses the lock-free writeSupervisorIntentLockHeld
	// body (re-entering WriteSupervisorIntent would deadlock).
	lock := flock.New(intentPath + supervisorIntentLockSuffix)
	if err := lock.Lock(); err != nil {
		return false, nil, nil, fmt.Errorf("supervisor-intent flock %s: %w", intentPath+supervisorIntentLockSuffix, err)
	}
	defer func() { _ = lock.Unlock() }()

	prior, existed, err := readSupervisorIntentForMerge(intentPath)
	if err != nil {
		return false, nil, nil, err
	}
	if !existed {
		return false, nil, nil, nil // no intent file → nothing to clean
	}

	keptDaemons := make([]SupervisorDaemon, 0, len(prior.Daemons))
	for _, d := range prior.Daemons {
		if supervisorIntentRowOwnedByScope(d, server, ownershipScope) {
			changed = true
			removedDaemons = append(removedDaemons, d)
			continue
		}
		keptDaemons = append(keptDaemons, d)
	}

	keptTimers := make([]MaintenanceTimer, 0, len(prior.MaintenanceTimers))
	for _, tm := range prior.MaintenanceTimers {
		if maintenanceTimerOwnedBy(tm, server) {
			changed = true
			continue
		}
		keptTimers = append(keptTimers, tm)
	}

	keptStops := pruneStopsForRemovedSupervisorTargets(prior.Stops, removedDaemons)
	if !stopsMapsEqual(prior.Stops, keptStops) {
		changed = true
	}

	if !changed {
		return false, nil, nil, nil // this server owned nothing in the intent
	}

	merged := &SupervisorIntentFile{
		Version:           1,
		UpdatedAt:         time.Now().UTC().Format(time.RFC3339Nano),
		Daemons:           keptDaemons,
		MaintenanceTimers: keptTimers,
		StrictMode:        prior.StrictMode,
		Stops:             keptStops,
	}
	if err := writeSupervisorIntentLockHeld(intentPath, merged); err != nil {
		return false, nil, nil, fmt.Errorf("write supervisor intent %s: %w", intentPath, err)
	}
	return true, removedDaemons, removedPIDByTask, nil
}

// nudgeSupervisorReconcileAfterUninstall asks a running supervisor to
// re-read supervisor-intent.json and converge — so the daemon whose
// descriptor row removeServerFromSupervisorIntent just deleted is terminated
// promptly instead of surviving until the next IntentWatcher poll (≤60s).
// Best-effort: an unreachable supervisor (ErrSupervisorIPCUnavailable) means
// nothing is currently spawning the daemon, so there is nothing to nudge; any
// other reconcile error is non-fatal to the uninstall. Uses the same
// supervisorReconcileApplyFn seam as stopSupervisorOwnedDaemons. On
// IPC-unavailable it runs the same live-owner probe as the manifest-install
// removed-target nudge (demoteIPCUnavailableWhenOwnerAlive): a live-but-wedged
// supervisor still tracks the daemon, so the removed-target force-kill must be
// skipped rather than fight the reaper.
func nudgeSupervisorReconcileAfterUninstall(ctx context.Context) supervisorReconcileNudgeResult {
	return demoteIPCUnavailableWhenOwnerAlive(nudgeSupervisorReconcileApply(ctx))
}

// removeServerFromSupervisorIntentBestEffort is the uninstall-side wrapper
// shared by the manifest-backed (Uninstall) and retired-manifest
// (uninstallWithoutManifest) paths. It runs the supervisor-intent cleanup and,
// only when the cleanup actually removed something, nudges a running
// supervisor to reconcile so the now-descriptorless daemon is terminated
// promptly. A cleanup error is recorded as a TaskDeleteWarns entry (the
// closest existing report channel for managed-daemon-state removal) and never
// aborts the uninstall — uninstall is idempotent and must complete the
// scheduler-task + client-entry removal regardless (bot PR #288 F1).
func (a *API) removeServerFromSupervisorIntentBestEffort(server string, report *UninstallReport) {
	a.removeServerFromSupervisorIntentBestEffortScope(server, retiredManifestUninstallScope(), report)
}

// retiredManifestUninstallScope builds the sibling-safe ownership scope for the
// no-manifest (retired-manifest) uninstall path. The manifest is gone, so there
// are no taskNames/daemonKeys to seed Arm 1 — but ownership must still defer to a
// longer-installed hyphen sibling, exactly as the manifest-backed uninstall does.
//
// r36-3: before this, the no-manifest path passed scope==nil, which routed
// supervisorIntentRowOwnedByScope to the RAW HasPrefix residual. That raw prefix
// match force-PRUNES + KILLS a hyphen-sibling's blank-Server row — e.g. retiring
// `gdb` would mis-claim installed sibling `gdb-remote`'s row
// \mcp-local-hub-gdb-remote-default. With taskNames empty, ownership now routes
// through Arm 2 (legacy-prefix fallback), where
// blankServerRowOwnedByLongestInstalledPrefix preserves the sibling. The
// installed-server catalog read is best-effort: on a read failure the set is left
// empty, which makes the fallback claim any blank-Server prefix row for this
// server (no sibling proof available) — the same outcome as the prior scope==nil
// residual, never worse.
func retiredManifestUninstallScope() *supervisorIntentOwnershipScope {
	scope := &supervisorIntentOwnershipScope{legacyPrefixFallback: true}
	if names, err := listManifestNamesEmbedFirst(); err == nil {
		for _, n := range names {
			scope.addInstalledServer(n)
		}
	}
	return scope
}

func (a *API) removeServerFromSupervisorIntentBestEffortForManifest(m *config.ServerManifest, report *UninstallReport) {
	server := ""
	var ownershipScope *supervisorIntentOwnershipScope
	if m != nil {
		server = m.Name
		ownershipScope = supervisorIntentOwnershipScopeForManifest(m, nil, "")
	}
	a.removeServerFromSupervisorIntentBestEffortScope(server, ownershipScope, report)
}

func (a *API) removeServerFromSupervisorIntentBestEffortScope(server string, ownershipScope *supervisorIntentOwnershipScope, report *UninstallReport) {
	changed, removedTargets, removedPIDByTask, err := a.removeServerFromSupervisorIntentCore(context.Background(), server, ownershipScope, true)
	if err != nil {
		if report != nil {
			report.TaskDeleteWarns = append(report.TaskDeleteWarns,
				fmt.Sprintf("remove %s supervisor-intent rows: %v", server, err))
		}
		return
	}
	if changed {
		// Only nudge when a descriptor was actually removed: a no-op cleanup
		// (this server owned nothing in the intent — e.g. a remote-http or
		// already-cleaned install) has no daemon to terminate.
		nudgeResult := nudgeSupervisorReconcileAfterUninstall(context.Background())
		killRemovedSupervisorTargetsAfterUninstall(server, removedTargets, removedPIDByTask, nudgeResult, report)
	}
}

func killRemovedSupervisorTargetsAfterUninstall(server string, targets []SupervisorDaemon, pidByTask map[string]int, nudgeResult supervisorReconcileNudgeResult, report *UninstallReport) {
	killRemovedSupervisorTargetsAfterNudge(
		targets,
		pidByTask,
		nudgeResult,
		fmt.Sprintf("re-run uninstall or `mcphub stop --force %s`", server),
		func(format string, args ...any) {
			if report != nil {
				report.TaskDeleteWarns = append(report.TaskDeleteWarns, fmt.Sprintf(format, args...))
			}
		},
	)
}

// mergeServerWeeklyRefreshTimer is the single owner of the maintenance-timer
// merge for a per-server install. It mirrors buildMergedSupervisorIntent's
// Daemons replace-by-server logic: every sibling server's timers pass through
// untouched, and THIS server's server-weekly-refresh timer is replaced (when
// the manifest wants one), dropped (when a full global install turns it off),
// or left verbatim (for workspace-scoped / filtered installs that do not own
// the whole-server timer).
//
// Materialization condition (matches the pre-Phase-F Pass A gate at
// install.go:1230 — `m.WeeklyRefresh && opts.DaemonFilter == ""`): the manifest
// is global (not workspace-scoped), opts weekly_refresh, and this is a FULL
// install (no daemon filter). A weekly restart restarts the WHOLE server, so a
// single-daemon filtered install never owns it.
//
// When the condition holds, the freshly-materialized timer carries the exact
// shape the legacy scheduler task used (`restart --server <m.Name>`, canonical
// task name `\mcp-local-hub-<server>-weekly-refresh`), and preserves any prior
// timer's operator off-switch (Enabled=&false) so a re-install never silently
// re-enables a deliberately-disabled timer.
func mergeServerWeeklyRefreshTimer(m *config.ServerManifest, daemonFilter string, prior []MaintenanceTimer) []MaintenanceTimer {
	if m.Kind == config.KindWorkspaceScoped || daemonFilter != "" {
		return prior
	}

	wantName := canonicalIntentTaskKey("mcp-local-hub-" + m.Name + "-weekly-refresh")

	// Preserve every sibling timer; drop this server's prior
	// server-weekly-refresh timer (it is either re-materialized below or
	// intentionally removed when weekly_refresh=false). Capture its Enabled
	// off-switch so a deliberately-disabled timer is not re-enabled.
	out := make([]MaintenanceTimer, 0, len(prior)+1)
	var priorEnabled *bool
	for _, tm := range prior {
		if serverWeeklyRefreshTimerOwnedBy(tm, m.Name) {
			priorEnabled = tm.Enabled
			continue // replaced below
		}
		out = append(out, tm)
	}
	if !m.WeeklyRefresh {
		return out
	}

	// Resolve the mcphub binary the supervisor will exec, matching
	// supervisorDaemonsFromPlan's fallback posture (bare name when
	// `mcphub setup` has not run; the install preflight surfaces that upstream).
	command, perr := canonicalMcphubPath()
	if perr != nil {
		command = mcphubShortName
	}

	out = append(out, MaintenanceTimer{
		Name:    wantName,
		Kind:    "server-weekly-refresh",
		Server:  m.Name,
		Command: command,
		Args:    []string{"restart", "--server", m.Name},
		Enabled: priorEnabled,
	})
	return out
}

func serverWeeklyRefreshTimerOwnedBy(tm MaintenanceTimer, server string) bool {
	return tm.Kind == "server-weekly-refresh" && maintenanceTimerOwnedBy(tm, server)
}

// fanOutAuditTaskNames returns the per-workspace task names the pre-mutation
// audit must fail-close on for a workspace-scoped fan-out install, derived from
// the MATERIALIZED supervisor-intent daemon rows for this server (the
// SerenaTaskNameForWorkspace names BuildSupervisorDaemonsForSerena produced and
// filterExistingWorkspaceRows already pruned to live workspaces). Returning
// these names — rather than the empty m.Daemons-derived list — makes
// recordInstallAuditForTasks emit a server-install audit entry per workspace
// daemon and fail-close BEFORE any intent/scheduler mutation.
//
// Returns nil for a non-fan-out (global) manifest: installPlan then falls back
// to the manifest-derived installAuditTaskNames list, preserving the legacy
// audit task set exactly. The guard mirrors serenaOrPlanDaemons's branch
// condition so the audited names match the daemon rows actually written.
func fanOutAuditTaskNames(m *config.ServerManifest, desired *SupervisorIntentFile) []string {
	if m.DaemonTemplate == nil || desired == nil {
		return nil
	}
	var names []string
	for _, d := range desired.Daemons {
		if d.Server == m.Name {
			names = append(names, d.TaskName)
		}
	}
	return names
}

// serenaOrPlanDaemons returns the SupervisorDaemon descriptors this install
// contributes for m. It fans out per registered serena workspace when m is a
// workspace-scoped dynamic-pool manifest (DaemonTemplate != nil) and the
// workspaces snapshot is non-empty; otherwise it falls back to the static
// plan-derived set (supervisorDaemonsFromPlan), filtered by daemonFilter when
// the operator requested a single daemon, which is what api.Install and
// the template-only-no-workspaces path use.
//
// BuildSupervisorDaemonsForSerena itself guards on m.DaemonTemplate != nil,
// m.Kind == KindWorkspaceScoped, and len(workspaces) > 0 (returning nil
// otherwise), so a non-workspace-scoped manifest or an empty workspaces
// snapshot deterministically takes the plan-derived branch here.
func serenaOrPlanDaemons(m *config.ServerManifest, workspaces []WorkspaceEntry, daemonFilter string, w io.Writer) []SupervisorDaemon {
	if m.DaemonTemplate != nil && len(workspaces) > 0 {
		// FIX 3 — drop stale workspace rows (path no longer exists on disk)
		// BEFORE the fan-out. BuildSupervisorDaemonsForSerena's contract
		// (supervisor_intent_build.go §"Filesystem existence") leaves stale-
		// path filtering to the caller: it emits a descriptor for every row
		// verbatim, and the supervisor sets cmd.Dir = d.Workspace
		// unconditionally before cmd.Start, so a removed/moved workspace dir
		// makes the daemon spawn-loop. Filter here and emit an operator-
		// visible warn per dropped row so the prune is never silent.
		live := filterExistingWorkspaceRows(m.Name, workspaces, w)
		// Resolve the mcphub binary the supervisor will exec for each
		// descriptor. canonicalMcphubPath only fails when `mcphub setup`
		// has not run, which the install preflight already surfaces
		// upstream; fall back to the bare name (the descriptor stays
		// well-formed and the supervisor resolves it on PATH), matching
		// supervisorDaemonsFromPlan's fallback posture.
		mcphubPath, perr := canonicalMcphubPath()
		if perr != nil {
			mcphubPath = mcphubShortName
		}
		// ManifestHash is left empty here: this slice does not compute a
		// content hash for the parsed manifest, and the field is
		// diagnostic provenance only (the supervisor spawns from the
		// self-sufficient argv, not the hash). A later slice may thread a
		// real hash through.
		if serena := BuildSupervisorDaemonsForSerena(m, live, "", mcphubPath); serena != nil {
			return serena
		}
	}
	return supervisorDaemonsFromPlan(m, daemonFilter)
}

// workspacePathStale reports whether a workspace path is stale (deleted /
// moved) for the purpose of dropping its daemon row before the fan-out. It is
// a PURE predicate: it does an os.Stat and classifies the result, with no
// logging, no event emission, and no other side effect. The two call sites —
// filterExistingWorkspaceRows (non-dry path, which emits the warn) and the
// dry-run preview (label only) — share this one liveness rule so they can
// never diverge.
//
// Classification:
//   - empty path -> NOT stale (the fan-out helper itself skips empty-path
//     rows, and statting "" would spuriously report not-exist).
//   - os.Stat IsNotExist -> stale (the directory entry is gone).
//   - any other stat error (permission denied, I/O error) -> NOT stale: a
//     transient stat failure must not silently drop a live workspace; the
//     supervisor surfaces a real spawn failure if the path is genuinely
//     unusable.
//
// os.Stat (not os.Lstat) is intentional: a workspace reachable only through a
// symlinked directory is still a live workspace; we care whether the target
// resolves, not whether the entry itself is a symlink.
func workspacePathStale(path string) bool {
	if path == "" {
		return false
	}
	if _, statErr := os.Stat(path); statErr != nil && os.IsNotExist(statErr) {
		return true
	}
	return false
}

// filterExistingWorkspaceRows returns the subset of workspaces whose
// WorkspacePath is not stale per workspacePathStale. A dropped row (stale
// path) gets a warn emitted to w AND to supervisor-events.log (best-effort)
// so the skipped row is operator-visible rather than silently pruned.
func filterExistingWorkspaceRows(server string, workspaces []WorkspaceEntry, w io.Writer) []WorkspaceEntry {
	live := make([]WorkspaceEntry, 0, len(workspaces))
	for _, ws := range workspaces {
		if workspacePathStale(ws.WorkspacePath) {
			if w != nil {
				fmt.Fprintf(w, "⚠ Skipping stale workspace %q (path no longer exists): %s daemon row dropped from supervisor-intent\n", ws.WorkspacePath, server)
			}
			emitStaleWorkspaceSkippedEvent(server, ws.WorkspacePath)
			continue
		}
		live = append(live, ws)
	}
	return live
}

// previewWorkspaceFanOut prints, on a DRY RUN, a one-line preview per
// workspace of the per-workspace supervisor-intent daemon rows the real
// (non-dry) path would write for a DaemonTemplate manifest. The legacy
// BuildPlanWithOpts plan carries zero scheduler tasks for such a manifest, so
// without this preview a dry run of the main fan-out case reports no planned
// changes.
//
// It is intentionally side-effect-free beyond writing to w: it does NOT read
// or parse the existing supervisor-intent.json, acquires no flock, writes no
// file, and emits NO supervisor-events.log entry. Stale rows (path gone) are
// labelled via the same pure workspacePathStale predicate the non-dry path
// uses, but here the label is print-only — no warn event is emitted (the
// real install path owns event emission).
//
// No-op unless the manifest is a DaemonTemplate manifest with at least one
// workspace — the same condition under which the real path fans out.
//
// The preview mirrors BuildSupervisorDaemonsForSerena's row filters so it
// reflects what the real install would actually write: a row whose path is
// gone is labelled stale, and a row whose Language is not the serena sentinel
// is labelled non-serena. Both are "would be skipped", so the header's
// written-row count is the real fan-out size, not the raw snapshot length.
func previewWorkspaceFanOut(w io.Writer, m *config.ServerManifest, workspaces []WorkspaceEntry) {
	if m.DaemonTemplate == nil || len(workspaces) == 0 {
		return
	}
	type previewRow struct{ task, path, label string }
	rows := make([]previewRow, 0, len(workspaces))
	writeCount := 0
	for _, ws := range workspaces {
		if ws.WorkspacePath == "" {
			continue
		}
		r := previewRow{task: SerenaTaskNameForWorkspace(ws.WorkspacePath), path: ws.WorkspacePath}
		switch {
		case ws.Language != SerenaLanguageSentinel:
			// BuildSupervisorDaemonsForSerena skips non-sentinel rows, so the
			// real path would write no daemon for this workspace.
			r.label = "  [skipped: not a serena workspace]"
		case workspacePathStale(ws.WorkspacePath):
			r.label = "  [stale: path no longer exists — would be skipped]"
		default:
			writeCount++
		}
		rows = append(rows, r)
	}
	fmt.Fprintf(w, "\nSupervisor-intent daemon rows to write for server %q (%d of %d workspace(s)):\n", m.Name, writeCount, len(rows))
	for _, r := range rows {
		fmt.Fprintf(w, "    • %s  ->  %s%s\n", r.task, r.path, r.label)
	}
}

// emitStaleWorkspaceSkippedEvent records a best-effort warn to
// supervisor-events.log when a stale workspace row is dropped during the
// install fan-out. Mirrors emitStateFileFallbackEvent's channel discipline:
// supervisor-events.log is the canonical audit channel for supervisor-domain
// events; a log failure never blocks the install.
func emitStaleWorkspaceSkippedEvent(server, workspacePath string) {
	stateDir, sdErr := DaemonStateDir()
	if sdErr != nil {
		return
	}
	logger, openErr := OpenSupervisorEventLog(filepath.Join(stateDir, SupervisorEventLogFileLeaf))
	if openErr != nil {
		return
	}
	defer func() { _ = logger.Close() }()
	_ = logger.Emit(SupervisorEvent{
		SchemaVersion: SupervisorEventSchemaVersion,
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Severity:      SupervisorEventSeverityWarn,
		Source:        "reconcile",
		Event:         "stale-workspace-skipped",
		TaskName:      SerenaTaskNameForWorkspace(workspacePath),
		Body: map[string]any{
			"server":         server,
			"workspace_path": workspacePath,
			"reason":         "workspace path no longer exists on disk; daemon row dropped before supervisor-intent write to avoid cmd.Dir spawn-loop",
		},
	})
}

// emitSpecBearingInstallRefusedEvent records a best-effort warn to
// supervisor-events.log when the §7.1 spec-bearing write gate refuses an install
// (a supervisor is running, or liveness is undeterminable). This gives durable
// audit parity with the reconcile path's legacy-serena-descriptor-skipped event
// (consultant PR #246 r2 (d)-observability): without it, the gate refusal lived
// only in the returned error string. Best-effort: a log failure never blocks or
// alters the refuse — the returned error is the authoritative operator signal.
func emitSpecBearingInstallRefusedEvent(server, reason string, pid int) {
	stateDir, sdErr := DaemonStateDir()
	if sdErr != nil {
		return
	}
	logger, openErr := OpenSupervisorEventLog(filepath.Join(stateDir, SupervisorEventLogFileLeaf))
	if openErr != nil {
		return
	}
	defer func() { _ = logger.Close() }()
	body := map[string]any{
		"server": server,
		"reason": reason,
		"gate":   "spec-bearing-supervisor-intent-write (design §7.1)",
	}
	if pid > 0 {
		body["supervisor_pid"] = pid
	}
	_ = logger.Emit(SupervisorEvent{
		SchemaVersion: SupervisorEventSchemaVersion,
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Severity:      SupervisorEventSeverityWarn,
		Source:        "install",
		Event:         "spec-bearing-install-refused",
		Body:          body,
	})
}

// emitSpecBearingInstallAllowedUnderLockEvent records a best-effort INFO row to
// supervisor-events.log when the §7.1 spec-bearing write gate is bypassed
// because the caller proved it holds the gate's own supervisor.lock (the migrate
// / serena auto-register interlock seam, Phase 1). It is the audit mirror of
// emitSpecBearingInstallRefusedEvent: the refuse path emits a warn, the verified
// bypass emits an info so an operator can see WHY a spec-bearing write was
// allowed while a lock was held. Best-effort: a log failure never blocks or
// alters the install.
func emitSpecBearingInstallAllowedUnderLockEvent(server, lockPath string) {
	stateDir, sdErr := DaemonStateDir()
	if sdErr != nil {
		return
	}
	logger, openErr := OpenSupervisorEventLog(filepath.Join(stateDir, SupervisorEventLogFileLeaf))
	if openErr != nil {
		return
	}
	defer func() { _ = logger.Close() }()
	_ = logger.Emit(SupervisorEvent{
		SchemaVersion: SupervisorEventSchemaVersion,
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Severity:      SupervisorEventSeverityInfo,
		Source:        "install",
		Event:         "spec-bearing-write-allowed-under-caller-lock",
		Body: map[string]any{
			"server":           server,
			"matched_lock":     lockPath,
			"gate":             "spec-bearing-supervisor-intent-write (design §7.1)",
			"bypass_rationale": "caller holds the gate's supervisor.lock (migrate/auto-register interlock); the held lock is the caller's own handle, not a foreign supervisor",
		},
	})
}

// supervisorDaemonsFromPlan derives the SupervisorDaemon descriptors for a
// global manifest from its daemon list. Each global daemon maps to one
// long-lived supervisor child keyed by the canonical leading-backslash task
// name. Workspace-scoped manifests carry no static Daemons (their fan-out is
// the D.2 helper, out of this slice's scope) so this returns nil for them.
// daemonFilter limits the materialized rows for partial installs; empty means
// all manifest daemons.
func supervisorDaemonsFromPlan(m *config.ServerManifest, daemonFilter string) []SupervisorDaemon {
	if m.Kind == config.KindWorkspaceScoped {
		return nil
	}
	canonical, err := canonicalMcphubPath()
	if err != nil {
		// Fall back to the bare name; the descriptor is still well-formed
		// and the supervisor resolves the binary on PATH. canonicalMcphubPath
		// only fails when `mcphub setup` has not run, which the install
		// preflight already surfaces upstream.
		canonical = mcphubShortName
	}
	out := make([]SupervisorDaemon, 0, len(m.Daemons))
	for _, d := range m.Daemons {
		if daemonFilter != "" && d.Name != daemonFilter {
			continue
		}
		bare := "mcp-local-hub-" + m.Name + "-" + d.Name
		out = append(out, SupervisorDaemon{
			TaskName: canonicalIntentTaskKey(bare),
			Server:   m.Name,
			Daemon:   d.Name,
			Command:  canonical,
			Args:     []string{"daemon", "--server", m.Name, "--daemon", d.Name},
			Env:      cloneStringMap(m.Env),
			Port:     d.Port,
		})
	}
	return out
}

// readSupervisorIntentForMerge reads + parses the intent file. A missing
// file is not an error: it returns an empty (non-nil) SupervisorIntentFile
// and existed=false so callers can distinguish first-install from replace.
func readSupervisorIntentForMerge(path string) (file *SupervisorIntentFile, existed bool, err error) {
	if _, serr := os.Stat(path); serr != nil {
		if os.IsNotExist(serr) {
			return &SupervisorIntentFile{Version: 1}, false, nil
		}
		return nil, false, fmt.Errorf("stat %s: %w", path, serr)
	}
	parsed, perr := ReadSupervisorIntent(path)
	if perr != nil {
		return nil, false, perr
	}
	return parsed, true, nil
}

// preflightSupervisorIntentWrite dry-writes desired to a temp path in the
// same state directory via WriteStateFileAtomic, then removes it. It proves
// the secure-write pipeline (parent-dir DACL gate, atomic rename,
// post-rename re-verify) will accept the real write BEFORE any mutation
// happens, so a doomed install fails fast with a pristine end-state.
func preflightSupervisorIntentWrite(stateDir string, desired *SupervisorIntentFile) error {
	tmp := joinStateFilePath(stateDir, supervisorIntentFileLeaf+".preflight")
	if err := WriteStateFileAtomic(tmp, desired); err != nil {
		return err
	}
	// Best-effort cleanup of the probe file + its flock leaf. A leftover
	// probe file is harmless (it is overwritten next probe) but tidy is
	// better.
	_ = os.Remove(tmp)
	_ = os.Remove(tmp + ".lock")
	return nil
}
