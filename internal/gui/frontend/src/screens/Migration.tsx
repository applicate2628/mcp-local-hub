import { useEffect, useState } from "preact/hooks";
import { fetchOrThrow, postDismiss } from "../api";
import { useEventSource } from "../hooks/useEventSource";
import { groupMigrationEntries, type MigrationGroups } from "../lib/migration-grouping";
import type { ScanEntry, ScanResult } from "../types";

// DismissedResponse mirrors the /api/dismissed handler shape from
// internal/gui/dismiss.go. Declared inline here rather than in
// types.ts because no other screen needs it today; promote to
// types.ts if A4 Settings reuses it.
interface DismissedResponse {
  unknown: string[];
}

// MigrationScreen is the §5.2 Migration view: scan-driven grouping of
// MCP server entries across all four supported clients, with per-group
// actions (Demigrate in Task 6; Migrate selected + Dismiss + gated
// Create-manifest in Task 7; Per-session readonly in Task 8). This
// scaffolding ships h1, parallel /api/scan + /api/dismissed fetches,
// groupMigrationEntries wiring with the dismissed-unknowns filter,
// empty-state copy, and the per-group scaffolding component so the
// route + router are testable end-to-end before the action handlers land.
export function MigrationScreen() {
  const [scan, setScan] = useState<ScanResult | null>(null);
  const [dismissedUnknown, setDismissedUnknown] = useState<Set<string>>(new Set());
  const [error, setError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [actionBusy, setActionBusy] = useState<string | null>(null); // server name being demigrated
  const [scanReloadToken, setScanReloadToken] = useState(0);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [migrateBusy, setMigrateBusy] = useState<boolean>(false);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const [s, d] = await Promise.all([
          fetchOrThrow<ScanResult>("/api/scan", "object"),
          fetchOrThrow<DismissedResponse>("/api/dismissed", "object"),
        ]);
        if (!cancelled) {
          setScan(s);
          setDismissedUnknown(new Set(d.unknown ?? []));
          setError(null);
          const canMigrateNames = (s.entries ?? [])
            .filter((e) => e.status === "can-migrate")
            .map((e) => e.name);
          setSelected(new Set(canMigrateNames));
        }
      } catch (err) {
        if (!cancelled) setError((err as Error).message);
      }
    })();
    return () => { cancelled = true; };
  }, [scanReloadToken]);

  // SSE refresh: any out-of-band change (another GUI tab migrated, CLI
  // ran on this machine, user hand-edited .claude.json) should refresh
  // the view. Migrate/Demigrate/Dismiss local actions already bump
  // scanReloadToken on success; SSE covers the rest. Event names here
  // are whatever the hub broadcaster (internal/gui/events.go) actually
  // emits — keep the subscription narrow so unknown events do not cause
  // pointless rescans.
  useEventSource("/api/events", {
    "daemon-state": () => setScanReloadToken((n) => n + 1),
  });

  async function runDemigrate(serverName: string) {
    setActionBusy(serverName);
    setActionError(null);
    try {
      const resp = await fetch("/api/demigrate", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ servers: [serverName] }),
      });
      if (!resp.ok && resp.status !== 204) {
        const body = await resp.json().catch(() => ({ error: resp.statusText }));
        throw new Error(body?.error ?? `HTTP ${resp.status}`);
      }
      setScanReloadToken((n) => n + 1);
    } catch (err) {
      setActionError(`Demigrate ${serverName}: ${(err as Error).message}`);
    } finally {
      setActionBusy(null);
    }
  }

  async function runMigrateSelected() {
    if (selected.size === 0) return;
    setMigrateBusy(true);
    setActionError(null);
    try {
      const resp = await fetch("/api/migrate", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ servers: [...selected] }),
      });
      if (!resp.ok && resp.status !== 204) {
        const body = await resp.json().catch(() => ({ error: resp.statusText }));
        throw new Error(body?.error ?? `HTTP ${resp.status}`);
      }
      setScanReloadToken((n) => n + 1);
    } catch (err) {
      setActionError(`Migrate selected: ${(err as Error).message}`);
    } finally {
      setMigrateBusy(false);
    }
  }

  function toggleSelected(name: string, next: boolean) {
    setSelected((prev) => {
      const s = new Set(prev);
      if (next) s.add(name);
      else s.delete(name);
      return s;
    });
  }

  async function runDismiss(entry: ScanEntry) {
    setActionError(null);
    try {
      await postDismiss(entry.name);
      setScanReloadToken((n) => n + 1);
    } catch (err) {
      setActionError(`Dismiss ${entry.name}: ${(err as Error).message}`);
    }
  }

  const groups: MigrationGroups = scan
    ? groupMigrationEntries(scan, dismissedUnknown)
    : { viaHub: [], canMigrate: [], unknown: [], perSession: [] };

  if (error) {
    return (
      <section class="screen migration">
        <h1>Migration</h1>
        <p class="error">{error}</p>
      </section>
    );
  }
  if (scan == null) {
    return (
      <section class="screen migration">
        <h1>Migration</h1>
        <p>Loading…</p>
      </section>
    );
  }

  const totalRows =
    groups.viaHub.length +
    groups.canMigrate.length +
    groups.unknown.length +
    groups.perSession.length;

  return (
    <section class="screen migration">
      <h1>Migration</h1>
      {actionError && <p class="error action-error">{actionError}</p>}
      {totalRows === 0 ? (
        <p class="empty-state">
          No MCP servers found across any client config. Install or configure
          an MCP server in Claude Code, Codex CLI, Gemini CLI, or Antigravity
          to see it here.
        </p>
      ) : (
        <>
          <ViaHubGroup
            entries={groups.viaHub}
            actionBusy={actionBusy}
            onDemigrate={runDemigrate}
          />
          <CanMigrateGroup
            entries={groups.canMigrate}
            selected={selected}
            onToggle={toggleSelected}
            onMigrateSelected={runMigrateSelected}
            migrateBusy={migrateBusy}
          />
          <UnknownGroup
            entries={groups.unknown}
            onDismiss={runDismiss}
          />
          <PerSessionGroup entries={groups.perSession} />
        </>
      )}
      <button
        type="button"
        class="rescan"
        onClick={() => setScanReloadToken((n) => n + 1)}
      >
        Rescan
      </button>
    </section>
  );
}

function ViaHubGroup(props: {
  entries: ScanEntry[];
  actionBusy: string | null;
  onDemigrate: (server: string) => void;
}) {
  if (props.entries.length === 0) {
    return (
      <section class="group group-via-hub" data-group="via-hub">
        <h2>Via hub</h2>
        <p class="empty">No hub-routed entries yet.</p>
      </section>
    );
  }
  return (
    <section class="group group-via-hub" data-group="via-hub">
      <h2>Via hub</h2>
      <ul class="group-rows">
        {props.entries.map((e) => (
          <li key={e.name} data-server={e.name}>
            <span class="server-name">{e.name}</span>
            <button
              type="button"
              class="demigrate"
              data-action="demigrate"
              disabled={props.actionBusy != null}
              onClick={() => props.onDemigrate(e.name)}
            >
              {props.actionBusy === e.name ? "Demigrating…" : "Demigrate"}
            </button>
          </li>
        ))}
      </ul>
    </section>
  );
}

function CanMigrateGroup(props: {
  entries: ScanEntry[];
  selected: Set<string>;
  onToggle: (name: string, next: boolean) => void;
  onMigrateSelected: () => void;
  migrateBusy: boolean;
}) {
  if (props.entries.length === 0) {
    return (
      <section class="group group-can-migrate" data-group="can-migrate">
        <h2>Can migrate</h2>
        <p class="empty">No stdio entries with matching manifests.</p>
      </section>
    );
  }
  const selectedInGroup = props.entries.filter((e) => props.selected.has(e.name)).length;
  return (
    <section class="group group-can-migrate" data-group="can-migrate">
      <h2>Can migrate</h2>
      <ul class="group-rows">
        {props.entries.map((e) => (
          <li key={e.name} data-server={e.name}>
            <label>
              <input
                type="checkbox"
                data-action="select"
                checked={props.selected.has(e.name)}
                onChange={(ev) =>
                  props.onToggle(e.name, (ev.currentTarget as HTMLInputElement).checked)
                }
              />
              <span class="server-name">{e.name}</span>
            </label>
          </li>
        ))}
      </ul>
      <button
        type="button"
        class="migrate-selected"
        data-action="migrate-selected"
        disabled={selectedInGroup === 0 || props.migrateBusy}
        onClick={props.onMigrateSelected}
      >
        {props.migrateBusy ? "Migrating…" : `Migrate selected (${selectedInGroup})`}
      </button>
    </section>
  );
}

function UnknownGroup(props: {
  entries: ScanEntry[];
  onDismiss: (entry: ScanEntry) => void;
}) {
  if (props.entries.length === 0) {
    return (
      <section class="group group-unknown" data-group="unknown">
        <h2>Unknown</h2>
        <p class="empty">No unknown stdio entries.</p>
      </section>
    );
  }
  return (
    <section class="group group-unknown" data-group="unknown">
      <h2>Unknown</h2>
      <ul class="group-rows">
        {props.entries.map((e) => (
          <li key={e.name} data-server={e.name}>
            <span class="server-name">{e.name}</span>
            <button
              type="button"
              class="create-manifest"
              data-action="create-manifest"
              disabled
              title="Available after A2 (Add/Edit manifest) ships"
            >
              Create manifest
            </button>
            <button
              type="button"
              class="dismiss"
              data-action="dismiss"
              onClick={() => props.onDismiss(e)}
            >
              Dismiss
            </button>
          </li>
        ))}
      </ul>
    </section>
  );
}

function PerSessionGroup(props: { entries: ScanEntry[] }) {
  if (props.entries.length === 0) {
    return (
      <section class="group group-per-session" data-group="per-session">
        <h2>Per-session</h2>
        <p class="empty">No per-session entries.</p>
      </section>
    );
  }
  return (
    <section class="group group-per-session" data-group="per-session">
      <h2>Per-session</h2>
      <p class="info">
        These entries are shareable per-session only (e.g. running IDE
        integrations). They cannot be migrated into the hub and do not
        support Demigrate.
      </p>
      <ul class="group-rows">
        {props.entries.map((e) => (
          <li key={e.name} data-server={e.name}>
            <span class="server-name">{e.name}</span>
          </li>
        ))}
      </ul>
    </section>
  );
}
