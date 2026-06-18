import { test, expect } from "../fixtures/hub";

test.describe("shell", () => {
  test("renders sidebar with brand + ten nav links", async ({ page, hub }) => {
    await page.goto(`${hub.url}/`);
    await expect(page.locator(".sidebar .brand")).toHaveText("mcp-local-hub");
    const links = page.locator(".sidebar nav a");
    // Phase 3B-II A3-a added Secrets between Add server and Dashboard.
    // Phase 3B-II A4-a added Settings as the 7th link.
    // Phase 3B-II A5 added About as the 8th link.
    // Phase 3B-II G3 added Capabilities as the 7th link.
    // G5 marketplace added Catalog as the 2nd link (after Servers);
    // every subsequent link shifts down by one (10 links total).
    // Authoritative order is app.tsx NAV_ITEMS.
    await expect(links).toHaveCount(10);
    await expect(links.nth(0)).toHaveText("Servers");
    await expect(links.nth(1)).toHaveText("Catalog");
    await expect(links.nth(2)).toHaveText("Discovery");
    await expect(links.nth(3)).toHaveText("Add server");
    await expect(links.nth(4)).toHaveText("Secrets");
    await expect(links.nth(5)).toHaveText("Dashboard");
    await expect(links.nth(6)).toHaveText("Logs");
    await expect(links.nth(7)).toHaveText("Capabilities");
    await expect(links.nth(8)).toHaveText("Settings");
    await expect(links.nth(9)).toHaveText("About");
  });

  test("default route is Dashboard and nav highlights on click", async ({ page, hub }) => {
    await page.goto(`${hub.url}/`);
    const dashboardLink = page.locator(".sidebar nav a", { hasText: "Dashboard" });
    await expect(dashboardLink).toHaveClass(/active/);
    await expect(page.locator("h1")).toHaveText("Dashboard");
    await page.locator(".sidebar nav a", { hasText: "Servers" }).click();
    await expect(page.locator(".sidebar nav a", { hasText: "Servers" })).toHaveClass(/active/);
    await expect(page.locator("h1")).toHaveText("Servers");
    await page.locator(".sidebar nav a", { hasText: "Discovery" }).click();
    await expect(page.locator(".sidebar nav a", { hasText: "Discovery" })).toHaveClass(/active/);
    await expect(page.locator("h1")).toHaveText("Discovery");
    await page.locator(".sidebar nav a", { hasText: "Add server" }).click();
    await expect(page.locator(".sidebar nav a", { hasText: "Add server" })).toHaveClass(/active/);
    await expect(page.locator("h1")).toHaveText("Add server");
    await page.locator(".sidebar nav a", { hasText: "Logs" }).click();
    await expect(page.locator(".sidebar nav a", { hasText: "Logs" })).toHaveClass(/active/);
    await expect(page.locator("h1")).toHaveText("Logs");
  });

  test("hashchange triggers screen swap (browser back/forward)", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/dashboard`);
    await expect(page.locator("h1")).toHaveText("Dashboard");
    await page.goto(`${hub.url}/#/add-server`);
    await expect(page.locator("h1")).toHaveText("Add server");
    await page.goto(`${hub.url}/#/logs`);
    await expect(page.locator("h1")).toHaveText("Logs");
    await page.goBack();
    await expect(page.locator("h1")).toHaveText("Add server");
  });
});
