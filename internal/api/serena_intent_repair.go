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
// PendingSerenaRemoval mark is reported as fresh for diagnostics while the
// repair asks the liveness fence whether the teardown is actually still alive.
//
// Sizing: the marked window is [SetSerenaPendingRemoval(true) ->
// RemoveSerenaIntent -> DeleteSerenaRow] in PruneWorkspacePhases. Its slow leg
// is RemoveSerenaIntent's live-supervisor reconcile nudge, bounded by
// DefaultReconcileTimeout (30s); the two registry writes around it are
// sub-second. 10 minutes is ~20x that worst case.
//
// The TTL is not the reclaim decision — the fence is (see
// serena_removal_fence.go). Elapsed time cannot separate a teardown that
// CRASHED from one that is merely SLOW, and a repair is event-driven: a fresh
// mark from a crashed unregister might otherwise never be revisited after the
// single startup pass. Every pending mark is therefore fenced; the TTL only
// distinguishes an expected in-flight teardown from an anomalously long one in
// audit output. Every reclaim requires the fence to prove the owner is gone.
const serenaPendingRemovalLeaseTTL = 10 * time.Minute

// pendingSerenaRemovalLeaseFresh reports whether a PendingSerenaRemoval mark
// stamped at stampedAt is still within its lease at now — i.e. whether the mark
// is inside the expected teardown window for audit purposes.
//
// Both tails fail toward CONSULTING THE FENCE, never toward the permanent skip
// this lease exists to end:
//   - a ZERO stamp cannot be aged (an older binary's write, or a hand edit), so
//     it is expired;
//   - a stamp in the FUTURE (clock step or skew) is honored only while it is
//     less than one TTL ahead, so a clock jumped far forward and then back
//     cannot pin a row as "being removed" indefinitely.
//
// Neither a fresh nor expired mark authorizes reclaim on its own: the caller
// always asks the fence whether a live teardown still owns the row, and only a
// fence that proves the owner is GONE authorizes the reclaim.
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

// pendingRemovalFenceSkip records one row whose pending-removal LIVENESS FENCE
// refused the reclaim. probeErr is nil when
// the fence was cleanly observed as HELD (a live teardown), and non-nil when the
// probe itself failed (liveness undeterminable — also a fail-closed skip). The
// two are audited under one event with distinct counts so an operator can tell a
// genuinely long-running unregister apart from a broken fence.
type pendingRemovalFenceSkip struct {
	workspaceKey string
	probeErr     error
	reason       SerenaIntentRepairIncompleteReason
}

type pendingSerenaRemovalFenceClassification struct {
	reclaim          bool
	recoveryReason   SerenaIntentRepairRecoveryReason
	incompleteReason SerenaIntentRepairIncompleteReason
}

func classifyPendingSerenaRemovalFence(registryGeneration string, fence serenaRemovalFenceObservation, leaseFresh bool, probeErr error) pendingSerenaRemovalFenceClassification {
	if probeErr != nil {
		return pendingSerenaRemovalFenceClassification{incompleteReason: SerenaIntentRepairIncompleteGenerationProbeFailed}
	}
	if fence.held {
		return pendingSerenaRemovalFenceClassification{incompleteReason: SerenaIntentRepairIncompleteHolderLive}
	}
	if registryGeneration != "" && registryGeneration == fence.generation {
		return pendingSerenaRemovalFenceClassification{reclaim: true, recoveryReason: SerenaIntentRepairRecoveryGenerationReclaimed}
	}
	if !leaseFresh {
		return pendingSerenaRemovalFenceClassification{reclaim: true, recoveryReason: SerenaIntentRepairRecoveryLegacyLeaseExpired}
	}
	if registryGeneration != "" && fence.generation != "" {
		return pendingSerenaRemovalFenceClassification{incompleteReason: SerenaIntentRepairIncompleteGenerationMismatch}
	}
	return pendingSerenaRemovalFenceClassification{incompleteReason: SerenaIntentRepairIncompleteLegacyLeaseFresh}
}

// SerenaIntentRepairRecoveryReason is the stable reason one pending-removal
// row was safely reclaimed during this pass.
type SerenaIntentRepairRecoveryReason string

const (
	SerenaIntentRepairRecoveryGenerationReclaimed SerenaIntentRepairRecoveryReason = "generation_reclaimed"
	SerenaIntentRepairRecoveryLegacyLeaseExpired  SerenaIntentRepairRecoveryReason = "legacy_lease_expired"
)

type SerenaIntentRepairRecovery struct {
	WorkspaceKey string                           `json:"workspace_key"`
	Reason       SerenaIntentRepairRecoveryReason `json:"reason"`
}

// SerenaIntentRepairIncompleteReason is the stable reason one pending-removal
// row prevented this pass from certifying complete Serena alignment.
type SerenaIntentRepairIncompleteReason string

const (
	SerenaIntentRepairIncompleteHolderLive            SerenaIntentRepairIncompleteReason = "holder_live"
	SerenaIntentRepairIncompleteLegacyLeaseFresh      SerenaIntentRepairIncompleteReason = "legacy_lease_fresh"
	SerenaIntentRepairIncompleteGenerationMismatch    SerenaIntentRepairIncompleteReason = "generation_mismatch"
	SerenaIntentRepairIncompleteGenerationProbeFailed SerenaIntentRepairIncompleteReason = "generation_probe_failed"
)

type SerenaIntentRepairIncomplete struct {
	WorkspaceKey string                             `json:"workspace_key"`
	Reason       SerenaIntentRepairIncompleteReason `json:"reason"`
}

// SerenaIntentRepairOutcome is the terminal classification of one repair or
// preview pass. A lock skip is deliberately not an error: the caller still has
// a valid registry/scheduler drift result, but Serena classification is
// incomplete and must be retried before it can certify fleet alignment.
type SerenaIntentRepairOutcome string

const (
	SerenaIntentRepairOutcomeCompleted                SerenaIntentRepairOutcome = "completed"
	SerenaIntentRepairOutcomeSkippedRegistryLock      SerenaIntentRepairOutcome = "skipped_registry_lock"
	SerenaIntentRepairOutcomeSkippedIntentLock        SerenaIntentRepairOutcome = "skipped_intent_lock"
	SerenaIntentRepairOutcomeSkippedRemovalFenceProbe SerenaIntentRepairOutcome = "skipped_removal_fence_probe"
	SerenaIntentRepairOutcomeIncompleteRemovalFence   SerenaIntentRepairOutcome = "incomplete_removal_fence"
	SerenaIntentRepairOutcomeError                    SerenaIntentRepairOutcome = "error"
)

// SerenaIntentRepairResult is the single typed result emitted by the repair
// owner. Repaired is the count actually appended in apply mode or that would
// be appended in preview mode; Deferred retains the existing first-introduction
// and legacy nil-spec deferral semantics. Recovered preserves the stable cause
// for every pending-removal row safely reclaimed by this pass.
type SerenaIntentRepairResult struct {
	Outcome    SerenaIntentRepairOutcome
	Repaired   int
	Deferred   []string
	Incomplete []SerenaIntentRepairIncomplete
	Recovered  []SerenaIntentRepairRecovery
}

func completedSerenaIntentRepairResult(repaired int, deferred []string, recovered []SerenaIntentRepairRecovery) SerenaIntentRepairResult {
	return SerenaIntentRepairResult{
		Outcome:   SerenaIntentRepairOutcomeCompleted,
		Repaired:  repaired,
		Deferred:  deferred,
		Recovered: recovered,
	}
}

// finalizedSerenaIntentRepairResult preserves successful work from this pass,
// but never certifies completion while any pending-removal row remains held,
// fresh-unattributed, mismatched, or unprobeable. The skipped rows remain
// untouched and their stable reasons reach every caller.
func finalizedSerenaIntentRepairResult(repaired int, deferred []string, incomplete []SerenaIntentRepairIncomplete, recovered []SerenaIntentRepairRecovery) SerenaIntentRepairResult {
	if len(incomplete) > 0 {
		return SerenaIntentRepairResult{
			Outcome:    SerenaIntentRepairOutcomeIncompleteRemovalFence,
			Repaired:   repaired,
			Deferred:   deferred,
			Incomplete: incomplete,
			Recovered:  recovered,
		}
	}
	return completedSerenaIntentRepairResult(repaired, deferred, recovered)
}

func skippedSerenaIntentRepairResult(outcome SerenaIntentRepairOutcome) SerenaIntentRepairResult {
	return SerenaIntentRepairResult{Outcome: outcome}
}

func failedSerenaIntentRepairResult(err error) (SerenaIntentRepairResult, error) {
	if err == nil {
		err = errors.New("serena intent repair: error outcome requires a cause")
	}
	return SerenaIntentRepairResult{Outcome: SerenaIntentRepairOutcomeError}, err
}

// validateSerenaIntentRepairResult fails closed if a future return path omits
// an outcome or pairs one with the wrong Go error state. It converts the
// invalid pair into the only permitted error result rather than allowing a
// zero-value result to masquerade as a completed no-op.
func validateSerenaIntentRepairResult(result SerenaIntentRepairResult, err error) (SerenaIntentRepairResult, error) {
	for _, item := range result.Recovered {
		if item.WorkspaceKey == "" || !validSerenaIntentRepairRecoveryReason(item.Reason) {
			return failedSerenaIntentRepairResult(fmt.Errorf("serena intent repair: invalid pending-removal recovery row %+v", item))
		}
	}
	switch result.Outcome {
	case SerenaIntentRepairOutcomeCompleted:
		if err != nil {
			return failedSerenaIntentRepairResult(fmt.Errorf("serena intent repair: outcome %q cannot carry an error: %w", result.Outcome, err))
		}
		if len(result.Incomplete) != 0 {
			return failedSerenaIntentRepairResult(fmt.Errorf("serena intent repair: outcome %q cannot carry pending-removal incomplete rows", result.Outcome))
		}
	case SerenaIntentRepairOutcomeSkippedRegistryLock, SerenaIntentRepairOutcomeSkippedIntentLock, SerenaIntentRepairOutcomeSkippedRemovalFenceProbe:
		if err != nil {
			return failedSerenaIntentRepairResult(fmt.Errorf("serena intent repair: outcome %q cannot carry an error: %w", result.Outcome, err))
		}
		if len(result.Incomplete) != 0 || len(result.Recovered) != 0 {
			return failedSerenaIntentRepairResult(fmt.Errorf("serena intent repair: outcome %q cannot carry pending-removal row classifications", result.Outcome))
		}
	case SerenaIntentRepairOutcomeIncompleteRemovalFence:
		if err != nil {
			return failedSerenaIntentRepairResult(fmt.Errorf("serena intent repair: outcome %q cannot carry an error: %w", result.Outcome, err))
		}
		if len(result.Incomplete) == 0 {
			return failedSerenaIntentRepairResult(errors.New("serena intent repair: incomplete removal-fence outcome requires at least one row reason"))
		}
		for _, item := range result.Incomplete {
			if item.WorkspaceKey == "" || !validSerenaIntentRepairIncompleteReason(item.Reason) {
				return failedSerenaIntentRepairResult(fmt.Errorf("serena intent repair: invalid pending-removal incomplete row %+v", item))
			}
		}
	case SerenaIntentRepairOutcomeError:
		if err == nil {
			return failedSerenaIntentRepairResult(errors.New("serena intent repair: error outcome returned without an error"))
		}
		if len(result.Incomplete) != 0 || len(result.Recovered) != 0 {
			return failedSerenaIntentRepairResult(errors.New("serena intent repair: error outcome cannot carry pending-removal row classifications"))
		}
	default:
		return failedSerenaIntentRepairResult(fmt.Errorf("serena intent repair: unknown or empty outcome %q", result.Outcome))
	}
	return result, err
}

func validSerenaIntentRepairRecoveryReason(reason SerenaIntentRepairRecoveryReason) bool {
	switch reason {
	case SerenaIntentRepairRecoveryGenerationReclaimed, SerenaIntentRepairRecoveryLegacyLeaseExpired:
		return true
	default:
		return false
	}
}

func validSerenaIntentRepairIncompleteReason(reason SerenaIntentRepairIncompleteReason) bool {
	switch reason {
	case SerenaIntentRepairIncompleteHolderLive, SerenaIntentRepairIncompleteLegacyLeaseFresh, SerenaIntentRepairIncompleteGenerationMismatch, SerenaIntentRepairIncompleteGenerationProbeFailed:
		return true
	default:
		return false
	}
}

// RepairSerenaIntentFromRegistry re-reads the workspace registry and the
// supervisor intent under FRESH locks and APPENDS the daemon rows for any
// serena registry row whose per-workspace daemon is missing from the intent.
//
// It writes the REGISTRY in exactly one case: clearing a PendingSerenaRemoval
// mark whose fence is free (step 4b — debris from an unregister that never
// reached its registry-row delete). That write happens inside the same held
// registry flock as the classification, and it only ever drops a mark; it never
// adds, removes, or otherwise edits a row.
//
// Returns one typed outcome plus a Go error. Completed includes healthy no-op,
// first-introduction/legacy deferral, stale, and divergent-row
// verdicts. A contended registry or intent lock is a distinct retryable skip;
// only an actual path, lock syscall, load, materialization, or write failure
// returns outcome error with a non-nil cause.
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
func (a *API) RepairSerenaIntentFromRegistry(stateDir string) (SerenaIntentRepairResult, error) {
	result, err := repairSerenaIntentFromRegistry(stateDir, true)
	return validateSerenaIntentRepairResult(result, err)
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
// Returns the same typed outcome contract as the commit variant. Repaired is
// the count a real --apply WOULD append — computed through the SAME
// BuildSupervisorDaemonsForSerena call the commit path uses, so a
// manifest-shape rejection here is the SAME failure --apply would hit, never a
// preview that promises a count apply cannot actually deliver. Lock contention
// remains a distinct skipped outcome with a nil error; a real failure returns
// outcome error with its causal error.
func (a *API) PreviewSerenaIntentRepairFromRegistry(stateDir string) (SerenaIntentRepairResult, error) {
	result, err := repairSerenaIntentFromRegistry(stateDir, false)
	return validateSerenaIntentRepairResult(result, err)
}

// repairSerenaIntentFromRegistry is the shared implementation behind
// RepairSerenaIntentFromRegistry (commit=true, the pre-existing behavior —
// used by the supervisor's own startup self-heal) and
// PreviewSerenaIntentRepairFromRegistry (commit=false — used by dry-run
// reconcile). commit=false skips ONLY the final write + audit-event steps;
// every read, lock, and classification decision is identical, so the two
// modes can never diverge on WHICH rows are orphaned/deferred/skipped.
func repairSerenaIntentFromRegistry(stateDir string, commit bool) (SerenaIntentRepairResult, error) {
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
		return failedSerenaIntentRepairResult(fmt.Errorf("serena intent repair: empty state dir (caller must thread the supervisor's resolved state root)"))
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
		return failedSerenaIntentRepairResult(fmt.Errorf("serena intent repair: resolve registry path: %w", err))
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
		return failedSerenaIntentRepairResult(fmt.Errorf("serena intent repair: lock registry: %w", err))
	}
	if !ok {
		return skippedSerenaIntentRepairResult(SerenaIntentRepairOutcomeSkippedRegistryLock), nil
	}
	defer regUnlock()

	if err := reg.Load(); err != nil {
		return failedSerenaIntentRepairResult(fmt.Errorf("serena intent repair: load registry: %w", err))
	}
	rows := reg.SerenaEntries()
	if len(rows) == 0 {
		return completedSerenaIntentRepairResult(0, nil, nil), nil
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
		return failedSerenaIntentRepairResult(fmt.Errorf("serena intent repair: try-lock supervisor intent %s: %w", intentPath+supervisorIntentLockSuffix, err))
	}
	if !intentLocked {
		return skippedSerenaIntentRepairResult(SerenaIntentRepairOutcomeSkippedIntentLock), nil
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
			return failedSerenaIntentRepairResult(fmt.Errorf("serena intent repair: read supervisor intent %s: %w", intentPath, rerr))
		}
	}

	// 4. Classify each serena registry row from THIS fresh read into: SKIP
	//    (divergent key or stale path), DEFER (legacy nil-spec row), or APPEND
	//    (orphan). A nil/missing intent makes every row an orphan.
	now := time.Now().UTC()
	var missing []WorkspaceEntry
	var skippedDivergent []string
	var deferredLegacy []string
	var reclaimedRemovalMarks []WorkspaceEntry
	var recoveredRemovalMarks []SerenaIntentRepairRecovery
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
		// The FENCE is the load-bearing reclaim gate. A wall-clock lease measures
		// elapsed time, not liveness, and this repair is event-driven: honoring a
		// fresh mark without probing can strand a row forever after the single pass
		// observes a crashed unregister. The kernel releases the fence when its
		// holder dies, so a live-but-slow teardown keeps its row while a killed one
		// is recovered immediately only when the free fence generation exactly
		// matches the generation persisted in this mark. Missing or mismatched
		// metadata is unattributed legacy state: preserve a fresh lease, then
		// recover after expiry. Never retrofit a token to an old mark.
		if ws.PendingSerenaRemoval {
			leaseFresh := pendingSerenaRemovalLeaseFresh(ws.PendingSerenaRemovalAt, now)
			fence, ferr := observeSerenaRemovalFenceFn(registryDir, ws.WorkspaceKey)
			classification := classifyPendingSerenaRemovalFence(ws.PendingSerenaRemovalGeneration, fence, leaseFresh, ferr)
			if !classification.reclaim {
				fenceSkips = append(fenceSkips, pendingRemovalFenceSkip{workspaceKey: ws.WorkspaceKey, probeErr: ferr, reason: classification.incompleteReason})
				continue
			}
			reclaimedRemovalMarks = append(reclaimedRemovalMarks, ws)
			recoveredRemovalMarks = append(recoveredRemovalMarks, SerenaIntentRepairRecovery{WorkspaceKey: ws.WorkspaceKey, Reason: classification.recoveryReason})
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
	// 4b. Crash recovery for the pending-removal mark. Every row here has its
	//     liveness fence observed FREE, so the unregister that set the mark is gone
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
	if len(reclaimedRemovalMarks) > 0 {
		reclaimedKeys := missingWorkspaceKeys(reclaimedRemovalMarks)
		body := map[string]any{
			"reclaimed_count":     len(reclaimedKeys),
			"reclaimed_workspace": reclaimedKeys,
			"recovered":           recoveredRemovalMarks,
			"lease_ttl":           serenaPendingRemovalLeaseTTL.String(),
			"reason":              "generation_reclaimed denotes an exact free generation; legacy_lease_expired denotes unattributed legacy state recovered only after its lease expired",
			"operator_action":     "no action required — re-run `mcphub workspace unregister <path> --backend serena` if the removal was still intended",
		}
		if commit {
			if cerr := clearReclaimedSerenaRemovalMarks(reg, reclaimedRemovalMarks); cerr != nil {
				body["clear_error"] = cerr.Error()
			}
		}
		repairEvents = append(repairEvents, serenaIntentRepairEvent{
			severity: SupervisorEventSeverityWarn,
			event:    "serena-pending-removal-fence-free",
			body:     body,
		})
	}

	// 4c. Fence-refused reclaims. The liveness fence says the teardown that set
	//     the mark is still alive (or could not be observed at all), so the row is
	//     skipped rather than reclaimed. A probe error is also returned below as a
	//     typed incomplete result; it may not be certified as a healthy pass.
	//
	//     Emitted in BOTH modes' event slice but, like every other event here,
	//     flushed only when commit is true (preview never writes the audit log).
	if len(fenceSkips) > 0 {
		liveKeys := make([]string, 0, len(fenceSkips))
		legacyFreshKeys := make([]string, 0, len(fenceSkips))
		mismatchKeys := make([]string, 0, len(fenceSkips))
		var probeErrKeys []string
		var probeErrs []string
		for _, s := range fenceSkips {
			if s.reason == SerenaIntentRepairIncompleteHolderLive {
				liveKeys = append(liveKeys, s.workspaceKey)
				continue
			}
			if s.reason == SerenaIntentRepairIncompleteLegacyLeaseFresh {
				legacyFreshKeys = append(legacyFreshKeys, s.workspaceKey)
				continue
			}
			if s.reason == SerenaIntentRepairIncompleteGenerationMismatch {
				mismatchKeys = append(mismatchKeys, s.workspaceKey)
				continue
			}
			probeErrKeys = append(probeErrKeys, s.workspaceKey)
			probeErrs = append(probeErrs, s.probeErr.Error())
		}
		body := map[string]any{
			"skipped_count":                 len(fenceSkips),
			"live_fence_count":              len(liveKeys),
			"live_workspace":                liveKeys,
			"probe_error_count":             len(probeErrKeys),
			"legacy_lease_fresh_workspace":  legacyFreshKeys,
			"generation_mismatch_workspace": mismatchKeys,
			"lease_ttl":                     serenaPendingRemovalLeaseTTL.String(),
			"reason":                        "holder_live, legacy_lease_fresh, generation_mismatch, or generation_probe_failed prevented an immediate reclaim; only an exact free generation may bypass the legacy lease",
			"operator_action":               "no action required if an `mcphub workspace unregister` / auto-prune is genuinely in flight; if none is, check for a teardown blocked on supervisor-intent.json.lock or workspaces.yaml.lock",
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
	var repairIncomplete []SerenaIntentRepairIncomplete
	for _, s := range fenceSkips {
		repairIncomplete = append(repairIncomplete, SerenaIntentRepairIncomplete{WorkspaceKey: s.workspaceKey, Reason: s.reason})
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
		return finalizedSerenaIntentRepairResult(0, deferredLegacy, repairIncomplete, recoveredRemovalMarks), nil
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
		return finalizedSerenaIntentRepairResult(0, append(introduceKeys, deferredLegacy...), repairIncomplete, recoveredRemovalMarks), nil
	}

	// 6. Live-add APPEND. The §7.1 gate is satisfied (the prior intent already
	//    carries runtime_spec, so any running supervisor is provably this binary).
	//    Materialize the missing daemon rows from the dynamic-pool manifest and
	//    APPEND them — never replace the existing rows.
	catalog, cerr := loadSerenaCatalogManifest()
	if cerr != nil {
		return failedSerenaIntentRepairResult(fmt.Errorf("serena intent repair: load serena catalog manifest: %w", cerr))
	}
	dyn, derr := BuildInMemorySerenaDynamicPoolManifest(catalog)
	if derr != nil {
		return failedSerenaIntentRepairResult(fmt.Errorf("serena intent repair: build dynamic-pool manifest: %w", derr))
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
		return failedSerenaIntentRepairResult(fmt.Errorf("serena intent repair: intent carries a runtime_spec row but no daemon exposed a command to copy for the appended rows"))
	}

	// Materialize with manifestHash "" — mirrors the install fan-out
	// (install_parsed_manifest.go calls BuildSupervisorDaemonsForSerena with "").
	newDaemons := BuildSupervisorDaemonsForSerena(dyn, missing, "", mcphubPath)
	if len(newDaemons) == 0 {
		// Every `missing` row is a valid serena row (filtered in step 4), so the
		// fan-out must produce a descriptor for each. An empty result means a
		// manifest-shape gate inside BuildSupervisorDaemonsForSerena refused —
		// fail loud rather than silently report zero repairs.
		return failedSerenaIntentRepairResult(fmt.Errorf("serena intent repair: dynamic-pool fan-out produced no daemons for %d missing serena row(s) %v (manifest shape rejected)", len(missing), missingWorkspaceKeys(missing)))
	}

	if !commit {
		// Preview: report the count a real --apply WOULD append, computed via the
		// EXACT same BuildSupervisorDaemonsForSerena call the commit path uses
		// (so a manifest-shape rejection surfaces here as the SAME error --apply
		// would hit — never a count preview promises that --apply cannot
		// actually deliver). Never write, never emit an audit event.
		return finalizedSerenaIntentRepairResult(len(newDaemons), deferredLegacy, repairIncomplete, recoveredRemovalMarks), nil
	}

	intent.Daemons = append(intent.Daemons, newDaemons...)

	// Write under the held intent flock (writeSupervisorIntentLockHeld assumes
	// the lock at intentPath+".lock" is held, which it is — see step 3).
	if werr := writeSupervisorIntentLockHeld(intentPath, intent); werr != nil {
		return failedSerenaIntentRepairResult(fmt.Errorf("serena intent repair: write supervisor intent %s: %w", intentPath, werr))
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
	return finalizedSerenaIntentRepairResult(len(newDaemons), deferredLegacy, repairIncomplete, recoveredRemovalMarks), nil
}

// clearReclaimedSerenaRemovalMarks drops the PendingSerenaRemoval flag (and its
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
func clearReclaimedSerenaRemovalMarks(reg *Registry, reclaimed []WorkspaceEntry) error {
	changed := false
	for i := range reclaimed {
		e, ok := reg.GetSerena(reclaimed[i].WorkspaceKey)
		if !ok || (!e.PendingSerenaRemoval && e.PendingSerenaRemovalAt.IsZero() && e.PendingSerenaRemovalGeneration == "") {
			continue
		}
		e.PendingSerenaRemoval = false
		e.PendingSerenaRemovalAt = time.Time{}
		e.PendingSerenaRemovalGeneration = ""
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
