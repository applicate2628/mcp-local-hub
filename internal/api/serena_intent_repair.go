// Package api — supervisor-side serena intent self-heal primitive.
//
// RepairSerenaIntentFromRegistry reconciles a registry/intent split left by a
// crash between an auto-register registry Save and its install commit.
//
// AutoRegisterSerenaWorkspace (serena_auto_register.go) holds the registry
// flock continuously from before the row Save through the install commit, so
// within a live process the registry serena row and the matching
// supervisor-intent daemon row commit atomically. But a PROCESS crash
// (taskkill, OOM, power loss) between the Save and the commit RELEASES the
// flock on death, leaving a registry serena row (Language ==
// SerenaLanguageSentinel) with NO matching daemon in supervisor-intent.json —
// an "orphan row". The resolver then forwards `/serena/mcp` calls for that
// workspace to a port no supervisor ever spawned, and the existing-row fast
// path in AutoRegisterSerenaWorkspace returns the orphan unrepaired (only the
// next auto-register for a DIFFERENT workspace would heal it as a side effect).
//
// This primitive is the supervisor's own startup self-heal: the SUPERVISOR
// calls it ONCE at startup, BEFORE it loads the intent for its first reconcile
// pass, so the reconcile reads the now-complete intent and spawns the missing
// daemons. It deliberately does NOT re-use InstallParsedManifest: that path's
// buildMergedSupervisorIntent REMOVES every serena row and re-appends from the
// caller's snapshot, so a stale snapshot would clobber a concurrent
// auto-register's freshly-committed row (the trap that killed the abandoned
// re-install approach). Instead this APPENDS only the missing serena daemon
// rows, computed from a FRESH locked read of both files — never a stale
// snapshot, never a replace-all.
package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// RepairSerenaIntentFromRegistry re-reads the workspace registry and the
// supervisor intent under FRESH locks and APPENDS the daemon rows for any
// serena registry row whose per-workspace daemon is missing from the intent.
//
// Returns:
//   - repaired:  the number of serena daemon rows appended to the intent.
//   - deferred:  workspace keys that were orphaned but could NOT be appended
//     because doing so would introduce the FIRST runtime_spec row while a
//     supervisor runs (the §7.1 split-brain hazard). The operator must run
//     `mcphub migrate serena legacy-to-dynamic-pool` to introduce the pool.
//   - err:       a real I/O / materialization error only. A healthy registry,
//     a contended lock, an empty registry, or "nothing missing" all return a
//     zero result with nil error (best-effort, non-fatal at the call site).
//
// Lock order: registry flock BEFORE intent flock — matching auto-register's own
// discipline (it holds the registry flock across its install, which acquires the
// intent lock INSIDE it). Holding registry-across-intent is deadlock-free: the
// intent lock is only ever acquired by a registry holder, so once we hold the
// registry, no other writer can be holding the intent. Both locks are held
// across the WHOLE repair so the missing-set computation and the append commit
// see a consistent, race-free view.
func (a *API) RepairSerenaIntentFromRegistry() (repaired int, deferred []string, err error) {
	// 1. Resolve the registry + intent paths.
	regPath, err := DefaultRegistryPath()
	if err != nil {
		return 0, nil, fmt.Errorf("serena intent repair: resolve registry path: %w", err)
	}
	stateDir, err := DaemonStateDir()
	if err != nil {
		return 0, nil, fmt.Errorf("serena intent repair: resolve state dir: %w", err)
	}
	intentPath := joinStateFilePath(stateDir, supervisorIntentFileLeaf)

	// 2. Registry flock (non-blocking). A concurrent auto-register / migrate /
	//    repair holding it self-heals the orphan anyway, so SKIP on contention
	//    rather than stall supervisor startup; the next startup re-scans. HOLD
	//    the registry flock across the WHOLE repair (released by the deferred
	//    unlock below), so the intent we read+write cannot be raced by another
	//    registry-holder (the only writers that take the intent lock).
	reg := NewRegistry(regPath)
	regUnlock, ok, err := reg.TryLock()
	if err != nil {
		return 0, nil, fmt.Errorf("serena intent repair: lock registry: %w", err)
	}
	if !ok {
		return 0, nil, nil // contended — the holder self-heals; next startup re-scans
	}
	defer regUnlock()

	if err := reg.Load(); err != nil {
		return 0, nil, fmt.Errorf("serena intent repair: load registry: %w", err)
	}
	rows := reg.SerenaEntries()
	if len(rows) == 0 {
		return 0, nil, nil // no serena workspaces registered — nothing to repair
	}

	// 3. Intent flock (non-blocking), acquired while STILL holding the registry
	//    flock. SKIP on contention — a hung intent writer must not stall startup.
	//    Defer BOTH unlocks in reverse acquire order (intent first, then
	//    registry via the earlier defer). The fresh locked read below is the
	//    clobber-safety point: `missing` is computed from THIS read, never a
	//    stale snapshot.
	intentLock := flock.New(intentPath + supervisorIntentLockSuffix)
	intentLocked, err := intentLock.TryLock()
	if err != nil {
		return 0, nil, fmt.Errorf("serena intent repair: try-lock supervisor intent %s: %w", intentPath+supervisorIntentLockSuffix, err)
	}
	if !intentLocked {
		return 0, nil, nil // contended — the holder commits its own intent; next startup re-scans
	}
	defer func() { _ = intentLock.Unlock() }()

	intent, rerr := ReadSupervisorIntent(intentPath)
	if rerr != nil {
		if errors.Is(rerr, os.ErrNotExist) {
			// A missing intent file is a valid empty intent (every serena row is
			// then an orphan); treat as nil and let the missing-set + introduce
			// guard below handle it.
			intent = nil
		} else {
			return 0, nil, fmt.Errorf("serena intent repair: read supervisor intent %s: %w", intentPath, rerr)
		}
	}

	// 4. Compute the MISSING serena rows from THIS fresh read. A row qualifies
	//    when it is a valid serena row (sentinel language + non-empty path) AND
	//    its per-workspace daemon is absent from the intent (a nil/missing
	//    intent makes EVERY row missing).
	var missing []WorkspaceEntry
	for i := range rows {
		ws := rows[i]
		if ws.Language != SerenaLanguageSentinel || ws.WorkspacePath == "" {
			continue
		}
		if intent == nil || !intent.HasSerenaDaemonForWorkspaceKey(ws.WorkspaceKey) {
			missing = append(missing, ws)
		}
	}
	if len(missing) == 0 {
		return 0, nil, nil // healthy — every registry row owns its intent daemon (the common case; no write)
	}

	// 5. Introduce-crash guard. A live APPEND cannot safely introduce the FIRST
	//    runtime_spec row while a supervisor is running: an OLD supervisor
	//    binary's intent watcher uses DisallowUnknownFields and would reject the
	//    new field, leaving split-brain (the §7.1 hazard). If the intent carries
	//    NO runtime_spec row, the first introduce died mid-cutover; appending
	//    here would re-introduce the same hazard. Defer to the migrate command —
	//    an explicit deferral policy, not an implicit skip.
	if intent == nil || !intent.HasRuntimeSpecRow() {
		deferredKeys := missingWorkspaceKeys(missing)
		emitSerenaIntentRepairEvent(
			SupervisorEventSeverityWarn,
			"serena-intent-repair-deferred",
			map[string]any{
				"deferred_count":     len(deferredKeys),
				"deferred_workspace": deferredKeys,
				"reason":             "supervisor intent carries no runtime_spec (first-introduce crash); a live append cannot introduce the dynamic pool while a supervisor runs (design §7.1)",
				"operator_action":    "run `mcphub migrate serena legacy-to-dynamic-pool` to re-introduce the serena dynamic pool",
			},
		)
		return 0, deferredKeys, nil
	}

	// 6. Live-add APPEND. The §7.1 gate is satisfied (the prior intent already
	//    carries runtime_spec, so any running supervisor is provably this binary).
	//    Materialize the missing daemon rows from the dynamic-pool manifest and
	//    APPEND them — never replace the existing rows.
	catalog, cerr := loadSerenaCatalogManifest()
	if cerr != nil {
		return 0, nil, fmt.Errorf("serena intent repair: load serena catalog manifest: %w", cerr)
	}
	dyn, derr := BuildInMemorySerenaDynamicPoolManifest(catalog)
	if derr != nil {
		return 0, nil, fmt.Errorf("serena intent repair: build dynamic-pool manifest: %w", derr)
	}

	// Resolve the mcphub binary path by COPYING .Command from an EXISTING serena
	// daemon in the intent, so the appended daemons stay consistent with the
	// running ones. HasRuntimeSpecRow() == true above guarantees at least one
	// runtime_spec-bearing daemon exists.
	mcphubPath := firstRuntimeSpecCommand(intent)
	if mcphubPath == "" {
		// Defensive: HasRuntimeSpecRow() is true but no row exposed a Command.
		// A blank Command would yield an unspawnable descriptor; fail loud
		// rather than commit one.
		return 0, nil, fmt.Errorf("serena intent repair: intent carries a runtime_spec row but no daemon exposed a command to copy for the appended rows")
	}

	// Materialize with manifestHash "" — mirrors the install fan-out
	// (install_parsed_manifest.go calls BuildSupervisorDaemonsForSerena with "").
	newDaemons := BuildSupervisorDaemonsForSerena(dyn, missing, "", mcphubPath)
	if len(newDaemons) == 0 {
		// Every `missing` row is a valid serena row (filtered in step 4), so the
		// fan-out must produce a descriptor for each. An empty result means a
		// manifest-shape gate inside BuildSupervisorDaemonsForSerena refused —
		// fail loud rather than silently report zero repairs.
		return 0, nil, fmt.Errorf("serena intent repair: dynamic-pool fan-out produced no daemons for %d missing serena row(s) %v (manifest shape rejected)", len(missing), missingWorkspaceKeys(missing))
	}

	intent.Daemons = append(intent.Daemons, newDaemons...)

	// Write under the held intent flock (writeSupervisorIntentLockHeld assumes
	// the lock at intentPath+".lock" is held, which it is — see step 3).
	if werr := writeSupervisorIntentLockHeld(intentPath, intent); werr != nil {
		return 0, nil, fmt.Errorf("serena intent repair: write supervisor intent %s: %w", intentPath, werr)
	}

	appliedKeys := missingWorkspaceKeys(missing)
	emitSerenaIntentRepairEvent(
		SupervisorEventSeverityInfo,
		"serena-intent-repair-applied",
		map[string]any{
			"repaired_count":     len(newDaemons),
			"repaired_workspace": appliedKeys,
			"mode":               "live-add append (no replace-all)",
		},
	)
	return len(newDaemons), nil, nil
}

// missingWorkspaceKeys projects the WorkspaceKey of each entry.
func missingWorkspaceKeys(entries []WorkspaceEntry) []string {
	keys := make([]string, 0, len(entries))
	for i := range entries {
		keys = append(keys, entries[i].WorkspaceKey)
	}
	return keys
}

// firstRuntimeSpecCommand returns the Command of the first daemon in the intent
// whose RuntimeSpec is non-nil (an existing serena per-workspace daemon), or ""
// when none is found. The Command of such a daemon is the mcphub binary path
// the supervisor execs, so copying it keeps the appended rows' Command
// consistent with the running ones.
func firstRuntimeSpecCommand(intent *SupervisorIntentFile) string {
	if intent == nil {
		return ""
	}
	for i := range intent.Daemons {
		if intent.Daemons[i].RuntimeSpec != nil {
			return intent.Daemons[i].Command
		}
	}
	return ""
}

// emitSerenaIntentRepairEvent records a best-effort structured event to
// supervisor-events.log. Mirrors the api-package emit idiom used by
// emitWorkspaceAutoRegisteredEvent / emitStaleWorkspaceSkippedEvent: resolve
// the state dir, open the canonical supervisor event log, emit, close. A
// failure to resolve/open/emit is silently non-fatal — the audit is
// observability, not a gate.
func emitSerenaIntentRepairEvent(severity, event string, body map[string]any) {
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
		Severity:      severity,
		Source:        SupervisorEventSourceReconcile,
		Event:         event,
		Body:          body,
	})
}
