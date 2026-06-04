import { useEffect, useMemo, useState } from "preact/hooks";
import {
  applyDaemonEnv,
  listDaemonEnv,
  respawnDaemon,
  type DaemonEnvRow,
} from "../../api";

const ENV_KEY_RE = /^[A-Za-z_][A-Za-z0-9_]*$/;

type Banner = { kind: "ok" | "error"; text: string };

function stableEnvSignature(env: Record<string, string> | undefined): string {
  if (!env) return "";
  return JSON.stringify(Object.entries(env).sort(([a], [b]) => a.localeCompare(b)));
}

export function DaemonEnvSettings(): preact.JSX.Element {
  const [rows, setRows] = useState<DaemonEnvRow[]>([]);
  const [selectedTask, setSelectedTask] = useState("");
  const [key, setKey] = useState("MEMORY_FILE_PATH");
  const [value, setValue] = useState("");
  const [busy, setBusy] = useState(false);
  const [banner, setBanner] = useState<Banner | null>(null);

  const selected = useMemo(
    () => rows.find((row) => row.task_name === selectedTask) ?? rows[0],
    [rows, selectedTask],
  );
  const selectedEnvSignature = useMemo(
    () => stableEnvSignature(selected?.env),
    [selected?.env],
  );

  useEffect(() => {
    void refreshRows();
  }, []);

  useEffect(() => {
    if (!selected) return;
    if (selectedTask === "") {
      setSelectedTask(selected.task_name);
    }
    const keys = Object.keys(selected.env).sort();
    const nextKey = keys.includes(key) ? key : keys[0] ?? key;
    if (nextKey !== key) {
      setKey(nextKey);
    }
    setValue(selected.env[nextKey] ?? "");
  }, [selected?.task_name, selectedEnvSignature]);

  async function refreshRows() {
    setBusy(true);
    setBanner(null);
    try {
      const result = await listDaemonEnv();
      setRows(result.daemons);
      if (selectedTask && result.daemons.every((row) => row.task_name !== selectedTask)) {
        setSelectedTask(result.daemons[0]?.task_name ?? "");
      }
    } catch (err) {
      setBanner({ kind: "error", text: (err as Error).message });
    } finally {
      setBusy(false);
    }
  }

  async function apply() {
    if (!selected) return;
    const trimmedKey = key.trim();
    if (!ENV_KEY_RE.test(trimmedKey)) {
      setBanner({ kind: "error", text: "Env key must match [A-Za-z_][A-Za-z0-9_]*." });
      return;
    }
    setBusy(true);
    setBanner(null);
    try {
      await applyDaemonEnv(selected.task_name, { [trimmedKey]: value });
      await refreshRows();
      setBanner({ kind: "ok", text: "Saved. Restart the daemon for the change to take effect." });
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
      <h3>Server env overrides</h3>
      <div class="settings-field-row">
        <label class="settings-field-label" for="daemon-env-task">Daemon</label>
        <select
          id="daemon-env-task"
          value={selected?.task_name ?? ""}
          disabled={busy || rows.length === 0}
          onChange={(e) => {
            const task = (e.currentTarget as HTMLSelectElement).value;
            setSelectedTask(task);
            const row = rows.find((r) => r.task_name === task);
            const keys = Object.keys(row?.env ?? {}).sort();
            const nextKey = keys[0] ?? "MEMORY_FILE_PATH";
            setKey(nextKey);
            setValue(row?.env[nextKey] ?? "");
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
        {selected ? <small class="settings-field-help">{selected.task_name}</small> : null}
      </div>

      <div class="daemon-env-grid">
        <label class="settings-field-row">
          <span class="settings-field-label">Key</span>
          <input
            type="text"
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
        <label class="settings-field-row">
          <span class="settings-field-label">Value</span>
          <input
            type="text"
            value={value}
            disabled={busy || !selected}
            onInput={(e) => setValue((e.currentTarget as HTMLInputElement).value)}
            data-testid="daemon-env-value"
          />
        </label>
      </div>

      <div class="settings-section-footer daemon-env-actions">
        {banner ? (
          <span class={`save-banner ${banner.kind}`} role="status" data-testid="daemon-env-banner">
            {banner.text}
          </span>
        ) : null}
        <button type="button" disabled={busy || !selected} onClick={() => void apply()} data-testid="daemon-env-apply">
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
  );
}
