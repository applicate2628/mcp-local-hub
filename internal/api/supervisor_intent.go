package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// SupervisorIntentFile is the on-disk schema for <state-dir>/supervisor-intent.json.
// Spec §"supervisor-intent.json (NEW)".
type SupervisorIntentFile struct {
	Version           int                `json:"version"`
	UpdatedAt         string             `json:"updated_at"`
	Daemons           []SupervisorDaemon `json:"daemons"`
	MaintenanceTimers []MaintenanceTimer `json:"maintenance_timers,omitempty"`
	StrictMode        bool               `json:"strict_mode"`
}

// SupervisorDaemon is one daemon descriptor keyed by canonical
// leading-backslash task name. Reconcile-prune (Q12) strips the
// prefix at compare time to match production install.go:1639-1642
// BARE form planned map.
type SupervisorDaemon struct {
	TaskName     string            `json:"task_name"` // canonical, e.g. "\\mcp-local-hub-memory-default"
	Server       string            `json:"server"`
	Daemon       string            `json:"daemon"`
	Command      string            `json:"command"`
	Args         []string          `json:"args"`
	Env          map[string]string `json:"env,omitempty"`
	Workspace    string            `json:"workspace,omitempty"`
	Port         int               `json:"port"`
	ManifestHash string            `json:"manifest_hash"`
}

// MaintenanceTimer schedules a fixed-cadence in-process job. Two
// kinds in v0.5.0: workspace-weekly-refresh, server-weekly-refresh.
// No cron parser; new kinds get new in-tree evaluators.
type MaintenanceTimer struct {
	Name    string   `json:"name"` // canonical task name for migration provenance
	Kind    string   `json:"kind"` // "workspace-weekly-refresh" | "server-weekly-refresh"
	Server  string   `json:"server,omitempty"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// ReadSupervisorIntent reads + parses with DisallowUnknownFields per
// the daemon-intent.json precedent at internal/api/daemon_intent.go:570-580.
func ReadSupervisorIntent(path string) (*SupervisorIntentFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var f SupervisorIntentFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &f, nil
}

// WriteSupervisorIntent goes through WriteStateFileAtomic (Task 1.1).
func WriteSupervisorIntent(path string, f *SupervisorIntentFile) error {
	return WriteStateFileAtomic(path, f)
}
