import type { CapabilityRow, ProbeRow } from "../types";

interface Props {
  row: CapabilityRow;
  probe: ProbeRow | null;
}

// Per-server card. Header shows server + daemon + probe-status pill.
// Body has 3 collapsible section placeholders (Phase 5 adds the
// CapabilitySection collapsible mechanic; Phase 4 just shows counts).
export function CapabilityCard({ row, probe }: Props) {
  const testId = `capability-card-${row.server}-${row.daemon}`;
  const probeOk = probe?.ok ?? true;
  const probeErr = probe?.err ?? "";

  const itemCount = (items: { items: unknown[] | null }) => (items.items?.length ?? 0);

  return (
    <article class="capability-card" data-testid={testId}>
      <header class="capability-card-header">
        <span class="capability-card-server">{row.server}</span>
        <span class="capability-card-daemon">{row.daemon}</span>
        <span class={`capability-card-probe-status ${probeOk ? "ok" : "err"}`}>
          {probeOk ? "✓ probed" : "✗ probe err"}
        </span>
        {!probeOk && probeErr && (
          <span class="capability-card-probe-err">{probeErr}</span>
        )}
      </header>

      <div class="capability-card-body">
        <div class="capability-section" data-testid="capability-section-tools">
          <header class="capability-section-header">
            Tools ({itemCount(row.tools)})
          </header>
        </div>
        <div class="capability-section" data-testid="capability-section-prompts">
          <header class="capability-section-header">
            Prompts ({itemCount(row.prompts)})
          </header>
        </div>
        <div class="capability-section" data-testid="capability-section-resources">
          <header class="capability-section-header">
            Resources ({itemCount(row.resources)})
          </header>
        </div>
      </div>
    </article>
  );
}
