// screens/Groups.tsx — the Groups authoring screen (groups Phase 5b-2).
//
// A "group" is a NAMED subset of MCP servers exposed at /g/<group>/mcp with
// optional per-server hidden tools — the tool-context-bloat fix (decision
// work-items/decisions/2026-06-18-groups-namespaces-tool-visibility.md). The
// operator authors groups visually here instead of hand-editing
// <state-dir>/groups.yaml.
//
// Structure mirrors the existing screens' conventions:
//   - a list of existing groups (cards) + a "New group" affordance + an
//     empty-state on a clean install (mirrors Catalog's empty-state);
//   - a draft/dirty/Save editor (mirrors SectionClients): name (inline-
//     validated, no ':'), description, a server multi-select sourced from
//     available_servers, and a per-server tool-hide tag input;
//   - Save → POST /api/groups; on restart_required (gate-OFF / hub not live)
//     a "restart the hub to apply" notice (the live-republish only fires
//     gate-ON + hub-live);
//   - Delete → ConfirmModal-gated DELETE;
//   - validation codes (GROUPS_UNKNOWN_SERVER / _INVALID_NAME /
//     _HIDDEN_NONMEMBER) surface as inline field errors;
//   - a dirty-guard reported up to App via onDirtyChange so nav-away prompts.
//
// The pure draft/dirty/validation logic lives in lib/groups-draft.ts (unit-
// tested there); this file is the rendering + network glue.

import { useEffect, useMemo, useState } from "preact/hooks";
import {
  getGroups,
  saveGroup,
  deleteGroup,
  GroupsApiError,
  type GroupDTO,
  type GroupConnectionDTO,
} from "../api";
import { ConfirmModal } from "../components/ConfirmModal";
import {
  type GroupDraft,
  type GroupErrorField,
  draftToBody,
  emptyDraft,
  draftFromGroup,
  isDirty,
  validateGroupName,
  selectedServers,
  fieldForCode,
} from "../lib/groups-draft";

type LoadState =
  | { kind: "loading" }
  | { kind: "ok"; groups: GroupDTO[]; available: string[] }
  | { kind: "error"; error: string };

// EditorTarget is what the editor is currently authoring:
//   - "none": list view, no editor open.
//   - {mode:"new"}: a fresh group; persisted is null → any content is dirty.
//   - {mode:"edit", name}: editing an existing group; persisted is the loaded
//     row keyed by name (name is locked — rename = delete + create).
type EditorTarget =
  | { mode: "none" }
  | { mode: "new" }
  | { mode: "edit"; name: string };

function asError(e: unknown): string {
  if (e instanceof Error) return e.message;
  return String(e);
}

export interface GroupsScreenProps {
  // onDirtyChange reports whether the editor has unsaved changes so App's
  // nav-away guard prompts before discarding (mirrors AddServer/Settings).
  onDirtyChange?: (dirty: boolean) => void;
}

export function GroupsScreen({ onDirtyChange }: GroupsScreenProps): preact.JSX.Element {
  const [state, setState] = useState<LoadState>({ kind: "loading" });
  const [target, setTarget] = useState<EditorTarget>({ mode: "none" });
  const [draft, setDraft] = useState<GroupDraft>(emptyDraft([]));
  const [busy, setBusy] = useState(false);
  // fieldError maps a backend validation code to the offending field so the
  // error renders next to the right control; form-level errors render at the
  // editor footer.
  const [fieldError, setFieldError] = useState<{ field: GroupErrorField; msg: string } | null>(null);
  // restartNotice carries the post-save "restart the hub to apply" message
  // when the write persisted but the live hub could not be re-published.
  const [restartNotice, setRestartNotice] = useState<string | null>(null);
  const [savedNotice, setSavedNotice] = useState(false);
  // deleteTarget drives the ConfirmModal; null = closed.
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);
  const [deleteError, setDeleteError] = useState<string | null>(null);

  async function load(): Promise<void> {
    setState({ kind: "loading" });
    try {
      const r = await getGroups();
      setState({ kind: "ok", groups: r.groups, available: r.available_servers });
    } catch (e) {
      setState({ kind: "error", error: asError(e) });
    }
  }

  useEffect(() => {
    void load();
  }, []);

  // The persisted row for the group currently being edited (null for a new
  // group). Drives isDirty and the editor's initial hydration.
  const persisted = useMemo<GroupDTO | null>(() => {
    if (target.mode !== "edit" || state.kind !== "ok") return null;
    return state.groups.find((g) => g.name === target.name) ?? null;
  }, [target, state]);

  const dirty = target.mode !== "none" && isDirty(draft, persisted);

  // Report dirty up to App for the nav-away guard. Clears on unmount so a
  // remount (e.g. screen swap) does not leave a stale dirty flag.
  useEffect(() => {
    onDirtyChange?.(dirty);
    return () => onDirtyChange?.(false);
  }, [dirty, onDirtyChange]);

  // confirmDiscardIfDirty guards an intra-screen editor switch: when the open
  // editor has unsaved edits and the operator opens a DIFFERENT target (another
  // group, or "new"), the App-level route dirty guard never fires (the route
  // stays #/groups), so without this the unsaved draft is silently discarded.
  // Mirrors the established window.confirm intra-screen pattern (AddServer's
  // daemon-delete + sidebar-intercept guards). Returns true to proceed.
  function confirmDiscardIfDirty(): boolean {
    if (!dirty) return true;
    // eslint-disable-next-line no-alert
    return window.confirm(
      "Discard your unsaved changes to this group?",
    );
  }

  function openNew(): void {
    if (state.kind !== "ok") return;
    if (!confirmDiscardIfDirty()) return;
    setDraft(emptyDraft(state.available));
    setTarget({ mode: "new" });
    setFieldError(null);
    setRestartNotice(null);
    setSavedNotice(false);
  }

  function openEdit(group: GroupDTO): void {
    if (state.kind !== "ok") return;
    // Opening the SAME target is a no-op re-hydration, not a target switch, so
    // it never needs a discard prompt; only a switch to a different target does.
    if (!(target.mode === "edit" && target.name === group.name) && !confirmDiscardIfDirty()) {
      return;
    }
    setDraft(draftFromGroup(group, state.available));
    setTarget({ mode: "edit", name: group.name });
    setFieldError(null);
    setRestartNotice(null);
    setSavedNotice(false);
  }

  // closeEditor closes the editor. The Close button path (force=false) guards
  // unsaved edits with a discard confirm (S1, sonnet): F5 guarded openNew /
  // openEdit but NOT this path, so the Close button silently discarded a dirty
  // draft. Returns without closing if the operator cancels the discard prompt.
  // The post-delete path passes force=true — there is nothing meaningful to
  // discard once the group is gone, and prompting there would be wrong UX.
  function closeEditor(force = false): void {
    if (!force && !confirmDiscardIfDirty()) return;
    setTarget({ mode: "none" });
    setFieldError(null);
    setRestartNotice(null);
    setSavedNotice(false);
  }

  function toggleServer(server: string): void {
    setSavedNotice(false);
    setRestartNotice(null);
    setDraft((d) => ({ ...d, selected: { ...d.selected, [server]: !d.selected[server] } }));
  }

  function setName(name: string): void {
    setSavedNotice(false);
    setFieldError((e) => (e?.field === "name" ? null : e));
    setDraft((d) => ({ ...d, name }));
  }

  function setDescription(description: string): void {
    setSavedNotice(false);
    setDraft((d) => ({ ...d, description }));
  }

  function setHiddenText(server: string, text: string): void {
    setSavedNotice(false);
    setFieldError((e) => (e?.field === "tools" ? null : e));
    setDraft((d) => ({ ...d, hiddenText: { ...d.hiddenText, [server]: text } }));
  }

  // nameError is the live client-side name validation (mirrors the Go rule);
  // null when valid. Shown inline under the name field.
  const nameError = target.mode === "new" ? validateGroupName(draft.name) : null;
  const chosen = selectedServers(draft);

  // serverRows is the UNION of (currently-available servers ∪ the draft's
  // selected members), sorted, each tagged stale=true when it is a selected
  // member that is NO LONGER available (e.g. its manifest was removed, or it
  // became daemonless/remote-http after the group was authored). Rendering the
  // union — not just `available` — keeps a stale persisted member VISIBLE with
  // its checkbox so the operator can uncheck + Save to remove it; otherwise it
  // would be invisible and un-removable, and a re-save would re-post it →
  // GROUPS_UNKNOWN_SERVER. selectedServers() reads draft.selected, which
  // draftFromGroup hydrates from the persisted group's members.
  const availableSet = useMemo(() => new Set(state.kind === "ok" ? state.available : []), [state]);
  const serverRows = useMemo(() => {
    const names = new Set<string>(availableSet);
    for (const s of selectedServers(draft)) names.add(s);
    return Array.from(names)
      .sort()
      .map((name) => ({ name, stale: !availableSet.has(name) }));
  }, [availableSet, draft]);
  const canSave =
    target.mode !== "none" &&
    dirty &&
    !busy &&
    validateGroupName(draft.name) === null;

  async function save(): Promise<void> {
    if (!canSave) return;
    setBusy(true);
    setFieldError(null);
    setRestartNotice(null);
    setSavedNotice(false);
    try {
      const body = draftToBody(draft);
      const resp = await saveGroup(body);
      setSavedNotice(true);
      if (resp.restart_required) {
        setRestartNotice(
          resp.hub_live
            ? "Saved, but the live hub could not be re-published in place. Restart the hub to apply this group."
            : "Saved. The aggregated hub is not running, so restart the hub (or enable the aggregated hub endpoint) to serve /g/" +
                body.name +
                "/mcp.",
        );
      }
      // Reload the list so the new/updated row appears and `persisted`
      // refreshes; switch the editor to edit-mode on the saved name so a
      // follow-up edit starts from the persisted state (dirty clears).
      await load();
      setTarget({ mode: "edit", name: body.name });
    } catch (e) {
      if (e instanceof GroupsApiError) {
        setFieldError({ field: fieldForCode(e.code), msg: e.message });
      } else {
        setFieldError({ field: "form", msg: asError(e) });
      }
    } finally {
      setBusy(false);
    }
  }

  async function confirmDelete(): Promise<void> {
    if (deleteTarget === null) return;
    setDeleteError(null);
    const deletedName = deleteTarget;
    try {
      const resp = await deleteGroup(deletedName);
      // If the editor was open on the deleted group, close it.
      // force=true: the group is gone, so there is nothing meaningful to
      // discard — skip the dirty-discard prompt the Close button uses.
      if (target.mode === "edit" && target.name === deletedName) closeEditor(true);
      setDeleteTarget(null);
      // The DELETE response carries restart_required just like save(): when
      // the write persisted but the live hub could not be re-published in
      // place (gate-OFF or the in-place publish failed), the operator must
      // restart the hub for the route to fully stop resolving. Surface the
      // same restart notice rather than discarding it.
      if (resp.restart_required) {
        setRestartNotice(
          resp.hub_live
            ? `Deleted "${deletedName}", but the live hub could not be re-published in place. Restart the hub to fully apply the removal.`
            : `Deleted "${deletedName}". The aggregated hub is not running, so restart the hub (or enable the aggregated hub endpoint) for /g/${deletedName}/mcp to stop resolving.`,
        );
      } else {
        setRestartNotice(null);
      }
      await load();
    } catch (e) {
      setDeleteError(asError(e));
      setDeleteTarget(null);
    }
  }

  if (state.kind === "loading") {
    return (
      <section class="groups-screen" data-testid="groups-loading">
        <h1>Groups</h1>
        <p>Loading…</p>
      </section>
    );
  }

  if (state.kind === "error") {
    return (
      <section class="groups-screen" data-testid="groups-error">
        <h1>Groups</h1>
        <p class="settings-error" data-testid="groups-load-error">
          Could not load groups: {state.error}
        </p>
        <button type="button" class="btn" onClick={() => void load()}>
          Retry
        </button>
      </section>
    );
  }

  const { groups } = state;

  return (
    <section class="groups-screen" data-testid="groups-loaded">
      <h1>Groups</h1>
      <p class="m-0 mb-4 text-sm text-app-muted">
        A group is a named subset of your MCP servers exposed at{" "}
        <code>/g/&lt;group&gt;/mcp</code>, with optional per-server hidden tools.
        Point a client at a group URL to give it only that group&rsquo;s tools —
        the fix for tool-context bloat.
      </p>

      {deleteError !== null && (
        <p class="settings-error" data-testid="groups-delete-error">
          Could not delete group: {deleteError}
        </p>
      )}

      {/* Top-level restart notice (e.g. after a DELETE while the editor is
          closed). When the editor is open it renders its own copy near Save,
          so this one is gated to the list view to avoid a duplicate. */}
      {restartNotice !== null && target.mode === "none" && (
        <p
          class="save-banner partial mb-4 text-sm"
          data-testid="groups-restart-notice-list"
          role="status"
        >
          {restartNotice}
        </p>
      )}

      <div class="mb-4 flex items-center gap-3">
        <button
          type="button"
          class="btn btn-primary"
          data-testid="groups-new"
          onClick={openNew}
        >
          New group
        </button>
      </div>

      {groups.length === 0 ? (
        <p class="empty-state" data-testid="groups-empty">
          No groups yet. Create one to expose a named subset of your servers.
        </p>
      ) : (
        <ul
          class="groups-list m-0 list-none p-0"
          data-testid="groups-list"
        >
          {groups.map((g) => (
            <li
              key={g.name}
              class="card mb-3"
              data-testid={`groups-row-${g.name}`}
              data-group={g.name}
            >
              <div class="card-title flex items-center justify-between gap-3">
                <span data-testid={`groups-row-name-${g.name}`}>{g.name}</span>
                <span class="flex gap-2">
                  <button
                    type="button"
                    class="btn"
                    data-testid={`groups-edit-${g.name}`}
                    onClick={() => openEdit(g)}
                  >
                    Edit
                  </button>
                  <button
                    type="button"
                    class="btn danger"
                    data-testid={`groups-delete-${g.name}`}
                    onClick={() => {
                      setDeleteError(null);
                      setDeleteTarget(g.name);
                    }}
                  >
                    Delete
                  </button>
                </span>
              </div>
              {g.description && (
                <p class="m-0 mb-1 text-sm text-app-muted">{g.description}</p>
              )}
              <p class="m-0 text-xs text-app-muted" data-testid={`groups-row-servers-${g.name}`}>
                {g.servers.length === 0
                  ? "No servers"
                  : `Servers: ${g.servers.join(", ")}`}
              </p>
              <ConnectionDetails group={g.name} connection={g.connection} />
            </li>
          ))}
        </ul>
      )}

      {target.mode !== "none" && (
        <div
          class="card mt-5"
          data-testid="groups-editor"
          role="region"
          aria-label={target.mode === "new" ? "New group" : `Edit group ${target.name}`}
        >
          <h2 class="m-0 mb-3 text-lg font-semibold text-app-text">
            {target.mode === "new" ? "New group" : `Edit group: ${target.name}`}
          </h2>

          <div class="mb-3">
            <label class="block text-sm text-app-text" for="groups-name-input">
              Name
            </label>
            <input
              id="groups-name-input"
              type="text"
              class="mt-1 w-full max-w-sm"
              value={draft.name}
              disabled={target.mode === "edit"}
              data-testid="groups-name-input"
              aria-invalid={nameError !== null || fieldError?.field === "name"}
              aria-describedby={
                nameError !== null || fieldError?.field === "name" ? "groups-name-error" : undefined
              }
              onInput={(e) => setName((e.target as HTMLInputElement).value)}
            />
            {target.mode === "edit" && (
              <p class="m-0 mt-1 text-xs text-app-muted">
                Renaming is delete + create. To rename, delete this group and make a new one.
              </p>
            )}
            {(nameError !== null || fieldError?.field === "name") && (
              <p id="groups-name-error" class="settings-error" data-testid="groups-name-error" role="alert">
                {fieldError?.field === "name" ? fieldError.msg : nameError}
              </p>
            )}
          </div>

          <div class="mb-3">
            <label class="block text-sm text-app-text" for="groups-description-input">
              Description <span class="text-app-muted">(optional)</span>
            </label>
            <input
              id="groups-description-input"
              type="text"
              class="mt-1 w-full max-w-md"
              value={draft.description}
              data-testid="groups-description-input"
              onInput={(e) => setDescription((e.target as HTMLInputElement).value)}
            />
          </div>

          <fieldset class="mb-3 border-0 p-0">
            <legend class="text-sm font-medium text-app-text">Servers</legend>
            <p class="m-0 mb-2 text-xs text-app-muted">
              Pick the servers this group exposes. Only the selected servers&rsquo;
              tools reach a client pointed at the group URL.
            </p>
            {serverRows.length === 0 ? (
              <p class="text-sm text-app-muted" data-testid="groups-no-servers">
                No servers available.
              </p>
            ) : (
              <ul class="groups-server-list m-0 list-none p-0" data-testid="groups-server-list">
                {serverRows.map(({ name: server, stale }) => {
                  const checked = draft.selected[server] === true;
                  return (
                    <li
                      key={server}
                      class="py-1.5"
                      data-server={server}
                      data-stale={stale ? "true" : undefined}
                    >
                      <label class="flex items-center gap-2 text-sm text-app-text">
                        <input
                          type="checkbox"
                          checked={checked}
                          disabled={busy}
                          data-testid={`groups-server-checkbox-${server}`}
                          onChange={() => toggleServer(server)}
                        />
                        <code class="text-sm text-app-text">{server}</code>
                        {stale && (
                          <span
                            class="text-xs text-app-muted"
                            data-testid={`groups-server-unavailable-${server}`}
                            title="This server is no longer available. Uncheck and Save to remove it from the group."
                          >
                            (unavailable)
                          </span>
                        )}
                      </label>
                      {checked && (
                        <div class="ml-6 mt-1">
                          <label
                            class="block text-xs text-app-muted"
                            for={`groups-hidden-input-${server}`}
                          >
                            Hide tools (comma-separated raw tool names; optional)
                          </label>
                          <input
                            id={`groups-hidden-input-${server}`}
                            type="text"
                            class="mt-1 w-full max-w-md"
                            value={draft.hiddenText[server] ?? ""}
                            placeholder="e.g. delete_file, write_file"
                            disabled={busy}
                            data-testid={`groups-hidden-input-${server}`}
                            onInput={(e) =>
                              setHiddenText(server, (e.target as HTMLInputElement).value)
                            }
                          />
                        </div>
                      )}
                    </li>
                  );
                })}
              </ul>
            )}
            {fieldError?.field === "servers" && (
              <p class="settings-error" data-testid="groups-servers-error" role="alert">
                {fieldError.msg}
              </p>
            )}
            {fieldError?.field === "tools" && (
              <p class="settings-error" data-testid="groups-tools-error" role="alert">
                {fieldError.msg}
              </p>
            )}
          </fieldset>

          <div class="mt-4 flex flex-wrap items-center gap-3 border-t border-app-border/60 pt-4">
            <button
              type="button"
              class="btn btn-primary"
              disabled={!canSave}
              data-testid="groups-save"
              onClick={() => void save()}
            >
              {busy ? "Saving…" : "Save"}
            </button>
            <button
              type="button"
              class="btn"
              disabled={busy}
              data-testid="groups-cancel"
              onClick={() => closeEditor()}
            >
              Close
            </button>
            <span class="text-xs text-app-muted" data-testid="groups-server-count">
              {chosen.length} server{chosen.length === 1 ? "" : "s"} selected
            </span>
            {savedNotice && !dirty && (
              <span class="save-banner ok text-sm" data-testid="groups-saved" role="status">
                Saved.
              </span>
            )}
          </div>

          {restartNotice !== null && (
            <p
              class="save-banner partial mt-2 text-sm"
              data-testid="groups-restart-notice"
              role="status"
            >
              {restartNotice}
            </p>
          )}

          {fieldError?.field === "form" && (
            <p class="settings-error" data-testid="groups-form-error" role="alert">
              {fieldError.msg}
            </p>
          )}
        </div>
      )}

      <ConfirmModal
        open={deleteTarget !== null}
        title="Delete group?"
        body={
          <p>
            Delete the group <code>{deleteTarget}</code>? Any client pointed at{" "}
            <code>/g/{deleteTarget}/mcp</code> will stop resolving until repointed.
          </p>
        }
        confirmLabel="Delete group"
        danger
        testId="groups-confirm-delete"
        onConfirm={() => void confirmDelete()}
        onCancel={() => setDeleteTarget(null)}
      />
    </section>
  );
}

// CopyRow renders a labelled read-only value with a copy-to-clipboard button.
// The value is the operator's own loopback bearer / endpoint metadata, surfaced
// in the same-origin GUI by design (B4) so the operator can wire a client.
function CopyRow({
  label,
  value,
  testId,
}: {
  label: string;
  value: string;
  testId: string;
}): preact.JSX.Element {
  const [copied, setCopied] = useState(false);
  async function copy(): Promise<void> {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      // Clipboard unavailable (insecure context / denied) — the value is still
      // visible + selectable in the <code> block, so this is a non-fatal
      // convenience failure.
    }
  }
  return (
    <div class="mt-1 flex items-center gap-2">
      <span class="text-xs text-app-muted" style="min-width: 5.5rem">{label}</span>
      <code
        class="flex-1 overflow-x-auto whitespace-nowrap text-xs text-app-text"
        data-testid={testId}
      >
        {value}
      </code>
      <button
        type="button"
        class="btn text-xs"
        data-testid={`${testId}-copy`}
        onClick={() => void copy()}
      >
        {copied ? "Copied" : "Copy"}
      </button>
    </div>
  );
}

// ConnectionDetails renders the copy-pasteable /g/<group>/mcp connection triple
// for a group (B4). When the hub is live + primed it shows the URL plus the two
// required headers (X-Mcphub-Hub-Token, X-Mcphub-Instance-Id) as copyable
// fields; otherwise it shows the backend's hint (start the hub) instead of a
// dead URL. It also carries the tools_hidden NOT-a-security-boundary disclaimer
// (B5 + consultant) so an operator never mistakes a group for an access fence.
function ConnectionDetails({
  group,
  connection,
}: {
  group: string;
  connection?: GroupConnectionDTO;
}): preact.JSX.Element | null {
  if (!connection) return null;
  return (
    <div class="mt-2 border-t border-app-border/40 pt-2" data-testid={`groups-connection-${group}`}>
      {connection.available ? (
        <>
          <p class="m-0 mb-1 text-xs font-medium text-app-text">
            Point a client at this group:
          </p>
          <CopyRow label="URL" value={connection.url ?? ""} testId={`groups-conn-url-${group}`} />
          <CopyRow
            label="Hub token"
            value={connection.token ?? ""}
            testId={`groups-conn-token-${group}`}
          />
          <CopyRow
            label="Instance ID"
            value={connection.instance_id ?? ""}
            testId={`groups-conn-instance-${group}`}
          />
          <p class="m-0 mt-1 text-xs text-app-muted">
            Send the token as <code>X-Mcphub-Hub-Token</code> and the instance ID
            as <code>X-Mcphub-Instance-Id</code>. localhost only.
          </p>
        </>
      ) : (
        <p
          class="m-0 text-xs text-app-muted"
          data-testid={`groups-connection-hint-${group}`}
          role="status"
        >
          {connection.hint ?? "The aggregated hub is not running."}
        </p>
      )}
      <p class="m-0 mt-2 text-xs text-app-muted">
        <strong>Note:</strong> hiding tools reduces the surface exposed at the
        hub; it is <strong>not</strong> an access-control boundary — daemon ports
        stay directly reachable, and at gate-OFF the hub filter is not in the
        path. Filter changes apply to new client sessions (reconnect to apply).
      </p>
    </div>
  );
}
