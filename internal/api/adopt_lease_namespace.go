package api

import "fmt"

type AdoptLeaseNamespaceState string

const (
	AdoptLeaseNamespaceReady   AdoptLeaseNamespaceState = "ready"
	AdoptLeaseNamespaceMissing AdoptLeaseNamespaceState = "missing"
	AdoptLeaseNamespaceLegacy  AdoptLeaseNamespaceState = "legacy"
	AdoptLeaseNamespaceRefused AdoptLeaseNamespaceState = "refused"
)

// AdoptLeaseNamespaceReport is deliberately path-free. Counts describe only
// validated entry kinds; names, paths, SIDs, and raw OS errors never cross the
// API boundary.
type AdoptLeaseNamespaceReport struct {
	State             AdoptLeaseNamespaceState `json:"state"`
	ReasonID          AdoptLeaseReasonID       `json:"reason_id"`
	Action            AdoptLeaseAction         `json:"action"`
	MigrationEligible bool                     `json:"migration_eligible"`
	LeaseLeafCount    int                      `json:"lease_leaf_count"`
	SnapshotDirCount  int                      `json:"snapshot_dir_count"`
	ChangedLeafCount  int                      `json:"changed_leaf_count,omitempty"`
	NamespaceChanged  bool                     `json:"namespace_changed,omitempty"`
	RollbackPerformed bool                     `json:"rollback_performed,omitempty"`
}

type AdoptLeaseNamespaceMigrationOpts struct {
	Yes bool
}

// LeaseNamespaceFailure retains a protected in-process cause while rendering
// only stable path-free identifiers.
type LeaseNamespaceFailure struct {
	FailureID string
	ReasonID  AdoptLeaseReasonID
	Action    AdoptLeaseAction
	cause     error
}

func (e *LeaseNamespaceFailure) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s reason=%s action=%s", e.FailureID, e.ReasonID, e.Action)
}

func (e *LeaseNamespaceFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func newLeaseNamespaceOperationFailure(reason AdoptLeaseReasonID, action AdoptLeaseAction, cause error) error {
	return &LeaseNamespaceFailure{
		FailureID: adoptLeaseFailureNamespaceRefused,
		ReasonID:  reason,
		Action:    action,
		cause:     cause,
	}
}

func InspectAdoptLeaseNamespace() (AdoptLeaseNamespaceReport, error) {
	return inspectAdoptLeaseNamespacePlatform()
}

func MigrateLegacyAdoptLeaseNamespace(opts AdoptLeaseNamespaceMigrationOpts) (AdoptLeaseNamespaceReport, error) {
	if !opts.Yes {
		return InspectAdoptLeaseNamespace()
	}
	return migrateLegacyAdoptLeaseNamespacePlatform()
}
