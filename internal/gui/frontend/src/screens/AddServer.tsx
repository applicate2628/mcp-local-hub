import { useState } from "preact/hooks";
import { BLANK_FORM, toYAML } from "../lib/manifest-yaml";
import { useDebouncedValue } from "../hooks/useDebouncedValue";
import type { ManifestFormState } from "../types";

// MANIFEST_NAME_REGEX mirrors internal/api/manifest.go:23 validManifestName.
// Live client-side regex check provides instant feedback; the backend still
// authoritatively validates at create time.
const MANIFEST_NAME_REGEX = /^[a-z0-9][a-z0-9._-]*$/;

// KIND_OPTIONS and TRANSPORT_OPTIONS mirror the enum values accepted by
// internal/config/manifest.go. Keeping them as const tuples lets TS narrow
// them into the literal-union fields of ManifestFormState.
const KIND_OPTIONS = [
  { value: "global", label: "global (shared across all projects)" },
  { value: "workspace-scoped", label: "workspace-scoped (per-workspace lazy proxy)" },
] as const;
const TRANSPORT_OPTIONS = [
  { value: "stdio-bridge", label: "stdio-bridge (daemon multiplexes stdio child)" },
  { value: "native-http", label: "native-http (upstream speaks HTTP directly)" },
] as const;

export function AddServerScreen() {
  const [formState, setFormState] = useState<ManifestFormState>(BLANK_FORM);
  const debouncedState = useDebouncedValue(formState, 150);
  const yamlPreview = toYAML(debouncedState);

  const nameError = formState.name.length > 0 && !MANIFEST_NAME_REGEX.test(formState.name)
    ? "Must match [a-z0-9][a-z0-9._-]* (lowercase, digits, '.', '_', '-')"
    : "";

  function updateField<K extends keyof ManifestFormState>(key: K, value: ManifestFormState[K]) {
    setFormState((prev) => ({ ...prev, [key]: value }));
  }

  function updateBaseArg(index: number, value: string) {
    setFormState((prev) => {
      const next = prev.base_args.slice();
      next[index] = value;
      return { ...prev, base_args: next };
    });
  }

  function addBaseArg() {
    setFormState((prev) => ({ ...prev, base_args: [...prev.base_args, ""] }));
  }

  function deleteBaseArg(index: number) {
    setFormState((prev) => ({
      ...prev,
      base_args: prev.base_args.filter((_, i) => i !== index),
    }));
  }

  function addEnv() {
    setFormState((prev) => ({ ...prev, env: [...prev.env, { key: "", value: "" }] }));
  }

  function updateEnv(index: number, field: "key" | "value", value: string) {
    setFormState((prev) => {
      const next = prev.env.slice();
      next[index] = { ...next[index], [field]: value };
      return { ...prev, env: next };
    });
  }

  function deleteEnv(index: number) {
    setFormState((prev) => ({
      ...prev,
      env: prev.env.filter((_, i) => i !== index),
    }));
  }

  return (
    <section class="screen add-server">
      <h1>Add server</h1>
      <div class="add-server-grid">
        <div class="add-server-form">
          <AccordionSection title="Basics" open={true}>
            <div class="form-row">
              <label for="field-name">Name</label>
              <input
                id="field-name"
                type="text"
                value={formState.name}
                placeholder="memory"
                onInput={(e) => updateField("name", (e.currentTarget as HTMLInputElement).value)}
              />
              {nameError && <span class="inline-error">{nameError}</span>}
            </div>
            <div class="form-row">
              <label for="field-kind">Kind</label>
              <select
                id="field-kind"
                value={formState.kind}
                onChange={(e) => updateField("kind", (e.currentTarget as HTMLSelectElement).value as ManifestFormState["kind"])}
              >
                {KIND_OPTIONS.map((opt) => (
                  <option key={opt.value} value={opt.value}>{opt.label}</option>
                ))}
              </select>
            </div>
          </AccordionSection>

          <AccordionSection title="Command">
            <div class="form-row">
              <label for="field-transport">Transport</label>
              <select
                id="field-transport"
                value={formState.transport}
                onChange={(e) => updateField("transport", (e.currentTarget as HTMLSelectElement).value as ManifestFormState["transport"])}
              >
                {TRANSPORT_OPTIONS.map((opt) => (
                  <option key={opt.value} value={opt.value}>{opt.label}</option>
                ))}
              </select>
            </div>
            <div class="form-row">
              <label for="field-command">Command</label>
              <input
                id="field-command"
                type="text"
                value={formState.command}
                placeholder="npx"
                onInput={(e) => updateField("command", (e.currentTarget as HTMLInputElement).value)}
              />
            </div>
            <div class="form-row">
              <label>Base args</label>
              <div class="repeatable-rows" data-testid="base-args">
                {formState.base_args.map((arg, i) => (
                  <div class="form-row" key={i}>
                    <input
                      type="text"
                      value={arg}
                      onInput={(e) => updateBaseArg(i, (e.currentTarget as HTMLInputElement).value)}
                    />
                    <button type="button" onClick={() => deleteBaseArg(i)} data-action="delete-base-arg">×</button>
                  </div>
                ))}
                <button type="button" onClick={addBaseArg} data-action="add-base-arg">+ Add arg</button>
              </div>
            </div>
            <div class="form-row">
              <label for="field-weekly">Weekly refresh</label>
              <input
                id="field-weekly"
                type="checkbox"
                checked={formState.weekly_refresh}
                onChange={(e) => updateField("weekly_refresh", (e.currentTarget as HTMLInputElement).checked)}
              />
            </div>
          </AccordionSection>

          <AccordionSection title="Environment">
            <div class="repeatable-rows" data-testid="env-rows">
              {formState.env.map((row, i) => (
                <div class="form-row env-row" key={i} data-env-row={i}>
                  <input
                    type="text"
                    placeholder="KEY"
                    value={row.key}
                    onInput={(e) => updateEnv(i, "key", (e.currentTarget as HTMLInputElement).value)}
                  />
                  <input
                    type="text"
                    placeholder="value (literal or ${HOME}/...)"
                    value={row.value}
                    onInput={(e) => updateEnv(i, "value", (e.currentTarget as HTMLInputElement).value)}
                  />
                  <button type="button" onClick={() => deleteEnv(i)} data-action="delete-env">×</button>
                </div>
              ))}
              <button type="button" onClick={addEnv} data-action="add-env">+ Add environment variable</button>
            </div>
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
