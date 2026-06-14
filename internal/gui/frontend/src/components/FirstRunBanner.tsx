import { useCallback, useEffect, useState } from "preact/hooks";
import { fetchOrThrow } from "../api";
import { useEventSource } from "../hooks/useEventSource";
import type { DaemonStatus } from "../types";

// FirstRunBanner is the §14 first-run onboarding affordance. On a fresh
// install the operator lands on an empty GUI with no obvious next step;
// this welcome banner points them at the Add-server flow so they install
// their first MCP server without hunting through the nav.
//
// "First run" === no MCP servers installed yet. The signal is the EXISTING
// /api/status endpoint (the same one the Dashboard polls): an empty array —
// after dropping scheduler-maintenance rows, matching the Dashboard's own
// filter — means nothing is installed. We deliberately reuse that signal
// rather than add a new backend endpoint.
//
// The banner is:
//   - Hidden until /api/status resolves (no welcome flash before we know
//     whether servers exist).
//   - Hidden when any non-maintenance server is present.
//   - Hidden after the operator dismisses it (in-memory for the session —
//     a fresh GUI launch with still-zero servers shows it again, which is
//     the right nudge; persisting the dismissal is intentionally out of
//     scope to keep this simple).
//   - Hidden if /api/status errors. A welcome banner is onboarding sugar,
//     not a load-bearing surface; showing it on a backend error would be
//     misleading, and the Dashboard already owns the error surface.
//
// It also listens to the `daemon-state` SSE stream so that installing the
// first server mid-session hides the banner immediately, without waiting
// for a reload — the same live channel the Dashboard consumes.
export function FirstRunBanner() {
  // null = not yet loaded / unknown; true = zero servers; false = has at
  // least one server. Kept tri-state so the banner never flashes before
  // /api/status resolves.
  const [empty, setEmpty] = useState<boolean | null>(null);
  const [dismissed, setDismissed] = useState(false);

  useEffect(() => {
    let cancelled = false;
    async function loadStatus() {
      try {
        const rows = await fetchOrThrow<DaemonStatus[]>("/api/status", "array");
        if (cancelled) return;
        // Same maintenance filter the Dashboard applies — weekly-refresh
        // scheduler rows are not "installed servers".
        const installed = rows.filter((r) => !r.is_maintenance);
        setEmpty(installed.length === 0);
      } catch {
        // Backend unreachable / error → suppress the banner entirely.
        if (!cancelled) setEmpty(false);
      }
    }
    void loadStatus();
    return () => {
      cancelled = true;
    };
  }, []);

  // A daemon-state delta for a real (non-maintenance) server means at least
  // one server now exists → hide the banner live. We never flip back to
  // "empty" from a delta: a "Gone" transition during teardown should not
  // re-surface the welcome banner mid-session.
  const onDelta = useCallback((ev: MessageEvent) => {
    const body = JSON.parse(ev.data) as DaemonStatus & { state: string };
    if (body.is_maintenance) return;
    if (body.state !== "Gone") setEmpty(false);
  }, []);
  useEventSource("/api/events", { "daemon-state": onDelta });

  if (empty !== true || dismissed) return null;

  return (
    <div class="banner banner-info first-run-banner" data-testid="first-run-banner">
      <strong>Welcome to mcp-local-hub</strong>
      <p style="margin: 4px 0 8px">
        No MCP servers are installed yet. Install your first server to start
        routing it through the hub.
      </p>
      <div class="first-run-banner-actions">
        <a href="#/add-server" class="first-run-banner-cta" data-testid="first-run-banner-cta">
          Install your first MCP server
        </a>
        <button
          type="button"
          class="linklike"
          data-testid="first-run-banner-dismiss"
          onClick={() => setDismissed(true)}
        >
          Dismiss
        </button>
      </div>
    </div>
  );
}
