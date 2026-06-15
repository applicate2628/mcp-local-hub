// ScanRefreshControls renders the shared "Rescan now" button + a small
// "updated Ns ago" / last-scanned indicator used by the scan-driven
// screens (Discovery + Servers). It is a pure presentation component: the
// timer/visibility lifecycle lives in useAutoScan; this only renders the
// state that hook exposes plus the per-screen pause note.
//
// Stable data-testids:
//   - rescan button:   data-testid="scan-rescan-btn"
//   - elapsed label:   data-testid="scan-updated-ago"
//   - paused note:     data-testid="scan-paused-note" (rendered only while paused)

export type ScanRefreshControlsProps = {
  /** Whole seconds since the last successful scan; null until the first. */
  agoSeconds: number | null;
  /** Fire an immediate refetch + reset the auto-refresh timer. */
  onRescan: () => void;
  /**
   * When true, auto-refresh is paused (Servers: unsaved matrix edits). The
   * pause note renders and the Rescan button is disabled so a click can't
   * silently drop pending edits. Discovery passes false (no edit state).
   */
  paused?: boolean;
  /** Human-readable reason shown next to the indicator while paused. */
  pauseReason?: string;
  /** Tooltip on the disabled Rescan button while paused. */
  disabledReason?: string;
};

// agoLabel renders the elapsed time as a compact "updated Ns ago" /
// "updated just now" string. null (pre-first-scan) → "scanning…".
function agoLabel(agoSeconds: number | null): string {
  if (agoSeconds == null) return "scanning…";
  if (agoSeconds <= 0) return "updated just now";
  if (agoSeconds < 60) return `updated ${agoSeconds}s ago`;
  const m = Math.floor(agoSeconds / 60);
  const s = agoSeconds % 60;
  if (m < 60) return s > 0 ? `updated ${m}m ${s}s ago` : `updated ${m}m ago`;
  const h = Math.floor(m / 60);
  return `updated ${h}h ${m % 60}m ago`;
}

export function ScanRefreshControls({
  agoSeconds,
  onRescan,
  paused = false,
  pauseReason = "auto-refresh paused — unsaved changes",
  disabledReason = "Apply or discard your unsaved changes before rescanning",
}: ScanRefreshControlsProps): preact.JSX.Element {
  return (
    <div class="scan-refresh-controls">
      <button
        type="button"
        class="scan-rescan-btn"
        data-testid="scan-rescan-btn"
        disabled={paused}
        title={paused ? disabledReason : "Re-scan client configs now"}
        onClick={onRescan}
      >
        Rescan now
      </button>
      <span class="scan-updated-ago" data-testid="scan-updated-ago" aria-live="polite">
        {agoLabel(agoSeconds)}
      </span>
      {paused && (
        <span class="scan-paused-note" data-testid="scan-paused-note">
          {pauseReason}
        </span>
      )}
    </div>
  );
}
