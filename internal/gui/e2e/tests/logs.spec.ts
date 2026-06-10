import { test, expect } from "../fixtures/hub";

test.describe("logs", () => {
  // Workstream B (§3.1 fail-loud): with no supervisor the GUI /api/status now
  // FAILS LOUD (500 STATUS_FAILED) instead of the old scheduler-scan 200 []. The
  // Logs screen is a /api/status consumer (Logs.tsx), so its no-daemons
  // assertions need an explicit empty-status mock to reproduce the
  // healthy-but-idle state these tests target.
  test.beforeEach(async ({ page }) => {
    await page.route("**/api/status", (r) =>
      r.fulfill({ status: 200, contentType: "application/json", body: "[]" }),
    );
  });

  test("renders heading + controls + notice when no daemons are running", async ({
    page,
    hub,
  }) => {
    await page.goto(`${hub.url}/#/logs`);
    await expect(page.locator("h1")).toHaveText("Logs");
    const controls = page.locator("#logs-controls");
    await expect(controls).toBeVisible();
    await expect(controls.locator("select")).toBeVisible();
    await expect(controls.locator("input[type=number]")).toBeVisible();
    await expect(controls.locator("input[type=checkbox]")).toBeVisible();
    await expect(controls.locator("button", { hasText: "Refresh" })).toBeVisible();
  });

  test("picker is empty + notice explains no daemons running", async ({
    page,
    hub,
  }) => {
    await page.goto(`${hub.url}/#/logs`);
    const body = page.locator("#logs-body");
    await expect(body).toBeVisible();
    await expect(body).toContainText(/no daemons running|no global-server logs/i, {
      timeout: 5_000,
    });
  });

  test("controls are disabled when no daemons are eligible", async ({
    page,
    hub,
  }) => {
    await page.goto(`${hub.url}/#/logs`);
    await expect(page.locator("#logs-body")).toContainText(
      /no daemons running|no global-server logs/i,
      { timeout: 5_000 },
    );
    const controls = page.locator("#logs-controls");
    await expect(controls.locator("select")).toBeDisabled();
    await expect(controls.locator("input[type=number]")).toBeDisabled();
    await expect(controls.locator("input[type=checkbox]")).toBeDisabled();
    await expect(controls.locator("button", { hasText: "Refresh" })).toBeDisabled();
  });
});
