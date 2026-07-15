import { useCallback, useEffect, useRef, useState } from "preact/hooks";
import {
  fetchOrThrow,
  getDeAdoptEligible,
  postAdopt,
  postAdoptPlan,
  postDeAdopt,
  postDeAdoptPlan,
  postDismiss,
  type APIError,
  type AdoptPlan,
  type DeAdoptEligible,
  type DeAdoptPlan,
  type DeAdoptReport,
} from "../api";
import { InfoTip } from "../components/InfoTip";
import { ScanRefreshControls } from "../components/ScanRefreshControls";
import { LoadingState } from "../components/LoadingState";
import { useAutoScan } from "../hooks/useAutoScan";
import { useEventSource } from "../hooks/useEventSource";
import { groupMigrationEntries, type MigrationGroups } from "../lib/migration-grouping";
import { pushToast } from "../lib/toast-store";
import type { ClientCapability, ScanEntry, ScanResult } from "../types";

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
  const [actionBusy, setActionBusy] = useState<string | null>(null);
  const [scanReloadToken, setScanReloadToken] = useState(0);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [migrateBusy, setMigrateBusy] = useState<boolean>(false);
  const [adoptPlan, setAdoptPlan] = useState<AdoptPlan | null>(null);
  const [adoptConsent, setAdoptConsent] = useState(false);
  const [adoptConfirmBusy, setAdoptConfirmBusy] = useState(false);
  const [adoptModalError, setAdoptModalError] = useState<string | null>(null);
  const [deAdoptEligibility, setDeAdoptEligibility] = useState<Record<string, DeAdoptEligible>>({});
  const [deAdoptPlan, setDeAdoptPlan] = useState<DeAdoptPlan | null>(null);
  const [deAdoptReport, setDeAdoptReport] = useState<{
    server: string;
    report: DeAdoptReport;
  } | null>(null);
  const [deAdoptAcceptedConflicts, setDeAdoptAcceptedConflicts] = useState<Set<string>>(new Set());
  const [deAdoptConfirmBusy, setDeAdoptConfirmBusy] = useState(false);
  const [deAdoptModalError, setDeAdoptModalError] = useState<string | null>(null);
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
      // G3: ask the backend eligibility owner for every discovered server.
      // Rendering never derives de-adopt ownership from scan status, endpoint
      // shape, or a loopback URL heuristic. A failed eligibility read is
      // fail-closed for that row: no affordance is published.
      const eligibilityPairs = await Promise.all(
        [...new Set((s.entries ?? []).map((entry) => entry.name))].map(async (name) => {
          try {
            return [name, await getDeAdoptEligible(name)] as const;
          } catch (err) {
            console.warn(`Discovery: de-adopt eligibility failed for ${name}:`, err);
            return null;
          }
        }),
      );
      if (!isLatest()) return;
      setScan(s);
      setDismissedUnknown(new Set(d.unknown ?? []));
      setDeAdoptEligibility(
        Object.fromEntries(eligibilityPairs.filter((pair) => pair !== null)),
      );
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

  async function runAdoptPlan(entry: ScanEntry, sourceClient?: string) {
    const client = sourceClient || firstClientFor(entry);
    if (!client) {
      setActionError(`Adopt ${entry.name}: no stdio client found`);
      return;
    }
    const busyKey = adoptActionKey(entry.name);
    setActionBusy(busyKey);
    setActionError(null);
    setAdoptModalError(null);
    try {
      const plan = await postAdoptPlan({ entry: entry.name, client });
      setAdoptPlan(plan);
      setAdoptConsent(false);
    } catch (err) {
      setActionError(`Adopt ${entry.name}: ${(err as Error).message}`);
    } finally {
      setActionBusy(null);
    }
  }

  async function confirmAdopt() {
    if (!adoptPlan) return;
    setAdoptConfirmBusy(true);
    setAdoptModalError(null);
    const req = {
      entry: adoptPlan.EntryName,
      client: adoptPlan.SourceClient,
      clients: adoptPlan.AdoptClients,
      name: adoptPlan.ManifestName,
      port: adoptPlan.Port,
    };
    try {
      await postAdopt({
        ...req,
        symlink_consent: adoptConsent ? adoptPlan.symlink_targets : [],
      });
      pushToast("success", `Adopted ${adoptPlan.ManifestName} into hub.`);
      setAdoptPlan(null);
      setAdoptConsent(false);
      setScanReloadToken((n) => n + 1);
    } catch (err) {
      const apiErr = err as APIError;
      if (apiErr.code === "SYMLINK_CONSENT_REQUIRED") {
        setAdoptModalError("Symlink consent is required before adopting this entry.");
        try {
          const refreshed = await postAdoptPlan(req);
          setAdoptPlan(refreshed);
          setAdoptConsent(false);
        } catch (refreshErr) {
          setAdoptModalError((refreshErr as Error).message);
        }
        return;
      }
      setAdoptModalError((err as Error).message);
    } finally {
      setAdoptConfirmBusy(false);
    }
  }

  async function runDeAdoptPlan(server: string) {
    setActionBusy(deAdoptActionKey(server));
    setActionError(null);
    setDeAdoptModalError(null);
    setDeAdoptReport(null);
    try {
      const plan = await postDeAdoptPlan(server);
      setDeAdoptPlan(plan);
      setDeAdoptAcceptedConflicts(new Set());
    } catch (err) {
      setActionError(`De-adopt ${server}: ${(err as Error).message}`);
    } finally {
      setActionBusy(null);
    }
  }

  async function confirmDeAdopt() {
    if (!deAdoptPlan) return;
    setDeAdoptConfirmBusy(true);
    setDeAdoptModalError(null);
    try {
      const report = await postDeAdopt(
        deAdoptPlan.ManifestName,
        [...deAdoptAcceptedConflicts],
      );
      setDeAdoptReport({ server: deAdoptPlan.ManifestName, report });
      setDeAdoptPlan(null);
      setDeAdoptAcceptedConflicts(new Set());
      pushToast(
        report.failed.length === 0 ? "success" : "danger",
        `De-adopted ${deAdoptPlan.ManifestName}: ${report.restored.length} restored, ${report.accepted.length} accepted, ${report.failed.length} failed.`,
      );
      setScanReloadToken((n) => n + 1);
    } catch (err) {
      setDeAdoptModalError((err as Error).message);
    } finally {
      setDeAdoptConfirmBusy(false);
    }
  }

  function toggleDeAdoptConflict(client: string, next: boolean) {
    setDeAdoptAcceptedConflicts((prev) => {
      const accepted = new Set(prev);
      if (next) accepted.add(client);
      else accepted.delete(client);
      return accepted;
    });
  }

  const groups: MigrationGroups = scan
    ? groupMigrationEntries(scan, dismissedUnknown)
    : { viaHub: [], viaHubInherited: [], canMigrate: [], unknown: [], external: [], perSession: [], dismissed: [] };

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
        <LoadingState label="Loading discovery results" />
      </section>
    );
  }

  const totalRows =
    groups.viaHub.length +
    groups.viaHubInherited.length +
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
            deAdoptEligibility={deAdoptEligibility}
            actionBusy={actionBusy}
            onDemigrate={runDemigrate}
            onDeAdopt={runDeAdoptPlan}
          />
          <ManagedInheritedGroup entries={groups.viaHubInherited} />
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
            clientCapabilities={scan.client_capabilities ?? {}}
            actionBusy={actionBusy}
            onAdopt={runAdoptPlan}
            onDismiss={runDismiss}
          />
          <PerSessionGroup entries={groups.perSession} />
          <DismissedGroup
            entries={groups.dismissed}
          />
        </div>
      )}
      <AdoptConfirmModal
        plan={adoptPlan}
        consent={adoptConsent}
        busy={adoptConfirmBusy}
        error={adoptModalError}
        onConsent={setAdoptConsent}
        onCancel={() => {
          if (adoptConfirmBusy) return;
          setAdoptPlan(null);
          setAdoptConsent(false);
          setAdoptModalError(null);
        }}
        onConfirm={confirmAdopt}
      />
      <DeAdoptConfirmModal
        plan={deAdoptPlan}
        report={deAdoptReport}
        acceptedConflicts={deAdoptAcceptedConflicts}
        busy={deAdoptConfirmBusy}
        error={deAdoptModalError}
        onAcceptConflict={toggleDeAdoptConflict}
        onCancel={() => {
          if (deAdoptConfirmBusy) return;
          setDeAdoptPlan(null);
          setDeAdoptReport(null);
          setDeAdoptAcceptedConflicts(new Set());
          setDeAdoptModalError(null);
        }}
        onConfirm={confirmDeAdopt}
      />
    </section>
  );
}

function ManagedByHubGroup(props: {
  entries: ScanEntry[];
  deAdoptEligibility: Record<string, DeAdoptEligible>;
  actionBusy: string | null;
  onDemigrate: (server: string) => void;
  onDeAdopt: (server: string) => void;
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
        {props.entries.map((e) => {
          const eligibility = props.deAdoptEligibility[e.name];
          const showDeAdopt = eligibility?.adopt_owned === true;
          const gateHint = eligibility?.blocked_reason;
          return (
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
            {/* Peer actions render as matching bordered buttons (Edit manifest
                is a button-styled link, Demigrate is a button) so the two
                affordances read as a unified pair, not a text link beside a
                button. */}
            <a
              class="edit-manifest btn"
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
            {showDeAdopt && (
              <span class="de-adopt-action">
                <button
                  type="button"
                  class="de-adopt btn"
                  data-action="de-adopt"
                  disabled={props.actionBusy != null || eligibility?.eligible !== true}
                  aria-describedby={gateHint ? `de-adopt-hint-${e.name}` : undefined}
                  onClick={() => props.onDeAdopt(e.name)}
                >
                  {props.actionBusy === deAdoptActionKey(e.name)
                    ? "Planning..."
                    : "De-adopt to native"}
                </button>
                {gateHint && eligibility?.eligible !== true && (
                  <span
                    id={`de-adopt-hint-${e.name}`}
                    class="de-adopt-hint"
                    data-testid={`de-adopt-hint-${e.name}`}
                  >
                    {gateHint}
                  </span>
                )}
              </span>
            )}
          </li>
          );
        })}
      </ul>
    </section>
  );
}

// ManagedInheritedGroup renders the hub-routed-but-INHERITED bucket
// (backend classify "via-hub-inherited"): servers reached through a hub
// loopback URL whose SOURCE is an import (~/.claude.json) or a
// below-write-target layer the hub never wrote (currently only MiMoCode).
// They ARE hub-routed but the hub does NOT own them, so this group is
// strictly READ-ONLY — NO Demigrate button (a demigrate would fail closed,
// since the hub cannot remove an entry it never wrote). It is rendered
// (never dropped) so the "see all MCP servers" Discovery view shows these
// import-inherited rows instead of hiding them. Rendered only when non-empty
// (the common host has none — an empty section would be noise).
function ManagedInheritedGroup(props: { entries: ScanEntry[] }) {
  if (props.entries.length === 0) {
    return null;
  }
  return (
    <section class="group group-via-hub-inherited" data-group="via-hub-inherited">
      <div class="group-heading">
        <h2>Managed by hub (inherited)</h2>
        <InfoTip
          label="About inherited hub entries"
          text="These servers are routed through the hub, but their definition lives in a layer the hub never wrote — an imported ~/.claude.json entry or a lower config layer. They are shown read-only: the hub cannot demigrate an entry it does not own. To remove the routing, edit the source config that defines the server."
        />
      </div>
      <ul class="group-rows">
        {props.entries.map((e) => (
          <li key={e.name} data-server={e.name}>
            <span class="server-name">{e.name}</span>
            <span
              class="badge badge-inherited"
              data-testid={`inherited-badge-${e.name}`}
            >
              Inherited (read-only)
            </span>
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
  clientCapabilities: Record<string, ClientCapability>;
  actionBusy: string | null;
  onAdopt: (entry: ScanEntry, sourceClient: string) => void;
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
          text="Unknown stdio entries (no mcphub manifest) can be adopted into the hub or used to create a manifest draft. External remotes are real off-host MCP servers (e.g. context7, qt-docs) routed directly by the client — they are shown read-only so you can see every MCP server, not just hub-managed ones. Dismiss parks an entry in the collapsed Dismissed section below."
        />
      </div>
      {props.unknownEntries.length > 0 && (
        <ul class="group-rows group-rows-unknown" data-subgroup="unknown">
          {props.unknownEntries.map((e) => {
            const adoptClient = firstAdoptClientFor(e, props.clientCapabilities);
            return (
              <li key={e.name} data-server={e.name}>
                <span class="server-name">{e.name}</span>
                <span class="badge badge-unknown">Unknown stdio</span>
                {adoptClient && (
                  <button
                    type="button"
                    class="adopt btn btn-primary"
                    data-action="adopt"
                    disabled={props.actionBusy != null}
                    onClick={() => props.onAdopt(e, adoptClient)}
                  >
                    {props.actionBusy === adoptActionKey(e.name) ? "Planning..." : "Adopt into hub"}
                  </button>
                )}
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
            );
          })}
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

function firstAdoptClientFor(
  entry: { client_presence?: Record<string, { transport?: string }> },
  clientCapabilities: Record<string, ClientCapability>,
): string {
  const presence = entry.client_presence ?? {};
  for (const [client, info] of Object.entries(presence)) {
    if (info?.transport === "stdio" && clientCapabilities[client]?.adopt_supported === true) {
      return client;
    }
  }
  return "";
}

function adoptActionKey(name: string): string {
  return `adopt:${name}`;
}

function deAdoptActionKey(name: string): string {
  return `de-adopt:${name}`;
}

function DeAdoptConfirmModal(props: {
  plan: DeAdoptPlan | null;
  report: { server: string; report: DeAdoptReport } | null;
  acceptedConflicts: Set<string>;
  busy: boolean;
  error: string | null;
  onAcceptConflict: (client: string, next: boolean) => void;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const open = props.plan !== null || props.report !== null;
  useEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog) return;
    if (open && !dialog.open) {
      dialog.showModal();
    } else if (!open && dialog.open) {
      dialog.close();
    }
  }, [open]);
  if (!open) return null;

  const plan = props.plan;
  const report = props.report;
  const confirmDisabled =
    props.busy ||
    plan?.Routing === "REFUSE" ||
    plan?.Manifest.HashReady !== true;
  return (
    <dialog
      ref={dialogRef}
      data-testid="deadopt-confirm-modal"
      onCancel={(ev) => {
        ev.preventDefault();
        props.onCancel();
      }}
    >
      {plan ? (
        <>
          <h2>De-adopt to native</h2>
          <div class="adopt-plan-summary de-adopt-plan-summary">
            <p>Server: {plan.ManifestName}</p>
            <p>Routing verdict: {plan.Routing}</p>
            <p>
              Manifest: {plan.Manifest.HashReady ? "ready" : plan.Manifest.Reason || "not ready"}
            </p>
            {plan.RefusalReason && <p class="error">{plan.RefusalReason}</p>}
            <section>
              <h3>Client dispositions</h3>
              <ul>
                {plan.Clients.map((client) => (
                  <li key={client.Client}>
                    <strong>{client.Client}</strong>: {client.Disposition}
                    {client.Reason && ` — ${client.Reason}`}
                    {client.AcceptEligible && (
                      <label class="de-adopt-conflict-consent">
                        <input
                          type="checkbox"
                          checked={props.acceptedConflicts.has(client.Client)}
                          onChange={(ev) =>
                            props.onAcceptConflict(
                              client.Client,
                              (ev.currentTarget as HTMLInputElement).checked,
                            )
                          }
                        />
                        <span>Accept the current native conflict for {client.Client}</span>
                      </label>
                    )}
                  </li>
                ))}
              </ul>
            </section>
          </div>
          {props.error && <p class="error">{props.error}</p>}
          <menu>
            <button type="button" onClick={props.onCancel} disabled={props.busy}>
              Cancel
            </button>
            <button
              type="button"
              class="primary"
              data-action="confirm-de-adopt"
              disabled={confirmDisabled}
              onClick={props.onConfirm}
            >
              {props.busy ? "De-adopting..." : "De-adopt to native"}
            </button>
          </menu>
        </>
      ) : report ? (
        <>
          <h2>De-adopt report</h2>
          <div class="adopt-plan-summary de-adopt-report">
            <p>Server: {report.server}</p>
            <ReportNames title="Restored" names={report.report.restored} />
            <ReportNames title="Accepted" names={report.report.accepted} />
            <section>
              <h3>Failed</h3>
              {report.report.failed.length === 0 ? (
                <p>None</p>
              ) : (
                <ul>
                  {report.report.failed.map((failure) => (
                    <li key={failure.client}>{failure.client}</li>
                  ))}
                </ul>
              )}
            </section>
          </div>
          <menu>
            <button type="button" class="primary" onClick={props.onCancel}>
              Close
            </button>
          </menu>
        </>
      ) : null}
    </dialog>
  );
}

function ReportNames(props: { title: string; names: string[] }) {
  return (
    <section>
      <h3>{props.title}</h3>
      {props.names.length === 0 ? (
        <p>None</p>
      ) : (
        <ul>
          {props.names.map((name) => (
            <li key={name}>{name}</li>
          ))}
        </ul>
      )}
    </section>
  );
}

function AdoptConfirmModal(props: {
  plan: AdoptPlan | null;
  consent: boolean;
  busy: boolean;
  error: string | null;
  onConsent: (next: boolean) => void;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const { plan } = props;
  useEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog) return;
    if (plan && !dialog.open) {
      dialog.showModal();
    } else if (!plan && dialog.open) {
      dialog.close();
    }
  }, [plan]);
  if (!plan) return null;

  const symlinkTargets = plan.symlink_targets ?? [];
  const needsSymlinkConsent = symlinkTargets.length > 0;
  return (
    <dialog
      ref={dialogRef}
      data-testid="adopt-confirm-modal"
      onCancel={(ev) => {
        ev.preventDefault();
        props.onCancel();
      }}
    >
      <h2>Adopt into hub</h2>
      <div class="adopt-plan-summary">
        <p>Manifest: {plan.ManifestName}</p>
        <p>Port: {plan.Port}</p>
        <section>
          <h3>Clients to repoint</h3>
          <ul>
            {plan.AdoptClients.map((client) => (
              <li key={client}>{client}</li>
            ))}
          </ul>
        </section>
        {(plan.AlsoPresent.length > 0 ||
          plan.SignatureMismatches.length > 0 ||
          plan.DisabledSameName.length > 0) && (
          <section>
            <h3>Excluded same-name clients</h3>
            <ul>
              {plan.AlsoPresent.map((client) => (
                <li key={`also-${client}`}>{client}: also present, not selected</li>
              ))}
              {plan.SignatureMismatches.map((mismatch) => (
                <li key={`mismatch-${mismatch.Client}`}>
                  {mismatch.Client}: {mismatch.Reason}
                </li>
              ))}
              {plan.DisabledSameName.map((disabled) => (
                <li key={`disabled-${disabled.Client}`}>{disabled.Client}: disabled</li>
              ))}
            </ul>
          </section>
        )}
        {plan.SecretRoutedKeys.length > 0 && (
          <section>
            <h3>Secret-routed keys</h3>
            <ul>
              {plan.SecretRoutedKeys.map((key) => (
                <li key={key}>{key}</li>
              ))}
            </ul>
          </section>
        )}
        {needsSymlinkConsent && (
          <section>
            <h3>Symlink write consent</h3>
            <ul>
              {symlinkTargets.map((target) => (
                <li key={`${target.client}:${target.resolved_path}`}>
                  {target.client} config is a symlink -&gt; {target.resolved_path}; adopting
                  writes through the resolved target.
                </li>
              ))}
            </ul>
            <label class="adopt-symlink-consent">
              <input
                type="checkbox"
                checked={props.consent}
                onChange={(ev) => props.onConsent((ev.currentTarget as HTMLInputElement).checked)}
              />
              <span>I understand and consent to writing through the resolved target.</span>
            </label>
          </section>
        )}
      </div>
      {props.error && <p class="error">{props.error}</p>}
      <menu>
        <button type="button" onClick={props.onCancel} disabled={props.busy}>
          Cancel
        </button>
        <button
          type="button"
          class="primary"
          data-action="confirm-adopt"
          disabled={props.busy || (needsSymlinkConsent && !props.consent)}
          onClick={props.onConfirm}
        >
          {props.busy ? "Adopting..." : "Adopt into hub"}
        </button>
      </menu>
    </dialog>
  );
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
