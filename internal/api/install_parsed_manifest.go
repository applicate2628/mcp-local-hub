// Package api - Phase D.3 in-process install seam for pre-parsed manifests.
//
// InstallParsedManifest is the workspace-scoped sister to (*API).Install.
// Where Install loads a manifest by name and refuses workspace-scoped
// kinds, InstallParsedManifest accepts an already-parsed manifest (the
// caller owns parsing) and BYPASSES refuseWorkspaceScopedInstall because
// workspace-scoped dynamic-pool manifests are its intended input.
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
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/config"
)

// InstallParsedManifestOpts controls an InstallParsedManifest invocation.
type InstallParsedManifestOpts struct {
	Writer            io.Writer
	ClientsInclude    []string
	IncludeAllClients bool
	// Snapshot of registered serena workspaces; consumed by the
	// per-workspace fan-out; populated by future callers (migrate redesign
	// / E.2 auto-register).
	Workspaces []WorkspaceEntry
	DryRun     bool
	// StartAfterWrite gates Pass B (immediate daemon start). The zero value
	// (false) means scheduler tasks are created and supervisor-intent.json
	// is written, but the daemons are NOT started here — the supervisor
	// reconciler starts them on its next tick once it observes the new
	// intent. That deferred behavior is the intended default for
	// workspace-scoped installs through this seam (api.Install does not use
	// this field; it drives executeInstallTo's Pass B directly). Set true
	// only when a caller needs the daemons started in-process before this
	// returns.
	StartAfterWrite bool
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
//     step INSIDE executeInstallTo's rollback scope. StartAfterWrite gates
//     Pass B (immediate daemon start). On dry-run, installPlan prints the
//     plan and returns; this function then returns ("", nil) because nothing
//     was written and there is no intent path for the caller to dereference.
//
// On any sub-failure inside step 4, the shared rollback stack runs and the
// function returns the error with every side effect undone.
func (a *API) InstallParsedManifest(ctx context.Context, m *config.ServerManifest, opts InstallParsedManifestOpts) (intentPath string, err error) {
	w := opts.Writer
	if w == nil {
		w = os.Stderr
	}

	// 1. Preflight: check-only gate shared with the legacy install paths.
	// Runs FIRST, before the pre-flight intent write and before any mutation,
	// so a missing command or unresolved secret: ref fails fast.
	if err := Preflight(m, ""); err != nil {
		return "", fmt.Errorf("preflight: %w", err)
	}

	// 1a. FIX 4 — fail-loud on StartAfterWrite for a workspace-scoped fan-out
	// install. A workspace-scoped DaemonTemplate manifest with a non-empty
	// workspaces snapshot produces ZERO scheduler tasks from BuildPlanWithOpts,
	// so executeInstallTo's Pass B would iterate an empty createdTasks set and
	// start NO daemon — a silent no-op. The per-workspace serena daemons start
	// via the supervisor RECONCILER once it observes the new intent, NOT via
	// this seam's Pass B. Honoring StartAfterWrite here is therefore impossible;
	// returning a clear error BEFORE any mutation beats a silent no-start (per
	// the operational-contract failure-transparency discipline).
	if m.DaemonTemplate != nil && len(opts.Workspaces) > 0 && opts.StartAfterWrite {
		return "", fmt.Errorf("StartAfterWrite is not supported for workspace-scoped fan-out installs; per-workspace daemons start via the supervisor reconciler after the intent write — call with StartAfterWrite=false")
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

	// 2. Pre-flight intent-write gate: dry-write to a temp path. No rollback
	// push — this is a read-only-ish probe that leaves no committed side
	// effect (the temp file is removed immediately). SKIPPED on dry-run: a
	// dry run must not do any flock-guarded disk I/O. The probe targets a
	// DISTINCT ".preflight" path with its OWN ".preflight.lock" leaf, so it
	// never re-acquires the supervisor-intent.json.lock we hold here.
	if !opts.DryRun {
		if err := preflightSupervisorIntentWrite(stateDir, desiredIntent); err != nil {
			return "", fmt.Errorf("pre-flight supervisor-intent write: %w", err)
		}
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
		Writer:             w,
		DaemonFilter:       "",
		DryRun:             opts.DryRun,
		StartTasks:         opts.StartAfterWrite,
		Intermediate:       intermediate,
		SkipSchedulerPrune: m.DaemonTemplate != nil,
	}); err != nil {
		return "", err
	}
	// Dry-run wrote nothing (the pre-flight write was skipped and the
	// intermediate hook never fired), so return an empty path — the caller
	// must not dereference a path for a file that was never committed.
	if opts.DryRun {
		return "", nil
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
//   - Otherwise (global manifest, OR a workspace-scoped manifest with no
//     registered workspaces) -> supervisorDaemonsFromPlan(m): the static
//     per-daemon descriptors (empty for a template-only manifest with no
//     workspaces). This keeps the api.Install path and the
//     template-only-no-workspaces path byte-identical to the pre-D.3b
//     behavior.
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
		Version:           1,
		UpdatedAt:         time.Now().UTC().Format(time.RFC3339Nano),
		Daemons:           kept,
		MaintenanceTimers: prior.MaintenanceTimers,
		StrictMode:        prior.StrictMode,
	}
	return merged, prior, existed, nil
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

// filterExistingWorkspaceRows returns the subset of workspaces whose
// WorkspacePath still resolves to an existing directory entry on disk. A row
// whose path is absent (deleted / moved workspace) is dropped and a warn is
// emitted to w AND to supervisor-events.log (best-effort) so the skipped row
// is operator-visible rather than silently pruned. A row with an empty path is
// passed through unchanged — the fan-out helper itself skips empty-path rows,
// and statting "" would spuriously report not-exist.
//
// os.Stat (not os.Lstat) is intentional: a workspace reachable only through a
// symlinked directory is still a live workspace; we care whether the target
// resolves, not whether the entry itself is a symlink.
func filterExistingWorkspaceRows(server string, workspaces []WorkspaceEntry, w io.Writer) []WorkspaceEntry {
	live := make([]WorkspaceEntry, 0, len(workspaces))
	for _, ws := range workspaces {
		if ws.WorkspacePath == "" {
			live = append(live, ws)
			continue
		}
		if _, statErr := os.Stat(ws.WorkspacePath); statErr != nil && os.IsNotExist(statErr) {
			if w != nil {
				fmt.Fprintf(w, "⚠ Skipping stale workspace %q (path no longer exists): %s daemon row dropped from supervisor-intent\n", ws.WorkspacePath, server)
			}
			emitStaleWorkspaceSkippedEvent(server, ws.WorkspacePath)
			continue
		}
		// A non-IsNotExist stat error (permission denied, I/O error) is NOT a
		// stale-path signal — keep the row so a transient stat failure does
		// not silently drop a live workspace. The supervisor will surface a
		// real spawn failure if the path is genuinely unusable.
		live = append(live, ws)
	}
	return live
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
