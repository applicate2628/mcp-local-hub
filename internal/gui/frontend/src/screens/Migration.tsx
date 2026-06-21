import { useCallback, useEffect, useRef, useState } from "preact/hooks";
import { fetchOrThrow, postDismiss } from "../api";
import { InfoTip } from "../components/InfoTip";
import { ScanRefreshControls } from "../components/ScanRefreshControls";
import { useAutoScan } from "../hooks/useAutoScan";
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

// DiscoveryScreen (formerly MigrationScreen) is the comprehensive
// "see ALL MCP servers" view: a scan-driven grouping of every MCP
// server entry across all managed clients, with hub-managed servers
// flagged separately from unmanaged / external remotes. Groups:
//   - "Managed by hub" (via-hub, badged via the Managed flag) — Demigrate.
//   - "Ready to migrate" (can-migrate) — Migrate-selected.
//   - "Unmanaged / External" (unknown stdio + external remote http) —
//     Create-manifest (unknown) / read-only (external) + Dismiss.
//   - "Per-session" (read-only info).
//   - A collapsed, expandable "Dismissed" section so dismissed entries
//     are parked, not lost.
//
// The export is still named MigrationScreen-compatible via the alias at
// the bottom of the file so the route key `migration` in app.tsx keeps
// working unchanged; the user-facing label is "Discovery".
export function DiscoveryScreen() {
  const [scan, setScan] = useState<ScanResult | null>(null);
  const [dismissedUnknown, setDismissedUnknown] = useState<Set<string>>(new Set());
  const [error, setError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [actionBusy, setActionBusy] = useState<string | null>(null); // server name being demigrated
  const [scanReloadToken, setScanReloadToken] = useState(0);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [migrateBusy, setMigrateBusy] = useState<boolean>(false);
  // Monotonic guard for scan refreshes. Auto-refresh, manual Rescan,
  // reload-token effects, and SSE can overlap; only the newest request may
  // publish state so a slow older response cannot replace fresher results.
  const latestScanRequestRef = useRef(0);
  const didInitSelection = useRef(false);

  // loadScan is the single fetch path for this screen. It is driven by
  // three sources, all calling the SAME closure so the rendered baseline
  // reconciles identically regardless of trigger:
  //   - useAutoScan (initial mount fetch + 10s poll + on-visible refresh),
  //   - the scanReloadToken effect (local Migrate/Demigrate/Dismiss
  //     actions + SSE daemon-state / clients-rescan bumps),
  //   - the "Rescan now" button (via useAutoScan.rescan).
  // Discovery has NO edit/dirty state, so auto-refresh is never paused and
  // a refetch can never clobber pending work — it just re-derives groups
  // and prunes the `selected` set against the fresh can-migrate names.
  const loadScan = useCallback(async () => {
    const requestId = latestScanRequestRef.current + 1;
    latestScanRequestRef.current = requestId;
    const isLatest = () => latestScanRequestRef.current === requestId;
    try {
      // /api/scan is authoritative — its failure means we cannot render
      // the screen. /api/dismissed is auxiliary — a transient
      // permission/read error on gui-dismissed.json should degrade to
      // "no dismissals known" rather than blanking the entire screen
      // (which would also block Demigrate / Migrate selected for
      // unaffected groups). (PR #4 Codex R2.)
      const dismissedFallback: DismissedResponse = { unknown: [] };
      const [s, d] = await Promise.all([
        fetchOrThrow<ScanResult>("/api/scan", "object"),
        fetchOrThrow<DismissedResponse>("/api/dismissed", "object").catch(
          (err: unknown) => {
            console.warn(
              "Discovery: /api/dismissed failed, rendering without dismissal filter:",
              err,
            );
            return dismissedFallback;
          },
        ),
      ]);
      if (!isLatest()) return;
      setScan(s);
      setDismissedUnknown(new Set(d.unknown ?? []));
      setError(null);
      const canMigrateNames = (s.entries ?? [])
        .filter((e) => e.status === "can-migrate")
        .map((e) => e.name);
      const canMigrateSet = new Set(canMigrateNames);
      setSelected((prev) => {
        if (!didInitSelection.current) {
          didInitSelection.current = true;
          return new Set(canMigrateNames);
        }
        // Preserve the operator's current selection across a poll/refresh,
        // dropping only names that are no longer can-migrate (already
        // migrated out-of-band). This keeps an auto-refresh tick from
        // re-checking everything the operator deliberately unchecked.
        const next = new Set<string>();
        prev.forEach((name) => {
          if (canMigrateSet.has(name)) next.add(name);
        });
        return next;
      });
    } catch (err) {
      if (isLatest()) setError((err as Error).message);
    }
  }, []);

  // Auto-refresh: 10s poll while mounted, paused while the tab is hidden,
  // immediate refresh on becoming visible again. Discovery is never
  // edit-paused (no dirty state) so `paused` is a constant false.
  const { rescan, agoSeconds } = useAutoScan(loadScan, false);

  // scanReloadToken-driven refetch for local actions + SSE. useAutoScan
  // already issues the initial mount fetch, so this effect skips token 0
  // to avoid a redundant duplicate fetch on first paint.
  useEffect(() => {
    if (scanReloadToken === 0) return;
    void loadScan();
  }, [scanReloadToken, loadScan]);

  // SSE refresh: any out-of-band change (another GUI tab migrated, CLI
  // ran on this machine, user hand-edited .claude.json) should refresh
  // the view. Migrate/Demigrate/Dismiss local actions already bump
  // scanReloadToken on success; SSE covers the rest. Event names here
  // are whatever the hub broadcaster (internal/gui/events.go) actually
  // emits — keep the subscription narrow so unknown events do not cause
  // pointless rescans.
  useEventSource("/api/events", {
    "daemon-state": () => setScanReloadToken((n) => n + 1),
    // Tray "Rescan client configs" → backend publishes clients-rescan
    // → every open Servers/Discovery screen re-fetches. The UI side
    // is a thin re-trigger of the existing reload mechanism so the
    // tray click and a manual refresh share one code path.
    "clients-rescan": () => setScanReloadToken((n) => n + 1),
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
      // Bug-bash B1 closure (#7): the handler now distinguishes
      // 200 (full success), 207 (partial — Failed[] has rows), and
      // 4xx/5xx (setup error). 204 is kept for forward compat with
      // test fakes that may still return it.
      if (resp.status === 200 || resp.status === 204) {
        setScanReloadToken((n) => n + 1);
        return;
      }
      if (resp.status === 207) {
        // Partial-failure body: {restored: [...], failed: [{server, client, err}]}.
        const body = (await resp.json().catch(() => ({}))) as {
          failed?: { server: string; client: string; err: string }[];
        };
        const rows = body.failed ?? [];
        // Demigrate at the Discovery screen targets one server, so
        // every failed row is the same server — render the client
        // names + error messages on separate lines.
        const detail = rows.map((r) => `${r.client}: ${r.err}`).join("\n");
        // Bot r1 P2 closure on PR #182: even on partial failure, the
        // backend MAY have restored some rows (report.restored). Reload
        // scan state so the Discovery screen reflects that — leaving
        // successful rows in stale "can-migrate" view encourages a
        // double-action against already-restored clients.
        setScanReloadToken((n) => n + 1);
        throw new Error(
          rows.length === 0
            ? "demigrate partial failure (no row details returned)"
            : detail,
        );
      }
      const body = await resp.json().catch(() => ({ error: resp.statusText }));
      throw new Error(body?.error ?? `HTTP ${resp.status}`);
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
      // B1 #7 symmetry: same 200/207/4xx-5xx triad as /api/demigrate.
      if (resp.status === 200 || resp.status === 204) {
        setScanReloadToken((n) => n + 1);
        return;
      }
      if (resp.status === 207) {
        const body = (await resp.json().catch(() => ({}))) as {
          failed?: { server: string; client: string; err: string }[];
        };
        const rows = body.failed ?? [];
        const detail = rows
          .map((r) => `${r.server}/${r.client}: ${r.err}`)
          .join("\n");
        // Bot r1 P2 closure on PR #182 (symmetric with /api/demigrate):
        // partial-failure 207 may still report successful rows in
        // report.applied[]. Reload scan state so the UI removes them
        // from the "can-migrate" view; otherwise the operator might
        // unnecessarily retry already-migrated servers.
        setScanReloadToken((n) => n + 1);
        throw new Error(
          rows.length === 0
            ? "migrate partial failure (no row details returned)"
            : detail,
        );
      }
      const body = await resp.json().catch(() => ({ error: resp.statusText }));
      throw new Error(body?.error ?? `HTTP ${resp.status}`);
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
    : { viaHub: [], canMigrate: [], unknown: [], external: [], perSession: [], dismissed: [] };

  if (error) {
    return (
      <section class="screen migration discovery">
        <h1>Discovery</h1>
        <p class="error">{error}</p>
      </section>
    );
  }
  if (scan == null) {
    return (
      <section class="screen migration discovery">
        <h1>Discovery</h1>
        <p>Loading…</p>
      </section>
    );
  }

  const totalRows =
    groups.viaHub.length +
    groups.canMigrate.length +
    groups.unknown.length +
    groups.external.length +
    groups.perSession.length +
    groups.dismissed.length;

  return (
    <section class="screen migration discovery">
      <div class="screen-header-row">
        <h1>Discovery</h1>
        <ScanRefreshControls agoSeconds={agoSeconds} onRescan={rescan} />
      </div>
      <p class="discovery-intro">
        Every MCP server found across all client configs. Servers routed
        through the hub are flagged separately from unmanaged and external
        remotes.
      </p>
      {actionError && <p class="error action-error">{actionError}</p>}
      {totalRows === 0 ? (
        <p class="empty-state">
          No MCP servers found across any client config. Install or configure
          an MCP server in Claude Code, Codex CLI, Gemini CLI, or Antigravity
          to see it here.
        </p>
      ) : (
        <div class="card">
          <ManagedByHubGroup
            entries={groups.viaHub}
            actionBusy={actionBusy}
            onDemigrate={runDemigrate}
          />
          <ReadyToMigrateGroup
            entries={groups.canMigrate}
            selected={selected}
            onToggle={toggleSelected}
            onMigrateSelected={runMigrateSelected}
            migrateBusy={migrateBusy}
          />
          <UnmanagedExternalGroup
            unknownEntries={groups.unknown}
            externalEntries={groups.external}
            onDismiss={runDismiss}
          />
          <PerSessionGroup entries={groups.perSession} />
          <DismissedGroup
            entries={groups.dismissed}
          />
        </div>
      )}
    </section>
  );
}

function ManagedByHubGroup(props: {
  entries: ScanEntry[];
  actionBusy: string | null;
  onDemigrate: (server: string) => void;
}) {
  if (props.entries.length === 0) {
    return (
      <section class="group group-via-hub" data-group="via-hub">
        <h2>Managed by hub</h2>
        <p class="empty">No hub-routed entries yet.</p>
      </section>
    );
  }
  return (
    <section class="group group-via-hub" data-group="via-hub">
      <h2>Managed by hub</h2>
      <ul class="group-rows">
        {props.entries.map((e) => (
          <li key={e.name} data-server={e.name}>
            <span class="server-name">{e.name}</span>
            {/* Badge driven by the backend Managed flag (Status==="via-hub").
                Rendered only when the flag is actually true so an older
                fixture without the field doesn't show a misleading badge. */}
            {e.managed && (
              <span class="badge badge-managed" data-testid={`managed-badge-${e.name}`}>
                Managed by hub
              </span>
            )}
            <a
              href={`#/edit-server?name=${encodeURIComponent(e.name)}`}
              data-action="edit-manifest"
            >Edit manifest</a>
            <button
              type="button"
              class="demigrate btn"
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

function ReadyToMigrateGroup(props: {
  entries: ScanEntry[];
  selected: Set<string>;
  onToggle: (name: string, next: boolean) => void;
  onMigrateSelected: () => void;
  migrateBusy: boolean;
}) {
  if (props.entries.length === 0) {
    return (
      <section class="group group-can-migrate" data-group="can-migrate">
        <h2>Ready to migrate</h2>
        <p class="empty">No stdio entries with matching manifests.</p>
      </section>
    );
  }
  const selectedInGroup = props.entries.filter((e) => props.selected.has(e.name)).length;
  return (
    <section class="group group-can-migrate" data-group="can-migrate">
      <h2>Ready to migrate</h2>
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
        class="migrate-selected btn btn-primary"
        data-action="migrate-selected"
        disabled={selectedInGroup === 0 || props.migrateBusy}
        onClick={props.onMigrateSelected}
      >
        {props.migrateBusy ? "Migrating…" : `Migrate selected (${selectedInGroup})`}
      </button>
    </section>
  );
}

// UnmanagedExternalGroup renders the two "unmanaged" buckets together:
//   - unknown: stdio entries with no manifest — operator can Create
//     manifest to adopt them into the hub, or Dismiss.
//   - external: real external remote MCP servers (non-hub http). These
//     are read-only (no Create-manifest — they're remote, not stdio) but
//     CAN be Dismissed to park a noisy remote.
function UnmanagedExternalGroup(props: {
  unknownEntries: ScanEntry[];
  externalEntries: ScanEntry[];
  onDismiss: (entry: ScanEntry) => void;
}) {
  const total = props.unknownEntries.length + props.externalEntries.length;
  if (total === 0) {
    return (
      <section class="group group-unmanaged" data-group="unmanaged">
        <h2>Unmanaged / External</h2>
        <p class="empty">No unmanaged or external MCP servers.</p>
      </section>
    );
  }
  return (
    <section class="group group-unmanaged" data-group="unmanaged">
      <div class="group-heading">
        <h2>Unmanaged / External</h2>
        <InfoTip
          label="About unmanaged / external entries"
          text="Unknown stdio entries (no mcphub manifest) can be adopted with Create manifest. External remotes are real off-host MCP servers (e.g. context7, qt-docs) routed directly by the client — they are shown read-only so you can see every MCP server, not just hub-managed ones. Dismiss parks an entry in the collapsed Dismissed section below."
        />
      </div>
      {props.unknownEntries.length > 0 && (
        <ul class="group-rows group-rows-unknown" data-subgroup="unknown">
          {props.unknownEntries.map((e) => (
            <li key={e.name} data-server={e.name}>
              <span class="server-name">{e.name}</span>
              <span class="badge badge-unknown">Unknown stdio</span>
              <button
                type="button"
                class="create-manifest btn"
                data-action="create-manifest"
                onClick={() => {
                  const client = firstClientFor(e);
                  const url = client
                    ? `#/add-server?server=${encodeURIComponent(e.name)}&from-client=${encodeURIComponent(client)}`
                    : `#/add-server?server=${encodeURIComponent(e.name)}`;
                  window.location.hash = url;
                }}
              >
                Create manifest
              </button>
              <button
                type="button"
                class="dismiss btn btn-danger"
                data-action="dismiss"
                onClick={() => props.onDismiss(e)}
              >
                Dismiss
              </button>
            </li>
          ))}
        </ul>
      )}
      {props.externalEntries.length > 0 && (
        <ul class="group-rows group-rows-external" data-subgroup="external">
          {props.externalEntries.map((e) => (
            <li key={e.name} data-server={e.name}>
              <span class="server-name">{e.name}</span>
              <span class="badge badge-external" data-testid={`external-badge-${e.name}`}>
                External remote
              </span>
              <span class="external-endpoint" title={externalEndpointFor(e)}>
                {externalEndpointFor(e)}
              </span>
              <button
                type="button"
                class="dismiss btn btn-danger"
                data-action="dismiss"
                onClick={() => props.onDismiss(e)}
              >
                Dismiss
              </button>
            </li>
          ))}
        </ul>
      )}
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
      <div class="group-heading">
        <h2>Per-session</h2>
        <InfoTip
          label="About per-session entries"
          text="These entries are shareable per-session only (e.g. running IDE integrations). They cannot be migrated into the hub and do not support Demigrate."
        />
      </div>
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

// DismissedGroup is a COLLAPSED, expandable section so dismissed entries
// are visible-on-demand rather than dropped. STOP hiding dismissed
// entirely (prior behavior): the operator can expand to see what was
// parked. Re-instating a dismissed entry is a future affordance; for now
// expansion is read-only visibility.
function DismissedGroup(props: { entries: ScanEntry[] }) {
  if (props.entries.length === 0) {
    // Render nothing when there are no dismissed entries — an empty
    // <details> would be noise.
    return null;
  }
  return (
    <details class="group group-dismissed" data-group="dismissed">
      <summary>
        <strong>Dismissed ({props.entries.length})</strong>
        {" — "}
        entries you hid from the live list; expand to review
      </summary>
      <ul class="group-rows">
        {props.entries.map((e) => (
          <li key={e.name} data-server={e.name}>
            <span class="server-name">{e.name}</span>
            <span class="badge badge-dismissed">
              {e.status === "external" ? "External remote" : "Unknown stdio"}
            </span>
          </li>
        ))}
      </ul>
    </details>
  );
}

// firstClientFor picks a sensible client name to extract from for a given
// Unknown scan entry. The stdio entry may live in any one client's config
// (typically the user had the server set up in Claude Code first). We pick
// the first client that has a stdio transport for the entry; fallback:
// empty string, which still navigates (fresh-create with just the name).
function firstClientFor(entry: { client_presence?: Record<string, { transport?: string }> }): string {
  const presence = entry.client_presence ?? {};
  for (const [client, info] of Object.entries(presence)) {
    if (info?.transport === "stdio") return client;
  }
  return "";
}

// externalEndpointFor returns a display string for an external remote's
// URL — the first http endpoint found in its client_presence. Read-only
// display so the operator can identify the remote at a glance.
function externalEndpointFor(entry: ScanEntry): string {
  const presence = entry.client_presence ?? {};
  for (const info of Object.values(presence)) {
    if (info?.transport === "http" && info?.endpoint) return info.endpoint;
  }
  return "";
}

// Back-compat alias: app.tsx imports MigrationScreen and the route key is
// still `migration`. The screen's user-facing identity is "Discovery"; the
// symbol name keeps the import wiring stable.
export const MigrationScreen = DiscoveryScreen;
