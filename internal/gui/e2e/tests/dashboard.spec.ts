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
    // Dashboard.tsx now has a STARTUP GRACE (STARTUP_GRACE_POLLS = 2) that
    // shows a calm "Loading status…" during the initial supervisor-IPC bind
    // window before the fail-loud banner; the 30s HTTP poll makes the
    // post-grace banner ~90s away. Drive the grace-bypass deterministically:
    // one successful /api/status (sets hasEverLoaded=true), then the live
    // 5s `poller-error` SSE the backend emits because the supervisor is
    // genuinely down renders the banner immediately. See the matching note
    // in dashboard-fail-loud.spec.ts. The wire-level 500 STATUS_FAILED
    // contract is asserted there.
    await page.route("**/api/status", (r) =>
      r.fulfill({ status: 200, contentType: "application/json", body: "[]" }),
    );
    await page.goto(`${hub.url}/#/dashboard`);
    await expect(page.locator("h1")).toHaveText("Dashboard");

    // The degraded banner names the operator action (the message comes from
    // api.ErrSupervisorDown, surfaced through the poller-error SSE event).
    const banner = page.locator('[data-testid="dashboard-error"]');
    await expect(banner).toBeVisible({ timeout: 15_000 });
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
