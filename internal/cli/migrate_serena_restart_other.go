//go:build !windows

// Package cli — non-Windows stub for the serena migrate §7.1 supervisor
// REAP-FIRST restart gate.
//
// The supervisor cold-restart IPC + spawn primitives (IPC quiesce-timers /
// exit{graceful}, force-kill fallback, detached supervisor spawn) are
// Windows-only production wiring in v0.5.0 (release scope: Windows GA / Linux
// beta / macOS preview). On other platforms the §7.1 reap is not wired, so a
// spec-bearing serena dynamic-pool migrate cannot guarantee the "no old
// supervisor reads the new runtime_spec intent" property. Rather than silently
// commit an intent that a running old supervisor might ignore, the driver FAILS
// LOUD at the REAP step (the same fail-loud posture §7.1 acceptance criterion 2
// mandates) — and critically, because the reap runs BEFORE the intent write in
// the reap-first ordering, the intent is NEVER written on a platform where the
// reap is unsupported, so legacy stays untouched.
package cli

import (
	"context"
	"fmt"
	"io"
)

// errSupervisorReapUnsupported is returned by defaultMigrateSerenaReap on
// non-Windows builds. The migrate driver wraps it with operator guidance; since
// the reap precedes the intent write, the intent is not committed here.
var errSupervisorReapUnsupported = fmt.Errorf(
	"supervisor reap-before-restart is not wired on this platform (v0.5.0 ships the supervisor " +
		"upgrade/restart flow on Windows only; Linux is beta and macOS preview) — the serena " +
		"dynamic-pool intent was NOT written and legacy serena is untouched; stop any running " +
		"supervisor manually and run the cutover on a Windows host")

// defaultMigrateSerenaReap fails loud on non-Windows: the §7.1 reap flow is
// unavailable here. The reap-first ordering means the spec-bearing intent is
// never written on this path.
func defaultMigrateSerenaReap(_ context.Context, _ io.Writer) error {
	return errSupervisorReapUnsupported
}

// defaultMigrateSerenaStart fails loud on non-Windows: the detached supervisor
// spawn primitive is Windows-only in v0.5.0. In practice the driver only reaches
// the start step after a successful reap, which cannot happen here, so this is
// the recovery-path guard for completeness.
func defaultMigrateSerenaStart(_ context.Context, _ io.Writer) error {
	return fmt.Errorf(
		"supervisor start is not wired on this platform (v0.5.0 ships the supervisor spawn " +
			"primitive on Windows only; Linux is beta and macOS preview) — run `mcphub supervise` " +
			"from a shell to start the supervisor")
}
