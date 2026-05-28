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
//
// StartAfterWrite defaults to true to match api.Install's
// immediate-daemon-start behavior; the migrate driver passes false so the
// daemons are spawned later by the supervisor reconciler when it observes
// the new supervisor-intent.json rather than at install time.
type InstallParsedManifestOpts struct {
	Writer            io.Writer
	ClientsInclude    []string
	IncludeAllClients bool
	Workspaces        []WorkspaceEntry // pre-loaded snapshot of registered serena workspaces
	DryRun            bool
	StartAfterWrite   bool // default true (matches api.Install); migrate driver later passes false
}

// InstallParsedManifest installs a pre-parsed manifest in-process and
// returns the absolute path of the supervisor-intent.json it wrote.
//
// Sequence:
//
//  1. Pre-flight gate (fail-fast, no mutation): dry-write the intent file
//     to a temp path via WriteStateFileAtomic. A failure here (disk full,
//     permission denied, parent-dir DACL gate refusal under
//     MCPHUB_REQUIRE_SINGLE_USER_HOME=1) returns BEFORE any other mutation
//     so the end-state is pristine.
//  2. BuildPlanWithOpts on the parsed manifest.
//  3. installPlan -> executeInstallTo: scheduler-task creation + per-client
//     config writes, then the supervisor-intent write as the intermediate
//     step INSIDE executeInstallTo's rollback scope. StartAfterWrite gates
//     Pass B (immediate daemon start).
//
// On any sub-failure inside step 3, the shared rollback stack runs and the
// function returns the error with every side effect undone.
func (a *API) InstallParsedManifest(ctx context.Context, m *config.ServerManifest, opts InstallParsedManifestOpts) (intentPath string, err error) {
	w := opts.Writer
	if w == nil {
		w = os.Stderr
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
	desiredIntent, priorIntent, priorExisted, err := a.buildMergedSupervisorIntent(m, intentPath)
	if err != nil {
		return "", err
	}

	// 1. Pre-flight gate: dry-write to a temp path. No rollback push — this
	// is a read-only-ish probe that leaves no committed side effect (the
	// temp file is removed immediately).
	if err := preflightSupervisorIntentWrite(stateDir, desiredIntent); err != nil {
		return "", fmt.Errorf("pre-flight supervisor-intent write: %w", err)
	}

	// 2. Build the plan. NOTE: refuseWorkspaceScopedInstall is intentionally
	// NOT called — workspace-scoped is the intended input here.
	plan, err := BuildPlanWithOpts(m, BuildPlanOpts{
		ClientsInclude:    opts.ClientsInclude,
		IncludeAllClients: opts.IncludeAllClients,
	})
	if err != nil {
		return "", err
	}

	// 3. Audit-first + execute, with the supervisor-intent write folded into
	// executeInstallTo's rollback stack via the intermediate hook.
	intermediate := func() (func(), error) {
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
// For a workspace-scoped dynamic-pool manifest the per-workspace serena
// fan-out (D.2 BuildSupervisorDaemonsForSerena) is NOT applied here — that
// wiring is a separate task. The install-foundation slice writes the
// plan-derived daemon set (empty for a template-only manifest), which keeps
// the on-disk file well-formed and the sibling-server entries intact.
func (a *API) buildMergedSupervisorIntent(m *config.ServerManifest, intentPath string) (merged, prior *SupervisorIntentFile, priorExisted bool, err error) {
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
	kept = append(kept, supervisorDaemonsFromPlan(m)...)

	merged = &SupervisorIntentFile{
		Version:           1,
		UpdatedAt:         time.Now().UTC().Format(time.RFC3339Nano),
		Daemons:           kept,
		MaintenanceTimers: prior.MaintenanceTimers,
		StrictMode:        prior.StrictMode,
	}
	return merged, prior, existed, nil
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
