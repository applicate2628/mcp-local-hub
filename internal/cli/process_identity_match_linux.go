//go:build linux

package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const linuxDeletedSuffix = " (deleted)"

func pidMatchesMcphub(pid int) bool {
	if pid <= 0 {
		return false
	}
	target, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil {
		return false
	}
	target = normalizeProcExeTarget(target)
	return pathsEqual(target, canonicalMcphubPath())
}

func normalizeProcExeTarget(target string) string {
	// Linux appends " (deleted)" when the underlying inode is unlinked
	// (e.g. binary upgrade replaces the exe while the daemon is alive).
	// The remaining path bytes are still the original install location.
	target = strings.TrimSuffix(target, linuxDeletedSuffix)
	// Resolve symlinks to handle ${install-dir}/mcphub -> realpath cases.
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}
	return target
}
