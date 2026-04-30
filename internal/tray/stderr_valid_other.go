//go:build !windows

package tray

// stderrIsValid: on POSIX, std handles inherited from the parent are
// always valid file descriptors (0/1/2 are reserved by the kernel
// even when redirected to /dev/null). exec.Cmd accepts os.Stderr
// without checks, so the function unconditionally returns true.
// Codex bot review on PR #24 P1 (the failure mode this guards
// against is Windows-specific).
func stderrIsValid() bool { return true }
