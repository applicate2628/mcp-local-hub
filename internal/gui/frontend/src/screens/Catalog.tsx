import { useEffect, useRef, useState } from "preact/hooks";
import { fetchOrThrow } from "../api";
import type { DaemonStatus } from "../types";

// Mirrors manifestListResponse in internal/gui/manifest.go — the GET
// /api/manifests body shape ({ "manifests": ["serena", "memory", ...] }).
// These are the supported/shipped server NAMES (embed-first union of the
// installed servers/ dir and the embedded defaults). Empty set is a
// normal first-run state the screen renders as a friendly empty card,
// not an error (the backend returns 200 [] per its handler comment).
interface ManifestListResponse {
  manifests: string[];
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

// CatalogScreen is the v1 slice of the "§10 GUI MCP Store": browse every
// supported/shipped MCP server and install any with one click. It fetches
// /api/manifests (the catalog) and /api/status (which servers are already
// running) in parallel on mount, marks each row as installed or offers an
// Install button, and POSTs /api/install?name=<server> on click.
export function CatalogScreen() {
  const [manifests, setManifests] = useState<string[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  // installedServers is the set of server names that already appear in
  // /api/status. The status array carries one row per daemon, so multiple
  // rows can share a `server`; collapse to a Set of names.
  const [installedServers, setInstalledServers] = useState<Set<string>>(new Set());
  // Per-row install lifecycle. A row absent from the map is "idle".
  const [installStates, setInstallStates] = useState<Record<string, InstallState>>({});
  // Bump to re-run the load effect after an install settles so the
  // /api/status refresh re-resolves the now-running server into the
  // installed set (mirrors ServersScreen's reloadToken pattern).
  const [reloadToken, setReloadToken] = useState(0);

  // mountedRef guards post-await setState in the click handler against the
  // "operator navigated away mid-POST" race (mirrors ServersScreen).
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
          fetchOrThrow<ManifestListResponse>("/api/manifests", "object"),
          fetchOrThrow<DaemonStatus[]>("/api/status", "array"),
        ]);
        if (cancelled) return;
        const names = Array.isArray(list.manifests) ? list.manifests : [];
        setManifests(names);
        const installed = new Set<string>();
        for (const row of status) {
          if (row.server) installed.add(row.server);
        }
        setInstalledServers(installed);
        setError(null);
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

  if (error) {
    return (
      <section class="screen catalog" data-testid="catalog-error">
        <h1>Catalog</h1>
        <p class="error">Failed to load: {error}</p>
      </section>
    );
  }

  if (!manifests) {
    return (
      <section class="screen catalog">
        <h1>Catalog</h1>
        <p>Loading…</p>
      </section>
    );
  }

  if (manifests.length === 0) {
    return (
      <section class="screen catalog">
        <h1>Catalog</h1>
        <p class="empty-state" data-testid="catalog-empty">
          No supported servers found.
        </p>
      </section>
    );
  }

  return (
    <section class="screen catalog">
      <h1>Catalog</h1>
      <p class="catalog-intro">
        Browse the supported MCP servers and install any with one click.
      </p>
      <div class="cards" data-testid="catalog-cards">
        {manifests.map((name) => {
          const state = installStates[name] ?? IDLE;
          // A row is "installed" if /api/status already reports it OR the
          // most recent install POST for this row returned 204.
          const installed = installedServers.has(name) || state.phase === "installed";
          return (
            <div class="card catalog-card" key={name} data-testid={`catalog-card-${name}`}>
              <div class="card-title">{name}</div>
              <div class="catalog-card-actions">
                {installed ? (
                  <span
                    class="lsp-chip lsp-chip-via-hub"
                    data-testid={`catalog-installed-${name}`}
                  >
                    installed
                  </span>
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
            </div>
          );
        })}
      </div>
    </section>
  );
}
