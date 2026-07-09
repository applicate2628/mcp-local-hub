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
      // No workspace registration → row carries data-registered=false and
      // renders the in-GUI "Enable" affordance (Servers.tsx:1503-1516)
      // instead of an Edit-env button. (The old static "run `mcphub
      // register`" placeholder copy was replaced by the Enable button;
      // with no workspace selected the button renders disabled.)
      await expect(row).toHaveAttribute("data-registered", "false");
      const enableBtn = row.locator(`[data-testid="lsp-enable-${lang}"]`);
      await expect(enableBtn).toBeVisible();
      await expect(enableBtn).toBeDisabled();
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

  test("per-client LSP toggle disables router entries through the GUI API", async ({ page, hub }) => {
    const workspaces = {
      workspaces: [{ workspace_key: "default", workspace_path: "/proj" }],
      entries: [
        buildWorkspace({
          key: "default",
          path: "/proj",
          language: "python",
          port: 9201,
          clientEntries: { "codex-cli": "mcp-language-server-python" },
        }),
      ],
    };
    const disableBodies: unknown[] = [];
    await routeStandardLspMocks(page, {
      scan: {
        at: "2026-05-20T00:00:00Z",
        entries: [
          {
            name: "mcp-language-server-python",
            manifest_exists: false,
            can_migrate: false,
            status: "via-hub",
            client_presence: {
              "codex-cli": {
                transport: "http",
                endpoint: "http://127.0.0.1:9125/lsp/python/mcp",
              },
            },
          },
        ],
        client_config_presence: { "codex-cli": "ok" },
      },
      status: [],
      workspaces,
    });
    await page.route("**/api/lsp-router/disable", async (r) => {
      disableBodies.push(JSON.parse(r.request().postData() ?? "{}"));
      await r.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          client: "codex-cli",
          enabled: false,
          report: {
            removed: [
              { client: "codex-cli", language: "python", entry_name: "mcp-language-server-python" },
            ],
          },
        }),
      });
    });

    await page.goto(`${hub.url}/#/servers`);
    const toggle = page.locator('[data-testid="lsp-toggle-python-codex-cli"]');
    await expect(toggle).toBeChecked();
    await toggle.uncheck();

    await expect.poll(() => disableBodies.length).toBe(1);
    expect(disableBodies[0]).toEqual({ client: "codex-cli" });
  });

  test("per-client LSP toggle enables router entries through the GUI API", async ({ page, hub }) => {
    const workspaces = {
      workspaces: [{ workspace_key: "default", workspace_path: "/proj" }],
      entries: [
        buildWorkspace({
          key: "default",
          path: "/proj",
          language: "python",
          port: 9201,
          clientEntries: {},
        }),
      ],
    };
    const enableBodies: unknown[] = [];
    await routeStandardLspMocks(page, {
      scan: {
        at: "2026-05-20T00:00:00Z",
        entries: [],
        client_config_presence: { "codex-cli": "ok" },
      },
      status: [],
      workspaces,
    });
    await page.route("**/api/lsp-router/enable", async (r) => {
      enableBodies.push(JSON.parse(r.request().postData() ?? "{}"));
      await r.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          client: "codex-cli",
          enabled: true,
          report: {
            applied: [
              { client: "codex-cli", language: "python", entry_name: "mcp-language-server-python" },
            ],
          },
        }),
      });
    });

    await page.goto(`${hub.url}/#/servers`);
    const toggle = page.locator('[data-testid="lsp-toggle-python-codex-cli"]');
    await expect(toggle).not.toBeChecked();
    await toggle.check();

    await expect.poll(() => enableBodies.length).toBe(1);
    expect(enableBodies[0]).toEqual({ client: "codex-cli" });
  });

  test("workspace selector empty-state placeholder when no registry", async ({ page, hub }) => {
    await routeStandardLspMocks(page);
    await page.goto(`${hub.url}/#/servers`);
    await page.waitForSelector('[data-testid="workspace-selector"]');
    // Empty registry → no <select>, just the in-GUI register hint (NOT a CLI
    // instruction — the fresh-machine connect path must not dead-end here).
    await expect(page.locator('[data-testid="workspace-selector-select"]')).toHaveCount(0);
    const text = (await page.locator('[data-testid="workspace-selector"]').textContent()) ?? "";
    expect(text).toContain("register a workspace folder");
    expect(text).not.toContain("mcphub register");
  });

  test("fresh-machine: register the first workspace from the GUI (no CLI)", async ({ page, hub }) => {
    // Start with an empty registry, then mock /api/lsp/register so a fresh
    // operator can register a workspace path + language straight from the GUI.
    // No live LSP is exercised — the route is fulfilled synthetically.
    const registerBodies: unknown[] = [];
    await routeStandardLspMocks(page, {
      scan: emptyScanResult,
      status: [],
      workspaces: emptyWorkspaces,
    });
    await page.route("**/api/lsp/register", async (r) => {
      registerBodies.push(JSON.parse(r.request().postData() ?? "{}"));
      await r.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          workspace: "/proj",
          workspace_key: "proj",
          entries: [
            buildWorkspace({ key: "proj", path: "/proj", language: "python", port: 9202 }),
          ],
          results: [{ language: "python", status: "ok" }],
        }),
      });
    });

    await page.goto(`${hub.url}/#/servers`);
    await page.waitForSelector('[data-testid="lsp-register-workspace"]');

    // Empty-state copy nudges the operator to register here, not via the CLI.
    await expect(
      page.locator('[data-testid="lsp-register-workspace-intro"]'),
    ).toContainText("No workspace registered yet");

    const submit = page.locator('[data-testid="lsp-register-workspace-submit"]');
    await expect(submit).toBeDisabled();

    await page.locator('[data-testid="lsp-register-workspace-path"]').fill("/proj");
    await page
      .locator('[data-testid="lsp-register-workspace-language"]')
      .selectOption("python");
    await expect(submit).toBeEnabled();
    await submit.click();

    await expect.poll(() => registerBodies.length).toBe(1);
    expect(registerBodies[0]).toEqual({ workspace_path: "/proj", language: "python" });
  });
});
