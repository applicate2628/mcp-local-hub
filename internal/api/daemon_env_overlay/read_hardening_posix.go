//go:build !windows

// read_hardening_posix.go — POSIX hardenedOpen using O_NOFOLLOW so
// the kernel refuses to traverse a symlink at the overlay-file leaf.
//
// Spec: docs/superpowers/specs/2026-05-19-servers-matrix-lsp-and-env-revamp-design.md
// §"Read-side hardening" (B-V4-1).
//
// Behavior:
//   - O_NOFOLLOW makes open() return ELOOP if path refers to a
//     symlink. Targets reached by following symlinks in PARENT
//     components are still permitted; only the final-component
//     symlink is refused. That matches the threat model — the
//     overlay file lives under <state-dir> which the operator
//     controls; the failure mode this guard exists for is an
//     attacker planting a symlink AT the overlay file's path to
//     redirect the read to attacker-chosen target bytes.
//   - Mode bits are 0 on a read-only open; no file is being
//     created, so they are ignored by the kernel.
//   - The returned *os.File owns the underlying fd and closes it
//     via *os.File.Close(); callers in overlay.go already defer
//     Close on the Load() open.

package daemon_env_overlay

import (
	"os"
	"syscall"
)

func hardenedOpen(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}
