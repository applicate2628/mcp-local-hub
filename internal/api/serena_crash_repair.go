package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// SerenaRepairResult is the outcome of a RepairOrphanSerenaWorkspaces scan.
type SerenaRepairResult struct {
	// Repaired is the count of orphan rows confirmed re-installed into the committed
	// supervisor intent.
	Repaired int
	// Unresolved are orphan workspace keys that could NOT be auto-repaired: an
	// introduce-crash (intent has no runtime_spec → `mcphub migrate serena legacy-to-dynamic-pool`) or a stale row
	// whose workspace directory was removed (`mcphub workspace unregister`). Each is
	// logged to the supervisor event log with per-category remediation.
	Unresolved []string
	// SupervisorGone is set when the post-install reconcile found the supervisor —
	// sampled up at GUI startup — has since exited. The repair does NOT start a
	// replacement itself (a detached start would escape the caller's supervisor
	// ownership, bot PR #254 r3); the CALLER, which owns the supervisor lifecycle,
	// must re-ensure it under ownership. The orphan daemons are already committed to
	// the intent, so a re-ensured supervisor cold-reconciles and spawns them.
	SupervisorGone bool
}

// RepairOrphanSerenaWorkspaces reconciles a registry/intent split left by a crash
// between an auto-register registry Save and its install commit.
//
// AutoRegisterSerenaWorkspace holds the registry flock continuously from before the
// row Save through the install commit, so within a live process the registry row and
// the supervisor-intent daemon row commit atomically. But a PROCESS crash (taskkill,
// OOM, power loss) between the Save and the commit RELEASES the flock on death,
// leaving a registry serena row with NO matching supervisor-intent daemon row. The
// resolver then finds that row and forwards calls to a port no supervisor ever
// spawned, and the existing-row fast path (serena_auto_register.go idempotency check)
// returns the orphan unrepaired forever.
//
// This is the "snapshot-to-action authority" convergence the consultant named in the
// PR #253 r6/r7 strategic review (docs/serena-lifecycle-invariants.md §1): a registry
// row is a DURABLE claim; the matching intent daemon is the convergence the supervisor
// acts on; after a crash the two diverge, and this scan re-converges them. It is meant
// to run ONCE at GUI startup, AFTER the supervisor is up, as non-fatal convergence
// (the GUI must start regardless).
//
// The repair is itself a lifecycle step, so it obeys the same §1 invariant it mitigates:
//   - it does NOT block GUI startup on a contended lock — it TryLocks the registry and
//     skips (the next startup re-scans) rather than waiting on a hung holder;
//   - it VERIFIES the action — re-reads the committed intent to confirm each orphan
//     actually landed (the install's stale-workspace filter silently drops a row whose
//     directory vanished, which must NOT be reported as repaired or it re-runs forever);
//   - it handles STALE LIVENESS — the post-install reconcile can find the supervisor
//     gone (ErrSupervisorIPCUnavailable) and SIGNALS the caller (SupervisorGone) to
//     re-ensure the supervisor under ownership, rather than starting a detached
//     replacement that would escape the GUI lifecycle.
//
// For orphan rows where the intent ALREADY carries runtime_spec (the healthy rows
// introduced the dynamic pool), the repair is a LIVE-ADD re-install of the full serena
// fan-out. The rare introduce-crash double-fault — ALL serena rows orphaned AND the
// intent carries NO runtime_spec (the very first introduce died mid-cutover) — cannot
// be repaired by a live-add (the §7.1 gate refuses a spec-bearing write while a
// supervisor runs); those keys are returned in Unresolved and logged for the operator
// to resolve with `mcphub migrate serena legacy-to-dynamic-pool`.
//
// Returns a real error only on an I/O / install failure; a healthy registry, a
// contended lock, or no serena rows return a zero result + nil error.
func (a *API) RepairOrphanSerenaWorkspaces(ctx context.Context) (SerenaRepairResult, error) {
	var result SerenaRepairResult
	regPath, err := DefaultRegistryPath()
	if err != nil {
		return result, fmt.Errorf("serena repair: resolve registry path: %w", err)
	}
	stateDir, err := DaemonStateDir()
	if err != nil {
		return result, fmt.Errorf("serena repair: resolve state dir: %w", err)
	}
	intentPath := joinStateFilePath(stateDir, supervisorIntentFileLeaf)

	// Serialize against a concurrent in-process auto-register cutover (the same
	// process-global mutex it uses — a bounded in-process wait), then TRY the registry
	// flock without blocking: a hung cross-process migrate/auto-register holding the
	// lock must NOT stall GUI startup. Mirrors the lock order install-mutex →
	// registry-flock.
	// TryLock the in-process cutover mutex too — a blocking acquire is the LAST way
	// this best-effort startup repair could stall GUI startup: a concurrent
	// auto-register that hit the just-published server may hold this mutex while it is
	// itself blocked on a cross-process lock, so waiting here would stall transitively
	// despite the non-blocking registry/intent lock paths (bot PR #254 P2). Skip on
	// contention; the next startup re-scans.
	if !serenaAutoRegisterInstallMu.TryLock() {
		return result, nil
	}
	defer serenaAutoRegisterInstallMu.Unlock()

	reg := NewRegistry(regPath)
	unlock, locked, err := reg.TryLock()
	if err != nil {
		return result, fmt.Errorf("serena repair: lock registry: %w", err)
	}
	if !locked {
		// Another mcphub process holds the registry lock (a concurrent migrate /
		// auto-register / repair). Skip this best-effort scan rather than stall GUI
		// startup; the holder commits its own intent, and the next GUI startup re-scans.
		return result, nil
	}
	regReleased := false
	releaseReg := func() {
		if regReleased {
			return
		}
		regReleased = true
		unlock()
	}
	defer releaseReg()
	if err := reg.Load(); err != nil {
		return result, fmt.Errorf("serena repair: load registry: %w", err)
	}

	rows := reg.SerenaEntries()
	if len(rows) == 0 {
		return result, nil // no serena workspaces registered — nothing to repair
	}

	intent, ierr := ReadSupervisorIntent(intentPath)
	if ierr != nil && !errors.Is(ierr, os.ErrNotExist) {
		return result, fmt.Errorf("serena repair: read supervisor intent %s: %w", intentPath, ierr)
	}

	// Orphans: a registry serena row whose per-workspace daemon is absent from the
	// committed intent (a missing intent file makes EVERY row an orphan).
	var orphanKeys []string
	for i := range rows {
		if intent == nil || !intent.HasSerenaDaemonForWorkspaceKey(rows[i].WorkspaceKey) {
			orphanKeys = append(orphanKeys, rows[i].WorkspaceKey)
		}
	}
	if len(orphanKeys) == 0 {
		return result, nil // healthy — every registry row owns its intent daemon
	}

	// Introduce-crash double-fault: the intent carries NO runtime_spec, so a first
	// introduce died mid-cutover. A live-add re-install cannot introduce runtime_spec
	// while the supervisor runs (§7.1 gate, design), so log + defer to migrate.
	if intent == nil || !intent.HasRuntimeSpecRow() {
		emitSerenaOrphanRepairEvent(SupervisorEventSeverityWarn, "serena-orphan-repair-deferred", orphanKeys, map[string]any{
			"reason":           "intent-has-no-runtime_spec (first-introduce crash)",
			"operator_action":  "run `mcphub migrate serena legacy-to-dynamic-pool` to re-introduce the serena dynamic pool",
			"orphan_workspace": orphanKeys,
		})
		result.Unresolved = orphanKeys
		return result, nil
	}

	// Live-add repair: re-install the FULL serena fan-out so the orphan daemons land
	// in the intent (the §7.1 gate passes — the prior intent already has runtime_spec).
	catalog, cerr := loadSerenaCatalogManifest()
	if cerr != nil {
		return result, fmt.Errorf("serena repair: load serena catalog manifest: %w", cerr)
	}
	dyn, derr := BuildInMemorySerenaDynamicPoolManifest(catalog)
	if derr != nil {
		return result, fmt.Errorf("serena repair: build dynamic-pool manifest: %w", derr)
	}
	committedIntentPath, iErr := autoRegisterInstallParsedManifestFn(ctx, a, dyn, InstallParsedManifestOpts{
		Writer:     io.Discard,
		Workspaces: rows,
		// Best-effort: TryLock the supervisor-intent lock too, so a hung intent writer
		// cannot stall GUI startup here either (the registry TryLock above is not the
		// only blocking lock — the install takes the intent lock; bot PR #254 P2).
		NonBlockingIntentLock: true,
	})
	if iErr != nil {
		if errors.Is(iErr, ErrSupervisorIntentLockContended) {
			// Another intent writer (install / migrate / auto-register) holds the
			// supervisor-intent lock. Skip this best-effort scan rather than stall GUI
			// startup; the next startup re-scans (the holder commits its own intent).
			return result, nil
		}
		return result, fmt.Errorf("serena repair: re-install fan-out for %d orphan(s) %v: %w", len(orphanKeys), orphanKeys, iErr)
	}

	// Verify convergence (§1 — verify the action, do not assume it). The install's
	// stale-workspace filter DROPS a row whose directory has since been removed,
	// committing an intent that still lacks it. Re-read the committed intent and
	// count only the orphans that ACTUALLY landed; a row that stayed missing is a
	// stale row (its dir is gone) — surface it for operator cleanup.
	postIntent, pErr := ReadSupervisorIntent(committedIntentPath)
	if pErr != nil && !errors.Is(pErr, os.ErrNotExist) {
		return result, fmt.Errorf("serena repair: re-read committed intent %s: %w", committedIntentPath, pErr)
	}
	var converged []string
	for _, k := range orphanKeys {
		if postIntent != nil && postIntent.HasSerenaDaemonForWorkspaceKey(k) {
			converged = append(converged, k)
		} else {
			result.Unresolved = append(result.Unresolved, k)
		}
	}
	if len(result.Unresolved) > 0 {
		emitSerenaOrphanRepairEvent(SupervisorEventSeverityWarn, "serena-orphan-repair-stale-skip", result.Unresolved, map[string]any{
			"reason":           "workspace directory removed — the install stale-filter dropped the row",
			"operator_action":  "run `mcphub workspace unregister <path> --backend serena` to drop the stale serena row",
			"orphan_workspace": result.Unresolved,
		})
	}

	// Release the flock before the reconcile (it touches neither the registry nor the
	// intent file), then nudge the running supervisor to reconcile the now-complete
	// intent NOW.
	releaseReg()
	if _, recErr := autoRegisterReconcileFn(ctx, true); recErr != nil {
		if errors.Is(recErr, ErrSupervisorIPCUnavailable) {
			// Stale liveness (§1): the supervisor sampled up at GUI startup has exited,
			// so there is no IntentWatcher to spawn the just-committed orphan daemons.
			// Do NOT start a detached replacement here — it would escape the caller's
			// supervisor ownership (bot PR #254 r3). SIGNAL the caller, which owns the
			// lifecycle, to re-ensure the supervisor under ownership; the orphans are
			// committed, so the re-ensured supervisor cold-reconciles + spawns them.
			result.SupervisorGone = true
			emitSerenaOrphanRepairEvent(SupervisorEventSeverityWarn, "serena-orphan-repair-supervisor-gone", converged, map[string]any{
				"reason":          "supervisor exited post-startup; no IntentWatcher to spawn the committed daemons",
				"operator_action": "the GUI re-ensures the supervisor under ownership; if it cannot, run `mcphub supervise`",
			})
		}
		// else: the supervisor is up; the reconcile nudge failed transiently; its own
		// IntentWatcher (60s) backstops the spawn.
	}

	if len(converged) > 0 {
		emitSerenaOrphanRepairEvent(SupervisorEventSeverityInfo, "serena-orphan-repair-applied", converged, map[string]any{
			"repaired_workspace": converged,
			"mode":               "live-add re-install",
		})
	}
	result.Repaired = len(converged)
	return result, nil
}

// emitSerenaOrphanRepairEvent records a structured crash-repair event to the supervisor
// event log (best-effort; never fatal). Reuses the same log + envelope as the
// auto-register and §7.1-gate emitters.
func emitSerenaOrphanRepairEvent(severity, event string, keys []string, body map[string]any) {
	stateDir, sdErr := DaemonStateDir()
	if sdErr != nil {
		return
	}
	logger, openErr := OpenSupervisorEventLog(filepath.Join(stateDir, SupervisorEventLogFileLeaf))
	if openErr != nil {
		return
	}
	defer func() { _ = logger.Close() }()
	if body == nil {
		body = map[string]any{}
	}
	body["orphan_count"] = len(keys)
	_ = logger.Emit(SupervisorEvent{
		SchemaVersion: SupervisorEventSchemaVersion,
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Severity:      severity,
		Source:        "reconcile",
		Event:         event,
		Body:          body,
	})
}
