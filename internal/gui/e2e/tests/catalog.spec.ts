import { test, expect } from "../fixtures/hub";
import type { Page } from "@playwright/test";

// Store one-click install (roadmap §B #1, frontend slice S3). These specs
// drive the Catalog → Marketplace section against a live mcphub gui, with
// /api/catalog, /api/status, /api/marketplace, and /api/marketplace/install
// stubbed via page.route so the install lifecycle is deterministic (the real
// /api/marketplace does a network fetch to the curated GitHub registry, which
// is non-deterministic in a sandbox; the install path touches the live fleet).
//
// Route-precedence note: Playwright matches route handlers in REVERSE
// registration order, and the broad `**/api/marketplace` glob also matches
// `/api/marketplace/install`. Register the broad list route FIRST and the
// specific install route LAST so the install POST is matched by its own
// handler, not swallowed by the list stub.

const json = (status: number, body: unknown) => ({
  status,
  contentType: "application/json",
  body: JSON.stringify(body),
});

// stubBase wires the always-needed routes (catalog/status empty, marketplace
// list) so the screen renders the Marketplace section. `entries` is the
// /api/marketplace body; install is wired per-test on top.
async function stubBase(page: Page, entries: unknown[]) {
  await page.route("**/api/catalog", (r) => r.fulfill(json(200, { catalog: [] })));
  await page.route("**/api/status", (r) => r.fulfill(json(200, [])));
  await page.route("**/api/marketplace", (r) => r.fulfill(json(200, { entries })));
}

const stdioEntry = {
  id: "git",
  name: "Git",
  summary: "Git repository tooling.",
  categories: ["dev"],
  homepage: "https://example.com/git",
  transport: "stdio",
};

const httpEntry = {
  id: "remote",
  name: "Remote",
  summary: "A remote http MCP server.",
  categories: [],
  homepage: "",
  transport: "http",
};

test.describe("catalog store install", () => {
  test("stdio entry shows HUB-ONLY (no Install-directly toggle)", async ({ page, hub }) => {
    await stubBase(page, [stdioEntry]);
    await page.goto(`${hub.url}/#/catalog`);

    const hub_ = page.getByTestId("catalog-marketplace-hub-git");
    await expect(hub_).toBeVisible();
    await expect(hub_).toHaveText(/Add to hub/);
    // Two-tier rule: stdio installs only via the shared hub daemon.
    await expect(page.getByTestId("catalog-marketplace-direct-toggle-git")).toHaveCount(0);
  });

  test("http entry shows BOTH hub + direct modes", async ({ page, hub }) => {
    await stubBase(page, [httpEntry]);
    await page.goto(`${hub.url}/#/catalog`);

    await expect(page.getByTestId("catalog-marketplace-hub-remote")).toBeVisible();
    await expect(page.getByTestId("catalog-marketplace-direct-toggle-remote")).toBeVisible();
    // The client multiselect is collapsed until the toggle is clicked.
    await expect(page.getByTestId("catalog-marketplace-direct-panel-remote")).toHaveCount(0);
  });

  test("hub install POSTs {mode:'hub'} and reflects the 201 success", async ({ page, hub }) => {
    await stubBase(page, [stdioEntry]);
    const bodies: any[] = [];
    await page.route("**/api/marketplace/install", async (r) => {
      bodies.push(JSON.parse(r.request().postData() ?? "{}"));
      await r.fulfill(json(201, { name: "git", port: 9201, mode: "hub" }));
    });
    await page.goto(`${hub.url}/#/catalog`);

    await page.getByTestId("catalog-marketplace-hub-git").click();
    const ok = page.getByTestId("catalog-marketplace-installed-git");
    await expect(ok).toBeVisible();
    await expect(ok).toContainText("git");
    await expect(ok).toContainText("9201");
    expect(bodies[0]).toMatchObject({ id: "git", mode: "hub" });
  });

  test("hub install 409 NAME_CONFLICT offers a one-click suggested-name retry", async ({ page, hub }) => {
    await stubBase(page, [stdioEntry]);
    const bodies: any[] = [];
    let call = 0;
    await page.route("**/api/marketplace/install", async (r) => {
      bodies.push(JSON.parse(r.request().postData() ?? "{}"));
      call += 1;
      if (call === 1) {
        await r.fulfill(json(409, { error_code: "NAME_CONFLICT", suggested_name: "git-2" }));
      } else {
        await r.fulfill(json(201, { name: "git-2", port: 9202, mode: "hub" }));
      }
    });
    await page.goto(`${hub.url}/#/catalog`);

    await page.getByTestId("catalog-marketplace-hub-git").click();
    const retry = page.getByTestId("catalog-marketplace-conflict-retry-git");
    await expect(retry).toBeVisible();
    await expect(retry).toContainText("git-2");
    await retry.click();

    await expect(page.getByTestId("catalog-marketplace-installed-git")).toContainText("git-2");
    // Second POST carried the suggested name.
    expect(bodies[1]).toMatchObject({ id: "git", mode: "hub", name: "git-2" });
  });

  test("direct mode: client multiselect + POST {mode:'direct', clients:[…]} shape", async ({ page, hub }) => {
    await stubBase(page, [httpEntry]);
    const bodies: any[] = [];
    await page.route("**/api/marketplace/install", async (r) => {
      bodies.push(JSON.parse(r.request().postData() ?? "{}"));
      await r.fulfill(
        json(200, { clients_updated: ["claude-code", "cursor"], clients_failed: [], mode: "direct" }),
      );
    });
    await page.goto(`${hub.url}/#/catalog`);

    // Open the direct-mode panel.
    await page.getByTestId("catalog-marketplace-direct-toggle-remote").click();
    await expect(page.getByTestId("catalog-marketplace-direct-panel-remote")).toBeVisible();

    // Install disabled until a client is picked.
    const installBtn = page.getByTestId("catalog-marketplace-direct-install-remote");
    await expect(installBtn).toBeDisabled();

    await page.getByTestId("catalog-marketplace-client-remote-claude-code").check();
    await page.getByTestId("catalog-marketplace-client-remote-cursor").check();
    await expect(installBtn).toBeEnabled();
    await installBtn.click();

    const result = page.getByTestId("catalog-marketplace-direct-updated-remote");
    await expect(result).toContainText("claude-code");
    expect(bodies[0]).toMatchObject({ id: "remote", mode: "direct" });
    expect(bodies[0].clients).toEqual(expect.arrayContaining(["claude-code", "cursor"]));
  });

  test("direct mode 207 partial renders both updated + failed clients", async ({ page, hub }) => {
    await stubBase(page, [httpEntry]);
    await page.route("**/api/marketplace/install", (r) =>
      r.fulfill(
        json(207, {
          clients_updated: ["claude-code"],
          clients_failed: [{ client: "vscode", error: "config file is a symlink" }],
          mode: "direct",
        }),
      ),
    );
    await page.goto(`${hub.url}/#/catalog`);

    await page.getByTestId("catalog-marketplace-direct-toggle-remote").click();
    await page.getByTestId("catalog-marketplace-client-remote-claude-code").check();
    await page.getByTestId("catalog-marketplace-direct-install-remote").click();

    await expect(page.getByTestId("catalog-marketplace-direct-updated-remote")).toContainText("claude-code");
    const failed = page.getByTestId("catalog-marketplace-direct-failed-remote");
    await expect(failed).toContainText("vscode");
    await expect(failed).toContainText("config file is a symlink");
  });

  test("install error surfaces inline and leaves the hub button re-enabled", async ({ page, hub }) => {
    await stubBase(page, [stdioEntry]);
    await page.route("**/api/marketplace/install", (r) =>
      r.fulfill(json(502, { error: "marketplace catalog unavailable", code: "CATALOG_UNAVAILABLE" })),
    );
    await page.goto(`${hub.url}/#/catalog`);

    await page.getByTestId("catalog-marketplace-hub-git").click();
    await expect(page.getByTestId("catalog-marketplace-error-git")).toContainText(
      "marketplace catalog unavailable",
    );
    // The row survives: hub button present + re-enabled for a retry.
    await expect(page.getByTestId("catalog-marketplace-hub-git")).toBeEnabled();
  });
});
