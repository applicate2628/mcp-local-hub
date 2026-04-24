import { useState } from "preact/hooks";
import { BLANK_FORM, toYAML } from "../lib/manifest-yaml";
import { useDebouncedValue } from "../hooks/useDebouncedValue";
import type { ManifestFormState } from "../types";

// AddServerScreen is the Phase 3B-II A2a create-flow. Renders an accordion
// form that serializes to YAML on every (debounced) keystroke; the preview
// shows what will be sent to /api/manifest/create. Actual sections are
// wired up in Tasks 7-10; this scaffolding commit is just the shell +
// route so the sidebar link and hashchange behavior are testable
// end-to-end before form fields land.
export function AddServerScreen() {
  const [formState, _setFormState] = useState<ManifestFormState>(BLANK_FORM);
  const debouncedState = useDebouncedValue(formState, 150);
  const yamlPreview = toYAML(debouncedState);
  return (
    <section class="screen add-server">
      <h1>Add server</h1>
      <div class="add-server-grid">
        <div class="add-server-form">
          <AccordionSection title="Basics" open={true}>
            <p class="placeholder">Name, kind (Task 7)</p>
          </AccordionSection>
          <AccordionSection title="Command">
            <p class="placeholder">Transport, command, base_args (Task 7)</p>
          </AccordionSection>
          <AccordionSection title="Environment">
            <p class="placeholder">env key-value rows (Task 8)</p>
          </AccordionSection>
          <AccordionSection title="Daemons">
            <p class="placeholder">name + port rows with cascade rename/delete (Task 9)</p>
          </AccordionSection>
          <AccordionSection title="Client bindings">
            <p class="placeholder">adaptive 1-vs-multi daemon (Task 10)</p>
          </AccordionSection>
        </div>
        <aside class="add-server-preview">
          <h2>YAML preview</h2>
          <pre data-testid="yaml-preview">{yamlPreview}</pre>
        </aside>
      </div>
    </section>
  );
}

// AccordionSection is the reusable collapsible container used by every form
// section. `open` controls initial state; clicking the header toggles.
function AccordionSection(props: { title: string; open?: boolean; children: preact.ComponentChildren }) {
  const [expanded, setExpanded] = useState(props.open ?? false);
  return (
    <section class={`accordion ${expanded ? "open" : "closed"}`}>
      <button
        type="button"
        class="accordion-header"
        aria-expanded={expanded}
        onClick={() => setExpanded((x) => !x)}
      >
        <span class="chevron">{expanded ? "▾" : "▸"}</span>
        <span>{props.title}</span>
      </button>
      {expanded && <div class="accordion-body">{props.children}</div>}
    </section>
  );
}
