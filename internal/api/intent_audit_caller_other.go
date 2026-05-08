//go:build !windows && !linux && !darwin

// intent_audit_caller_other.go — fallback CallerStartTime helper for
// platforms not covered by the per-OS files (windows/linux/darwin).
// Per plan §25 v9: stub returns time.Now().UTC() so the audit-line
// path stays defined; mark as a TODO for any future platform port.

package api

import "time"

// CallerStartTime returns the current UTC time as a placeholder for
// unsupported operating systems. TODO(future-platform-port): wire a
// real per-OS conversion when adding *BSD / Solaris / Plan 9 support.
func CallerStartTime() time.Time {
	return time.Now().UTC()
}
