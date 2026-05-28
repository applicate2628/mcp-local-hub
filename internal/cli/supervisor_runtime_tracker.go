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
	// OrphanPID records the PID of a Windows post-create orphan when
	// the supervisor's best-effort kill failed. SEPARATE from CurrentPID
	// because makeProductionTerminateFn reads CurrentPID as the live
	// daemon's PID to terminate; conflating an orphan PID into
	// CurrentPID would cause the terminate identity-proof check to
	// silently nil-success (StartedAt empty -> ErrProcessIdentityMismatch
	// -> handleSupervisorRespawn spawns a new daemon while the orphan
	// is alive, port-collision).
	//
	// Operator visibility: `mcphub status` and the GUI Dashboard
	// surface OrphanPID alongside CurrentPID. Manual cleanup is
	// `taskkill /F /T /PID <OrphanPID>` on Windows.
	//
	// Closes bot finding on PR #238 3ba6773 (P2 don't-expose-orphan-
	// as-terminable-daemon-PID).
	OrphanPID int `json:",omitempty"`

	// JobProtection records the per-spawn Job Object allocation state
	// for the CURRENT spawn attempt. Tri-state semantics via *bool:
	// nil = unknown/legacy/not-yet-probed (default-trust, no warning
	// in UI), &true = Job allocated successfully (orphan-cleanup
	// invariant holds), &false = NewKillOnCloseJob failed AND the
	// supervisor fell through to cmd.Start (no orphan protection).
	// Persisted to supervisor-state.json and surfaced through the IPC
	// status response + GUI Dashboard.
	//
	// Field design rationale: a plain Go bool defaults to false on
	// zero-value, which would retroactively mark every legacy
	// supervisor-state.json daemon as "unprotected" after a v0.5.x
	// upgrade adds the field — a silent false-negative diagnostic.
	// Codex deep-sec flagged this as the load-bearing trap on PR
	// #242 sequencing review (2026-05-28). *bool with omitempty
	// preserves the absent-field-means-unknown semantic.
	JobProtection *bool `json:",omitempty"`
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

// clearOrphanPIDLocked zeroes the orphan PID field. Called by all
// state transitions that signal "we are past the orphan failure
// case" - successful spawn, exit, ordinary spawn-failed, backoff,
// terminate, quarantine. Without this clear, a failed orphan kill
// + operator manual cleanup + supervisor restart + successful
// spawn would leave the stale orphan PID in state.json, prompting
// operators to taskkill an unrelated reused PID.
//
// Caller MUST hold t.mu. Closes bot finding on PR #238 044489a
// (P2 clear-stale-orphan-PIDs-after-recovery).
func clearOrphanPIDLocked(entry *DaemonRuntimeEntry) {
	entry.OrphanPID = 0
}

// clearJobProtectionLocked clears the per-spawn Job Object status
// when no current spawn is active. The field describes a running
// spawn attempt, not a daemon's historical last outcome; preserving a
// stale false through backoff/idle/quarantine would warn operators
// about an unprotected process that no longer exists.
//
// Caller MUST hold t.mu.
func clearJobProtectionLocked(entry *DaemonRuntimeEntry) {
	entry.JobProtection = nil
}

// MarkJobProtection records the per-spawn Job Object allocation
// state for the daemon's current spawn attempt. Called by the
// spawn closure in supervise.go:
//
//   - protected=&true on the success branch immediately after
//     process.NewKillOnCloseJob returns nil (orphan-protection
//     invariant holds for this spawn).
//   - protected=&false on the documented non-fatal fallback —
//     NewKillOnCloseJob failed AND the supervisor proceeded with
//     plain cmd.Start (orphan-protection lost for this spawn).
//   - protected=nil when the platform has no Job Object protection
//     surface (POSIX no-op Job stub).
//
// Persisted to supervisor-state.json and surfaced through the IPC
// status response + api.DaemonStatus + GUI Dashboard so operators
// see the degraded state in steady-state monitoring rather than
// only post-incident via the supervisor-events.log warn entry.
// Closes consultant strategic concern #1 on PR #241.
//
// MarkSpawned does NOT clear JobProtection — the value is intrinsic
// to the spawn attempt and must remain visible until the next spawn
// rewrites it. Transitions with no current spawn clear the field so
// status does not show stale protection state for idle/backoff/
// quarantined daemons.
func (t *DaemonRuntimeTracker) MarkJobProtection(taskName string, protected *bool) {
	if t == nil {
		return
	}
	taskName = canonicalSupervisorTaskName(taskName)
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[taskName]
	entry.JobProtection = protected
	t.entries[taskName] = entry
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
	clearOrphanPIDLocked(&entry)
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
	clearOrphanPIDLocked(&entry)
	clearJobProtectionLocked(&entry)
	t.entries[taskName] = entry
}

// MarkSpawnFailedPreservePID is the orphan-aware variant of
// MarkSpawnFailed for the Windows post-create-orphan path. Records
// the orphan PID in the SEPARATE OrphanPID field (NOT in
// CurrentPID) so operator visibility is preserved without
// conflating the orphan with the "live daemon PID, terminate it"
// semantic of CurrentPID.
//
// Why a separate field: makeProductionTerminateFn reads CurrentPID
// and builds a PID identity proof; conflating an orphan PID into
// CurrentPID would silently nil-success the terminate (StartedAt
// empty -> ErrProcessIdentityMismatch -> respawn proceeds while
// orphan is alive -> port collision). With OrphanPID separate,
// terminate sees CurrentPID=0 (no live daemon), respawn proceeds
// normally; if orphan still holds the port, the respawn naturally
// hits port-in-use as the duplicate cap. Closes bot finding on PR
// #238 3ba6773 (P2 don't-expose-orphan-as-terminable-daemon-PID).
//
// Used only by the orphan path in supervise.go after a
// process.BestEffortKillByPID failure (errors.Is(startErr,
// process.ErrSpawnPostCreate) && kill returned non-nil). The
// common pre-child case still uses MarkSpawnFailed.
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
	entry.CurrentPID = 0
	entry.OrphanPID = orphanPID
	entry.StartedAt = time.Time{}
	entry.LastError = errorString(err)
	clearJobProtectionLocked(&entry)
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
	clearOrphanPIDLocked(&entry)
	clearJobProtectionLocked(&entry)
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
	clearOrphanPIDLocked(&entry)
	clearJobProtectionLocked(&entry)
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
	clearOrphanPIDLocked(&entry)
	clearJobProtectionLocked(&entry)
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
			OrphanPID:     daemonState.OrphanPID,
			JobProtection: daemonState.JobProtection,
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
			OrphanPID:     entry.OrphanPID,
			JobProtection: entry.JobProtection,
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
