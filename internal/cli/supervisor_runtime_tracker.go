package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"mcp-local-hub/internal/api"
)

const (
	daemonRuntimeStateIdle       = "idle"
	daemonRuntimeStateRunning    = "running"
	daemonRuntimeStateBackoff    = "backoff"
	daemonRuntimeStateQuarantine = "quarantine"
)

type DaemonRuntimeEntry struct {
	State         string
	CurrentPID    int
	StartedAt     time.Time
	PIDGeneration int
	RestartCount  int
	LastError     string
}

type DaemonRuntimeTracker struct {
	mu       sync.RWMutex
	entries  map[string]DaemonRuntimeEntry
	failures map[string][]time.Time // per-task crash timestamps (sliding window, not persisted)
}

func NewDaemonRuntimeTracker() *DaemonRuntimeTracker {
	return &DaemonRuntimeTracker{
		entries:  map[string]DaemonRuntimeEntry{},
		failures: map[string][]time.Time{},
	}
}

// RecordCrashAndCountInWindow appends `now` to the per-task failures
// slice, prunes entries older than (now - window), and returns the
// current count within the window. Crash timestamps are in-memory
// only — not persisted to supervisor-state.json. On supervisor cold
// restart the window starts fresh, which matches the design intent:
// pre-restart crashes are not relevant for runtime respawn decisions
// (the operator deliberately reset state).
func (t *DaemonRuntimeTracker) RecordCrashAndCountInWindow(taskName string, now time.Time, window time.Duration) int {
	if t == nil {
		return 0
	}
	taskName = canonicalSupervisorTaskName(taskName)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failures == nil {
		t.failures = map[string][]time.Time{}
	}
	cutoff := now.Add(-window)
	prev := t.failures[taskName]
	kept := make([]time.Time, 0, len(prev)+1)
	for _, ts := range prev {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	kept = append(kept, now)
	t.failures[taskName] = kept
	return len(kept)
}

// CrashCountInWindow returns the current failures-in-window count
// without recording a new failure. Used by tests + diagnostic
// snapshots; production callers go through RecordCrashAndCountInWindow.
func (t *DaemonRuntimeTracker) CrashCountInWindow(taskName string, now time.Time, window time.Duration) int {
	if t == nil {
		return 0
	}
	taskName = canonicalSupervisorTaskName(taskName)
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.failures == nil {
		return 0
	}
	cutoff := now.Add(-window)
	n := 0
	for _, ts := range t.failures[taskName] {
		if ts.After(cutoff) {
			n++
		}
	}
	return n
}

// ClearCrashes drops the per-task failures slice. Called when a
// daemon survives long enough to be considered healthy — resets the
// sliding window so future crashes start the backoff sequence from
// scratch.
func (t *DaemonRuntimeTracker) ClearCrashes(taskName string) {
	if t == nil {
		return
	}
	taskName = canonicalSupervisorTaskName(taskName)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failures != nil {
		delete(t.failures, taskName)
	}
}

func (t *DaemonRuntimeTracker) MarkSpawned(taskName string, pid int, startedAt time.Time) {
	if t == nil {
		return
	}
	taskName = canonicalSupervisorTaskName(taskName)
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[taskName]
	if entry.PIDGeneration > 0 {
		entry.RestartCount++
	}
	entry.State = daemonRuntimeStateRunning
	entry.CurrentPID = pid
	entry.StartedAt = startedAt.UTC()
	entry.PIDGeneration++
	entry.LastError = ""
	t.entries[taskName] = entry
}

func (t *DaemonRuntimeTracker) MarkSpawnFailed(taskName string, err error) {
	if t == nil {
		return
	}
	taskName = canonicalSupervisorTaskName(taskName)
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[taskName]
	if entry.PIDGeneration > 0 {
		entry.RestartCount++
	}
	entry.State = daemonRuntimeStateBackoff
	entry.CurrentPID = 0
	entry.StartedAt = time.Time{}
	entry.LastError = errorString(err)
	t.entries[taskName] = entry
}

// MarkSpawnFailedPreservePID is the orphan-aware variant of
// MarkSpawnFailed for the Windows post-create-orphan path. Unlike
// MarkSpawnFailed (which clears CurrentPID=0), this method preserves
// the supplied orphanPID so the operator can find it in
// supervisor-state.json for manual cleanup (`taskkill /F /T /PID <pid>`)
// when the supervisor's best-effort Job-level kill failed.
//
// Used only by the orphan path in supervise.go after a TerminateJobObject
// failure (errors.Is(startErr, process.ErrSpawnPostCreate) && job kill
// returned non-nil). The common pre-child case still uses MarkSpawnFailed
// (no orphan PID to preserve).
//
// Closes bot finding on PR #237 16d99d7 (P2 preserve-orphan-PID).
func (t *DaemonRuntimeTracker) MarkSpawnFailedPreservePID(taskName string, err error, orphanPID int) {
	if t == nil {
		return
	}
	taskName = canonicalSupervisorTaskName(taskName)
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[taskName]
	if entry.PIDGeneration > 0 {
		entry.RestartCount++
	}
	entry.State = daemonRuntimeStateBackoff
	entry.CurrentPID = orphanPID
	entry.StartedAt = time.Time{}
	entry.LastError = errorString(err)
	t.entries[taskName] = entry
}

func (t *DaemonRuntimeTracker) MarkExited(taskName string) {
	if t == nil {
		return
	}
	taskName = canonicalSupervisorTaskName(taskName)
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[taskName]
	entry.State = daemonRuntimeStateIdle
	entry.CurrentPID = 0
	entry.StartedAt = time.Time{}
	entry.LastError = ""
	t.entries[taskName] = entry
}

func (t *DaemonRuntimeTracker) MarkTerminated(taskName string) {
	t.MarkExited(taskName)
}

// MarkBackoff transitions a task into the backoff state — used by
// the respawn dispatcher between a crash event and the next spawn
// attempt. Without this, status snapshots (mcphub status, GUI
// dashboard) report the daemon as Stopped during the backoff window
// even though a respawn is pending. Per codex bot P2 PR #230 round 2:
// "Set runtime state to backoff when respawn is scheduled".
//
// current_pid is cleared (no live child during backoff) and
// PIDGeneration is preserved (the next MarkSpawned increments it).
func (t *DaemonRuntimeTracker) MarkBackoff(taskName string) {
	if t == nil {
		return
	}
	taskName = canonicalSupervisorTaskName(taskName)
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[taskName]
	entry.State = daemonRuntimeStateBackoff
	entry.CurrentPID = 0
	entry.StartedAt = time.Time{}
	t.entries[taskName] = entry
}

// MarkQuarantined transitions a task into the quarantine state —
// reached when the per-task crash count crosses
// respawnQuarantineThreshold (10 failures in 30 min). Quarantine
// suspends further respawn attempts until supervisor cold-restart.
// Per codex bot P2 PR #230 round 2: "Transition runtime state to
// quarantine on threshold breach" + "Persist quarantined runtime
// state before returning".
//
// Like MarkBackoff, current_pid is cleared and PIDGeneration is
// preserved.
func (t *DaemonRuntimeTracker) MarkQuarantined(taskName string) {
	if t == nil {
		return
	}
	taskName = canonicalSupervisorTaskName(taskName)
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[taskName]
	entry.State = daemonRuntimeStateQuarantine
	entry.CurrentPID = 0
	entry.StartedAt = time.Time{}
	t.entries[taskName] = entry
}

func (t *DaemonRuntimeTracker) Get(taskName string) (DaemonRuntimeEntry, bool) {
	if t == nil {
		return DaemonRuntimeEntry{}, false
	}
	taskName = canonicalSupervisorTaskName(taskName)
	t.mu.RLock()
	defer t.mu.RUnlock()
	entry, ok := t.entries[taskName]
	return entry, ok
}

func (t *DaemonRuntimeTracker) Snapshot() map[string]DaemonRuntimeEntry {
	if t == nil {
		return map[string]DaemonRuntimeEntry{}
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]DaemonRuntimeEntry, len(t.entries))
	for taskName, entry := range t.entries {
		out[taskName] = entry
	}
	return out
}

func (t *DaemonRuntimeTracker) HydrateFromState(file *api.SupervisorStateFile) {
	if t == nil || file == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for taskName, daemonState := range file.Daemons {
		startedAt := time.Time{}
		if daemonState.StartedAt != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, daemonState.StartedAt); err == nil {
				startedAt = parsed.UTC()
			}
		}
		t.entries[canonicalSupervisorTaskName(taskName)] = DaemonRuntimeEntry{
			State:         runtimeStateFromSupervisorState(daemonState.State),
			CurrentPID:    daemonState.CurrentPID,
			StartedAt:     startedAt,
			PIDGeneration: daemonState.PIDGeneration,
			RestartCount:  len(daemonState.RestartHistory),
		}
	}
}

func (t *DaemonRuntimeTracker) PersistTo(path string) error {
	if t == nil {
		return nil
	}
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("empty supervisor state path")
	}
	snapshot := t.Snapshot()
	file := &api.SupervisorStateFile{
		Version: 1,
		Daemons: make(map[string]api.SupervisorDaemonState, len(snapshot)),
	}
	existing, err := api.ReadSupervisorState(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read existing supervisor state: %w", err)
		}
	} else if existing != nil {
		if existing.Version != 0 {
			file.Version = existing.Version
		}
		file.TransientPIDs = existing.TransientPIDs
		file.MaintenanceFiredAt = existing.MaintenanceFiredAt
	}
	for taskName, entry := range snapshot {
		daemonState := api.SupervisorDaemonState{
			State:         supervisorStateFromRuntimeState(entry.State),
			CurrentPID:    entry.CurrentPID,
			PIDGeneration: entry.PIDGeneration,
		}
		if !entry.StartedAt.IsZero() {
			daemonState.StartedAt = entry.StartedAt.UTC().Format(time.RFC3339Nano)
		}
		file.Daemons[taskName] = daemonState
	}
	return api.WriteStateFileAtomic(path, file)
}

func loadDaemonRuntimeTrackerFromStatePath(path string) (*DaemonRuntimeTracker, error) {
	tracker := NewDaemonRuntimeTracker()
	existing, err := api.ReadSupervisorState(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return tracker, nil
		}
		return tracker, fmt.Errorf("read supervisor-state.json: %w", err)
	}
	tracker.HydrateFromState(existing)
	return tracker, nil
}

func persistDaemonRuntimeTracker(events *api.SupervisorEventLog, tracker *DaemonRuntimeTracker, statePath string, taskName string) error {
	if tracker == nil || statePath == "" {
		return nil
	}
	if err := tracker.PersistTo(statePath); err != nil {
		emitRuntimeStatePersistFailed(events, taskName, err)
		return err
	}
	return nil
}

func emitRuntimeStatePersistFailed(events *api.SupervisorEventLog, taskName string, err error) {
	if events == nil || err == nil {
		return
	}
	_ = events.Emit(api.SupervisorEvent{
		Severity: api.SupervisorEventSeverityError,
		Source:   "lifecycle",
		Event:    "daemon-runtime-state-persist-failed",
		TaskName: canonicalSupervisorTaskName(taskName),
		Body: map[string]any{
			"err": err.Error(),
		},
	})
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func runtimeStateFromSupervisorState(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "", daemonRuntimeStateIdle:
		return daemonRuntimeStateIdle
	case daemonRuntimeStateRunning:
		return daemonRuntimeStateRunning
	case daemonRuntimeStateBackoff, "backoff-waiting":
		return daemonRuntimeStateBackoff
	case daemonRuntimeStateQuarantine, "quarantined":
		return daemonRuntimeStateQuarantine
	default:
		return strings.ToLower(strings.TrimSpace(state))
	}
}

func supervisorStateFromRuntimeState(state string) string {
	switch runtimeStateFromSupervisorState(state) {
	case daemonRuntimeStateBackoff:
		return "backoff-waiting"
	case daemonRuntimeStateQuarantine:
		return "quarantined"
	case daemonRuntimeStateRunning:
		return daemonRuntimeStateRunning
	default:
		return daemonRuntimeStateIdle
	}
}
