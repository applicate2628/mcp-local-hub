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

export async function cleanBackups(): Promise<{ cleaned: number }> {
  const res = await fetch("/api/backups/clean", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({}),
  });
  return await jsonOrThrow(res);
}

// Maintenance section helpers — Cleanup-5 per
// docs/superpowers/specs/2026-05-06-cleanup-buttons-design.md.
//
// Each cleanup endpoint accepts the same body shape: dry_run + filter
// flags. Dry-run mode returns the candidate list without calling kill;
// apply mode (dry_run=false) returns the same list with kill_err
// populated for any per-PID failures and counters for the summary.

export type OrphanProcess = {
  pid: number;
  parent_pid: number;
  server: string;
  cmdline: string;
  age_sec: number;
  ram_bytes: number;
  kill_err?: string;
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

export type CleanupOrphansResponse = {
  orphans: OrphanProcess[];
  killed: number;
  skipped: number;
};

export type CleanupLogWatchersResponse = {
  watchers: LogWatcher[];
  killed: number;
  skipped: number;
};

export async function cleanupOrphans(dryRun: boolean): Promise<CleanupOrphansResponse> {
  const res = await fetch("/api/cleanup/orphans", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ dry_run: dryRun }),
  });
  return await jsonOrThrow(res);
}

export async function cleanupLogWatchers(
  dryRun: boolean,
  includeLive: boolean,
): Promise<CleanupLogWatchersResponse> {
  const res = await fetch("/api/cleanup/log-watchers", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ dry_run: dryRun, include_live: includeLive }),
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
