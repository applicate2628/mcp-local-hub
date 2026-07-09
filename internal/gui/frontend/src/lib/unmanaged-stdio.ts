import type { ScanEntry } from "../types";

export function unmanagedStdioCount(entries: readonly ScanEntry[] | null | undefined): number {
  let count = 0;
  for (const entry of entries ?? []) {
    if (isUnmanagedStdio(entry)) count++;
  }
  return count;
}

function isUnmanagedStdio(entry: ScanEntry): boolean {
  if (entry.status !== "unknown") return false;
  return Object.values(entry.client_presence ?? {}).some((presence) => presence?.transport === "stdio");
}
