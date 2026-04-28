import { test, expect } from "../fixtures/hub";

test.describe("about", () => {
  test("renders version metadata fetched from /api/version", async ({ page, hub }) => {
    const reqPromise = page.waitForRequest(
      (r) => r.url() === `${hub.url}/api/version`,
      { timeout: 5_000 },
    );
    await page.goto(`${hub.url}/#/about`);
    const req = await reqPromise;
    expect(req.method()).toBe("GET");

    // Heading + the data-testid hooks the React component exposes.
    await expect(page.locator("h1")).toHaveText("About mcp-local-hub");
    await expect(page.getByTestId("about-version")).toBeVisible();
    await expect(page.getByTestId("about-commit")).toBeVisible();
    await expect(page.getByTestId("about-build-date")).toBeVisible();

    // Default `go run` build leaves version="dev" / commit="unknown" /
    // build_date="unknown". Assert these are rendered (and not the
    // empty string a regression in the JSON wiring would produce).
    const version = await page.getByTestId("about-version").textContent();
    expect(version?.trim().length ?? 0).toBeGreaterThan(0);
  });

  test("includes external links with safe rel attributes", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/about`);
    // Wait for the loaded state so links exist.
    await expect(page.getByTestId("about-loaded")).toBeVisible();
    const links = page.locator(".about-links a");
    await expect(links).toHaveCount(2);
    for (let i = 0; i < (await links.count()); i++) {
      const link = links.nth(i);
      await expect(link).toHaveAttribute("target", "_blank");
      await expect(link).toHaveAttribute("rel", "noopener noreferrer");
    }
  });

  test("sidebar About link routes here and highlights active", async ({ page, hub }) => {
    await page.goto(`${hub.url}/`);
    const aboutLink = page.locator(".sidebar nav a", { hasText: "About" });
    await aboutLink.click();
    await expect(aboutLink).toHaveClass(/active/);
    await expect(page.locator("h1")).toHaveText("About mcp-local-hub");
  });
});
