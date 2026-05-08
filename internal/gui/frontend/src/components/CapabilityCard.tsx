import type { CapabilityRow, ProbeRow } from "../types";
import { CapabilitySection } from "./CapabilitySection";

interface Props {
  row: CapabilityRow;
  probe: ProbeRow | null;
}

export function CapabilityCard({ row, probe }: Props) {
  const testId = `capability-card-${row.server}-${row.daemon}`;
  // Codex bot PR #144 round-5 P2: probe === null means NO probe row
  // exists for this server/daemon — cache drift or daemon churn can
  // produce capabilities without a matching probe. Defaulting probeOk
  // to true would mis-render a green "✓ probed" pill when nothing was
  // probed. Three states now:
  //   - probe === null         → "? not probed" (unknown, gray)
  //   - probe.ok === true      → "✓ probed"    (success, green)
  //   - probe.ok === false     → "✗ probe err" (failure, red)
  const probeStatus: "unknown" | "ok" | "err" =
    probe === null ? "unknown" : probe.ok ? "ok" : "err";
  const probeStatusLabel: Record<typeof probeStatus, string> = {
    unknown: "? not probed",
    ok: "✓ probed",
    err: "✗ probe err",
  };
  const probeErr = probe?.err ?? "";
  const isSynthetic = probe?.source === "proxy-synthetic";

  return (
    <article class="capability-card" data-testid={testId}>
      <header class="capability-card-header">
        <span class="capability-card-server">{row.server}</span>
        <span class="capability-card-daemon">{row.daemon}</span>
        <span class={`capability-card-probe-status ${probeStatus}`}>
          {probeStatusLabel[probeStatus]}
        </span>
        {isSynthetic && (
          <span
            class="synthetic-source-pill"
            data-testid="synthetic-source-pill"
            title="Capabilities reported by the lazy-proxy stub; not a live MCP roundtrip."
          >
            synthetic
          </span>
        )}
        {probeStatus === "err" && probeErr && (
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
