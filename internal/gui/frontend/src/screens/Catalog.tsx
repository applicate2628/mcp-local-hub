import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { fetchOrThrow, installMarketplaceEntry } from "../api";
import type { MarketplaceInstallResult } from "../api";
import { CORE_CLIENTS, WAVE2_CLIENTS } from "../lib/routing";
import { InfoTip } from "../components/InfoTip";
import type { DaemonStatus } from "../types";

// Mirrors catalogEntry in internal/gui/manifest.go — one row of the GET
// /api/catalog body. Each shipped server projects {name, description,
// kind}; the Catalog ("store") screen renders the description as a
// one-line summary under the server name. description/kind may be empty
// strings for a manifest that omits them (the field is additive/optional
// in the manifest schema).
interface CatalogEntry {
  name: string;
  description: string;
  kind: string;
}

// Mirrors catalogListResponse in internal/gui/manifest.go — the GET
// /api/catalog body shape ({ "catalog": [{name, description, kind}, …] }).
// These are the supported/shipped servers (embed-first union of the
// installed servers/ dir and the embedded defaults). Empty set is a
// normal first-run state the screen renders as a friendly empty card,
// not an error (the backend returns 200 {"catalog":[]} per its handler).
interface CatalogListResponse {
  catalog: CatalogEntry[];
}

// Mirrors marketplaceEntry in internal/gui/marketplace.go — one row of the
// GET /api/marketplace body. The Store one-click install (roadmap §B #1,
// slice S3) installs a curated registry entry straight from the GUI. The
// `transport` discriminator ("stdio" | "http") drives the two-tier install
// rule the UI enforces so the operator never hits a backend 400:
//
//   stdio → HUB ONLY (one shared hub daemon every client routes to —
//           the process-tail-compression path).
//   http  → BOTH "Add to hub" AND "Install directly" (direct writes the
//           remote URL straight into the chosen client configs, no daemon).
interface MarketplaceEntry {
  id: string;
  name: string;
  summary: string;
  categories: string[];
  homepage: string;
  // "stdio" | "http" — the install-mode discriminator. An older backend that
  // omits it (or a hostile/partial body) reads as "" and falls back to the
  // safe HUB-ONLY affordance.
  transport: string;
}

// Mirrors marketplaceListResponse in internal/gui/marketplace.go — the GET
// /api/marketplace body shape ({ "entries": [{id, name, summary, …}, …] }).
// A fetch/cache miss is a best-effort 200 {"entries":[]} (the backend never
// 500s the page), so an empty list is the normal degraded state.
interface MarketplaceListResponse {
  entries: MarketplaceEntry[];
}

// PerServerInstall tracks the install button lifecycle for one catalog
// row. "idle" → click → "installing" (button disabled, POST in flight) →
// "installed" on 204, or "error" with the inline message on failure.
// Keyed by server name in the parent map.
type InstallState =
  | { phase: "idle" }
  | { phase: "installing" }
  | { phase: "installed" }
  | { phase: "error"; message: string };

const IDLE: InstallState = { phase: "idle" };

// UninstallState tracks the per-row uninstall lifecycle. "uninstalling"
// disables the button while the DELETE is in flight; "error" surfaces the
// backend message inline without crashing the row. A successful uninstall
// flips the row back to an Install affordance via the status refresh, so
// there is no terminal "uninstalled" state to render — the row absent from
// the map is "idle".
type UninstallState =
  | { phase: "idle" }
  | { phase: "uninstalling" }
  | { phase: "error"; message: string };

const UNINSTALL_IDLE: UninstallState = { phase: "idle" };

// CatalogScreen is the GUI "§10 MCP Store": browse every supported/shipped
// MCP server and install/uninstall with one click, search/filter the
// shipped cards by name + description, and browse the read-only curated
// marketplace registry. It fetches /api/catalog (the enriched {name,
// description, kind} projection), /api/status (which servers are already
// running), and /api/marketplace (the curated registry) on mount; marks
// each shipped row as installed (with an Uninstall affordance) or offers
// an Install button.
export function CatalogScreen() {
  const [catalog, setCatalog] = useState<CatalogEntry[] | null>(null);
  const [marketplace, setMarketplace] = useState<MarketplaceEntry[]>([]);
  const [error, setError] = useState<string | null>(null);
  // installedServers is the set of server names that already appear in
  // /api/status. The status array carries one row per daemon, so multiple
  // rows can share a `server`; collapse to a Set of names.
  const [installedServers, setInstalledServers] = useState<Set<string>>(new Set());
  // Per-row install lifecycle. A row absent from the map is "idle".
  const [installStates, setInstallStates] = useState<Record<string, InstallState>>({});
  // Per-row uninstall lifecycle. A row absent from the map is "idle".
  const [uninstallStates, setUninstallStates] = useState<Record<string, UninstallState>>({});
  // The name awaiting uninstall confirmation (a confirm gate in front of
  // the DESTRUCTIVE DELETE). null = no confirm prompt open.
  const [confirmUninstall, setConfirmUninstall] = useState<string | null>(null);
  // Client-side search filter over shipped-server name + description. The
  // backend is not involved — purely a render-time narrowing.
  const [query, setQuery] = useState("");
  // Bump to re-run the load effect after an install/uninstall settles so
  // the /api/status refresh re-resolves the now-running/now-gone server
  // into/out of the installed set (mirrors ServersScreen's reloadToken).
  const [reloadToken, setReloadToken] = useState(0);

  // mountedRef guards post-await setState in the click handlers against the
  // "operator navigated away mid-POST/DELETE" race (mirrors ServersScreen).
  const mountedRef = useRef(true);
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        // /api/status decodes as an array (never null — fetchOrThrow's
        // "array" guard rejects a null/object body). A server is
        // "installed" if any daemon row reports its name.
        const [list, status] = await Promise.all([
          fetchOrThrow<CatalogListResponse>("/api/catalog", "object"),
          fetchOrThrow<DaemonStatus[]>("/api/status", "array"),
        ]);
        if (cancelled) return;
        const entries = Array.isArray(list.catalog) ? list.catalog : [];
        setCatalog(entries);
        const installed = new Set<string>();
        for (const row of status) {
          if (row.server) installed.add(row.server);
        }
        setInstalledServers(installed);
        setError(null);
        // Marketplace load is intentionally separate + non-fatal: a
        // failure here must not blank the shipped-server store (which is
        // the primary surface). The backend is best-effort (never 500s),
        // but we still guard so a transport error degrades gracefully.
        try {
          const mp = await fetchOrThrow<MarketplaceListResponse>("/api/marketplace", "object");
          if (!cancelled) {
            // Normalize `transport` to a string so an older backend that omits
            // it (or a partial body) reads as "" and falls back to the safe
            // HUB-ONLY affordance rather than crashing the row.
            const rows = (Array.isArray(mp.entries) ? mp.entries : []).map((e) => ({
              ...e,
              transport: typeof e.transport === "string" ? e.transport : "",
            }));
            setMarketplace(rows);
          }
        } catch {
          if (!cancelled) setMarketplace([]);
        }
      } catch (err) {
        if (!cancelled) setError((err as Error).message);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [reloadToken]);

  async function installServer(name: string) {
    // Guard against double-fire while a POST is in flight for this row.
    if (installStates[name]?.phase === "installing") return;
    setInstallStates((prev) => ({ ...prev, [name]: { phase: "installing" } }));
    try {
      // POST /api/install?name=<server> → 204 on success, {error, code}
      // envelope on failure. The query path mirrors the backend handler's
      // documented frontend contract (shell-greppable install triggers).
      const resp = await fetch(`/api/install?name=${encodeURIComponent(name)}`, {
        method: "POST",
      });
      if (resp.status === 204) {
        if (!mountedRef.current) return;
        setInstallStates((prev) => ({ ...prev, [name]: { phase: "installed" } }));
        // Re-fetch /api/status so the server flips into the installed set
        // authoritatively (and survives a navigate-away + return).
        setReloadToken((n) => n + 1);
        return;
      }
      // Failure: surface the backend's {error} envelope inline without
      // crashing the row. Non-JSON bodies fall through to statusText.
      let body: { error?: string } | null = null;
      try {
        body = (await resp.json()) as { error?: string };
      } catch {
        // Non-JSON error body; fall through.
      }
      if (!mountedRef.current) return;
      const message = body?.error ?? resp.statusText ?? "install failed";
      setInstallStates((prev) => ({ ...prev, [name]: { phase: "error", message } }));
    } catch (err) {
      if (!mountedRef.current) return;
      setInstallStates((prev) => ({
        ...prev,
        [name]: { phase: "error", message: (err as Error).message },
      }));
    }
  }

  async function uninstallServer(name: string) {
    // Guard against double-fire while a DELETE is in flight for this row.
    if (uninstallStates[name]?.phase === "uninstalling") return;
    setConfirmUninstall(null);
    setUninstallStates((prev) => ({ ...prev, [name]: { phase: "uninstalling" } }));
    try {
      // DELETE /api/install/<server> → 200/207 with {uninstall_results}
      // on a clean/partial teardown, {error, code} envelope on failure.
      // Both 200 and 207 are success (207 = partial per-target warnings);
      // any 2xx means "uninstall succeeded structurally", so refresh status
      // so the row flips back to an Install affordance.
      const resp = await fetch(`/api/install/${encodeURIComponent(name)}`, {
        method: "DELETE",
      });
      if (resp.ok) {
        if (!mountedRef.current) return;
        // Clear any prior install-state so the refreshed row renders the
        // Install button (not a stale "installed" badge from this session).
        setInstallStates((prev) => {
          const next = { ...prev };
          delete next[name];
          return next;
        });
        setUninstallStates((prev) => {
          const next = { ...prev };
          delete next[name];
          return next;
        });
        setReloadToken((n) => n + 1);
        return;
      }
      let body: { error?: string } | null = null;
      try {
        body = (await resp.json()) as { error?: string };
      } catch {
        // Non-JSON error body; fall through.
      }
      if (!mountedRef.current) return;
      const message = body?.error ?? resp.statusText ?? "uninstall failed";
      setUninstallStates((prev) => ({ ...prev, [name]: { phase: "error", message } }));
    } catch (err) {
      if (!mountedRef.current) return;
      setUninstallStates((prev) => ({
        ...prev,
        [name]: { phase: "error", message: (err as Error).message },
      }));
    }
  }

  // Client-side filter: match the trimmed, case-folded query against the
  // server name + description. An empty query shows everything.
  const filteredCatalog = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q || !catalog) return catalog ?? [];
    return catalog.filter(
      (e) =>
        e.name.toLowerCase().includes(q) ||
        (e.description ?? "").toLowerCase().includes(q),
    );
  }, [catalog, query]);

  if (error) {
    return (
      <section class="screen catalog" data-testid="catalog-error">
        <h1>Catalog</h1>
        <p class="error">Failed to load: {error}</p>
      </section>
    );
  }

  if (!catalog) {
    return (
      <section class="screen catalog">
        <h1>Catalog</h1>
        <p>Loading…</p>
      </section>
    );
  }

  return (
    <section class="screen catalog">
      <h1>Catalog</h1>
      <p class="catalog-intro">
        Browse the supported MCP servers and install any with one click.
      </p>

      <div class="catalog-search">
        <input
          type="search"
          data-testid="catalog-search"
          placeholder="Filter servers by name or description…"
          value={query}
          onInput={(e) => setQuery((e.currentTarget as HTMLInputElement).value)}
          aria-label="Filter servers"
        />
      </div>

      {catalog.length === 0 ? (
        <p class="empty-state" data-testid="catalog-empty">
          No supported servers found.
        </p>
      ) : filteredCatalog.length === 0 ? (
        <p class="empty-state" data-testid="catalog-no-matches">
          No servers match “{query}”.
        </p>
      ) : (
        <div class="cards" data-testid="catalog-cards">
          {filteredCatalog.map((entry) => {
            const name = entry.name;
            const state = installStates[name] ?? IDLE;
            const unstate = uninstallStates[name] ?? UNINSTALL_IDLE;
            // A row is "installed" if /api/status already reports it OR the
            // most recent install POST for this row returned 204.
            const installed = installedServers.has(name) || state.phase === "installed";
            return (
              <div class="card catalog-card" key={name} data-testid={`catalog-card-${name}`}>
                <div class="card-title">{name}</div>
                {entry.description && (
                  <p class="catalog-card-desc" data-testid={`catalog-desc-${name}`}>
                    {entry.description}
                  </p>
                )}
                <div class="catalog-card-actions">
                  {installed ? (
                    <>
                      <span
                        class="lsp-chip lsp-chip-via-hub"
                        data-testid={`catalog-installed-${name}`}
                      >
                        installed
                      </span>
                      {confirmUninstall === name ? (
                        <>
                          <button
                            type="button"
                            class="danger"
                            data-testid={`catalog-uninstall-confirm-${name}`}
                            disabled={unstate.phase === "uninstalling"}
                            onClick={() => uninstallServer(name)}
                          >
                            {unstate.phase === "uninstalling"
                              ? "Uninstalling…"
                              : "Confirm uninstall"}
                          </button>
                          <button
                            type="button"
                            data-testid={`catalog-uninstall-cancel-${name}`}
                            disabled={unstate.phase === "uninstalling"}
                            onClick={() => setConfirmUninstall(null)}
                          >
                            Cancel
                          </button>
                        </>
                      ) : (
                        <button
                          type="button"
                          class="danger"
                          data-testid={`catalog-uninstall-${name}`}
                          disabled={unstate.phase === "uninstalling"}
                          onClick={() => setConfirmUninstall(name)}
                        >
                          Uninstall
                        </button>
                      )}
                    </>
                  ) : (
                    <button
                      type="button"
                      data-testid={`catalog-install-${name}`}
                      disabled={state.phase === "installing"}
                      onClick={() => installServer(name)}
                    >
                      {state.phase === "installing" ? "Installing…" : "Install"}
                    </button>
                  )}
                </div>
                {state.phase === "error" && (
                  <p class="error" data-testid={`catalog-error-${name}`}>
                    {state.message}
                  </p>
                )}
                {unstate.phase === "error" && (
                  <p class="error" data-testid={`catalog-uninstall-error-${name}`}>
                    {unstate.message}
                  </p>
                )}
              </div>
            );
          })}
        </div>
      )}

      <MarketplaceSection entries={marketplace} />
    </section>
  );
}

// The supported direct-mode client list, mirroring the Servers matrix client
// set (CORE_CLIENTS first, then the wave-2 opt-in adapters). Direct mode
// writes the remote URL straight into each selected client config, so the
// multiselect offers the same superset the rest of the GUI knows about.
const DIRECT_CLIENTS: readonly string[] = [...CORE_CLIENTS, ...WAVE2_CLIENTS];

// MarketplaceInstallState tracks the per-entry install lifecycle for one
// marketplace row, mirroring the shipped-server PerServerInstall pattern
// (idle → installing → installed → error), keyed per entry id in the parent
// map. The richer terminal states carry the data the row renders inline:
//   "installed"      → hub-mode 201 success (name + resolved port).
//   "name-conflict"  → hub-mode 409: offer a one-click retry under suggestedName.
//   "direct-result"  → direct-mode 200/207: per-client updated / failed split.
//   "error"          → any unmodelled failure, rendered as an inline message.
type MarketplaceInstallState =
  | { phase: "idle" }
  | { phase: "installing" }
  | { phase: "installed"; name: string; port: number }
  | { phase: "name-conflict"; suggestedName: string }
  | {
      phase: "direct-result";
      partial: boolean;
      clientsUpdated: string[];
      clientsFailed: Array<{ client: string; error: string }>;
    }
  | { phase: "error"; message: string };

const MARKETPLACE_IDLE: MarketplaceInstallState = { phase: "idle" };

// MarketplaceSection renders the curated marketplace registry as a one-click
// Store. Each entry installs straight from the GUI per the two-tier rule:
// stdio entries get a single "Add to hub" action; http entries additionally
// offer "Install directly" (a remote-URL write into a chosen client set). An
// empty list (fetch/cache miss or genuinely empty registry) renders a muted
// notice rather than nothing, so operators know the section exists.
function MarketplaceSection({ entries }: { entries: MarketplaceEntry[] }) {
  // Per-row install lifecycle. A row absent from the map is "idle".
  const [states, setStates] = useState<Record<string, MarketplaceInstallState>>({});
  // mountedRef guards post-await setState against the "operator navigated away
  // mid-POST" race (mirrors the shipped-server install handlers above).
  const mountedRef = useRef(true);
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  function setState(id: string, next: MarketplaceInstallState) {
    setStates((prev) => ({ ...prev, [id]: next }));
  }

  // runInstall is the shared POST driver for both hub + direct modes. `name`
  // carries the suggested-name retry for the hub 409 path; `clients` is the
  // direct-mode target list. It maps the discriminated api result onto the
  // per-row terminal state.
  async function runInstall(
    id: string,
    mode: "hub" | "direct",
    opts: { name?: string; clients?: string[] } = {},
  ) {
    // Guard against double-fire while a POST is in flight for this row.
    if (states[id]?.phase === "installing") return;
    setState(id, { phase: "installing" });
    try {
      const result: MarketplaceInstallResult = await installMarketplaceEntry({
        id,
        mode,
        name: opts.name,
        clients: opts.clients,
      });
      if (!mountedRef.current) return;
      if (result.kind === "hub-installed") {
        setState(id, { phase: "installed", name: result.name, port: result.port });
      } else if (result.kind === "name-conflict") {
        setState(id, { phase: "name-conflict", suggestedName: result.suggestedName });
      } else {
        setState(id, {
          phase: "direct-result",
          partial: result.partial,
          clientsUpdated: result.clientsUpdated,
          clientsFailed: result.clientsFailed,
        });
      }
    } catch (err) {
      if (!mountedRef.current) return;
      setState(id, { phase: "error", message: (err as Error).message });
    }
  }

  return (
    <div class="catalog-marketplace" data-testid="catalog-marketplace">
      <h2>Marketplace</h2>
      <p class="catalog-intro">
        Discover and install MCP servers from the curated registry with one
        click.
      </p>
      {entries.length === 0 ? (
        <p class="empty-state" data-testid="catalog-marketplace-empty">
          No marketplace entries available right now.
        </p>
      ) : (
        <div class="cards" data-testid="catalog-marketplace-cards">
          {entries.map((entry) => (
            <MarketplaceCard
              key={entry.id}
              entry={entry}
              state={states[entry.id] ?? MARKETPLACE_IDLE}
              onInstall={runInstall}
            />
          ))}
        </div>
      )}
    </div>
  );
}

// MarketplaceCard renders one marketplace entry: name, summary, categories,
// the install affordance (per the two-tier transport rule), and the inline
// install result/error. It is a leaf component so the direct-mode client
// multiselect state is local to the row.
function MarketplaceCard({
  entry,
  state,
  onInstall,
}: {
  entry: MarketplaceEntry;
  state: MarketplaceInstallState;
  onInstall: (
    id: string,
    mode: "hub" | "direct",
    opts?: { name?: string; clients?: string[] },
  ) => void;
}) {
  const isHttp = entry.transport === "http";
  const installing = state.phase === "installing";
  // Direct-mode client multiselect open + selected set, local to this row.
  const [directOpen, setDirectOpen] = useState(false);
  const [selected, setSelected] = useState<Set<string>>(new Set());

  function toggleClient(client: string) {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(client)) next.delete(client);
      else next.add(client);
      return next;
    });
  }

  return (
    <div
      class="card catalog-card catalog-marketplace-card"
      data-testid={`catalog-marketplace-card-${entry.id}`}
    >
      <div class="card-title">{entry.name}</div>
      {entry.summary && (
        <p
          class="catalog-card-desc"
          data-testid={`catalog-marketplace-summary-${entry.id}`}
        >
          {entry.summary}
        </p>
      )}
      {entry.categories.length > 0 && (
        <p class="catalog-marketplace-categories">
          {entry.categories.map((c) => (
            <span class="lsp-chip" key={c}>
              {c}
            </span>
          ))}
        </p>
      )}

      {/* Install affordance. stdio → hub only; http → hub + direct. */}
      {state.phase === "installed" ? (
        <p
          class="catalog-marketplace-status catalog-marketplace-status-ok"
          role="status"
          data-testid={`catalog-marketplace-installed-${entry.id}`}
        >
          Added to hub as <strong>{state.name}</strong>
          {state.port > 0 ? ` on port ${state.port}.` : "."}
        </p>
      ) : (
        <div class="catalog-marketplace-install" data-testid={`catalog-marketplace-install-${entry.id}`}>
          <div class="catalog-marketplace-actions">
            <button
              type="button"
              class="btn-primary"
              data-testid={`catalog-marketplace-hub-${entry.id}`}
              disabled={installing}
              onClick={() => onInstall(entry.id, "hub")}
            >
              {installing ? "Installing…" : "Add to hub"}
            </button>
            <InfoTip
              label="What is Add to hub?"
              text="Runs the server once as a shared hub daemon that every client routes to — instead of each client spawning its own copy."
            />
            {isHttp && (
              <button
                type="button"
                data-testid={`catalog-marketplace-direct-toggle-${entry.id}`}
                aria-expanded={directOpen}
                aria-controls={directOpen ? `catalog-marketplace-direct-panel-${entry.id}` : undefined}
                disabled={installing}
                onClick={() => setDirectOpen((o) => !o)}
              >
                Install directly
              </button>
            )}
            {isHttp && (
              <InfoTip
                label="What is Install directly?"
                text="Writes the remote URL straight into the client configs you pick — no hub daemon. Available for remote (http) servers only."
              />
            )}
          </div>

          {/* Direct-mode client multiselect — http entries only, revealed on
              the "Install directly" toggle. */}
          {isHttp && directOpen && (
            <div
              class="catalog-marketplace-direct-panel"
              id={`catalog-marketplace-direct-panel-${entry.id}`}
              data-testid={`catalog-marketplace-direct-panel-${entry.id}`}
            >
              <p class="catalog-marketplace-direct-label" id={`catalog-marketplace-direct-legend-${entry.id}`}>
                Pick the clients to write this server into:
              </p>
              <div
                class="catalog-marketplace-clients"
                role="group"
                aria-labelledby={`catalog-marketplace-direct-legend-${entry.id}`}
              >
                {DIRECT_CLIENTS.map((client) => (
                  <label class="catalog-marketplace-client" key={client}>
                    <input
                      type="checkbox"
                      data-testid={`catalog-marketplace-client-${entry.id}-${client}`}
                      checked={selected.has(client)}
                      disabled={installing}
                      onChange={() => toggleClient(client)}
                    />
                    {client}
                  </label>
                ))}
              </div>
              <button
                type="button"
                class="btn-primary"
                data-testid={`catalog-marketplace-direct-install-${entry.id}`}
                disabled={installing || selected.size === 0}
                onClick={() => onInstall(entry.id, "direct", { clients: [...selected] })}
              >
                {installing ? "Installing…" : "Install into selected clients"}
              </button>
            </div>
          )}

          {/* 409 NAME_CONFLICT: offer a one-click retry under the suggested
              name. */}
          {state.phase === "name-conflict" && (
            <div
              class="catalog-marketplace-status catalog-marketplace-status-warn"
              role="alert"
              data-testid={`catalog-marketplace-conflict-${entry.id}`}
            >
              A server named <strong>{entry.id}</strong> already exists.{" "}
              <button
                type="button"
                data-testid={`catalog-marketplace-conflict-retry-${entry.id}`}
                onClick={() => onInstall(entry.id, "hub", { name: state.suggestedName })}
              >
                Install as {state.suggestedName}
              </button>
            </div>
          )}

          {/* Direct-mode result: per-client updated / failed split. 207 =
              partial. */}
          {state.phase === "direct-result" && (
            <div
              class={`catalog-marketplace-status ${state.partial ? "catalog-marketplace-status-warn" : "catalog-marketplace-status-ok"}`}
              role="status"
              data-testid={`catalog-marketplace-direct-result-${entry.id}`}
            >
              {state.clientsUpdated.length > 0 && (
                <p data-testid={`catalog-marketplace-direct-updated-${entry.id}`}>
                  Installed into: {state.clientsUpdated.join(", ")}.
                </p>
              )}
              {state.clientsFailed.length > 0 && (
                <ul class="catalog-marketplace-direct-failed" data-testid={`catalog-marketplace-direct-failed-${entry.id}`}>
                  {state.clientsFailed.map((f) => (
                    <li key={f.client}>
                      <strong>{f.client}</strong>: {f.error}
                    </li>
                  ))}
                </ul>
              )}
            </div>
          )}

          {state.phase === "error" && (
            <p class="error" data-testid={`catalog-marketplace-error-${entry.id}`}>
              {state.message}
            </p>
          )}
        </div>
      )}

      {/* homepage comes from an UNTRUSTED external registry — only render the
          link when it is an http(s) URL, so a hostile catalog cannot inject a
          javascript:/data: href. */}
      {/^https?:\/\//i.test(entry.homepage) && (
        <p class="catalog-marketplace-homepage">
          <a
            href={entry.homepage}
            target="_blank"
            rel="noopener noreferrer"
            data-testid={`catalog-marketplace-homepage-${entry.id}`}
          >
            Homepage
          </a>
        </p>
      )}
    </div>
  );
}
