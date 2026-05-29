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
	"time"

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

	stateDir, err := DaemonStateDir()
	if err != nil {
		return "", fmt.Errorf("resolve state dir: %w", err)
	}
	intentPath = joinStateFilePath(stateDir, supervisorIntentFileLeaf)

	// Build the intent file we intend to write up front so the pre-flight
	// dry-write exercises the same payload and the same secure-write
	// pipeline the real write will use. priorIntent + priorExisted drive the
	// rollback restore.
	desiredIntent, priorIntent, priorExisted, err := a.buildMergedSupervisorIntent(m, intentPath, opts.Workspaces)
	if err != nil {
		return "", err
	}

	// 2. Pre-flight intent-write gate: dry-write to a temp path. No rollback
	// push — this is a read-only-ish probe that leaves no committed side
	// effect (the temp file is removed immediately). SKIPPED on dry-run: a
	// dry run must not do any flock-guarded disk I/O.
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
	// installPlan prints the plan and returns without invoking the hook.
	var intermediate intentWriteStep = func() (func(), error) {
		if werr := WriteSupervisorIntent(intentPath, desiredIntent); werr != nil {
			return nil, fmt.Errorf("write supervisor intent %s: %w", intentPath, werr)
		}
		// Compensating undo: restore the prior file content verbatim, or
		// remove the file entirely if it did not exist before this install.
		undo := func() {
			if priorExisted {
				if rerr := WriteSupervisorIntent(intentPath, priorIntent); rerr != nil {
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

	if err := a.installPlan(ctx, m, plan, installPlanOpts{
		Writer:       w,
		DaemonFilter: "",
		DryRun:       opts.DryRun,
		StartTasks:   opts.StartAfterWrite,
		Intermediate: intermediate,
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
func (a *API) buildMergedSupervisorIntent(m *config.ServerManifest, intentPath string, workspaces []WorkspaceEntry) (merged, prior *SupervisorIntentFile, priorExisted bool, err error) {
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
	kept = append(kept, serenaOrPlanDaemons(m, workspaces)...)

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
func serenaOrPlanDaemons(m *config.ServerManifest, workspaces []WorkspaceEntry) []SupervisorDaemon {
	if m.DaemonTemplate != nil && len(workspaces) > 0 {
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
		if serena := BuildSupervisorDaemonsForSerena(m, workspaces, "", mcphubPath); serena != nil {
			return serena
		}
	}
	return supervisorDaemonsFromPlan(m)
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
