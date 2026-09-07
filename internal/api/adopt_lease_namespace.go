package api

import "fmt"

type AdoptLeaseNamespaceState string

// AdoptLeaseNamespaceFailureCategory is a bounded, path-free diagnostic for
// state-root admission failures. It refines a stable reason/action pair only;
// it never alters refusal policy or the stable failure/reason/action values.
type AdoptLeaseNamespaceFailureCategory string

const (
	AdoptLeaseNamespaceFailureRootOpen        AdoptLeaseNamespaceFailureCategory = "root-open"
	AdoptLeaseNamespaceFailureRootKindReparse AdoptLeaseNamespaceFailureCategory = "root-kind-reparse"
	AdoptLeaseNamespaceFailureRootSecurity    AdoptLeaseNamespaceFailureCategory = "root-security"
)

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
	FailureID       string
	ReasonID        AdoptLeaseReasonID
	Action          AdoptLeaseAction
	Category        AdoptLeaseNamespaceFailureCategory
	NativeErrorCode uint32
	cause           error
}

func (e *LeaseNamespaceFailure) Error() string {
	if e == nil {
		return ""
	}
	return publicLeaseNamespaceFailureMessage(e.FailureID, e.ReasonID, e.Action, e.Category, e.NativeErrorCode)
}

func (e *LeaseNamespaceFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func newLeaseNamespaceOperationFailure(reason AdoptLeaseReasonID, action AdoptLeaseAction, cause error) error {
	return newLeaseNamespaceOperationFailureWithDiagnostic(reason, action, "", 0, cause)
}

func newLeaseNamespaceOperationFailureWithDiagnostic(reason AdoptLeaseReasonID, action AdoptLeaseAction, category AdoptLeaseNamespaceFailureCategory, nativeErrorCode uint32, cause error) error {
	return &LeaseNamespaceFailure{
		FailureID:       adoptLeaseFailureNamespaceRefused,
		ReasonID:        reason,
		Action:          action,
		Category:        category,
		NativeErrorCode: nativeErrorCode,
		cause:           cause,
	}
}

func publicLeaseNamespaceFailureMessage(failureID string, reason AdoptLeaseReasonID, action AdoptLeaseAction, category AdoptLeaseNamespaceFailureCategory, nativeErrorCode uint32) string {
	message := fmt.Sprintf("%s reason=%s action=%s", failureID, reason, action)
	if !category.isPublic() {
		return message
	}
	message += fmt.Sprintf(" category=%s", category)
	if nativeErrorCode != 0 {
		message += fmt.Sprintf(" native_error_code=%d", nativeErrorCode)
	}
	return message
}

func (category AdoptLeaseNamespaceFailureCategory) isPublic() bool {
	switch category {
	case AdoptLeaseNamespaceFailureRootOpen,
		AdoptLeaseNamespaceFailureRootKindReparse,
		AdoptLeaseNamespaceFailureRootSecurity:
		return true
	default:
		return false
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
