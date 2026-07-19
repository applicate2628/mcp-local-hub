import { test, expect } from "../fixtures/hub";

test.describe("dashboard", () => {
  // v0.6 Workstream B (§3.1): the e2e fixture sets MCPHUB_E2E_SUPERVISOR=none
  // (no supervisor spawned) while the supervisor IPC status seam IS wired
  // (cli/gui.go wires api.DialSupervisorIPCStatus unconditionally). So
  // /api/status dials a supervisor that isn't there → ErrSupervisorIPCUnavailable.
  // Before Workstream B the status path silently fell back to the legacy
  // scheduler scan (which returned [] under MCPHUB_E2E_SCHEDULER=none), so the
  // Dashboard rendered an empty cards grid. After Workstream B the status path
  // fails loud (500 STATUS_FAILED, "supervisor unreachable — restart the hub")
  // and the Dashboard renders the explicit degraded surface instead — the
  // "Failed to load status" banner plus the Restart-supervisor recovery
  // affordance, and ZERO daemon cards (no Running daemon painted as
  // failed/Restarting from stale scheduler data).
  test("renders Dashboard heading and fail-loud degraded surface when the supervisor is down", async ({
    page,
    hub,
  }) => {
    // Dashboard.tsx debounces the RED fail-loud banner via a `degradedSince`
    // timestamp: on a failure it shows a calm reconnecting cue and turns the
    // banner RED only once the failure PERSISTS past RESTART_GRACE_MS (~20s),
    // so a normal restart/handoff window doesn't flash RED but a genuine
    // prolonged outage still does (fail-loud preserved). Here the supervisor is
    // genuinely down: one successful /api/status (sets hasEverLoaded=true), then
    // the live ~5s `poller-error` SSE sets `degradedSince` → the banner turns
    // RED after ~5s + 20s. See the matching note in dashboard-fail-loud.spec.ts.
    // The wire-level 500 STATUS_FAILED contract is asserted there.
    // First /api/status succeeds (sets hasEverLoaded=true, cards render), then
    // the supervisor stays down → PERSISTENT 500 STATUS_FAILED on every later
    // poll. A route that returned 200 forever would let the Dashboard's own 30s
    // poll clear `degradedSince` and make the RED banner OSCILLATE; the
    // persistent 500 keeps the streak monotonic so the banner is stable once
    // the grace elapses (and the failing HTTP poll is a backstop if SSE is slow).
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
    await expect(page.locator("h1")).toHaveText("Dashboard");

    // The degraded banner names the operator action (the message comes from
    // api.ErrSupervisorDown, surfaced through the poller-error SSE event).
    const banner = page.locator('[data-testid="dashboard-error"]');
    // >= 30s HTTP-poll-set + 20s RESTART_GRACE_MS worst case for a persistent outage.
    await expect(banner).toBeVisible({ timeout: 60_000 });
    await expect(banner).toContainText("supervisor unreachable — restart the hub");

    // No daemon cards in the degraded state.
    await expect(page.locator(".cards .card")).toHaveCount(0);

    // The Restart-supervisor recovery affordance must be present so the
    // operator can act on the "restart the hub" guidance.
    await expect(
      page.locator('[data-testid="recovery-restart-supervisor"]'),
    ).toBeVisible();
  });

  test("opens an EventSource to /api/events on mount", async ({ page, hub }) => {
    const reqPromise = page.waitForRequest(
      (r) => r.url() === `${hub.url}/api/events`,
      { timeout: 5_000 },
    );
    await page.goto(`${hub.url}/#/dashboard`);
    const req = await reqPromise;
    expect(req.method()).toBe("GET");
  });
});
