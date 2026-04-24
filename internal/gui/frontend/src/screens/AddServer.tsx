import { useEffect, useRef, useState } from "preact/hooks";
import { BLANK_FORM, parseYAMLToForm, toYAML } from "../lib/manifest-yaml";
import { useDebouncedValue } from "../hooks/useDebouncedValue";
import {
  getExtractManifest,
  getManifest,
  postManifestCreate,
  postManifestEdit,
  postManifestValidate,
  ManifestHashMismatchError,
} from "../api";
import { generateUUID } from "../lib/uuid";
import type { BindingFormEntry, DaemonFormEntry, ManifestFormState } from "../types";

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
const KNOWN_CLIENTS = ["claude-code", "codex-cli", "gemini-cli", "antigravity"] as const;

// deepEqualForm compares two ManifestFormState instances structurally. Used
// by the Q8 dirty check. JSON.stringify is defensible for this shape: all
// fields are serializable primitives, arrays, and plain objects with no
// Date/Map/Set/functions. If a future field breaks that assumption, switch
// to a proper deep-equal import and update the test.
function deepEqualForm(a: ManifestFormState, b: ManifestFormState): boolean {
  return JSON.stringify(a) === JSON.stringify(b);
}

// parseAddServerQuery extracts ?server=...&from-client=... from the current
// hash. A1's Create-manifest button navigates to
// #/add-server?server=<name>&from-client=<client> — we pick those up on
// mount and run the prefill fetch.
function parseAddServerQuery(): { server: string; fromClient: string } {
  const hash = window.location.hash;
  const q = hash.split("?")[1] ?? "";
  const params = new URLSearchParams(q);
  return {
    server: params.get("server") ?? "",
    fromClient: params.get("from-client") ?? "",
  };
}

export function AddServerScreen(props: {
  mode?: "create" | "edit";
  onDirtyChange?: (dirty: boolean) => void;
} = {}) {
  const mode = props.mode ?? "create";
  const [formState, setFormState] = useState<ManifestFormState>(BLANK_FORM);
  // initialSnapshot is the post-normalization baseline the dirty check
  // compares against. Updated on mount (after any prefill path) and on
  // successful Save. Critically NOT updated on Paste YAML import (Q8
  // anti-silent-data-loss: paste must not move the baseline).
  const [initialSnapshot, setInitialSnapshot] = useState<ManifestFormState>(BLANK_FORM);
  const debouncedState = useDebouncedValue(formState, 150);
  const yamlPreview = toYAML(debouncedState);

  const isDirty = !deepEqualForm(formState, initialSnapshot);

  useEffect(() => {
    props.onDirtyChange?.(isDirty);
  }, [isDirty]);

  // Prefill path (Q8 baseline gotcha): fetch extract-manifest when the
  // user arrives from A1, parse → set form state → take the snapshot
  // AFTER normalization so dirty is false on first render.
  useEffect(() => {
    const { server, fromClient } = parseAddServerQuery();
    if (!server || !fromClient) return;
    let cancelled = false;
    (async () => {
      try {
        const yaml = await getExtractManifest(fromClient, server);
        if (cancelled) return;
        const parsed = parseYAMLToForm(yaml);
        setFormState(parsed);
        setInitialSnapshot(parsed);
      } catch (err) {
        if (cancelled) return;
        setBanner({
          kind: "error",
          text: `Could not prefill from ${fromClient}/${server}: ${(err as Error).message}. Continuing with empty form.`,
        });
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

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

  function addDaemon() {
    setFormState((prev) => ({
      ...prev,
      daemons: [...prev.daemons, { _id: generateUUID(), name: "", port: 0 }],
    }));
  }

  // updateDaemon handles port updates. Bindings reference daemons by _id
  // (identity-stable UUID), so rename no longer needs cascade — the binding
  // automatically resolves to the updated name at toYAML time.
  function updateDaemon(index: number, field: "name" | "port", value: string) {
    setFormState((prev) => {
      const target = prev.daemons[index];
      if (!target) return prev;
      const nextDaemon: DaemonFormEntry = field === "name"
        ? { ...target, name: value }
        : { ...target, port: parsePort(value) };
      const nextDaemons = prev.daemons.slice();
      nextDaemons[index] = nextDaemon;
      // No cascade needed — bindings key by _id, which is identity-stable.
      return { ...prev, daemons: nextDaemons };
    });
  }

  // deleteDaemon cascades to bindings: if any bindings reference this
  // daemon by _id, the user is prompted; on confirm both the daemon row and
  // every binding that pointed at it are removed in one state update.
  function deleteDaemon(index: number) {
    setFormState((prev) => {
      const target = prev.daemons[index];
      if (!target) return prev;
      const orphans = prev.client_bindings.filter((b) => b.daemonId === target._id);
      if (orphans.length > 0) {
        // eslint-disable-next-line no-alert
        const ok = window.confirm(
          `Delete daemon "${target.name}" and its ${orphans.length} client binding${orphans.length === 1 ? "" : "s"}?`,
        );
        if (!ok) return prev;
      }
      return {
        ...prev,
        daemons: prev.daemons.filter((_, i) => i !== index),
        client_bindings: prev.client_bindings.filter((b) => b.daemonId !== target._id),
      };
    });
  }

  function parsePort(raw: string): number {
    const n = Number(raw);
    return Number.isFinite(n) && n >= 0 ? Math.trunc(n) : 0;
  }

  // addBinding creates a new binding referencing the daemon by its stable _id.
  function addBinding(daemonId: string) {
    setFormState((prev) => ({
      ...prev,
      client_bindings: [
        ...prev.client_bindings,
        { client: KNOWN_CLIENTS[0], daemonId, url_path: "/mcp" },
      ],
    }));
  }

  function updateBinding(index: number, field: "client" | "daemonId" | "url_path", value: string) {
    setFormState((prev) => {
      const next = prev.client_bindings.slice();
      const target = next[index];
      if (!target) return prev;
      next[index] = { ...target, [field]: value };
      return { ...prev, client_bindings: next };
    });
  }

  function deleteBinding(index: number) {
    setFormState((prev) => ({
      ...prev,
      client_bindings: prev.client_bindings.filter((_, i) => i !== index),
    }));
  }

  const [warnings, setWarnings] = useState<string[] | null>(null);
  const [banner, setBanner] = useState<{ kind: "error" | "success"; text: string; retry?: () => Promise<void> } | null>(null);
  const [busy, setBusy] = useState<"" | "validate" | "save" | "install">("");
  // submissionVersion: bumped every time a Save/Save&Install click starts
  // its own inline serialize-validate-submit pipeline. If a second click
  // happens while the first is still in flight, the older pipeline sees
  // submissionCounter.current != its own captured value and bails before
  // writing to state. (Q3 Codex-identified gotcha.)
  const submissionCounter = useRef(0);
  // validateVersion: same pattern for the async Validate button path. A
  // newer Validate click invalidates an older in-flight validate's result
  // so stale warnings don't paint over fresh state. (Q5.)
  const validateCounter = useRef(0);

  async function runValidate() {
    const version = ++validateCounter.current;
    setBusy("validate");
    setBanner(null);
    try {
      const payload = toYAML(formState); // FRESH, not debounced
      const out = await postManifestValidate(payload);
      if (version !== validateCounter.current) return; // preempted
      setWarnings(out);
      if (out.length === 0) {
        setBanner({ kind: "success", text: "Validation passed — no warnings." });
      } else {
        setBanner({ kind: "error", text: `${out.length} validation warning${out.length === 1 ? "" : "s"}.` });
      }
    } catch (err) {
      if (version !== validateCounter.current) return;
      setBanner({ kind: "error", text: `/api/manifest/validate: ${(err as Error).message}` });
    } finally {
      setBusy("");
    }
  }

  async function runSave(opts: { install: boolean }) {
    const version = ++submissionCounter.current;
    setBusy(opts.install ? "install" : "save");
    setBanner(null);
    try {
      const name = formState.name.trim();
      if (!name) {
        setBanner({ kind: "error", text: "Name is required." });
        return;
      }
      const payload = toYAML(formState); // FRESH snapshot, not debounced preview
      const validateOut = await postManifestValidate(payload);
      if (version !== submissionCounter.current) return;
      if (validateOut.length > 0) {
        setWarnings(validateOut);
        setBanner({ kind: "error", text: `Cannot save: ${validateOut.length} validation warning${validateOut.length === 1 ? "" : "s"}.` });
        return;
      }
      await postManifestCreate(name, payload);
      if (version !== submissionCounter.current) return;
      // Commit the save as the new baseline. Paste does NOT do this; only
      // actual persist does. (Q8.)
      setInitialSnapshot(formState);
      if (!opts.install) {
        setWarnings(null);
        setBanner({ kind: "success", text: `Saved servers/${name}/manifest.yaml.` });
        return;
      }
      // Save & Install: run install; on failure, keep manifest on disk, offer retry.
      await runInstallNow(name, version);
    } catch (err) {
      if (version !== submissionCounter.current) return;
      setBanner({ kind: "error", text: (err as Error).message });
    } finally {
      if (version === submissionCounter.current) {
        setBusy("");
      }
    }
  }

  async function runInstallNow(name: string, version: number) {
    try {
      const resp = await fetch(`/api/install?name=${encodeURIComponent(name)}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
      });
      if (version !== submissionCounter.current) return;
      if (!resp.ok) {
        const body = await resp.json().catch(() => ({}));
        const err = (body as { error?: string }).error ?? resp.statusText;
        setBanner({
          kind: "error",
          text: `Saved servers/${name}/manifest.yaml, but install failed: ${err}`,
          retry: () => runInstallNow(name, ++submissionCounter.current),
        });
        return;
      }
      setWarnings(null);
      setBanner({ kind: "success", text: `Installed ${name}. Daemons will start at next logon (or run "mcphub restart --server ${name}" now).` });
    } catch (err) {
      if (version !== submissionCounter.current) return;
      setBanner({
        kind: "error",
        text: `Saved servers/${name}/manifest.yaml, but install threw: ${(err as Error).message}`,
        retry: () => runInstallNow(name, ++submissionCounter.current),
      });
    }
  }

  async function handlePasteYAML() {
    const pasted = window.prompt("Paste YAML manifest:", "");
    if (pasted == null || pasted.trim() === "") return;
    let parsed: ManifestFormState;
    try {
      parsed = parseYAMLToForm(pasted);
    } catch (err) {
      setBanner({ kind: "error", text: `Paste failed: ${(err as Error).message}` });
      return;
    }
    setFormState(parsed);
    // Per Q8 decision: paste does NOT reset the dirty baseline. Only
    // successful Save does. We DO auto-run structural validate since
    // paste is a mode switch and users expect "this parsed / this
    // mapped" feedback (Codex xhigh memo).
    //
    // Inline the validate against `parsed` — NOT via runValidate(),
    // whose closure would see the pre-paste formState (Task 12 review
    // must-fix).
    const version = ++validateCounter.current;
    setBusy("validate");
    setBanner(null);
    try {
      const payload = toYAML(parsed);
      const out = await postManifestValidate(payload);
      if (version !== validateCounter.current) return;
      setWarnings(out);
      if (out.length === 0) {
        setBanner({ kind: "success", text: "Pasted YAML passed validation." });
      } else {
        setBanner({ kind: "error", text: `Pasted YAML has ${out.length} validation warning${out.length === 1 ? "" : "s"}.` });
      }
    } catch (err) {
      if (version !== validateCounter.current) return;
      setBanner({ kind: "error", text: `/api/manifest/validate: ${(err as Error).message}` });
    } finally {
      setBusy("");
    }
  }

  async function handleCopyYAML() {
    const yaml = toYAML(formState); // fresh, not debounced
    try {
      await navigator.clipboard.writeText(yaml);
      setBanner({ kind: "success", text: "YAML copied to clipboard." });
    } catch {
      // Fallback for environments without clipboard API (older E2E setup etc.)
      setBanner({ kind: "error", text: "Clipboard API unavailable — copy manually from the preview pane." });
    }
  }

  // Suppress unused-import warnings for edit-mode symbols imported for
  // Tasks 12-13. These are intentionally imported now so that Task 11
  // restores typecheck-clean state across the whole frontend.
  void getManifest;
  void postManifestEdit;
  void ManifestHashMismatchError;

  return (
    <section class="screen add-server">
      <h1>Add server</h1>
      <div class="toolbar" data-testid="add-server-toolbar">
        <button
          type="button"
          onClick={runValidate}
          disabled={busy !== ""}
          data-action="validate"
        >
          {busy === "validate" ? "Validating…" : "Validate"}
        </button>
        <button
          type="button"
          onClick={() => runSave({ install: false })}
          disabled={busy !== "" || !!nameError}
          data-action="save"
        >
          {busy === "save" ? "Saving…" : "Save"}
        </button>
        <button
          type="button"
          class="primary"
          onClick={() => runSave({ install: true })}
          disabled={busy !== "" || !!nameError}
          data-action="save-and-install"
        >
          {busy === "install" ? "Installing…" : "Save & Install"}
        </button>
        <button
          type="button"
          onClick={handlePasteYAML}
          disabled={busy !== ""}
          data-action="paste-yaml"
        >
          Paste YAML
        </button>
        <button
          type="button"
          onClick={handleCopyYAML}
          disabled={busy !== ""}
          data-action="copy-yaml"
        >
          Copy YAML
        </button>
      </div>
      {banner && (
        <div class={`banner ${banner.kind}`} data-testid="banner">
          <p>{banner.text}</p>
          {banner.retry && (
            <button type="button" onClick={() => banner.retry?.()} data-action="retry-install">
              Retry Install
            </button>
          )}
        </div>
      )}
      {warnings && warnings.length > 0 && (
        <ul class="validation-warnings" data-testid="validation-warnings">
          {warnings.map((w, i) => (
            <li key={i}>{w}</li>
          ))}
        </ul>
      )}
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
                disabled={mode === "edit"}
                title={mode === "edit" ? "Kind and name are immutable after first install. Delete and recreate the server to change them." : undefined}
              />
              {nameError && <span class="inline-error">{nameError}</span>}
            </div>
            <div class="form-row">
              <label for="field-kind">Kind</label>
              <select
                id="field-kind"
                value={formState.kind}
                onChange={(e) => updateField("kind", (e.currentTarget as HTMLSelectElement).value as ManifestFormState["kind"])}
                disabled={mode === "edit"}
                title={mode === "edit" ? "Kind and name are immutable after first install. Delete and recreate the server to change them." : undefined}
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
            <div class="repeatable-rows" data-testid="daemon-rows">
              {formState.daemons.map((d, i) => (
                <div class="form-row daemon-row" key={d._id} data-daemon-row={i}>
                  <input
                    type="text"
                    placeholder="name (e.g. default)"
                    value={d.name}
                    onInput={(e) => updateDaemon(i, "name", (e.currentTarget as HTMLInputElement).value)}
                    data-field="daemon-name"
                  />
                  <input
                    type="number"
                    min={0}
                    max={65535}
                    placeholder="9100"
                    value={d.port}
                    onInput={(e) => updateDaemon(i, "port", (e.currentTarget as HTMLInputElement).value)}
                    data-field="daemon-port"
                  />
                  <button type="button" onClick={() => deleteDaemon(i)} data-action="delete-daemon">×</button>
                </div>
              ))}
              <button type="button" onClick={addDaemon} data-action="add-daemon">+ Add daemon</button>
            </div>
          </AccordionSection>
          <AccordionSection title="Client bindings">
            <ClientBindingsSection
              daemons={formState.daemons}
              bindings={formState.client_bindings}
              onAdd={addBinding}
              onUpdate={updateBinding}
              onDelete={deleteBinding}
            />
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

// ClientBindingsSection adaptively renders the bindings list:
//   - When there's exactly one daemon: flat [client][url_path][x] rows,
//     no inner accordion chrome. New bindings are added under that daemon.
//   - When there are 0 or 2+ daemons: grouped by daemon, each group is
//     its own collapsible inner subsection. Zero-daemon case shows a
//     helpful empty-state instructing the user to add a daemon first.
function ClientBindingsSection(props: {
  daemons: DaemonFormEntry[];
  bindings: BindingFormEntry[];
  onAdd: (daemonId: string) => void;
  onUpdate: (index: number, field: "client" | "daemonId" | "url_path", value: string) => void;
  onDelete: (index: number) => void;
}) {
  const { daemons, bindings, onAdd, onUpdate, onDelete } = props;
  if (daemons.length === 0) {
    return (
      <p class="placeholder">
        Add at least one daemon (in the section above) before creating
        client bindings — each binding must reference a daemon by name.
      </p>
    );
  }
  if (daemons.length === 1) {
    const only = daemons[0]._id;
    return (
      <BindingsList
        bindings={bindings}
        onAdd={() => onAdd(only)}
        onUpdate={onUpdate}
        onDelete={onDelete}
      />
    );
  }
  return (
    <div data-testid="bindings-adaptive-multi">
      {daemons.map((d) => {
        const indices: number[] = [];
        const group = bindings.filter((b, idx) => {
          if (b.daemonId === d._id) { indices.push(idx); return true; }
          return false;
        });
        return (
          <section class="bindings-daemon-group" key={d._id} data-daemon-group={d.name}>
            <h3>daemon: {d.name} (port {d.port})</h3>
            <BindingsList
              bindings={group}
              indices={indices}
              onAdd={() => onAdd(d._id)}
              onUpdate={onUpdate}
              onDelete={onDelete}
            />
          </section>
        );
      })}
    </div>
  );
}

// BindingsList renders a flat list of bindings. When the `indices` prop
// is supplied (multi-daemon path), it maps each displayed row to its
// absolute index in the parent client_bindings array, so the onUpdate /
// onDelete calls operate on the correct slot. Single-daemon path supplies
// the whole bindings array without an indices map.
function BindingsList(props: {
  bindings: BindingFormEntry[];
  indices?: number[];
  onAdd: () => void;
  onUpdate: (index: number, field: "client" | "daemonId" | "url_path", value: string) => void;
  onDelete: (index: number) => void;
}) {
  const { bindings, indices, onAdd, onUpdate, onDelete } = props;
  return (
    <div class="repeatable-rows bindings-list" data-testid="bindings-list">
      {bindings.map((b, displayIdx) => {
        const absIdx = indices ? indices[displayIdx] : displayIdx;
        return (
          <div class="form-row binding-row" key={absIdx} data-binding-row={absIdx}>
            <select
              value={b.client}
              data-field="binding-client"
              onChange={(e) => onUpdate(absIdx, "client", (e.currentTarget as HTMLSelectElement).value)}
            >
              {KNOWN_CLIENTS.map((c) => (
                <option key={c} value={c}>{c}</option>
              ))}
            </select>
            <input
              type="text"
              value={b.url_path}
              placeholder="/mcp"
              data-field="binding-url-path"
              onInput={(e) => onUpdate(absIdx, "url_path", (e.currentTarget as HTMLInputElement).value)}
            />
            <button type="button" onClick={() => onDelete(absIdx)} data-action="delete-binding">×</button>
          </div>
        );
      })}
      <button type="button" onClick={onAdd} data-action="add-binding">+ Add binding</button>
    </div>
  );
}
