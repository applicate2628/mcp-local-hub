import type { SettingsEnvelope, BackupInfo } from "./settings-types";

async function jsonOrThrow(res: Response): Promise<any> {
  const ct = res.headers.get("content-type") || "";
  let body: any = null;
  if (ct.includes("application/json")) {
    try {
      body = await res.json();
    } catch { /* fall through */ }
  }
  if (!res.ok) {
    const msg = body?.error || body?.reason || res.statusText || `HTTP ${res.status}`;
    const err: any = new Error(String(msg));
    err.status = res.status;
    err.body = body;
    throw err;
  }
  return body;
}

export async function getSettings(): Promise<SettingsEnvelope> {
  const res = await fetch("/api/settings", { credentials: "same-origin" });
  return await jsonOrThrow(res);
}

export async function putSetting(key: string, value: string): Promise<void> {
  const res = await fetch(`/api/settings/${encodeURIComponent(key)}`, {
    method: "PUT",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ value }),
  });
  await jsonOrThrow(res);
}

export async function postAction(key: string): Promise<any> {
  const res = await fetch(`/api/settings/${encodeURIComponent(key)}`, {
    method: "POST",
    credentials: "same-origin",
  });
  return await jsonOrThrow(res);
}

export async function getBackups(): Promise<BackupInfo[]> {
  const res = await fetch("/api/backups", { credentials: "same-origin" });
  const body = await jsonOrThrow(res);
  return body.backups as BackupInfo[];
}

export async function getBackupsCleanPreview(keepN: number): Promise<string[]> {
  const res = await fetch(`/api/backups/clean-preview?keep_n=${keepN}`, {
    credentials: "same-origin",
  });
  const body = await jsonOrThrow(res);
  return (body.would_remove ?? []) as string[];
}

// Bug-bash B2 closure (#21): per-client preview. Empty client falls
// back to the bulk preview above. The handler validates the client
// name and returns 400 BACKUPS_PREVIEW_UNKNOWN_CLIENT for unknown
// values; jsonOrThrow surfaces that as a typed error.
export async function getBackupsCleanPreviewForClient(
  client: string,
  keepN: number,
): Promise<string[]> {
  const qs = new URLSearchParams({ keep_n: String(keepN), client });
  const res = await fetch(`/api/backups/clean-preview?${qs.toString()}`, {
    credentials: "same-origin",
  });
  const body = await jsonOrThrow(res);
  return (body.would_remove ?? []) as string[];
}

// cleanBackups deletes eligible timestamped backups across ALL managed
// clients. When keepN is provided it is sent as ?keep_n=N so the clean
// honors the live slider draft (WYSIWYG with the preview) instead of the
// persisted setting; omit it to fall back to the persisted backups.keep_n.
export async function cleanBackups(keepN?: number): Promise<{ cleaned: number }> {
  const qs = keepN != null ? `?keep_n=${encodeURIComponent(String(keepN))}` : "";
  const res = await fetch(`/api/backups/clean${qs}`, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({}),
  });
  return await jsonOrThrow(res);
}

// Bug-bash B2 closure (#21): per-client clean. Same handler as
// cleanBackups but narrows the prune to one client via the ?client=X
// query param. The handler returns 400 BACKUPS_CLEAN_UNKNOWN_CLIENT
// for unknown client ids; jsonOrThrow surfaces that as a typed error.
export async function cleanBackupsForClient(
  client: string,
  keepN?: number,
): Promise<{ cleaned: number; client: string }> {
  const params: Record<string, string> = { client };
  // Send the live slider draft so the per-client clean matches its preview
  // (WYSIWYG). Omit to fall back to the persisted backups.keep_n.
  if (keepN != null) params.keep_n = String(keepN);
  const qs = new URLSearchParams(params);
  const res = await fetch(`/api/backups/clean?${qs.toString()}`, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({}),
  });
  return await jsonOrThrow(res);
}

// Per-timestamp backup actions (#2). Both POST a {client, path} body to a
// requireSameOrigin-guarded handler that re-validates the path belongs to
// the named client's config dir and matches the
// `.bak-mcp-local-hub-` naming convention server-side — the client never
// influences WHICH file is touched beyond naming one already shown in the
// list. restoreBackup overwrites the live config from the chosen backup
// (the server snapshots the current config first so the restore is
// undoable); deleteBackup removes one recognized backup file.
export async function restoreBackup(
  client: string,
  path: string,
): Promise<{ restored: string; client: string; snapshot: string }> {
  const res = await fetch("/api/backups/restore", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ client, path }),
  });
  return await jsonOrThrow(res);
}

export async function deleteBackup(
  client: string,
  path: string,
): Promise<{ deleted: string; client: string }> {
  const res = await fetch("/api/backups/delete", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ client, path }),
  });
  return await jsonOrThrow(res);
}

// Maintenance section helpers — Cleanup-5 per
// docs/superpowers/specs/2026-05-06-cleanup-buttons-design.md.
//
// Each cleanup endpoint accepts the same body shape: apply + filter
// flags. apply=false (or omitted) lists candidates without killing
// (dry-run); apply=true returns the same list with kill_err populated
// for any per-PID failures and counters for the summary.
//
// Codex Cloud bot P2 on PR #131 / kosyak
// 2026-05-07-destructive-endpoint-with-unsafe-zero-value-default.md:
// the prior wire used `dry_run` whose Go zero-value (false) inverted
// safety polarity — `{}` body triggered the kill path. Switched to
// `apply` so the zero-value path is safe.

// Cleanup-6: the GUI sees `cmdline_display` (executable basename only) on
// the wire. The legacy `cmdline` field carried the full command line —
// workspace paths, username segments, possible API keys/tokens in args —
// and is now `json:"-"` on the Go struct. Keep `cmdline` typed as
// optional for backward compatibility with any in-flight test fixtures
// that still set it, but production code MUST consume `cmdline_display`.
export type OrphanProcess = {
  pid: number;
  parent_pid: number;
  server: string;
  cmdline_display: string;
  /** @deprecated Cleanup-6: server no longer emits this; use cmdline_display. Test-fixture compatibility only. */
  cmdline?: string;
  age_sec: number;
  ram_bytes: number;
  kill_err?: string;
  /**
   * match_source explains why an AGGRESSIVE candidate was included: the
   * ancestor basename that anchored the scope (e.g. "codex") for a
   * --client run, or "root-pid <pid>" for a --root-pid run. Empty for
   * the default safe sweep (POST /api/cleanup/orphans). Redacted basename
   * / fixed label only — never a full cmdline — so it is wire-safe.
   */
  match_source?: string;
};

export type LogWatcher = {
  pid: number;
  parent_pid: number;
  parent_alive: boolean;
  name: string;
  age_sec: number;
  cmdline: string;
  kill_err?: string;
};

// CleanupOrphansResponse is the single owner of the POST /api/cleanup/orphans
// envelope shape. src/api.ts re-exports it as `CleanupOrphansResult` (its
// Dashboard-facing cleanupOrphans() wrapper returns that alias), so this type
// is the one place to change if the wire shape changes — deep-review r2 P4.
export type CleanupOrphansResponse = {
  orphans: OrphanProcess[];
  killed: number;
  skipped: number;
};

// AggressiveCleanupResponse extends the orphan response with the confirm
// `token` (bot #373 R2 Finding 1). The dry-run carries a token bound to
// the previewed candidate set; the apply replays it. A `code` field is
// present ONLY on the 409 token-mismatch body
// (CLEANUP_AGGRESSIVE_TOKEN_MISMATCH), where `orphans` is the fresh
// candidate set and `token` is the new token over it.
export type AggressiveCleanupResponse = CleanupOrphansResponse & {
  token?: string;
  code?: string;
};

// CLEANUP_AGGRESSIVE_TOKEN_MISMATCH is the stable code the backend returns
// in the 409 body when the candidate set drifted between Preview and the
// apply (the previewed token no longer matches the freshly-recomputed
// set). The GUI keys its "re-Preview" reset on this exact string.
export const CLEANUP_AGGRESSIVE_TOKEN_MISMATCH = "CLEANUP_AGGRESSIVE_TOKEN_MISMATCH";

export type CleanupLogWatchersResponse = {
  watchers: LogWatcher[];
  killed: number;
  skipped: number;
};

export async function cleanupOrphans(apply: boolean): Promise<CleanupOrphansResponse> {
  const res = await fetch("/api/cleanup/orphans", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ apply }),
  });
  return await jsonOrThrow(res);
}

export async function cleanupLogWatchers(
  apply: boolean,
  includeLive: boolean,
): Promise<CleanupLogWatchersResponse> {
  const res = await fetch("/api/cleanup/log-watchers", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ apply, include_live: includeLive }),
  });
  return await jsonOrThrow(res);
}

// AggressiveCleanupScope narrows the aggressive sweep to exactly one of:
//   - by client launcher basename (claude / codex / gemini / qwen /
//     cursor / code / cascade / antigravity), or
//   - by an explicit root PID whose descendants are swept.
// The backend (POST /api/cleanup/aggressive) requires exactly one set;
// the GUI enforces "one scope chosen" before enabling Preview.
export type AggressiveCleanupScope =
  | { kind: "client"; client: string }
  | { kind: "root-pid"; rootPid: number };

// cleanupAggressive wraps POST /api/cleanup/aggressive — the
// operator-confirmed override that kills the live-rooted MCP-stdio
// fan-out the default safe sweep (cleanupOrphans) correctly refuses to
// touch. apply=false previews (dry-run); apply=true kills. includeClasses
// opts default-excluded dangerous classes (cmd/conhost/pwsh/powershell/
// chrome) back into the kill set. Returns the same shape as
// cleanupOrphans plus a confirm `token`; the candidates carry match_source
// explaining inclusion.
//
// Token contract (bot #373 R2 Finding 1): the dry-run response carries a
// `token` bound to the previewed candidate set. The apply MUST replay
// that token; the backend recomputes the CURRENT candidate set and refuses
// with HTTP 409 + code CLEANUP_AGGRESSIVE_TOKEN_MISMATCH (fresh candidates
// + new token in the body) if the set drifted since the preview. An empty
// token on apply is a 400. jsonOrThrow throws on a non-2xx response with
// `err.status` + `err.body` attached, so the caller inspects `err.status
// === 409` / `err.body` to surface the drift and require a fresh Preview.
export async function cleanupAggressive(
  apply: boolean,
  scope: AggressiveCleanupScope,
  includeClasses: string[] = [],
  token?: string,
): Promise<AggressiveCleanupResponse> {
  const body: Record<string, unknown> = { apply, include_classes: includeClasses };
  if (scope.kind === "client") {
    body.client = scope.client;
  } else {
    body.root_pid = scope.rootPid;
  }
  // Only attach the token on apply — the dry-run ignores it server-side.
  if (apply && token !== undefined) {
    body.token = token;
  }
  const res = await fetch("/api/cleanup/aggressive", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  return await jsonOrThrow(res);
}

// stopAll wraps /api/stop-all (existing route in
// internal/gui/servers.go) so the Maintenance "Stop all daemons"
// button has a single import surface alongside the cleanup helpers.
//
// /api/stop-all returns HTTP 207 + per-daemon stop_results[*] on
// partial failure, HTTP 200 on full success. The per-row shape is
// `api.RestartResult` (internal/api/install.go:1623) which has JSON
// tags `task_name` and `error` — NOT `name` and `err`.
//
// Codex Cloud bot P1 on PR #131 / kosyak
// `2026-05-06-third-time-shipped-without-checking-json-tags.md`: the
// prior "fix" used {name, err} from imagination; sr.err was always
// undefined, partial failures rendered as "Stopped all". Field names
// here are now verified against the Go struct's actual json tags.
export type StopResult = { task_name: string; error: string };
export type StopAllResponse = { stop_results: StopResult[] };

export async function stopAllDaemons(): Promise<StopAllResponse> {
  const res = await fetch("/api/stop-all", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({}),
  });
  return (await jsonOrThrow(res)) as StopAllResponse;
}

// forceKillProbe / forceKillApply wrap the existing /api/force-kill/*
// routes (internal/gui/force_kill.go). Probe is read-only and returns
// the C1 Verdict struct as JSON; Apply runs the 3-part identity gate
// and returns the post-kill Verdict.
export async function forceKillProbe(): Promise<unknown> {
  const res = await fetch("/api/force-kill/probe", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({}),
  });
  return await jsonOrThrow(res);
}

export async function forceKillApply(): Promise<unknown> {
  const res = await fetch("/api/force-kill", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({}),
  });
  return await jsonOrThrow(res);
}
