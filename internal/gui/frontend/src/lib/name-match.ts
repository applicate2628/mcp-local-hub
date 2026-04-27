// Conservative name-matching for the SecretPicker dropdown sort (memo D9).
// Lowercases and converts hyphens to underscores on BOTH sides so a vault
// key written "OPENAI_API_KEY" still matches an env KEY of "openai-api-key".
// No fuzzy matching, no Levenshtein — only exact-after-normalization gets
// the "matches KEY name" badge.

export function normalizeForMatch(s: string): string {
  return s.toLowerCase().replace(/-/g, "_");
}

export function matchTier(vaultKey: string, envKey: string): 0 | 1 | 2 {
  const v = normalizeForMatch(vaultKey);
  const e = normalizeForMatch(envKey);
  if (v === e) return 0;
  if (v.startsWith(e) || e.startsWith(v)) return 1;
  return 2;
}
