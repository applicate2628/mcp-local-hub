// SectionTrustedRoots — Settings panel for the LSP trusted-roots store
// (<state-dir>/lsp-trusted-roots.json). PR #272 added the LSP-router
// trusted-root containment gate; this section is the management UI on
// top of it so the operator views / adds / removes operator-config roots
// in the GUI instead of hand-editing the JSON file.
//
// This is a self-contained section (no SettingsSnapshot prop, like
// SectionMaintenance) because the trusted-roots store has its own
// endpoint + fetch lifecycle, independent of /api/gui-settings. Add and
// Remove apply immediately to disk and re-render from the fresh list the
// backend returns, so there is no draft/dirty/save flow.

import { useEffect, useState } from "preact/hooks";
import {
  getTrustedRoots,
  addTrustedRoot,
  removeTrustedRoot,
} from "../../api";
import { ConfirmModal } from "../ConfirmModal";
import { InfoTip } from "../InfoTip";

type LoadState =
  | { kind: "loading" }
  | { kind: "ok"; roots: string[]; path: string }
  | { kind: "error"; error: string };

function asError(e: unknown): string {
  if (e instanceof Error) return e.message;
  return String(e);
}

// isAbsolutePath mirrors the backend's filepath.IsAbs check closely
// enough for an inline client-side validation hint: a POSIX path starts
// with "/", a Windows path starts with a drive letter + ":\" or ":/", or
// a UNC path starts with "\\". The authoritative rejection still happens
// server-side (LSP_TRUSTED_ROOTS_NOT_ABSOLUTE); this only spares the
// operator a round-trip for an obviously-relative entry.
function isAbsolutePath(p: string): boolean {
  if (p.startsWith("/")) return true; // POSIX absolute
  if (/^[A-Za-z]:[\\/]/.test(p)) return true; // Windows drive-absolute
  if (p.startsWith("\\\\")) return true; // Windows UNC
  return false;
}

export function SectionTrustedRoots(): preact.JSX.Element {
  const [state, setState] = useState<LoadState>({ kind: "loading" });
  const [draft, setDraft] = useState("");
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  // The root pending a remove-confirm. null = modal closed.
  const [pendingRemove, setPendingRemove] = useState<string | null>(null);

  async function load() {
    setState({ kind: "loading" });
    try {
      const r = await getTrustedRoots();
      setState({ kind: "ok", roots: r.roots, path: r.path });
    } catch (e) {
      setState({ kind: "error", error: asError(e) });
    }
  }

  useEffect(() => {
    void load();
  }, []);

  const trimmed = draft.trim();
  // Inline validation: empty is "not yet a candidate" (no error shown),
  // a non-empty relative path is an explicit error.
  const relativeError =
    trimmed !== "" && !isAbsolutePath(trimmed)
      ? "Enter an absolute path (e.g. C:\\dev or /home/you/dev)."
      : "";
  const canAdd = trimmed !== "" && relativeError === "" && !busy;

  async function add() {
    if (!canAdd) return;
    setBusy(true);
    setActionError(null);
    try {
      const r = await addTrustedRoot(trimmed);
      setState({ kind: "ok", roots: r.roots, path: r.path });
      setDraft("");
    } catch (e) {
      setActionError(asError(e));
    } finally {
      setBusy(false);
    }
  }

  async function confirmRemove() {
    const root = pendingRemove;
    setPendingRemove(null);
    if (root === null) return;
    setBusy(true);
    setActionError(null);
    try {
      const r = await removeTrustedRoot(root);
      setState({ kind: "ok", roots: r.roots, path: r.path });
    } catch (e) {
      setActionError(asError(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <section
      data-section="trusted_roots"
      class="mb-6 rounded-xl border border-app-border bg-app-card p-5 shadow-sm sm:p-6"
    >
      <header class="mb-2 flex items-center gap-1.5">
        <h2 class="m-0 text-lg font-semibold text-app-text">Trusted Roots</h2>
        <InfoTip text={'A trusted root lets any workspace under it auto-register an LSP daemon without explicit registration. The first workspace under any tree must still be registered explicitly (via the Servers matrix "Enable" or mcphub register); after that, sibling workspaces under that tree auto-register. Add only roots you control.'} />
      </header>
      <p class="m-0 mb-4 text-sm text-app-muted">
        Trees whose workspaces may auto-register an LSP daemon. Add only roots
        you control.
      </p>

      {state.kind === "loading" && <p>Loading…</p>}

      {state.kind === "error" && (
        <div class="trusted-roots-load-error">
          <p class="settings-error" data-testid="trusted-roots-load-error">
            Could not load trusted roots: {state.error}
          </p>
          <button type="button" onClick={() => void load()}>
            Retry
          </button>
        </div>
      )}

      {state.kind === "ok" && (
        <>
          <div class="mt-5">
            <h3 class="m-0 mb-1 text-xs font-semibold uppercase tracking-wide text-app-muted">
              Roots
            </h3>
            {state.roots.length === 0 ? (
              <p class="trusted-roots-empty m-0 text-sm text-app-muted" data-testid="trusted-roots-empty">
                No trusted roots yet. The first workspace under any tree must be
                registered explicitly (via the Servers matrix "Enable" or{" "}
                <code>mcphub register</code>); after that, sibling workspaces
                under that tree auto-register. You can also pre-trust a broad
                root here.
              </p>
            ) : (
              <ul class="trusted-roots-list m-0 list-none divide-y divide-app-border/60 p-0" data-testid="trusted-roots-list">
                {state.roots.map((root) => (
                  <li
                    key={root}
                    class="trusted-roots-row flex flex-wrap items-center justify-between gap-x-4 gap-y-1.5 py-3"
                    data-root={root}
                  >
                    <code class="trusted-roots-path text-sm text-app-text">{root}</code>
                    <button
                      type="button"
                      class="danger"
                      disabled={busy}
                      data-testid="trusted-roots-remove"
                      onClick={() => setPendingRemove(root)}
                    >
                      Remove
                    </button>
                  </li>
                ))}
              </ul>
            )}
          </div>

          <div class="mt-5 border-t border-app-border/60 pt-4">
            <h3 class="m-0 mb-1 text-xs font-semibold uppercase tracking-wide text-app-muted">
              Add a root
            </h3>
            <div class="trusted-roots-add flex flex-wrap items-center justify-between gap-x-4 gap-y-1.5 py-3">
              <label class="trusted-roots-add-label flex items-center gap-1.5 text-sm font-medium text-app-text" for="trusted-roots-input">
                Add a trusted root
                <InfoTip text="Enter an absolute path. POSIX: /home/you/dev. Windows: C:\dev or a UNC \\server\share path. The path is validated client-side and again on the server." />
              </label>
              <div class="flex items-center gap-2">
                <input
                  id="trusted-roots-input"
                  type="text"
                  class="field-ctl w-64"
                  value={draft}
                  placeholder="C:\dev  (or  /home/you/dev)"
                  disabled={busy}
                  data-testid="trusted-roots-input"
                  onInput={(e) => setDraft((e.target as HTMLInputElement).value)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter") {
                      e.preventDefault();
                      void add();
                    }
                  }}
                />
                <button
                  type="button"
                  disabled={!canAdd}
                  data-testid="trusted-roots-add-button"
                  onClick={() => void add()}
                >
                  Add
                </button>
              </div>
            </div>
            {relativeError !== "" && (
              <p class="settings-error" data-testid="trusted-roots-relative-error">
                {relativeError}
              </p>
            )}
            {actionError !== null && (
              <p class="settings-error" data-testid="trusted-roots-action-error">
                {actionError}
              </p>
            )}

            <p class="trusted-roots-store-path m-0 mt-2 text-xs text-app-muted">
              Stored at <code>{state.path}</code>
            </p>
          </div>
        </>
      )}

      <ConfirmModal
        open={pendingRemove !== null}
        title="Remove this trusted root?"
        body={
          <p>
            Workspaces under{" "}
            <code>{pendingRemove ?? ""}</code> will no longer auto-register an
            LSP daemon unless they are registered explicitly. Already-running
            daemons are unaffected.
          </p>
        }
        confirmLabel="Remove"
        danger
        onConfirm={confirmRemove}
        onCancel={() => setPendingRemove(null)}
      />
    </section>
  );
}
