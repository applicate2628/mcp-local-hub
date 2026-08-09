// internal/api/prune_workspace.go
//
// PruneWorkspace — the shared two-phase (LSP + serena) workspace teardown
// owner. Phase 1 of the workspace-daemon auto-prune feature (auto-prune the
// structural cases; no new persisted state).
//
// Before this file the two-phase composition lived ONLY in the CLI
// `runWorkspaceUnregister` (internal/cli/workspace_cmd.go). That made the CLI
// the sole home of the teardown ordering + the paired-descriptor invariants,
// so the new in-GUI auto-prune sweeper could not reuse it without duplicating
// the logic. This file LIFTS that composition into one exported owner both the
// CLI and the sweeper call. The CLI re-points at it so there is a single
// maintained owner of the teardown ordering (no logic duplication).
//
// Ordering invariants preserved EXACTLY from the CLI (do not reorder):
//   - LSP teardown (unregisterWithManifest) runs BEFORE the serena registry-row
//     delete — the CLI's two-phase split.
//   - Within the serena phase, the supervisor-descriptor teardown
//     (RemoveSerenaSupervisorIntentForWorkspace) runs BEFORE the registry-row
//     delete (bot r32 P2 teardown-before-delete): on a live-supervisor
//     reconcile failure the teardown restores the descriptor and returns a
//     retry-asking error, so the durable registry row that drives the next
//     retry's paired teardown must outlive a failed teardown.
//
// Locking: PruneWorkspace takes NO new locks. The LSP teardown
// (unregisterWithManifest) and the serena registry delete each acquire their
// OWN brief registry lock; the serena descriptor teardown fails closed on a
// wedged supervisor via RemoveSerenaSupervisorIntentForWorkspace. It does NOT
// take supervisor.lock. This is the same lock posture the CLI uses.
//
// Existence tolerance: a backend whose rows are already gone is a no-op success
// (mirrors the CLI unregister contract). The owner reads the registry once up
// front to learn which backends actually have rows, so a workspace with only a
// serena row (or only LSP rows, or nothing at all) prunes cleanly without the
// "is not registered" hard error unregisterWithManifest raises on an empty LSP
// set. A pruned workspace re-registers on next open (EnsureLSPRegistered /
// AutoRegisterSerenaWorkspace) — prune is non-destructive.

package api

import (
	"errors"
	"fmt"
	"path/filepath"
)

// PruneReport summarizes what PruneWorkspace removed. It mirrors the
// UnregisterReport shape (per-backend outcome + warnings) so callers and logs
// can treat the two surfaces uniformly.
type PruneReport struct {
	Workspace     string   `json:"workspace"`
	WorkspaceKey  string   `json:"workspace_key"`
	Backend       string   `json:"backend"`            // the requested filter: "lsp" | "serena" | "all"
	LSPRemoved    []string `json:"lsp_removed"`        // LSP language rows actually torn down
	SerenaRemoved int      `json:"serena_removed"`     // count of serena (sentinel) rows removed
	Warnings      []string `json:"warnings,omitempty"` // best-effort failures (descriptor teardown notes, etc.)
}

// PruneWorkspaceTeardown carries the two teardown closures the two-phase
// composition (PruneWorkspacePhases) calls, plus the post-mutation serena
// registry-row delete. It exists so BOTH the api owner (PruneWorkspace) and the
// CLI `mcphub workspace unregister` route their LSP+serena ordering through ONE
// shared sequencer without the CLI losing its test-injectable seams (the CLI
// passes its own stubbed closures here; the api owner passes direct calls).
//
//   - LSPUnregister tears down the given LSP languages paired with their
//     supervisor-intent descriptors (production: (*API).Unregister; CLI tests
//     stub it). An empty languages slice means "every LSP language".
//   - RemoveSerenaIntent tears down the serena per-workspace supervisor-intent
//     descriptor and fails closed on a live-but-wedged supervisor (production:
//     (*API).RemoveSerenaSupervisorIntentForWorkspace; CLI tests stub it).
//   - DeleteSerenaRow commits the serena registry-row delete and returns the
//     count removed. It runs ONLY after RemoveSerenaIntent succeeds (the
//     teardown-before-delete invariant).
//   - BeginSerenaPendingRemoval stages the exact pending-removal tuple before
//     RemoveSerenaIntent and returns its ownership-aware rollback. A teardown
//     failure (including a commit-unknown stage error) invokes that rollback so
//     a retry sees the exact prior tuple, without clobbering a later writer.
//   - AcquireSerenaRemovalFence takes the per-workspace unregister LIVENESS
//     FENCE (production: api.AcquireSerenaRemovalFence) and returns its release
//     closure. PruneWorkspacePhases holds it across the ENTIRE marked window so
//     RepairSerenaIntentFromRegistry can distinguish a teardown that is merely
//     SLOW from one that CRASHED: the mark alone cannot, and the wall-clock
//     lease that used to answer it measured elapsed time, not liveness (see
//     serena_removal_fence.go). Nil is tolerated and simply skips the fence,
//     which leaves the pre-fence lease-only behavior — the marked row is still
//     protected for the lease window, just not beyond it.
type PruneWorkspaceTeardown struct {
	LSPUnregister                       func(workspacePath string, languages []string) (*UnregisterReport, error)
	RemoveSerenaIntent                  func(canonicalWorkspacePath string) (bool, error)
	DeleteSerenaRow                     func() PruneSerenaDeleteResult
	BeginSerenaPendingRemoval           func(generation string) (rollback func() error, err error)
	AcquireSerenaRemovalFence           func() (release func() error, err error)
	PublishSerenaRemovalFenceGeneration func() (string, error)
}

// PruneSerenaDeleteResult separates the committed mutation result from lock
// release confirmation so teardown rollback is driven only by MutationErr.
type PruneSerenaDeleteResult struct {
	Removed       int
	MutationErr   error
	PostCommitErr error
	ReleaseErr    error
}

// PruneWorkspace tears down a workspace's daemon rows in the SAME order the CLI
// `mcphub workspace unregister` uses, composing the LSP and serena teardown
// owners. It is the shared owner the CLI and the GUI auto-prune sweeper both
// call.
//
// backend selects scope:
//   - "lsp"    → LSP rows only (every per-language row for the workspace).
//   - "serena" → the serena (sentinel) row only.
//   - "all"    → both, LSP teardown first then serena.
//
// workspacePath is the operator/sweeper-supplied path (not pre-canonicalized);
// the underlying teardown owners compute their own canonical + legacy keys via
// CanonicalWorkspacePathForCleanup so a deleted/moved workspace still prunes.
//
// Existence-tolerant: a backend with no matching rows is a no-op success.
func (a *API) PruneWorkspace(workspacePath string, backend string) (*PruneReport, error) {
	switch backend {
	case "lsp", "serena", "all":
	default:
		return nil, fmt.Errorf("PruneWorkspace: invalid backend %q (want \"lsp\", \"serena\", or \"all\")", backend)
	}

	canonical, err := CanonicalWorkspacePathForCleanup(workspacePath)
	if err != nil {
		return nil, err
	}
	wsKey := WorkspaceKey(canonical)
	legacyCanonical, err := CanonicalWorkspacePathLegacyCompat(workspacePath)
	if err != nil {
		return nil, err
	}
	legacyWSKey := WorkspaceKey(legacyCanonical)

	regPath, err := registryPathForRegister()
	if err != nil {
		return nil, err
	}

	// Read the registry once up front (brief lock) to learn which backends
	// actually have rows. This drives the existence-tolerant routing below: we
	// only invoke a teardown owner for a backend that has rows, so a workspace
	// with only a serena row (or only LSP rows, or nothing) prunes cleanly
	// without the "is not registered" hard error unregisterWithManifest raises
	// on an empty LSP set. The decision is read-only — the mutation happens in
	// the teardown owners below under their own locks, mirroring the CLI's
	// classify-then-mutate phase split.
	hasLSP, hasSerena, err := workspaceBackendPresence(regPath, wsKey, legacyWSKey)
	if err != nil {
		return nil, err
	}

	report := &PruneReport{Workspace: canonical, WorkspaceKey: wsKey, Backend: backend}

	wantLSP := (backend == "lsp" || backend == "all") && hasLSP
	wantSerena := (backend == "serena" || backend == "all") && hasSerena

	// Wire the production teardown closures and route them through the SHARED
	// two-phase sequencer so this owner and the CLI share ONE home for the
	// teardown ordering (no logic duplication). The LSP teardown reuses the
	// production (*API).Unregister body (descriptor-paired removal + reconcile +
	// kill-by-port + scheduler delete + client-entry removal); the serena delete
	// removes the sentinel row under its own brief registry lock AFTER the
	// descriptor teardown succeeds.
	td := PruneWorkspaceTeardown{
		LSPUnregister:      func(p string, langs []string) (*UnregisterReport, error) { return a.Unregister(p, langs) },
		RemoveSerenaIntent: a.RemoveSerenaSupervisorIntentForWorkspace,
		BeginSerenaPendingRemoval: func(generation string) (func() error, error) {
			reg := NewRegistry(regPath)
			return reg.BeginSerenaPendingRemoval(wsKey, legacyWSKey, generation)
		},
		// Fence on the CANONICAL key only. The legacy key names the same
		// workspace, so a second leaf would be a second mutex for one resource,
		// and the repair probes with the row's own WorkspaceKey — which the
		// divergence guard requires to equal WorkspaceKey(WorkspacePath), i.e.
		// the canonical key. One key, one fence.
		AcquireSerenaRemovalFence: func() (func() error, error) {
			return AcquireSerenaRemovalFence(filepath.Dir(regPath), wsKey)
		},
		PublishSerenaRemovalFenceGeneration: func() (string, error) {
			return PublishSerenaRemovalFenceGeneration(filepath.Dir(regPath), wsKey)
		},
		DeleteSerenaRow: func() (result PruneSerenaDeleteResult) {
			reg := NewRegistry(regPath)
			unlock, lerr := reg.Lock()
			if lerr != nil {
				result.MutationErr = lerr
				return result
			}
			defer func() { result.ReleaseErr = unlock() }()
			if lerr := reg.Load(); lerr != nil {
				result.MutationErr = lerr
				return result
			}
			keys := dedupeWorkspaceKeys(wsKey, legacyWSKey)
			n, committed, serr := reg.RemoveSerenaRowsAndSave(keys...)
			if !committed {
				result.MutationErr = serr
				return result
			}
			result.Removed = n
			result.PostCommitErr = serr
			return result
		},
	}

	if err := PruneWorkspacePhases(workspacePath, canonical, nil, wantLSP, wantSerena, td, report); err != nil {
		return report, err
	}
	return report, nil
}

// ListAllWorkspaceRows returns EVERY registered workspace row — the serena
// (sentinel) rows AND every per-language LSP row — as pointer copies. The prune
// sweep needs the FULL set, unlike the serena router resolver's ListWorkspaces
// which is serena-ONLY: an ephemeral agent worktree whose only daemon is a
// per-language LSP (e.g. `language: go`) is invisible to the serena resolver, so
// a sweep driven by it would prune only the serena worktrees and leave the LSP
// worktrees growing. Reading the registry directly (SerenaEntries + LSPEntries)
// covers both. The brief read lock is released before the caller prunes, so it
// does not nest with PruneWorkspace's own per-workspace locks.
func (a *API) ListAllWorkspaceRows() (rows []*WorkspaceEntry, err error) {
	regPath, err := registryPathForRegister()
	if err != nil {
		return nil, err
	}
	reg := NewRegistry(regPath)
	unlock, lerr := reg.Lock()
	if lerr != nil {
		return nil, lerr
	}
	defer ReleaseAndJoin(&err, unlock)
	if lerr := reg.Load(); lerr != nil {
		return nil, lerr
	}
	var out []*WorkspaceEntry
	for _, e := range reg.SerenaEntries() {
		out = append(out, &e)
	}
	for _, e := range reg.LSPEntries() {
		out = append(out, &e)
	}
	return out, nil
}

// PruneWorkspacePhases is the SINGLE owner of the two-phase (LSP-then-serena)
// teardown ordering. Both (*API).PruneWorkspace and the CLI `mcphub workspace
// unregister` call it so the ordering + paired-descriptor invariants live in
// one maintained place (no logic duplication).
//
// Ordering (do NOT reorder — preserved exactly from the original CLI path):
//   - Phase 1: LSP teardown (td.LSPUnregister with the given lspLangs; nil/empty
//     = every LSP language) runs FIRST. A hard failure aborts BEFORE the serena
//     phase (the two-phase split). bot r32 P2: tearing serena down while the
//     paired LSP teardown failed would leave an unreported half-applied state.
//   - Phase 2: serena descriptor teardown (td.RemoveSerenaIntent) runs BEFORE
//     the registry-row delete (td.DeleteSerenaRow). On a live-supervisor
//     reconcile failure RemoveSerenaIntent restores the descriptor and returns a
//     retry-asking error; returning here WITHOUT deleting the row keeps the
//     durable record that drives the next retry's paired teardown intact.
//     IMMEDIATELY before RemoveSerenaIntent runs,
//     td.BeginSerenaPendingRemoval stages the tuple so a reconcile nudged
//     INSIDE RemoveSerenaIntent cannot mistake this deliberate teardown for a
//     crash-orphan and resurrect the row. A failure rolls back only that exact
//     transaction's tuple, preserving pre-existing and later writer state.
//   - WRAPPING the whole serena phase, td.AcquireSerenaRemovalFence is taken
//     BEFORE the mark and released only after the row delete (or after the
//     failure path has cleared the mark). The fence is the LIVENESS half of the
//     same guard: the mark says "a teardown claims this row", the fence says
//     "and that teardown is still alive". Acquiring it after the mark, or
//     releasing it before the delete, would leave a window in which the mark is
//     set with no live holder — which is precisely the shape the repair reads
//     as reclaimable crash debris.
//
// wantLSP / wantSerena gate the two phases (the caller decides scope from its
// own classification). report accumulates the per-backend outcome + warnings;
// it must be non-nil.
func PruneWorkspacePhases(workspacePath, canonical string, lspLangs []string, wantLSP, wantSerena bool, td PruneWorkspaceTeardown, report *PruneReport) (err error) {
	if wantLSP && td.LSPUnregister != nil {
		ureport, uerr := td.LSPUnregister(workspacePath, lspLangs)
		if ureport != nil {
			report.LSPRemoved = append(report.LSPRemoved, ureport.Removed...)
			report.Warnings = append(report.Warnings, ureport.Warnings...)
		}
		if uerr != nil {
			return fmt.Errorf("paired LSP teardown for workspace %s: %w", canonical, uerr)
		}
	}

	if wantSerena {
		var rollbackPendingRemoval func() error
		rollback := func(primary error) error {
			if rollbackPendingRemoval == nil {
				return primary
			}
			rollbackErr := rollbackPendingRemoval()
			if rollbackErr != nil {
				return errors.Join(primary, fmt.Errorf("rollback pending Serena-removal tuple: %w", rollbackErr))
			}
			return primary
		}

		// Liveness fence FIRST — before the mark, and held past the row delete.
		// The deferred release runs at function exit, which is the end of the
		// serena phase (nothing follows it), so every early return below —
		// including the RemoveSerenaIntent failure path that clears the mark —
		// releases the fence only AFTER the registry has been left in a state no
		// repair can misread.
		//
		// A fence failure ABORTS before any mutation. Proceeding unfenced would
		// silently reinstate the exact race this closes, and nothing has been
		// changed yet at this point, so the abort is free.
		if td.AcquireSerenaRemovalFence != nil {
			release, ferr := td.AcquireSerenaRemovalFence()
			if ferr != nil {
				return fmt.Errorf("acquire serena removal fence for workspace %s: %w", canonical, ferr)
			}
			if release != nil {
				defer func() {
					if releaseErr := release(); releaseErr != nil {
						err = errors.Join(err, fmt.Errorf("serena teardown for workspace %s completed its mutations but could not release the removal fence: %w", canonical, releaseErr))
					}
				}()
			}
		}
		if td.RemoveSerenaIntent != nil {
			generation := ""
			if td.PublishSerenaRemovalFenceGeneration != nil {
				var gerr error
				generation, gerr = td.PublishSerenaRemovalFenceGeneration()
				if gerr != nil {
					return fmt.Errorf("publish serena removal fence generation for workspace %s: %w", canonical, gerr)
				}
			}
			if td.BeginSerenaPendingRemoval != nil {
				// Store rollback before inspecting the error: Registry.Save can fail
				// after rename/publication, so its error is commit-unknown.
				var merr error
				rollbackPendingRemoval, merr = td.BeginSerenaPendingRemoval(generation)
				if merr != nil {
					return fmt.Errorf("mark workspace %s pending serena removal: %w", canonical, rollback(merr))
				}
			}
			if _, serr := td.RemoveSerenaIntent(canonical); serr != nil {
				return fmt.Errorf("paired serena supervisor teardown for workspace %s: %w", canonical, rollback(serr))
			}
		}
		if td.DeleteSerenaRow != nil {
			deleteResult := td.DeleteSerenaRow()
			report.SerenaRemoved += deleteResult.Removed
			if deleteResult.MutationErr == nil {
				rollbackPendingRemoval = nil // deletion commits even when Removed is zero.
			} else {
				err = rollback(deleteResult.MutationErr)
			}
			err = errors.Join(err, deleteResult.PostCommitErr, deleteResult.ReleaseErr)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// workspaceBackendPresence reads the registry under a brief lock and reports
// whether the workspace (canonical or legacy key) has any LSP rows and/or a
// serena (sentinel) row. Read-only — it never mutates. Mirrors the CLI's
// classify pass but collapsed to the two-bit "which backends are present"
// signal PruneWorkspace's existence-tolerant routing needs.
func workspaceBackendPresence(regPath, wsKey, legacyWSKey string) (hasLSP bool, hasSerena bool, err error) {
	reg := NewRegistry(regPath)
	unlock, lerr := reg.Lock()
	if lerr != nil {
		return false, false, lerr
	}
	defer ReleaseAndJoin(&err, unlock)
	if lerr := reg.Load(); lerr != nil {
		return false, false, lerr
	}
	for _, key := range dedupeWorkspaceKeys(wsKey, legacyWSKey) {
		for _, e := range reg.ListByWorkspace(key) {
			if e.Language == SerenaLanguageSentinel {
				hasSerena = true
			} else {
				hasLSP = true
			}
		}
	}
	return hasLSP, hasSerena, nil
}

// dedupeWorkspaceKeys returns the distinct non-empty keys among the canonical
// and legacy workspace keys (they coincide on non-legacy rows).
func dedupeWorkspaceKeys(wsKey, legacyWSKey string) []string {
	keys := []string{wsKey}
	if legacyWSKey != "" && legacyWSKey != wsKey {
		keys = append(keys, legacyWSKey)
	}
	return keys
}
