package cli

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

const (
	supervisorPortProbeTimeout = 300 * time.Millisecond
	supervisorPortBindGrace    = 5 * time.Second
	supervisorLivenessInterval = 5 * time.Second

	supervisorLivenessRuntimeClearedBodyKey = "runtime_pid_cleared"

	supervisorLivenessReasonMissingPID          = "missing_pid"
	supervisorLivenessReasonPIDDead             = "pid_dead"
	supervisorLivenessReasonPIDIdentityMissing  = "pid_identity_missing"
	supervisorLivenessReasonPIDIdentityMismatch = "pid_identity_mismatch"
	supervisorLivenessReasonPortOwnerUnverified = "port_owner_unverified"
	supervisorLivenessReasonPortUnbound         = "port_unbound"
	supervisorLivenessReasonPortOwnerSelf       = "port_owner_self"
	supervisorLivenessReasonPortOwnerMismatch   = "port_owner_mismatch"
)

type supervisorLivenessProbe struct {
	PIDAlive     func(pid int) bool
	PIDIdentity  func(proof process.PIDIdentityProof) error
	PortLive     func(port int) bool
	PortOwnerPID func(port int) (pid int, ok bool, err error)
}

var supervisorLivenessProbeFns = defaultSupervisorLivenessProbe()

func defaultSupervisorLivenessProbe() supervisorLivenessProbe {
	probe := supervisorLivenessProbe{
		PIDAlive:    process.IsPidAlive,
		PIDIdentity: process.VerifyPIDIdentity,
		PortLive:    supervisorPortLive,
	}
	if runtime.GOOS == "windows" {
		probe.PortOwnerPID = supervisorPortOwnerPID
	}
	return probe
}

var supervisorSelfPIDFn = os.Getpid

func setSupervisorLivenessProbeForTest(p supervisorLivenessProbe) func() {
	prev := supervisorLivenessProbeFns
	supervisorLivenessProbeFns = p
	return func() { supervisorLivenessProbeFns = prev }
}

func supervisorPortLive(port int) bool {
	if port <= 0 {
		return true
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), supervisorPortProbeTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func supervisorPortOwnerPID(port int) (int, bool, error) {
	return api.LoopbackPortOwnerPID(port)
}

func hydrateControllerRunningStates(ctrl *supervisorController, currentRunning map[string]bool) {
	if ctrl == nil {
		return
	}
	for taskName, running := range currentRunning {
		if !running {
			continue
		}
		ctrl.smStates.Store(canonicalSupervisorTaskName(taskName), api.StRunning)
	}
}

func startSupervisorLivenessMonitor(
	ctxDone <-chan struct{},
	stateDir string,
	intent *api.SupervisorIntentFile,
	tracker *DaemonRuntimeTracker,
	loop *api.EventLoop,
	events *api.SupervisorEventLog,
) {
	ticker := time.NewTicker(supervisorLivenessInterval)
	defer ticker.Stop()
	// Immediate first sweep at supervisor start, BEFORE the first ticker
	// tick. Warm-restart leaves alive-but-port-stale daemons recorded as
	// running in supervisor-state.json (loadSupervisorCurrentRunning keeps
	// their live PID for exactly this handoff — Codex bot #268 r10 P1). The
	// startup reconcile treats them as running and no-ops, so without this
	// immediate sweep the wedged wrapper would survive the full 5s liveness
	// interval before the terminate-first-then-respawn fires. Sweeping once
	// up front terminates the stale PID immediately, then the ticker drives
	// the steady-state cadence. Healthy daemons and dead-PID rows are
	// no-ops here (dead rows were already cleared to CurrentPID=0).
	sweepSupervisorLivenessOnce(stateDir, livenessSweepIntent(stateDir, intent), tracker, loop, events)
	for {
		select {
		case <-ctxDone:
			return
		case <-ticker.C:
			sweepSupervisorLivenessOnce(stateDir, livenessSweepIntent(stateDir, intent), tracker, loop, events)
		}
	}
}

// livenessSweepIntent returns the freshest supervisor intent for a sweep:
// the on-disk supervisor-intent.json when stateDir is set (so a mid-run
// install/migrate that rewrote ports is honored), falling back to the
// startup snapshot on any read error or when stateDir is empty (tests).
func livenessSweepIntent(stateDir string, fallback *api.SupervisorIntentFile) *api.SupervisorIntentFile {
	if stateDir == "" {
		return fallback
	}
	if refreshed, err := api.ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json")); err == nil {
		return refreshed
	}
	return fallback
}

func sweepSupervisorLivenessOnce(
	stateDir string,
	intent *api.SupervisorIntentFile,
	tracker *DaemonRuntimeTracker,
	loop *api.EventLoop,
	events *api.SupervisorEventLog,
) {
	if tracker == nil || loop == nil || intent == nil {
		return
	}
	byTask := map[string]api.SupervisorDaemon{}
	for _, d := range intent.Daemons {
		byTask[canonicalSupervisorTaskName(d.TaskName)] = d
	}
	now := time.Now().UTC()
	for taskName, entry := range tracker.Snapshot() {
		taskName = canonicalSupervisorTaskName(taskName)
		if entry.State != daemonRuntimeStateRunning || entry.CurrentPID <= 0 {
			continue
		}
		d, ok := byTask[taskName]
		if !ok {
			continue
		}
		live, reason := supervisorDaemonEntryLive(d, entry, now)
		if live {
			continue
		}
		if events != nil {
			_ = events.Emit(api.SupervisorEvent{
				Severity: "warn",
				Source:   "liveness",
				Event:    "daemon-running-state-stale",
				TaskName: taskName,
				Body: map[string]any{
					"pid":    entry.CurrentPID,
					"port":   d.Port,
					"reason": reason,
				},
			})
		}
		eventKind := api.EvChildExit
		if supervisorLivenessReasonNeedsRestart(reason) {
			eventKind = api.EvManualRestart
		}
		body := map[string]any{
			"pid":    entry.CurrentPID,
			"port":   d.Port,
			"reason": reason,
		}
		if eventKind == api.EvManualRestart && !supervisorLivenessReasonHasLivePID(reason) {
			tracker.MarkExited(taskName)
			body[supervisorLivenessRuntimeClearedBodyKey] = true
			if stateDir != "" {
				_ = persistDaemonRuntimeTracker(events, tracker, filepath.Join(stateDir, "supervisor-state.json"), taskName)
			}
		}
		loop.Post(api.LoopEvent{
			Kind:     eventKind,
			TaskName: taskName,
			Body:     body,
		})
	}
}

func supervisorLivenessRestartClearedRuntime(ev api.LoopEvent) bool {
	if ev.Kind != api.EvManualRestart || ev.Body == nil {
		return false
	}
	cleared, _ := ev.Body[supervisorLivenessRuntimeClearedBodyKey].(bool)
	if !cleared {
		return false
	}
	reason, _ := ev.Body["reason"].(string)
	return supervisorLivenessReasonNeedsRestart(reason)
}

func supervisorDaemonEntryLive(d api.SupervisorDaemon, entry DaemonRuntimeEntry, now time.Time) (bool, string) {
	probe := supervisorLivenessProbeFns
	if probe.PIDAlive == nil {
		probe.PIDAlive = process.IsPidAlive
	}
	if entry.CurrentPID <= 0 {
		return false, supervisorLivenessReasonMissingPID
	}
	if !probe.PIDAlive(entry.CurrentPID) {
		return false, supervisorLivenessReasonPIDDead
	}
	if probe.PIDIdentity != nil {
		if entry.StartedAt.IsZero() {
			return false, supervisorLivenessReasonPIDIdentityMissing
		}
		expectedExe := canonicalMcphubPath()
		if expectedExe == "" {
			return false, supervisorLivenessReasonPIDIdentityMissing
		}
		err := probe.PIDIdentity(process.PIDIdentityProof{
			PID:            entry.CurrentPID,
			ExecutablePath: expectedExe,
			StartedAt:      entry.StartedAt.UTC().Format(time.RFC3339Nano),
		})
		if err != nil {
			if errors.Is(err, process.ErrProcessIdentityUnsupported) {
				// Keep the PIDAlive result on platforms without start-time proof.
			} else if errors.Is(err, process.ErrProcessAlreadyExited) {
				return false, supervisorLivenessReasonPIDDead
			} else {
				return false, supervisorLivenessReasonPIDIdentityMismatch
			}
		}
	}
	if d.Port <= 0 {
		return true, ""
	}
	if probe.PortOwnerPID != nil {
		ownerPID, ok, err := probe.PortOwnerPID(d.Port)
		if err != nil {
			return false, supervisorLivenessReasonPortOwnerUnverified
		}
		if !ok {
			if !entry.StartedAt.IsZero() && now.Sub(entry.StartedAt) < supervisorPortBindGrace {
				return true, ""
			}
			return false, supervisorLivenessReasonPortUnbound
		}
		if supervisorSelfPIDFn != nil && ownerPID == supervisorSelfPIDFn() {
			return false, supervisorLivenessReasonPortOwnerSelf
		}
		if ownerPID != entry.CurrentPID {
			return false, supervisorLivenessReasonPortOwnerMismatch
		}
		return true, ""
	}
	if probe.PortLive == nil {
		probe.PortLive = supervisorPortLive
	}
	if !probe.PortLive(d.Port) {
		if !entry.StartedAt.IsZero() && now.Sub(entry.StartedAt) < supervisorPortBindGrace {
			return true, ""
		}
		return false, supervisorLivenessReasonPortUnbound
	}
	return true, ""
}

func supervisorLivenessReasonNeedsRestart(reason string) bool {
	switch reason {
	case supervisorLivenessReasonPortUnbound,
		supervisorLivenessReasonPortOwnerMismatch,
		supervisorLivenessReasonPortOwnerSelf,
		supervisorLivenessReasonPortOwnerUnverified:
		return true
	default:
		return false
	}
}

func supervisorLivenessReasonHasLivePID(reason string) bool {
	switch reason {
	case supervisorLivenessReasonPortUnbound,
		supervisorLivenessReasonPortOwnerMismatch,
		supervisorLivenessReasonPortOwnerSelf,
		supervisorLivenessReasonPortOwnerUnverified:
		return true
	default:
		return false
	}
}

func supervisorIntentPortMapForStateDir(stateDir string) map[string]int {
	out := map[string]int{}
	intent, err := api.ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"))
	if err != nil || intent == nil {
		return out
	}
	for _, d := range intent.Daemons {
		out[canonicalSupervisorTaskName(d.TaskName)] = d.Port
	}
	return out
}
