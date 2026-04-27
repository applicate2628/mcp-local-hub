// Pure helpers for classifying manifest env values into resolution kinds.
// The picker uses isSecretRef for auto-open-on-typing trigger and
// hasSecretKey to distinguish "user is mid-typing the prefix" from
// "user has committed to a key" (so we don't show missing-marker on
// intermediate input). See memo §5.4.

export type RefKind = "secret" | "file" | "env" | "literal";

export interface ParsedRef {
  kind: RefKind;
  key: string | null;
}

export function parseSecretRef(value: string): ParsedRef {
  if (value.startsWith("secret:")) return { kind: "secret", key: value.slice("secret:".length) };
  if (value.startsWith("file:"))   return { kind: "file",   key: value.slice("file:".length) };
  if (value.startsWith("$"))       return { kind: "env",    key: value.slice("$".length) };
  return { kind: "literal", key: null };
}

export function isSecretRef(value: string): boolean {
  return value.startsWith("secret:");
}

export function hasSecretKey(value: string): boolean {
  return value.startsWith("secret:") && value.length > "secret:".length;
}
