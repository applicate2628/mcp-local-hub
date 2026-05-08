import { useEffect, useRef, useState, useCallback } from "preact/hooks";
import { fetchOrThrow } from "../api";
import type { HealthSnapshot, ProbeRow } from "../types";

// Codex bot PR #144 round-8 P2 + r10 architecture MINOR: the sentinel
// err string emitted by health.go::computeProbesSection for stopped /
// probe-disabled daemons. NOT a hard failure — operator state.
// If the backend wording changes, both sides need updating in lockstep.
// Future-proof: G4 should add a stable `probe.reason` enum on the wire.
const PROBE_NOT_RUNNING_SENTINEL = "no probe (daemon not running or probe disabled)";

// isActionableProbeFailure returns true ONLY for probe rows that
// represent a real backend error (HTTP 500, parse failures, timeouts).
// Stopped / probe-disabled daemons (sentinel err) return false —
// they're a normal operator state, not a failure to display.
function isActionableProbeFailure(p: ProbeRow): boolean {
  return !p.ok && p.err !== PROBE_NOT_RUNNING_SENTINEL;
}
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

  // Codex bot PR #144 r10 reliability MINOR: single-flight guard for
  // refresh. The `refreshing` state alone races: two click handlers
  // firing in the same browser tick read the SAME stale `refreshing
  // === false` value before Preact commits the disabled prop. A
  // synchronous ref captures the in-flight state instantly so any
  // second-click in the same tick early-exits.
  const refreshInFlightRef = useRef(false);

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
    // Codex bot PR #144 r10 reliability MINOR: synchronous single-flight
    // guard. `refreshing` state alone has a one-tick gap before Preact
    // commits the disabled prop; the ref closes that gap. Set the ref
    // BEFORE setRefreshing so two same-tick clicks both observe the
    // ref-true and the second early-exits.
    if (refreshInFlightRef.current) return;
    refreshInFlightRef.current = true;
    setRefreshing(true);
    setRefreshError(null);
    fetchOrThrow<HealthSnapshot>("/api/health?include=capabilities&refresh=true", "object")
      .then((data) => {
        refreshInFlightRef.current = false;
        if (!mountedRef.current) return;
        setState({ status: "ok", data });
        setRefreshing(false);
      })
      .catch((err: Error) => {
        refreshInFlightRef.current = false;
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
  }, []);

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

  // Codex bot PR #144 rounds 4 + 6 + 7 P2: rows.length === 0 alone is
  // ambiguous. health.go::computeCapabilitiesSection skips `if !p.OK`
  // (including intentionally stopped / probe-disabled daemons —
  // legitimate empty rows, NOT failures) and accumulates true
  // per-daemon fetch errors into section.Errors with `continue`.
  // PLUS — round 7 P2 — probe failures arrive as
  // `probes.items[*].ok === false` with non-empty err message, NOT
  // necessarily into `probes.errors[]`. Both shapes signal failures,
  // so the classifier must check both.
  const probeErrors = state.data.probes?.errors ?? [];
  const capabilityErrors = caps?.errors ?? [];
  const daemonErrors = state.data.daemons?.errors ?? [];
  // Codex bot PR #144 r8 P2 + r10 architecture MINOR: predicate
  // extracted to module level (isActionableProbeFailure) so the
  // sentinel string isn't duplicated in two places.
  const failedProbes = state.data.probes?.items.filter(isActionableProbeFailure) ?? [];

  // Codex bot PR #144 r10 P2: the failure-empty banner used to
  // interpolate `daemonCount` (total known daemons), which overstated
  // impact when only some daemons failed (1 stopped + 1 real error
  // misreported as "failed for 2 daemons"). Count UNIQUE failing
  // daemons by extracting `<server>/<daemon>` identities from the
  // error sources. SectionError.scope shapes seen in health.go:
  //   - `daemon:<server>/<daemon>` (daemonErrors)
  //   - `probe:<server>/<daemon>` (probeErrors)
  //   - `capability:<server>/<daemon>` (capabilityErrors)
  // failedProbes carries explicit `{server, daemon}` fields.
  const failedDaemonSet = new Set<string>();
  for (const p of failedProbes) {
    failedDaemonSet.add(`${p.server}/${p.daemon}`);
  }
  const SCOPE_DAEMON_RE = /^(?:daemon|probe|capability):(.+)$/;
  for (const e of [...probeErrors, ...capabilityErrors, ...daemonErrors]) {
    const m = SCOPE_DAEMON_RE.exec(e.scope);
    if (m) failedDaemonSet.add(m[1]);
    // If scope doesn't match the daemon-specific pattern (e.g.
    // `wmic` for system-level errors), it's NOT a per-daemon
    // failure and is counted in the error-list but not the
    // banner's daemon count.
  }
  const failedDaemonCount = failedDaemonSet.size;
  const hasFailures =
    probeErrors.length > 0 ||
    capabilityErrors.length > 0 ||
    daemonErrors.length > 0 ||
    failedProbes.length > 0;
  const showFailureEmpty = rows.length === 0 && hasFailures;
  // Codex bot PR #144 round 7 P2 (#2): section errors must render
  // alongside successful cards too — partial failures (one daemon
  // succeeds, another fails) would otherwise lose visibility.
  // Extract the renderer + render OUTSIDE the empty-state branch.
  const renderSectionErrors = () => (
    <ul class="capabilities-section-errors" data-testid="capabilities-section-errors">
      {daemonErrors.map((e, i) => (
        <li key={`daemon-${i}`}><strong>{e.scope}</strong>: {e.err}</li>
      ))}
      {probeErrors.map((e, i) => (
        <li key={`probe-${i}`}><strong>{e.scope}</strong>: {e.err}</li>
      ))}
      {capabilityErrors.map((e, i) => (
        <li key={`capability-${i}`}><strong>{e.scope}</strong>: {e.err}</li>
      ))}
      {failedProbes.map((p) => (
        <li key={`failedprobe-${p.server}-${p.daemon}`}>
          <strong>probe:{p.server}/{p.daemon}</strong>: {p.err || "ok=false (no message)"}
        </li>
      ))}
    </ul>
  );

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

      {/* Section errors banner: render whenever any failures exist,
          even if some cards rendered successfully (round 7 P2 #2). */}
      {hasFailures && rows.length > 0 && (
        <div class="capabilities-partial-failures" data-testid="capabilities-partial-failures">
          <p class="error" role="alert">Some daemons reported probe or capability failures:</p>
          {renderSectionErrors()}
        </div>
      )}

      {rows.length === 0 ? (
        showFailureEmpty ? (
          <div class="capabilities-empty" data-testid="capabilities-empty-failures">
            <p class="error" role="alert">
              Capabilities not yet available — {failedDaemonCount > 0
                ? `probes or capability fetch failed for ${failedDaemonCount} daemon${failedDaemonCount === 1 ? "" : "s"}`
                : "see backend errors below"}.
            </p>
            {renderSectionErrors()}
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
