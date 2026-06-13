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
	"errors"
	"fmt"
	"strings"
	"time"
)

// stopForceKillPIDFn is the test seam for the PID-kill fallback used when a
// supervisor-owned descriptor carries no port (e.g. a degenerate or
// not-yet-bound row) but the IPC status reports a live PID. Production uses the
// strongest tree primitive per platform: Windows goes through the Lane A
// taskkill /T primitive; POSIX goes through process.TreeKillByPID, which kills
// the process group and falls back to a single PID only when the target is not a
// group leader. Mirrors killByPortFn's seam discipline so the force-kill path is
// exercisable without touching real processes.
var stopForceKillPIDFn = stopForceKillSupervisorPIDTree

// supervisorIPCStatusFn is the test seam for supervisor IPC status reads that
// need the supervisor-owned live PID map. The force path uses it to recover a
// live PID when a descriptor has no port; the serena idle-wake readiness proof
// uses it to bind the woken task to the supervisor-reported child PID before
// trusting the daemon port. Production: DialSupervisorIPCStatus. In the force
// path, an unreachable supervisor (ErrSupervisorIPCUnavailable) is non-fatal
// only after the liveness probe confirms no live supervisor owner remains; a
// live owner with broken IPC would respawn a non-clean descriptor kill.
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
//     primitive, unless IPC is unavailable while the supervisor owner is still
//     live/undeterminable. A live owner receives retryable error rows instead
//     of a force kill because the supervisor reaper can respawn the child. A
//     target with no descriptor port, no port-owner lookup, or no current port
//     listener falls back to a PID kill resolved from the supervisor IPC status.
//     A portless target with no live PID is an error row: force-stop has no kill
//     surface and cannot prove the daemon is gone.
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
	// whose descriptor carries no port. If IPC is unavailable, prove there is no
	// live supervisor owner before falling back to descriptor force-kill.
	pidByTask, reachable, statusErr := supervisorOwnedLivePIDsWithReachabilityErr(ctx)
	if !reachable && errors.Is(statusErr, ErrSupervisorIPCUnavailable) {
		if rows, blocked := supervisorIPCUnavailableRetryRowsForLiveOwner(targets, statusErr); blocked {
			return rows, true, nil
		}
	}

	results := make([]RestartResult, 0, len(targets))
	for _, d := range targets {
		results = append(results, forceKillOneSupervisorTarget(d, pidByTask))
	}
	return results, true, nil
}

// forceKillOneSupervisorTarget kills a single supervisor-owned descriptor.
// A supervisor IPC PID snapshot is preferred when available because it was
// captured from the supervisor-owned task identity. If that PID cannot be
// trusted or cannot be killed, the port classifier still gets a chance to report
// the honest current state. If the PID kill succeeds, the descriptor port is
// never blindly killed: waitPortReleasedAfterPIDKill owns that release/reuse
// table so a dropped descriptor cannot kill a foreign process that re-bound the
// port after our daemon already died.
func forceKillOneSupervisorTarget(d SupervisorDaemon, pidByTask map[string]int) RestartResult {
	var pidKillErr error
	var pidKillContext string
	pidSeen := false
	if pid, ok := pidByTask[strings.TrimPrefix(d.TaskName, `\`)]; ok && pid > 0 {
		pidSeen = true
		if err := requireMcphubPIDImage(pid); err != nil {
			pidKillErr = fmt.Errorf("force kill daemon by pid %d: %w", pid, err)
			pidKillContext = pidKillErr.Error()
		} else if err := stopForceKillPIDFn(pid); err != nil {
			pidKillErr = fmt.Errorf("force kill daemon by pid %d: %w", pid, err)
			pidKillContext = pidKillErr.Error()
			if pidKillAlreadyGoneError(err) {
				if d.Port == 0 {
					return RestartResult{TaskName: d.TaskName}
				}
			} else {
				return RestartResult{TaskName: d.TaskName, Err: pidKillErr.Error()}
			}
		} else {
			pidKillContext = fmt.Sprintf("force kill daemon by pid %d succeeded", pid)
			if d.Port == 0 {
				return RestartResult{TaskName: d.TaskName}
			}
			warnContext, err := waitPortReleasedAfterPIDKill(d.Port, pid, 5*time.Second)
			if err != nil {
				return RestartResult{TaskName: d.TaskName, Err: appendPIDKillContext("wait for port release after pid kill: "+err.Error(), pidKillContext)}
			}
			return RestartResult{TaskName: d.TaskName, Warning: warnContext}
		}
	}
	portKillUnsupported := false
	if d.Port != 0 {
		outcome, err := forceKillByPortFn(d.Port, 5*time.Second)
		if err != nil {
			return RestartResult{TaskName: d.TaskName, Err: appendPIDKillContext("force kill daemon by port: "+err.Error(), pidKillContext)}
		}
		if outcome == portKillKilled {
			return RestartResult{TaskName: d.TaskName}
		}
		portKillUnsupported = outcome == portKillLookupUnavailable
	}
	if portKillUnsupported {
		return RestartResult{TaskName: d.TaskName, Err: appendPIDKillContext("force kill daemon by port: "+errPortKillUnsupported.Error()+"; no usable port-release proof", pidKillContext)}
	}
	if d.Port == 0 && pidKillErr != nil {
		return RestartResult{TaskName: d.TaskName, Err: pidKillErr.Error()}
	}
	if d.Port == 0 && !pidSeen {
		return RestartResult{TaskName: d.TaskName, Err: "no kill surface: portless descriptor with no live supervisor PID; daemon may still be running; retry when the supervisor is reachable"}
	}
	// No targetable port listener and no live PID: the daemon is already not
	// running from the caller's observable kill surfaces, so the force-stop goal
	// already holds. Success row (no Err).
	return RestartResult{TaskName: d.TaskName}
}

// waitPortReleasedAfterPIDKill waits for a descriptor port after the trusted
// supervisor-reported PID was killed successfully.
//
// Decision table for PID kill SUCCEEDED + d.Port != 0:
//   - port becomes unbound before timeout → SUCCESS; the intended daemon is
//     gone and no port owner needs killing.
//   - timeout + same PID still reported → ERROR; that PID was already killed,
//     so this is a process-table/port-lookup race and we must not switch to a
//     blind port kill.
//   - timeout + mcphub.exe listener → KILL that PID's tree through the existing
//     identity-gated taskkill primitive, then require the port to release.
//   - timeout + foreign listener image → SUCCESS with warning context
//     ("port now owned by foreign process <image>; not killing"); the force-stop
//     goal holds because our daemon PID died, and killing the re-user is the
//     unsafe r21 over-correction this path must avoid.
//   - lookup/identity proof unavailable → ERROR; without a release or identity
//     proof, this path cannot honestly claim the descriptor port is safe.
func waitPortReleasedAfterPIDKill(port, killedPID int, timeout time.Duration) (string, error) {
	if port == 0 {
		return "", nil
	}
	if lookupProcess == nil {
		return "", fmt.Errorf("%w; no usable port-release proof", errPortKillUnsupported)
	}

	deadline := time.Now().Add(timeout)
	var pid int
	var ok bool
	for {
		pid, _, _, ok = lookupProcess(port)
		if !ok {
			return "", nil
		}
		if timeout <= 0 || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(daemonPortReleasePollInterval)
	}

	if pid == killedPID {
		return "", fmt.Errorf("port %d still bound to killed pid %d after %s", port, killedPID, timeout)
	}

	image, parentImage, verified, lookupAvailable, lookupOK := mcphubPIDImageVerified(pid)
	if !lookupAvailable {
		return "", fmt.Errorf("port %d still bound after %s; process identity lookup unavailable for pid %d", port, timeout, pid)
	}
	if !lookupOK {
		return "", fmt.Errorf("port %d still bound after %s; process identity lookup failed for pid %d", port, timeout, pid)
	}
	if !verified {
		return fmt.Sprintf("port now owned by foreign process %q; not killing", image), nil
	}

	// A DIFFERENT mcphub PID on the port after our kill is the supervisor's
	// auto-revival of the force-stopped daemon — the documented force-stop
	// semantic (non-clean kill → restart policy respawns), NOT a remnant of
	// the killed tree (an mcphub daemon's children are uvx/python/node
	// wrappers, never another mcphub.exe). The 200ms poll can simply miss the
	// brief unbound interval. Killing here would chase the freshly respawned
	// daemon in a loop (bot PR #288 r30); report success with the revival
	// noted instead.
	return fmt.Sprintf("port re-bound by respawned mcphub pid %d (parent %q) after the force kill; supervisor auto-revival per restart policy", pid, parentImage), nil
}

func appendPIDKillContext(msg string, pidKillContext string) string {
	if pidKillContext == "" {
		return msg
	}
	return msg + "; pid path: " + pidKillContext
}

func pidKillAlreadyGoneError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "already gone"):
		return true
	case strings.Contains(msg, "process") && strings.Contains(msg, "not found"):
		return true
	case strings.Contains(msg, "no such process"):
		return true
	case strings.Contains(msg, "could not find") && strings.Contains(msg, "process"):
		return true
	default:
		return false
	}
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
	pids, reachable, _ := supervisorOwnedLivePIDsWithReachabilityErr(ctx)
	return pids, reachable
}

func supervisorOwnedLivePIDsWithReachabilityErr(ctx context.Context) (map[string]int, bool, error) {
	if supervisorIPCStatusFn == nil {
		return map[string]int{}, false, nil
	}
	rows, err := supervisorIPCStatusFn(ctx)
	if err != nil {
		return map[string]int{}, false, err
	}
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		if r.PID > 0 {
			out[strings.TrimPrefix(r.TaskName, `\`)] = r.PID
		}
	}
	return out, true, nil
}

func supervisorIPCUnavailableRetryRowsForLiveOwner(targets []SupervisorDaemon, ipcErr error) ([]RestartResult, bool) {
	stateDir, err := DaemonStateDir()
	if err != nil {
		return supervisorRetryRows(targets, fmt.Sprintf("supervisor IPC is unreachable and state dir could not be resolved before liveness probe: %v; stop not applied to avoid force-killing under a possibly live reaper; run `mcphub restart`, or kill the wedged process if restart cannot reach it", err)), true
	}
	running, pid, err := installSupervisorRunningProbeFn(stateDir)
	if err != nil {
		return supervisorRetryRows(targets, fmt.Sprintf("supervisor IPC is unreachable and supervisor liveness probe failed: %v; stop not applied to avoid force-killing under a possibly live reaper; run `mcphub restart`, or kill the wedged process if restart cannot reach it", err)), true
	}
	if !running {
		return nil, false
	}
	return supervisorRetryRows(targets, supervisorLiveOwnerIPCUnavailableStopError(pid, ipcErr)), true
}

func supervisorRetryRows(targets []SupervisorDaemon, msg string) []RestartResult {
	results := make([]RestartResult, 0, len(targets))
	for _, d := range targets {
		results = append(results, RestartResult{TaskName: d.TaskName, Err: msg})
	}
	return results
}

func supervisorManualReviewStopError(taskName string) string {
	return fmt.Sprintf("supervisor flagged %s for manual review; stop not applied — inspect supervisor-events.log / mcphub status", taskName)
}

func supervisorUnhandledDriftActionStopError(taskName, action string) string {
	return fmt.Sprintf("supervisor returned unsupported reconcile action %q for %s; stop not applied — inspect supervisor-events.log / mcphub status", action, taskName)
}

func supervisorLiveOwnerIPCUnavailableStopError(pid int, ipcErr error) string {
	pidText := ""
	if pid > 0 {
		pidText = fmt.Sprintf(" pid=%d", pid)
	}
	if ipcErr != nil {
		return fmt.Sprintf("supervisor lock owner%s is alive but IPC is unreachable: %v; stop not applied to avoid force-killing under a live reaper; run `mcphub restart`, or kill the wedged process if restart cannot reach it", pidText, ipcErr)
	}
	return fmt.Sprintf("supervisor lock owner%s is alive but IPC is unreachable; stop not applied to avoid force-killing under a live reaper; run `mcphub restart`, or kill the wedged process if restart cannot reach it", pidText)
}
