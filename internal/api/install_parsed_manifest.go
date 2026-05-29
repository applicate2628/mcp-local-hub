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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/config"
)

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
	desiredIntent, priorIntent, priorExisted, err := a.buildMergedSupervisorIntent(m, intentPath, opts.Workspaces, w)
	if err != nil {
		return "", err
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
	if desiredIntent.HasRuntimeSpecRow() {
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
	return intentPath, nil
}

// supervisorIntentFileLeaf is the canonical basename of the supervisor
// intent file under the per-user state directory.
const supervisorIntentFileLeaf = "supervisor-intent.json"

// buildMergedSupervisorIntent loads the existing supervisor-intent.json (if
// any), removes the daemons that belong to m.Name, appends the daemons this
// install plans for m, and returns the merged file plus the prior raw bytes
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
//     -> supervisorDaemonsFromPlan(m): the static per-daemon descriptors
//     (empty for a template-only manifest with no workspaces). This keeps the
//     template-only-no-workspaces path byte-identical to the pre-D.3b
//     behavior.
//
// MaintenanceTimers are carried through from the prior intent VERBATIM (this
// server's timers AND every sibling's). This seam is workspace-scoped-only,
// and serena sets weekly_refresh=false, so it never materializes a
// per-server weekly timer of its own. Preserving the prior set untouched
// means an operator's deliberately-disabled timer (Enabled=&false) is never
// dropped and re-added — it survives every re-install of an unrelated
// workspace-scoped manifest. Per-server weekly-cadence machinery is out of
// scope for this foundation seam (it would need a maintenance_fired_at schema
// keyed by Server, not Kind, in the already-merged supervisor maintenance
// scheduler).
func (a *API) buildMergedSupervisorIntent(m *config.ServerManifest, intentPath string, workspaces []WorkspaceEntry, w io.Writer) (merged, prior *SupervisorIntentFile, priorExisted bool, err error) {
	prior, existed, err := readSupervisorIntentForMerge(intentPath)
	if err != nil {
		return nil, nil, false, err
	}

	kept := make([]SupervisorDaemon, 0, len(prior.Daemons))
	for _, d := range prior.Daemons {
		if d.Server == m.Name {
			continue // replaced below
		}
		kept = append(kept, d)
	}
	kept = append(kept, serenaOrPlanDaemons(m, workspaces, w)...)

	merged = &SupervisorIntentFile{
		Version:   1,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Daemons:   kept,
		// Preserve the prior maintenance-timer set untouched (this server's
		// AND siblings'). See the function doc: workspace-scoped-only seam,
		// serena has weekly_refresh=false, so there is no per-server timer to
		// materialize here and an operator's Enabled=&false timer must survive.
		MaintenanceTimers: prior.MaintenanceTimers,
		StrictMode:        prior.StrictMode,
	}
	return merged, prior, existed, nil
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
// plan-derived set (supervisorDaemonsFromPlan), which is what api.Install and
// the template-only-no-workspaces path use.
//
// BuildSupervisorDaemonsForSerena itself guards on m.DaemonTemplate != nil,
// m.Kind == KindWorkspaceScoped, and len(workspaces) > 0 (returning nil
// otherwise), so a non-workspace-scoped manifest or an empty workspaces
// snapshot deterministically takes the plan-derived branch here.
func serenaOrPlanDaemons(m *config.ServerManifest, workspaces []WorkspaceEntry, w io.Writer) []SupervisorDaemon {
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
	return supervisorDaemonsFromPlan(m)
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

// supervisorDaemonsFromPlan derives the SupervisorDaemon descriptors for a
// global manifest from its daemon list. Each global daemon maps to one
// long-lived supervisor child keyed by the canonical leading-backslash task
// name. Workspace-scoped manifests carry no static Daemons (their fan-out is
// the D.2 helper, out of this slice's scope) so this returns nil for them.
func supervisorDaemonsFromPlan(m *config.ServerManifest) []SupervisorDaemon {
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
