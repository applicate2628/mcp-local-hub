package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// SupervisorStateFile is the on-disk schema for <state-dir>/supervisor-state.json.
// Spec §"supervisor-state.json (NEW)".
type SupervisorStateFile struct {
	Version            int                              `json:"version"`
	Daemons            map[string]SupervisorDaemonState `json:"daemons"`
	TransientPIDs      []TransientPID                   `json:"transient_pids,omitempty"`
	MaintenanceFiredAt map[string]string                `json:"maintenance_fired_at,omitempty"`
}

// SupervisorDaemonState is per-daemon Hybrid C state.
type SupervisorDaemonState struct {
	State           string         `json:"state"` // idle|spawning|running|exiting|backoff-waiting|quarantined
	CurrentPID      int            `json:"current_pid"`
	PIDGeneration   int            `json:"pid_generation"`
	StartedAt       string         `json:"started_at,omitempty"`
	RestartHistory  []RestartEvent `json:"restart_history,omitempty"`
	BackoffUntil    string         `json:"backoff_until,omitempty"`
	QuarantineSince string         `json:"quarantine_since,omitempty"`
	QueuedAction    *QueuedAction  `json:"queued_action,omitempty"`
}

// RestartEvent is one entry in the 30-min sliding window failure-count
// store. Pruned on every state persist to entries with at >= now-30m.
type RestartEvent struct {
	At       string `json:"at"`
	ExitCode int    `json:"exit_code"`
	Signal   *int   `json:"signal,omitempty"`
}

// QueuedAction records the pending side-effect after the current
// `exiting` transition completes. Cleared on transition out of exiting.
type QueuedAction struct {
	Kind   string `json:"kind"` // "respawn" | "none"
	Reason string `json:"reason"`
}

// TransientPID tracks maintenance-timer fire-and-forget children that
// are NOT Job Object members. Used by quiesce-timers IPC + graceful
// exit + cold-start reaper.
type TransientPID struct {
	PID       int    `json:"pid"`
	Kind      string `json:"kind"`
	StartedAt string `json:"started_at"`
}

// ReadSupervisorState reads + parses with DisallowUnknownFields per
// the daemon-intent.json precedent at internal/api/daemon_intent.go:570-580.
func ReadSupervisorState(path string) (*SupervisorStateFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var f SupervisorStateFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &f, nil
}

// WriteSupervisorState goes through WriteStateFileAtomic (Task 1.1).
func WriteSupervisorState(path string, f *SupervisorStateFile) error {
	return WriteStateFileAtomic(path, f)
}
