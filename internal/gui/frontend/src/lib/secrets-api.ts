// internal/gui/frontend/src/lib/secrets-api.ts

// "access_denied" (P2.1): the vault files exist and are intact, but the
// fail-closed read-hardening refused them because of a broadened DACL /
// wrong owner (the corp/sandbox %LOCALAPPDATA% case). This is a remediable
// permission problem, NOT corruption — the GUI must steer the operator to
// the icacls/chmod owner-only fix and NEVER suggest deleting the vault.
export type VaultState = "ok" | "missing" | "decrypt_failed" | "corrupt" | "access_denied";

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
  // Single-owner backend remediation text (deep-review P3): populated only
  // when vault_state === "access_denied". The backend (vaultAccessDeniedFix,
  // internal/api/secrets.go) is the sole author of this actionable guidance;
  // AccessDeniedView renders it verbatim instead of re-authoring its own copy.
  remediation?: string;
}

export interface RestartResult {
  task_name: string;
  error: string;
}

export interface SecretsRotateResult {
  vault_updated: boolean;
  restart_results: RestartResult[];
  // Codex PR #18 P1: present when the backend returned 500 +
  // RESTART_FAILED with vault_updated:true (orchestration crashed
  // post-vault-write, e.g. Status() failed before any restart
  // attempts so restart_results may be empty). The banner uses this
  // to show an orchestration-error message + retry instead of a
  // false "0 daemons restarted" success.
  error?: string;
  code?: string;
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

export interface SecretsInitReference {
  recovery_id?: string;
  recovery_status?: "ready" | "incomplete" | "unavailable";
}

export interface SecretsInitResult extends SecretsInitReference {
  vault_state?: string;
  cleanup_status?: string;
  error?: string;
  code?: string;
  status: number;
  body: unknown;
}

function initReference(value: unknown): SecretsInitReference {
  if (!value || typeof value !== "object") return {};
  const fields = value as Record<string, unknown>;
  const state = fields.recovery_status;
  if (state !== "ready" && state !== "incomplete" && state !== "unavailable") return {};
  const id = fields.recovery_id;
  return {
    recovery_status: state,
    ...(state !== "unavailable" && typeof id === "string" && /^[0-9a-f]{32}$/.test(id) && !/^0{32}$/.test(id)
      ? { recovery_id: id } : {}),
  };
}

// Init presentation never uses server error text or arbitrary caught messages.
export function secretsInitErrorMessage(value: unknown): string {
  const fields = value && typeof value === "object" ? value as Record<string, unknown> : {};
  const ref = initReference(fields);
  let message: string;
  if (fields.code === "SECRETS_INIT_UNSUPPORTED") {
    message = "Initialization is unavailable. Reload or update the application before initializing the secrets vault.";
  } else if (fields.code === "SECRETS_INIT_BLOCKED") {
    message = "Initialization is blocked by existing files. Reload or update the application and contact local support.";
  } else if (ref.recovery_status === "incomplete" || ref.recovery_status === "unavailable") {
    message = "Recovery evidence could not be verified. Reload or update the application and contact local support before trying again.";
  } else {
    message = "Initialization failed. Reload or update the application and contact local support before trying again.";
  }
  return ref.recovery_id ? `${message} Support reference: ${ref.recovery_id}.` : message;
}

function initError(status: number, code: string | undefined, body: unknown): APIError & SecretsInitReference {
  const ref = initReference(body);
  return Object.assign(makeAPIError(status, code, secretsInitErrorMessage({ code, ...ref }), body), ref);
}

export async function checkSecretsInitCapability(signal?: AbortSignal): Promise<void> {
  let status = 0;
  let body: unknown;
  try {
    const resp = await fetch("/api/secrets/init-capabilities", { cache: "no-store", signal });
    status = resp.status;
    body = await parseJSONBody(resp);
    if (status === 200 && body && typeof body === "object" && !Array.isArray(body)) {
      const cap = body as Record<string, unknown>;
      if (Object.keys(cap).length === 3 && cap.schema_version === 1 &&
          cap.workflow === "cli_support_readonly" && cap.path_free_errors === true) return;
    }
  } catch {
    // A transport failure supplies no positive permission to initialize.
  }
  throw initError(status, "SECRETS_INIT_UNSUPPORTED", body);
}

export async function secretsInit(): Promise<SecretsInitResult> {
  // Every init caller checks the current server; version metadata is not permission.
  await checkSecretsInitCapability();
  let resp: Response;
  try {
    resp = await fetch("/api/secrets/init", { method: "POST" });
  } catch {
    throw initError(0, undefined, undefined);
  }
  const body: unknown = await parseJSONBody(resp);
  const fields = body && typeof body === "object" && !Array.isArray(body) ? body as Record<string, unknown> : {};
  const code = typeof fields.code === "string" ? fields.code : undefined;
  // Cleanup-ok failure is HTTP200 with missing vault; it must stop every caller.
  if (resp.status === 200 && fields.vault_state === "ok" && fields.code === undefined && fields.error === undefined) {
    return { vault_state: "ok", ...(typeof fields.cleanup_status === "string" ? { cleanup_status: fields.cleanup_status } : {}),
      ...initReference(fields), status: resp.status, body };
  }
  throw initError(resp.status, code, body);
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
