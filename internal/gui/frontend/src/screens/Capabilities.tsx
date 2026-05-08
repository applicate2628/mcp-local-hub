import { useEffect, useRef, useState, useCallback } from "preact/hooks";
import { fetchOrThrow } from "../api";
import type { HealthSnapshot } from "../types";
import { CapabilityCard } from "../components/CapabilityCard";
import { CapabilityLegend } from "../components/CapabilityLegend";

// G3 — capability status display screen. Read-only view of the G2
// /api/health snapshot. NO tool execution; items render as labels only.

type LoadState =
  | { status: "loading" }
  | { status: "ok"; data: HealthSnapshot }
  | { status: "error"; error: string };

export function CapabilitiesScreen() {
  const [state, setState] = useState<LoadState>({ status: "loading" });
  const [refreshing, setRefreshing] = useState(false);
  const [refreshError, setRefreshError] = useState<string | null>(null);

  // Codex stage-1 BLOCKER fix: mountedRef gates ALL setState in the
  // event-handler-driven refresh path. The mount effect's local
  // cancelled-flag covers only the initial fetch — refreshes fired
  // from a click handler need their own mounted check, otherwise
  // setState fires on a stale instance after unmount.
  const mountedRef = useRef(true);
  useEffect(() => {
    return () => {
      mountedRef.current = false;
    };
  }, []);

  // On-mount fetch. cancelled-flag prevents setState after unmount
  // (mirrors Dashboard.tsx:41-63 / About.tsx:28-40 — pattern preserved).
  useEffect(() => {
    let cancelled = false;
    fetchOrThrow<HealthSnapshot>("/api/health?include=capabilities", "object")
      .then((data) => {
        if (!cancelled) setState({ status: "ok", data });
      })
      .catch((err: Error) => {
        if (!cancelled) setState({ status: "error", error: err.message });
      });
    return () => { cancelled = true; };
  }, []);

  const onRefresh = useCallback(() => {
    if (refreshing) return;  // belt + suspenders for the disabled button
    setRefreshing(true);
    setRefreshError(null);
    fetchOrThrow<HealthSnapshot>("/api/health?include=capabilities&refresh=true", "object")
      .then((data) => {
        if (!mountedRef.current) return;
        setState({ status: "ok", data });
        setRefreshing(false);
      })
      .catch((err: Error) => {
        if (!mountedRef.current) return;
        // Codex bot PR #144 round-3 P2: if the screen is currently in
        // the error branch (initial load failed), updating only
        // `refreshError` leaves `state.error` stale — the alert keeps
        // showing the original failure message after every retry. Use
        // the functional updater to read current status; when error,
        // promote the new error to state.error so the displayed
        // message reflects the LATEST retry. When ok, keep state
        // unchanged and surface the inline refresh-error in the header.
        setState((prev) => {
          if (prev.status === "error") {
            return { status: "error", error: err.message };
          }
          return prev;
        });
        setRefreshError(err.message);
        setRefreshing(false);
      });
  }, [refreshing]);

  if (state.status === "loading") {
    return (
      <section class="capabilities-screen" data-testid="capabilities-screen">
        <h1>Capabilities</h1>
        <p>Loading…</p>
      </section>
    );
  }

  if (state.status === "error") {
    // Codex bot PR #144 round-2 P2: error state must keep the Refresh
    // affordance so transient failures (daemon still starting, brief
    // network hiccup) aren't a dead end. Operator clicks Refresh →
    // onRefresh fires `?refresh=true`; on success state transitions
    // to ok and the full screen renders. The Refresh button is the
    // SAME control as in the ok-state header — single recovery path.
    return (
      <section class="capabilities-screen" data-testid="capabilities-screen">
        <header class="capabilities-header">
          <h1>Capabilities</h1>
          <div class="capabilities-meta">
            <button
              class="capabilities-refresh-btn"
              data-testid="capabilities-refresh-btn"
              onClick={onRefresh}
              disabled={refreshing}
            >
              {refreshing ? "Refreshing…" : "Refresh"}
            </button>
          </div>
        </header>
        <p class="error" role="alert">Failed to load capabilities: {state.error}</p>
      </section>
    );
  }

  const caps = state.data.capabilities;
  const rows = caps?.items ?? [];
  const generatedAt = caps?.generated_at ?? 0;

  // Codex bot PR #144 rounds 4 + 6 P2: rows.length === 0 alone is
  // ambiguous. health.go::computeCapabilitiesSection skips `if !p.OK`
  // (including intentionally stopped / probe-disabled daemons —
  // legitimate empty rows, NOT failures) and accumulates true
  // per-daemon fetch errors into section.Errors with `continue`.
  // The failure-empty branch must trigger ONLY on real backend
  // errors (probes.errors / capabilities.errors / daemons.errors),
  // NOT on `daemonCount > 0` alone — a system with all daemons
  // stopped is healthy-but-empty, not a failure.
  const daemonCount = state.data.daemons?.items.length ?? 0;
  const probeErrors = state.data.probes?.errors ?? [];
  const capabilityErrors = caps?.errors ?? [];
  const daemonErrors = state.data.daemons?.errors ?? [];
  const hasFailures =
    probeErrors.length > 0 || capabilityErrors.length > 0 || daemonErrors.length > 0;
  const showFailureEmpty = rows.length === 0 && hasFailures;

  return (
    <section class="capabilities-screen" data-testid="capabilities-screen">
      <header class="capabilities-header">
        <h1>Capabilities</h1>
        <div class="capabilities-meta">
          {generatedAt > 0 && (
            <span data-testid="capabilities-generated-at">
              Generated {new Date(generatedAt * 1000).toISOString()}
            </span>
          )}
          <button
            class="capabilities-refresh-btn"
            data-testid="capabilities-refresh-btn"
            onClick={onRefresh}
            disabled={refreshing}
          >
            {refreshing ? "Refreshing…" : "Refresh"}
          </button>
        </div>
        <CapabilityLegend />
        {refreshError && (
          <p class="error" role="alert">Refresh failed: {refreshError}</p>
        )}
      </header>

      {rows.length === 0 ? (
        showFailureEmpty ? (
          <div class="capabilities-empty" data-testid="capabilities-empty-failures">
            <p class="error" role="alert">
              Capabilities not yet available — {daemonCount > 0
                ? `probes or capability fetch failed for ${daemonCount} daemon${daemonCount === 1 ? "" : "s"}`
                : "see backend errors below"}.
            </p>
            {(probeErrors.length > 0 || capabilityErrors.length > 0 || daemonErrors.length > 0) && (
              <ul class="capabilities-section-errors">
                {daemonErrors.map((e, i) => (
                  <li key={`daemon-${i}`}><strong>{e.scope}</strong>: {e.err}</li>
                ))}
                {probeErrors.map((e, i) => (
                  <li key={`probe-${i}`}><strong>{e.scope}</strong>: {e.err}</li>
                ))}
                {capabilityErrors.map((e, i) => (
                  <li key={`capability-${i}`}><strong>{e.scope}</strong>: {e.err}</li>
                ))}
              </ul>
            )}
          </div>
        ) : (
          <p class="capabilities-empty" data-testid="capabilities-empty">
            No capabilities found — install servers via the Add server screen.
          </p>
        )
      ) : (
        <div class="capabilities-list">
          {rows.map((row) => {
            const probeMatch = state.data.probes?.items.find(
              (p) => p.server === row.server && p.daemon === row.daemon,
            );
            return (
              <CapabilityCard
                key={`${row.server}-${row.daemon}`}
                row={row}
                probe={probeMatch ?? null}
              />
            );
          })}
        </div>
      )}
    </section>
  );
}
