//go:build !windows

package binaryadmission

import "context"

func AdmitWindowsCanonicalEntrySetV2(context.Context, string, WindowsCanonicalEntryBuildBindingV1) (*WindowsCanonicalEntrySetV2, error) {
	return nil, errWindowsCanonicalEntryUnsupported
}
func AcquireExecutingWindowsCanonicalEntrySetV2(context.Context, WindowsCanonicalEntryBuildBindingV1) (*WindowsCanonicalEntrySetV2, error) {
	return nil, errWindowsCanonicalEntryUnsupported
}
func AcquireWindowsCanonicalEntrySetV2(context.Context, WindowsCanonicalEntrySetTargetsV2, WindowsCanonicalEntrySetSnapshotV2) (*WindowsCanonicalEntrySetV2, error) {
	return nil, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntrySetV2) Snapshot() (WindowsCanonicalEntrySetSnapshotV2, error) {
	return WindowsCanonicalEntrySetSnapshotV2{}, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntrySetV2) BundleDirectorySnapshot() (WindowsCanonicalEntryPayloadDirectorySnapshotV1, error) {
	return WindowsCanonicalEntryPayloadDirectorySnapshotV1{}, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntrySetV2) VerifyUnchanged() error {
	return errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntrySetV2) VerifyOriginalProductProcessHandle(uintptr) error {
	return errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntrySetV2) Close() error { return nil }
