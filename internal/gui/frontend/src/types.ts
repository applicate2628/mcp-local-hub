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

// ManifestFormState is the authoritative in-memory shape for the AddServer
// screen. The form writes to it; toYAML(state) serializes it for the backend.
// Using array-of-objects for env and client_bindings (rather than maps) keeps
// add/delete operations and render ordering deterministic.
export interface ManifestFormState {
  name: string;
  kind: "global" | "workspace-scoped";
  transport: "stdio-bridge" | "native-http";
  command: string;
  base_args: string[];
  env: Array<{ key: string; value: string }>;
  daemons: Array<{ name: string; port: number }>;
  client_bindings: Array<{ client: string; daemon: string; url_path: string }>;
  weekly_refresh: boolean;
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
