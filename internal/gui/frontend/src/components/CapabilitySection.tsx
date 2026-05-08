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
// class on the wrapper. When expanded, renders the item list (or a
// "(no items)" placeholder for empty/unsupported/error subsections).
export function CapabilitySection({ kind, sub }: Props) {
  const [expanded, setExpanded] = useState(false);
  // AC #19 — items: CapabilityItem[] | null; normalize at the section
  // boundary so the rest of the component never sees null.
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
        // Codex bot PR #144 round-6 P3: expose collapsed/expanded
        // state to assistive tech so screen readers announce section
        // open/close after click or Enter/Space activation.
        aria-expanded={expanded}
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

      {expanded && (
        <ul class="capability-item-list">
          {count === 0 ? (
            <li class="capability-item capability-item-empty">(no items)</li>
          ) : (
            items.map((item) => (
              <li key={item.id} class="capability-item">
                <span class="capability-item-name">{item.name}</span>
                <span class="capability-item-id">{item.id}</span>
              </li>
            ))
          )}
        </ul>
      )}
    </div>
  );
}
