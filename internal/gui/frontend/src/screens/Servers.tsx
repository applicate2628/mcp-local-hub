import { useEffect, useState } from "preact/hooks";
import { fetchOrThrow } from "../api";
import { collectServers } from "../lib/routing";
import { aggregateStatus } from "../lib/status";
import type { DaemonStatus, ScanResult, ServerRow, Routing } from "../types";

const CLIENTS = ["claude-code", "codex-cli", "gemini-cli", "antigravity"] as const;

// Per-cell dirty tracking: server name -> Set<client name>. A (server,
// client) pair is dirty iff the user's checked state differs from initial.
// Tracking per-cell (not per-server) is load-bearing: /api/migrate's
// ClientsInclude narrows the rewrite to the listed clients, so sending
// ONLY the affected pairs prevents one flipped checkbox from silently
// rewriting every other client binding on that server.
type DirtyMap = Map<string, Set<string>>;

export function ServersScreen() {
  const [servers, setServers] = useState<ServerRow[] | null>(null);
  const [statusByServer, setStatusByServer] = useState<Record<string, { state: string; port: number | null }>>({});
  const [error, setError] = useState<string | null>(null);
  const [dirty, setDirty] = useState<DirtyMap>(new Map());
  const [applyMsg, setApplyMsg] = useState<string>("");
  const [applying, setApplying] = useState<boolean>(false);
  const [reloadToken, setReloadToken] = useState<number>(0);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const [scan, status] = await Promise.all([
          fetchOrThrow<ScanResult>("/api/scan", "object"),
          fetchOrThrow<DaemonStatus[]>("/api/status", "array"),
        ]);
        if (cancelled) return;
        if (scan.entries != null && !Array.isArray(scan.entries)) {
          setError("/api/scan returned malformed entries");
          return;
        }
        setServers(collectServers(scan));
        const agg = aggregateStatus(status);
        const flat: Record<string, { state: string; port: number | null }> = {};
        for (const [name, a] of Object.entries(agg)) {
          flat[name] = { state: a.state, port: a.port };
        }
        setStatusByServer(flat);
        setError(null);
        setDirty(new Map());
      } catch (err) {
        if (!cancelled) setError((err as Error).message);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [reloadToken]);

  function toggleCell(server: string, client: string, nextChecked: boolean, initialChecked: boolean) {
    setDirty((prev) => {
      const next = new Map(prev);
      if (nextChecked !== initialChecked) {
        let clients = next.get(server);
        if (!clients) {
          clients = new Set();
          next.set(server, clients);
        }
        clients.add(client);
      } else {
        const clients = next.get(server);
        if (clients) {
          clients.delete(client);
          if (clients.size === 0) next.delete(server);
        }
      }
      return next;
    });
  }

  async function applyChanges() {
    // Migrate is called PER-SERVER-GROUP, not once with a unioned client
    // list. MigrateOpts.ClientsInclude is a global filter applied to every
    // Server in the request, so a single call with {servers:[A,B],
    // clients:[claude,gemini]} would rewrite all four cells (A×claude,
    // A×gemini, B×claude, B×gemini) even if the user only dirtied
    // (A,claude) and (B,gemini). Looping keeps each server's client list
    // scoped to exactly its own dirty cells.
    const changes = Array.from(dirty.entries())
      .filter(([, clients]) => clients.size > 0)
      .map(([server, clients]) => ({ server, clients: Array.from(clients) }));
    if (changes.length === 0) return;
    setApplying(true);
    setApplyMsg(`Migrating ${changes.length} server(s)…`);
    const failed: string[] = [];
    for (const change of changes) {
      try {
        const resp = await fetch("/api/migrate", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ servers: [change.server], clients: change.clients }),
        });
        if (!resp.ok) {
          const body = (await resp.json().catch(() => ({}))) as { error?: string };
          failed.push(`${change.server}: ${body.error ?? resp.status}`);
        }
      } catch (e) {
        failed.push(`${change.server}: ${(e as Error).message ?? "unknown"}`);
      }
    }
    if (failed.length === 0) {
      setApplyMsg("Migrated. Refreshing…");
      // Clear dirty BEFORE the reload effect completes so the Apply
      // button stays disabled through the refresh window. Without this,
      // setApplying(false) below re-enables Apply while the reload is
      // still fetching /api/scan + /api/status — the user could click
      // Apply again and resubmit the same /api/migrate POSTs. The
      // reload effect's own setDirty(new Map()) at completion is then
      // idempotent. (Codex CLI R2.)
      setDirty(new Map());
      setReloadToken((x) => x + 1);
    } else {
      setApplyMsg(`Failed: ${failed.join("; ")}`);
    }
    setApplying(false);
  }

  if (error) {
    return (
      <div>
        <h1>Servers</h1>
        <p class="error">Failed to load: {error}</p>
      </div>
    );
  }

  if (!servers) {
    return (
      <div>
        <h1>Servers</h1>
        <p>Loading…</p>
      </div>
    );
  }

  const applyDisabled = applying || dirty.size === 0;

  return (
    <div>
      <h1>Servers</h1>
      <div id="servers-toolbar">
        <button onClick={applyChanges} disabled={applyDisabled}>
          Apply changes
        </button>
        <span style="margin-left:12px" class={applyMsg.startsWith("Failed") ? "error" : ""}>
          {applyMsg}
        </span>
      </div>
      <table class="servers-matrix">
        <thead>
          <tr>
            <th>Server</th>
            {CLIENTS.map((c) => (
              <th key={c}>{c}</th>
            ))}
            <th>Port</th>
            <th>State</th>
          </tr>
        </thead>
        <tbody>
          {servers.map((server) => (
            <ServerRowView
              key={server.name}
              server={server}
              status={statusByServer[server.name]}
              onToggle={toggleCell}
            />
          ))}
        </tbody>
      </table>
    </div>
  );
}

function ServerRowView(props: {
  server: ServerRow;
  status?: { state: string; port: number | null };
  onToggle: (server: string, client: string, nextChecked: boolean, initialChecked: boolean) => void;
}) {
  const { server, status, onToggle } = props;
  return (
    <tr>
      <td>
        <a
          href={`#/edit-server?name=${encodeURIComponent(server.name)}`}
          data-action="edit-server"
        >
          {server.name}
        </a>
      </td>
      {CLIENTS.map((client) => (
        <CellView key={client} server={server} client={client} onToggle={onToggle} />
      ))}
      <td>{status?.port ?? "—"}</td>
      <td>{status?.state ?? "—"}</td>
    </tr>
  );
}

function CellView(props: {
  server: ServerRow;
  client: string;
  onToggle: (server: string, client: string, nextChecked: boolean, initialChecked: boolean) => void;
}) {
  const { server, client, onToggle } = props;
  // Treat undefined routing as "not-installed" — perClientRouting only
  // populates keys present in /api/scan's client_presence map.
  const routing: Routing = server.routing[client] ?? "not-installed";
  const initialChecked = routing === "via-hub";
  const [checked, setChecked] = useState(initialChecked);
  // Keep local `checked` in sync with the authoritative initialChecked
  // when routing actually changes (a scan reload moving a cell from
  // direct→via-hub, an external config change, etc.). Deps `[initialChecked]`
  // means unrelated parent re-renders do not stomp an in-progress user edit.
  useEffect(() => {
    setChecked(initialChecked);
  }, [initialChecked]);
  // Disable when:
  //  - "unsupported" or "not-installed": cell is meaningless
  //  - "via-hub": MVP has no reverse-migrate API yet (Phase 3B-II B1).
  //    Allowing uncheck would let the user dirty, Apply, and receive a
  //    silent no-op because MigrateFrom is idempotent on already-migrated
  //    bindings.
  const disabled = routing === "unsupported" || routing === "not-installed" || routing === "via-hub";
  let title: string | undefined;
  if (routing === "via-hub") {
    title = `Already routed through the hub. To disable, run \`mcphub rollback --client ${client}\` (Phase 3B-II will add a UI for this).`;
  } else if (routing === "not-installed") {
    title = `${client} is not installed on this machine.`;
  } else if (routing === "unsupported") {
    title = `${client} cannot route this server through the hub (e.g., per-session servers).`;
  }
  return (
    <td>
      <input
        type="checkbox"
        checked={checked}
        disabled={disabled}
        title={title}
        onChange={(ev) => {
          const next = (ev.currentTarget as HTMLInputElement).checked;
          setChecked(next);
          onToggle(server.name, client, next, initialChecked);
        }}
      />
    </td>
  );
}
