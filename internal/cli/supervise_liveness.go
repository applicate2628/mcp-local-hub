package cli

import (
	"fmt"
	"net"
	"path/filepath"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

const (
	supervisorPortProbeTimeout = 300 * time.Millisecond
	supervisorPortBindGrace    = 5 * time.Second
	supervisorLivenessInterval = 5 * time.Second
)

type supervisorLivenessProbe struct {
	PIDAlive func(pid int) bool
	PortLive func(port int) bool
}

var supervisorLivenessProbeFns = supervisorLivenessProbe{
	PIDAlive: process.IsPidAlive,
	PortLive: supervisorPortLive,
}

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
	for {
		select {
		case <-ctxDone:
			return
		case <-ticker.C:
			currentIntent := intent
			if stateDir != "" {
				if refreshed, err := api.ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json")); err == nil {
					currentIntent = refreshed
				}
			}
			sweepSupervisorLivenessOnce(stateDir, currentIntent, tracker, loop, events)
		}
	}
}

func sweepSupervisorLivenessOnce(
	stateDir string,
	intent *api.SupervisorIntentFile,
	tracker *DaemonRuntimeTracker,
	loop *api.EventLoop,
	events *api.SupervisorEventLog,
) {
	_ = stateDir
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
		if reason == "port_unbound" {
			eventKind = api.EvManualRestart
		}
		loop.Post(api.LoopEvent{
			Kind:     eventKind,
			TaskName: taskName,
			Body: map[string]any{
				"pid":    entry.CurrentPID,
				"port":   d.Port,
				"reason": reason,
			},
		})
	}
}

func supervisorDaemonEntryLive(d api.SupervisorDaemon, entry DaemonRuntimeEntry, now time.Time) (bool, string) {
	probe := supervisorLivenessProbeFns
	if probe.PIDAlive == nil {
		probe.PIDAlive = process.IsPidAlive
	}
	if probe.PortLive == nil {
		probe.PortLive = supervisorPortLive
	}
	if entry.CurrentPID <= 0 {
		return false, "missing_pid"
	}
	if !probe.PIDAlive(entry.CurrentPID) {
		return false, "pid_dead"
	}
	if d.Port > 0 && !probe.PortLive(d.Port) {
		if !entry.StartedAt.IsZero() && now.Sub(entry.StartedAt) < supervisorPortBindGrace {
			return true, ""
		}
		return false, "port_unbound"
	}
	return true, ""
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
