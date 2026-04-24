// generateUUID returns a v4-style unique identifier. Uses the native
// crypto.randomUUID when available (Node 14.17+, all modern browsers);
// falls back to a Math.random-based non-cryptographic shape for happy-dom
// test environments that may not wire crypto.randomUUID.
//
// Used by manifest-yaml.ts to assign stable DaemonFormEntry._id and
// LanguageFormEntry._id at parse time. These IDs are NEVER serialized
// to YAML — they exist only to keep form-state references stable across
// user-driven rename/delete/reorder operations.
export function generateUUID(): string {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
    return crypto.randomUUID();
  }
  // Fallback (test env). Not cryptographically secure; uniqueness is
  // sufficient for in-memory form identity over a session.
  const s = () => Math.floor((1 + Math.random()) * 0x10000).toString(16).substring(1);
  return `${s()}${s()}-${s()}-${s()}-${s()}-${s()}${s()}${s()}`;
}
