import { test, expect } from "../fixtures/hub";
import { readFileSync, existsSync, writeFileSync } from "node:fs";
import { join } from "node:path";

test.describe("Discovery screen", () => {
  test("renders h1 + empty-state copy on fresh tmp home", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/migration`);
    await expect(page.locator("h1")).toHaveText("Discovery");
    await expect(page.locator(".empty-state")).toContainText("No MCP servers found");
  });

  test("Rescan button is present and clickable on empty home", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/migration`);
    // The inline rescan button was extracted into the shared
    // ScanRefreshControls component: stable testid `scan-rescan-btn`,
    // label "Rescan now" (internal/gui/frontend/src/components/ScanRefreshControls.tsx).
    const rescan = page.locator('[data-testid="scan-rescan-btn"]');
    await expect(rescan).toBeVisible();
    await expect(rescan).toHaveText("Rescan now");
    await rescan.click();
    await expect(page.locator(".empty-state")).toBeVisible();
  });

  test("group sections are not rendered when total row count is zero", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/migration`);
    await expect(page.locator('[data-group]')).toHaveCount(0);
  });

  test("hashchange from Servers to Discovery swaps h1", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/servers`);
    await expect(page.locator("h1")).toHaveText("Servers");
    await page.locator(".sidebar nav a", { hasText: "Discovery" }).click();
    await expect(page.locator("h1")).toHaveText("Discovery");
  });

  test("POST /api/dismiss → GET /api/dismissed → on-disk JSON all agree", async ({
    page,
    hub,
  }) => {
    // The hub fixture redirects the Windows state dir to
    // <home>/AppData/Local (via LOCALAPPDATA + MCPHUB_STATE_DIR_OVERRIDE,
    // honored by the test_state_path_env binary global-setup builds), so
    // api.dismiss.go's dismissedFilePath resolves to
    // <home>/AppData/Local/mcp-local-hub/gui-dismissed.json. Three
    // assertions together prove the full round-trip on a real spawned
    // binary:
    //   (a) POST /api/dismiss returns 204
    //   (b) The JSON file on disk includes the name with version=1
    //   (c) GET /api/dismissed returns the same name in its list
    const resp = await page.request.post(`${hub.url}/api/dismiss`, {
      data: { server: "synthetic-dismissed-e2e" },
      headers: { "Content-Type": "application/json" },
    });
    expect(resp.status()).toBe(204);

    const dismissedPath = join(
      hub.home,
      "AppData",
      "Local",
      "mcp-local-hub",
      "gui-dismissed.json",
    );
    expect(existsSync(dismissedPath)).toBe(true);
    const raw = readFileSync(dismissedPath, "utf-8");
    const parsed = JSON.parse(raw) as { version: number; unknown: string[] };
    expect(parsed.version).toBe(1);
    expect(parsed.unknown).toContain("synthetic-dismissed-e2e");

    // GET /api/dismissed should return what we just wrote. This is
    // the endpoint the Discovery screen consumes in Task 5.
    const list = await page.request.get(`${hub.url}/api/dismissed`);
    expect(list.status()).toBe(200);
    const listBody = (await list.json()) as { unknown: string[] };
    expect(Array.isArray(listBody.unknown)).toBe(true);
    expect(listBody.unknown).toContain("synthetic-dismissed-e2e");
  });

  test("/api/scan remains unfiltered by dismissals (Servers-matrix invariant)", async ({
    page,
    hub,
  }) => {
    // Regression guard: Servers matrix (via collectServers) consumes
    // every /api/scan entry without status inspection, so dismissing
    // an unknown entry MUST NOT hide it from /api/scan. We prove this
    // by seeding a real unknown stdio entry in ~/.claude.json that
    // the hub's scanner will classify as unknown (no matching
    // manifest), POSTing /api/dismiss for its exact name, and then
    // asserting /api/scan still includes that name. Without this
    // assertion a future regression (someone re-adding a server-side
    // filter to /api/scan) would silently pass every other test.
    const claudePath = join(hub.home, ".claude.json");
    writeFileSync(
      claudePath,
      JSON.stringify({
        mcpServers: {
          "e2e-unknown-guard": {
            type: "stdio",
            command: "npx",
            args: ["-y", "e2e-unknown-guard"],
          },
        },
      }),
      "utf-8",
    );

    // Pre-check: /api/scan should now show the seeded unknown entry.
    const preScan = await page.request.get(`${hub.url}/api/scan`);
    expect(preScan.status()).toBe(200);
    const preBody = (await preScan.json()) as {
      entries: Array<{ name: string; status?: string }> | null;
    };
    const preNames = (preBody.entries ?? []).map((e) => e.name);
    expect(preNames).toContain("e2e-unknown-guard");

    // Dismiss that name.
    const dismiss = await page.request.post(`${hub.url}/api/dismiss`, {
      data: { server: "e2e-unknown-guard" },
      headers: { "Content-Type": "application/json" },
    });
    expect(dismiss.status()).toBe(204);

    // /api/scan must STILL contain the name. Filtering moved
    // client-side in R2; /api/scan is shared with Servers and must
    // stay unfiltered.
    const postScan = await page.request.get(`${hub.url}/api/scan`);
    expect(postScan.status()).toBe(200);
    const postBody = (await postScan.json()) as {
      entries: Array<{ name: string; status?: string }> | null;
    };
    const postNames = (postBody.entries ?? []).map((e) => e.name);
    expect(postNames).toContain("e2e-unknown-guard");
  });
});
