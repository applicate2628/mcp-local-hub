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

// GuiRestartResult mirrors gui.guiSelfRestartResponse: the outcome of
// the GUI self-restart handoff. `restarting: true` means the replacement
// `mcphub gui` child was spawned and the current process is about to exit
// to hand off the single-instance lock — the browser tab will lose its
// connection momentarily, then the child's listener (same port) answers.
// `spawned: false` + `spawn_error` means the child did NOT launch and the
// current GUI keeps running, so the operator sees the error rather than
// losing the GUI.
export interface GuiRestartResult {
  spawned: boolean;
  spawned_pid: number;
  spawn_error?: string;
  restarting: boolean;
}

// restartGui posts to /api/gui/restart. On success the backend spawns a
// replacement GUI process and then exits the current one to release the
// single-instance lock, so the HTTP response may be the LAST byte this
// listener sends before the connection drops — callers must treat a
// post-200 fetch/network error as the expected handoff, not a failure.
export async function restartGui(): Promise<GuiRestartResult> {
  const resp = await fetch("/api/gui/restart", {
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
    throw new Error(`/api/gui/restart [${code}]: ${msg}`);
  }
  return (await resp.json()) as GuiRestartResult;
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

// ───────────────────────────────────────────────────────────────────
// Default-install client set override (Settings → Clients panel)
//
// Thin wrappers around /api/client-install-prefs (GET/POST). The override
// lets the operator pick which clients are in the default-install set (the
// compile-time {claude-code, codex-cli, cursor} trio plus opt-ins) without
// CLI flags; the chosen set is persisted to gui-preferences.yaml and becomes
// the effective default for installs that do not name an explicit --clients
// target. Both methods return the fresh snapshot so the caller re-renders
// without a follow-up GET.
// ───────────────────────────────────────────────────────────────────

export interface ClientInstallPrefRow {
  name: string;
  // True when the compile-time registry marks this client default-install
  // (the canonical {claude-code, codex-cli, cursor} trio).
  compile_default: boolean;
  // True when this client is in the currently-effective default-install set.
  selected: boolean;
}

export interface ClientInstallPrefsResponse {
  clients: ClientInstallPrefRow[];
  // True when an explicit operator override is persisted (vs. the
  // compile-time trio fallback).
  override_active: boolean;
}

// normalizeClientInstallPrefs guarantees a non-null `clients` array and a
// boolean `override_active` regardless of what the wire carried — defensive
// against an older/partial backend body (consistent with
// normalizeTrustedRoots' philosophy).
function normalizeClientInstallPrefs(
  raw: Partial<ClientInstallPrefsResponse>,
): ClientInstallPrefsResponse {
  return {
    clients: Array.isArray(raw.clients)
      ? raw.clients.map((c) => ({
          name: typeof c?.name === "string" ? c.name : "",
          compile_default: c?.compile_default === true,
          selected: c?.selected === true,
        }))
      : [],
    override_active: raw.override_active === true,
  };
}

// getClientInstallPrefs reads the current default-install client set. An
// absent gui-preferences.yaml yields the compile-time trio selected with
// override_active=false — the normal first-run path.
export async function getClientInstallPrefs(): Promise<ClientInstallPrefsResponse> {
  return normalizeClientInstallPrefs(
    await fetchOrThrow<Partial<ClientInstallPrefsResponse>>("/api/client-install-prefs", "object"),
  );
}

// setClientInstallPrefs persists `clients` as the default-install set (POST).
// The backend rejects an empty set or an unknown client name with 400; the
// message embeds the backend code (CLIENT_INSTALL_PREFS_INVALID) so the
// caller can surface a precise validation error. Resolves to the refreshed
// snapshot on success.
export async function setClientInstallPrefs(
  clients: string[],
): Promise<ClientInstallPrefsResponse> {
  return normalizeClientInstallPrefs(
    await fetchOrThrow<Partial<ClientInstallPrefsResponse>>("/api/client-install-prefs", "object", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ clients }),
    }),
  );
}

// Path validation for the Settings "Browse…" affordance (TypePath fields
// such as appearance.default_home once it is wired).
//
// The GUI is a browser UI served by a headless HTTP server; the actual UI
// is a separate Chrome --app window. A native OS folder dialog spawned
// from the server process surfaces unreliably (no parent-window
// relationship to the browser, would block a request goroutine), so the
// Browse affordance is a plain path text field with live exists/is-dir
// feedback driven by GET /api/path/validate. This wrapper hits that
// read-only endpoint. A non-existent path is a NORMAL result
// (exists:false), not a thrown error — the operator is mid-typing.
// ───────────────────────────────────────────────────────────────────

export interface PathValidateResult {
  path: string;
  exists: boolean;
  is_dir: boolean;
  // Set only when the path could not be checked for a reason OTHER than
  // "does not exist" (e.g. permission denied). A plain not-found leaves
  // this empty.
  error?: string;
}

// validatePath GETs /api/path/validate?path=<p>. Resolves to the
// {exists, is_dir} record for any syntactically-valid path (including
// non-existent ones). Rejects (throws) only on a transport/HTTP error
// such as 400 PATH_REQUIRED / PATH_INVALID (empty or control-char path)
// — the caller should debounce input so these are rare — surfacing the
// backend {error, code} envelope.
export async function validatePath(path: string): Promise<PathValidateResult> {
  const resp = await fetch(`/api/path/validate?path=${encodeURIComponent(path)}`);
  if (!resp.ok) {
    let body: { error?: string; code?: string } | null = null;
    try {
      body = (await resp.json()) as { error?: string; code?: string };
    } catch {
      // Non-JSON error body; fall through.
    }
    const msg = body?.error ?? resp.statusText ?? "unknown";
    const code = body?.code ?? `HTTP_${resp.status}`;
    throw new Error(`/api/path/validate [${code}]: ${msg}`);
  }
  const body = (await resp.json()) as Partial<PathValidateResult>;
  return {
    path: typeof body.path === "string" ? body.path : path,
    exists: body.exists === true,
    is_dir: body.is_dir === true,
    error: typeof body.error === "string" ? body.error : undefined,
  };
}

// ───────────────────────────────────────────────────────────────────
// Marketplace one-click install (roadmap §B #1, frontend slice S3)
//
// POST /api/marketplace/install drives the Store "one-click install" UI.
// It is intentionally a hand-rolled fetch (not fetchOrThrow) because the
// success and conflict shapes differ across HTTP statuses the UI must
// branch on, and fetchOrThrow's single-shape guard would collapse them:
//
//   mode "hub":    201 { name, port, mode } on success;
//                  409 { error_code:"NAME_CONFLICT", suggested_name } on a
//                      name collision (the UI offers a one-click retry under
//                      suggested_name).
//   mode "direct": 200 { clients_updated, clients_failed:[], mode } all-ok;
//                  207 { clients_updated, clients_failed:[{client,error}],
//                      mode } partial (some client writes failed).
//
// The handler is requireSameOrigin-guarded like the other mutating /api/*
// routes; the fetch below is same-origin (relative path, default credentials)
// so it satisfies the guard exactly as the other POST helpers do.
// ───────────────────────────────────────────────────────────────────

// MarketplaceInstallMode is the two-tier install lane. "hub" registers a
// shared hub daemon every client routes to (the process-tail-compression
// path, valid for every transport). "direct" writes the remote URL straight
// into the chosen client configs with no hub daemon — http transport only.
export type MarketplaceInstallMode = "hub" | "direct";

// MarketplaceInstallRequest is the POST body. name/port are optional hub-mode
// overrides (name carries the suggested-name retry); clients is the required
// direct-mode target list.
export interface MarketplaceInstallRequest {
  id: string;
  mode: MarketplaceInstallMode;
  name?: string;
  port?: number;
  clients?: string[];
}

// The discriminated result the UI branches on. `kind` distinguishes the four
// terminal outcomes the handler can return so the screen never has to re-read
// HTTP status codes:
//
//   "hub-installed"  → 201: hub daemon created (name + resolved port).
//   "name-conflict"  → 409: a server with this name exists; suggested_name is
//                      the "-2" variant the UI offers as a one-click retry.
//   "direct"         → 200/207: per-client updated / failed split. `partial`
//                      is true on 207 (some client writes failed).
export type MarketplaceInstallResult =
  | { kind: "hub-installed"; name: string; port: number }
  | { kind: "name-conflict"; suggestedName: string }
  | {
      kind: "direct";
      partial: boolean;
      clientsUpdated: string[];
      clientsFailed: Array<{ client: string; error: string }>;
    };

// installMarketplaceEntry POSTs the install request and resolves to the
// discriminated result above. It rejects (throws Error) only on a genuine
// transport/HTTP failure that is NOT one of the modelled success/conflict
// shapes — e.g. 400 BAD_ENTRY, 404 ENTRY_NOT_FOUND, 502 CATALOG_UNAVAILABLE,
// 500 INSTALL_FAILED — surfacing the backend's {error, code} envelope so the
// row can render an inline error. The 409 NAME_CONFLICT is modelled as a
// RESULT (not a throw) because it is an expected, recoverable branch.
export async function installMarketplaceEntry(
  req: MarketplaceInstallRequest,
): Promise<MarketplaceInstallResult> {
  const resp = await fetch("/api/marketplace/install", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });

  // 409 NAME_CONFLICT is an expected, recoverable branch — surface the
  // suggested name as a result, not an error.
  if (resp.status === 409) {
    let body: { error_code?: string; suggested_name?: string } | null = null;
    try {
      body = (await resp.json()) as { error_code?: string; suggested_name?: string };
    } catch {
      // Non-JSON 409 body; fall through to a generic error.
    }
    if (body?.error_code === "NAME_CONFLICT" && body.suggested_name) {
      return { kind: "name-conflict", suggestedName: body.suggested_name };
    }
    throw new Error(`/api/marketplace/install: name conflict (no suggested name)`);
  }

  if (!resp.ok) {
    let body: { error?: string; code?: string } | null = null;
    try {
      body = (await resp.json()) as { error?: string; code?: string };
    } catch {
      // Non-JSON error body; fall through.
    }
    const msg = body?.error ?? resp.statusText ?? "install failed";
    const code = body?.code ?? `HTTP_${resp.status}`;
    throw new Error(`/api/marketplace/install [${code}]: ${msg}`);
  }

  if (req.mode === "hub") {
    // 201 hub success: { name, port, mode }.
    const body = (await resp.json()) as { name?: string; port?: number };
    return {
      kind: "hub-installed",
      name: body.name ?? req.name ?? req.id,
      port: typeof body.port === "number" ? body.port : 0,
    };
  }

  // Direct mode: 200 all-ok or 207 partial. Either way the body carries the
  // per-client split; 207 is the only signal of partial failure.
  const body = (await resp.json()) as {
    clients_updated?: string[];
    clients_failed?: Array<{ client: string; error: string }>;
  };
  return {
    kind: "direct",
    partial: resp.status === 207,
    clientsUpdated: Array.isArray(body.clients_updated) ? body.clients_updated : [],
    clientsFailed: Array.isArray(body.clients_failed) ? body.clients_failed : [],
  };
}

// ───────────────────────────────────────────────────────────────────
// Marketplace force-refresh (roadmap §B — Catalog "Refresh" button)
//
// POST /api/marketplace/refresh forces an unconditional re-fetch of the
// curated registry (bypassing the 24h TTL + ETag, rewriting the cache) —
// the SAME force-refresh the CLI `mcphub marketplace refresh` runs — and
// returns the refreshed entries in the SAME body shape as GET
// /api/marketplace ({ "entries": [{id, name, summary, …, transport}] }).
//
// Unlike the best-effort GET (which the backend degrades to an empty array
// on a fetch miss), the refresh route is an EXPLICIT operator-triggered
// re-fetch: the backend surfaces a refresh failure as 500
// {error, code:"MARKETPLACE_REFRESH_FAILED"} so the operator knows the cache
// was NOT updated. We therefore throw on non-2xx (surfacing the envelope)
// rather than silently returning an empty list.
//
// The shape mirrors internal/gui/marketplace.go marketplaceEntry. `transport`
// is normalized to a string so an older backend that omits it (or a partial
// body) reads as "" and the caller falls back to the safe HUB-ONLY affordance,
// matching the GET-path normalization in Catalog.tsx.
// ───────────────────────────────────────────────────────────────────

export interface MarketplaceCatalogEntry {
  id: string;
  name: string;
  summary: string;
  categories: string[];
  homepage: string;
  transport: string;
}

export async function refreshMarketplace(): Promise<MarketplaceCatalogEntry[]> {
  const resp = await fetch("/api/marketplace/refresh", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
  });
  if (!resp.ok) {
    let body: { error?: string; code?: string } | null = null;
    try {
      body = (await resp.json()) as { error?: string; code?: string };
    } catch {
      // Non-JSON error body; fall through.
    }
    const msg = body?.error ?? resp.statusText ?? "refresh failed";
    const code = body?.code ?? `HTTP_${resp.status}`;
    throw new Error(`/api/marketplace/refresh [${code}]: ${msg}`);
  }
  const body = (await resp.json()) as { entries?: Array<Partial<MarketplaceCatalogEntry>> };
  const rows = Array.isArray(body.entries) ? body.entries : [];
  return rows.map((e) => ({
    id: typeof e.id === "string" ? e.id : "",
    name: typeof e.name === "string" ? e.name : "",
    summary: typeof e.summary === "string" ? e.summary : "",
    categories: Array.isArray(e.categories) ? e.categories : [],
    homepage: typeof e.homepage === "string" ? e.homepage : "",
    transport: typeof e.transport === "string" ? e.transport : "",
  }));
}

// ───────────────────────────────────────────────────────────────────
// Groups / namespaces (Groups screen, groups Phase 5b-2)
//
// A group is a NAMED subset of MCP servers exposed at /g/<group>/mcp with
// optional per-server hidden tools — the tool-context-bloat fix (decision
// work-items/decisions/2026-06-18-groups-namespaces-tool-visibility.md).
// Thin wrappers around /api/groups (GET list + POST upsert + DELETE), the
// thin HTTP wrapper over the api data layer (internal/gui/groups.go).
//
// Error-shape policy: the POST/DELETE handlers surface validation errors
// via the {error, code} envelope (GROUPS_INVALID_NAME / _UNKNOWN_SERVER /
// _HIDDEN_NONMEMBER / _INVALID_JSON / _NAME_REQUIRED / _NOT_FOUND). We
// preserve `code` on a typed GroupsApiError (mirroring InitClientConfigError)
// so the Groups screen can map a 400 to a precise inline field error rather
// than grepping the human-readable string.
// ───────────────────────────────────────────────────────────────────

// GroupDTO mirrors groupDTO in internal/gui/groups.go. Servers is always a
// non-nil array on the wire; tools_hidden is omitted by the backend when
// empty (omitempty) — we normalize it to {} so callers never branch on
// undefined.
export interface GroupDTO {
  name: string;
  description?: string;
  servers: string[];
  tools_hidden?: Record<string, string[]>;
  // connection is populated only on the GET list path (B4). It carries the
  // copy-pasteable /g/<group>/mcp connection triple when the gate-ON hub is
  // live, else a not-available placeholder with a hint.
  connection?: GroupConnectionDTO;
}

// GroupConnectionDTO mirrors groupConnectionDTO in internal/gui/groups.go. When
// available is true the url + token + instance_id are the values a client uses
// (the URL plus X-Mcphub-Hub-Token and X-Mcphub-Instance-Id headers). When
// false, the hub is gate-OFF / not bound and hint explains how to bring it up —
// the backend deliberately omits url/token in that case so the GUI never shows
// a dead URL with a live token. localhost same-origin only.
export interface GroupConnectionDTO {
  available: boolean;
  url?: string;
  instance_id?: string;
  token?: string;
  hint?: string;
}

// GroupsListResponse mirrors groupsListResponse — the GET /api/groups body.
// Both arrays are always non-nil (the backend guarantees it); the normalize
// helper defends against an older/partial body anyway.
export interface GroupsListResponse {
  groups: GroupDTO[];
  available_servers: string[];
}

// GroupMutationResponse mirrors groupMutationResponse — the POST/DELETE body.
// restart_required is true when the write persisted but the live hub could
// NOT be re-published in-place (gate-OFF or hub not live); the screen shows a
// "restart the hub to apply" notice in that case. hub_live reports whether
// the gate-ON hub listener was live at mutation time so the banner can be
// worded precisely.
export interface GroupMutationResponse {
  group?: GroupDTO;
  restart_required: boolean;
  hub_live: boolean;
}

// SaveGroupBody is the POST /api/groups upsert body shape.
export interface SaveGroupBody {
  name: string;
  description?: string;
  servers: string[];
  tools_hidden?: Record<string, string[]>;
}

// GroupsApiError preserves the backend's `code` field and the HTTP status so
// the Groups screen can surface the operational error code (and map it to the
// offending field) without parsing the free-form `error` string.
export class GroupsApiError extends Error {
  readonly code: string;
  readonly status: number;
  constructor(message: string, code: string, status: number) {
    super(message);
    this.name = "GroupsApiError";
    this.code = code;
    this.status = status;
  }
}

// normalizeGroup guarantees a non-null `servers` array and a normalized
// `tools_hidden` map regardless of what the wire carried — defensive against
// an older/partial backend body (consistent with normalizeTrustedRoots).
function normalizeGroup(raw: Partial<GroupDTO>): GroupDTO {
  const hidden: Record<string, string[]> = {};
  if (raw.tools_hidden && typeof raw.tools_hidden === "object") {
    for (const [srv, tools] of Object.entries(raw.tools_hidden)) {
      hidden[srv] = Array.isArray(tools) ? tools.filter((t) => typeof t === "string") : [];
    }
  }
  const out: GroupDTO = {
    name: typeof raw.name === "string" ? raw.name : "",
    description: typeof raw.description === "string" ? raw.description : "",
    servers: Array.isArray(raw.servers) ? raw.servers.filter((s) => typeof s === "string") : [],
    tools_hidden: hidden,
  };
  // connection (B4) is present only on the GET list path. Carry it through
  // defensively (the backend omits url/token unless available is true).
  if (raw.connection && typeof raw.connection === "object") {
    const c = raw.connection;
    out.connection = {
      available: c.available === true,
      url: typeof c.url === "string" ? c.url : undefined,
      instance_id: typeof c.instance_id === "string" ? c.instance_id : undefined,
      token: typeof c.token === "string" ? c.token : undefined,
      hint: typeof c.hint === "string" ? c.hint : undefined,
    };
  }
  return out;
}

// getGroups reads every group from groups.yaml plus the available server
// names a picker offers. An absent groups.yaml yields an empty `groups`
// array — the normal first-run path.
export async function getGroups(): Promise<GroupsListResponse> {
  const raw = await fetchOrThrow<Partial<GroupsListResponse>>("/api/groups", "object");
  return {
    groups: Array.isArray(raw.groups) ? raw.groups.map(normalizeGroup) : [],
    available_servers: Array.isArray(raw.available_servers)
      ? raw.available_servers.filter((s) => typeof s === "string")
      : [],
  };
}

// readMutationError pulls the {error, code} envelope off a non-2xx response
// and throws a GroupsApiError carrying the operational code. Shared by
// saveGroup + deleteGroup so both surface the same typed error.
async function readMutationError(path: string, resp: Response): Promise<never> {
  let body: { error?: string; code?: string } | null = null;
  try {
    body = (await resp.json()) as { error?: string; code?: string };
  } catch {
    // Non-JSON error body; fall through.
  }
  const msg = body?.error ?? resp.statusText ?? "unknown";
  const code = body?.code ?? `HTTP_${resp.status}`;
  throw new GroupsApiError(`${path} [${code}]: ${msg}`, code, resp.status);
}

// saveGroup create-or-updates ONE group (POST). The backend rejects an
// invalid name (GROUPS_INVALID_NAME), an unknown server (GROUPS_UNKNOWN_SERVER),
// or a tools_hidden key naming a non-member server (GROUPS_HIDDEN_NONMEMBER)
// with 400; the thrown GroupsApiError carries the code so the screen can map
// it to the offending field. Resolves to the mutation response (the updated
// group + restart_required/hub_live) on success.
export async function saveGroup(body: SaveGroupBody): Promise<GroupMutationResponse> {
  const resp = await fetch("/api/groups", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!resp.ok) {
    return readMutationError("/api/groups", resp);
  }
  const raw = (await resp.json()) as Partial<GroupMutationResponse>;
  return {
    group: raw.group ? normalizeGroup(raw.group) : undefined,
    restart_required: raw.restart_required === true,
    hub_live: raw.hub_live === true,
  };
}

// deleteGroup removes one group by name (DELETE). The backend rejects an
// empty name with 400 GROUPS_NAME_REQUIRED and an absent group with 404
// GROUPS_NOT_FOUND; both surface as a GroupsApiError carrying the code.
// Resolves to the mutation response (restart_required/hub_live; group absent)
// on success.
export async function deleteGroup(name: string): Promise<GroupMutationResponse> {
  const resp = await fetch(`/api/groups?name=${encodeURIComponent(name)}`, {
    method: "DELETE",
    headers: { "Content-Type": "application/json" },
  });
  if (!resp.ok) {
    return readMutationError("/api/groups", resp);
  }
  const raw = (await resp.json()) as Partial<GroupMutationResponse>;
  return {
    group: raw.group ? normalizeGroup(raw.group) : undefined,
    restart_required: raw.restart_required === true,
    hub_live: raw.hub_live === true,
  };
}
