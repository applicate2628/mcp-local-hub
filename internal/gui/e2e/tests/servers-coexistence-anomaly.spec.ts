import { test, expect } from "../fixtures/hub";
import {
  buildWorkspace,
  routeStandardLspMocks,
  seedCoexistence,
} from "../fixtures/lsp-helpers";

// servers-coexistence-anomaly.spec.ts — v0.5.x Task 4.4 / plan §4.4 #3.
// Asserts the LSP matrix renders dual badges in a cell where the same
// (language, client) pair has both a hub-routed http binding AND a
// legacy direct-stdio binding. This is the operator-visible signal
// that a migration left orphaned state behind.
test.describe("servers — coexistence anomaly", () => {
  test("LSP cell renders BOTH via-hub + legacy chips when client_presence and legacy_conflict both target the same client", async ({ page, hub }) => {
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
    // Same-client coexistence: hub URL + legacy stdio both on codex-cli.
    const scan = seedCoexistence({
      language: "rust",
      clientHub: "codex-cli",
      clientLegacy: "codex-cli",
    });
    await routeStandardLspMocks(page, { scan, status: [], workspaces });

    await page.goto(`${hub.url}/#/servers`);
    await page.waitForSelector('[data-testid="lsp-row-rust"]');

    const cell = page.locator('[data-testid="lsp-cell-rust-codex-cli"]');
    await expect(cell).toBeVisible();
    await expect(cell).toHaveAttribute("data-dual-badge", "true");
    // The primary [via-hub] chip + the [legacy] chip must BOTH render.
    await expect(
      cell.locator('[data-testid="lsp-chip-primary-rust-codex-cli"]'),
    ).toHaveText("via-hub");
    await expect(
      cell.locator('[data-testid="lsp-chip-legacy-rust-codex-cli"]'),
    ).toHaveText("legacy");
  });

  test("LSP cell with no conflict renders only the primary chip + no dual-badge attribute", async ({ page, hub }) => {
    const workspaces = {
      workspaces: [{ workspace_key: "default", workspace_path: "/proj" }],
      entries: [
        buildWorkspace({
          key: "default",
          path: "/proj",
          language: "clangd",
          port: 9200,
          clientEntries: { "codex-cli": "mcp-language-server-clangd" },
        }),
      ],
    };
    // No legacy_conflict — just a clean hub binding.
    const scan = {
      at: "2026-05-20T00:00:00Z",
      entries: [
        {
          name: "mcp-language-server-clangd",
          manifest_exists: false,
          can_migrate: false,
          status: "via-hub",
          client_presence: {
            "codex-cli": { transport: "http", endpoint: "http://127.0.0.1:9200/lsp/clangd" },
          },
        },
      ],
    };
    await routeStandardLspMocks(page, { scan, status: [], workspaces });

    await page.goto(`${hub.url}/#/servers`);
    await page.waitForSelector('[data-testid="lsp-row-clangd"]');

    const cell = page.locator('[data-testid="lsp-cell-clangd-codex-cli"]');
    await expect(cell).not.toHaveAttribute("data-dual-badge", "true");
    await expect(
      cell.locator('[data-testid="lsp-chip-primary-clangd-codex-cli"]'),
    ).toHaveText("via-hub");
    await expect(
      cell.locator('[data-testid="lsp-chip-legacy-clangd-codex-cli"]'),
    ).toHaveCount(0);
  });
});
