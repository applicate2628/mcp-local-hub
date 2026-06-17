// format — shared humanizers for daemon lifetime metrics (uptime + RAM).
//
// These were originally defined inline in Dashboard.tsx; they are now the
// single owner so the Servers row drawer and the Dashboard metric cards
// render identical strings without duplicating the rounding rules. Dashboard
// re-exports them for backward compatibility with its existing test imports.

// formatUptime humanizes a duration-in-seconds into a compact "2h 14m" /
// "3d 5h" / "47s" string. The two-largest-unit convention keeps the cell
// readable (a daemon up for 3 days does not need minute precision). Returns
// "" for a non-positive / undefined value so the caller can omit the row —
// uptime_sec is 0/absent for a just-spawned or non-running daemon.
export function formatUptime(sec: number | undefined): string {
  if (!sec || sec <= 0 || !Number.isFinite(sec)) return "";
  const s = Math.floor(sec);
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  const secs = s % 60;
  if (d > 0) return h > 0 ? `${d}d ${h}h` : `${d}d`;
  if (h > 0) return m > 0 ? `${h}h ${m}m` : `${h}h`;
  if (m > 0) return secs > 0 ? `${m}m ${secs}s` : `${m}m`;
  return `${secs}s`;
}

// formatBytes humanizes a byte count into "48 MB" / "1.2 GB". Uses binary
// (1024) units to match the working-set semantics of the underlying
// GetProcessMemoryInfo value. Returns "" for a non-positive / undefined
// value so the caller can omit the row — ram_bytes is 0/absent when RAM
// could not be determined.
export function formatBytes(bytes: number | undefined): string {
  if (!bytes || bytes <= 0 || !Number.isFinite(bytes)) return "";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let v = bytes;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  // Whole numbers for B/KB/MB (process RAM is naturally MB-scale); one
  // decimal for GB+ so a 1.2 GB daemon does not collapse to "1 GB".
  const rounded = i >= 3 ? v.toFixed(1) : Math.round(v).toString();
  return `${rounded} ${units[i]}`;
}
