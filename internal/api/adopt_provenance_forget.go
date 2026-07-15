package api

import (
	"fmt"
	"os"
)

// ForgetAdoptProvenanceOpts controls ForgetAdoptProvenance.
type ForgetAdoptProvenanceOpts struct {
	// Yes is advisory only for the API layer (it always removes); the CLI uses it to
	// gate the confirmation prompt. Kept here so the API contract mirrors de-adopt.
	Yes bool
}

// ForgetAdoptProvenancePlan describes what `mcphub adopt-provenance forget <manifest>`
// would (or did) remove: the durable provenance row and/or its snapshot dir. It carries
// NAMES / PATHS / STATES only — never a secret value or config content (same redaction
// posture as the adopt-provenance events).
type ForgetAdoptProvenancePlan struct {
	ManifestName   string
	HasRow         bool
	RowState       AdoptOperationState
	Clients        []string // client NAMES from the row (empty if !HasRow)
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
		plan.Clients = make([]string, 0, len(rec.Clients))
		for _, c := range rec.Clients {
			plan.Clients = append(plan.Clients, c.Client)
		}
		if rec.OperationState == AdoptOperationStateAdopted {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf(
				"row is 'adopted' (a COMMITTED adopt): forgetting it discards the ability to run "+
					"'mcphub de-adopt %s' back to the exact pre-adopt config", manifestName))
		}
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
		rec = nil
	}
	return plan, rec, nil
}

func errForgetLeaseBusy(manifestName string) error {
	return fmt.Errorf("adopt-provenance forget %q: the per-manifest lease is held by a concurrent "+
		"adopt / de-adopt / GC; retry once it completes", manifestName)
}
