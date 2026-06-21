//go:build !windows

// hub_mcp_state_read_inode_posix.go — inode-anchored secure read
// for hub-mcp state files on POSIX, mirror of the Windows leg in
// hub_mcp_state_read_inode_windows.go.
//
// POSIX openat with O_NOFOLLOW gives us an inode-bound fd. This file
// keeps verify + read on that single fd so the contract matches the
// Windows leg and no path-based read occurs after verification.

package api

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// readStateFileInodeAnchored opens the file via openat against
// the verified parent fd, runs the owner/mode allowlist gate, and reads
// the content via the same fd. No path-based read between verify and read.
//
// The byte cap is resolved from the state-file kind so intent/vault reads do
// not inherit the small hub-state ceiling.
func readStateFileInodeAnchored(path string) ([]byte, error) {
	return readStateFileInodeAnchoredWithStrictPolicy(path, operatorRequiresSingleUserHome)
}

func readStateFileInodeAnchoredWithStrictPolicy(path string, requiresStrict func() bool) ([]byte, error) {
	return readStateFileInodeAnchoredWithOptions(path, requiresStrict, stateFileReadCapBytes(path), true)
}

func readStateFileInodeAnchoredWithStrictPolicyNoAudit(path string, requiresStrict func() bool) ([]byte, error) {
	return readStateFileInodeAnchoredWithOptions(path, requiresStrict, stateFileReadCapBytes(path), false)
}

func readStateFileInodeAnchoredWithOptions(path string, requiresStrict func() bool, maxBytes int64, auditFallbacks bool) ([]byte, error) {
	parentPath := filepath.Dir(path)
	basename := filepath.Base(path)

	pfd, err := unix.Open(parentPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, &os.PathError{Op: "open", Path: parentPath, Err: os.ErrNotExist}
		}
		return nil, fmt.Errorf("open parent %s: %w", parentPath, err)
	}
	defer unix.Close(pfd)

	var pst unix.Stat_t
	if err := unix.Fstat(pfd, &pst); err != nil {
		return nil, fmt.Errorf("fstat parent %s: %w", parentPath, err)
	}
	pmode := uint32(pst.Mode)
	if int(pst.Uid) != os.Getuid() {
		parentErr := fmt.Errorf("%w: parent=%s uid=%d (need current uid %d)", ErrWrongOwner, parentPath, pst.Uid, os.Getuid())
		if !stateFileParentGateAllowsDefaultRelax(parentErr) {
			return nil, parentErr
		}
	}
	if pmode&0o022 != 0 {
		parentErr := fmt.Errorf("%w: parent=%s mode=%04o grants group/world write", ErrTooLoose, parentPath, pmode&0o777)
		// Write/DAC-edit broadening on parent — safe under the
		// inode-anchored read (the openat below pins the fd to the
		// verified inode regardless of subsequent directory-entry
		// changes; an attacker who replaces the file via a writable
		// parent does not change the inode our fd points to).
		// Strict mode (MCPHUB_REQUIRE_SINGLE_USER_HOME=1) still
		// refuses any parent broadening because the underlying mode
		// diverges from the single-user invariant strict mode
		// promises.
		if requiresStrict() {
			return nil, fmt.Errorf("%w: parent=%s mode=%04o grants group/world write; %s=1 enforces refusal", ErrTooLoose, parentPath, pmode&0o777, RequireSingleUserHomeEnv)
		}
		if !stateFileParentGateAllowsDefaultRelax(parentErr) {
			return nil, parentErr
		}
		if auditFallbacks {
			_ = LogHubMcpEvent("warn", "hub-mcp-state-read-unhardened-parent-fallback", map[string]any{
				"path":        path,
				"parent":      parentPath,
				"reason":      "default-relax-on-solo-host (parent grants group/world write; safe under inode-anchored read because subsequent read(2) is bound to the openat fd, not the path)",
				"parent_mode": fmt.Sprintf("%04o", pmode&0o777),
			})
		}
	}
	if pmode&0o055 != 0 {
		parentErr := fmt.Errorf("%w: parent=%s mode=%04o exposes read/exec bits to group/world", ErrTooLoose, parentPath, pmode&0o777)
		if requiresStrict() {
			return nil, parentErr
		}
		if !stateFileParentGateAllowsDefaultRelax(parentErr) {
			return nil, parentErr
		}
		if auditFallbacks {
			_ = LogHubMcpEvent("warn", "hub-mcp-state-read-unhardened-parent-fallback", map[string]any{
				"path":        path,
				"parent":      parentPath,
				"reason":      "default-relax-on-solo-host (parent group/world read/exec bits set; write bits cleared)",
				"parent_mode": fmt.Sprintf("%04o", pmode&0o777),
			})
		}
	}

	fd, err := unix.Openat(pfd, basename, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
		}
		if errors.Is(err, syscall.ELOOP) {
			return nil, ErrIrregularFile
		}
		return nil, err
	}
	defer unix.Close(fd)

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, fmt.Errorf("hub-mcp state verify: fstat: %w", err)
	}
	mode := os.FileMode(st.Mode & 0o777)
	switch st.Mode & syscall.S_IFMT {
	case syscall.S_IFLNK:
		return nil, ErrIrregularFile
	case syscall.S_IFREG:
		// regular file — proceed
	default:
		return nil, ErrIrregularFile
	}
	if int(st.Uid) != os.Getuid() {
		return nil, fmt.Errorf("%w: path=%s uid=%d (need current uid %d)", ErrWrongOwner, path, st.Uid, os.Getuid())
	}
	if mode&0o022 != 0 {
		return nil, fmt.Errorf("%w: path=%s mode=%04o grants group/world write.%s", ErrTooLoose, path, mode, stateFileReadRemediation(path))
	}
	if mode&0o055 != 0 {
		if requiresStrict() {
			return nil, fmt.Errorf("%w: path=%s mode=%04o exposes read/exec bits to group/world.%s", ErrTooLoose, path, mode, stateFileReadRemediation(path))
		}
		if isSecretBearingStateFilePath(path) {
			return nil, fmt.Errorf("%w: path=%s mode=%04o exposes read/exec bits to group/world on secret-bearing state file.%s", ErrTooLoose, path, mode, stateFileReadRemediation(path))
		}
		if auditFallbacks {
			_ = LogHubMcpEvent("warn", "hub-mcp-state-read-unhardened-file-fallback", map[string]any{
				"path":      path,
				"parent":    parentPath,
				"reason":    "default-relax-on-solo-host (file group/world read/exec bits set; write bits cleared)",
				"file_mode": fmt.Sprintf("%04o", mode),
			})
		}
	}

	// Read via the verified fd. unix.Read in a loop until EOF or
	// the cap fires. Pre-allocate based on Stat_t.Size.
	if st.Size > maxBytes {
		return nil, fmt.Errorf("hub-mcp state read %s: file size %d exceeds cap %d (OOM-protection)", path, st.Size, maxBytes)
	}
	if st.Size < 0 {
		return nil, fmt.Errorf("hub-mcp state read %s: invalid file size %d", path, st.Size)
	}
	buf := make([]byte, 0, st.Size)
	chunk := make([]byte, 4096)
	for {
		n, err := unix.Read(fd, chunk)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		if n == 0 {
			break
		}
		buf = append(buf, chunk[:n]...)
		if int64(len(buf)) > maxBytes {
			return nil, fmt.Errorf("hub-mcp state read %s: content exceeds cap %d (OOM-protection)", path, maxBytes)
		}
	}
	if buf == nil {
		buf = []byte{}
	}
	return buf, nil
}

func stateFileReadRemediation(path string) string {
	return fmt.Sprintf(" Remediate: chmod 600 \"%s\" && chown $USER \"%s\" (and tighten the parent dir).", path, path)
}
