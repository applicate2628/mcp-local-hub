package api

import (
	"time"
)

// State is the snapshot of what the API knows about the running system.
// Currently treated as read-only after NewAPI; mutation paths add their
// own synchronization where they exist.
type State struct {
	Daemons  map[string]DaemonStatus // key: "<server>.<daemon>"
	LastScan *ScanResult
	LastSync time.Time
}

// DaemonStatus enriches the scheduler-task view with process stats.
type DaemonStatus struct {
	Server     string `json:"server"`
	Daemon     string `json:"daemon"`
	TaskName   string `json:"task_name"`
	State      string `json:"state"` // "Running" | "Ready" | "Failed" | "Stopped"
	Port       int    `json:"port"`
	LastResult int32  `json:"last_result"`
	NextRun    string `json:"next_run"` // backend-specific text (e.g. "Sunday, April 19, 2026 3:00:00 AM" on Windows; "N/A" when no trigger)
	PID        int    `json:"pid,omitempty"`
	RAMBytes   uint64 `json:"ram_bytes,omitempty"`
	UptimeSec  int64  `json:"uptime_sec,omitempty"`
}

// ScanEntry is one row in the unified "across all clients" view.
type ScanEntry struct {
	Name           string                 `json:"name"`
	Status         string                 `json:"status"` // "via-hub" | "can-migrate" | "unknown" | "per-session" | "not-installed"
	ClientPresence map[string]ClientEntry `json:"client_presence"`
	ManifestExists bool                   `json:"manifest_exists"`
	CanMigrate     bool                   `json:"can_migrate"`
	ProcessCount   int                    `json:"process_count,omitempty"`
}

// ClientEntry captures the shape of how one MCP server is configured inside
// one client config.
type ClientEntry struct {
	Transport string         `json:"transport"` // "http" | "stdio" | "relay" | "absent"
	Endpoint  string         `json:"endpoint"`  // URL for http, command for stdio, etc.
	Raw       map[string]any `json:"raw"`       // the original JSON/TOML fragment
}

// ScanResult bundles a full scan with timestamp for caching / SSE.
type ScanResult struct {
	At      time.Time   `json:"at"`
	Entries []ScanEntry `json:"entries"`
}

// BackupInfo describes one file in the backup area.
type BackupInfo struct {
	Client   string    `json:"client"`
	Path     string    `json:"path"`
	Kind     string    `json:"kind"` // "original" | "timestamped"
	ModTime  time.Time `json:"mod_time"`
	SizeByte int64     `json:"size_byte"`
}
