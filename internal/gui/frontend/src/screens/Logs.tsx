import { useCallback, useEffect, useMemo, useRef, useState } from "preact/hooks";
import { fetchOrThrow } from "../api";
import { useEventSource } from "../hooks/useEventSource";
import type { DaemonStatus } from "../types";

interface PickerEntry {
  value: string; // JSON-encoded {server, daemon}
  label: string;
  server: string;
  daemon: string;
}

export function LogsScreen() {
  const [entries, setEntries] = useState<PickerEntry[] | null>(null);
  const [selected, setSelected] = useState<string>("");
  const [tail, setTail] = useState<number>(500);
  const [follow, setFollow] = useState<boolean>(false);
  const [body, setBody] = useState<string>("");
  const [notice, setNotice] = useState<string>("");
  const preRef = useRef<HTMLPreElement | null>(null);

  // Initial status load + picker population. Filters:
  //  - is_workspace_scoped: lazy-proxy daemons write to
  //    lsp-<workspaceKey>-<language>.log, not <server>-<daemon>.log that
  //    api.LogsGet reads. Picking them would 404 the stream. Phase 3B-II
  //    adds proper workspace log surfacing.
  //  - is_maintenance: weekly-refresh tasks have no server name and no
  //    matching log file. Derived server-side from the structural flag
  //    to avoid JS-side task_name regex drift.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const rows = await fetchOrThrow<DaemonStatus[]>("/api/status", "array");
        if (cancelled) return;
        const eligible = rows.filter((r) => !r.is_workspace_scoped && !r.is_maintenance);
        const picker: PickerEntry[] = eligible.map((r) => ({
          value: JSON.stringify({ server: r.server, daemon: r.daemon || "" }),
          label: r.daemon && r.daemon !== "default" ? `${r.server} (${r.daemon})` : r.server,
          server: r.server,
          daemon: r.daemon || "",
        }));
        setEntries(picker);
        if (picker.length > 0) {
          setSelected(picker[0].value);
        } else {
          const hiddenCount = rows.length - eligible.length;
          setNotice(
            hiddenCount > 0
              ? `No global-server logs available (${hiddenCount} workspace-proxy entries hidden — Phase 3B-II will surface their lsp-<key>-<lang>.log files).`
              : "No daemons running.",
          );
        }
      } catch (err) {
        if (!cancelled) setNotice("Failed to load status: " + (err as Error).message);
        setEntries([]);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  // Fetch snapshot whenever (selected, tail, reloadToken) changes.
  // Follow=true triggers the SSE subscription below AFTER the snapshot so
  // the user sees the static tail first then the live appends.
  const [reloadToken, setReloadToken] = useState<number>(0);
  useEffect(() => {
    if (!selected) return;
    let cancelled = false;
    (async () => {
      const { server, daemon } = JSON.parse(selected) as { server: string; daemon: string };
      setBody("Loading…");
      const qs =
        `tail=${encodeURIComponent(String(tail))}` +
        (daemon ? `&daemon=${encodeURIComponent(daemon)}` : "");
      const resp = await fetch(`/api/logs/${encodeURIComponent(server)}?${qs}`);
      const text = await resp.text();
      if (!cancelled) setBody(text);
    })();
    return () => {
      cancelled = true;
    };
  }, [selected, tail, reloadToken]);

  const streamUrl = useMemo(() => {
    if (!follow || !selected) return null;
    const { server, daemon } = JSON.parse(selected) as { server: string; daemon: string };
    const qs = daemon ? `?daemon=${encodeURIComponent(daemon)}` : "";
    return `/api/logs/${encodeURIComponent(server)}/stream${qs}`;
  }, [follow, selected]);

  const onLine = useCallback((ev: MessageEvent) => {
    setBody((prev) => {
      const next = prev + ev.data + "\n";
      // Schedule an auto-scroll after Preact commits.
      queueMicrotask(() => {
        const pre = preRef.current;
        if (pre) pre.scrollTop = pre.scrollHeight;
      });
      return next;
    });
  }, []);

  useEventSource(streamUrl, { "log-line": onLine });

  const controlsDisabled = entries !== null && entries.length === 0;

  return (
    <div>
      <h1>Logs</h1>
      <div id="logs-controls">
        <select
          value={selected}
          disabled={controlsDisabled}
          onChange={(ev) => setSelected((ev.currentTarget as HTMLSelectElement).value)}
        >
          {(entries ?? []).map((e) => (
            <option key={e.value} value={e.value}>
              {e.label}
            </option>
          ))}
        </select>
        <label>
          <input
            type="number"
            value={tail}
            min={1}
            max={10000}
            disabled={controlsDisabled}
            onChange={(ev) => {
              const n = Number((ev.currentTarget as HTMLInputElement).value);
              if (Number.isFinite(n) && n >= 1) setTail(n);
            }}
          />{" "}
          lines
        </label>
        <label>
          <input
            type="checkbox"
            checked={follow}
            disabled={controlsDisabled}
            onChange={(ev) => setFollow((ev.currentTarget as HTMLInputElement).checked)}
          />{" "}
          Follow
        </label>
        <button disabled={controlsDisabled} onClick={() => setReloadToken((x) => x + 1)}>
          Refresh
        </button>
      </div>
      <pre id="logs-body" ref={preRef}>
        {notice || body}
      </pre>
    </div>
  );
}
