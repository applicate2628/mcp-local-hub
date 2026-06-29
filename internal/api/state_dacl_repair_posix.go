//go:build !windows

package api

import (
	"fmt"
	"os"
	"path/filepath"
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
	parentPath := filepath.Dir(path)
	base := filepath.Base(path)
	pfd, err := unix.Open(parentPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		report := repairReportFromError(path, err)
		return report, fmt.Errorf("open parent %s: %w", parentPath, err)
	}
	defer unix.Close(pfd)

	fd, err := unix.Openat(pfd, base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		report := repairReportFromError(path, err)
		return report, fmt.Errorf("open %s for mode repair: %w", path, err)
	}
	defer unix.Close(fd)

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		report := repairReportFromError(path, err)
		return report, fmt.Errorf("fstat %s: %w", path, err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		report := repairReportFromError(path, ErrIrregularFile)
		return report, ErrIrregularFile
	}
	if int(st.Uid) != os.Getuid() {
		err := fmt.Errorf("%w: path=%s uid=%d (need current uid %d)", ErrWrongOwner, path, st.Uid, os.Getuid())
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
	if err := unix.Fchmod(fd, 0o600); err != nil {
		report := repairReportFromError(path, err)
		return report, fmt.Errorf("chmod %s to 0600: %w", path, err)
	}
	if err := verifyPosixOwnerAndModeFromFd(
		fd,
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
