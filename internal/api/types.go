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

	// Workspace-scoped daemon fields (all empty for global daemons). Populated
	// from the workspace registry when TaskName matches the lazy-proxy pattern
	// `mcp-local-hub-lsp-<workspaceKey>-<language>`. Lifecycle is one of
	// LifecycleConfigured / LifecycleStarting / LifecycleActive /
	// LifecycleMissing / LifecycleFailed.
	//
	// IsWorkspaceScoped mirrors IsLazyProxyTaskName(TaskName) and is the
	// authoritative structural flag for consumers that only need to know
	// "is this a workspace-scoped lazy-proxy row?" without parsing TaskName
	// or depending on registry-derived fields (Workspace/Language/Lifecycle),
	// which can be empty when registry loading or enrichment fails. The GUI
	// Logs picker uses this flag to filter workspace-scoped rows out of the
	// global-daemon log dropdown; see internal/gui/assets/logs.js.
	IsWorkspaceScoped  bool      `json:"is_workspace_scoped,omitempty"`
	Workspace          string    `json:"workspace,omitempty"`
	Language           string    `json:"language,omitempty"`
	Backend            string    `json:"backend,omitempty"`
	Lifecycle          string    `json:"lifecycle,omitempty"`
	LastMaterializedAt time.Time `json:"last_materialized_at,omitempty"`
	LastToolsCallAt    time.Time `json:"last_tools_call_at,omitempty"`
	LastError          string    `json:"last_error,omitempty"`

	// IsMaintenance marks scheduler-maintenance rows (weekly-refresh tasks
	// in all three naming variants: hub-wide global, hub-wide workspace,
	// and legacy per-server). Populated by enrichStatusWithRegistry from
	// the canonical parseTaskName output (daemon == "weekly-refresh").
	//
	// The GUI uses this flag to filter maintenance rows out of surfaces
	// that only make sense for daemon rows:
	//   - Logs picker (internal/gui/assets/logs.js): empty `server` would
	//     produce a GET /api/logs/?... → 404.
	//   - Dashboard (internal/gui/assets/dashboard.js): empty `server`
	//     would render a blank-name card whose Restart button hits
	//     /api/servers//restart with an invalid target.
	// Using a server-side structural flag instead of duplicating the
	// task-name match in JS keeps the canonical Go parser as the single
	// source of truth; future maintenance tasks only need to update the
	// Go predicate.
	IsMaintenance bool `json:"is_maintenance,omitempty"`
}

// HealthProbe records the outcome of an MCP protocol smoke test against
// a daemon's HTTP endpoint. OK=true + ToolCount>0 = minimally operational.
// Err is populated (with OK=false) on transport error, non-2xx response,
// or a parseable JSON-RPC error in the tools/list response.
//
// Source distinguishes what the probe actually reached. Global daemons
// proxy requests straight to their upstream so "proxy" and "backend" are
// the same process; Source stays empty ("") there. Workspace-scoped
// lazy proxies answer initialize+tools/list synthetically from the
// embedded catalog without spawning the heavy backend — those rows
// carry Source=="proxy-synthetic". When --force-materialize also ran,
// the row's Lifecycle field (LifecycleActive | LifecycleMissing |
// LifecycleFailed) tells the caller the backend side; the CLI layer
// composes that into a combined human-readable cell.
type HealthProbe struct {
	OK        bool   `json:"ok"`
	ToolCount int    `json:"tool_count,omitempty"`
	Err       string `json:"err,omitempty"`
	Source    string `json:"source,omitempty"` // "proxy-synthetic" for workspace-scoped rows; "" otherwise
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
