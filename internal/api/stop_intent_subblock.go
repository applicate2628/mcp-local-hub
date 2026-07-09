// Package api — v0.6 Phase 4-E2 stop-intent write path (the DESTRUCTIVE
// step of the dual-intent collapse, Workstream §5 Phase E, step E2).
//
// stop_intent_subblock.go owns the SOLE stop WRITE path after E2: it folds
// every operator stop / re-enable / uninstall tombstone directly into the
// supervisor-intent.json `stops` sub-block (supervisor_intent.go), under the
// supervisor-intent flock, replacing the E1-era WriteDaemonIntent path that
// wrote the now-deleted daemon-intent.json file.
//
// Why a new owner (not a re-purpose of WriteDaemonIntent): WriteDaemonIntent
// remains the legacy daemon-intent.json writer the E2 one-time boot-merge
// still reads from when an OLD binary left a daemon-intent.json behind (and
// the merge tests seed via it). Repointing the five production stop writers
// to this sub-block owner is the E2 write-path move; the merge owner
// (intent_collapse.go) handles the read-side migration + the ordered file
// deletion.
//
// Semantics (identical to what the merge owner persists, so a live write and a
// boot-merge converge to the same sub-block shape — intent_collapse.go
// mergeDaemonIntentStops):
//   - Desired=stopped AND IsActiveStop(now) → SET Stops[task] = intent
//     (carry Desired/Reason/UpdatedAt verbatim so TTL/clock-skew/reason
//     semantics round-trip through the pure predicate unchanged).
//   - Desired=running, OR an inactive/expired stop → DELETE Stops[task].
//     This is the re-enable tombstone path: install/restart/register write
//     Desired=running to clear a prior stop so a re-installed/restarted
//     daemon is no longer suppressed (E1 docstring intent_collapse.go: "the
//     re-enable path writes a Desired=running tombstone … that the merge loop
//     drops"). After E2 the write applies that drop directly to the sub-block
//     rather than recording a running tombstone the merge later drops, and
//     snapshots the departing stop into LegacyStopWatermarks[task].
//
// The sub-block therefore stays a clean ACTIVE-STOPS-ONLY map, byte-identical
// to the merge owner's MergedStops output, so the two write paths (boot-merge
// vs live stop) can never disagree on shape.
package api

import (
	"fmt"
	"time"

	"github.com/gofrs/flock"
)

// WriteStopIntent is the Phase 4-E2 SOLE stop writer. It records (or clears)
// the per-task stop directive for taskName directly in the
// supervisor-intent.json `stops` sub-block, replacing the E1-era
// WriteDaemonIntent(daemon-intent.json) path for the five production stop
// writers (recordStopIntentAs, recordInstallIntentPostSuccess,
// recordRestartIntentForTask, recordUninstallIntentForTasks,
// recordRegisterIntentForTask).
//
// Behavior mirrors WriteDaemonIntent's contract so the five callers are a
// drop-in repoint:
//   - canonical leading-backslash key normalization (canonicalIntentTaskKey),
//     with the same 1KB IdentityFieldByteCap fail-closed guard on both `who`
//     and the canonical key;
//   - UTC normalization of UpdatedAt (zero → now);
//   - the same set-intent / clear-intent audit emission through the
//     appendIntentAuditFn seam, with Before/After snapshots, so the forensic
//     trail is unchanged.
//
// The active/inactive decision uses IsActiveStop(now) so a Desired=running or
// already-expired write DROPS the entry (re-enable) and a fresh stop SETS it —
// keeping the sub-block a pure active-stops map (see file docstring).
//
// Concurrency: acquires the supervisor-intent flock (`<path>.lock`) for the
// whole read-modify-write, serializing against every other supervisor-intent
// writer (InstallParsedManifest, serena_intent_repair, the boot-merge owner,
// the autostart shim) cross-process. It applies ONLY the Stops sub-block onto
// the freshly-read struct, so a concurrent Daemons/StrictMode/runtime_spec
// edit is never clobbered (same lost-update guard the merge owner uses).
func (a *API) WriteStopIntent(taskName string, intent DaemonIntent, who string) error {
	if len(who) > IdentityFieldByteCap {
		return ErrEntryOversize
	}
	taskName = canonicalIntentTaskKey(taskName)
	if len(taskName) > IdentityFieldByteCap {
		return ErrEntryOversize
	}

	intent.UpdatedAt = intent.UpdatedAt.UTC()
	if intent.UpdatedAt.IsZero() {
		intent.UpdatedAt = time.Now().UTC()
	}

	before, after, changed, err := mutateStopSubBlock(taskName, func(stops map[string]DaemonIntent) {
		// SET only when the directive is an ACTIVE stop; otherwise DROP the
		// entry (re-enable / expired tombstone). time.Now().UTC() is the
		// evaluation clock — a future-dated stop fail-closes to active via
		// IsActiveStop, identical to the merge owner.
		active, _ := intent.IsActiveStop(time.Now().UTC())
		if active {
			stops[taskName] = intent
		} else {
			delete(stops, taskName)
		}
	})
	if err != nil {
		return err
	}

	// Audit emission matches WriteDaemonIntent: a SET emits set-intent with
	// the After snapshot; a DROP (entry removed) emits clear-intent with the
	// Before snapshot. A no-op write (drop of an absent entry) emits nothing.
	if changed {
		emitStopIntentAudit(before, after, taskName, who, intent.Reason)
	}
	return nil
}

// WriteStopIntentIdleGuarded is the reason-guarded idle-stop writer (FIX-2a).
// It behaves like WriteStopIntent for an idle SET EXCEPT it REFUSES to overwrite
// an existing ACTIVE NON-IDLE stop: an operator stop (user-stop / user-disabled)
// or a chronic-failure quarantine written between the sweeper's status snapshot
// and this write must WIN — the idle sweeper must never resurrect-by-overwrite
// an operator-suppressed daemon. The arbitration is performed INSIDE the held
// flock (in the mutate closure), so it is atomic against every other
// supervisor-intent writer — there is no snapshot/write TOCTOU.
//
// Contract:
//   - prior entry absent OR an idle stop OR an INACTIVE stop (expired/running
//     tombstone) → SET the idle stop (the normal idle-stop path).
//   - prior entry is an ACTIVE non-idle stop → NO-OP (the operator stop stays;
//     returns nil — refusing is success, not an error, so the sweeper does not
//     log a spurious failure). The idle directive is simply dropped.
//
// `intent` must carry Reason==IntentReasonIdle (the only reason this writer is
// for); a caller passing another reason still gets the guard but the guard is
// only meaningful for idle. `now` is the evaluation clock for IsActiveStop
// (production passes time.Now().UTC()).
func (a *API) WriteStopIntentIdleGuarded(taskName string, intent DaemonIntent, who string, now time.Time) error {
	_, err := a.WriteStopIntentIdleGuardedResult(taskName, intent, who, now)
	return err
}

// WriteStopIntentIdleGuardedResult is the bool-returning form used by callers
// that must distinguish an actually written idle stop from a guarded refusal.
// The returned bool is true only when the stops sub-block changed.
func (a *API) WriteStopIntentIdleGuardedResult(taskName string, intent DaemonIntent, who string, now time.Time) (bool, error) {
	if len(who) > IdentityFieldByteCap {
		return false, ErrEntryOversize
	}
	taskName = canonicalIntentTaskKey(taskName)
	if len(taskName) > IdentityFieldByteCap {
		return false, ErrEntryOversize
	}

	intent.UpdatedAt = intent.UpdatedAt.UTC()
	if intent.UpdatedAt.IsZero() {
		intent.UpdatedAt = time.Now().UTC()
	}

	var refused *DaemonIntent
	before, after, changed, err := mutateStopSubBlock(taskName, func(stops map[string]DaemonIntent) {
		// FIX-2a: arbitration under the flock. If an ACTIVE non-idle stop is
		// already present, REFUSE the idle SET (operator reasons win). Read the
		// prior entry from the (copied) sub-block the mutate operates on.
		if prior, ok := stops[taskName]; ok {
			active, reason := prior.IsActiveStop(now.UTC())
			if active && reason != IntentReasonIdle {
				// An operator stop / chronic-failure / clock-skew-suspect is
				// active: leave it untouched. Do NOT downgrade it to idle.
				p := prior
				refused = &p
				return
			}
		}
		// No active non-idle stop blocking us: SET when the directive is itself
		// an active stop, else DROP (matches WriteStopIntent's re-enable path).
		active, _ := intent.IsActiveStop(now.UTC())
		if active {
			stops[taskName] = intent
		} else {
			delete(stops, taskName)
		}
	})
	if err != nil {
		return false, err
	}
	if changed {
		emitStopIntentAudit(before, after, taskName, who, intent.Reason)
	} else if refused != nil {
		emitIdleStopRefusedAudit(refused, taskName, who)
	}
	return changed, nil
}

// ClearStopIntentIfReason is the compare-and-clear stop clearer (FIX-2b). It
// removes the stop directive for taskName ONLY when the CURRENT on-disk entry
// is still a stop whose Reason == wantReason — read and compared INSIDE the held
// flock so there is no read/clear TOCTOU. This closes the wake-resurrection
// hole: WakeIdleSerenaDaemon reads the stop lock-free to classify it as idle,
// then clears; an operator stop written between that read and the clear would be
// erased by a blind ClearStopIntent. With the compare-and-clear, the clear is
// refused if the entry is no longer idle (the operator stop survives, and the
// wake's caller treats the daemon as still-down).
//
// It returns clearAllowed=true when the caller may proceed (matching entry
// cleared, or no entry existed) and clearAllowed=false when a different current
// reason blocked the clear. Emits clear-intent only when an entry was actually
// removed.
func (a *API) ClearStopIntentIfReason(taskName string, wantReason string, who string) (bool, error) {
	if len(who) > IdentityFieldByteCap {
		return false, ErrEntryOversize
	}
	taskName = canonicalIntentTaskKey(taskName)
	if len(taskName) > IdentityFieldByteCap {
		return false, ErrEntryOversize
	}

	before, _, changed, err := mutateStopSubBlock(taskName, func(stops map[string]DaemonIntent) {
		// FIX-2b: only delete when the CURRENT entry still matches wantReason.
		// An entry that was replaced by an operator stop (different reason)
		// between the caller's lock-free read and now is LEFT in place.
		if prior, ok := stops[taskName]; ok && prior.Reason == wantReason {
			delete(stops, taskName)
		}
	})
	if err != nil {
		return false, err
	}
	// Emit clear-intent only when we actually removed the matching entry. before
	// is the prior snapshot; the entry was removed iff before existed AND its
	// reason matched wantReason (the only case the mutate deletes).
	if changed && before != nil && before.Reason == wantReason && appendIntentAuditFn != nil {
		_ = appendIntentAuditFn(NewIntentAuditEntry(
			WithAction("clear-intent"),
			WithTask(taskName),
			WithWho(who),
			WithReason(before.Reason),
			WithBefore(before),
		))
	}
	return before == nil || before.Reason == wantReason, nil
}

// ClearStopIntent removes the stop directive for taskName from the
// supervisor-intent.json `stops` sub-block. Idempotent — a missing entry is a
// no-op success. Same flock + canonical-key + audit contract as
// ClearDaemonIntent (the E1-era daemon-intent.json clear), so it is a drop-in
// repoint for any caller that explicitly cleared a stop. Emits clear-intent
// only when the entry actually existed.
func (a *API) ClearStopIntent(taskName string, who string) error {
	if len(who) > IdentityFieldByteCap {
		return ErrEntryOversize
	}
	taskName = canonicalIntentTaskKey(taskName)
	if len(taskName) > IdentityFieldByteCap {
		return ErrEntryOversize
	}

	before, _, changed, err := mutateStopSubBlock(taskName, func(stops map[string]DaemonIntent) {
		delete(stops, taskName)
	})
	if err != nil {
		return err
	}
	if changed && before != nil && appendIntentAuditFn != nil {
		_ = appendIntentAuditFn(NewIntentAuditEntry(
			WithAction("clear-intent"),
			WithTask(taskName),
			WithWho(who),
			WithReason(before.Reason),
			WithBefore(before),
		))
	}
	return nil
}

// mutateStopSubBlock is the shared read-modify-write core for the sub-block
// stop writers. It acquires the supervisor-intent flock, re-reads the file
// fresh under it, applies `mutate` to a COPY of the existing Stops map, writes
// ONLY the recomputed Stops sub-block back (preserving every other field of
// the freshly-read struct), and returns the Before/After snapshots for the
// taskName so callers can emit the matching audit entry.
//
// before is the prior value for taskName (nil when absent); after is the new
// value (nil when the mutation removed/left-absent the entry). changed reports
// whether that stop entry actually changed; the function may still commit a
// watermark-only normalization write with changed=false, which must not emit a
// stop audit entry.
//
// Lock-free write body: it commits via writeSupervisorIntentLockHeld (the same
// helper the merge owner uses) because the flock is already held — re-entering
// WriteSupervisorIntent would re-acquire the flock and deadlock on Windows
// LockFileEx (the readIntentLocked/writeIntentLocked split daemon_intent.go
// established).
func mutateStopSubBlock(taskName string, mutate func(stops map[string]DaemonIntent)) (before, after *DaemonIntent, changed bool, err error) {
	path, err := DefaultSupervisorIntentPath()
	if err != nil {
		return nil, nil, false, fmt.Errorf("stop-intent: resolve supervisor-intent path: %w", err)
	}

	lock := flock.New(path + supervisorIntentLockSuffix)
	if err := lock.Lock(); err != nil {
		return nil, nil, false, fmt.Errorf("stop-intent: flock supervisor-intent: %w", err)
	}
	defer func() { _ = lock.Unlock() }()

	// Read FRESH under the held lock. Missing file → empty intent (a stop
	// recorded before `mcphub install` ever ran still lands in a freshly
	// minted supervisor-intent.json). readSupervisorIntentForMerge owns the
	// missing-file semantics (install_parsed_manifest.go), shared with the
	// merge owner.
	intent, _, rawStopMaps, readErr := readSupervisorIntentForMergeWithRawStopMaps(path)
	if readErr != nil {
		return nil, nil, false, fmt.Errorf("stop-intent: read supervisor-intent.json: %w", readErr)
	}
	if intent == nil {
		intent = &SupervisorIntentFile{}
	}

	// Copy the existing stops so the mutation never aliases the read struct's
	// map (defensive — the struct is local, but the copy keeps the
	// before/after snapshot honest).
	stops := map[string]DaemonIntent{}
	for k, v := range intent.Stops {
		stops[k] = v
	}
	watermarks := map[string]DaemonIntent{}
	for k, v := range intent.LegacyStopWatermarks {
		watermarks[k] = v
	}
	if prior, ok := stops[taskName]; ok {
		p := prior
		before = &p
	}

	mutate(stops)

	if newVal, ok := stops[taskName]; ok {
		n := newVal
		after = &n
	}

	switch {
	case before != nil && after == nil:
		watermarks[taskName] = *before
	}

	entryChanged := !stopEntriesEqual(before, after)
	candidate := cloneSupervisorIntentFile(intent)
	candidate.Stops = stops
	if len(candidate.Stops) == 0 {
		candidate.Stops = nil
	}
	candidate.LegacyStopWatermarks = watermarks
	if len(candidate.LegacyStopWatermarks) == 0 {
		candidate.LegacyStopWatermarks = nil
	}
	normalizeAbsentOnlyStopWatermarks(candidate)
	persistenceChanged := !stopsMapsEqual(rawStopMaps.Stops, candidate.Stops) ||
		!stopsMapsEqual(rawStopMaps.LegacyStopWatermarks, candidate.LegacyStopWatermarks)

	// Idempotent no-op short-circuit: if the mutation produced no change to
	// either the sub-block entry or its absent-only watermark, skip the write
	// entirely (no flock-held rewrite, no audit churn). stopEntriesEqual handles
	// both-present-and-equal and both-absent.
	if !persistenceChanged {
		return before, after, false, nil
	}

	intent.Stops = candidate.Stops
	intent.LegacyStopWatermarks = candidate.LegacyStopWatermarks
	if err := writeSupervisorIntentLockHeld(path, intent); err != nil {
		return nil, nil, false, fmt.Errorf("stop-intent: write supervisor-intent.json: %w", err)
	}
	return before, after, entryChanged, nil
}

// stopEntriesEqual reports whether two optional DaemonIntent snapshots are
// equal (both nil, or both present with the same Desired/Reason/UpdatedAt).
// Time equality uses Equal for monotonic-clock/location safety, mirroring
// stopsMapsEqual.
func stopEntriesEqual(a, b *DaemonIntent) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Desired == b.Desired && a.Reason == b.Reason && a.UpdatedAt.Equal(b.UpdatedAt)
}

// emitStopIntentAudit mirrors WriteDaemonIntent's auto-emitted audit record on
// the sub-block write path: a SET (after != nil) emits set-intent with the
// After snapshot + the prior Before; a DROP (after == nil but before != nil)
// emits clear-intent with the Before snapshot. A no-op (both nil) emits
// nothing. Routed through the appendIntentAuditFn seam so tests intercept it
// exactly as they do for WriteDaemonIntent.
func emitStopIntentAudit(before, after *DaemonIntent, taskName, who, reason string) {
	if appendIntentAuditFn == nil {
		return
	}
	switch {
	case after != nil:
		_ = appendIntentAuditFn(NewIntentAuditEntry(
			WithAction("set-intent"),
			WithTask(taskName),
			WithWho(who),
			WithReason(reason),
			WithBefore(before),
			WithAfter(after),
		))
	case before != nil:
		// after==nil with a prior entry → the write dropped the stop
		// (re-enable). Record it as a clear so the forensic trail shows the
		// suppression lifting.
		_ = appendIntentAuditFn(NewIntentAuditEntry(
			WithAction("clear-intent"),
			WithTask(taskName),
			WithWho(who),
			WithReason(reason),
			WithBefore(before),
		))
	}
}

func emitIdleStopRefusedAudit(before *DaemonIntent, taskName, who string) {
	if appendIntentAuditFn == nil {
		return
	}
	_ = appendIntentAuditFn(NewIntentAuditEntry(
		WithAction("idle-stop-refused-operator-stop-active"),
		WithTask(taskName),
		WithWho(who),
		WithReason(before.Reason),
		WithBefore(before),
	))
}
