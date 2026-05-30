//go:build !windows

// Package cli — non-Windows stub for the serena migrate §7.1 supervisor
// upgrade/restart gate.
//
// The supervisor cold-restart IPC + spawn primitives (IPC quiesce-timers /
// exit{graceful}, force-kill fallback, detached supervisor spawn) are
// Windows-only production wiring in v0.5.0 (release scope: Windows GA / Linux
// beta / macOS preview). On other platforms the §7.1 restart gate is not yet
// wired, so a spec-bearing serena dynamic-pool migrate cannot guarantee the
// "no old supervisor reads the new runtime_spec intent" property. Rather than
// silently commit an intent that a running old supervisor might ignore, the
// driver FAILS LOUD via this stub (the same fail-loud posture §7.1 acceptance
// criterion 2 mandates when the prior supervisor cannot be reaped).
package cli

import (
	"context"
	"fmt"
	"io"
)

// errSupervisorRestartUnsupported is returned by defaultMigrateSerenaRestart on
// non-Windows builds. The migrate driver wraps it with operator guidance.
var errSupervisorRestartUnsupported = fmt.Errorf(
	"supervisor cold-restart is not wired on this platform (v0.5.0 ships the supervisor " +
		"upgrade/restart flow on Windows only; Linux is beta and macOS preview) — the serena " +
		"dynamic-pool intent was written, but stop any running supervisor manually so the " +
		"current binary reconciles the new runtime_spec on its next start")

// defaultMigrateSerenaRestart fails loud on non-Windows: the §7.1 cold-restart
// flow is unavailable here.
func defaultMigrateSerenaRestart(_ context.Context, _ io.Writer) error {
	return errSupervisorRestartUnsupported
}
