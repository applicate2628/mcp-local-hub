//go:build !windows

package binaryadmission

import (
	"context"
	"errors"
)

var errWindowsCanonicalEntryUnsupported = errors.New("original Windows canonical entry native acquisition is unsupported on this host")

func AdmitWindowsCanonicalEntrySetV1(context.Context, string, WindowsCanonicalEntryBuildBindingV1) (*WindowsCanonicalEntrySetV1, error) {
	return nil, errWindowsCanonicalEntryUnsupported
}
func AcquireExecutingWindowsCanonicalEntrySetV1(context.Context, WindowsCanonicalEntryBuildBindingV1) (*WindowsCanonicalEntrySetV1, error) {
	return nil, errWindowsCanonicalEntryUnsupported
}
func AcquireDispatchWindowsCanonicalEntrySetV1(context.Context, string) (*WindowsCanonicalEntrySetV1, error) {
	return nil, errWindowsCanonicalEntryUnsupported
}
func AcquireWindowsCanonicalEntrySetV1(context.Context, WindowsCanonicalEntrySetTargetsV1, WindowsCanonicalEntrySetSnapshotV1) (*WindowsCanonicalEntrySetV1, error) {
	return nil, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntrySetV1) Snapshot() (WindowsCanonicalEntrySetSnapshotV1, error) {
	return WindowsCanonicalEntrySetSnapshotV1{}, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntrySetV1) VerifyUnchanged() error {
	return errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntrySetV1) VerifyOriginalProductProcessHandle(uintptr) error {
	return errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntrySetV1) Stage(context.Context, string) (*StagedWindowsCanonicalEntrySetV1, error) {
	return nil, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntrySetV1) Close() error { return nil }
func (*StagedWindowsCanonicalEntrySetV1) Targets() (WindowsCanonicalEntrySetTargetsV1, error) {
	return WindowsCanonicalEntrySetTargetsV1{}, errWindowsCanonicalEntryUnsupported
}
func (*StagedWindowsCanonicalEntrySetV1) DestinationTargets() (WindowsCanonicalEntrySetTargetsV1, error) {
	return WindowsCanonicalEntrySetTargetsV1{}, errWindowsCanonicalEntryUnsupported
}
func (*StagedWindowsCanonicalEntrySetV1) Snapshot() (WindowsCanonicalEntrySetSnapshotV1, error) {
	return WindowsCanonicalEntrySetSnapshotV1{}, errWindowsCanonicalEntryUnsupported
}
func (*StagedWindowsCanonicalEntrySetV1) VerifyUnchanged() error {
	return errWindowsCanonicalEntryUnsupported
}
func (*StagedWindowsCanonicalEntrySetV1) TakeForTransaction(context.Context, string) (WindowsCanonicalEntrySetSnapshotV1, WindowsCanonicalEntrySetTargetsV1, error) {
	return WindowsCanonicalEntrySetSnapshotV1{}, WindowsCanonicalEntrySetTargetsV1{}, errWindowsCanonicalEntryUnsupported
}
func (*StagedWindowsCanonicalEntrySetV1) Close() error { return nil }
func (*StagedWindowsCanonicalEntrySetV1) MoveOriginal(context.Context, string, string) (WindowsCanonicalEntryMoveResultV1, error) {
	return WindowsCanonicalEntryMoveResultV1{Error: errWindowsCanonicalEntryUnsupported}, errWindowsCanonicalEntryUnsupported
}
func (*StagedWindowsCanonicalEntrySetV1) AcquireOriginalPriorSet(context.Context) (*WindowsCanonicalEntryPriorSetV1, error) {
	return nil, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryPriorSetV1) ReadOriginal(string) (bool, []byte, string, error) {
	return false, nil, "", errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryPriorSetV1) MoveOriginal(context.Context, string, string) (WindowsCanonicalEntryMoveResultV1, error) {
	return WindowsCanonicalEntryMoveResultV1{Error: errWindowsCanonicalEntryUnsupported}, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryPriorSetV1) DiscardOriginal(context.Context, string, string) (WindowsCanonicalEntryMoveResultV1, error) {
	return WindowsCanonicalEntryMoveResultV1{Error: errWindowsCanonicalEntryUnsupported}, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryPriorSetV1) Close() error { return nil }
func (*StagedWindowsCanonicalEntrySetV1) DiscardOriginal(context.Context, string, string) (WindowsCanonicalEntryMoveResultV1, error) {
	return WindowsCanonicalEntryMoveResultV1{Error: errWindowsCanonicalEntryUnsupported}, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryPriorSetV1) RuntimeProduct(context.Context) (*WindowsCanonicalEntryPriorRuntimeV1, error) {
	return nil, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryPriorRuntimeV1) ValidateOriginal(context.Context) error {
	return errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryPriorRuntimeV1) VerifyOriginalProductProcessHandle(uintptr) error {
	return errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryPriorSetV1) validateOriginalRuntime(context.Context, int, bool) error {
	return errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryPriorSetV1) verifyOriginalRuntimeProcess(int, bool, uintptr) error {
	return errWindowsCanonicalEntryUnsupported
}
func AcquireWindowsCanonicalEntryForRecoveryV1(context.Context, WindowsCanonicalRecoverySourceV1, WindowsCanonicalRecoverySideV1) (*WindowsCanonicalEntryRecoverySetV1, error) {
	return nil, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryRecoverySetV1) ReadOriginal(string) (bool, []byte, string, error) {
	return false, nil, "", errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryRecoverySetV1) MoveOriginal(context.Context, string, string) (WindowsCanonicalEntryMoveResultV1, error) {
	return WindowsCanonicalEntryMoveResultV1{Error: errWindowsCanonicalEntryUnsupported}, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryRecoverySetV1) DiscardOriginal(context.Context, string, string) (WindowsCanonicalEntryMoveResultV1, error) {
	return WindowsCanonicalEntryMoveResultV1{Error: errWindowsCanonicalEntryUnsupported}, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryRecoverySetV1) RuntimeProduct(context.Context) (*WindowsCanonicalEntryPriorRuntimeV1, error) {
	return nil, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryRecoverySetV1) validateOriginalRuntime(context.Context, int, bool) error {
	return errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryRecoverySetV1) verifyOriginalRuntimeProcess(int, bool, uintptr) error {
	return errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryRecoverySetV1) Close() error { return nil }
