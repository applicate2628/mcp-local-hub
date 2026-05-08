import { useState } from "preact/hooks";
import type { CapabilitySubSection } from "../types";
import { StateBadge } from "./StateBadge";

interface Props {
  kind: "tools" | "prompts" | "resources";
  sub: CapabilitySubSection;
}

const labels: Record<Props["kind"], string> = {
  tools: "Tools",
  prompts: "Prompts",
  resources: "Resources",
};

// Collapsed-by-default section. Header click toggles the .expanded
// class on the wrapper. Item-list rendering arrives in Phase 6.
export function CapabilitySection({ kind, sub }: Props) {
  const [expanded, setExpanded] = useState(false);
  const items = sub.items ?? [];
  const count = items.length;

  return (
    <div
      class={`capability-section ${expanded ? "expanded" : ""}`}
      data-testid={`capability-section-${kind}`}
    >
      <header
        class="capability-section-header"
        role="button"
        tabIndex={0}
        onClick={() => setExpanded((e) => !e)}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            setExpanded((v) => !v);
          }
        }}
      >
        <span class="capability-section-label">{labels[kind]} ({count})</span>
        <StateBadge state={sub.state} />
      </header>
      {sub.err && (
        <p class="capability-section-err" role="alert">{sub.err}</p>
      )}
      {/* Phase 6 inserts the item-list here when expanded */}
    </div>
  );
}
