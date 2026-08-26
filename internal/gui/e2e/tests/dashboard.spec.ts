import { test, expect } from "../fixtures/hub";
import { routePersistentSupervisorDown } from "../fixtures/lsp-helpers";

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
    // Dashboard.tsx gates the RED fail-loud banner on a fresh-observation
    // timestamp compare (Fix B round-2): on a failure it shows a calm
    // reconnecting cue and turns the banner RED only when a RESOLVED failing
    // /api/status observation lands AT/after the ~20s grace bound
    // (RESTART_GRACE_MS) — never on elapsed time alone. So a normal
    // restart/handoff window self-heals before the bound (no RED flash) but a
    // genuine prolonged outage still turns RED (fail-loud preserved). Here the
    // supervisor is genuinely down: one successful /api/status (sets
    // hasEverLoaded=true), then the live ~5s `poller-error` SSE marks
    // `degradedSince`/`lastFailAt`; at the ~20s bound the grace recheck 500s
    // (≤5s backend latency, not aborted) → a fresh past-bound failing
    // observation → RED after ~20s + ≤5s. See the matching note in
    // dashboard-fail-loud.spec.ts. The wire-level 500 STATUS_FAILED contract is
    // asserted there.
    // First /api/status succeeds (sets hasEverLoaded=true, cards render), then
    // the supervisor stays down → PERSISTENT 500 STATUS_FAILED on every later
    // poll. A route that returned 200 forever would let the Dashboard's own 30s
    // poll clear `degradedSince` and make the RED banner OSCILLATE; the
    // persistent 500 keeps the streak monotonic so the banner is stable once
    // the grace elapses (and the failing HTTP poll is a backstop if SSE is slow).
    const supervisorDown = await routePersistentSupervisorDown(page);
    const initialStatus = page.waitForResponse((response) => response.url().endsWith("/api/status"));
    await page.goto(`${hub.url}/#/dashboard`);
    expect((await initialStatus).status()).toBe(200);
    await expect(page.locator("h1")).toHaveText("Dashboard");
    await expect(page.locator(".dashboard-header")).toBeVisible();
    await supervisorDown.emitPollerError();

    // The degraded banner names the operator action (the message comes from
    // api.ErrSupervisorDown, surfaced through the poller-error SSE event).
    const banner = page.locator('[data-testid="dashboard-error"]');
    // The fixture releases poller-error only after the initial 200 has rendered
    // the normal Dashboard header, so the calm reconnecting state proves the
    // fresh failure observation that starts the 20s grace window.
    await expect(page.getByTestId("dashboard-reconnecting")).toBeVisible({ timeout: 5_000 });
    await expect(banner).toBeVisible({ timeout: 24_000 });
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
