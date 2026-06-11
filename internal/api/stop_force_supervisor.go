package api

// Phase F follow-up (bot PR #288 F3) — make `mcphub stop --force` terminate
// SUPERVISOR-OWNED daemons.
//
// The --force branch records NO Desired=stopped intent (its documented
// contract: the daemon auto-revives), so it deliberately skips the
// reconcile-driven stop path the no-force branch uses
// (stopSupervisorOwnedDaemons). It then calls stopKillCore, whose kill
// targets come ONLY from the scheduler List / listTasksForServer. Under
// the v0.6 Phase F model a global daemon has NO scheduler task — it lives
// in supervisor-intent.json — so for such a daemon stopKillCore iterates an
// empty set and kills NOTHING: `mcphub stop --force <global>` recorded the
// forced-stop audit + intent but left the daemon running until the next
// reconcile. This file closes that gap: the force branch also resolves the
// supervisor-owned descriptors in scope and kills them directly, preserving
// the force semantic (a non-clean kill the reaper observes → auto-revive).

import (
	"context"
	"fmt"
	"strings"
	"time"

	"mcp-local-hub/internal/process"
)

// stopForceKillPIDFn is the test seam for the PID-kill fallback used when a
// supervisor-owned descriptor carries no port (e.g. a degenerate or
// not-yet-bound row) but the IPC status reports a live PID. Production:
// process.BestEffortKillByPID. Mirrors killByPortFn's seam discipline so
// the force-kill path is exercisable without touching real processes.
var stopForceKillPIDFn = process.BestEffortKillByPID

// supervisorIPCStatusFn is the test seam for supervisor IPC status reads that
// need the supervisor-owned live PID map. The force path uses it to recover a
// live PID when a descriptor has no port; the serena idle-wake readiness proof
// uses it to bind the woken task to the supervisor-reported child PID before
// trusting the daemon port. Production: DialSupervisorIPCStatus. In the force
// path, an unreachable supervisor (ErrSupervisorIPCUnavailable) is non-fatal —
// the port path already covers the common case.
var supervisorIPCStatusFn = DialSupervisorIPCStatus

// stopForceKillSupervisorOwned terminates the supervisor-owned daemons in
// scope for `mcphub stop --force`. It is the force-branch counterpart to the
// no-force stopSupervisorOwnedDaemons (which drives reconcile-apply): force
// records no stop intent, so a reconcile would read Desired=running and post
// nothing — the only correct force action is a direct kill the reaper sees as
// a non-clean exit, leaving the daemon to auto-revive.
//
// Returns (results, handled, err):
//
//   - No supervisor intent file or no targets in scope → (nil, false, nil):
//     nothing is supervisor-owned here; the legacy stopKillCore owns the
//     stop (it still iterates the scheduler tasks for legacy rows).
//   - Otherwise → one RestartResult row per target with handled=true. Each
//     target is killed by its descriptor Port via the shared port-kill
//     primitive; a target with no descriptor port, no port-owner lookup, or no
//     current port listener falls back to a PID kill resolved from the
//     supervisor IPC status.
//     A target that is already not running (no port bound, no live PID) is a
//     success row — the force-stop goal (daemon not running) already holds.
//
// The returned handled set's task names let the caller skip these rows in
// stopKillCore so a daemon is never double-killed (and a legacy row that
// ALSO has a scheduler task is still reached by stopKillCore for the names
// NOT in this set).
func stopForceKillSupervisorOwned(ctx context.Context, server, daemonFilter string) ([]RestartResult, bool, error) {
	targets, err := loadSupervisorOwnedTargets(server, daemonFilter)
	if err != nil {
		return nil, false, err
	}
	if len(targets) == 0 {
		return nil, false, nil
	}

	// Best-effort IPC status, used only to recover a live PID for a target
	// whose descriptor carries no port. An unreachable supervisor is fine —
	// the port path covers the common case and a down supervisor means
	// nothing will respawn a killed daemon anyway.
	pidByTask := supervisorOwnedLivePIDs(ctx)

	results := make([]RestartResult, 0, len(targets))
	for _, d := range targets {
		results = append(results, forceKillOneSupervisorTarget(d, pidByTask))
	}
	return results, true, nil
}

// forceKillOneSupervisorTarget kills a single supervisor-owned descriptor.
// Port is the primary target (the daemon's listening socket — the same signal
// stopKillCore uses). Unsupported port lookup falls back to a PID resolved from
// the IPC status snapshot; if neither path can target a process, only a
// zero-port descriptor is treated as already-stopped.
func forceKillOneSupervisorTarget(d SupervisorDaemon, pidByTask map[string]int) RestartResult {
	portKillUnsupported := false
	if d.Port != 0 {
		outcome, err := forceKillByPortFn(d.Port, 5*time.Second)
		if err != nil {
			return RestartResult{TaskName: d.TaskName, Err: "force kill daemon by port: " + err.Error()}
		}
		if outcome == portKillKilled {
			return RestartResult{TaskName: d.TaskName}
		}
		portKillUnsupported = outcome == portKillLookupUnavailable
	}
	if pid, ok := pidByTask[strings.TrimPrefix(d.TaskName, `\`)]; ok && pid > 0 {
		if err := stopForceKillPIDFn(pid); err != nil {
			return RestartResult{TaskName: d.TaskName, Err: fmt.Sprintf("force kill daemon by pid %d: %v", pid, err)}
		}
		return RestartResult{TaskName: d.TaskName}
	}
	if portKillUnsupported {
		return RestartResult{TaskName: d.TaskName, Err: "force kill daemon by port: " + errPortKillUnsupported.Error() + "; no live PID fallback"}
	}
	// No targetable port listener and no live PID: the daemon is already not
	// running from the caller's observable kill surfaces, so the force-stop goal
	// already holds. Success row (no Err).
	return RestartResult{TaskName: d.TaskName}
}

// supervisorOwnedLivePIDs returns a (bare-task-name → live PID) map from the
// supervisor IPC status, used to recover a kill target for a port-less
// descriptor. Best-effort: ANY status error — an unreachable supervisor
// (ErrSupervisorIPCUnavailable) or a real transport/decode failure — yields an
// empty map. The descriptor port is the primary kill signal, so a missing PID
// map only loses the rare port-less-descriptor fallback, never the common
// case; we do not pretend we have PIDs we could not read.
func supervisorOwnedLivePIDs(ctx context.Context) map[string]int {
	pids, _ := supervisorOwnedLivePIDsWithReachability(ctx)
	return pids
}

func supervisorOwnedLivePIDsWithReachability(ctx context.Context) (map[string]int, bool) {
	if supervisorIPCStatusFn == nil {
		return map[string]int{}, false
	}
	rows, err := supervisorIPCStatusFn(ctx)
	if err != nil {
		return map[string]int{}, false
	}
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		if r.PID > 0 {
			out[strings.TrimPrefix(r.TaskName, `\`)] = r.PID
		}
	}
	return out, true
}
