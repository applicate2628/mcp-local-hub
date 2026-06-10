// Package api — supervisor-liveness scheduled-task install surface
// (v0.6 redesign spec §15 P1-b / §5.x Phase 3a).
//
// This is the ADDITIVE owner-death recovery added BEFORE Phase C/D delete the
// watchdog. The liveness task drives `mcphub supervise --ensure-alive` every
// ~1 minute; that action probes the flock-authoritative
// SupervisorRunningUnderStateDir and, when no supervisor holds the lock,
// relaunches the owner via the autostart task. The watchdog stays fully
// intact in 3a — the two operate on DISJOINT targets (owner-relaunch vs
// daemon-revival) so they do not fight.
//
// Surface mirrors InstallWatchdogTask / UninstallWatchdogTask
// (api_surfaces.go) exactly: same canonical-exe / current-user resolvers,
// same newScheduler() seam, same scheduler.ImportXML / Delete primitives. It
// is kept in its OWN file so Phase D's watchdog deletion does not have to
// surgically untangle the two install paths.
package api

import (
	"path/filepath"

	"mcp-local-hub/internal/scheduler"
)

// LivenessTaskName is the canonical scheduled-task name installed alongside
// the daemon tasks by `mcphub setup` (Phase 3a). Kept as a package-level
// constant mirroring WatchdogTaskName so callers reference one literal.
const LivenessTaskName = "\\mcp-local-hub-liveness"

// InstallLivenessTask is the idempotent install of the supervisor-liveness
// scheduled task. Mirrors InstallWatchdogTask (api_surfaces.go:754): resolve
// the canonical mcphub.exe path + the current Windows user, render the
// liveness XML via scheduler.BuildLivenessXML, and ImportXML it under
// LivenessTaskName.
//
// CLI-level concerns (admin-elevation refusal, audit entry, interactive
// confirm) live in the CLI layer that calls this (runSetupWatchdog rides the
// same state-dir-sanity + elevation gates the watchdog install already
// passed); this API method is the unconditional execution path.
//
// On Linux/macOS scheduler.New() returns "not implemented" so this fails
// loud at the ImportXML call — the liveness task is a Windows-GA capability
// in v0.6, same posture as the watchdog task it precedes.
func (a *API) InstallLivenessTask() error {
	canonicalExe, err := canonicalMcphubPathFn()
	if err != nil {
		return err
	}
	userName, err := currentWindowsUserFn()
	if err != nil {
		return err
	}
	workingDir := filepath.Dir(canonicalExe)
	xmlDoc := scheduler.BuildLivenessXML(canonicalExe, workingDir, userName)

	sch, err := newScheduler()
	if err != nil {
		return err
	}
	return sch.ImportXML(LivenessTaskName, []byte(xmlDoc))
}

// UninstallLivenessTask is the idempotent removal of the supervisor-liveness
// scheduled task. Mirrors UninstallWatchdogTask (api_surfaces.go:692).
//
// Wired into the CLI uninstall path at internal/cli/setup.go's
// runUninstallWatchdog, INSIDE the same last-server partial-uninstall gate
// (shouldRemoveGlobalWatchdog) that authorizes the watchdog teardown: the
// liveness task is a hub-wide shared maintenance job, so it is removed only
// when the last managed server is uninstalled, never while peer servers
// remain. The call is non-fatal / idempotent there — scheduler.Delete
// returns nil for an absent task.
func (a *API) UninstallLivenessTask() error {
	sch, err := newScheduler()
	if err != nil {
		return err
	}
	return sch.Delete(LivenessTaskName)
}
