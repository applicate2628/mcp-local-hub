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
