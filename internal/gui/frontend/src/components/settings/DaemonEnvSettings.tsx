import { useEffect, useMemo, useState } from "preact/hooks";
import {
  applyDaemonEnv,
  listDaemonEnv,
  respawnDaemon,
  type DaemonEnvRow,
} from "../../api";
import { ConfirmModal } from "../ConfirmModal";

const ENV_KEY_RE = /^[A-Za-z_][A-Za-z0-9_]*$/;
const DEFAULT_ENV_KEY = "MEMORY_FILE_PATH";

type Banner = { kind: "ok" | "error"; text: string };

export type DaemonEnvSettingsProps = {
  onDirtyChange?: (dirty: boolean) => void;
};

function stableEnvSignature(env: Record<string, string> | undefined): string {
  if (!env) return "";
  return JSON.stringify(Object.entries(env).sort(([a], [b]) => a.localeCompare(b)));
}

export function DaemonEnvSettings({
  onDirtyChange = () => {},
}: DaemonEnvSettingsProps = {}): preact.JSX.Element {
  const [rows, setRows] = useState<DaemonEnvRow[]>([]);
  const [selectedTask, setSelectedTask] = useState("");
  const [key, setKey] = useState(DEFAULT_ENV_KEY);
  const [value, setValue] = useState("");
  const [draftBase, setDraftBase] = useState({
    taskName: "",
    envSignature: "",
    key: DEFAULT_ENV_KEY,
    value: "",
  });
  const [busy, setBusy] = useState(false);
  const [banner, setBanner] = useState<Banner | null>(null);
  // Finding #268 r11 P2 (DaemonEnvSettings.tsx:53): when the operator picks a
  // different daemon while the current row has an unsaved edit (envDirty),
  // intercept the switch and stash the target task here so a ConfirmModal can
  // gate the discard. The <select> stays controlled by selected?.task_name, so
  // it visually reverts on cancel; the parent dirty guard is NOT cleared until
  // the edit is actually applied or the operator confirms the discard.
  const [pendingSwitchTask, setPendingSwitchTask] = useState<string | null>(null);

  const selected = useMemo(
    () => rows.find((row) => row.task_name === selectedTask) ?? rows[0],
    [rows, selectedTask],
  );
  const selectedEnvSignature = useMemo(
    () => stableEnvSignature(selected?.env),
    [selected?.env],
  );
  const envDirty = useMemo(() => {
    if (!selected) return false;
    if (
      draftBase.taskName !== selected.task_name ||
      draftBase.envSignature !== selectedEnvSignature
    ) {
      return false;
    }
    return key !== draftBase.key || value !== draftBase.value;
  }, [selected?.task_name, selectedEnvSignature, draftBase, key, value]);

  useEffect(() => {
    void refreshRows();
  }, []);

  useEffect(() => {
    if (!selected) return;
    if (selectedTask === "") {
      setSelectedTask(selected.task_name);
    }
    const keys = Object.keys(selected.env).sort();
    const nextKey = keys.includes(key) ? key : (keys[0] ?? key);
    if (nextKey !== key) {
      setKey(nextKey);
    }
    setValue(selected.env[nextKey] ?? "");
    setDraftBase({
      taskName: selected.task_name,
      envSignature: selectedEnvSignature,
      key: nextKey,
      value: selected.env[nextKey] ?? "",
    });
  }, [selected?.task_name, selectedEnvSignature]);

  useEffect(() => {
    onDirtyChange(envDirty);
  }, [envDirty, onDirtyChange]);

  // refreshRows reports success/failure to its caller so apply() can avoid
  // overwriting a refresh-error banner with a bare "Saved" (Finding #268 r11
  // P3, DaemonEnvSettings.tsx:112). The mount effect and the Refresh button
  // ignore the return value — for them the in-band error banner set here is
  // the only feedback path. Returns { ok: true } on success, or
  // { ok: false, error } when listDaemonEnv rejects.
  async function refreshRows(): Promise<{ ok: boolean; error?: string }> {
    setBusy(true);
    setBanner(null);
    try {
      const result = await listDaemonEnv();
      setRows(result.daemons);
      if (selectedTask && result.daemons.every((row) => row.task_name !== selectedTask)) {
        setSelectedTask(result.daemons[0]?.task_name ?? "");
      }
      return { ok: true };
    } catch (err) {
      const message = (err as Error).message;
      setBanner({ kind: "error", text: message });
      return { ok: false, error: message };
    } finally {
      setBusy(false);
    }
  }

  // performSwitch applies the daemon selection + seeds the editor from the
  // target row. Extracted so both the unguarded onChange path and the
  // ConfirmModal "discard and switch" confirm path reuse identical logic.
  function performSwitch(task: string) {
    setSelectedTask(task);
    const row = rows.find((r) => r.task_name === task);
    const keys = Object.keys(row?.env ?? {}).sort();
    const nextKey = keys[0] ?? DEFAULT_ENV_KEY;
    setKey(nextKey);
    setValue(row?.env[nextKey] ?? "");
  }

  function confirmSwitch() {
    const task = pendingSwitchTask;
    setPendingSwitchTask(null);
    if (task !== null) performSwitch(task);
  }

  function cancelSwitch() {
    // Drop the pending target; the controlled <select> reverts to the
    // current daemon and the unsaved edit stays intact (envDirty unchanged,
    // so the parent dirty guard is preserved).
    setPendingSwitchTask(null);
  }

  async function apply() {
    if (!selected) return;
    // Guard the no-op Apply: on a fresh panel the editor seeds the
    // placeholder key with an empty value, so without this guard a user who
    // just opens the panel and clicks Apply writes {MEMORY_FILE_PATH: ""} as a
    // real override, pinning an empty env var and blocking auto-discovery for
    // that row. envDirty is only true when key/value differ from the loaded
    // baseline (Codex bot #268 r10 P2).
    if (!envDirty) return;
    const trimmedKey = key.trim();
    if (!ENV_KEY_RE.test(trimmedKey)) {
      setBanner({ kind: "error", text: "Env key must match [A-Za-z_][A-Za-z0-9_]*." });
      return;
    }
    setBusy(true);
    setBanner(null);
    try {
      await applyDaemonEnv(selected.task_name, { [trimmedKey]: value });
      const refreshed = await refreshRows();
      if (refreshed.ok) {
        setBanner({ kind: "ok", text: "Saved. Restart the daemon for the change to take effect." });
      } else {
        // The POST succeeded but the follow-up list fetch failed, so the
        // editor still shows the pre-apply row and may look dirty. Surface
        // the refresh failure instead of replacing it with a bare "Saved"
        // (Finding #268 r11 P3, DaemonEnvSettings.tsx:112).
        setBanner({
          kind: "error",
          text: `Saved, but could not refresh the list: ${refreshed.error ?? "unknown error"}. Restart the daemon for the change to take effect.`,
        });
      }
    } catch (err) {
      setBanner({ kind: "error", text: (err as Error).message });
    } finally {
      setBusy(false);
    }
  }

  async function restart() {
    if (!selected) return;
    setBusy(true);
    setBanner(null);
    try {
      await respawnDaemon(selected.task_name, false);
      setBanner({ kind: "ok", text: "Restart requested." });
    } catch (err) {
      setBanner({ kind: "error", text: (err as Error).message });
    } finally {
      setBusy(false);
    }
  }

  return (
    <div class="daemon-env-settings" data-testid="daemon-env-settings">
      <h3 class="m-0 mb-1 text-xs font-semibold uppercase tracking-wide text-app-muted">Server env overrides</h3>

      <div class="daemon-env-body">
        <div class="flex flex-wrap items-center justify-between gap-x-4 gap-y-1.5 py-3">
          <label class="text-sm font-medium text-app-text" for="daemon-env-task">Daemon</label>
          <select
            id="daemon-env-task"
            class="field-ctl w-64 max-w-full"
            value={selected?.task_name ?? ""}
            disabled={busy || rows.length === 0}
            onChange={(e) => {
              const task = (e.currentTarget as HTMLSelectElement).value;
              // Same task re-selected → nothing to guard.
              if (task === (selected?.task_name ?? "")) return;
              // Unsaved edit on the current row → gate the switch behind a
              // ConfirmModal instead of silently overwriting the draft. The
              // <select> is controlled by selected?.task_name, so it visually
              // reverts to the current daemon until the operator confirms.
              if (envDirty) {
                setPendingSwitchTask(task);
                return;
              }
              performSwitch(task);
            }}
            data-testid="daemon-env-task"
          >
            {rows.length === 0 ? (
              <option value="">No supervised daemons</option>
            ) : (
              rows.map((row) => (
                <option key={row.task_name} value={row.task_name}>
                  {row.server}/{row.daemon}
                </option>
              ))
            )}
          </select>
        </div>
        {selected ? (
          <p class="daemon-env-taskname">{selected.task_name}</p>
        ) : null}

        <div class="daemon-env-kv">
          <label class="daemon-env-field">
            <span class="text-sm font-medium text-app-text">Key</span>
            <input
              type="text"
              class="field-ctl font-mono"
              value={key}
              disabled={busy || !selected}
              onInput={(e) => {
                const next = (e.currentTarget as HTMLInputElement).value;
                setKey(next);
                setValue(selected?.env[next] ?? value);
              }}
              data-testid="daemon-env-key"
            />
          </label>
          <label class="daemon-env-field">
            <span class="text-sm font-medium text-app-text">Value</span>
            <input
              type="text"
              class="field-ctl font-mono"
              value={value}
              placeholder="value"
              disabled={busy || !selected}
              onInput={(e) => setValue((e.currentTarget as HTMLInputElement).value)}
              data-testid="daemon-env-value"
            />
          </label>
        </div>

        <div class="daemon-env-actions">
          {banner ? (
            <span class={`save-banner ${banner.kind}`} role="status" data-testid="daemon-env-banner">
              {banner.text}
            </span>
          ) : null}
          <button type="button" class="btn-primary" disabled={busy || !selected || !envDirty} onClick={() => void apply()} data-testid="daemon-env-apply">
            Apply
          </button>
          <button type="button" disabled={busy} onClick={() => void refreshRows()} data-testid="daemon-env-refresh">
            Refresh
          </button>
          <button type="button" disabled={busy || !selected} onClick={() => void restart()} data-testid="daemon-env-restart">
            Restart
          </button>
        </div>
      </div>

      <ConfirmModal
        open={pendingSwitchTask !== null}
        title="Discard unsaved env changes?"
        body={
          <p>
            You have an unsaved env override for{" "}
            <b>{selected ? `${selected.server}/${selected.daemon}` : "this daemon"}</b>
            {selected ? <> (<code>{selected.task_name}</code>)</> : null}. Switching
            daemons discards that edit.
          </p>
        }
        confirmLabel="Discard and switch"
        danger
        onConfirm={confirmSwitch}
        onCancel={cancelSwitch}
      />
    </div>
  );
}
