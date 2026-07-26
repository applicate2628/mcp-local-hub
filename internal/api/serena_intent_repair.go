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

// registryLockRetry bounds the brief retry on the registry flock. A non-mutating
// registry READER (the serena routing cache refresh, serena_routing/resolver.go)
// holds the SAME exclusive flock only momentarily; retrying briefly avoids
// forfeiting the supervisor's single startup repair pass to such a transient
// holder. A genuinely long-held MUTATING lock (auto-register / migrate mid-install)
// still exhausts the budget and the caller skips (that holder self-heals). Total
// worst-case wait ~250ms — acceptable for a once-per-startup self-heal.
const (
	registryLockRetryAttempts = 10
	registryLockRetryDelay    = 25 * time.Millisecond
)

// serenaPendingRemovalLeaseTTL bounds how long a WorkspaceEntry's
// PendingSerenaRemoval mark is honored as "an unregister is tearing this row
// down right now" WITHOUT consulting the liveness fence, before the repair asks
// whether the teardown is actually still alive.
//
// Sizing: the marked window is [SetSerenaPendingRemoval(true) ->
// RemoveSerenaIntent -> DeleteSerenaRow] in PruneWorkspacePhases. Its slow leg
// is RemoveSerenaIntent's live-supervisor reconcile nudge, bounded by
// DefaultReconcileTimeout (30s); the two registry writes around it are
// sub-second. 10 minutes is ~20x that worst case.
//
// The TTL is NO LONGER the reclaim decision — the fence is (see
// serena_removal_fence.go). Elapsed time cannot separate a teardown that
// CRASHED from one that is merely SLOW, and both legs above sit behind BLOCKING
// flock acquires with no timeout, so a teardown can genuinely outlive any TTL
// while remaining perfectly alive. The TTL survives only as the cheap fast path:
// inside it the mark is honored outright with no probe at all, past it the fence
// is consulted. Every reclaim now additionally requires the fence to prove the
// owner is gone.
const serenaPendingRemovalLeaseTTL = 10 * time.Minute

// pendingSerenaRemovalLeaseFresh reports whether a PendingSerenaRemoval mark
// stamped at stampedAt is still within its lease at now — i.e. whether the mark
// may be honored WITHOUT probing the liveness fence.
//
// Both tails fail toward CONSULTING THE FENCE, never toward the permanent skip
// this lease exists to end:
//   - a ZERO stamp cannot be aged (an older binary's write, or a hand edit), so
//     it is expired;
//   - a stamp in the FUTURE (clock step or skew) is honored only while it is
//     less than one TTL ahead, so a clock jumped far forward and then back
//     cannot pin a row as "being removed" indefinitely.
//
// "Expired" no longer means "reclaim" on its own: past the lease the caller asks
// the fence whether a live teardown still owns the row, and only a fence that
// proves the owner is GONE authorizes the reclaim.
func pendingSerenaRemovalLeaseFresh(stampedAt, now time.Time) bool {
	if stampedAt.IsZero() {
		return false
	}
	if stampedAt.After(now) {
		return stampedAt.Sub(now) < serenaPendingRemovalLeaseTTL
	}
	return now.Sub(stampedAt) < serenaPendingRemovalLeaseTTL
}

type serenaIntentRepairEvent struct {
	severity string
	event    string
	taskName string
	body     map[string]any
}

// pendingRemovalFenceSkip records one row whose pending-removal mark outlived
// its lease but whose LIVENESS FENCE refused the reclaim. probeErr is nil when
// the fence was cleanly observed as HELD (a live teardown), and non-nil when the
// probe itself failed (liveness undeterminable — also a fail-closed skip). The
// two are audited under one event with distinct counts so an operator can tell a
// genuinely long-running unregister apart from a broken fence.
type pendingRemovalFenceSkip struct {
	workspaceKey string
	probeErr     error
}

// RepairSerenaIntentFromRegistry re-reads the workspace registry and the
// supervisor intent under FRESH locks and APPENDS the daemon rows for any
// serena registry row whose per-workspace daemon is missing from the intent.
//
// It writes the REGISTRY in exactly one case: clearing a PendingSerenaRemoval
// mark whose lease has expired (step 4b — debris from an unregister that never
// reached its registry-row delete). That write happens inside the same held
// registry flock as the classification, and it only ever drops a mark; it never
// adds, removes, or otherwise edits a row.
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
// intent lock INSIDE it).
//
// Deadlock-freedom comes from TryLock on BOTH locks (skip-on-contention), NOT
// from exclusive ownership. Other writers DO take the supervisor-intent.json.lock
// leaf WITHOUT holding the registry lock — strict-mode and autostart both
// write the intent under that leaf alone. So the repair must never BLOCK while
// holding a lock; because both acquisitions are non-blocking, it can never wait
// on a lock that another (possibly stuck) holder is behind. Do NOT "optimize"
// either TryLock into a blocking Lock — that reintroduces a real deadlock against
// a concurrent intent writer.
//
// Clobber-safety has two independent layers: (a) vs a concurrent auto-register —
// registry-lock mutual exclusion (auto-register holds the registry lock across its
// whole install, so while we hold it no auto-register can be mid-commit); (b) vs
// any other intent writer — the shared supervisor-intent.json.lock leaf serializes
// the read-modify-write. We hold that leaf across the WHOLE repair, so the
// missing-set computation and the append commit see one consistent, race-free
// snapshot and no concurrent write can interleave between our read and our write.
func (a *API) RepairSerenaIntentFromRegistry(stateDir string) (repaired int, deferred []string, err error) {
	return repairSerenaIntentFromRegistry(stateDir, true)
}

// PreviewSerenaIntentRepairFromRegistry computes the IDENTICAL classification
// RepairSerenaIntentFromRegistry would act on — the same registry+intent
// locks, the same orphan/deferred/skip rules, even the same manifest-shape
// validation of the descriptors that would be appended — but NEVER writes
// supervisor-intent.json, NEVER writes the registry (including the expired
// pending-removal mark the commit path clears), and NEVER emits an audit event.
//
// This closes the "unpreviewed global side effect" gap (BLOCKING 3,
// mcphub-register-intent REVISE round 2): before this existed, ONLY
// apply-mode reconcile ever computed which orphaned serena registry rows
// would be materialized, so a dry-run `mcphub reconcile` could never predict
// what the very next `--apply` was about to silently do — including to
// workspaces the dry-run caller never asked about. handleReconcile now calls
// this in dry-run mode and RepairSerenaIntentFromRegistry (via the shared
// commit=true path) in apply mode, surfacing the SAME count/deferred-keys
// tuple in ReconcileResponse either way.
//
// Returns wouldRepair (the count of daemon rows a real --apply WOULD append —
// computed via the SAME BuildSupervisorDaemonsForSerena call the commit path
// uses, so a manifest-shape rejection here is the SAME failure --apply would
// hit, never a preview that promises a count apply cannot actually deliver),
// the same deferred-key semantics as the commit variant, and a nil error on
// every benign outcome (lock contention, empty registry, etc.) exactly like
// the commit variant.
func (a *API) PreviewSerenaIntentRepairFromRegistry(stateDir string) (wouldRepair int, deferred []string, err error) {
	return repairSerenaIntentFromRegistry(stateDir, false)
}

// repairSerenaIntentFromRegistry is the shared implementation behind
// RepairSerenaIntentFromRegistry (commit=true, the pre-existing behavior —
// used by the supervisor's own startup self-heal) and
// PreviewSerenaIntentRepairFromRegistry (commit=false — used by dry-run
// reconcile). commit=false skips ONLY the final write + audit-event steps;
// every read, lock, and classification decision is identical, so the two
// modes can never diverge on WHICH rows are orphaned/deferred/skipped.
func repairSerenaIntentFromRegistry(stateDir string, commit bool) (repaired int, deferred []string, err error) {
	// 1. Resolve the registry + intent paths. stateDir is the SUPERVISOR's already-
	//    resolved state root, threaded in by the caller — NOT re-resolved via
	//    DaemonStateDir() here. The cli stateDirFunc honors MCPHUB_STATE_DIR_OVERRIDE
	//    (env) while api.DaemonStateDir() honors a separate package-var override
	//    (daemonStateRootOverride); under such a seam the two diverge, and an
	//    internally-resolved repair would write a DIFFERENT supervisor-intent.json
	//    than the loadIntentFiles(stateDir, ...) the caller runs immediately after —
	//    leaving the orphan unrepaired for the first reconcile, and possibly mutating
	//    the default user's state file. Threading stateDir keeps both on one root.
	//    The registry stays on DefaultRegistryPath() — the canonical resolver
	//    auto-register also uses, so repair reads exactly the rows auto-register wrote.
	if stateDir == "" {
		return 0, nil, fmt.Errorf("serena intent repair: empty state dir (caller must thread the supervisor's resolved state root)")
	}

	// Event emission touches supervisor-events.log. Keep it out of the
	// registry/intent critical section: collect best-effort events while the locks
	// are held, then let the lock defers below run before this earlier defer emits
	// them with TryEmit. This preserves startup's non-fatal repair contract even if
	// the event-log lock is wedged.
	//
	// Preview (commit=false) NEVER flushes these: the audit log records ACTUAL
	// repairs, not what-if projections, and a dry-run reconcile may be polled
	// far more often than an apply — flushing "would repair" / "would skip"
	// warnings on every poll would spam supervisor-events.log with events that
	// describe nothing that actually happened.
	var repairEvents []serenaIntentRepairEvent
	defer func() {
		if !commit {
			return
		}
		for _, evt := range repairEvents {
			emitSerenaIntentRepairEvent(stateDir, evt)
		}
	}()

	regPath, err := DefaultRegistryPath()
	if err != nil {
		return 0, nil, fmt.Errorf("serena intent repair: resolve registry path: %w", err)
	}
	intentPath := joinStateFilePath(stateDir, supervisorIntentFileLeaf)
	// The per-workspace unregister fences are co-located with the registry (the
	// same rule the default-workspace marker follows), NOT with the supervisor
	// state dir: the unregister side resolves only a registry path, and the two
	// roots can diverge under a test/override seam. Deriving both sides from the
	// registry directory keeps the fence a single shared rendezvous.
	registryDir := filepath.Dir(regPath)

	// 2. Registry flock with a BRIEF BOUNDED RETRY, HELD across the WHOLE repair.
	//    A single TryLock-then-skip is too eager: a non-mutating registry READER —
	//    the serena routing cache refresh (serena_routing/resolver.go:123) or a
	//    workspace proxy bind — briefly takes the SAME exclusive flock just to read
	//    SerenaEntries and does NOT write supervisor-intent.json, so skipping on its
	//    momentary hold would forfeit the only startup repair pass and leave the
	//    orphan for this whole supervisor lifetime. Retry briefly so a transient
	//    reader clears; a genuinely long-held MUTATING lock (auto-register / migrate
	//    mid-install) still exhausts the budget and we skip (that holder self-heals
	//    the orphan via its own replace-all install). Holding the registry across
	//    the intent read+write keeps another registry-holder from racing it.
	reg := NewRegistry(regPath)
	regUnlock, ok, err := tryLockRegistryBrief(reg)
	if err != nil {
		return 0, nil, fmt.Errorf("serena intent repair: lock registry: %w", err)
	}
	if !ok {
		return 0, nil, nil // long-held mutating lock — that holder self-heals; next startup re-scans
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

	// 4. Classify each serena registry row from THIS fresh read into: SKIP
	//    (divergent key or stale path), DEFER (legacy nil-spec row), or APPEND
	//    (orphan). A nil/missing intent makes every row an orphan.
	now := time.Now().UTC()
	var missing []WorkspaceEntry
	var skippedDivergent []string
	var deferredLegacy []string
	var expiredRemovalMarks []WorkspaceEntry
	var fenceSkips []pendingRemovalFenceSkip
	for i := range rows {
		ws := rows[i]
		if ws.Language != SerenaLanguageSentinel || ws.WorkspacePath == "" {
			continue
		}
		// Mid-unregister guard (unregister-resurrects-serena-intent fix). A row
		// PruneWorkspacePhases has flagged PendingSerenaRemoval is DELIBERATELY
		// being torn down: its matching intent descriptor was just removed (or is
		// about to be) and a reconcile was nudged in the SAME window this repair
		// can run in. Without this check, that transient
		// registry-row-present/intent-row-absent state is indistinguishable from
		// a genuine crash-orphan, and repair would re-append the very row the
		// operator just unregistered. Skip silently — not a defer, not a warning:
		// this is expected, self-resolving state (the row disappears for good once
		// DeleteSerenaRow commits, or the flag clears if the teardown fails and a
		// normal orphan classification applies on the next pass).
		//
		// "Self-resolving" holds for a teardown that RUNS to a verdict; it does
		// NOT hold for one that is INTERRUPTED after the mark and the descriptor
		// removal but before DeleteSerenaRow (the process is killed, or that
		// delete's registry write fails) — nothing then ever clears the flag, and
		// an unconditional skip here would strand the row forever: no daemon in
		// the intent, the resolver still routing to it, and `mcphub workspace
		// register` still rejecting it as already registered. So the mark must be
		// reclaimable — but ONLY once its owner is provably gone.
		//
		// Two conditions gate that reclaim, in cost order:
		//
		//  1. The LEASE must have expired. Inside it the mark is honored with no
		//     probe at all (the overwhelmingly common case: a teardown finishes in
		//     well under a second).
		//  2. The FENCE must be free. This is the load-bearing one. A wall-clock
		//     lease measures elapsed time, not liveness, and both slow legs of the
		//     teardown sit behind BLOCKING flock acquires with no timeout — so a
		//     teardown blocked past the lease is indistinguishable from a dead one
		//     by the clock alone. Reclaiming there produced a resurrected
		//     descriptor with no registry row: the repair cleared the mark, the
		//     unblocked teardown's own reconcile re-appended the now-unmarked
		//     "orphan", and its DeleteSerenaRow then removed only the registry
		//     row. The fence answers the liveness question directly — the kernel
		//     releases it when its holder dies — so a live-but-slow teardown keeps
		//     its row and a killed one releases it (serena_removal_fence.go).
		//
		// The fence is consulted ONLY to ADD skips, never to remove the lease's
		// protection: an unobservable fence, or a mark written by an older binary
		// that took no fence at all, still gets the full lease window before
		// anything reclaims it.
		if ws.PendingSerenaRemoval {
			if pendingSerenaRemovalLeaseFresh(ws.PendingSerenaRemovalAt, now) {
				continue
			}
			held, ferr := serenaRemovalFenceHeldFn(registryDir, ws.WorkspaceKey)
			if ferr != nil || held {
				// held → a live teardown owns this row; ferr → liveness could not
				// be determined. Fail closed on both: the cost of skipping is one
				// more pass before recovery, the cost of reclaiming wrongly is the
				// resurrected-descriptor split above. Audit it either way — a mark
				// this old is anomalous regardless of which branch fired.
				fenceSkips = append(fenceSkips, pendingRemovalFenceSkip{
					workspaceKey: ws.WorkspaceKey,
					probeErr:     ferr,
				})
				continue
			}
			expiredRemovalMarks = append(expiredRemovalMarks, ws)
			// fall through to the normal classification below
		}
		// Divergence guard: WorkspaceKey must equal WorkspaceKey(WorkspacePath).
		// Detection keys off ws.WorkspaceKey, but the materialized daemon's TaskName
		// derives from WorkspaceKey(ws.WorkspacePath) (BuildSupervisorDaemonsForSerena
		// -> SerenaTaskNameForWorkspace). Auto-register/manual rows agree (same
		// canonical path); a hand-edited workspaces.yaml or a legacy
		// pre-symlink-resolution row (CanonicalWorkspacePathLegacyCompat) could
		// diverge → an appended daemon would be seen as still-missing next startup
		// and re-appended forever. Skip + warn (fail closed).
		if ws.WorkspaceKey != WorkspaceKey(ws.WorkspacePath) {
			skippedDivergent = append(skippedDivergent, ws.WorkspaceKey)
			continue
		}
		// Stale-path filter (bot PR #256 F1). A deleted/moved workspace dir makes the
		// materialized daemon spawn-loop: BuildSupervisorDaemonsForSerena emits the
		// descriptor verbatim and the supervisor sets cmd.Dir = d.Workspace
		// unconditionally before cmd.Start. The install fan-out drops these rows via
		// filterExistingWorkspaceRows before materializing
		// (install_parsed_manifest.go:526); the repair applies the same liveness
		// predicate + the same emitStaleWorkspaceSkippedEvent so the prune is never
		// silent.
		if workspacePathStale(ws.WorkspacePath) {
			repairEvents = append(repairEvents, staleWorkspaceSkippedRepairEvent(SerenaServerName, ws.WorkspacePath))
			continue
		}
		// Presence classification (bot PR #256 F2). HasSerenaDaemonForWorkspaceKey is
		// TASK-NAME-only; a legacy pre-redesign serena row (RuntimeSpec == nil) carries
		// the task name but the reconciler EXCLUDES it from the spawn-desired set
		// (supervise_reconcile.go:177), so it is not a live daemon.
		switch {
		case intent.HasSpecBearingSerenaDaemonForWorkspaceKey(ws.WorkspaceKey):
			// Healthy — a spec-bearing daemon already owns this workspace.
			continue
		case intent != nil && intent.HasSerenaDaemonForWorkspaceKey(ws.WorkspaceKey):
			// A nil-spec legacy row occupies the task name. Appending a spec-bearing
			// row would DUPLICATE the TaskName; replacing it would violate the
			// append-only contract. Defer to the re-install/migrate that
			// re-materializes the runtime_spec (the reconciler's own guidance at
			// supervise_reconcile.go:187).
			deferredLegacy = append(deferredLegacy, ws.WorkspaceKey)
		default:
			// Orphan — no serena daemon at all for this workspace (the core repair).
			missing = append(missing, ws)
		}
	}
	// 4b. Crash recovery for the pending-removal mark. Every row here cleared BOTH
	//     gates: its mark aged out of the lease AND its liveness fence was
	//     observed FREE, so the unregister that set the mark is provably gone
	//     (the kernel releases a flock on holder death) — it never reached its
	//     DeleteSerenaRow. Each has ALREADY been reclassified normally (as an
	//     orphan / healthy / deferred row, whichever it is). Clear the stale
	//     mark so the registry stops asserting a teardown that is not happening
	//     — we still hold the registry flock, so this write cannot race another
	//     registry mutator.
	//
	//     Preview (commit=false) never writes: the classification above is
	//     already identical in both modes, so skipping the clear here cannot
	//     make preview and apply disagree about WHICH rows are orphaned.
	//
	//     A clear failure is NON-fatal and audited, never returned: the mark is
	//     bookkeeping, the intent append below is the actual recovery, and
	//     failing the whole repair over a registry write would keep the
	//     workspace unusable for exactly the operator this branch exists to
	//     rescue. The next pass re-classifies and retries the clear.
	if len(expiredRemovalMarks) > 0 {
		expiredKeys := missingWorkspaceKeys(expiredRemovalMarks)
		body := map[string]any{
			"expired_count":     len(expiredKeys),
			"expired_workspace": expiredKeys,
			"lease_ttl":         serenaPendingRemovalLeaseTTL.String(),
			"reason":            "registry row carries a pending_serena_removal mark older than the lease AND its unregister fence is free, so the unregister that set it is provably gone and never reached its registry-row delete (killed process, or a failed delete write); the row is reclassified as an ordinary crash-orphan and the stale mark is cleared",
			"operator_action":   "no action required — re-run `mcphub workspace unregister <path> --backend serena` if the removal was still intended",
		}
		if commit {
			if cerr := clearExpiredSerenaRemovalMarks(reg, expiredRemovalMarks); cerr != nil {
				body["clear_error"] = cerr.Error()
			}
		}
		repairEvents = append(repairEvents, serenaIntentRepairEvent{
			severity: SupervisorEventSeverityWarn,
			event:    "serena-pending-removal-lease-expired",
			body:     body,
		})
	}

	// 4c. Fence-refused reclaims. The mark outlived its lease, but the liveness
	//     fence says the teardown that set it is still alive (or could not be
	//     observed at all), so the row was skipped rather than reclaimed. This is
	//     the event that distinguishes "a teardown has been blocked for over ten
	//     minutes" from a silent no-op — without it, a wedged unregister would
	//     look identical to a healthy pass.
	//
	//     Emitted in BOTH modes' event slice but, like every other event here,
	//     flushed only when commit is true (preview never writes the audit log).
	if len(fenceSkips) > 0 {
		liveKeys := make([]string, 0, len(fenceSkips))
		var probeErrKeys []string
		var probeErrs []string
		for _, s := range fenceSkips {
			if s.probeErr == nil {
				liveKeys = append(liveKeys, s.workspaceKey)
				continue
			}
			probeErrKeys = append(probeErrKeys, s.workspaceKey)
			probeErrs = append(probeErrs, s.probeErr.Error())
		}
		body := map[string]any{
			"skipped_count":     len(fenceSkips),
			"live_fence_count":  len(liveKeys),
			"live_workspace":    liveKeys,
			"probe_error_count": len(probeErrKeys),
			"lease_ttl":         serenaPendingRemovalLeaseTTL.String(),
			"reason":            "registry row carries a pending_serena_removal mark older than the lease, but its per-workspace unregister fence is still held (a live teardown) or could not be probed; the row was skipped instead of reclaimed, because reclaiming a LIVE teardown's row resurrects its descriptor with no registry row behind it",
			"operator_action":   "no action required if an `mcphub workspace unregister` / auto-prune is genuinely in flight; if none is, check for a teardown blocked on supervisor-intent.json.lock or workspaces.yaml.lock",
		}
		if len(probeErrKeys) > 0 {
			body["probe_error_workspace"] = probeErrKeys
			body["probe_error"] = probeErrs
		}
		repairEvents = append(repairEvents, serenaIntentRepairEvent{
			severity: SupervisorEventSeverityWarn,
			event:    "serena-pending-removal-fence-held",
			body:     body,
		})
	}

	if len(skippedDivergent) > 0 {
		repairEvents = append(repairEvents, serenaIntentRepairEvent{
			severity: SupervisorEventSeverityWarn,
			event:    "serena-intent-repair-divergent-row-skipped",
			body: map[string]any{
				"skipped_count":     len(skippedDivergent),
				"skipped_workspace": skippedDivergent,
				"reason":            "registry row WorkspaceKey != WorkspaceKey(WorkspacePath) (hand-edited workspaces.yaml or a legacy pre-symlink-resolution row); appending would re-append on every boot",
				"operator_action":   "re-register the workspace so its registry key matches its canonical path",
			},
		})
	}
	if len(deferredLegacy) > 0 {
		repairEvents = append(repairEvents, serenaIntentRepairEvent{
			severity: SupervisorEventSeverityWarn,
			event:    "serena-intent-repair-legacy-nil-spec-deferred",
			body: map[string]any{
				"deferred_count":     len(deferredLegacy),
				"deferred_workspace": deferredLegacy,
				"reason":             "supervisor-intent.json has a pre-redesign serena row (no runtime_spec) for this workspace; the reconciler excludes it from the spawn set and the append-only repair cannot replace it",
				"operator_action":    "run `mcphub migrate serena legacy-to-dynamic-pool` (or re-install) to re-materialize the descriptor with a runtime_spec",
			},
		})
	}
	if len(missing) == 0 {
		// No orphan to append. Still report any legacy nil-spec deferrals so the
		// caller (and the operator) sees a workspace that needs a migrate.
		return 0, deferredLegacy, nil
	}

	// 5. Introduce-crash guard. A live APPEND cannot safely introduce the FIRST
	//    runtime_spec row while a supervisor is running: an OLD supervisor
	//    binary's intent watcher uses DisallowUnknownFields and would reject the
	//    new field, leaving split-brain (the §7.1 hazard). If the intent carries
	//    NO runtime_spec row, the first introduce died mid-cutover; appending
	//    here would re-introduce the same hazard. Defer to the migrate command —
	//    an explicit deferral policy, not an implicit skip.
	if intent == nil || !intent.HasRuntimeSpecRow() {
		introduceKeys := missingWorkspaceKeys(missing)
		repairEvents = append(repairEvents, serenaIntentRepairEvent{
			severity: SupervisorEventSeverityWarn,
			event:    "serena-intent-repair-deferred",
			body: map[string]any{
				"deferred_count":     len(introduceKeys),
				"deferred_workspace": introduceKeys,
				"reason":             "supervisor intent carries no runtime_spec (first-introduce crash); a live append cannot introduce the dynamic pool while a supervisor runs (design §7.1)",
				"operator_action":    "run `mcphub migrate serena legacy-to-dynamic-pool` to re-introduce the serena dynamic pool",
			},
		})
		// Both the introduce-crash orphans and any legacy nil-spec rows defer to
		// the same migrate; return their union (deferredLegacy is empty when the
		// intent is absent — there are no daemons to be nil-spec).
		return 0, append(introduceKeys, deferredLegacy...), nil
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

	if !commit {
		// Preview: report the count a real --apply WOULD append, computed via the
		// EXACT same BuildSupervisorDaemonsForSerena call the commit path uses
		// (so a manifest-shape rejection surfaces here as the SAME error --apply
		// would hit — never a count preview promises that --apply cannot
		// actually deliver). Never write, never emit an audit event.
		return len(newDaemons), deferredLegacy, nil
	}

	intent.Daemons = append(intent.Daemons, newDaemons...)

	// Write under the held intent flock (writeSupervisorIntentLockHeld assumes
	// the lock at intentPath+".lock" is held, which it is — see step 3).
	if werr := writeSupervisorIntentLockHeld(intentPath, intent); werr != nil {
		return 0, nil, fmt.Errorf("serena intent repair: write supervisor intent %s: %w", intentPath, werr)
	}

	appliedKeys := missingWorkspaceKeys(missing)
	repairEvents = append(repairEvents, serenaIntentRepairEvent{
		severity: SupervisorEventSeverityInfo,
		event:    "serena-intent-repair-applied",
		body: map[string]any{
			"repaired_count":     len(newDaemons),
			"repaired_workspace": appliedKeys,
			"mode":               "live-add append (no replace-all)",
		},
	})
	// deferredLegacy (nil-spec rows that coexisted with the spec-bearing pool) is
	// returned alongside the append count so the caller still surfaces them.
	return len(newDaemons), deferredLegacy, nil
}

// clearExpiredSerenaRemovalMarks drops the PendingSerenaRemoval flag (and its
// stamp) from each given row and commits the registry.
//
// Callers MUST already hold the registry flock: reg is the SAME already-loaded
// *Registry the repair classified from, and (*Registry).Save takes no internal
// lock by contract (see its doc comment), so this is a lock-held read-modify-
// write inside the repair's own critical section — never a second acquisition.
//
// It re-reads each row out of reg rather than writing back the classification-
// time copies, so an unrelated field another step of this same pass may have
// touched is not reverted, and a row that vanished under us is skipped.
func clearExpiredSerenaRemovalMarks(reg *Registry, expired []WorkspaceEntry) error {
	changed := false
	for i := range expired {
		e, ok := reg.GetSerena(expired[i].WorkspaceKey)
		if !ok || (!e.PendingSerenaRemoval && e.PendingSerenaRemovalAt.IsZero()) {
			continue
		}
		e.PendingSerenaRemoval = false
		e.PendingSerenaRemovalAt = time.Time{}
		reg.Put(e)
		changed = true
	}
	if !changed {
		return nil
	}
	return reg.Save()
}

// missingWorkspaceKeys projects the WorkspaceKey of each entry.
func missingWorkspaceKeys(entries []WorkspaceEntry) []string {
	keys := make([]string, 0, len(entries))
	for i := range entries {
		keys = append(keys, entries[i].WorkspaceKey)
	}
	return keys
}

// tryLockRegistryBrief acquires the registry flock with a brief bounded retry
// (registryLockRetryAttempts attempts, registryLockRetryDelay apart). Returns
// (unlock, true, nil) on success, (nil, false, nil) if every attempt found the
// lock contended, or (nil, false, err) on a real filesystem error. See the
// registryLockRetry constants for why a single TryLock-then-skip is too eager.
func tryLockRegistryBrief(reg *Registry) (func(), bool, error) {
	for attempt := range registryLockRetryAttempts {
		unlock, ok, err := reg.TryLock()
		if err != nil {
			return nil, false, err
		}
		if ok {
			return unlock, true, nil
		}
		if attempt < registryLockRetryAttempts-1 {
			time.Sleep(registryLockRetryDelay)
		}
	}
	return nil, false, nil
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
// failure to resolve/open/emit (including event-log lock contention) is silently
// non-fatal — the audit is observability, not a gate.
func emitSerenaIntentRepairEvent(stateDir string, evt serenaIntentRepairEvent) {
	logger, openErr := OpenSupervisorEventLog(filepath.Join(stateDir, SupervisorEventLogFileLeaf))
	if openErr != nil {
		return
	}
	defer func() { _ = logger.Close() }()
	_ = logger.TryEmit(SupervisorEvent{
		SchemaVersion: SupervisorEventSchemaVersion,
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Severity:      evt.severity,
		Source:        SupervisorEventSourceReconcile,
		Event:         evt.event,
		TaskName:      evt.taskName,
		Body:          evt.body,
	})
}

func staleWorkspaceSkippedRepairEvent(server, workspacePath string) serenaIntentRepairEvent {
	return serenaIntentRepairEvent{
		severity: SupervisorEventSeverityWarn,
		event:    "stale-workspace-skipped",
		taskName: SerenaTaskNameForWorkspace(workspacePath),
		body: map[string]any{
			"server":         server,
			"workspace_path": workspacePath,
			"reason":         "workspace path no longer exists on disk; daemon row dropped before supervisor-intent write to avoid cmd.Dir spawn-loop",
		},
	}
}
