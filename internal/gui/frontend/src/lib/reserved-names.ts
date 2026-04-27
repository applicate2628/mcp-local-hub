// Vault key names reserved due to HTTP route conflicts. Maintained
// alongside the backend constant in internal/api/secrets.go (per A3-a
// memo §4 D8 + Secrets.tsx:12-18 doc). The picker and AddSecretModal
// both consume this single set so client-side validation matches the
// server-side guard.

export const RESERVED_SECRET_NAMES: ReadonlySet<string> = new Set(["init"]);

export function isReservedName(name: string): boolean {
  return RESERVED_SECRET_NAMES.has(name);
}
