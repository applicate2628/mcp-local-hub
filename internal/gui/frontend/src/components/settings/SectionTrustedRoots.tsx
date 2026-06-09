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
    <section data-section="trusted_roots" class="settings-section">
      <h2>Trusted Roots</h2>
      <p class="settings-section-help">
        A trusted root lets any workspace under it auto-register an LSP
        daemon without explicit registration. Add only roots you control.
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
          {state.roots.length === 0 ? (
            <p class="trusted-roots-empty" data-testid="trusted-roots-empty">
              No trusted roots yet. The first workspace under any tree must be
              registered explicitly (via the Servers matrix "Enable" or{" "}
              <code>mcphub register</code>); after that, sibling workspaces
              under that tree auto-register. You can also pre-trust a broad
              root here.
            </p>
          ) : (
            <ul class="trusted-roots-list" data-testid="trusted-roots-list">
              {state.roots.map((root) => (
                <li key={root} class="trusted-roots-row" data-root={root}>
                  <code class="trusted-roots-path">{root}</code>
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

          <div class="trusted-roots-add">
            <label class="trusted-roots-add-label">
              Add a trusted root
              <input
                type="text"
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
            </label>
            <button
              type="button"
              disabled={!canAdd}
              data-testid="trusted-roots-add-button"
              onClick={() => void add()}
            >
              Add
            </button>
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

          <p class="trusted-roots-store-path">
            Stored at <code>{state.path}</code>
          </p>
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
