// internal/gui/frontend/src/lib/secrets-api.ts

export type VaultState = "ok" | "missing" | "decrypt_failed" | "corrupt";

export interface UsageRef {
  server: string;
  env_var: string;
}

export interface ManifestError {
  name?: string;
  path: string;
  error: string;
}

export interface SecretRow {
  name: string;
  state: "present" | "referenced_missing" | "referenced_unverified";
  used_by: UsageRef[];
}

export interface SecretsEnvelope {
  vault_state: VaultState;
  secrets: SecretRow[];
  manifest_errors: ManifestError[];
}

export interface RestartResult {
  task_name: string;
  error: string;
}

export interface SecretsRotateResult {
  vault_updated: boolean;
  restart_results: RestartResult[];
}

export interface APIError extends Error {
  code?: string;
  status: number;
  body: unknown;
}

function makeAPIError(status: number, code: string | undefined, message: string, body: unknown): APIError {
  const err = new Error(message) as APIError;
  err.code = code;
  err.status = status;
  err.body = body;
  return err;
}

async function parseJSONBody(resp: Response): Promise<any> {
  try {
    return await resp.json();
  } catch {
    return {};
  }
}

export async function secretsInit(): Promise<{ vault_state?: string; cleanup_status?: string; orphan_path?: string; error?: string; code?: string }> {
  const resp = await fetch("/api/secrets/init", { method: "POST" });
  const body = await parseJSONBody(resp);
  if (resp.status === 200) return body;
  throw makeAPIError(resp.status, body.code, body.error ?? `init failed: ${resp.status}`, body);
}

export async function getSecrets(): Promise<SecretsEnvelope> {
  const resp = await fetch("/api/secrets");
  const body = await parseJSONBody(resp);
  if (!resp.ok) throw makeAPIError(resp.status, body.code, body.error ?? `list failed: ${resp.status}`, body);
  return body as SecretsEnvelope;
}

export async function addSecret(name: string, value: string): Promise<void> {
  const resp = await fetch("/api/secrets", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name, value }),
  });
  if (resp.status === 201) return;
  const body = await parseJSONBody(resp);
  throw makeAPIError(resp.status, body.code, body.error ?? `add failed: ${resp.status}`, body);
}

export async function rotateSecret(name: string, value: string, restart: boolean): Promise<SecretsRotateResult> {
  const resp = await fetch(`/api/secrets/${encodeURIComponent(name)}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ value, restart }),
  });
  const body = await parseJSONBody(resp);
  // 200 + 207: success (with or without per-task restart failures).
  if (resp.status === 200 || resp.status === 207) return body as SecretsRotateResult;
  // 500 RESTART_FAILED with vault_updated:true is a committed-vault +
  // orchestration-aborted path (memo §5.4). The vault is already
  // updated; the modal should close so the UI's banner can render the
  // partial state with retry. Throwing here would keep the modal open
  // and prompt the user to re-rotate, double-writing the new value.
  if (resp.status === 500 && body.vault_updated === true && body.code === "RESTART_FAILED") {
    return body as SecretsRotateResult;
  }
  throw makeAPIError(resp.status, body.code, body.error ?? `rotate failed: ${resp.status}`, body);
}

export async function restartSecret(name: string): Promise<{ restart_results: RestartResult[] }> {
  const resp = await fetch(`/api/secrets/${encodeURIComponent(name)}/restart`, { method: "POST" });
  const body = await parseJSONBody(resp);
  if (resp.status === 200 || resp.status === 207) return body as { restart_results: RestartResult[] };
  throw makeAPIError(resp.status, body.code, body.error ?? `restart failed: ${resp.status}`, body);
}

export async function deleteSecret(name: string, opts?: { confirm?: boolean }): Promise<void> {
  const url = `/api/secrets/${encodeURIComponent(name)}` + (opts?.confirm ? "?confirm=true" : "");
  const resp = await fetch(url, { method: "DELETE" });
  if (resp.status === 204) return;
  const body = await parseJSONBody(resp);
  throw makeAPIError(resp.status, body.code, body.error ?? `delete failed: ${resp.status}`, body);
}
