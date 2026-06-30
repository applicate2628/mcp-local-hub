import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import {
  fetchOrThrow,
  getServerReadiness,
  installMarketplaceEntry,
  refreshMarketplace,
} from "../api";
import type { MarketplaceInstallResult, ReadinessReport } from "../api";
import { directInstallableClients } from "../lib/routing";
import { groupByTheme } from "../lib/catalog-themes";
import { InfoTip } from "../components/InfoTip";
import { ReadinessPanel, readinessBlockerCount } from "../components/ReadinessPanel";
import { AddSecretModal } from "../components/AddSecretModal";
import { LoadingState } from "../components/LoadingState";
import type { ClientCapability, DaemonStatus } from "../types";

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
  // omits it (or a hostile/partial body) reads as "" and falls back to the safe
  // HUB-ONLY affordance. NOTE: docs-only POINTER rows are NOT entries — they
  // arrive in the SEPARATE docs_only[] array (S4, bot #446 P1), so an installable
  // entry never carries the docs-only discriminator.
  transport: string;
  // Availability (D-3, Tier-0): "" | "ready" | "watch" | "disabled-until-probe".
  // A "watch" / "disabled-until-probe" row is greyed and labeled "probe to
  // enable" — its host app/tool isn't detected yet. An older backend that omits
  // it (or "" / "ready") renders exactly as before. Optional + string-normalized.
  availability?: string;
  // ProbeState (D-3, Tier-0 — mirror-gate): the TRI-STATE browse-time host-probe
  // verdict — "ready" (installable now), "inert-blocked" (host app provably not
  // detected yet → greyed "probe to enable"), or "inert-unknown" (carries a
  // files[]/path-shaped probe the browse path defers — still offer install; the
  // real probe runs at install). This is the new authority. An older backend that
  // omits it reads as undefined → we fall back to the deprecated probe_passes
  // alias (ready iff true, else inert-blocked — fail-closed).
  probe_state?: string;
  // ProbePasses (DEPRECATED alias of probe_state, kept for one release): the bool
  // host-probe verdict (true iff probe_state == "ready"). New code reads
  // probe_state; this remains only so an un-regenerated bundle / older backend
  // degrades safely (undefined → fail-closed grey-on-availability).
  probe_passes?: boolean;
}

// ProbeBrowseState mirrors the backend api.ProbeBrowseState tri-state. Kept as a
// string union so an unknown future value coming off the wire still type-checks
// (we fall back to the fail-closed inert-blocked branch for anything unexpected).
type ProbeBrowseState = "ready" | "inert-blocked" | "inert-unknown";

// legacyProbeState maps the DEPRECATED probe_passes bool (when probe_state is
// absent on an older backend) onto the tri-state, fail-closed: probe_passes ===
// true → "ready"; anything else (false / undefined) → "inert-blocked". The
// legacy bool never carried the "unknown" state, so a now-inert row with a
// file/path probe stays greyed under the legacy path — the prior, stricter
// behavior. SINGLE place the deprecated alias is interpreted.
function legacyProbeState(entry: MarketplaceEntry): ProbeBrowseState {
  if (entry.availability !== "watch" && entry.availability !== "disabled-until-probe") {
    return "ready";
  }
  return entry.probe_passes === true ? "ready" : "inert-blocked";
}

// resolveProbeState picks the authoritative tri-state for one row: the backend
// probe_state when present (and a recognized value), else the legacy bool map.
function resolveProbeState(entry: MarketplaceEntry): ProbeBrowseState {
  switch (entry.probe_state) {
    case "ready":
    case "inert-blocked":
    case "inert-unknown":
      return entry.probe_state;
    default:
      return legacyProbeState(entry);
  }
}

// MarketplaceDocsOnlyEntry mirrors marketplaceDocsOnlyEntry in
// internal/gui/marketplace.go — ONE manual-install POINTER row from the catalog's
// SEPARATE top-level docs_only[] array (S4, bot #446 P1). It carries the pointer
// payload (id/name/summary/categories/homepage + the raw README link + the verbatim
// manual_install steps) and NO transport/probe/install fields — the Catalog renders
// a DOCS-ONLY badge + readme link + a "view setup" block, never an install
// affordance.
interface MarketplaceDocsOnlyEntry {
  id: string;
  name: string;
  summary: string;
  categories: string[];
  homepage: string;
  readme_url?: string;
  manual_install?: string;
}

// Mirrors marketplaceListResponse in internal/gui/marketplace.go — the GET
// /api/marketplace body shape ({ "entries": [...], "docs_only": [...] }).
// A fetch/cache miss is a best-effort 200 {"entries":[],"docs_only":[]} (the
// backend never 500s the page), so empty lists are the normal degraded state.
interface MarketplaceListResponse {
  entries: MarketplaceEntry[];
  docs_only: MarketplaceDocsOnlyEntry[];
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
  // S4: the docs-only POINTER rows arrive in a SEPARATE docs_only[] array, kept in
  // its own state slice (not folded into `marketplace`) so the install entries and
  // the manual-install pointers stay distinct shapes end-to-end.
  const [docsOnly, setDocsOnly] = useState<MarketplaceDocsOnlyEntry[]>([]);
  const [error, setError] = useState<string | null>(null);
  // installedServers is the set of server names that already appear in
  // /api/status. The status array carries one row per daemon, so multiple
  // rows can share a `server`; collapse to a Set of names.
  const [installedServers, setInstalledServers] = useState<Set<string>>(new Set());
  // Backend-derived per-client capability map (GET /api/client-capabilities).
  // The direct-install multiselect derives its URL-native client choices from
  // this single owner (the `direct_installable` flag = !IsRelayStdio) so it
  // can't drift behind the backend adapter registry. null until the (non-fatal)
  // fetch resolves; a failure leaves it null and the direct-install panel simply
  // offers no clients (honest — better than a hard-coded mirror that would offer
  // relay-stdio clients that deterministically fail a direct install).
  const [clientCapabilities, setClientCapabilities] =
    useState<Record<string, ClientCapability> | null>(null);
  // Per-row install lifecycle. A row absent from the map is "idle".
  const [installStates, setInstallStates] = useState<Record<string, InstallState>>({});
  // Per-row uninstall lifecycle. A row absent from the map is "idle".
  const [uninstallStates, setUninstallStates] = useState<Record<string, UninstallState>>({});
  // The name awaiting uninstall confirmation (a confirm gate in front of
  // the DESTRUCTIVE DELETE). null = no confirm prompt open.
  const [confirmUninstall, setConfirmUninstall] = useState<string | null>(null);
  // The shipped-server row whose pre-install readiness gate is open (epic
  // install-and-it-works, area 2). null = no gate open. Clicking Install opens
  // the gate (GET /api/server/readiness) so blockers/optional-secret prompts
  // surface BEFORE the POST /api/install, instead of failing later as a cryptic
  // HTTP-502 at the client. The actual POST runs from the gate's Confirm button.
  const [readinessGate, setReadinessGate] = useState<string | null>(null);
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
              // Normalize availability to a string so an older backend that
              // omits it reads as "" (ready) and the row renders as before.
              availability: typeof e.availability === "string" ? e.availability : "",
            }));
            setMarketplace(rows);
            // S4: docs_only is a SEPARATE array. An older backend (pre-S4) omits
            // it entirely → reads as [] → no pointer rows render (graceful).
            setDocsOnly(Array.isArray(mp.docs_only) ? mp.docs_only : []);
          }
        } catch {
          if (!cancelled) {
            setMarketplace([]);
            setDocsOnly([]);
          }
        }
        // Client-capabilities load is also separate + non-fatal: the
        // direct-install multiselect needs the backend's URL-native client
        // set, but a failure here must not blank the store. On failure the
        // map stays null and the direct panel offers no clients.
        try {
          const caps = await fetchOrThrow<Record<string, ClientCapability>>(
            "/api/client-capabilities",
            "object",
          );
          if (!cancelled) setClientCapabilities(caps);
        } catch {
          if (!cancelled) setClientCapabilities(null);
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
        // Close the pre-install readiness gate (if it was open for this row).
        setReadinessGate((g) => (g === name ? null : g));
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

  // The URL-native client set the direct-install multiselect may offer,
  // derived from the backend capability map's `direct_installable` flag
  // (!IsRelayStdio — the real "AddEntry accepts a URL-only entry" predicate)
  // — never a hard-coded mirror, and BROADER than the narrow remote-http
  // header matrix so URL-native non-core clients (hermes/openclaw/opencode)
  // are offered too. Empty until /api/client-capabilities resolves.
  const directClients = useMemo(
    () => directInstallableClients(clientCapabilities),
    [clientCapabilities],
  );

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
        <LoadingState label="Loading catalog" />
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
            // Workspace-scoped servers (mcp-language-server) cannot be installed
            // via POST /api/install — the backend rejects them
            // (refuseWorkspaceScopedInstall, install.go) because the
            // (workspace, language) tuples can't be inferred from the manifest
            // alone. A plain Install button on these rows always fails. Instead
            // route the operator to the Servers LSP section where the
            // RegisterWorkspacePanel registers a workspace path + language. We
            // suppress the install affordance entirely for these rows (no POST).
            const workspaceScoped = entry.kind === "workspace-scoped";
            return (
              <div class="card catalog-card" key={name} data-testid={`catalog-card-${name}`}>
                <div class="card-title">
                  <span>{name}</span>
                  {entry.description && (
                    <InfoTip
                      label={`About ${name}`}
                      text={entry.description}
                      // The description text now lives in the InfoTip popover.
                      // The data-testid is preserved on the trigger so coverage
                      // can still assert the prose by reading the text prop's
                      // rendered popover (open-on-click) without a layout shift.
                      data-testid={`catalog-desc-${name}`}
                    />
                  )}
                </div>
                <div class="catalog-card-actions">
                  {workspaceScoped ? (
                    // Workspace-scoped server: no POST /api/install (it would be
                    // rejected). Route to the Servers LSP section to register a
                    // workspace path + language instead.
                    <a
                      class="btn btn-primary"
                      href="#/servers"
                      data-testid={`catalog-setup-lsp-${name}`}
                      title="Register a workspace folder and enable a language server in the Servers screen"
                    >
                      Set up LSP / register workspace
                    </a>
                  ) : installed ? (
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
                            class="btn btn-danger"
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
                            class="btn btn-secondary"
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
                          class="btn btn-danger"
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
                      class="btn btn-secondary"
                      data-testid={`catalog-install-${name}`}
                      disabled={state.phase === "installing"}
                      // Open the pre-install readiness gate instead of POSTing
                      // immediately (epic area 2). The gate's Confirm button runs
                      // the actual installServer() POST once blockers are clear.
                      onClick={() => setReadinessGate(name)}
                    >
                      {state.phase === "installing" ? "Installing…" : "Install"}
                    </button>
                  )}
                </div>
                {!workspaceScoped && !installed && readinessGate === name && (
                  <CatalogInstallGate
                    name={name}
                    installing={state.phase === "installing"}
                    onConfirmInstall={() => installServer(name)}
                    onCancel={() => setReadinessGate(null)}
                  />
                )}
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

      <MarketplaceSection
        entries={marketplace}
        docsOnly={docsOnly}
        installedServers={installedServers}
        directClients={directClients}
        onRefreshed={(rows, docs) => {
          setMarketplace(rows);
          setDocsOnly(docs);
        }}
      />
    </section>
  );
}

// CatalogInstallGate is the pre-install readiness panel for ONE shipped catalog
// row (epic install-and-it-works, area 2 — env-secrets-onboarding). On mount it
// GETs /api/server/readiness?server=<name> and renders the shared ReadinessPanel
// so the operator sees exactly what blocks (or merely advises) the install
// BEFORE the POST, instead of a later cryptic HTTP-502 at the client:
//
//   • Blockers (non-optional unmet) → shown with their guided Fix; the Confirm
//     Install button stays DISABLED until they are resolved (honest UX — these
//     would fail the install).
//   • Optional unmet secrets → each offers "Set <key>" (opens AddSecretModal,
//     the SINGLE owner of POST /api/secrets, pre-filled with the key) and an
//     "Open Secrets" deep-link (#/secrets?key=<key>). The operator may set it
//     and install, OR skip and install without it (non-blocking — the server
//     runs without the env var and reports its own missing-key if it needs it).
//
// It consumes the readiness endpoint READ-ONLY and never mutates the install
// path; the actual POST runs in the parent's installServer() via onConfirmInstall.
function CatalogInstallGate({
  name,
  installing,
  onConfirmInstall,
  onCancel,
}: {
  name: string;
  // True while the parent's POST /api/install is in flight for this row, so the
  // gate disables its own buttons (mirrors the parent button's "Installing…").
  installing: boolean;
  onConfirmInstall: () => void;
  onCancel: () => void;
}) {
  const [report, setReport] = useState<ReadinessReport | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  // Bump to re-fetch readiness after an inline secret is set (so the secret row
  // flips from advisory → satisfied and any vault blocker clears).
  const [reloadToken, setReloadToken] = useState(0);
  // The vault key whose AddSecretModal is open (pre-filled), or null. Reusing
  // AddSecretModal keeps "set a secret" a single owner — no new POST handler.
  const [secretModalKey, setSecretModalKey] = useState<string | null>(null);
  // mountedRef guards post-await setState against the navigate-away race.
  const mountedRef = useRef(true);
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    getServerReadiness(name)
      .then((rep) => {
        if (cancelled) return;
        setReport(rep);
        setError(null);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        // A readiness fetch failure must not strand the operator: surface the
        // error in the panel but still allow install (the gate is advisory; the
        // backend install preflight remains the authoritative gate).
        setReport(null);
        setError((err as Error).message);
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [name, reloadToken]);

  // Blocker count drives the Confirm-Install disable on the SAME predicate the
  // panel renders (readinessBlockerCount is the single owner). While the report
  // is still loading we have no verdict yet, so Confirm is disabled then too.
  const blockers = readinessBlockerCount(report);
  // A fetch error means we could not verify readiness — do NOT block install on
  // it (the backend preflight is authoritative); only real blockers disable.
  const confirmDisabled = installing || loading || blockers > 0;

  return (
    <div class="catalog-install-gate" data-testid={`catalog-readiness-gate-${name}`}>
      <ReadinessPanel
        report={report}
        loading={loading}
        error={error}
        // Catalog does not use the inline-write-via-parent-save model; the
        // secret-action render below composes AddSecretModal + the deep-link
        // instead. inlineSecrets/onInlineSecretChange are inert here.
        inlineSecrets={{}}
        onInlineSecretChange={() => {}}
        inputsDisabled={installing}
        renderSecretAction={(key) => (
          <div class="readiness-secret-actions" data-testid={`catalog-secret-actions-${key}`}>
            <button
              type="button"
              class="btn btn-secondary"
              data-testid={`catalog-secret-set-${key}`}
              disabled={installing}
              onClick={() => setSecretModalKey(key)}
            >
              Set {key}
            </button>
            <a
              class="readiness-secret-deeplink"
              href={`#/secrets?key=${encodeURIComponent(key)}`}
              data-testid={`catalog-secret-open-secrets-${key}`}
            >
              Open Secrets
            </a>
          </div>
        )}
      />
      <div class="catalog-install-gate-actions">
        <button
          type="button"
          class="btn btn-secondary"
          data-testid={`catalog-install-confirm-${name}`}
          disabled={confirmDisabled}
          onClick={onConfirmInstall}
        >
          {installing
            ? "Installing…"
            : blockers > 0
              ? `Fix ${blockers} blocker${blockers === 1 ? "" : "s"} to install`
              : "Install"}
        </button>
        <button
          type="button"
          class="btn btn-secondary"
          data-testid={`catalog-install-cancel-${name}`}
          disabled={installing}
          onClick={onCancel}
        >
          Cancel
        </button>
      </div>

      {/* AddSecretModal — the single owner of POST /api/secrets — pre-filled
          with the key the operator chose to set inline. On save we re-fetch
          readiness so the just-set secret flips the row from advisory →
          satisfied (and clears any vault blocker). */}
      <AddSecretModal
        // key on the chosen key so the modal mounts with that name captured at
        // first render (AddSecretModal seeds its name field from prefillName via
        // useState) — deterministic prefill regardless of open-transition timing.
        key={secretModalKey ?? "closed"}
        open={secretModalKey !== null}
        prefillName={secretModalKey ?? undefined}
        onClose={() => setSecretModalKey(null)}
        onSaved={() => {
          if (mountedRef.current) setReloadToken((n) => n + 1);
        }}
      />
    </div>
  );
}

// MarketplaceInstallState tracks the per-entry install lifecycle for one
// marketplace row, mirroring the shipped-server PerServerInstall pattern
// (idle → installing → installed → error), keyed per entry id in the parent
// map. The richer terminal states carry the data the row renders inline:
//   "installed"      → hub-mode 201 success (name + resolved port).
//   "name-conflict"  → hub-mode 409: offer a one-click retry under suggestedName.
//   "probe-pending"  → 412 (AVAILABILITY_PROBE_PENDING): the D-3 host-probe
//                      precondition is unmet (host app not detected) — rendered as
//                      its OWN message, never the name-conflict retry.
//   "required-secret-missing" → 412 (REQUIRED_SECRET_MISSING): a REQUIRED vault
//                      secret is unset — rendered with a "set the secret on the
//                      Secrets screen" message + an Open-Secrets deep-link, NOT the
//                      misleading "host app not detected" copy (codex finding 1).
//   "direct-result"  → direct-mode 200/207: per-client updated / failed split.
//   "error"          → any unmodelled failure, rendered as an inline message.
type MarketplaceInstallState =
  | { phase: "idle" }
  | { phase: "installing" }
  | { phase: "installed"; name: string; port: number; warnings: string[] }
  | { phase: "name-conflict"; suggestedName: string }
  | { phase: "probe-pending"; reason: string }
  | { phase: "required-secret-missing"; reason: string }
  | {
      phase: "direct-result";
      partial: boolean;
      clientsUpdated: string[];
      clientsFailed: Array<{ client: string; error: string }>;
    }
  | { phase: "error"; message: string };

const MARKETPLACE_IDLE: MarketplaceInstallState = { phase: "idle" };

// MarketplaceRow is the unified discriminated row the theme grouper folds: either
// an installable entry (rendered as MarketplaceCard) or an S4 docs-only pointer
// (rendered as MarketplaceDocsOnlyCard). Both carry `categories` + `name`, so they
// group under the same coarse themes (PR #443).
type MarketplaceRow =
  | { kind: "entry"; entry: MarketplaceEntry }
  | { kind: "docs-only"; docs: MarketplaceDocsOnlyEntry };

// MarketplaceSection renders the curated marketplace registry as a one-click
// Store. Each entry installs straight from the GUI per the two-tier rule:
// stdio entries get a single "Add to hub" action; http entries additionally
// offer "Install directly" (a remote-URL write into a chosen client set). An
// empty list (fetch/cache miss or genuinely empty registry) renders a muted
// notice rather than nothing, so operators know the section exists.
function MarketplaceSection({
  entries,
  docsOnly,
  installedServers,
  directClients,
  onRefreshed,
}: {
  entries: MarketplaceEntry[];
  // S4 manual-install POINTER rows (the catalog's separate docs_only[] array).
  // Rendered alongside the installable entries — grouped under the same themes —
  // but as a DOCS-ONLY pointer card (badge + readme link + setup steps), never an
  // install affordance.
  docsOnly: MarketplaceDocsOnlyEntry[];
  // The backend-derived URL-native client set the direct-install multiselect
  // may offer (directInstallableClients of the /api/client-capabilities map).
  // Empty until capabilities load; a relay-stdio client is never in it, so
  // direct install can't be attempted against a client that would reject it.
  directClients: string[];
  // Names of servers already running per /api/status (same Set the shipped
  // store uses). A marketplace entry whose id OR name is in this set is
  // already installed, so we render an "Installed" badge instead of an
  // install affordance — never offer to install an already-running server.
  installedServers: Set<string>;
  // Called with the freshly-fetched entries + docs_only rows after a successful
  // force-refresh so the parent's marketplace state re-renders the section with the
  // updated registry (the refresh bypasses the 24h TTL + ETag and rewrites the cache).
  onRefreshed: (entries: MarketplaceEntry[], docsOnly: MarketplaceDocsOnlyEntry[]) => void;
}) {
  // Per-row install lifecycle. A row absent from the map is "idle".
  const [states, setStates] = useState<Record<string, MarketplaceInstallState>>({});
  // Force-refresh lifecycle for the "Refresh" button: "idle" → click →
  // "refreshing" (button disabled, POST in flight) → "idle" on success (the
  // parent re-renders with the new entries) or "error" with the inline
  // message on a backend 500 (the cache was NOT updated).
  const [refreshState, setRefreshState] = useState<
    { phase: "idle" } | { phase: "refreshing" } | { phase: "error"; message: string }
  >({ phase: "idle" });
  // mountedRef guards post-await setState against the "operator navigated away
  // mid-POST" race (mirrors the shipped-server install handlers above).
  const mountedRef = useRef(true);
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  async function runRefresh() {
    // Guard against double-fire while a refresh POST is in flight.
    if (refreshState.phase === "refreshing") return;
    setRefreshState({ phase: "refreshing" });
    try {
      const { entries: rows, docsOnly: docs } = await refreshMarketplace();
      if (!mountedRef.current) return;
      // The api helper already normalizes the entries (transport → string) and
      // carries the docs_only[] pointer rows through, so both are safe to hand
      // straight to the parent (same shape the GET load path produces).
      onRefreshed(rows, docs);
      setRefreshState({ phase: "idle" });
    } catch (err) {
      if (!mountedRef.current) return;
      setRefreshState({ phase: "error", message: (err as Error).message });
    }
  }

  function setState(id: string, next: MarketplaceInstallState) {
    setStates((prev) => ({ ...prev, [id]: next }));
  }

  // Group BOTH the installable entries AND the S4 docs-only pointer rows into
  // coarse-theme sections (Engineering & CAD, Development & Code, Data & Office,
  // Research & Docs, Music & Audio, Utilities, Other) over the per-row `categories`
  // field. A unified discriminated row carries either an install entry or a pointer,
  // so a docs-only pointer groups under the SAME theme as the entries (PR #443) and
  // renders its own pointer card. This is a pure render REORGANIZATION; each row's
  // affordance/probe-state/two-tier rule/ARIA is untouched. Empty entries + empty
  // docs_only yields zero sections, so the empty-state card below renders instead.
  const themeSections = useMemo(() => {
    const rows: MarketplaceRow[] = [
      ...entries.map((e): MarketplaceRow => ({ kind: "entry", entry: e })),
      ...docsOnly.map((d): MarketplaceRow => ({ kind: "docs-only", docs: d })),
    ];
    return groupByTheme(
      rows,
      (r) => (r.kind === "entry" ? r.entry.categories ?? [] : r.docs.categories ?? []),
      (r) => (r.kind === "entry" ? r.entry.name : r.docs.name),
    );
  }, [entries, docsOnly]);

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
        setState(id, {
          phase: "installed",
          name: result.name,
          port: result.port,
          warnings: result.warnings,
        });
      } else if (result.kind === "name-conflict") {
        setState(id, { phase: "name-conflict", suggestedName: result.suggestedName });
      } else if (result.kind === "probe-pending") {
        setState(id, { phase: "probe-pending", reason: result.reason });
      } else if (result.kind === "required-secret-missing") {
        setState(id, { phase: "required-secret-missing", reason: result.reason });
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
      <div class="catalog-marketplace-header">
        <h2>Marketplace</h2>
        <button
          type="button"
          class="btn"
          data-testid="catalog-marketplace-refresh"
          disabled={refreshState.phase === "refreshing"}
          onClick={() => runRefresh()}
        >
          {refreshState.phase === "refreshing" ? "Refreshing…" : "Refresh"}
        </button>
        <InfoTip
          label="What does Refresh do?"
          text="Re-fetches the curated registry now, bypassing the 24-hour cache, so newly published servers appear without waiting."
        />
      </div>
      <p class="catalog-intro">
        Discover and install MCP servers from the curated registry with one
        click.
      </p>
      {refreshState.phase === "error" && (
        <p class="error" data-testid="catalog-marketplace-refresh-error">
          {refreshState.message}
        </p>
      )}
      {entries.length === 0 && docsOnly.length === 0 ? (
        <p class="empty-state" data-testid="catalog-marketplace-empty">
          No marketplace entries available right now.
        </p>
      ) : (
        // Grouped-by-theme render. Each non-empty theme gets a section header
        // and its own cards grid; themes with no members are dropped by
        // groupByTheme so no empty headers appear. The catalog-marketplace-cards
        // testid is kept on a wrapper so existing coverage that counts all
        // marketplace cards still resolves across the grouped sections. Each row is
        // either an installable entry (MarketplaceCard) or an S4 docs-only pointer
        // (MarketplaceDocsOnlyCard).
        <div data-testid="catalog-marketplace-cards">
          {themeSections.map((section) => (
            <section
              class="catalog-theme-section"
              key={section.theme}
              data-testid={`catalog-theme-section-${section.theme}`}
              aria-label={section.theme}
            >
              <h3
                class="catalog-theme-header"
                data-testid={`catalog-theme-header-${section.theme}`}
              >
                {section.theme}
              </h3>
              <div class="cards" data-testid={`catalog-theme-cards-${section.theme}`}>
                {section.entries.map((row) =>
                  row.kind === "entry" ? (
                    <MarketplaceCard
                      key={row.entry.id}
                      entry={row.entry}
                      // An entry is already installed if /api/status reports a daemon
                      // whose server name matches the entry id OR its display name
                      // (e.g. the shipped `fetch` hub daemon is also a catalog entry —
                      // we must not offer to install it as "fetch-2").
                      installed={
                        installedServers.has(row.entry.id) || installedServers.has(row.entry.name)
                      }
                      state={states[row.entry.id] ?? MARKETPLACE_IDLE}
                      directClients={directClients}
                      onInstall={runInstall}
                    />
                  ) : (
                    <MarketplaceDocsOnlyCard key={row.docs.id} entry={row.docs} />
                  ),
                )}
              </div>
            </section>
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
  installed,
  state,
  directClients,
  onInstall,
}: {
  entry: MarketplaceEntry;
  // True when /api/status already reports this entry's server as running.
  // Suppresses the install affordance in favour of an "Installed" badge so
  // the GUI never offers to install an already-running server.
  installed: boolean;
  state: MarketplaceInstallState;
  // The backend-derived URL-native client set the direct-install multiselect
  // renders (directInstallableClients). A relay-stdio client is never present,
  // so the operator can't pick a client a direct install would reject.
  directClients: string[];
  onInstall: (
    id: string,
    mode: "hub" | "direct",
    opts?: { name?: string; clients?: string[] },
  ) => void;
}) {
  const isHttp = entry.transport === "http";
  // D-3 (Tier-0, mirror-gate): the TRI-STATE browse verdict drives the affordance.
  // "ready" and "inert-unknown" both show the install block — "ready" is detected
  // now, and "inert-unknown" carries a files[]/path-shaped probe the browse path
  // deliberately did NOT touch, so the Catalog still offers install and the real
  // probe runs at the install-time gate (a tooltip explains this). Only
  // "inert-blocked" (host app provably absent) shows the greyed "probe to enable"
  // badge. resolveProbeState falls back to the deprecated probe_passes bool when
  // an older backend omits probe_state (fail-closed).
  const probeState = resolveProbeState(entry);
  const showInstall = probeState === "ready" || probeState === "inert-unknown";
  const showProbeBadge = probeState === "inert-blocked";
  const isInertUnknown = probeState === "inert-unknown";
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
      <div class="card-title">
        <span>{entry.name}</span>
        {entry.summary && (
          <InfoTip
            label={`About ${entry.name}`}
            text={entry.summary}
            data-testid={`catalog-marketplace-summary-${entry.id}`}
          />
        )}
      </div>
      {entry.categories.length > 0 && (
        <p class="catalog-marketplace-categories">
          {entry.categories.map((c) => (
            <span class="lsp-chip" key={c}>
              {c}
            </span>
          ))}
        </p>
      )}

      {/* Install affordance. stdio → hub only; http → hub + direct. (docs-only
          POINTER rows are NOT entries — they render via MarketplaceDocsOnlyCard, so
          this card never handles them.) FIRST: a server already running per
          /api/status (e.g. the shipped `fetch` daemon, which is also a catalog
          entry) shows an "Installed" badge and NO install affordance — we must never
          offer to re-install it (which would hit NAME_CONFLICT → suggest fetch-2). */}
      {installed ? (
        <span
          class="lsp-chip lsp-chip-via-hub"
          data-testid={`catalog-marketplace-installed-badge-${entry.id}`}
        >
          installed
        </span>
      ) : state.phase === "installed" ? (
        <div role="status" data-testid={`catalog-marketplace-installed-${entry.id}`}>
          <p class="catalog-marketplace-status catalog-marketplace-status-ok">
            Added to hub as <strong>{state.name}</strong>
            {state.port > 0 ? ` on port ${state.port}.` : "."}
          </p>
          {/* Operator-facing install notices from GenerateDraftManifest (e.g. the
              kind:global ${workspaceFolder} freeze-to-CWD footgun). A non-error
              NOTICE — the install SUCCEEDED, these are caveats the one-click
              operator must still read, so it uses the warning idiom (left border +
              warning bg), never the scary danger styling. */}
          {state.warnings.length > 0 && (
            <div
              class="catalog-marketplace-install-warning"
              data-testid={`catalog-marketplace-install-warnings-${entry.id}`}
            >
              <p class="catalog-marketplace-install-warning-title">Heads up:</p>
              <ul>
                {state.warnings.map((w, i) => (
                  <li key={i}>{w}</li>
                ))}
              </ul>
            </div>
          )}
        </div>
      ) : showProbeBadge ? (
        /* D-3 (Tier-0): inert-blocked watch / disabled-until-probe row — the host
           app is provably not detected yet (a bare binary absent from PATH, or a
           fail-closed nil probe). Greyed, no install affordance, "probe to enable"
           badge. Installing it would only hit the backend's blocking
           availability-probe AdmissionError, so the UI suppresses the buttons
           until the host app is detected. An inert-UNKNOWN row (files[]/path-shaped
           probe the browse path deferred) is NOT here — it shows the install
           block below with an extra tooltip. */
        <span
          class="lsp-chip catalog-marketplace-probe-to-enable"
          data-testid={`catalog-marketplace-probe-to-enable-${entry.id}`}
          title="This server's host app or tool isn't detected on this machine yet. Install it, then this row enables."
        >
          probe to enable
        </span>
      ) : showInstall ? (
        /* showInstall: "ready" or "inert-unknown". The latter adds a tooltip
           explaining that clicking install runs the real host probe. */
        <div class="catalog-marketplace-install" data-testid={`catalog-marketplace-install-${entry.id}`}>
          <div class="catalog-marketplace-actions">
            <button
              type="button"
              class="btn-primary"
              data-testid={`catalog-marketplace-hub-${entry.id}`}
              disabled={installing}
              title={
                isInertUnknown
                  ? "host app not auto-detected; clicking install verifies it and reports if missing"
                  : undefined
              }
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
                {directClients.map((client) => (
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

          {/* 412 AVAILABILITY_PROBE_PENDING: the host app/tool isn't detected
              yet. Rendered as its OWN message — distinct from the 409
              name-conflict retry — so the operator knows to install the host app
              first, not to rename. */}
          {state.phase === "probe-pending" && (
            <p
              class="catalog-marketplace-status catalog-marketplace-status-warn"
              role="alert"
              data-testid={`catalog-marketplace-probe-pending-${entry.id}`}
            >
              Host app not detected yet — {state.reason}
            </p>
          )}

          {/* 412 REQUIRED_SECRET_MISSING: a REQUIRED vault secret is unset (or
              empty). DISTINCT from the probe-pending message above — the fix is
              to SET the secret, not to install a host app (codex finding 1). The
              backend reason already names the key + the fix; we add an
              Open-Secrets deep-link reusing the existing #/secrets nav. */}
          {state.phase === "required-secret-missing" && (
            <div
              class="catalog-marketplace-status catalog-marketplace-status-warn"
              role="alert"
              data-testid={`catalog-marketplace-required-secret-${entry.id}`}
            >
              {state.reason}{" "}
              <a
                href="#/secrets"
                data-testid={`catalog-marketplace-required-secret-open-${entry.id}`}
              >
                Open Secrets
              </a>
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
      ) : null}

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

// MarketplaceDocsOnlyCard renders ONE S4 manual-install POINTER row (from the
// catalog's separate docs_only[] array): name + summary + categories, a distinct
// DOCS-ONLY badge, the readme link, and a collapsible "view setup" block with the
// verbatim manual_install steps. It NEVER renders an install affordance — a
// docs_only row is a server the hub never installs (immature, git-clone-only,
// macOS-only, or a LAN-bind risk). homepage + readme_url come from an UNTRUSTED
// registry, so each link renders only when it is an http(s) URL (same guard as the
// entry card's homepage link).
function MarketplaceDocsOnlyCard({ entry }: { entry: MarketplaceDocsOnlyEntry }) {
  return (
    <div
      class="card catalog-card catalog-marketplace-card catalog-marketplace-docs-only"
      data-testid={`catalog-marketplace-docs-only-${entry.id}`}
    >
      <div class="card-title">
        <span>{entry.name}</span>
        {entry.summary && (
          <InfoTip
            label={`About ${entry.name}`}
            text={entry.summary}
            data-testid={`catalog-marketplace-docs-only-summary-${entry.id}`}
          />
        )}
      </div>
      {entry.categories.length > 0 && (
        <p class="catalog-marketplace-categories">
          {entry.categories.map((c) => (
            <span class="lsp-chip" key={c}>
              {c}
            </span>
          ))}
        </p>
      )}

      <span
        class="lsp-chip catalog-marketplace-docs-only-badge"
        data-testid={`catalog-marketplace-docs-only-badge-${entry.id}`}
        title="This server isn't one-click installable through mcphub — follow the manual setup steps below."
      >
        DOCS-ONLY · manual install
      </span>

      {/* readme link — UNTRUSTED external registry value, so only render it when it
          is an http(s) URL (same guard as the homepage link below). */}
      {entry.readme_url && /^https?:\/\//i.test(entry.readme_url) && (
        <p class="catalog-marketplace-docs-only-readme">
          <a
            href={entry.readme_url}
            target="_blank"
            rel="noopener noreferrer"
            data-testid={`catalog-marketplace-docs-only-readme-${entry.id}`}
          >
            Readme
          </a>
        </p>
      )}

      {entry.manual_install && (
        <details
          class="catalog-marketplace-docs-only-setup"
          data-testid={`catalog-marketplace-docs-only-setup-${entry.id}`}
        >
          <summary>View setup steps</summary>
          <p
            class="catalog-marketplace-docs-only-steps"
            data-testid={`catalog-marketplace-docs-only-steps-${entry.id}`}
          >
            {entry.manual_install}
          </p>
        </details>
      )}

      {/^https?:\/\//i.test(entry.homepage) && (
        <p class="catalog-marketplace-homepage">
          <a
            href={entry.homepage}
            target="_blank"
            rel="noopener noreferrer"
            data-testid={`catalog-marketplace-docs-only-homepage-${entry.id}`}
          >
            Homepage
          </a>
        </p>
      )}
    </div>
  );
}
