//go:build !windows

package api

func inspectAdoptLeaseNamespacePlatform() (AdoptLeaseNamespaceReport, error) {
	report := AdoptLeaseNamespaceReport{
		State:    AdoptLeaseNamespaceRefused,
		ReasonID: AdoptLeaseReasonPlatformUnsupported,
		Action:   AdoptLeaseActionLeaveUnchanged,
	}
	return report, newLeaseNamespaceOperationFailure(report.ReasonID, report.Action, nil)
}

func migrateLegacyAdoptLeaseNamespacePlatform() (AdoptLeaseNamespaceReport, error) {
	return inspectAdoptLeaseNamespacePlatform()
}
