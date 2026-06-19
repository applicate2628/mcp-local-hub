import { isSecretRef } from "./secret-ref";

// inlineSecretsToWrite returns the [key, value] pairs to write to the vault from
// the readiness panel's inline secret entries — ONLY for keys still referenced
// as a `secret:` ref in the current draft env, and only when non-empty. A value
// typed for a ref the user later removed or renamed is dropped rather than
// written to the vault under a key the manifest no longer uses (Codex #378).
export function inlineSecretsToWrite(
  inlineSecrets: Record<string, string>,
  env: { value: string }[],
): [string, string][] {
  const refKeys = new Set<string>();
  for (const row of env) {
    if (isSecretRef(row.value)) {
      const key = row.value.slice("secret:".length);
      if (key) refKeys.add(key);
    }
  }
  return Object.entries(inlineSecrets).filter(
    ([key, value]) => value.trim() !== "" && refKeys.has(key),
  );
}
