import { test, expect } from "../fixtures/hub";
import {
  buildWorkspace,
  emptyScanResult,
  emptyWorkspaces,
  routeStandardLspMocks,
} from "../fixtures/lsp-helpers";

// servers-lsp.spec.ts — v0.5.x Task 4.4 / plan §4.4 acceptance #1.
// Asserts the 9 LSP rows are ALWAYS visible regardless of registry
// state, and the per-row affordance flips from placeholder copy to
// the "Edit env" button only when a workspace entry registers the
// language.
test.describe("servers — LSP matrix", () => {
  test("9 LSP rows always render with empty registry + scan", async ({ page, hub }) => {
    await routeStandardLspMocks(page, {
      scan: emptyScanResult,
      status: [],
      workspaces: emptyWorkspaces,
    });
    await page.goto(`${hub.url}/#/servers`);
    await page.waitForSelector('[data-testid="lsp-matrix"]');
    // One stable test id per language. Order matches LSP_LANGUAGES in
    // internal/gui/frontend/src/lib/lsp-rows.ts — keep the two in sync
    // when adding a new language to the manifest.
    const expected = [
      "clangd",
      "fortran",
      "go",
      "javascript",
      "python",
      "rust",
      "typescript",
      "vscode-css",
      "vscode-html",
    ];
    const rows = page.locator('[data-testid^="lsp-row-"]');
    await expect(rows).toHaveCount(9);
    for (const lang of expected) {
      const row = page.locator(`[data-testid="lsp-row-${lang}"]`);
      await expect(row).toBeVisible();
      // No workspace registration → row carries data-registered=false
      // and renders the "(run `mcphub register` to register)" copy
      // instead of an Edit-env button.
      await expect(row).toHaveAttribute("data-registered", "false");
      await expect(row.locator('.lsp-row-unregistered')).toBeVisible();
      await expect(row.locator(`[data-testid="lsp-edit-env-${lang}"]`)).toHaveCount(0);
    }
  });

  test("registered language flips to Edit env affordance + workspace selector lists the workspace", async ({ page, hub }) => {
    const workspaces = {
      workspaces: [{ workspace_key: "default", workspace_path: "/proj" }],
      entries: [
        buildWorkspace({
          key: "default",
          path: "/proj",
          language: "rust",
          port: 9201,
          clientEntries: { "codex-cli": "mcp-language-server-rust" },
        }),
      ],
    };
    await routeStandardLspMocks(page, {
      scan: emptyScanResult,
      status: [],
      workspaces,
    });
    await page.goto(`${hub.url}/#/servers`);
    await page.waitForSelector('[data-testid="lsp-matrix"]');

    const rustRow = page.locator('[data-testid="lsp-row-rust"]');
    await expect(rustRow).toHaveAttribute("data-registered", "true");
    await expect(rustRow).toHaveAttribute("data-workspace", "default");
    await expect(rustRow.locator('[data-testid="lsp-edit-env-rust"]')).toBeVisible();

    // Other languages stay placeholders.
    await expect(
      page.locator('[data-testid="lsp-row-clangd"]'),
    ).toHaveAttribute("data-registered", "false");

    // Workspace selector surfaces the registered workspace; "(all
    // workspaces)" sentinel + 1 real option.
    const select = page.locator('[data-testid="workspace-selector-select"]');
    await expect(select).toBeVisible();
    await expect(select.locator("option")).toHaveCount(2);
    await expect(select.locator("option").nth(0)).toHaveText("(all workspaces)");
    await expect(select.locator("option").nth(1)).toHaveText("default");
  });

  test("workspace selector empty-state placeholder when no registry", async ({ page, hub }) => {
    await routeStandardLspMocks(page);
    await page.goto(`${hub.url}/#/servers`);
    await page.waitForSelector('[data-testid="workspace-selector"]');
    // Empty registry → no <select>, just the "register first" hint.
    await expect(page.locator('[data-testid="workspace-selector-select"]')).toHaveCount(0);
    const text = (await page.locator('[data-testid="workspace-selector"]').textContent()) ?? "";
    expect(text).toContain("register a workspace first");
  });
});
