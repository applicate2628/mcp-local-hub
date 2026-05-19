import { test, expect } from "../fixtures/hub";
import {
  buildWorkspace,
  emptyScanResult,
  routeStandardLspMocks,
} from "../fixtures/lsp-helpers";

// servers-env-overlay.spec.ts — v0.5.x Task 4.4 / plan §4.4 #2.
// Asserts the per-row env drawer's ${parent_path} warning chip
// surfaces when the operator-edited PATH does NOT include the literal
// ${parent_path} token, AND disappears as soon as the token is added.
test.describe("servers — env overlay drawer", () => {
  test("warning chip appears when PATH omits ${parent_path}, hides when token typed", async ({ page, hub }) => {
    const workspaces = {
      workspaces: [{ workspace_key: "default", workspace_path: "/proj" }],
      entries: [
        buildWorkspace({
          key: "default",
          path: "/proj",
          language: "clangd",
          port: 9200,
        }),
      ],
    };
    await routeStandardLspMocks(page, {
      scan: emptyScanResult,
      status: [],
      workspaces,
    });
    await page.goto(`${hub.url}/#/servers`);
    await page.waitForSelector('[data-testid="lsp-row-clangd"]');

    // Open the drawer for the registered language.
    await page.locator('[data-testid="lsp-edit-env-clangd"]').click();
    const drawer = page.locator('[data-testid="env-drawer"]');
    await expect(drawer).toBeVisible();
    // Drawer header carries the taskName so operators can sanity-
    // check which daemon they're editing across overlapping rows.
    await expect(drawer.locator('[data-testid="env-drawer-task"]')).toContainText(
      "\\mcp-local-hub-lsp-default-clangd",
    );

    // Initial PATH is empty → warning chip is hidden (no value to
    // worry about until the operator types something).
    await expect(
      page.locator('[data-testid="env-drawer-parent-path-warning"]'),
    ).toHaveCount(0);

    // Type a PATH WITHOUT ${parent_path} → warning chip appears.
    const ta = page.locator('[data-testid="env-drawer-path"]');
    await ta.fill("/usr/local/bin");
    await expect(
      page.locator('[data-testid="env-drawer-parent-path-warning"]'),
    ).toBeVisible();

    // Append ${parent_path} → warning chip hides.
    await ta.fill("/usr/local/bin;${parent_path}");
    await expect(
      page.locator('[data-testid="env-drawer-parent-path-warning"]'),
    ).toHaveCount(0);
  });

  test("Apply posts /api/daemon/env with the typed PATH and surfaces the changed-keys count", async ({ page, hub }) => {
    const workspaces = {
      workspaces: [{ workspace_key: "default", workspace_path: "/proj" }],
      entries: [
        buildWorkspace({
          key: "default",
          path: "/proj",
          language: "rust",
          port: 9201,
        }),
      ],
    };
    await routeStandardLspMocks(page, {
      scan: emptyScanResult,
      status: [],
      workspaces,
    });

    let envBody: string | null = null;
    await page.route("**/api/daemon/env", async (r) => {
      envBody = r.request().postData();
      await r.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          task_name: "\\mcp-local-hub-lsp-default-rust",
          changed_keys: ["Path"],
        }),
      });
    });

    await page.goto(`${hub.url}/#/servers`);
    await page.locator('[data-testid="lsp-edit-env-rust"]').click();
    const ta = page.locator('[data-testid="env-drawer-path"]');
    await ta.fill("/opt/rust/bin;${parent_path}");
    await page.locator('[data-testid="env-drawer-apply"]').click();

    await expect.poll(() => envBody).not.toBeNull();
    expect(JSON.parse(envBody!)).toEqual({
      task_name: "\\mcp-local-hub-lsp-default-rust",
      env: { Path: "/opt/rust/bin;${parent_path}" },
    });
    await expect(
      page.locator('[data-testid="env-drawer-apply-msg"]'),
    ).toContainText("Applied 1 key(s)");
  });

  test("Restart with 409 QUARANTINED surfaces the force-retry hint", async ({ page, hub }) => {
    const workspaces = {
      workspaces: [{ workspace_key: "default", workspace_path: "/proj" }],
      entries: [
        buildWorkspace({
          key: "default",
          path: "/proj",
          language: "go",
          port: 9203,
        }),
      ],
    };
    await routeStandardLspMocks(page, {
      scan: emptyScanResult,
      status: [],
      workspaces,
    });
    await page.route("**/api/daemon/respawn", async (r) => {
      await r.fulfill({
        status: 409,
        contentType: "application/json",
        body: JSON.stringify({
          error: "daemon is quarantined",
          code: "QUARANTINED",
        }),
      });
    });

    await page.goto(`${hub.url}/#/servers`);
    await page.locator('[data-testid="lsp-edit-env-go"]').click();
    await page.locator('[data-testid="env-drawer-restart"]').click();
    const msg = page.locator('[data-testid="env-drawer-restart-msg"]');
    await expect(msg).toBeVisible();
    await expect(msg).toContainText("quarantined");
    // The drawer's UX promise: the error message names the force
    // affordance so operators don't have to read the docs to recover.
    await expect(msg).toContainText("force");
  });
});
