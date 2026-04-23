import { useEffect, useState } from "preact/hooks";
import { fetchOrThrow } from "../api";
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
        }
      } catch (err) {
        if (!cancelled) setError((err as Error).message);
      }
    })();
    return () => { cancelled = true; };
  }, [scanReloadToken]);

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
          <GroupSection
            title="Can migrate"
            tone="can-migrate"
            entries={groups.canMigrate}
            emptyLabel="No stdio entries with matching manifests."
          />
          <GroupSection
            title="Unknown"
            tone="unknown"
            entries={groups.unknown}
            emptyLabel="No unknown stdio entries."
          />
          <GroupSection
            title="Per-session"
            tone="per-session"
            entries={groups.perSession}
            emptyLabel="No per-session entries."
          />
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

// GroupSection is a minimal per-group row-list renderer shared by the
// scaffolding. Task 6 replaces the Via-hub call with ViaHubGroup
// (Demigrate per row); Task 7 replaces Can-migrate / Unknown with
// CanMigrateGroup (pre-checked + Migrate-selected) and UnknownGroup
// (disabled Create-manifest + Dismiss); Task 8 replaces Per-session
// with PerSessionGroup and removes this generic renderer.
function GroupSection(props: {
  title: string;
  tone: "via-hub" | "can-migrate" | "unknown" | "per-session";
  entries: Array<{ name: string }>;
  emptyLabel: string;
}) {
  return (
    <section class={`group group-${props.tone}`} data-group={props.tone}>
      <h2>{props.title}</h2>
      {props.entries.length === 0 ? (
        <p class="empty">{props.emptyLabel}</p>
      ) : (
        <ul class="group-rows">
          {props.entries.map((e) => (
            <li key={e.name} data-server={e.name}>
              <span class="server-name">{e.name}</span>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
