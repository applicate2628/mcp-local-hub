package api

import (
	"fmt"
	"os"
	"time"
)

// ForgetAdoptProvenanceOpts controls ForgetAdoptProvenance.
type ForgetAdoptProvenanceOpts struct {
	// Yes is advisory only for the API layer (it always removes); the CLI uses it to
	// gate the confirmation prompt. Kept here so the API contract mirrors de-adopt.
	Yes bool

	// ConfirmIdentity, when true, gates the removal on the fresh under-lease row matching
	// the reviewed identity (ExpectedHasRow / ExpectedRowState / ExpectedUpdatedAt). The
	// CLI sets it so the destructive act cannot hit a row — OR a row that appeared where the
	// operator reviewed "row: none" — that was never displayed (a same-manifest adopt /
	// de-adopt / forget that committed in the gap between the dry-run's lease acquisition and
	// this one). A direct API caller that displayed no plan leaves it false to skip the gate.
	ConfirmIdentity   bool
	ExpectedHasRow    bool
	ExpectedRowState  AdoptOperationState
	ExpectedUpdatedAt time.Time
}

// ForgetAdoptProvenancePlan describes what `mcphub adopt-provenance forget <manifest>`
// would (or did) remove: the durable provenance row and/or its snapshot dir. It carries
// NAMES / PATHS / STATES only — never a secret value or config content (same redaction
// posture as the adopt-provenance events).
type ForgetAdoptProvenancePlan struct {
	ManifestName     string
	HasRow           bool
	RowState         AdoptOperationState
	UpdatedAt        time.Time // the row's identity, threaded into the --yes act (F2 gate)
	Clients          []string  // client NAMES from the row (empty if !HasRow)
	RoutedSecretKeys []string  // vault key NAMES the forgotten row referenced (F3) — forget does
	// NOT delete them; surfaced so the operator can clean the vault manually (via mcphub secrets).
	HasSnapshotDir bool
	SnapshotDir    string // absolute path to <state-dir>/adopt-provenance/<manifest>/ (display)
	Warnings       []string
}

// BuildForgetAdoptProvenancePlan reads the provenance row + snapshot-dir presence for
// manifestName UNDER the per-manifest lease and reports what forget would remove.
// Returns an error when neither a row nor a snapshot dir exists (nothing to forget), or
// when the lease is held by a concurrent adopt / de-adopt / GC.
func (a *API) BuildForgetAdoptProvenancePlan(manifestName string) (*ForgetAdoptProvenancePlan, error) {
	lk, acquired, err := tryAcquireAdoptManifestLease(manifestName)
	if err != nil {
		return nil, err
	}
	if !acquired {
		return nil, errForgetLeaseBusy(manifestName)
	}
	defer func() { _ = lk.Unlock() }()

	plan, _, err := buildForgetPlanUnderLease(manifestName)
	return plan, err
}

// ForgetAdoptProvenance removes the provenance row and/or snapshot dir for manifestName
// UNDER the per-manifest lease, then emits an `adopt-provenance-forgotten` event. It
// returns the plan it acted on. This is a DESTRUCTIVE operator escape: it discards
// provenance bookkeeping only — it does NOT restore any client config and does NOT
// touch routed vault keys (the operator cleans those manually once the row is gone).
func (a *API) ForgetAdoptProvenance(manifestName string, opts ForgetAdoptProvenanceOpts) (*ForgetAdoptProvenancePlan, error) {
	lk, acquired, err := tryAcquireAdoptManifestLease(manifestName)
	if err != nil {
		return nil, err
	}
	if !acquired {
		return nil, errForgetLeaseBusy(manifestName)
	}
	// Release explicitly BEFORE the event-log I/O below, so a contended/wedged
	// supervisor-events.log flock cannot pin the per-manifest lease after the row/snapshots
	// are already gone (which would make every subsequent adopt/de-adopt/forget for this
	// manifest report the lease busy). The deferred unlock is the safety net for early
	// returns; leaseReleased makes it idempotent (mirrors de-adopt's defer-emit-after-unlock).
	leaseReleased := false
	releaseLease := func() {
		if !leaseReleased {
			_ = lk.Unlock()
			leaseReleased = true
		}
	}
	defer releaseLease()

	plan, rec, err := buildForgetPlanUnderLease(manifestName)
	if err != nil {
		return nil, err
	}

	// F2 identity gate: refuse to destroy anything but the exact row (or exact absence of a
	// row) the operator reviewed. Covers both the row-changed case AND the row-appeared-where-
	// none-was-reviewed case (a same-manifest adopt in the gap between the two lease acquisitions).
	if opts.ConfirmIdentity {
		if err := forgetIdentityMismatch(manifestName, rec, opts); err != nil {
			return nil, err
		}
	}

	switch {
	case rec != nil:
		// reapAdoptProvenanceRow removes the snapshot dir AND the row atomically under
		// the store lock, matching on the (state, updated_at) we read under THIS held
		// lease — no concurrent adopt/GC/de-adopt can have changed it (they all take the
		// same lease first), so the match is guaranteed and the reap is not a no-op.
		if err := reapAdoptProvenanceRow(manifestName, rec.OperationState, rec.UpdatedAt); err != nil {
			return nil, fmt.Errorf("adopt-provenance forget %q: reap row: %w", manifestName, err)
		}
	case plan.HasSnapshotDir:
		// Rowless snapshot dir (a crash between capture and the row anchor, or a prior
		// partial forget): remove just the dir.
		if err := removeAdoptSnapshots(manifestName); err != nil {
			return nil, fmt.Errorf("adopt-provenance forget %q: remove snapshots: %w", manifestName, err)
		}
	}

	releaseLease() // lease is no longer needed; do NOT hold it across the event-log I/O
	emitAdoptProvenanceForgotten(manifestName, string(plan.RowState), len(plan.Clients), plan.HasSnapshotDir)
	return plan, nil
}

// forgetIdentityMismatch returns a non-nil error when the fresh under-lease row does not
// match the identity the operator reviewed (F2 + the rowless-appeared case).
func forgetIdentityMismatch(manifestName string, rec *AdoptProvenanceRecord, opts ForgetAdoptProvenanceOpts) error {
	if opts.ExpectedHasRow {
		if rec == nil {
			return fmt.Errorf("adopt-provenance forget %q: the reviewed row is gone (removed or renamed since "+
				"you reviewed it) — re-run to review the current state", manifestName)
		}
		if rec.OperationState != opts.ExpectedRowState || !rec.UpdatedAt.Equal(opts.ExpectedUpdatedAt) {
			return fmt.Errorf("adopt-provenance forget %q: the provenance row changed since it was reviewed "+
				"(reviewed state=%s, now %s) — re-run to review the current state",
				manifestName, opts.ExpectedRowState, forgetCurrentStateLabel(rec))
		}
		return nil
	}
	// The operator reviewed "row: none" (rowless snapshot dir). A row that appeared since must
	// not be silently reaped by a rowless-reviewed forget.
	if rec != nil {
		return fmt.Errorf("adopt-provenance forget %q: a provenance row (state=%s) was created since you "+
			"reviewed 'row: none' — re-run to review it before forgetting", manifestName, rec.OperationState)
	}
	return nil
}

// buildForgetPlanUnderLease assumes the per-manifest lease is already held. It returns
// the plan plus the raw record (nil when there is no row) so the caller can reap on the
// row's exact (state, updated_at).
func buildForgetPlanUnderLease(manifestName string) (*ForgetAdoptProvenancePlan, *AdoptProvenanceRecord, error) {
	plan := &ForgetAdoptProvenancePlan{ManifestName: manifestName}

	rec, found, err := ReadAdoptProvenance(manifestName)
	if err != nil {
		return nil, nil, fmt.Errorf("adopt-provenance forget %q: read store: %w", manifestName, err)
	}
	if found {
		plan.HasRow = true
		plan.RowState = rec.OperationState
		plan.UpdatedAt = rec.UpdatedAt
		plan.Clients = make([]string, 0, len(rec.Clients))
		for _, c := range rec.Clients {
			plan.Clients = append(plan.Clients, c.Client)
		}
		plan.RoutedSecretKeys = append([]string(nil), rec.RoutedSecretKeys...)
		plan.Warnings = append(plan.Warnings, forgetRowWarnings(manifestName, rec)...)
	}

	dir, derr := adoptSnapshotDir(manifestName)
	if derr != nil {
		return nil, nil, fmt.Errorf("adopt-provenance forget %q: resolve snapshot dir: %w", manifestName, derr)
	}
	if _, statErr := os.Stat(dir); statErr == nil {
		plan.HasSnapshotDir = true
		plan.SnapshotDir = dir
	} else if !os.IsNotExist(statErr) {
		return nil, nil, fmt.Errorf("adopt-provenance forget %q: stat snapshot dir: %w", manifestName, statErr)
	}

	if !plan.HasRow && !plan.HasSnapshotDir {
		return nil, nil, fmt.Errorf("adopt-provenance forget %q: no provenance row and no snapshot dir — nothing to forget", manifestName)
	}
	if !found {
		return plan, nil, nil
	}
	return plan, rec, nil
}

// forgetRowWarnings returns operator warnings for the row states forget can silently
// destroy DATA the operator likely still needs. These are WARNINGS, not refusals —
// forget is a deliberate escape — but the background GC refuses these same reaps behind
// its keep gates because the codebase classifies the mistaken reap as P1 data loss, so
// the operator must be told what they are discarding. No warning fires only for the
// provably-safe pre-Install crash orphan (the case forget exists for).
func forgetRowWarnings(manifestName string, rec *AdoptProvenanceRecord) []string {
	var warns []string
	switch rec.OperationState {
	case AdoptOperationStateAdopted:
		warns = append(warns, fmt.Sprintf(
			"row is 'adopted' (a COMMITTED adopt): forgetting it discards the ability to run "+
				"'mcphub de-adopt %s' back to the exact pre-adopt config", manifestName))
	case AdoptOperationStateDeAdopting:
		warns = append(warns,
			"row is 'de_adopting' (a de-adopt is mid-restore or crashed): forgetting it abandons "+
				"roll-forward recovery — any client not yet restored loses its pre-adopt restore source")
	case AdoptOperationStateAdopting:
		// An `adopting` row is USUALLY a pre-Install crash orphan (safe to forget), but it is
		// ALSO the state a committed-but-unflipped adopt and a preserved partial-commit
		// rollback (adopt.go InstallClientRollbackIncompleteError) sit in. If the manifest
		// still exists, its existence CANNOT be verified, OR the row is not provably
		// un-mutated, the adopt may have (partly) committed and the snapshot dir may be the
		// only non-prunable pre-adopt copy while a client is still pointed at the hub relay —
		// the exact reap the GC refuses as P1 data loss. Warn (no refusal); stay silent ONLY
		// for the provably-safe crash orphan (manifest verified absent AND provably unmutated).
		manifestExists, mErr := adoptManifestExistsFn(manifestName)
		if mErr != nil || manifestExists || !adoptRowProvablyUnmutated(*rec) {
			warns = append(warns, fmt.Sprintf(
				"row is 'adopting' but the adopt may have (partly) committed (manifest present, its "+
					"existence unverifiable, or the row not provably un-mutated): the snapshot dir may be "+
					"the only non-prunable pre-adopt copy while a client is still on the hub relay — prefer "+
					"'mcphub de-adopt %s'", manifestName))
		}
	case AdoptOperationStateClosed:
		// A `closed` row is post-de-adopt cleanup: de-adopt completed and consumed the
		// snapshots. Forgetting it is safe bookkeeping — no warning.
	default:
		// A corrupted store or a future state written without a schema bump: recoverability
		// semantics are unknown, so do NOT treat it as the safe crash orphan.
		warns = append(warns, fmt.Sprintf(
			"row has an unrecognized state %q: its recoverability is not understood — forgetting it "+
				"may discard data a restore would need; prefer inspecting it before --yes", rec.OperationState))
	}
	return warns
}

func forgetCurrentStateLabel(rec *AdoptProvenanceRecord) string {
	if rec == nil {
		return "removed"
	}
	return string(rec.OperationState)
}

func errForgetLeaseBusy(manifestName string) error {
	return fmt.Errorf("adopt-provenance forget %q: the per-manifest lease is held by a concurrent "+
		"adopt / de-adopt / GC; retry once it completes", manifestName)
}
