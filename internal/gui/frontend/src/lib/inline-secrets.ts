import { isSecretRef } from "./secret-ref";
import { SECRET_NAME_RE } from "./reserved-names";

// inlineSecretsToWrite returns the [key, value] pairs to write to the vault from
// the readiness panel's inline secret entries — ONLY for keys that are (a) still
// referenced as a `secret:` ref in the current draft env, (b) non-empty, and
// (c) a VALID vault key name (the /api/secrets endpoint rejects anything not
// matching SECRET_NAME_RE, e.g. `foo-bar` or `123`). A value typed for a ref the
// user later removed/renamed, or for a non-conforming key, is dropped rather
// than written under an orphaned or unacceptable key (Codex #378 r1/r2).
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
    ([key, value]) => value.trim() !== "" && refKeys.has(key) && SECRET_NAME_RE.test(key),
  );
}
