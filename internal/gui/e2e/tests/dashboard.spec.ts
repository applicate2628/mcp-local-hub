import { test, expect } from "../fixtures/hub";

test.describe("dashboard", () => {
  test("renders Dashboard heading and empty cards container on fresh home", async ({
    page,
    hub,
  }) => {
    await page.goto(`${hub.url}/#/dashboard`);
    await expect(page.locator("h1")).toHaveText("Dashboard");
    const cards = page.locator(".cards");
    // The .cards grid collapses to zero height when empty (auto-fit with no
    // items), so toBeVisible() would fail. toBeAttached() confirms it is in
    // the DOM without requiring a non-zero bounding box.
    await expect(cards).toBeAttached();
    await expect(cards.locator(".card")).toHaveCount(0);
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
