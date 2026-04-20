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

	// MCP-level health probe (populated only by Status with probeHealth=true).
	// Running daemon / bound port does NOT imply the MCP protocol is alive —
	// the subprocess may be in a broken state, or (in gdb/lldb's case) the
	// MCP server may respond but its backend binary is missing. A successful
	// tools/list round-trip is the first layer of "operational health".
	Health *HealthProbe `json:"health,omitempty"`
}

// HealthProbe records the outcome of an MCP protocol smoke test against
// a daemon's HTTP endpoint. OK=true + ToolCount>0 = minimally operational.
// Err is populated (with OK=false) on transport error, non-2xx response,
// or a parseable JSON-RPC error in the tools/list response.
type HealthProbe struct {
	OK        bool   `json:"ok"`
	ToolCount int    `json:"tool_count,omitempty"`
	Err       string `json:"err,omitempty"`
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
