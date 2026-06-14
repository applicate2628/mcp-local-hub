import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { fetchOrThrow } from "../api";
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
// GET /api/marketplace body. The marketplace browse is READ-ONLY: the
// Catalog screen lists the curated registry entries with their summary +
// a "Generate" hint pointing at the CLI flow (`mcphub marketplace generate
// <id>`). It deliberately carries no install/transport fields because
// stdio-wrapper generation is a CLI flow, not a GUI install.
interface MarketplaceEntry {
  id: string;
  name: string;
  summary: string;
  categories: string[];
  homepage: string;
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
            setMarketplace(Array.isArray(mp.entries) ? mp.entries : []);
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

// MarketplaceSection renders the read-only curated marketplace registry.
// Each entry shows its name, summary, categories, and a "Generate" hint
// linking to the CLI flow — there is NO in-GUI install because
// stdio-wrapper generation is a `mcphub marketplace generate <id>` flow.
// An empty list (fetch/cache miss or genuinely empty registry) renders a
// muted notice rather than nothing, so operators know the section exists.
function MarketplaceSection({ entries }: { entries: MarketplaceEntry[] }) {
  return (
    <div class="catalog-marketplace" data-testid="catalog-marketplace">
      <h2>Marketplace</h2>
      <p class="catalog-intro">
        Discover MCP servers from the curated registry. Generate a draft
        manifest from the CLI, then review and install it.
      </p>
      {entries.length === 0 ? (
        <p class="empty-state" data-testid="catalog-marketplace-empty">
          No marketplace entries available right now.
        </p>
      ) : (
        <div class="cards" data-testid="catalog-marketplace-cards">
          {entries.map((entry) => (
            <div
              class="card catalog-card catalog-marketplace-card"
              key={entry.id}
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
              <p
                class="catalog-marketplace-generate"
                data-testid={`catalog-marketplace-generate-${entry.id}`}
              >
                Generate a draft with{" "}
                <code>mcphub marketplace generate {entry.id}</code>
              </p>
              {/* homepage comes from an UNTRUSTED external registry — only
                  render the link when it is an http(s) URL, so a hostile
                  catalog cannot inject a javascript:/data: href. */}
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
          ))}
        </div>
      )}
    </div>
  );
}
