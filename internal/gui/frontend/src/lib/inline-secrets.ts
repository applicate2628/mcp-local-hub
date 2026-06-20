import { isSecretRef } from "./secret-ref";
import { SECRET_NAME_RE, isReservedName } from "./reserved-names";
import type { ManifestFormState } from "../types";

const SECRET_PLACEHOLDER_RE = /\$\{secret:([A-Za-z_][A-Za-z0-9_]*)\}/g;

function collectPlaceholders(value: string, out: Set<string>) {
  for (const match of value.matchAll(SECRET_PLACEHOLDER_RE)) {
    if (match[1]) out.add(match[1]);
  }
}

// secretRefKeys is the single owner of current inline-secret ref discovery for
// the Add Server draft. Env refs use the bare `secret:KEY` form; remote-http
// manifests keep `url`/`headers` as preserved top-level fields in this frontend
// state and embed placeholders as `${secret:KEY}`.
export function secretRefKeys(state: ManifestFormState): string[] {
  const refKeys = new Set<string>();
  for (const row of state.env) {
    if (isSecretRef(row.value)) {
      const key = row.value.slice("secret:".length);
      if (key) refKeys.add(key);
    }
  }

  const raw = state as ManifestFormState & { url?: unknown; headers?: unknown };
  const preserved = state._preservedRaw ?? {};
  const url = raw.url ?? preserved.url;
  if (typeof url === "string") collectPlaceholders(url, refKeys);

  const headers = raw.headers ?? preserved.headers;
  if (headers && typeof headers === "object" && !Array.isArray(headers)) {
    for (const value of Object.values(headers as Record<string, unknown>)) {
      if (typeof value === "string") collectPlaceholders(value, refKeys);
    }
  }

  return Array.from(refKeys).sort();
}

// inlineSecretsToWrite returns the [key, value] pairs to write to the vault from
// the readiness panel's inline secret entries — ONLY for keys that are (a) still
// referenced as a secret ref in the current draft, (b) non-empty, and (c) a VALID
// vault key name (the /api/secrets endpoint rejects anything not matching
// SECRET_NAME_RE, e.g. `foo-bar` or `123`). A value typed for a ref the user
// later removed/renamed, or for a non-conforming key, is dropped rather than
// written under an orphaned or unacceptable key (Codex #378 r1/r2).
export function inlineSecretsToWrite(
  inlineSecrets: Record<string, string>,
  state: ManifestFormState,
): [string, string][] {
  const refKeys = new Set(secretRefKeys(state));
  return Object.entries(inlineSecrets).filter(
    ([key, value]) =>
      value.trim() !== "" &&
      refKeys.has(key) &&
      SECRET_NAME_RE.test(key) &&
      // Reserved vault key names (e.g. `init`) collide with /api/secrets routes
      // and the add endpoint rejects them — never write one inline (Codex #378 r3).
      !isReservedName(key),
  );
}
