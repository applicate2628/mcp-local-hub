//go:build windows

package api

import (
	"errors"
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"
)

const (
	stateFileDACLRepairStrongAccess = uint32(
		windows.FILE_WRITE_DATA | windows.DELETE |
			windows.WRITE_DAC | windows.READ_CONTROL | windows.FILE_READ_ATTRIBUTES,
	)
	stateFileDACLRepairMetadataOnlyAccess = uint32(windows.WRITE_DAC | windows.READ_CONTROL | windows.FILE_READ_ATTRIBUTES)
	stateFileDACLRepairFallbackPath       = "tier1-access-denied-metadata-only"
)

type windowsStateFileDACLRepairOpen struct {
	handle                   windows.Handle
	writerExclusionGuarantee StateFileDACLWriterExclusionGuarantee
	openTier                 StateFileDACLRepairOpenTier
	fallbackPath             string
}

func inspectStateFileDACLForRepair(path string) (StateFileDACLRepairCandidate, bool, error) {
	pathW, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return StateFileDACLRepairCandidate{}, false, fmt.Errorf("utf16 %q: %w", path, err)
	}
	h, err := windows.CreateFile(
		pathW,
		windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return StateFileDACLRepairCandidate{}, false, fmt.Errorf("open %s: %w", path, err)
	}
	defer windows.CloseHandle(h)

	if err := refuseIrregularWindowsStateFileHandle(h, path); err != nil {
		return repairCandidateFromError(path, err), true, nil
	}
	currentOwner, err := stateFileOwnerIsCurrentUser(h)
	if err != nil {
		return StateFileDACLRepairCandidate{}, false, err
	}
	if !currentOwner {
		err := fmt.Errorf("%w: path=%s owner is not the current process user", ErrWrongOwner, path)
		return repairCandidateFromError(path, err), true, nil
	}
	if err := verifyWindowsDACLFromHandle(h); err != nil {
		return repairCandidateFromError(path, err), true, nil
	}
	return StateFileDACLRepairCandidate{}, false, nil
}

func repairStateFileDACL(path string) (StateFileDACLRepairReport, error) {
	parentDir := filepath.Dir(path)
	base := filepath.Base(path)
	parentHandle, err := openDirHandleNoReparse(parentDir)
	if err != nil {
		return repairReportFromError(path, err), fmt.Errorf("open parent %s: %w", parentDir, err)
	}
	defer windows.CloseHandle(parentHandle)
	if err := verifyWindowsRepairParentHandle(parentHandle, parentDir); err != nil {
		return repairReportFromError(path, err), err
	}

	repairOpen, err := openWindowsStateFileForDACLRepair(parentHandle, base, path)
	if err != nil {
		return repairReportFromError(path, err), err
	}
	fileHandle := repairOpen.handle
	defer windows.CloseHandle(fileHandle)

	if err := refuseIrregularWindowsStateFileHandle(fileHandle, path); err != nil {
		report := repairReportFromError(path, err)
		return report, err
	}
	currentOwner, err := stateFileOwnerIsCurrentUser(fileHandle)
	if err != nil {
		report := repairReportFromError(path, err)
		return report, err
	}
	if !currentOwner {
		err := fmt.Errorf("%w: path=%s owner is not the current process user", ErrWrongOwner, path)
		report := repairReportFromError(path, err)
		return report, err
	}

	var removedSIDs []string
	if err := verifyWindowsDACLFromHandle(fileHandle); err == nil {
		return StateFileDACLRepairReport{
			Path:                     path,
			Status:                   StateFileDACLRepairStatusUnchanged,
			Reason:                   "file DACL already matches the owner-only allowlist",
			WriterExclusionGuarantee: repairOpen.writerExclusionGuarantee,
			RepairOpenTier:           repairOpen.openTier,
			FallbackPath:             repairOpen.fallbackPath,
		}, nil
	} else if sid := stateFileDACLOffendingSID(err); sid != "" {
		removedSIDs = []string{sid}
	}

	if err := setRestrictiveDACL(fileHandle); err != nil {
		report := repairReportFromError(path, err)
		return report, fmt.Errorf("set restrictive DACL on %s: %w", path, err)
	}
	if err := verifyWindowsDACLFromHandle(fileHandle); err != nil {
		report := repairReportFromError(path, err)
		return report, fmt.Errorf("verify repaired DACL on %s: %w", path, err)
	}
	return StateFileDACLRepairReport{
		Path:                     path,
		Status:                   StateFileDACLRepairStatusRepaired,
		Reason:                   "file DACL tightened to owner-only allowlist",
		RemovedSIDs:              removedSIDs,
		WriterExclusionGuarantee: repairOpen.writerExclusionGuarantee,
		RepairOpenTier:           repairOpen.openTier,
		FallbackPath:             repairOpen.fallbackPath,
	}, nil
}

func openWindowsStateFileForDACLRepair(parentHandle windows.Handle, base, path string) (windowsStateFileDACLRepairOpen, error) {
	fileHandle, err := ntOpenRelativeWithShareAccess(parentHandle, base, stateFileDACLRepairStrongAccess, 0)
	if err == nil {
		return windowsStateFileDACLRepairOpen{
			handle:                   fileHandle,
			writerExclusionGuarantee: StateFileDACLWriterExclusionEnforced,
			openTier:                 StateFileDACLRepairOpenTierStrong,
		}, nil
	}
	if windowsRepairErrIsSharingViolation(err) {
		return windowsStateFileDACLRepairOpen{}, stateFileDACLRepairSharingViolation(path)
	}
	if !windowsRepairErrIsAccessDenied(err) {
		return windowsStateFileDACLRepairOpen{}, fmt.Errorf("open %s for DACL repair: %w", path, err)
	}

	fileHandle, err = ntOpenRelativeWithShareAccess(parentHandle, base, stateFileDACLRepairMetadataOnlyAccess, 0)
	if err != nil {
		if windowsRepairErrIsSharingViolation(err) {
			return windowsStateFileDACLRepairOpen{}, stateFileDACLRepairSharingViolation(path)
		}
		return windowsStateFileDACLRepairOpen{}, fmt.Errorf("open %s for DACL repair metadata-only fallback: %w", path, err)
	}
	return windowsStateFileDACLRepairOpen{
		handle:                   fileHandle,
		writerExclusionGuarantee: StateFileDACLWriterExclusionBestEffort,
		openTier:                 StateFileDACLRepairOpenTierMetadataOnlyFallback,
		fallbackPath:             stateFileDACLRepairFallbackPath,
	}, nil
}

func stateFileDACLRepairSharingViolation(path string) error {
	return fmt.Errorf("%w: a process currently holds %s open; stop it and re-run", ErrStateFileDACLSharingViolation, path)
}

func stateFileOwnerIsCurrentUser(h windows.Handle) (bool, error) {
	sd, err := windows.GetSecurityInfo(
		h,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
	)
	if err != nil {
		return false, fmt.Errorf("get security owner: %w", err)
	}
	ownerSID, _, err := sd.Owner()
	if err != nil {
		return false, fmt.Errorf("get owner: %w", err)
	}
	currentSID, err := currentUserSID()
	if err != nil {
		return false, err
	}
	return ownerSID != nil && ownerSID.Equals(currentSID), nil
}

func refuseIrregularWindowsStateFileHandle(h windows.Handle, path string) error {
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return fmt.Errorf("file info %s: %w", path, err)
	}
	if fi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return ErrIrregularFile
	}
	if fi.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return ErrIrregularFile
	}
	return nil
}

func verifyWindowsRepairParentHandle(h windows.Handle, path string) error {
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return fmt.Errorf("parent info %s: %w", path, err)
	}
	if fi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("%w: parent %s is a reparse point", ErrIrregularFile, path)
	}
	if fi.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return fmt.Errorf("%w: parent %s is not a directory", ErrIrregularFile, path)
	}
	return nil
}

func windowsRepairErrIsSharingViolation(err error) bool {
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return true
	}
	return ntStatusIs(err, windows.STATUS_SHARING_VIOLATION)
}

func windowsRepairErrIsAccessDenied(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || ntStatusIs(err, windows.STATUS_ACCESS_DENIED)
}
