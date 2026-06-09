// fetchOrThrow is the shared API wrapper mirroring the legacy fetchOrThrow
// from servers.js. Backend handlers surface errors via the {error, code}
// JSON envelope (writeAPIError in the Go side) — not the success shape the
// UI expects. Without the response-shape guard, callers that iterate the
// parsed body would treat the truthy envelope object as iterable and throw
// inside render logic, leaving the screen blank. Require resp.ok AND the
// declared top-level shape before trusting the payload.
export async function fetchOrThrow<T>(
  path: string,
  expect: "array" | "object",
  init?: RequestInit,
): Promise<T> {
  const resp = await fetch(path, init);
  let data: unknown = null;
  try {
    data = await resp.json();
  } catch {
    // Non-JSON body left as null; handled below.
  }
  if (!resp.ok) {
    const msg = (data as { error?: string } | null)?.error ?? resp.statusText ?? "unknown";
    throw new Error(`${path}: ${msg}`);
  }
  if (expect === "array" && !Array.isArray(data)) {
    throw new Error(`${path}: expected array, got ${typeof data}`);
  }
  if (
    expect === "object" &&
    (data === null || typeof data !== "object" || Array.isArray(data))
  ) {
    throw new Error(
      `${path}: expected object, got ${Array.isArray(data) ? "array" : typeof data}`,
    );
  }
  return data as T;
}

// postDismiss sends the Migration screen's Unknown-group Dismiss action
// to the hub. Backend persistence lives in Task 2; GET /api/dismissed
// in Task 3. This
// is a thin wrapper so the screen code does not repeat fetch plumbing.
// Throws on non-204 responses with a descriptive message including the
// backend-provided error field when present.
export async function postDismiss(server: string): Promise<void> {
  const resp = await fetch("/api/dismiss", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ server }),
  });
  if (resp.status === 204) return;
  let body: { error?: string } | null = null;
  try {
    body = (await resp.json()) as { error?: string };
  } catch {
    // Non-JSON error body; fall through.
  }
  throw new Error(`/api/dismiss: ${body?.error ?? resp.statusText}`);
}

// postManifestCreate writes a new manifest via the A2a GUI pipeline. On
// success the backend returns 204; any non-2xx is surfaced as a thrown
// Error carrying the backend's {error} envelope text when present. Callers
// handle the "already exists" case by inspecting the error message — the
// backend currently returns "manifest \"<name>\" already exists at ..."
// verbatim, which is user-friendly enough to show in a banner.
export async function postManifestCreate(name: string, yaml: string): Promise<void> {
  const resp = await fetch("/api/manifest/create", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name, yaml }),
  });
  if (resp.status === 204) return;
  let body: { error?: string } | null = null;
  try {
    body = (await resp.json()) as { error?: string };
  } catch {
    // Non-JSON error body; fall through.
  }
  throw new Error(`/api/manifest/create: ${body?.error ?? resp.statusText}`);
}

// postManifestValidate returns the list of structural warnings produced by
// api.ManifestValidate. Empty array == valid. Throws on transport/HTTP error
// (not on validation warnings — those are normal return values).
export async function postManifestValidate(yaml: string): Promise<string[]> {
  const resp = await fetch("/api/manifest/validate", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ yaml }),
  });
  if (!resp.ok) {
    let body: { error?: string } | null = null;
    try {
      body = (await resp.json()) as { error?: string };
    } catch {
      // Non-JSON error body; fall through.
    }
    throw new Error(`/api/manifest/validate: ${body?.error ?? resp.statusText}`);
  }
  const payload = (await resp.json()) as { warnings?: string[] };
  return payload.warnings ?? [];
}

// getExtractManifest fetches the prefill YAML that populates AddServer's
// form when the user arrives via the A1 Migration Create-manifest button.
// Returns the raw YAML string. Throws on non-2xx with the backend error.
export async function getExtractManifest(client: string, server: string): Promise<string> {
  const url = `/api/extract-manifest?client=${encodeURIComponent(client)}&server=${encodeURIComponent(server)}`;
  const resp = await fetch(url);
  if (!resp.ok) {
    let body: { error?: string } | null = null;
    try {
      body = (await resp.json()) as { error?: string };
    } catch {
      // Non-JSON error body; fall through.
    }
    throw new Error(`/api/extract-manifest: ${body?.error ?? resp.statusText}`);
  }
  const payload = (await resp.json()) as { yaml?: string };
  return payload.yaml ?? "";
}

// postInitClientConfig drives the Servers matrix "Initialize <client>"
// affordance (v0.4.5). Posts a JSON body naming the client adapter
// whose empty stub should be seeded. Resolves to the structured
// {client, path, created} record on success; rejects with an
// `InitClientConfigError` carrying the backend's structured
// {error, code, status} envelope on failure so the matrix banner
// can render the operational code (PARENT_MISSING / UNKNOWN_CLIENT
// / INIT_FAILED) alongside the human-readable message.
//
// The caller (ServersScreen) should refresh /api/scan after a
// successful resolve so the now-present file flips
// client_config_presence to "ok" and the matrix column becomes
// active without a manual reload.
export interface InitClientConfigResult {
  client: string;
  path: string;
  created: boolean;
}

// InitClientConfigError preserves the backend's `code` field and the
// HTTP status so the operator-visible banner can surface the
// operational error code. Deep-sec Lane B (PR #208) follow-up: a
// plain Error swallowed `code`, leaving the banner with only the
// free-form `error` string — operators reading docs that reference
// `PARENT_MISSING` / `INIT_FAILED` had no way to map a banner back
// to those codes.
export class InitClientConfigError extends Error {
  readonly code: string;
  readonly status: number;
  constructor(message: string, code: string, status: number) {
    super(message);
    this.name = "InitClientConfigError";
    this.code = code;
    this.status = status;
  }
}

export async function postInitClientConfig(client: string): Promise<InitClientConfigResult> {
  const resp = await fetch("/api/init-client-config", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ client }),
  });
  if (resp.ok) {
    return (await resp.json()) as InitClientConfigResult;
  }
  let body: { error?: string; code?: string } | null = null;
  try {
    body = (await resp.json()) as { error?: string; code?: string };
  } catch {
    // Non-JSON error body; fall through.
  }
  const message = body?.error ?? resp.statusText ?? "unknown";
  const code = body?.code ?? `HTTP_${resp.status}`;
  throw new InitClientConfigError(
    `/api/init-client-config [${code}]: ${message}`,
    code,
    resp.status,
  );
}

// ───────────────────────────────────────────────────────────────────
// Servers-matrix revamp (Task 4.3) — daemon env overlay + discovery
// refresh + respawn + workspace list. Thin wrappers around the v0.5.x
// /api/daemon/env, /api/discovery/refresh, /api/daemon/respawn, and
// /api/workspaces handlers landed in Task 4.2.
//
// Error-shape policy: every backend writes {error, code, status?} on
// non-2xx via writeAPIError (internal/gui/server.go) — we surface the
// `code` so callers can branch on QUARANTINED, UNKNOWN_TASK, etc.
// without grepping the human-readable error string.
// ───────────────────────────────────────────────────────────────────

// DaemonRespawnError preserves the backend's `code` field and the HTTP
// status so the EnvDrawer can distinguish QUARANTINED (409 — show
// "force?" affordance) from UNKNOWN_TASK (400 — no retry path) from
// SUPERVISOR_UNAVAILABLE (503 — transient, retry later) without
// reading the error string.
export class DaemonRespawnError extends Error {
  readonly code: string;
  readonly status: number;
  constructor(message: string, code: string, status: number) {
    super(message);
    this.name = "DaemonRespawnError";
    this.code = code;
    this.status = status;
  }
}

// applyDaemonEnv posts {task_name, env} to /api/daemon/env which writes
// a `source: operator` row into daemon-env-overrides.yaml. Does NOT
// trigger a respawn — the operator clicks Restart separately so they
// can review the merged-effective env first (per spec §"Apply env edit
// from GUI").
//
// Throws Error on non-2xx; the message embeds the backend's code so
// the EnvDrawer can show INVALID_KEY / INVALID_VALUE / UNKNOWN_TASK
// distinctly. Validation echoes the backend regex
// `^[A-Za-z_][A-Za-z0-9_]*$` — keys with hyphens, leading digits, or
// control chars must be rejected client-side before they hit the wire.
export interface ApplyDaemonEnvResult {
  task_name: string;
  changed_keys: string[];
}

export interface DaemonEnvRow {
  task_name: string;
  server: string;
  daemon: string;
  workspace?: string;
  port?: number;
  source?: string;
  env: Record<string, string>;
}

export interface DaemonEnvListResponse {
  daemons: DaemonEnvRow[];
}

export async function listDaemonEnv(taskName?: string): Promise<DaemonEnvListResponse> {
  const suffix = taskName ? `?task_name=${encodeURIComponent(taskName)}` : "";
  return fetchOrThrow<DaemonEnvListResponse>(`/api/daemon/env${suffix}`, "object");
}

export async function applyDaemonEnv(
  taskName: string,
  env: Record<string, string>,
): Promise<ApplyDaemonEnvResult> {
  const resp = await fetch("/api/daemon/env", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ task_name: taskName, env }),
  });
  if (resp.ok) {
    return (await resp.json()) as ApplyDaemonEnvResult;
  }
  let body: { error?: string; code?: string } | null = null;
  try {
    body = (await resp.json()) as { error?: string; code?: string };
  } catch {
    // Non-JSON error body; fall through.
  }
  const message = body?.error ?? resp.statusText ?? "unknown";
  const code = body?.code ?? `HTTP_${resp.status}`;
  throw new Error(`/api/daemon/env [${code}]: ${message}`);
}

// refreshDiscovery posts an empty body to /api/discovery/refresh which
// re-runs binary_discovery against every installed manifest and
// overwrites non-operator overlay rows (source:auto-discovery rows are
// replaced; source:operator rows are preserved per the
// daemon-env-overlay-skipped-operator-override event).
export interface RefreshDiscoveryResult {
  manifest_count: number;
}

export async function refreshDiscovery(): Promise<RefreshDiscoveryResult> {
  const resp = await fetch("/api/discovery/refresh", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: "{}",
  });
  if (resp.ok) {
    return (await resp.json()) as RefreshDiscoveryResult;
  }
  let body: { error?: string; code?: string } | null = null;
  try {
    body = (await resp.json()) as { error?: string; code?: string };
  } catch {
    // Non-JSON error body; fall through.
  }
  const message = body?.error ?? resp.statusText ?? "unknown";
  const code = body?.code ?? `HTTP_${resp.status}`;
  throw new Error(`/api/discovery/refresh [${code}]: ${message}`);
}

// respawnDaemon posts {task_name, force} to /api/daemon/respawn which
// dials the supervisor IPC `respawn` verb. The supervisor performs a
// graceful 5 s shutdown → force-kill on timeout → spawn-with-overlay
// pipeline. Returns the new spawn state on success.
//
// HTTP-status → DaemonRespawnError.code mapping (Task 4.2 daemon_env.go):
//   400 UNKNOWN_TASK | INVALID_ARGS    — typo / stale UI state
//   409 QUARANTINED                    — supervisor refused; user must
//                                        send force=true to override
//   503 RESPAWN_NOT_READY |            — supervisor not yet listening;
//       SUPERVISOR_UNAVAILABLE           transient, retry after a few s
//   500 RESPAWN_FAILED | IPC_FAILED    — supervisor saw the request but
//                                        spawn returned an error
export interface RespawnDaemonResult {
  task_name: string;
  force: boolean;
  state: string;
}

export async function respawnDaemon(
  taskName: string,
  force: boolean = false,
): Promise<RespawnDaemonResult> {
  const resp = await fetch("/api/daemon/respawn", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ task_name: taskName, force }),
  });
  if (resp.ok) {
    return (await resp.json()) as RespawnDaemonResult;
  }
  let body: { error?: string; code?: string } | null = null;
  try {
    body = (await resp.json()) as { error?: string; code?: string };
  } catch {
    // Non-JSON error body; fall through.
  }
  const message = body?.error ?? resp.statusText ?? "unknown";
  const code = body?.code ?? `HTTP_${resp.status}`;
  throw new DaemonRespawnError(
    `/api/daemon/respawn [${code}]: ${message}`,
    code,
    resp.status,
  );
}

// WorkspacePair mirrors the deduplicated (key, path) entries from the
// /api/workspaces top-level `workspaces` array. The selector renders
// these directly; the matrix uses them to scope LSP rows.
export interface WorkspacePair {
  workspace_key: string;
  workspace_path: string;
}

// WorkspaceEntryDTO mirrors one (key, language) tuple from the registry.
// Carries the LSP task_name so the Servers screen can match a registered
// LSP to the row it should populate (vs the always-rendered placeholder).
export interface WorkspaceEntryDTO {
  workspace_key: string;
  workspace_path: string;
  language: string;
  backend: string;
  port: number;
  task_name: string;
  client_entries?: Record<string, string>;
  lifecycle?: string;
  last_error?: string;
}

export interface WorkspacesResponse {
  workspaces: WorkspacePair[];
  entries: WorkspaceEntryDTO[];
}

export async function listWorkspaces(): Promise<WorkspacesResponse> {
  return fetchOrThrow<WorkspacesResponse>("/api/workspaces", "object");
}

export interface LspRegisterResponse {
  workspace: string;
  workspace_key: string;
  entries: WorkspaceEntryDTO[];
  warnings?: string[];
  results?: Array<{
    language: string;
    status: "ok" | "error";
    error?: string;
  }>;
  error?: string;
  code?: string;
}

export async function postLspRegister(
  workspacePath: string,
  language: string,
): Promise<LspRegisterResponse> {
  return fetchOrThrow<LspRegisterResponse>("/api/lsp/register", "object", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ workspace_path: workspacePath, language }),
  });
}

// ───────────────────────────────────────────────────────────────────
// Dashboard recovery actions (PR #222 ops UX gap)
// ───────────────────────────────────────────────────────────────────

// CleanupOrphansResult mirrors gui.cleanupResponse — the wire shape of
// POST /api/cleanup/orphans. The handler returns both dry-run and
// apply outputs with the same envelope; `killed` / `skipped` stay
// zero on dry-run.
export interface CleanupOrphansResult {
  orphans: Array<{
    pid: number;
    name?: string;
    cmdline?: string;
    matched_server?: string;
    age_seconds?: number;
    kill_err?: string;
  }>;
  killed: number;
  skipped: number;
}

// cleanupOrphans posts to /api/cleanup/orphans with `apply: true`
// (kill mode). Returns the per-process kill outcome list so the UI
// can surface "killed N orphans" feedback.
export async function cleanupOrphans(): Promise<CleanupOrphansResult> {
  const resp = await fetch("/api/cleanup/orphans", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ apply: true }),
  });
  if (!resp.ok) {
    let body: { error?: string; code?: string } | null = null;
    try {
      body = (await resp.json()) as { error?: string; code?: string };
    } catch {
      // non-JSON
    }
    const msg = body?.error ?? resp.statusText ?? "unknown";
    const code = body?.code ?? `HTTP_${resp.status}`;
    throw new Error(`/api/cleanup/orphans [${code}]: ${msg}`);
  }
  return (await resp.json()) as CleanupOrphansResult;
}

// SupervisorRestartResult mirrors gui.supervisorRestartResponse: the
// per-step outcome of read-lock → kill → spawn. `per_step_error` is
// keyed by step name ("read_lock", "kill", "spawn") with empty/absent
// values meaning "step succeeded".
export interface SupervisorRestartResult {
  killed_pid: number;
  killed: boolean;
  spawned_pid: number;
  spawned: boolean;
  per_step_error?: Record<string, string>;
}

// restartSupervisor posts to /api/supervisor/restart. The handler
// always returns 200 — partial-failure cases are signalled via
// `spawned: false` + per_step_error keys so the UI can render a
// "kill ok, spawn failed: …" status without parsing a non-2xx body.
export async function restartSupervisor(): Promise<SupervisorRestartResult> {
  const resp = await fetch("/api/supervisor/restart", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
  });
  if (!resp.ok) {
    let body: { error?: string; code?: string } | null = null;
    try {
      body = (await resp.json()) as { error?: string; code?: string };
    } catch {
      // non-JSON
    }
    const msg = body?.error ?? resp.statusText ?? "unknown";
    const code = body?.code ?? `HTTP_${resp.status}`;
    throw new Error(`/api/supervisor/restart [${code}]: ${msg}`);
  }
  return (await resp.json()) as SupervisorRestartResult;
}

// ManifestHashMismatchError marks the stale-file-detection branch so
// the AddServer edit flow can show the [Reload]/[Force Save] banner
// instead of a generic error toast.
export class ManifestHashMismatchError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ManifestHashMismatchError";
  }
}

// getManifest reads the named manifest from disk and returns the YAML
// together with the SHA-256 content hash. The hash is stored in form
// state at Load and passed back to postManifestEdit as expected_hash.
export async function getManifest(name: string): Promise<{ yaml: string; hash: string }> {
  const resp = await fetch(`/api/manifest/get?name=${encodeURIComponent(name)}`);
  if (!resp.ok) {
    let body: { error?: string } | null = null;
    try {
      body = (await resp.json()) as { error?: string };
    } catch {
      // Non-JSON error body; fall through.
    }
    throw new Error(`/api/manifest/get: ${body?.error ?? resp.statusText}`);
  }
  const payload = (await resp.json()) as { yaml?: string; hash?: string };
  return { yaml: payload.yaml ?? "", hash: payload.hash ?? "" };
}

// postManifestEdit overwrites an existing manifest. expectedHash is the
// hash returned by getManifest at Load time; the backend rejects the
// write with 409 if the on-disk hash has since changed (external edit).
// Pass expectedHash === "" to skip the concurrency check (Force Save
// re-read path).
//
// Appendix P1-3 override: returns the new hash from the 200 response so
// runSave can refresh loadedHash atomically without a separate GET
// round-trip. Rejects 200 responses missing a non-empty hash (R3 safety:
// an empty hash would silently downgrade the next save's
// optimistic-concurrency check).
export async function postManifestEdit(
  name: string,
  yaml: string,
  expectedHash: string,
): Promise<{ hash: string }> {
  const resp = await fetch("/api/manifest/edit", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name, yaml, expected_hash: expectedHash }),
  });
  if (resp.ok) {
    const payload = (await resp.json()) as { hash?: string };
    // R3 correction: reject malformed success (empty/missing hash).
    // An empty returned hash would become loadedHash, and the next
    // edit call would send expected_hash="" — which the backend treats
    // as "skip optimistic concurrency". That silently drops stale
    // detection. Reject 200 without a non-empty hash.
    if (!payload.hash) {
      throw new Error("/api/manifest/edit: success response missing hash field");
    }
    return { hash: payload.hash };
  }
  let body: { error?: string; code?: string } | null = null;
  try {
    body = (await resp.json()) as { error?: string; code?: string };
  } catch {
    // Non-JSON error body; fall through.
  }
  if (resp.status === 409 && body?.code === "MANIFEST_HASH_MISMATCH") {
    throw new ManifestHashMismatchError(body.error ?? "hash mismatch");
  }
  throw new Error(`/api/manifest/edit: ${body?.error ?? resp.statusText}`);
}

// ───────────────────────────────────────────────────────────────────
// LSP trusted roots (Settings → Trusted Roots, PR #272 follow-up)
//
// Thin wrappers around /api/lsp/trusted-roots (GET/POST/DELETE). The
// store is the authorization boundary the GUI LSP router consults
// before first-touch auto-register; this lets the operator view / add /
// remove operator-config roots in the GUI instead of hand-editing
// <state-dir>/lsp-trusted-roots.json. Every method returns the fresh
// list so the caller can re-render without a follow-up GET.
// ───────────────────────────────────────────────────────────────────

export interface TrustedRootsResponse {
  roots: string[];
  path: string;
}

// normalizeTrustedRoots guarantees a non-null `roots` array and a string
// `path` regardless of what the wire carried. The production backend
// always sends both, but a defensive normalize keeps the UI from
// crashing on an older/partial body (consistent with fetchOrThrow's
// shape-guard philosophy).
function normalizeTrustedRoots(raw: Partial<TrustedRootsResponse>): TrustedRootsResponse {
  return {
    roots: Array.isArray(raw.roots) ? raw.roots : [],
    path: typeof raw.path === "string" ? raw.path : "",
  };
}

// getTrustedRoots reads the current operator-config trusted roots. An
// absent store yields an empty `roots` array (the backend maps a missing
// file to empty), so the empty-state render is the normal first-run path.
export async function getTrustedRoots(): Promise<TrustedRootsResponse> {
  return normalizeTrustedRoots(
    await fetchOrThrow<Partial<TrustedRootsResponse>>("/api/lsp/trusted-roots", "object"),
  );
}

// addTrustedRoot blesses `root` as a trusted root (POST). The backend
// rejects empty / relative paths with 400; the message embeds the
// backend code (LSP_TRUSTED_ROOTS_NOT_ABSOLUTE / _INVALID) so the
// caller can surface a precise validation error. Resolves to the
// refreshed list on success.
export async function addTrustedRoot(root: string): Promise<TrustedRootsResponse> {
  return normalizeTrustedRoots(
    await fetchOrThrow<Partial<TrustedRootsResponse>>("/api/lsp/trusted-roots", "object", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ root }),
    }),
  );
}

// removeTrustedRoot un-trusts `root` (DELETE). Removing an absent root
// is an idempotent no-op success server-side. Resolves to the refreshed
// list on success.
export async function removeTrustedRoot(root: string): Promise<TrustedRootsResponse> {
  return normalizeTrustedRoots(
    await fetchOrThrow<Partial<TrustedRootsResponse>>("/api/lsp/trusted-roots", "object", {
      method: "DELETE",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ root }),
    }),
  );
}
