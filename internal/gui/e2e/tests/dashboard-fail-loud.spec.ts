import { test, expect } from "../fixtures/hub";

// dashboard-fail-loud.spec.ts — v0.6 Workstream B (§3.1).
//
// Bug being guarded: `mcphub status` + the GUI Dashboard painted EVERY
// daemon "Restarting" and the hub "down" while serena + fetch actually
// served verified traffic — a FALSE NEGATIVE. Root: computeDaemonsSection
// (internal/api/health.go) silently FELL BACK to the legacy scheduler scan
// on ErrSupervisorIPCUnavailable, and normalizeDaemonState coerced any
// unknown/blank scheduler state to "failed". A migrated daemon whose
// \mcp-local-hub-* scheduler task was deleted then surfaced as
// failed/Restarting even while the supervisor-owned process served traffic.
//
// Fix: when the supervisor IPC status seam is wired but the supervisor is
// unreachable, the status path FAILS LOUD (api.ErrSupervisorDown → 500
// STATUS_FAILED, "supervisor unreachable — restart the hub") instead of
// rendering stale scheduler rows. The GUI shows an explicit degraded
// surface with a recovery affordance.
//
// Test seam: the hub fixture sets MCPHUB_E2E_SUPERVISOR=none (no supervisor
// spawned) while internal/cli/gui.go wires api.DialSupervisorIPCStatus
// unconditionally, so /api/status dials a supervisor that isn't there →
// ErrSupervisorIPCUnavailable → the fail-loud path. No supervisor fixture
// is needed: the absence of a supervisor IS the degraded state under test.
test.describe("dashboard — supervisor-down fail-loud (Workstream B §3.1)", () => {
  test("Dashboard shows the degraded banner + restart affordance and NO daemon cards", async ({
    page,
    hub,
  }) => {
    // Dashboard.tsx debounces the RED fail-loud banner via a `degradedSince`
    // timestamp: a failure shows a calm reconnecting cue and turns the banner
    // RED only once the failure PERSISTS past RESTART_GRACE_MS (~20s). To
    // exercise the RED banner deterministically here, one successful
    // /api/status (sets hasEverLoaded=true) is followed by the live
    // `poller-error` SSE the backend emits every ~5s poll cycle:
    // The supervisor is genuinely down (MCPHUB_E2E_SUPERVISOR=none), so the
    // backend emits a `poller-error` SSE within one ~5s poll cycle, which sets
    // the Dashboard's `degradedSince`. A GENUINE prolonged outage stays down,
    // so after the RESTART_GRACE_MS (~20s) debounce window it turns the banner
    // RED (fail-loud preserved). A transient restart/handoff window instead
    // stays on the calm `dashboard-reconnecting` cue and self-heals before the
    // bound. The wait below therefore covers ~5s poll + 20s grace + margin.
    // The wire-level 500 STATUS_FAILED contract itself is asserted by the
    // sibling test below.
    // First /api/status succeeds (sets hasEverLoaded=true), then the supervisor
    // stays down → PERSISTENT 500 STATUS_FAILED. A route that returned 200
    // forever would let the 30s poll clear `degradedSince` and make the RED
    // banner OSCILLATE; the persistent 500 keeps the streak monotonic so the
    // banner is stable once the grace elapses (failing HTTP poll backstops SSE).
    let statusCalls = 0;
    await page.route("**/api/status", (r) => {
      statusCalls += 1;
      if (statusCalls === 1) {
        r.fulfill({ status: 200, contentType: "application/json", body: "[]" });
      } else {
        r.fulfill({
          status: 500,
          contentType: "application/json",
          body: JSON.stringify({
            code: "STATUS_FAILED",
            error: "supervisor unreachable — restart the hub",
          }),
        });
      }
    });
    await page.goto(`${hub.url}/#/dashboard`);

    // Heading still renders (the error branch keeps the shell intact).
    await expect(page.locator("h1")).toHaveText("Dashboard");

    // Explicit degraded banner naming the operator action — surfaced via the
    // backend `poller-error` SSE within one 5s poll cycle.
    const banner = page.locator('[data-testid="dashboard-error"]');
    // >= 30s HTTP-poll-set + 20s RESTART_GRACE_MS worst case for a persistent outage.
    await expect(banner).toBeVisible({ timeout: 60_000 });
    await expect(banner).toContainText("supervisor unreachable — restart the hub");

    // NO daemon cards — the operator must NOT see Running daemons painted
    // as failed/Restarting from stale scheduler data (the false negative).
    await expect(page.locator(".cards .card")).toHaveCount(0);

    // The Restart-supervisor recovery affordance is present and actionable.
    await expect(
      page.locator('[data-testid="recovery-restart-supervisor"]'),
    ).toBeVisible();
  });

  test("/api/status fails loud (HTTP 500 STATUS_FAILED) instead of returning stale rows", async ({
    page,
    hub,
  }) => {
    // Query the backend directly to assert the wire contract the Dashboard
    // relies on: a 500 with the degraded marker, not a 200 of scheduler
    // rows. (This is the §12 gate-#0 smoke at the GUI layer — §3.1 proved
    // the status itself lies under the old fallback.)
    const resp = await page.request.get(`${hub.url}/api/status`);
    expect(resp.status()).toBe(500);
    const body = (await resp.json()) as { error?: string; code?: string };
    expect(body.code).toBe("STATUS_FAILED");
    expect(body.error ?? "").toContain("supervisor unreachable — restart the hub");
  });

  test("Servers matrix also fails loud when the supervisor is down", async ({
    page,
    hub,
  }) => {
    // §15 Phase B requires the Servers matrix to show the same degraded
    // state — the matrix consumes /api/status alongside /api/scan, so a
    // fail-loud status surfaces the screen's "Failed to load" banner rather
    // than a misleading matrix of failed daemons.
    await page.goto(`${hub.url}/#/servers`);
    await expect(page.locator("h1")).toHaveText("Servers");
    // The screen's "Failed to load" banner renders instead of the matrix.
    // (The Servers screen aggregates /api/scan + /api/status; /api/scan
    // succeeds on the clean tmpHome, so the failing /api/status drives the
    // banner — assert the fail-loud shape, the matrix being absent, without
    // pinning the exact aggregated message.)
    await expect(page.locator("p.error")).toBeVisible();
    await expect(page.locator("p.error")).toContainText("Failed to load");
    // The matrix table is NOT rendered in the degraded state.
    await expect(page.locator("table.servers-matrix")).toHaveCount(0);
  });
});
