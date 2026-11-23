//go:build !windows

package binaryadmission

import "context"

func (*WindowsCanonicalEntrySetV2) Stage(context.Context, string) (*StagedWindowsCanonicalEntrySetV2, error) {
	return nil, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntrySetV2) StageRetainingEqualRuntime(context.Context, string) (*StagedWindowsCanonicalEntrySetV2, *WindowsCanonicalEntryPriorSetV2, error) {
	return nil, nil, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntrySetV2) StageForUpgrade(context.Context, string) (*StagedWindowsCanonicalEntrySetV2, *WindowsCanonicalEntryPriorSetV2, error) {
	return nil, nil, errWindowsCanonicalEntryUnsupported
}
func (*StagedWindowsCanonicalEntrySetV2) RetainedMembersWitness(context.Context) (WindowsCanonicalEntryRetainedMembersV2, error) {
	return WindowsCanonicalEntryRetainedMembersV2{}, errWindowsCanonicalEntryUnsupported
}
func (*StagedWindowsCanonicalEntrySetV2) Snapshot() (WindowsCanonicalEntrySetSnapshotV2, error) {
	return WindowsCanonicalEntrySetSnapshotV2{}, errWindowsCanonicalEntryUnsupported
}
func (*StagedWindowsCanonicalEntrySetV2) DestinationTargets() (WindowsCanonicalEntrySetTargetsV2, error) {
	return WindowsCanonicalEntrySetTargetsV2{}, errWindowsCanonicalEntryUnsupported
}
func (*StagedWindowsCanonicalEntrySetV2) Members() ([]WindowsCanonicalEntryMemberEvidenceV2, error) {
	return nil, errWindowsCanonicalEntryUnsupported
}
func (*StagedWindowsCanonicalEntrySetV2) AcquireOriginalPriorSet(context.Context) (*WindowsCanonicalEntryPriorSetV2, error) {
	return nil, errWindowsCanonicalEntryUnsupported
}
func (*StagedWindowsCanonicalEntrySetV2) EnsureOriginalMemberDirectories(context.Context) ([]*WindowsCanonicalEntryPayloadDirectoryV1, error) {
	return nil, errWindowsCanonicalEntryUnsupported
}
func (*StagedWindowsCanonicalEntrySetV2) VerifyUnchanged(context.Context) error {
	return errWindowsCanonicalEntryUnsupported
}
func (*StagedWindowsCanonicalEntrySetV2) ObserveOriginalMember(context.Context, string) (WindowsCanonicalEntryMemberEvidenceV2, error) {
	return WindowsCanonicalEntryMemberEvidenceV2{}, errWindowsCanonicalEntryUnsupported
}
func (*StagedWindowsCanonicalEntrySetV2) TakeForTransaction(context.Context, string) (WindowsCanonicalEntrySetSnapshotV2, error) {
	return WindowsCanonicalEntrySetSnapshotV2{}, errWindowsCanonicalEntryUnsupported
}
func (*StagedWindowsCanonicalEntrySetV2) TakeRetainingForTransaction(context.Context, string, WindowsCanonicalEntryRetainedMembersV2) (WindowsCanonicalEntrySetSnapshotV2, error) {
	return WindowsCanonicalEntrySetSnapshotV2{}, errWindowsCanonicalEntryUnsupported
}
func (*StagedWindowsCanonicalEntrySetV2) MoveOriginal(context.Context, string, string) (WindowsCanonicalEntryMoveResultV1, error) {
	return WindowsCanonicalEntryMoveResultV1{}, errWindowsCanonicalEntryUnsupported
}
func (*StagedWindowsCanonicalEntrySetV2) DiscardOriginal(context.Context, string, string) (WindowsCanonicalEntryMoveResultV1, error) {
	return WindowsCanonicalEntryMoveResultV1{}, errWindowsCanonicalEntryUnsupported
}
func (*StagedWindowsCanonicalEntrySetV2) Disposition() WindowsCanonicalEntryMemberDispositionV2 {
	return WindowsCanonicalEntryMemberDispositionV2{}
}
func (*StagedWindowsCanonicalEntrySetV2) CloseMembers() error { return nil }
func (*StagedWindowsCanonicalEntrySetV2) Close() error        { return nil }
func (*WindowsCanonicalEntryPriorSetV2) Observation(context.Context) (WindowsCanonicalEntryPriorObservationV2, error) {
	return WindowsCanonicalEntryPriorObservationV2{}, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryPriorSetV2) VerifyUnchanged(context.Context) error {
	return errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryPriorSetV2) ObserveOriginalMember(context.Context, string) (WindowsCanonicalEntryMemberEvidenceV2, error) {
	return WindowsCanonicalEntryMemberEvidenceV2{}, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryPriorSetV2) RuntimeProduct(context.Context) (*WindowsCanonicalEntryPriorRuntimeV1, error) {
	return nil, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryPriorSetV2) MoveOriginal(context.Context, string, string) (WindowsCanonicalEntryMoveResultV1, error) {
	return WindowsCanonicalEntryMoveResultV1{}, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryPriorSetV2) DiscardOriginal(context.Context, string, string) (WindowsCanonicalEntryMoveResultV1, error) {
	return WindowsCanonicalEntryMoveResultV1{}, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryPriorSetV2) Disposition() WindowsCanonicalEntryMemberDispositionV2 {
	return WindowsCanonicalEntryMemberDispositionV2{}
}
func (*WindowsCanonicalEntryPriorSetV2) Close() error { return nil }
func (*WindowsCanonicalEntryPayloadDirectoryV1) SnapshotForEntryMembersV2() (WindowsCanonicalEntryMemberDirectorySnapshotV2, error) {
	return WindowsCanonicalEntryMemberDirectorySnapshotV2{}, errWindowsCanonicalEntryUnsupported
}
func AcquireWindowsCanonicalEntryForRecoveryV2(context.Context, WindowsCanonicalRecoverySourceV2, WindowsCanonicalRecoverySideV1) (*WindowsCanonicalEntryRecoverySetV2, error) {
	return nil, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryRecoverySetV2) Members(context.Context) ([]WindowsCanonicalEntryMemberEvidenceV2, error) {
	return nil, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryRecoverySetV2) ObserveCleanupMember(context.Context, WindowsCanonicalRecoverySideV1, WindowsCanonicalEntryRecoveryPurposeV2, WindowsCanonicalEntryMemberEvidenceV2) (WindowsCanonicalEntryMemberEvidenceV2, bool, error) {
	return WindowsCanonicalEntryMemberEvidenceV2{}, false, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryRecoverySetV2) VerifyCleanupAbsences(context.Context, WindowsCanonicalRecoverySideV1, WindowsCanonicalEntryRecoveryPurposeV2) error {
	return errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryRecoverySetV2) Directories() []*WindowsCanonicalEntryPayloadDirectoryV1 {
	return nil
}
func (*WindowsCanonicalEntryRecoverySetV2) VerifyUnchanged(context.Context) error {
	return errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryRecoverySetV2) RuntimeProduct(context.Context) (*WindowsCanonicalEntryPriorRuntimeV1, error) {
	return nil, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryRecoverySetV2) MoveOriginal(context.Context, string, string) (WindowsCanonicalEntryMoveResultV1, error) {
	return WindowsCanonicalEntryMoveResultV1{}, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryRecoverySetV2) DiscardOriginal(context.Context, string, string) (WindowsCanonicalEntryMoveResultV1, error) {
	return WindowsCanonicalEntryMoveResultV1{}, errWindowsCanonicalEntryUnsupported
}
func (*WindowsCanonicalEntryRecoverySetV2) Disposition() WindowsCanonicalEntryMemberDispositionV2 {
	return WindowsCanonicalEntryMemberDispositionV2{}
}
func (*WindowsCanonicalEntryRecoverySetV2) Close() error { return nil }

func (*StagedWindowsCanonicalEntrySetV2) Directories() []*WindowsCanonicalEntryPayloadDirectoryV1 {
	return nil
}
func (*WindowsCanonicalEntryRecoverySetV2) IssuedImagePaths(context.Context) ([]string, error) {
	return nil, errWindowsCanonicalEntryUnsupported
}

func (*StagedWindowsCanonicalEntrySetV2) SuccessorRuntimeProduct(context.Context) (*WindowsCanonicalEntryRuntimeV1, error) {
	return nil, errWindowsCanonicalEntryUnsupported
}

func (*StagedWindowsCanonicalEntrySetV2) SuccessorRuntimeProductWithStagedLauncher(context.Context) (*WindowsCanonicalEntryRuntimeV1, error) {
	return nil, errWindowsCanonicalEntryUnsupported
}

func (*WindowsCanonicalEntryPriorSetV2) ReadOriginal(context.Context, string) (bool, []byte, string, error) {
	return false, nil, "", errWindowsCanonicalEntryUnsupported
}

func (*StagedWindowsCanonicalEntrySetV2) PublishedSnapshot(context.Context) (WindowsCanonicalEntrySetSnapshotV2, error) {
	return WindowsCanonicalEntrySetSnapshotV2{}, errWindowsCanonicalEntryUnsupported
}
