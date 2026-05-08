import { useState } from "preact/hooks";
import { StateBadge } from "./StateBadge";

const rows = [
  { state: "ok",          desc: "Server reported items successfully" },
  { state: "empty",       desc: "Server reported the category supports no items" },
  { state: "unsupported", desc: "Server explicitly declared no support for this category" },
  { state: "error",       desc: "Probe failed (see err)" },
  { state: "stale",       desc: "Last probe is older than the section TTL but no fresh data available" },
] as const;

export function CapabilityLegend() {
  const [open, setOpen] = useState(false);

  return (
    <div class={`capabilities-legend ${open ? "expanded" : ""}`} data-testid="capabilities-legend">
      <button
        class="capabilities-legend-toggle"
        data-testid="capabilities-legend-toggle"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
      >
        {open ? "Hide" : "Show"} state legend
      </button>
      {open && (
        <table class="capabilities-legend-table">
          <thead><tr><th>State</th><th>Meaning</th></tr></thead>
          <tbody>
            {rows.map((r) => (
              <tr key={r.state}>
                <td><StateBadge state={r.state} /></td>
                <td>{r.desc}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
