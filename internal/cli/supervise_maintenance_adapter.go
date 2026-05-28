package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

type maintenanceStateAdapter struct {
	statePath string
	events    *api.SupervisorEventLog
}

func newMaintenanceStateAdapter(statePath string, events *api.SupervisorEventLog) *maintenanceStateAdapter {
	return &maintenanceStateAdapter{statePath: statePath, events: events}
}

func (a *maintenanceStateAdapter) GetMaintenanceFiredAt(kind string) (string, bool) {
	if a == nil {
		return "", false
	}
	file, err := readSupervisorStateFile(a.statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false
		}
		a.emitStateError("maintenance-state-read-failed", kind, err)
		return "", false
	}
	if file == nil || file.MaintenanceFiredAt == nil {
		return "", false
	}
	v, ok := file.MaintenanceFiredAt[kind]
	return v, ok
}

func (a *maintenanceStateAdapter) SetMaintenanceFiredAt(kind, rfc3339nanoUTC string) error {
	if a == nil {
		return nil
	}
	err := mutateSupervisorStateFile(a.statePath, func(file *api.SupervisorStateFile) error {
		if file.MaintenanceFiredAt == nil {
			file.MaintenanceFiredAt = map[string]string{}
		}
		file.MaintenanceFiredAt[kind] = rfc3339nanoUTC
		return nil
	})
	if err != nil {
		// Audit emit AND propagate error to the scheduler so it can
		// activate the in-process storm prevention cache. Without
		// the propagation, a silent persist failure leaves the
		// scheduler reading stale state on the next 60s tick and
		// re-firing — consultant flagged this as the load-bearing
		// blocker on PR #243.
		a.emitStateError("maintenance-state-write-failed", kind, err)
		return err
	}
	return nil
}

func (a *maintenanceStateAdapter) AddTransientPID(p api.TransientPID) {
	if a == nil {
		return
	}
	err := mutateSupervisorStateFile(a.statePath, func(file *api.SupervisorStateFile) error {
		file.TransientPIDs = append(file.TransientPIDs, p)
		return nil
	})
	if err != nil {
		a.emitStateError("maintenance-transient-write-failed", p.Kind, err)
	}
}

func (a *maintenanceStateAdapter) RemoveTransientPID(pid int) {
	if a == nil {
		return
	}
	err := mutateSupervisorStateFile(a.statePath, func(file *api.SupervisorStateFile) error {
		out := file.TransientPIDs[:0]
		for _, p := range file.TransientPIDs {
			if p.PID == pid {
				continue
			}
			out = append(out, p)
		}
		file.TransientPIDs = out
		return nil
	})
	if err != nil {
		a.emitStateError("maintenance-transient-remove-failed", "", err)
	}
}

func (a *maintenanceStateAdapter) emitStateError(event, kind string, err error) {
	if a == nil || a.events == nil || err == nil {
		return
	}
	body := map[string]any{"err": err.Error()}
	if kind != "" {
		body["kind"] = kind
	}
	_ = a.events.Emit(api.SupervisorEvent{
		Severity: "error",
		Source:   "maintenance",
		Event:    event,
		Body:     body,
	})
}

type maintenanceSpawner struct {
	events *api.SupervisorEventLog

	mu    sync.Mutex
	procs map[int]*maintenanceProcess
}

type maintenanceProcess struct {
	timer    api.MaintenanceTimer
	cmd      *exec.Cmd
	done     chan struct{}
	waitErr  error
	exitCode int
}

type maintenanceShutdownResult struct {
	Drained      int
	Killed       []int
	StillRunning []int
}

func newMaintenanceSpawner(events *api.SupervisorEventLog) *maintenanceSpawner {
	return &maintenanceSpawner{
		events: events,
		procs:  map[int]*maintenanceProcess{},
	}
}

func (s *maintenanceSpawner) Start(t api.MaintenanceTimer) (int, error) {
	if s == nil {
		return 0, errors.New("maintenance spawner is nil")
	}
	if strings.TrimSpace(t.Command) == "" {
		err := errors.New("maintenance command is empty")
		s.emitSpawnFailed(t, err)
		return 0, err
	}
	cmd := exec.Command(t.Command, t.Args...)
	process.NoConsole(cmd)
	if err := cmd.Start(); err != nil {
		s.emitSpawnFailed(t, err)
		return 0, err
	}
	if cmd.Process == nil || cmd.Process.Pid <= 0 {
		err := fmt.Errorf("maintenance command started without valid PID")
		s.emitSpawnFailed(t, err)
		return 0, err
	}
	pid := cmd.Process.Pid
	entry := &maintenanceProcess{
		timer: t,
		cmd:   cmd,
		done:  make(chan struct{}),
	}
	s.mu.Lock()
	s.procs[pid] = entry
	s.mu.Unlock()

	go s.waitAndRecord(pid, entry)
	return pid, nil
}

func (s *maintenanceSpawner) Wait(pid int) error {
	if s == nil {
		return errors.New("maintenance spawner is nil")
	}
	s.mu.Lock()
	entry := s.procs[pid]
	s.mu.Unlock()
	if entry == nil {
		return fmt.Errorf("maintenance Wait: unknown PID %d", pid)
	}
	<-entry.done
	s.mu.Lock()
	delete(s.procs, pid)
	s.mu.Unlock()
	return entry.waitErr
}

func (s *maintenanceSpawner) Shutdown(timeout time.Duration) maintenanceShutdownResult {
	result := maintenanceShutdownResult{}
	if s == nil {
		return result
	}
	entries := s.snapshotProcesses()
	if len(entries) == 0 {
		return result
	}

	deadline := time.Now().Add(timeout)
	remaining := entries
	for len(remaining) > 0 && (timeout <= 0 || time.Now().Before(deadline)) {
		remaining = collectFinishedMaintenance(remaining, &result)
		if len(remaining) == 0 {
			return result
		}
		time.Sleep(25 * time.Millisecond)
	}

	remaining = collectFinishedMaintenance(remaining, &result)
	for pid, entry := range remaining {
		if entry.cmd != nil && entry.cmd.Process != nil {
			if err := entry.cmd.Process.Kill(); err == nil {
				result.Killed = append(result.Killed, pid)
			}
		}
	}

	killDeadline := time.Now().Add(5 * time.Second)
	for len(remaining) > 0 && time.Now().Before(killDeadline) {
		remaining = collectFinishedMaintenance(remaining, &result)
		if len(remaining) == 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	for pid := range remaining {
		result.StillRunning = append(result.StillRunning, pid)
	}
	if len(result.Killed) > 0 || len(result.StillRunning) > 0 {
		s.emitShutdown(result)
	}
	return result
}

func (s *maintenanceSpawner) waitAndRecord(pid int, entry *maintenanceProcess) {
	err := entry.cmd.Wait()
	exitCode := 0
	waitErr := err
	if entry.cmd.ProcessState != nil {
		exitCode = entry.cmd.ProcessState.ExitCode()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exitCode = exitErr.ExitCode()
		waitErr = nil
	}
	entry.exitCode = exitCode
	entry.waitErr = waitErr
	close(entry.done)
	s.emitExited(pid, entry, err)
}

func (s *maintenanceSpawner) snapshotProcesses() map[int]*maintenanceProcess {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int]*maintenanceProcess, len(s.procs))
	for pid, entry := range s.procs {
		out[pid] = entry
	}
	return out
}

func collectFinishedMaintenance(in map[int]*maintenanceProcess, result *maintenanceShutdownResult) map[int]*maintenanceProcess {
	out := make(map[int]*maintenanceProcess, len(in))
	for pid, entry := range in {
		select {
		case <-entry.done:
			result.Drained++
		default:
			out[pid] = entry
		}
	}
	return out
}

func (s *maintenanceSpawner) emitSpawnFailed(t api.MaintenanceTimer, err error) {
	if s == nil || s.events == nil || err == nil {
		return
	}
	_ = s.events.Emit(api.SupervisorEvent{
		Severity: "warn",
		Source:   "maintenance",
		Event:    "maintenance-spawn-failed",
		TaskName: t.Name,
		Body: map[string]any{
			"kind":    t.Kind,
			"command": t.Command,
			"err":     err.Error(),
		},
	})
}

func (s *maintenanceSpawner) emitExited(pid int, entry *maintenanceProcess, rawErr error) {
	if s == nil || s.events == nil || entry == nil {
		return
	}
	severity := "info"
	body := map[string]any{
		"pid":       pid,
		"kind":      entry.timer.Kind,
		"command":   entry.timer.Command,
		"exit_code": entry.exitCode,
	}
	if rawErr != nil {
		severity = "warn"
		body["wait_err"] = rawErr.Error()
	}
	_ = s.events.Emit(api.SupervisorEvent{
		Severity: severity,
		Source:   "maintenance",
		Event:    "maintenance-exited",
		TaskName: entry.timer.Name,
		Body:     body,
	})
}

func (s *maintenanceSpawner) emitShutdown(result maintenanceShutdownResult) {
	if s == nil || s.events == nil {
		return
	}
	severity := "info"
	if len(result.StillRunning) > 0 {
		severity = "warn"
	}
	_ = s.events.Emit(api.SupervisorEvent{
		Severity: severity,
		Source:   "maintenance",
		Event:    "maintenance-shutdown",
		Body: map[string]any{
			"drained":       result.Drained,
			"killed":        result.Killed,
			"still_running": result.StillRunning,
		},
	})
}

func maintenanceTimersFromController(ctrl *supervisorController) []api.MaintenanceTimer {
	if ctrl == nil {
		return nil
	}
	return maintenanceTimersFromIntentCache(ctrl.intentCache)
}

func maintenanceTimersFromIntentCache(cache *IntentCache) []api.MaintenanceTimer {
	if cache == nil {
		return nil
	}
	snap, ok := cache.snap.Load().(*intentSnapshot)
	if !ok || snap == nil || snap.intent == nil || len(snap.intent.MaintenanceTimers) == 0 {
		return nil
	}
	out := make([]api.MaintenanceTimer, len(snap.intent.MaintenanceTimers))
	copy(out, snap.intent.MaintenanceTimers)
	return out
}

func runMaintenanceScheduler(ctxDone <-chan struct{}, graceful *gracefulCounter, sched *MaintenanceScheduler, timers func() []api.MaintenanceTimer, cadence time.Duration) {
	if sched == nil || timers == nil {
		return
	}
	if cadence <= 0 {
		cadence = 60 * time.Second
	}
	tick := func(now time.Time) {
		if graceful != nil && graceful.InProgress() {
			return
		}
		sched.Tick(now, timers())
	}
	tick(time.Now())
	ticker := time.NewTicker(cadence)
	defer ticker.Stop()
	for {
		select {
		case <-ctxDone:
			return
		case now := <-ticker.C:
			tick(now)
		}
	}
}
