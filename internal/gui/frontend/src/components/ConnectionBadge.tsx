import type { SseConnectionState } from "../hooks/useEventSource";

// ConnectionBadge renders a subtle live/reconnecting indicator for an
// SSE-backed screen, driven by the connection state useEventSource now
// exposes. Without it, a dropped supervisor/GUI leaves the screen
// showing the last snapshot with no "stale / reconnecting" cue — the
// operator trusts frozen-but-plausible data. (G13 resilience.)
//
//   - "open"         → "● live" (green) — the stream is connected.
//   - "reconnecting" → "○ reconnecting…" (amber) — onerror fired;
//                       native EventSource is auto-retrying.
//   - "connecting"   → same amber "reconnecting…" affordance; the
//                       initial-open window is visually identical to a
//                       retry from the operator's standpoint.
//
// Subtle by design: a single small glyph + label, colored from the
// app's light/dark-aware design tokens (--success / --warning), so it
// reads as ambient status, not an alarm.
export function ConnectionBadge({ state }: { state: SseConnectionState }) {
  const live = state === "open";
  const color = live ? "var(--success)" : "var(--warning, #bf8700)";
  return (
    <span
      class={`connection-badge connection-badge-${live ? "live" : "reconnecting"}`}
      data-testid="connection-badge"
      title={
        live
          ? "Live feed connected"
          : "Reconnecting to the live feed — showing the last received data"
      }
      style={`display: inline-flex; align-items: center; gap: 4px; font-size: 0.85em; color: ${color}`}
    >
      <span class="connection-badge-dot" aria-hidden="true">
        {live ? "●" : "○"}
      </span>
      {live ? "live" : "reconnecting…"}
    </span>
  );
}
