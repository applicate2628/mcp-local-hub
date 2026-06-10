// Package api — supervisor-liveness scheduled-task install surface
// (v0.6 redesign spec §15 P1-b / §5.x Phase 3a + 3b).
//
// The liveness task is the v0.6 owner-death recovery: it drives `mcphub
// supervise --ensure-alive` every ~1 minute; that action probes the
// flock-authoritative SupervisorRunningUnderStateDir and, when no supervisor
// holds the lock, relaunches the owner via the autostart task.
//
// Phase 3a added it ALONGSIDE the still-present v0.4.x watchdog; Phase 3b
// (spec §5 Phase C/D) then DELETED the watchdog engine, leaving the liveness
// task as the sole maintenance-task install surface. This file therefore also
// hosts the canonical-exe / current-user resolvers migrated out of the deleted
// watchdog-side files (it is their surviving consumer) plus the
// RemoveLegacyWatchdogTask cleanup for existing hosts.
package api

import (
	"fmt"
	"os/user"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/scheduler"
)

// canonicalMcphubPathFn is the canonical-mcphub-path resolver used by the
// scheduled-task install Command field. Production: thin adapter over the
// internal canonicalMcphubPath() (install.go). Tests: inject a stub via
// SetTestCanonicalMcphubPathFn (testhooks.go).
//
// Migrated here from watchdog_xml_validator.go when the v0.6 redesign
// (spec §5 Phase D) deleted the watchdog engine — InstallLivenessTask
// (below) is the surviving consumer of both resolvers, so they live with
// the task they install rather than in deleted watchdog-side files.
var canonicalMcphubPathFn = func() (string, error) { return canonicalMcphubPath() }

// currentWindowsUserFn is the current-user resolver used by the
// scheduled-task install principal field. Production:
// defaultCurrentWindowsUser() (strips DOMAIN\\ prefix from
// user.Current().Username). Tests: inject via SetTestCurrentWindowsUserFn.
var currentWindowsUserFn = defaultCurrentWindowsUser

// defaultCurrentWindowsUser returns the bare username of the running
// process, stripping the DOMAIN\\ prefix Windows attaches to
// user.Current().Username.
func defaultCurrentWindowsUser() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("user.Current: %w", err)
	}
	name := u.Username
	if i := strings.LastIndex(name, "\\"); i >= 0 {
		name = name[i+1:]
	}
	return name, nil
}

// LivenessTaskName is the canonical scheduled-task name installed alongside
// the daemon tasks by `mcphub setup` (Phase 3a). Kept as a package-level
// constant so callers reference one literal.
const LivenessTaskName = "\\mcp-local-hub-liveness"

// InstallLivenessTask is the idempotent install of the supervisor-liveness
// scheduled task: resolve the canonical mcphub.exe path + the current Windows
// user, render the liveness XML via scheduler.BuildLivenessXML, and ImportXML
// it under LivenessTaskName.
//
// CLI-level concerns (admin-elevation refusal, audit entry, interactive
// confirm) live in the CLI layer that calls this (runSetupWatchdog rides the
// same state-dir-sanity + elevation gates); this API method is the
// unconditional execution path.
//
// On Linux/macOS scheduler.New() returns "not implemented" so this fails
// loud at the ImportXML call — the liveness task is a Windows-GA capability
// in v0.6.
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
// scheduled task.
//
// Wired into the CLI uninstall path at internal/cli/setup.go's
// runUninstallWatchdog, INSIDE the same last-server partial-uninstall gate
// (shouldRemoveGlobalWatchdog) that authorizes the legacy-watchdog teardown:
// the liveness task is a hub-wide shared maintenance job, so it is removed only
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

// LegacyWatchdogTaskName is the canonical name of the v0.4.x watchdog
// scheduled task. The watchdog ENGINE was deleted in the v0.6 redesign
// (spec §5 Phase D); this literal survives ONLY so existing hosts can
// have the leftover task removed by `mcphub setup` / `mcphub uninstall`.
// Kept here (not in a deleted watchdog-side file) so the one place that
// still needs the literal owns it.
const LegacyWatchdogTaskName = "\\mcp-local-hub-watchdog"

// RemoveLegacyWatchdogTask deletes the leftover v0.4.x
// `\mcp-local-hub-watchdog` scheduled task on existing hosts. The v0.6
// supervisor owns daemon revival via its Job-Object reaper + reconcile
// loop, and owner-death recovery is the liveness task — so the legacy
// watchdog is a no-op vestige that actively fights the supervisor every
// 5 min (it writes "suspicious-xml" warnings against the v0.5.0 task XML
// its v0.4.x validator can no longer parse).
//
// Idempotent + best-effort by contract: scheduler.Delete returns nil for
// an absent task (clean hosts), so callers treat this as non-fatal. Routes
// through the same newScheduler() factory seam as UninstallLivenessTask so
// tests can drive it with a recording fake.
func (a *API) RemoveLegacyWatchdogTask() error {
	sch, err := newScheduler()
	if err != nil {
		return err
	}
	return sch.Delete(LegacyWatchdogTaskName)
}
