// Vault key names reserved due to HTTP route conflicts. Maintained
// alongside the backend constant in internal/api/secrets.go (per A3-a
// memo §4 D8 + Secrets.tsx:12-18 doc). The picker and AddSecretModal
// both consume this single set so client-side validation matches the
// server-side guard.

export const RESERVED_SECRET_NAMES: ReadonlySet<string> = new Set(["init"]);

export function isReservedName(name: string): boolean {
  return RESERVED_SECRET_NAMES.has(name);
}

// SECRET_NAME_RE mirrors the backend AddSecret name validator at
// internal/api/secrets.go:secretNameRE (^[A-Za-z][A-Za-z0-9_]*$).
// AddSecretModal and SecretPicker both reference this single source so
// that client-side rejection matches what the backend would reject.
//
// Codex PR #19 P1: SecretPicker uses this to gate `[CRE]` for missing
// keys whose names contain non-identifier characters (e.g.
// `secret:foo-bar`). Without this gate, the contextual-create modal
// opens with a locked-name field that fails NAME_RE — Save is
// permanently disabled, which is a dead-end UX.
export const SECRET_NAME_RE = /^[A-Za-z][A-Za-z0-9_]*$/;

export function isValidSecretName(name: string): boolean {
  return SECRET_NAME_RE.test(name);
}
