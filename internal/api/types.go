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
	Server   string `json:"server"`
	Daemon   string `json:"daemon"`
	TaskName string `json:"task_name"`
	// DisplayName is a human-readable label for the row, computed by
	// ComputeDaemonDisplayName (the single owner of the presentation rule):
	// "serena · <project>" for workspace serena, "<lang> @ <workspace>" for
	// workspace LSP, empty for global daemons. DISPLAY-ONLY — task_name stays
	// the canonical routing/identity key; CLI/GUI render this when non-empty
	// and fall back to the plain name otherwise. Omitted from JSON when empty
	// so `mcphub status --json` keeps the raw task_name available for ops.
	DisplayName string `json:"display_name,omitempty"`
	State       string `json:"state"` // "Running" | "Ready" | "Failed" | "Stopped"
	Port        int    `json:"port"`
	LastResult  int32  `json:"last_result"`
	NextRun     string `json:"next_run"` // backend-specific text (e.g. "Sunday, April 19, 2026 3:00:00 AM" on Windows; "N/A" when no trigger)
	PID         int    `json:"pid,omitempty"`
	RAMBytes    uint64 `json:"ram_bytes,omitempty"`
	UptimeSec   int64  `json:"uptime_sec,omitempty"`
	// OrphanPID is the Windows post-create orphan PID when the
	// supervisor's best-effort kill failed during spawn. Operator-
	// visible via `mcphub status --json` and the GUI Dashboard for
	// manual cleanup (`taskkill /F /T /PID <orphan_pid>` on Windows).
	// Zero (omitted in JSON) on the happy path. Sourced from the
	// supervisor IPC status response; populated only via the v0.5.x
	// supervisor IPC path. Closes bot finding on PR #238 044489a
	// (P2 surface-orphan-PID-through-status-clients).
	OrphanPID int `json:"orphan_pid,omitempty"`

	// StalePID is the wedged PID of a port-stale running daemon the
	// supervisor is terminate-restarting (state "Restarting"). Diagnostic
	// only via `mcphub status --json`; zero (omitted) on the happy path.
	// Sourced from the supervisor IPC status response (deep-sec #268 Reg-F1).
	StalePID int `json:"stale_pid,omitempty"`

	// JobProtection records the per-spawn Windows Job Object
	// allocation state for the daemon's current spawn attempt.
	// Tri-state via *bool with backward-compatible default:
	//
	//   - nil    — unknown / legacy state file / not yet probed.
	//              GUI treats as "no warning" (default-trust).
	//   - &true  — per-spawn Job allocated successfully (orphan-
	//              cleanup invariant holds).
	//   - &false — NewKillOnCloseJob failed AND supervisor fell
	//              through to plain cmd.Start without StartWithJob.
	//              Daemon runs without Job Object orphan-protection;
	//              the orphan-cleanup branch downgrades to per-PID
	//              BestEffortKillByPID. Operator-visible warning
	//              badge in the Dashboard.
	//
	// Sourced from the v0.5.x supervisor IPC path. Closes consultant
	// strategic concern #1 on PR #241 (silent-degradation gap when
	// fallback fires). Codex deep-sec flagged the *bool over plain
	// bool design as load-bearing for backward compatibility — a
	// plain bool would retroactively mark every legacy daemon as
	// unprotected when an older supervisor-state.json file (without
	// the field) is read after upgrade.
	JobProtection *bool `json:"job_protection,omitempty"`

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
	Name   string `json:"name"`
	Status string `json:"status"` // "via-hub" | "via-hub-inherited" | "can-migrate" | "unknown" | "per-session" | "external" | "not-installed"
	// "via-hub-inherited": routed via an inherited (import / below-write-target)
	// source the hub did not write and cannot demigrate (multi-layer adapter,
	// currently only MiMoCode). Managed stays FALSE for this status.
	ClientPresence map[string]ClientEntry `json:"client_presence"`
	LegacyConflict map[string]ClientEntry `json:"legacy_conflict,omitempty"`
	ManifestExists bool                   `json:"manifest_exists"`
	CanMigrate     bool                   `json:"can_migrate"`
	// Managed reports whether this server is currently ROUTED THROUGH THE
	// HUB (Status == "via-hub"). It is the explicit, classifier-derived
	// flag the Discovery view badges "Managed by hub" against, instead of
	// re-deriving hub-routing from Status in the frontend. Mirrored as TS
	// ScanEntry.managed. Always emitted (no omitempty) so a false value is
	// an explicit "not hub-managed" signal rather than an absent field the
	// UI would have to treat as unknown.
	Managed bool `json:"managed"`
	// DaemonPorts lists the loopback TCP ports declared by this server's
	// manifest daemons. It is the data the frontend matrix needs to mirror
	// the backend's PORT-AWARE via-hub rule: a loopback-http client entry
	// is "via-hub" only when its URL port matches one of these. Empty/
	// absent when no manifest exists (so any loopback entry is unmanaged
	// for this server). Mirrored as TS ScanEntry.daemon_ports.
	DaemonPorts  []int `json:"daemon_ports,omitempty"`
	ProcessCount int   `json:"process_count,omitempty"`

	// ProjectEnabled is the per-project-GUI Phase 2b claude-code .mcp.json
	// (Project-scope) APPROVAL reconciliation result for THIS server: true =
	// approved/loaded, false = NOT approved (pending the trust prompt). Claude's
	// model is OPT-IN — a .mcp.json server is approved only when it is in
	// enabledMcpjsonServers OR enableAllProjectMcpServers is true, AND it is not
	// in disabledMcpjsonServers (deny wins, absolute); an unlisted server with no
	// approve-all is NOT approved. It is a *bool so nil means "reconciliation does
	// not apply" — the GLOBAL scan and every non-claude-code project entry leave
	// it nil, and a claude-code entry SHADOWED by the Local scope also leaves it
	// nil (see ProjectShadowedByLocal). Its omitempty keeps those wire bytes
	// byte-identical (golden invariant). Only the project scan, and only for a
	// non-shadowed claude-code .mcp.json entry, sets it. The single owner of the
	// rule is clients.ClaudeLocalScope.IsMcpjsonServerEnabled.
	ProjectEnabled *bool `json:"project_enabled,omitempty"`

	// ProjectShadowedByLocal is the per-project-GUI Phase 2b LOCAL-shadows-PROJECT
	// flag (golden invariant: claude-code Local scope > Project scope, matched BY
	// NAME, entire-entry — no merge). true means THIS claude-code .mcp.json
	// (Project-scope) entry's Name also appears in the project's ~/.claude.json
	// LOCAL-scope mcpServers set, so Claude loads the LOCAL definition, not this
	// .mcp.json one — the .mcp.json approval (ProjectEnabled) is MOOT and left
	// nil. It is a *bool so nil means "not shadowed / does not apply" — the
	// GLOBAL scan, every non-claude-code entry, and every non-shadowed claude
	// entry leave it nil, and its omitempty keeps their wire bytes byte-identical
	// (golden invariant). Only the project scan sets it (to true) for a shadowed
	// claude-code entry. The Local definition itself is surfaced once in
	// ProjectScope.LocalServers.
	ProjectShadowedByLocal *bool `json:"project_shadowed_by_local,omitempty"`
}

// ClientEntry captures the shape of how one MCP server is configured inside
// one client config.
type ClientEntry struct {
	Transport string         `json:"transport"`           // "http" | "stdio" | "relay" | "absent"
	Endpoint  string         `json:"endpoint"`            // URL for http, command for stdio, etc.
	RelayURL  string         `json:"relay_url,omitempty"` // resolved relay --url target, when present
	Raw       map[string]any `json:"raw"`                 // the original JSON/TOML fragment
	// Inherited marks a hub-loopback cell whose source is a layer the hub never
	// wrote and CANNOT demigrate — for a multi-layer adapter (currently only
	// MiMoCode) a name resolved from the ~/.claude.json mcpServers IMPORT or a
	// config.json layer strictly BELOW the write target. Mirrors the
	// SourceBelowWriteTarget rollback split (clients.go:69) on the SCAN/label
	// side: defaults FALSE = the historical hub-ownable shape, and ONLY the
	// mimocode scan path sets it true (clients.MimoCodeNameAtOrAboveWriteTarget
	// == false on an http hub-loopback cell). classify() turns an Inherited cell
	// into Status "via-hub-inherited" (read-only) instead of "via-hub". The
	// omitempty tag keeps EVERY other client's wire bytes byte-identical.
	Inherited bool `json:"inherited,omitempty"`
}

// ScanResult bundles a full scan with timestamp for caching / SSE.
type ScanResult struct {
	At      time.Time   `json:"at"`
	Entries []ScanEntry `json:"entries"`

	// GUIPort is the live GUI/hub listener port the scan ran under (0 for a CLI
	// scan with no live GUI). The frontend applies the SAME live-port check the
	// backend classify uses for serena's /serena/mcp router cell, so a stale-port
	// serena entry renders not-connected in the matrix (matching the backend's
	// "external" classification) instead of falsely checked (Codex #379 r3).
	GUIPort int `json:"gui_port,omitempty"`

	// ClientConfigPresence reports per-client config file state,
	// INDEPENDENT of any server's per-entry presence. Bug-bash A2 (#13)
	// closure: before this field, the UI inferred "client installed"
	// from "client appears in at least one entry's client_presence",
	// which treated an existing config file with empty mcpServers (e.g.,
	// after wholesale demigrate) as "not installed" and disabled the
	// entire client column — a state-machine deadlock.
	//
	// Keys are client names (claude-code, codex-cli, cursor, vscode,
	// gemini-cli, qwen-cli, antigravity). Values:
	//   "ok"                              config file exists, stat
	//                                     succeeded.
	//   "missing-init-possible"           config file does not exist,
	//                                     but its parent directory does
	//                                     and is a regular directory —
	//                                     operator can initialize via
	//                                     POST /api/init-client-config
	//                                     to seed an empty stub.
	//   "missing-init-creatable"          config file AND its parent
	//                                     directory do not exist, but the
	//                                     path is under the user home and
	//                                     the longest-existing prefix of
	//                                     the parent chain is a real
	//                                     (non-symlink) directory chain.
	//                                     The hardened init pipeline can
	//                                     securely create the missing
	//                                     parent components (component-by-
	//                                     component, refusing symlinks /
	//                                     reparse-points and any path
	//                                     outside the home) then seed the
	//                                     stub. The GUI renders Initialize
	//                                     with a "will create <dir>"
	//                                     tooltip so the operator can pre-
	//                                     configure a not-yet-installed
	//                                     client. G17 (2026-06-18).
	//   "missing-init-blocked-symlink"    config file does not exist
	//                                     and the parent path is a
	//                                     symlink (or the absent parent's
	//                                     longest-existing prefix passes
	//                                     through a symlink). The hardened
	//                                     init pipeline refuses to follow
	//                                     parent symlinks (POSIX
	//                                     O_NOFOLLOW, Windows
	//                                     FILE_FLAG_OPEN_REPARSE_POINT),
	//                                     so the GUI suppresses the
	//                                     Initialize affordance for
	//                                     this state. v0.4.5 PR #208
	//                                     codex r1 F2 closure.
	//   "missing"                         neither file nor parent
	//                                     directory exists, AND the path
	//                                     is not under the user home (or
	//                                     the existing prefix contains a
	//                                     non-directory) — client
	//                                     genuinely not installed and not
	//                                     securely creatable.
	//   "error"                           stat/read/parse returned an
	//                                     unexpected error (permissions,
	//                                     malformed config, ACL/I-O
	//                                     anomaly) OR the path is a non-
	//                                     regular non-symlink shape
	//                                     (directory, pipe, device).
	//   "error-symlink"                   the config path is a symlink
	//                                     (resolvable or dangling) and
	//                                     the secure-write pipeline
	//                                     refuses it in all modes
	//                                     (post-PR #209 confused-deputy
	//                                     closure; opt-in via
	//                                     MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK
	//                                     re-enables resolvable-target
	//                                     symlinks). Split from "error"
	//                                     so the matrix renders a
	//                                     symlink-specific diagnostic
	//                                     ("replace the symlink / edit
	//                                     the target") instead of the
	//                                     misleading generic stat-error
	//                                     tooltip. work-items/bugs/
	//                                     2026-05-19-codex-config-symlink-blocked-by-pr209.md.
	//
	// Frontend uses "ok" to render an "available (enabled, unchecked)"
	// matrix cell for a manifested server when the cell's client is "ok"
	// but absent from that entry's per-server client_presence. The
	// "missing-init-possible" state additionally surfaces a per-column
	// "Initialize <client>" affordance in the matrix header so the
	// operator can create the empty stub without leaving the GUI.
	ClientConfigPresence map[string]string `json:"client_config_presence,omitempty"`

	// ClientScanErrors reports adapter-level parse/read failures keyed by client
	// id. These are per-client failures: ScanFrom keeps scanning sibling clients
	// and records the failed client here instead of returning a whole-scan error.
	ClientScanErrors map[string]string `json:"client_scan_errors,omitempty"`

	// ClientCapabilities reports the per-client capability flags the GUI
	// uses to decide which clients it may safely offer, keyed by every
	// clients.SupportedClientNames() id. It is the backend's single source
	// of truth so the GUI derives its column / direct-install universe
	// without a hard-coded mirror that drifts:
	//
	//   - scannable           — the client has a clientScanners() parser, so
	//                           /api/scan can report its per-entry presence
	//                           truthfully. The Servers matrix shows a non-
	//                           core column ONLY for a scannable client; a
	//                           presence-probed-but-unparsed client
	//                           (copilot-cli/amazon-q/openhands/aider) can
	//                           never have its cell reconciled after a
	//                           migrate, so it gets no enabled column.
	//   - direct_installable  — the adapter's AddEntry accepts a URL-only entry
	//                           (!IsRelayStdio). The Catalog direct-install flow
	//                           offers ONLY these clients; a relay-stdio adapter
	//                           rejects a URL-only entry, so a direct install
	//                           would deterministically fail. Broader than
	//                           remote_http_capable — it includes URL-native
	//                           non-core adapters (hermes/openclaw/opencode).
	//   - remote_http_capable — the client adapter is on the NARROW remote-http
	//                           manifest/header matrix (the 6 legacy clients);
	//                           used by the remote-http install plan + draft
	//                           surfaces, NOT the direct-install client choices.
	//   - adopt_supported     — /api/adopt/plan accepts this client for adopting
	//                           unknown stdio rows. Discovery offers Adopt only
	//                           for stdio rows whose source client has this flag.
	//
	// See client_capabilities.go (ClientCapabilities()) for the owner.
	ClientCapabilities map[string]ClientCapability `json:"client_capabilities,omitempty"`

	// ProjectScope carries the per-project-GUI Phase 2b claude-code LOCAL-scope
	// projection (~/.claude.json → projects.<key>). It is set ONLY by the
	// read-only project scan (GET /api/projects/scan), never by the global
	// Servers-matrix scan — the global scan leaves it nil, so its omitempty
	// keeps the global wire bytes byte-identical (golden-test invariant). See
	// ProjectScopeInfo.
	ProjectScope *ProjectScopeInfo `json:"project_scope,omitempty"`
}

// ProjectScopeInfo is the per-project-GUI Phase 2b claude-code LOCAL-scope
// projection attached to a project ScanResult. claude-code has a DUAL
// per-project substrate (design doc 2026-06-24-per-project-gui-design.md):
//
//   - the .mcp.json PROJECT scope (checked-in) — already surfaced as the
//     ScanResult.Entries (P2a), with per-entry enabled/disabled reconciliation
//     now folded in as ScanEntry.ProjectEnabled;
//   - the ~/.claude.json projects.<key> LOCAL scope (private to the user) — a
//     SEPARATE server set, surfaced here in LocalServers.
//
// P2b only READS + EXPOSES both scopes; the §10.2 PRESENTATION decision (show
// both in the GUI, or one) is a P3 UX sign-off, NOT built here. All fields are
// additive/omitempty.
type ProjectScopeInfo struct {
	// LocalServers is the SORTED set of server names from the project's
	// ~/.claude.json projects.<key>.mcpServers (the LOCAL scope) — distinct from
	// the .mcp.json Project-scope servers in ScanResult.Entries. Empty/absent
	// when ~/.claude.json has no entry for this project.
	LocalServers []string `json:"local_servers,omitempty"`

	// DisabledMcpjsonServers / EnabledMcpjsonServers are the project's
	// claude-code toggle arrays (projects.<key>.disabled/enabledMcpjsonServers),
	// surfaced verbatim for transparency. They gate the .mcp.json Project-scope
	// servers; the per-entry result of applying them is ScanEntry.ProjectEnabled.
	DisabledMcpjsonServers []string `json:"disabled_mcpjson_servers,omitempty"`
	EnabledMcpjsonServers  []string `json:"enabled_mcpjson_servers,omitempty"`

	// EnableAllProjectMcpServers is the project's claude-code
	// projects.<key>.enableAllProjectMcpServers approve-all flag, surfaced for
	// transparency. When true, an unlisted .mcp.json server (in neither toggle
	// array) is APPROVED; when false (the opt-IN default), it is NOT approved.
	// It feeds the per-entry ScanEntry.ProjectEnabled result. omitempty keeps a
	// false value off the wire (no UI change vs absent).
	EnableAllProjectMcpServers bool `json:"enable_all_project_mcp_servers,omitempty"`
}

// BackupInfo describes one file in the backup area.
type BackupInfo struct {
	Client   string    `json:"client"`
	Path     string    `json:"path"`
	Kind     string    `json:"kind"` // "original" | "timestamped"
	ModTime  time.Time `json:"mod_time"`
	SizeByte int64     `json:"size_byte"`
}

// SupervisorIntentEntry is the v0.5.0 plan-side entry that describes one
// daemon (or weekly-refresh maintenance row) the supervisor should keep
// running. The shape is parallel to ScheduledTaskPlan during the
// transition so existing callers (buildPlan -> executeInstallTo,
// printPlanTo, prune set construction) can switch over without a wire
// format break.
//
// One entry per Plan.SupervisorIntent slot maps to one SupervisorDaemon
// row in supervisor-intent.json at install time; Name is the BARE form
// (no leading backslash) — supervisor-intent.json stores the canonical
// leading-backslash form and the prune-set comparator strips the prefix
// at compare time (see buildPruneSetForReconcile + install.go:1773).
//
// Spec §"Q12 CLI/GUI status seam" + plan §2611-2644.
type SupervisorIntentEntry struct {
	Name       string
	Command    string
	Args       []string
	WorkingDir string
	Trigger    string // human-readable; "At logon" or "Weekly Sun 03:00"
}
