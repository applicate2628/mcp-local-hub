import { test, expect } from "../fixtures/hub";

test.describe("shell", () => {
  test("renders sidebar with brand + six nav links", async ({ page, hub }) => {
    await page.goto(`${hub.url}/`);
    await expect(page.locator(".sidebar .brand")).toHaveText("mcp-local-hub");
    const links = page.locator(".sidebar nav a");
    // Phase 3B-II A3-a added Secrets between Add server and Dashboard.
    await expect(links).toHaveCount(6);
    await expect(links.nth(0)).toHaveText("Servers");
    await expect(links.nth(1)).toHaveText("Migration");
    await expect(links.nth(2)).toHaveText("Add server");
    await expect(links.nth(3)).toHaveText("Secrets");
    await expect(links.nth(4)).toHaveText("Dashboard");
    await expect(links.nth(5)).toHaveText("Logs");
  });

  test("default route is Servers and nav highlights on click", async ({ page, hub }) => {
    await page.goto(`${hub.url}/`);
    const serversLink = page.locator(".sidebar nav a", { hasText: "Servers" });
    await expect(serversLink).toHaveClass(/active/);
    await page.locator(".sidebar nav a", { hasText: "Migration" }).click();
    await expect(page.locator(".sidebar nav a", { hasText: "Migration" })).toHaveClass(/active/);
    await expect(page.locator("h1")).toHaveText("Migration");
    await page.locator(".sidebar nav a", { hasText: "Add server" }).click();
    await expect(page.locator(".sidebar nav a", { hasText: "Add server" })).toHaveClass(/active/);
    await expect(page.locator("h1")).toHaveText("Add server");
    await page.locator(".sidebar nav a", { hasText: "Dashboard" }).click();
    await expect(page.locator(".sidebar nav a", { hasText: "Dashboard" })).toHaveClass(/active/);
    await expect(page.locator("h1")).toHaveText("Dashboard");
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
