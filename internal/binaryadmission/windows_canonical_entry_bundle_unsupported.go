//go:build !windows

package binaryadmission

import "context"

func AcquireDispatchWindowsCanonicalEntrySetV2(context.Context, string) (*WindowsCanonicalEntryDispatchSetV2, error) {
	return nil, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryDispatchSetV2) DispatchSnapshot() (WindowsCanonicalEntryDispatchSnapshotV2, error) {
	return WindowsCanonicalEntryDispatchSnapshotV2{}, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryDispatchSetV2) VerifyUnchanged() error {
	return errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryDispatchSetV2) VerifyOriginalProductProcessHandle(uintptr) error {
	return errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryDispatchSetV2) Close() error { return nil }
