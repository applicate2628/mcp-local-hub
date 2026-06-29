//go:build !windows

package api

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func inspectStateFileDACLForRepair(path string) (StateFileDACLRepairCandidate, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return StateFileDACLRepairCandidate{}, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return repairCandidateFromError(path, ErrIrregularFile), true, nil
	}
	if uid, ok := statUID(info); ok && uid != os.Getuid() {
		err := fmt.Errorf("%w: path=%s uid=%d (need current uid %d)", ErrWrongOwner, path, uid, os.Getuid())
		return repairCandidateFromError(path, err), true, nil
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		err := fmt.Errorf("%w: path=%s mode=%04o is not 0600", ErrTooLoose, path, mode)
		return repairCandidateFromError(path, err), true, nil
	}
	return StateFileDACLRepairCandidate{}, false, nil
}

func repairStateFileDACL(path string) (StateFileDACLRepairReport, error) {
	stateDirAbs, targetAbs, rel, err := stateFileDACLRepairPathUnderStateDir(path)
	if err != nil {
		report := repairReportFromError(path, err)
		return report, err
	}
	path = targetAbs

	pfd, base, err := openStateFileDACLRepairParentFromStateDirPosix(stateDirAbs, rel)
	if err != nil {
		report := repairReportFromError(path, err)
		return report, err
	}
	defer unix.Close(pfd)

	var st unix.Stat_t
	if err := unix.Fstatat(pfd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		report := repairReportFromError(path, err)
		return report, fmt.Errorf("stat %s for mode repair: %w", path, err)
	}
	if err := validatePosixStateFileRepairInode(path, &st); err != nil {
		report := repairReportFromError(path, err)
		return report, err
	}
	if os.FileMode(st.Mode&0o777) == 0o600 {
		return StateFileDACLRepairReport{
			Path:   path,
			Status: StateFileDACLRepairStatusUnchanged,
			Reason: "file mode already owner-only 0600",
		}, nil
	}
	if err := chmodStateFileAt(pfd, base, 0o600); err != nil {
		report := repairReportFromError(path, err)
		return report, fmt.Errorf("chmod %s to 0600: %w", path, err)
	}
	if err := unix.Fstatat(pfd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		report := repairReportFromError(path, err)
		return report, fmt.Errorf("stat repaired mode on %s: %w", path, err)
	}
	if err := verifyPosixOwnerAndModeFromStat(
		&st,
		func(uid, want int) error {
			return fmt.Errorf("%w: file owned by uid %d, want %d", ErrWrongOwner, uid, want)
		},
		func(mode uint32) error {
			return fmt.Errorf("%w: file mode %#o is group- or other-accessible after repair", ErrTooLoose, mode)
		},
	); err != nil {
		report := repairReportFromError(path, err)
		return report, fmt.Errorf("verify repaired mode on %s: %w", path, err)
	}
	return StateFileDACLRepairReport{
		Path:   path,
		Status: StateFileDACLRepairStatusRepaired,
		Reason: "file mode tightened to 0600",
	}, nil
}

func openStateFileDACLRepairParentFromStateDirPosix(stateDirAbs, rel string) (int, string, error) {
	dirs, base, err := splitStateFileDACLRepairRel(rel)
	if err != nil {
		return -1, "", err
	}
	curFd, err := unix.Open(stateDirAbs, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", fmt.Errorf("open state dir %s for repair: %w", stateDirAbs, err)
	}
	for _, comp := range dirs {
		nextFd, openErr := openExistingRealDirAt(curFd, comp)
		_ = unix.Close(curFd)
		if openErr != nil {
			return -1, "", fmt.Errorf("%w: refuse to descend through non-directory or symlink at component %q of repair path %s: %v", ErrIrregularFile, comp, rel, openErr)
		}
		curFd = nextFd
	}
	return curFd, base, nil
}

func validatePosixStateFileRepairInode(path string, st *unix.Stat_t) error {
	if uint32(st.Mode)&syscall.S_IFMT != syscall.S_IFREG {
		return ErrIrregularFile
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%w: path=%s uid=%d (need current uid %d)", ErrWrongOwner, path, st.Uid, os.Getuid())
	}
	return nil
}
