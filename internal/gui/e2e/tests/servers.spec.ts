import { test, expect } from "../fixtures/hub";

test.describe("servers", () => {
  test("matrix renders headers with correct column set", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/servers`);
    await expect(page.locator("h1")).toHaveText("Servers");
    const matrix = page.locator("table.servers-matrix");
    await expect(matrix).toBeVisible();
    const headerCells = matrix.locator("thead th");
    await expect(headerCells).toHaveCount(7);
    await expect(headerCells.nth(0)).toHaveText("Server");
    await expect(headerCells.nth(1)).toHaveText("claude-code");
    await expect(headerCells.nth(2)).toHaveText("codex-cli");
    await expect(headerCells.nth(3)).toHaveText("gemini-cli");
    await expect(headerCells.nth(4)).toHaveText("antigravity");
    await expect(headerCells.nth(5)).toHaveText("Port");
    await expect(headerCells.nth(6)).toHaveText("State");
  });

  test("empty body when tmpHome has no client configs", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/servers`);
    const matrix = page.locator("table.servers-matrix");
    await expect(matrix).toBeVisible();
    await expect(matrix.locator("tbody tr")).toHaveCount(0);
    await expect(page.getByText("Loading…")).toHaveCount(0);
  });

  test("Apply button is disabled with no dirty cells", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/servers`);
    const applyBtn = page.getByRole("button", { name: "Apply changes" });
    await expect(applyBtn).toBeVisible();
    await expect(applyBtn).toBeDisabled();
  });
});
