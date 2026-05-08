import type { CapabilityRow, ProbeRow } from "../types";
import { CapabilitySection } from "./CapabilitySection";

interface Props {
  row: CapabilityRow;
  probe: ProbeRow | null;
}

export function CapabilityCard({ row, probe }: Props) {
  const testId = `capability-card-${row.server}-${row.daemon}`;
  const probeOk = probe?.ok ?? true;
  const probeErr = probe?.err ?? "";

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
        <CapabilitySection kind="tools" sub={row.tools} />
        <CapabilitySection kind="prompts" sub={row.prompts} />
        <CapabilitySection kind="resources" sub={row.resources} />
      </div>
    </article>
  );
}
