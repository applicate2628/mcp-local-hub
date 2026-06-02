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

// RepairOrphanSerenaWorkspaces reconciles a registry/intent split left by a crash
// between an auto-register registry Save and its install commit.
//
// AutoRegisterSerenaWorkspace holds the registry flock continuously from before the
// row Save through the install commit, so within a single live process the registry
// row and the supervisor-intent daemon row commit atomically. But a PROCESS crash
// (taskkill, OOM, power loss) between the Save and the commit RELEASES the flock on
// death, leaving a registry serena row with NO matching supervisor-intent daemon row.
// The resolver then finds that row and forwards calls to a port no supervisor ever
// spawned, and the existing-row fast path (serena_auto_register.go idempotency check)
// returns the orphan unrepaired forever.
//
// This is the "snapshot-to-action authority" convergence the consultant named in the
// PR #253 r6/r7 strategic review: a registry row is a DURABLE claim; the matching
// intent daemon is the convergence the supervisor acts on; after a crash the two
// diverge, and this scan re-converges them. It is meant to run ONCE at GUI startup,
// AFTER the supervisor is up, as non-fatal convergence (the GUI must start regardless).
//
// For orphan rows where the intent ALREADY carries runtime_spec (the healthy rows
// introduced the dynamic pool), the repair is a LIVE-ADD re-install of the full serena
// fan-out: the orphan daemons land in the intent and the next reconcile spawns them.
// The rare introduce-crash double-fault — ALL serena rows orphaned AND the intent
// carries NO runtime_spec (the very first introduce died mid-cutover) — cannot be
// repaired by a live-add (the §7.1 gate refuses a spec-bearing write while a supervisor
// runs); those keys are returned in `deferred` and logged for the operator to resolve
// with `mcphub migrate`, which owns the host-wide cutover.
//
// Returns the number of orphans re-installed (`repaired`), the keys deferred to
// migrate (`deferred`), and a real error only on an I/O / install failure. A healthy
// registry (every row owns its intent daemon) returns (0, nil, nil).
func (a *API) RepairOrphanSerenaWorkspaces(ctx context.Context) (repaired int, deferred []string, err error) {
	regPath, err := DefaultRegistryPath()
	if err != nil {
		return 0, nil, fmt.Errorf("serena repair: resolve registry path: %w", err)
	}
	stateDir, err := DaemonStateDir()
	if err != nil {
		return 0, nil, fmt.Errorf("serena repair: resolve state dir: %w", err)
	}
	intentPath := joinStateFilePath(stateDir, supervisorIntentFileLeaf)

	// Serialize against a concurrent auto-register cutover (the same process-global
	// mutex it uses), then hold the registry flock across the read + the repair
	// install so no other mcphub process observes a partial state. Mirrors
	// AutoRegisterSerenaWorkspace's lock order: install-mutex → registry-flock.
	serenaAutoRegisterInstallMu.Lock()
	defer serenaAutoRegisterInstallMu.Unlock()

	reg := NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		return 0, nil, fmt.Errorf("serena repair: lock registry: %w", err)
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
		return 0, nil, fmt.Errorf("serena repair: load registry: %w", err)
	}

	rows := reg.SerenaEntries()
	if len(rows) == 0 {
		return 0, nil, nil // no serena workspaces registered — nothing to repair
	}

	intent, ierr := ReadSupervisorIntent(intentPath)
	if ierr != nil && !errors.Is(ierr, os.ErrNotExist) {
		return 0, nil, fmt.Errorf("serena repair: read supervisor intent %s: %w", intentPath, ierr)
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
		return 0, nil, nil // healthy — every registry row owns its intent daemon
	}

	// Introduce-crash double-fault: the intent carries NO runtime_spec, so a first
	// introduce died mid-cutover. A live-add re-install cannot introduce runtime_spec
	// while the supervisor runs (§7.1 gate, design), so log + defer to migrate. This
	// is non-fatal: the GUI startup continues; the operator re-introduces the pool.
	if intent == nil || !intent.HasRuntimeSpecRow() {
		emitSerenaOrphanRepairEvent(SupervisorEventSeverityWarn, "serena-orphan-repair-deferred", orphanKeys, map[string]any{
			"reason":           "intent-has-no-runtime_spec (first-introduce crash)",
			"operator_action":  "run `mcphub migrate` to re-introduce the serena dynamic pool",
			"orphan_workspace": orphanKeys,
		})
		return 0, orphanKeys, nil
	}

	// Live-add repair: re-install the FULL serena fan-out so the orphan daemons land
	// in the intent (the §7.1 gate passes — the prior intent already has runtime_spec),
	// then reconcile so the running supervisor spawns them.
	catalog, cerr := loadSerenaCatalogManifest()
	if cerr != nil {
		return 0, nil, fmt.Errorf("serena repair: load serena catalog manifest: %w", cerr)
	}
	dyn, derr := BuildInMemorySerenaDynamicPoolManifest(catalog)
	if derr != nil {
		return 0, nil, fmt.Errorf("serena repair: build dynamic-pool manifest: %w", derr)
	}
	if _, iErr := autoRegisterInstallParsedManifestFn(ctx, a, dyn, InstallParsedManifestOpts{
		Writer:     io.Discard,
		Workspaces: rows,
	}); iErr != nil {
		return 0, nil, fmt.Errorf("serena repair: re-install fan-out for %d orphan(s) %v: %w", len(orphanKeys), orphanKeys, iErr)
	}

	// Release the flock before the reconcile (it touches neither the registry nor the
	// intent file), then nudge the running supervisor to reconcile the now-complete
	// intent NOW. A reconcile failure is non-fatal: the orphans are committed to the
	// intent, so the supervisor's own IntentWatcher (60s) backstops the spawn.
	releaseReg()
	if _, recErr := autoRegisterReconcileFn(ctx, true); recErr != nil {
		_ = recErr
	}
	emitSerenaOrphanRepairEvent(SupervisorEventSeverityInfo, "serena-orphan-repair-applied", orphanKeys, map[string]any{
		"repaired_workspace": orphanKeys,
		"mode":               "live-add re-install",
	})
	return len(orphanKeys), nil, nil
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
