package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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

	// SpawnHoldReason / SpawnHoldPath record that the pre-spawn existence gate
	// (P1.1) is holding this daemon because a path it needs is absent — a
	// stable id ("missing-binary" / "missing-workspace") plus the exact path.
	//
	// Deliberately NOT cleared by clearJobProtectionLocked or the state-
	// transition marks (MarkBackoff in particular): the gate marks the hold
	// BEFORE calling holdSpawnInBackoff so one MarkBackoff + persist pass
	// writes both the backoff state and the reason. The gate is the SOLE owner
	// and clears them itself (ClearSpawnHold) on the first pass where nothing
	// is absent.
	SpawnHoldReason string `json:",omitempty"`
	SpawnHoldPath   string `json:",omitempty"`
}

type DaemonRuntimeTracker struct {
	mu       sync.RWMutex
	entries  map[string]DaemonRuntimeEntry
	failures map[string][]time.Time // per-task crash timestamps (sliding window, not persisted)
	// reallocations is the per-task sliding window of ephemeral-collision port
	// REALLOCATIONS (the L1 self-heal). It is SEPARATE from `failures` on
	// purpose: a within-cap bind-refused reallocation must NOT fuel the crash /
	// quarantine march (mirrors the F1 port-gate "no crash increment"
	// precedent), so it is counted here, not in `failures`. In-memory only,
	// resets on cold restart — pre-restart reallocations are not relevant to
	// runtime respawn decisions, exactly like `failures`.
	reallocations       map[string][]time.Time
	ownershipGeneration atomic.Uint64
}

func NewDaemonRuntimeTracker() *DaemonRuntimeTracker {
	return &DaemonRuntimeTracker{
		entries:       map[string]DaemonRuntimeEntry{},
		failures:      map[string][]time.Time{},
		reallocations: map[string][]time.Time{},
	}
}

// Generation returns the fleet-wide port-ownership generation. It changes when
// the tracker records a daemon gaining or losing CurrentPID, so status can
// invalidate a cached OS port-owner snapshot for every spawn/exit path without
// a separate manual invalidation side channel.
func (t *DaemonRuntimeTracker) Generation() uint64 {
	if t == nil {
		return 0
	}
	return t.ownershipGeneration.Load()
}

// setCurrentPIDLocked updates CurrentPID and bumps the fleet ownership generation
// only when the ownership-relevant PID value actually changes.
//
// Caller MUST hold t.mu.
func (t *DaemonRuntimeTracker) setCurrentPIDLocked(entry *DaemonRuntimeEntry, pid int) {
	if entry.CurrentPID != pid {
		t.ownershipGeneration.Add(1)
	}
	entry.CurrentPID = pid
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

// RecordReallocationAndCountInWindow appends `now` to the per-task
// reallocations slice, prunes entries older than (now - window), and returns the
// count within the window (INCLUDING the just-recorded entry). The exact
// crash-window shape (RecordCrashAndCountInWindow) applied to the SEPARATE
// ephemeral-collision reallocation counter — so a within-cap reallocation is
// bounded per-window without ever touching the crash `failures` window that
// drives quarantine.
func (t *DaemonRuntimeTracker) RecordReallocationAndCountInWindow(taskName string, now time.Time, window time.Duration) int {
	if t == nil {
		return 0
	}
	taskName = canonicalSupervisorTaskName(taskName)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.reallocations == nil {
		t.reallocations = map[string][]time.Time{}
	}
	cutoff := now.Add(-window)
	prev := t.reallocations[taskName]
	kept := make([]time.Time, 0, len(prev)+1)
	for _, ts := range prev {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	kept = append(kept, now)
	t.reallocations[taskName] = kept
	return len(kept)
}

// ReallocationCountInWindow returns the current reallocations-in-window count
// WITHOUT recording a new reallocation (the peek used to decide whether the cap
// is already exhausted before spending a reallocation). Mirrors
// CrashCountInWindow.
func (t *DaemonRuntimeTracker) ReallocationCountInWindow(taskName string, now time.Time, window time.Duration) int {
	if t == nil {
		return 0
	}
	taskName = canonicalSupervisorTaskName(taskName)
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.reallocations == nil {
		return 0
	}
	cutoff := now.Add(-window)
	n := 0
	for _, ts := range t.reallocations[taskName] {
		if ts.After(cutoff) {
			n++
		}
	}
	return n
}

// ClearReallocations drops the per-task reallocations slice. Called (alongside
// ClearCrashes) when a reallocated daemon dwells stably in StRunning past the
// stabilize dwell — a genuinely-recovered daemon starts its next
// ephemeral-collision episode with a fresh reallocation budget. Mirrors
// ClearCrashes.
func (t *DaemonRuntimeTracker) ClearReallocations(taskName string) {
	if t == nil {
		return
	}
	taskName = canonicalSupervisorTaskName(taskName)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.reallocations != nil {
		delete(t.reallocations, taskName)
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

// MarkSpawnHold records that the pre-spawn existence gate is holding this task
// because reasonID's path is absent. Called BEFORE holdSpawnInBackoff so that
// call's MarkBackoff + persist writes the backoff state and the hold reason in
// one pass (MarkBackoff deliberately does not clear these fields).
func (t *DaemonRuntimeTracker) MarkSpawnHold(taskName, reasonID, path string) {
	if t == nil {
		return
	}
	taskName = canonicalSupervisorTaskName(taskName)
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[taskName]
	entry.SpawnHoldReason = reasonID
	entry.SpawnHoldPath = path
	t.entries[taskName] = entry
}

// ClearSpawnHold drops any recorded pre-spawn hold for the task and reports
// whether anything changed, so the caller can skip a state-file write on the
// healthy path (the gate runs on EVERY create-process transition).
func (t *DaemonRuntimeTracker) ClearSpawnHold(taskName string) bool {
	if t == nil {
		return false
	}
	taskName = canonicalSupervisorTaskName(taskName)
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[taskName]
	if entry.SpawnHoldReason == "" && entry.SpawnHoldPath == "" {
		return false
	}
	entry.SpawnHoldReason = ""
	entry.SpawnHoldPath = ""
	t.entries[taskName] = entry
	return true
}

// MarkSpawned records a fresh child PID for the task and bumps the tracker's
// monotonic PIDGeneration. It RETURNS the new generation so the production
// spawn closure can stamp the exit channel + wait goroutine with the exact
// generation this child belongs to (P1a generation-stamped exit attribution).
// A late cmd.Wait exit of a superseded (older-generation) child can then be
// dropped by MarkExitedIfCurrent / the controller stale guard instead of
// clearing the CURRENT child's tracking. Non-production callers ignore the
// return value, so the signature change is compile-compatible.
func (t *DaemonRuntimeTracker) MarkSpawned(taskName string, pid int, startedAt time.Time) int {
	if t == nil {
		return 0
	}
	taskName = canonicalSupervisorTaskName(taskName)
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[taskName]
	if entry.PIDGeneration > 0 {
		entry.RestartCount++
	}
	entry.State = daemonRuntimeStateRunning
	t.setCurrentPIDLocked(&entry, pid)
	entry.StartedAt = startedAt.UTC()
	entry.PIDGeneration++
	entry.LastError = ""
	clearOrphanPIDLocked(&entry)
	t.entries[taskName] = entry
	return entry.PIDGeneration
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
	t.setCurrentPIDLocked(&entry, 0)
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
	t.setCurrentPIDLocked(&entry, 0)
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
	t.setCurrentPIDLocked(&entry, 0)
	entry.StartedAt = time.Time{}
	entry.LastError = ""
	clearOrphanPIDLocked(&entry)
	clearJobProtectionLocked(&entry)
	t.entries[taskName] = entry
}

// MarkExitedIfCurrent clears the runtime entry (identical clears to MarkExited)
// ONLY when pidGeneration is the tracker's current generation for the task —
// i.e. pidGeneration >= entry.PIDGeneration. Returns false, entry UNTOUCHED,
// when the exit is STALE (pidGeneration < entry.PIDGeneration: an older
// generation's late cmd.Wait exit arriving after a MarkSpawned superseded it).
//
// This is the source-side half of P1a generation-stamped exit attribution: the
// per-child wait goroutine calls it with the generation MarkSpawned returned for
// THIS child, so a late exit of a superseded child never clears the CURRENT
// child's CurrentPID (the lost-child factory). The > case is impossible by
// monotonicity (only MarkSpawned bumps the generation, always by +1) and is
// treated as current defensively. Idempotent for the current generation: an
// already-cleared current-gen entry (CurrentPID already 0) still returns true.
func (t *DaemonRuntimeTracker) MarkExitedIfCurrent(taskName string, pidGeneration int) bool {
	if t == nil {
		return false
	}
	taskName = canonicalSupervisorTaskName(taskName)
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[taskName]
	if pidGeneration < entry.PIDGeneration {
		return false
	}
	entry.State = daemonRuntimeStateIdle
	t.setCurrentPIDLocked(&entry, 0)
	entry.StartedAt = time.Time{}
	entry.LastError = ""
	clearOrphanPIDLocked(&entry)
	clearJobProtectionLocked(&entry)
	t.entries[taskName] = entry
	return true
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
	t.setCurrentPIDLocked(&entry, 0)
	entry.StartedAt = time.Time{}
	clearOrphanPIDLocked(&entry)
	clearJobProtectionLocked(&entry)
	t.entries[taskName] = entry
}

// MarkQuarantined transitions a task into the quarantine state —
// reached when the per-task crash count crosses
// respawnQuarantineThreshold (10 failures in 30 min). Quarantine
// suspends AUTOMATIC respawns; an operator clears it without a full
// supervisor restart via `mcphub daemon recover <task>` (P2b — reap
// any port squatter then force a respawn) or a force respawn
// (POST /api/daemon/respawn {force:true}); a supervisor cold-restart
// also clears the in-memory window.
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
	t.setCurrentPIDLocked(&entry, 0)
	entry.StartedAt = time.Time{}
	clearOrphanPIDLocked(&entry)
	clearJobProtectionLocked(&entry)
	t.entries[taskName] = entry
}

func (t *DaemonRuntimeTracker) Remove(taskName string) {
	if t == nil {
		return
	}
	taskName = canonicalSupervisorTaskName(taskName)
	t.mu.Lock()
	defer t.mu.Unlock()
	if entry, ok := t.entries[taskName]; ok && entry.CurrentPID != 0 {
		t.ownershipGeneration.Add(1)
	}
	delete(t.entries, taskName)
	if t.failures != nil {
		delete(t.failures, taskName)
	}
	if t.reallocations != nil {
		delete(t.reallocations, taskName)
	}
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
			// RestartCount is in-memory-only (MarkSpawned/MarkSpawnFailed
			// bump it; it is never persisted and never surfaced — health.go
			// defaults the operator-facing restart_count to 0). It starts
			// fresh on cold restart, matching the in-memory-only restart
			// policy. The former len(RestartHistory) hydrate was always 0
			// because no production path ever wrote restart_history (removed
			// in the 2026-06-20 supervisor audit P3).
			RestartCount:  0,
			OrphanPID:     daemonState.OrphanPID,
			JobProtection: daemonState.JobProtection,
			// Hydrated for DISPLAY continuity across a supervisor restart only
			// — decision-inert, and corrected by the first gate pass for this
			// daemon (which re-probes the filesystem and clears or re-marks).
			SpawnHoldReason: daemonState.SpawnHoldReason,
			SpawnHoldPath:   daemonState.SpawnHoldPath,
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
	return mutateSupervisorStateFile(path, func(file *api.SupervisorStateFile) error {
		// REPLACE file.Daemons wholesale from the in-memory tracker
		// snapshot. SupervisorDaemonState now carries ONLY durable state
		// plus PID metadata (State/CurrentPID/PIDGeneration/StartedAt/
		// OrphanPID/JobProtection). The earlier forward-copy of
		// restart_history/backoff_until/quarantine_since/queued_action is
		// gone with those fields: no production path ever wrote them, so
		// there was never a real value to preserve. Restart-policy runtime
		// state (sliding window, backoff deadline, quarantine, queued
		// action, and the backoff/quarantine sub-states themselves) is
		// in-memory-only and resets on cold restart by design —
		// see SupervisorDaemonState's doc comment + the
		// DaemonRuntimeTracker.failures map (2026-06-20 supervisor audit
		// P3, superseding the Codex deep-sec PR #268 Conc-F1 forward-copy).
		file.Daemons = make(map[string]api.SupervisorDaemonState, len(snapshot))
		for taskName, entry := range snapshot {
			daemonState := api.SupervisorDaemonState{
				State:         supervisorStateFromRuntimeState(entry.State),
				CurrentPID:    entry.CurrentPID,
				PIDGeneration: entry.PIDGeneration,
				OrphanPID:     entry.OrphanPID,
				JobProtection: entry.JobProtection,
				// Pre-spawn existence-gate hold (P1.1). Persisted so `mcphub
				// status --json` and the GUI keep naming the absent path across
				// a supervisor restart instead of showing an unexplained
				// stopped daemon. Decision-inert by contract.
				SpawnHoldReason: entry.SpawnHoldReason,
				SpawnHoldPath:   entry.SpawnHoldPath,
			}
			if !entry.StartedAt.IsZero() {
				daemonState.StartedAt = entry.StartedAt.UTC().Format(time.RFC3339Nano)
			}
			file.Daemons[taskName] = daemonState
		}
		return nil
	})
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
	case daemonRuntimeStateRunning:
		return daemonRuntimeStateRunning
	default:
		return daemonRuntimeStateIdle
	}
}
