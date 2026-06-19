// SectionClients — Settings panel for the default-install client set
// override (redesign spec §9 multi-agent table / line 204). The compile-time
// default-install set is the fixed {claude-code, codex-cli, cursor} trio;
// this panel lets the operator toggle which clients are in the
// default-install set (the trio plus opt-ins) without CLI flags. The chosen
// set is persisted to gui-preferences.yaml and becomes the effective default
// for installs that do not name an explicit --clients target.
//
// This is a self-contained section (no SettingsSnapshot prop, like
// SectionTrustedRoots / SectionMaintenance) because the override has its own
// endpoint + fetch lifecycle, independent of /api/settings. Unlike
// trusted-roots (which applies each add/remove immediately), this panel uses
// a draft/dirty/Save flow: the operator toggles checkboxes, then Save commits
// the whole set. Save is disabled when the draft equals the persisted set or
// when zero clients are selected (a zero-client install is never the intent;
// the backend rejects it too).

import { useEffect, useMemo, useState } from "preact/hooks";
import {
  getClientInstallPrefs,
  setClientInstallPrefs,
  type ClientInstallPrefRow,
} from "../../api";
import { SettingsCard } from "./SettingsCard";

type LoadState =
  | { kind: "loading" }
  | { kind: "ok"; rows: ClientInstallPrefRow[]; overrideActive: boolean }
  | { kind: "error"; error: string };

function asError(e: unknown): string {
  if (e instanceof Error) return e.message;
  return String(e);
}

// selectedNames returns the sorted set of currently-checked client names.
// Sorting makes the draft-vs-persisted equality check order-independent.
function selectedNames(draft: Record<string, boolean>): string[] {
  return Object.keys(draft)
    .filter((name) => draft[name])
    .sort();
}

// sameSet reports whether two name lists hold the same members (order-
// independent). Used to disable Save when the draft equals what is on disk.
function sameSet(a: string[], b: string[]): boolean {
  if (a.length !== b.length) return false;
  const sa = [...a].sort();
  const sb = [...b].sort();
  return sa.every((v, i) => v === sb[i]);
}

export function SectionClients(): preact.JSX.Element {
  const [state, setState] = useState<LoadState>({ kind: "loading" });
  // draft[name] = checked. Initialized from the persisted snapshot on load
  // and on every successful save.
  const [draft, setDraft] = useState<Record<string, boolean>>({});
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [savedNotice, setSavedNotice] = useState(false);

  async function load() {
    setState({ kind: "loading" });
    setActionError(null);
    try {
      const r = await getClientInstallPrefs();
      setState({ kind: "ok", rows: r.clients, overrideActive: r.override_active });
      const next: Record<string, boolean> = {};
      for (const row of r.clients) next[row.name] = row.selected;
      setDraft(next);
    } catch (e) {
      setState({ kind: "error", error: asError(e) });
    }
  }

  useEffect(() => {
    void load();
  }, []);

  // The persisted selection, derived from the loaded snapshot's `selected`
  // flags. Recomputed only when the snapshot changes.
  const persisted = useMemo(() => {
    if (state.kind !== "ok") return [];
    return state.rows.filter((r) => r.selected).map((r) => r.name).sort();
  }, [state]);

  const chosen = selectedNames(draft);
  const dirty = state.kind === "ok" && !sameSet(chosen, persisted);
  const empty = chosen.length === 0;
  const canSave = dirty && !empty && !busy;

  function toggle(name: string) {
    setSavedNotice(false);
    setDraft((d) => ({ ...d, [name]: !d[name] }));
  }

  async function save() {
    if (!canSave) return;
    setBusy(true);
    setActionError(null);
    setSavedNotice(false);
    try {
      const r = await setClientInstallPrefs(chosen);
      setState({ kind: "ok", rows: r.clients, overrideActive: r.override_active });
      const next: Record<string, boolean> = {};
      for (const row of r.clients) next[row.name] = row.selected;
      setDraft(next);
      setSavedNotice(true);
    } catch (e) {
      setActionError(asError(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <SettingsCard
      section="clients"
      title="Clients"
      infoTip="Choose which MCP clients a default install touches. The default set is claude-code, codex-cli, and cursor; other clients are opt-in. Installs that do not name an explicit client target use this set. Already-installed servers are unaffected until you reinstall them."
      subtitle={
        <>
          Pick the clients in the default-install set. A plain install (Servers
          matrix Install, Save &amp; Install, or <code>mcphub install &lt;server&gt;</code>)
          writes hub entries to exactly these clients. Unchecking a client here
          does not remove existing entries; it only changes what the next default
          install touches.
        </>
      }
    >
      {state.kind === "loading" && <p>Loading…</p>}

      {state.kind === "error" && (
        <div class="client-prefs-load-error">
          <p class="settings-error" data-testid="client-prefs-load-error">
            Could not load client install preferences: {state.error}
          </p>
          <button type="button" onClick={() => void load()}>
            Retry
          </button>
        </div>
      )}

      {state.kind === "ok" && (
        <>
          <p class="m-0 mb-3 text-xs text-app-muted" data-testid="client-prefs-mode">
            {state.overrideActive
              ? "Using a custom default-install set."
              : "Using the built-in default set (claude-code, codex-cli, cursor)."}
          </p>

          <ul
            class="client-prefs-list m-0 list-none divide-y divide-app-border/60 p-0"
            data-testid="client-prefs-list"
          >
            {state.rows.map((row) => (
              <li
                key={row.name}
                class="client-prefs-row flex flex-wrap items-center justify-between gap-x-4 gap-y-1.5 py-2.5"
                data-client={row.name}
              >
                <label class="flex items-center gap-2 text-sm text-app-text">
                  <input
                    type="checkbox"
                    checked={draft[row.name] === true}
                    disabled={busy}
                    data-testid={`client-prefs-checkbox-${row.name}`}
                    onChange={() => toggle(row.name)}
                  />
                  <code class="text-sm text-app-text">{row.name}</code>
                  {row.compile_default && (
                    <span class="client-prefs-default-badge text-xs text-app-muted" data-testid={`client-prefs-default-${row.name}`}>
                      (default)
                    </span>
                  )}
                </label>
              </li>
            ))}
          </ul>

          <div class="mt-5 flex items-center gap-3 border-t border-app-border/60 pt-4">
            <button
              type="button"
              class="btn-primary"
              disabled={!canSave}
              data-testid="client-prefs-save"
              onClick={() => void save()}
            >
              Save
            </button>
            {empty && dirty && (
              <span class="settings-error" data-testid="client-prefs-empty-error">
                Select at least one client.
              </span>
            )}
            {savedNotice && !dirty && (
              <span class="client-prefs-saved text-sm text-app-muted" data-testid="client-prefs-saved">
                Saved.
              </span>
            )}
          </div>

          {actionError !== null && (
            <p class="settings-error" data-testid="client-prefs-action-error">
              {actionError}
            </p>
          )}
        </>
      )}
    </SettingsCard>
  );
}
