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
}

export interface ScanEntry {
  name: string;
  status?: string;
  client_presence?: Record<string, ClientPresence>;
}

export interface ClientPresence {
  transport?: "http" | "stdio" | "relay" | "absent" | string;
  endpoint?: string;
  raw?: unknown;
}

// Per-cell routing tag consumed by the Servers matrix.
export type Routing = "via-hub" | "direct" | "not-installed" | "unsupported";

export interface ServerRow {
  name: string;
  routing: Record<string, Routing>;
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
