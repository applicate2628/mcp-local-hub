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
