//go:build !windows

package binaryadmission

import "context"

func (*StagedWindowsCanonicalEntrySetV1) EnsureOriginalPayloadDirectory(context.Context) (*WindowsCanonicalEntryPayloadDirectoryV1, error) {
	return nil, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryPriorSetV1) ObservePayloadParentAbsent(context.Context) (bool, error) {
	return false, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryPayloadDirectoryV1) Snapshot() (WindowsCanonicalEntryPayloadDirectorySnapshotV1, error) {
	return WindowsCanonicalEntryPayloadDirectorySnapshotV1{}, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryPayloadDirectoryV1) DisposeOriginalIfEmpty(context.Context) (WindowsCanonicalEntryMoveResultV1, error) {
	return WindowsCanonicalEntryMoveResultV1{Error: errWindowsCanonicalEntryUnsupported}, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryPayloadDirectoryV1) Close() error { return nil }
func AcquireWindowsCanonicalEntryPayloadDirectoryForRecoveryV1(context.Context, WindowsCanonicalEntryPayloadDirectoryRecoverySourceV1) (*WindowsCanonicalEntryPayloadDirectoryV1, error) {
	return nil, errWindowsCanonicalEntryUnsupported
}
func (*StagedWindowsCanonicalEntrySetV1) ReadOriginal(string) (bool, []byte, string, error) {
	return false, nil, "", errWindowsCanonicalEntryUnsupported
}
func ObserveWindowsCanonicalEntryPayloadDirectoryAbsenceV1(context.Context, WindowsCanonicalEntryPayloadDirectoryAbsenceSourceV1) (WindowsCanonicalEntryPayloadDirectoryAbsenceObservationV1, error) {
	return WindowsCanonicalEntryPayloadDirectoryAbsenceObservationV1{}, errWindowsCanonicalEntryUnsupported
}
