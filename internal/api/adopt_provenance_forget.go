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

	// ExpectedUpdatedAt + ExpectedRowState, when ExpectedUpdatedAt is non-zero, gate the
	// removal on the fresh under-lease row still matching what the operator reviewed (the
	// CLI passes the dry-run plan's identity). A mismatch — a same-manifest adopt / de-adopt
	// committed in the gap between the dry-run's lease acquisition and this one — errors
	// instead of destroying a row the operator never saw (the same identity idiom
	// reapAdoptProvenanceRow uses). Zero value skips the check (a direct API caller that did
	// not display a plan).
	ExpectedUpdatedAt time.Time
	ExpectedRowState  AdoptOperationState
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
	// NOT delete them; surfaced so the operator can clean the vault manually (or via de-adopt).
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
// touch routed vault keys (those are de-adopt's `--reclaim-crashed`).
func (a *API) ForgetAdoptProvenance(manifestName string, opts ForgetAdoptProvenanceOpts) (*ForgetAdoptProvenancePlan, error) {
	lk, acquired, err := tryAcquireAdoptManifestLease(manifestName)
	if err != nil {
		return nil, err
	}
	if !acquired {
		return nil, errForgetLeaseBusy(manifestName)
	}
	defer func() { _ = lk.Unlock() }()

	plan, rec, err := buildForgetPlanUnderLease(manifestName)
	if err != nil {
		return nil, err
	}

	// F2 identity gate: if the caller reviewed a plan (CLI dry-run), refuse to destroy a
	// row that changed since — a same-manifest adopt/de-adopt that committed in the gap
	// between the two lease acquisitions would otherwise be destroyed unseen.
	if !opts.ExpectedUpdatedAt.IsZero() {
		if rec == nil || rec.OperationState != opts.ExpectedRowState || !rec.UpdatedAt.Equal(opts.ExpectedUpdatedAt) {
			return nil, fmt.Errorf("adopt-provenance forget %q: the provenance row changed since it was "+
				"reviewed (reviewed state=%s, now %s) — re-run to review the current state",
				manifestName, opts.ExpectedRowState, forgetCurrentStateLabel(rec))
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

	emitAdoptProvenanceForgotten(manifestName, string(plan.RowState), len(plan.Clients), plan.HasSnapshotDir)
	return plan, nil
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
		// still exists OR the row is not provably un-mutated, the adopt may have (partly)
		// committed and the snapshot dir may be the only non-prunable pre-adopt copy while a
		// client is still pointed at the hub relay — the exact reap the GC refuses as P1 data
		// loss. Warn (no refusal); stay silent only for the provably-safe crash orphan.
		manifestExists, _ := adoptManifestExistsFn(manifestName)
		if manifestExists || !adoptRowProvablyUnmutated(*rec) {
			warns = append(warns, fmt.Sprintf(
				"row is 'adopting' but the adopt may have (partly) committed (manifest present or "+
					"not provably un-mutated): the snapshot dir may be the only non-prunable pre-adopt "+
					"copy while a client is still on the hub relay — prefer 'mcphub de-adopt %s'", manifestName))
		}
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
