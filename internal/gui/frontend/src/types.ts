// Mirrors api.DaemonStatus (internal/api/types.go). Server-supplied
// structural flags (is_maintenance, is_workspace_scoped) MUST be read
// from these fields — do NOT re-derive from task_name in TS, or the
// filters drift from the canonical Go predicates.
export interface DaemonStatus {
  server: string;
  daemon: string;
  port?: number;
  pid?: number;
  state: string;
  task_name?: string;
  is_maintenance?: boolean;
  is_workspace_scoped?: boolean;
}

// Mirrors api.ScanResult.
export interface ScanResult {
  at: string;
  entries: ScanEntry[] | null;
  // Bug-bash A2 (#13): per-client config file state, independent of
  // per-entry client_presence. Keys are client names; values from
  // the ClientConfigState union below. Frontend uses this to render a
  // cell as "available" (enabled, unchecked) when the cell's client
  // config file exists but has no entry for this particular server.
  //
  // v0.4.5 init-button: the additional "missing-init-possible" state
  // surfaces a per-column "Initialize <client>" affordance in the
  // matrix header so the operator can seed the empty stub from the
  // GUI without dropping to the shell.
  //
  // v0.4.5 PR #208 codex r1 F2: "missing-init-blocked-symlink" is
  // emitted when the client's config parent path resolves through a
  // symlink. The hardened init pipeline refuses to follow parent
  // symlinks (POSIX O_NOFOLLOW, Windows FILE_FLAG_OPEN_REPARSE_POINT),
  // so the Initialize affordance is suppressed for this state; without
  // the new value, a dotfile-symlink setup (e.g. ~/.config/Claude/
  // -> ~/dotfiles/Claude) would render the button only for the click
  // to deterministically fail with INIT_FAILED.
  client_config_presence?: Record<string, ClientConfigState>;
}

export type ClientConfigState =
  | "ok"
  | "missing-init-possible"
  | "missing-init-blocked-symlink"
  | "missing"
  | "error";

export interface ScanEntry {
  name: string;
  status?: string;
  client_presence?: Record<string, ClientPresence>;
  // True iff servers/<name>/manifest.yaml exists. Bug-bash A3 (#11/#12):
  // legacy MCP entries discovered in client configs (e.g., `time-server`
  // in .cursor/mcp.json) appear in scan output with manifest_exists=false
  // and can_migrate=false. The main Servers matrix hides those rows;
  // they surface in a read-only "Other MCP entries" expander so the
  // operator can see them without confusing them for mcphub-managed.
  manifest_exists?: boolean;
  // Bug-bash A2 bot r1 P2 closure: non-migratable rows (no manifest,
  // per-session, unknown) must NOT get "available" cells from
  // client_config_presence — clicking them would hit deterministic
  // /api/migrate errors. Mirror backend ScanEntry.CanMigrate so
  // collectServers can gate the fallback to migratable rows only.
  can_migrate?: boolean;
}

export interface ClientPresence {
  transport?: "http" | "stdio" | "relay" | "absent" | string;
  endpoint?: string;
  raw?: unknown;
}

// Per-cell routing tag consumed by the Servers matrix.
//   "via-hub"       — entry exists and points at our loopback hub URL.
//   "direct"        — entry exists with non-hub endpoint (legacy / pre-mcphub).
//   "available"     — client config file exists, no entry for this
//                     server yet (operator can migrate). Bug-bash A2
//                     closure — without this, an empty-mcpServers config
//                     was indistinguishable from "client not installed"
//                     and disabled the whole column.
//   "not-installed" — client config file absent on this host.
//   "unsupported"   — client cannot route this server via the hub.
//   "config-error"  — `os.Stat` on the client config returned an error
//                     OTHER than IsNotExist (typically a permissions /
//                     ACL / I/O anomaly). Distinct from "not-installed"
//                     so the matrix can render an actionable diagnostic
//                     instead of silently dropping the cell. Surfaced
//                     by the v0.4.5 PR #208 deep-sec Lane B follow-up.
export type Routing =
  | "via-hub"
  | "direct"
  | "available"
  | "not-installed"
  | "unsupported"
  | "config-error";

export interface ServerRow {
  name: string;
  routing: Record<string, Routing>;
  // Bug-bash A3 (#11/#12): true when servers/<name>/manifest.yaml
  // exists. Main Servers matrix filters to manifested rows; legacy
  // non-mcphub entries surface in a separate expander.
  manifested: boolean;
}

// Aggregate-per-server shape produced by aggregateStatus.
export interface ServerAggregate {
  server: string;
  state: string;
  port: number | null;
  daemonCount: number;
}

// DaemonFormEntry — A2b extension: adds form-state-only UUID for identity-
// stable rename + delete. The UUID is NEVER serialized to YAML; it's
// replaced by daemon.name at toYAML time and re-generated on each Load.
export interface DaemonFormEntry {
  _id: string;
  name: string;
  port: number;
  // A2b Advanced, workspace-scoped only:
  context?: string;
  extra_args?: string[];
}

// BindingFormEntry — A2b extension: references daemon by _id internally
// for rename safety. At toYAML time the _id is resolved to the daemon's
// current name.
export interface BindingFormEntry {
  client: string;
  daemonId: string;
  url_path: string;
}

// LanguageFormEntry — A2b Advanced (workspace-scoped only).
export interface LanguageFormEntry {
  _id: string;
  name: string;
  backend: string;
  transport?: "stdio" | "http_listen" | "native_http";
  lsp_command?: string;
  extra_flags?: string[];
}

// ManifestFormState — A2b shape. loadedHash + _preservedRaw support
// stale-file detection + round-trip preservation. Advanced fields are
// optional: kind-gated sub-fields (languages, port_pool, daemon.context)
// are only serialized when kind === "workspace-scoped".
export interface ManifestFormState {
  name: string;
  kind: "global" | "workspace-scoped";
  transport: "stdio-bridge" | "native-http";
  command: string;
  base_args: string[];
  env: Array<{ key: string; value: string }>;
  daemons: DaemonFormEntry[];
  client_bindings: BindingFormEntry[];
  weekly_refresh: boolean;
  // A2b Advanced:
  idle_timeout_min?: number;
  base_args_template?: string[];
  languages?: LanguageFormEntry[];
  port_pool?: { start: number; end: number };
  // A2b state-only:
  loadedHash: string;
  _preservedRaw: Record<string, unknown>;
}

export interface ValidationWarning {
  message: string;
}

// ManifestValidateResponse mirrors the /api/manifest/validate handler shape.
export interface ManifestValidateResponse {
  warnings: string[];
}

// ExtractManifestResponse is a placeholder until the extract endpoint lands
// in a later task. Shape: { yaml: string }.
export interface ExtractManifestResponse {
  yaml: string;
}

// GetManifestResponse mirrors the new /api/manifest/get response.
export interface GetManifestResponse {
  yaml: string;
  hash: string;
}

// G3 — wire types mirroring internal/api/health.go HealthSnapshot.
// Field names and JSON tags match the Go side; keep in sync if the
// backend type evolves. Items[] may be null for unsupported/error/
// synthetic-empty subsections (Go has no `omitempty`); frontend MUST
// normalize via `sub.items ?? []` at the screen boundary.

export interface HealthSnapshot {
  schema_version: string;
  hub: HubSection;
  daemons: DaemonsSection;
  probes?: ProbesSection;
  capabilities?: CapabilitiesSection;
}

export interface HubSection {
  // health.go:43-49 — JSON tags exact mirror.
  version: string;
  commit: string;
  build_date: string;
  // health.go:45 StartedAt is `string` (RFC3339-ish), not unix seconds.
  started_at: string;
  // health.go:46 Lock is required (`json:"lock"` no omitempty).
  lock: { pid: number; port: number };
  generated_at: number;
  // health.go:48 TTLMs is `*int64` — nullable.
  ttl_ms: number | null;
}

export interface DaemonsSection {
  items: DaemonRow[];
  generated_at: number;
  // health.go:60 TTLMs is `int64` (NOT nullable for daemons section).
  ttl_ms: number;
  errors: SectionError[];
}

export interface DaemonRow {
  server: string;
  daemon: string;
  // health.go:72 Backend is `omitempty` — optional on the wire.
  backend?: string;
  pid: number;
  port: number;
  ram_bytes: number;
  uptime_sec: number;
  state: string;
  restart_count: number;
  // health.go:79 LastRestartAt is `*string` (RFC3339-ish or null).
  last_restart_at: string | null;
}

export interface ProbesSection {
  items: ProbeRow[];
  generated_at: number;
  // health.go:85 TTLMs is `int64` (NOT nullable for probes section).
  ttl_ms: number;
  errors: SectionError[];
}

export interface ProbeRow {
  server: string;
  daemon: string;
  ok: boolean;
  tool_count: number;
  // health.go:94 Err is required `string` (always emitted, "" when no err).
  err: string;
  // health.go:95 Source is required `string` (always emitted, "" or "proxy-synthetic").
  source: "" | "proxy-synthetic";
}

export interface CapabilitiesSection {
  items: CapabilityRow[];
  generated_at: number;
  ttl_ms: number;
  errors: SectionError[];
}

export interface CapabilityRow {
  server: string;
  daemon: string;
  tools: CapabilitySubSection;
  prompts: CapabilitySubSection;
  resources: CapabilitySubSection;
}

export interface CapabilitySubSection {
  state: "ok" | "empty" | "unsupported" | "error" | "stale";
  // null on unsupported/error/synthetic-empty paths — see types.ts header note.
  items: CapabilityItem[] | null;
  err?: string;
}

export interface CapabilityItem {
  name: string;
  id: string;        // canonical "server/daemon/kind/name"
  namespace: string; // == server name
  kind: "tool" | "prompt" | "resource";
}

export interface SectionError {
  scope: string;
  err: string;
}
