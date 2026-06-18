import { test, expect } from "../fixtures/hub";

test.describe("capabilities", () => {
  test("sidebar Capabilities link navigates to #/capabilities and shows h1", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/dashboard`);
    const link = page.locator("nav a", { hasText: "Capabilities" });
    await expect(link).toHaveCount(1);
    await link.click();
    await expect(page).toHaveURL(/#\/capabilities$/);
    const h1 = page.locator("h1");
    await expect(h1).toHaveText("Capabilities");
    await expect(link).toHaveClass(/active/);
  });

  test("empty-state copy renders when no servers are installed", async ({ page, hub }) => {
    // v0.6 Workstream B (§3.1): the e2e fixture runs with no supervisor
    // (MCPHUB_E2E_SUPERVISOR=none), so the real /api/health now FAILS
    // LOUD ("supervisor unreachable — restart the hub", HTTP 500) and the
    // Capabilities screen renders its error state, not the empty-state.
    // To exercise the empty-state render path, stub /api/health with a
    // valid HealthSnapshot carrying zero capability/probe/daemon rows and
    // no section errors — the same page.route convention servers.spec.ts
    // uses to drive the matrix past the supervisor-down banner. The
    // snapshot shape mirrors internal/api/health.go HealthSnapshot +
    // frontend src/types.ts.
    await page.route("**/api/health?include=capabilities", (r) =>
      r.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          schema_version: "1",
          hub: {
            version: "0.0.0-e2e",
            commit: "e2e",
            build_date: "e2e",
            started_at: "2026-01-01T00:00:00Z",
            lock: { pid: 0, port: 0 },
            generated_at: 0,
            ttl_ms: null,
          },
          daemons: { items: [], generated_at: 0, ttl_ms: 0, errors: [] },
          probes: { items: [], generated_at: 0, ttl_ms: 0, errors: [] },
          capabilities: { items: [], generated_at: 0, ttl_ms: 0, errors: [] },
        }),
      }),
    );
    await page.goto(`${hub.url}/#/capabilities`);
    const empty = page.locator('[data-testid="capabilities-empty"]');
    await expect(empty).toBeAttached();
    await expect(empty).toContainText("No capabilities found");
  });

  test("Refresh button issues a /api/health?include=capabilities&refresh=true request", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/capabilities`);
    const button = page.locator('[data-testid="capabilities-refresh-btn"]');
    await expect(button).toBeAttached();
    // Codex Phase 8 review MINOR fix: assert exact pathname + search,
    // not loose `.includes()` matches (which could match nearby
    // endpoints or extra query variants the implementation never
    // emits). Exact-shape match pins the contract: pathname is
    // `/api/health` and the query string is exactly
    // `include=capabilities&refresh=true` in that order.
    const req = page.waitForRequest((r) => {
      const u = new URL(r.url());
      return u.pathname === "/api/health" &&
        u.search === "?include=capabilities&refresh=true";
    });
    await button.click();
    await req;
  });
});
