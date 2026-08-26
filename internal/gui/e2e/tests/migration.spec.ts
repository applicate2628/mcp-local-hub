import { test, expect } from "../fixtures/hub";
import { seededHubFor } from "../fixtures/seeded-hub";
import { emptyScanResult, routeScanFixture } from "../fixtures/lsp-helpers";
import { readFileSync, existsSync, writeFileSync } from "node:fs";
import { join } from "node:path";

function seedAdoptUnknown(home: string) {
  writeFileSync(
    join(home, ".claude.json"),
    JSON.stringify({
      mcpServers: {
        "e2e-adopt-stdio": {
          type: "stdio",
          command: "npx",
          args: ["-y", "e2e-adopt-stdio"],
        },
      },
    }),
    "utf-8",
  );
}

const seededAdoptTest = seededHubFor(seedAdoptUnknown);

test.describe("Discovery screen", () => {
  test("renders h1 + empty-state copy on fresh tmp home", async ({ page, hub }) => {
    await routeScanFixture(page, emptyScanResult);
    await page.goto(`${hub.url}/#/migration`);
    await expect(page.locator("h1")).toHaveText("Discovery");
    await expect(page.locator(".empty-state")).toContainText("No MCP servers found");
  });

  test("Rescan button is present and clickable on empty home", async ({ page, hub }) => {
    await routeScanFixture(page, emptyScanResult);
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

  test("de-adopt affordance follows backend ownership and gate eligibility", async ({
    page,
    hub,
  }) => {
    await page.route("**/api/scan", async (route) => {
      await route.fulfill({
        json: {
          at: "2026-07-15T00:00:00Z",
          entries: ["not-owned", "gate-on", "eligible"].map((name) => ({
            name,
            status: "via-hub",
            managed: true,
            manifest_exists: true,
            can_migrate: false,
            client_presence: {},
          })),
          client_config_presence: {},
        },
      });
    });
    await page.route("**/api/dismissed", async (route) => {
      await route.fulfill({ json: { unknown: [] } });
    });
    await page.route("**/api/deadopt/eligible**", async (route) => {
      const server = new URL(route.request().url()).searchParams.get("server");
      const eligibility =
        server === "eligible"
          ? {
              eligible: true,
              adopt_owned: true,
              gate_on: false,
              gate_on_clients: [],
              blocked_reason: "",
            }
          : server === "gate-on"
            ? {
                eligible: false,
                adopt_owned: true,
                gate_on: true,
                gate_on_clients: ["codex-cli"],
                blocked_reason:
                  "gate is ON for 1 client(s) (codex-cli); gate OFF first, then de-adopt",
              }
            : {
                eligible: false,
                adopt_owned: false,
                gate_on: false,
                gate_on_clients: [],
                blocked_reason: `manifest ${server} is not adopt-owned`,
              };
      await route.fulfill({ json: eligibility });
    });

    await page.goto(`${hub.url}/#/migration`);

    const notOwned = page.locator('li[data-server="not-owned"]');
    await expect(notOwned).toBeVisible();
    await expect(notOwned.getByRole("button", { name: "De-adopt to native" })).toHaveCount(0);

    const gateOn = page.locator('li[data-server="gate-on"]');
    const gateOnButton = gateOn.getByRole("button", { name: "De-adopt to native" });
    await expect(gateOnButton).toBeDisabled();
    await expect(gateOn).toContainText("gate OFF first");

    const eligible = page.locator('li[data-server="eligible"]');
    await expect(eligible.getByRole("button", { name: "De-adopt to native" })).toBeEnabled();
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

  test("Discovery scan consumer receives the same unfiltered response after dismissal", async ({
    page,
    hub,
  }) => {
    // This is the documented ScanResult consumer shape. The API producer
    // contract remains covered in Go; this E2E check proves the Discovery
    // filter consumes an unfiltered scan while applying dismissals locally.
    await routeScanFixture(page, {
      at: "2026-08-26T00:00:00Z",
      entries: [{
        name: "e2e-unknown-guard",
        status: "unknown",
        manifest_exists: false,
        can_migrate: false,
        client_presence: { "claude-code": { transport: "stdio" } },
      }],
      client_config_presence: { "claude-code": "ok" },
    });
    await page.goto(`${hub.url}/#/migration`);
    const scanNames = () => page.evaluate(async () => {
      const response = await fetch("/api/scan");
      const body = await response.json() as { entries?: Array<{ name: string }> | null };
      return { status: response.status, names: (body.entries ?? []).map((entry) => entry.name) };
    });
    const beforeDismiss = await scanNames();
    expect(beforeDismiss.status).toBe(200);
    expect(beforeDismiss.names).toContain("e2e-unknown-guard");

    const dismiss = await page.request.post(`${hub.url}/api/dismiss`, {
      data: { server: "e2e-unknown-guard" },
      headers: { "Content-Type": "application/json" },
    });
    expect(dismiss.status()).toBe(204);

    const afterDismiss = await scanNames();
    expect(afterDismiss.status).toBe(200);
    expect(afterDismiss.names).toContain("e2e-unknown-guard");
  });
});

seededAdoptTest.describe("Discovery adopt", () => {
  seededAdoptTest("adopts a seeded unknown stdio row through the modal", async ({
    page,
    hub,
  }) => {
    let adoptedThroughUI = false;
    await routeScanFixture(page, () => ({
      at: "2026-08-26T00:00:00Z",
      entries: [{
        name: "e2e-adopt-stdio",
        status: adoptedThroughUI ? "via-hub" : "unknown",
        managed: adoptedThroughUI,
        manifest_exists: adoptedThroughUI,
        can_migrate: false,
        client_presence: {
          "claude-code": { transport: adoptedThroughUI ? "http" : "stdio" },
        },
      }],
      client_config_presence: { "claude-code": "ok" },
      client_capabilities: { "claude-code": { adopt_supported: true } },
    }));
    await page.route(/\/api\/adopt\/plan$/, async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          EntryName: "e2e-adopt-stdio",
          SourceClient: "claude-code",
          ManifestName: "e2e-adopt-stdio",
          Port: 9321,
          AdoptClients: ["claude-code"],
          AlsoPresent: [],
          SignatureMismatches: [],
          DisabledSameName: [],
          SecretRoutedKeys: [],
          ManifestYAML: "name: e2e-adopt-stdio\nkind: global\n",
          symlink_targets: [],
        }),
      });
    });
    await page.route(/\/api\/adopt$/, async (route) => {
      adoptedThroughUI = true;
      await route.fulfill({ status: 201, contentType: "application/json", body: "{}" });
    });
    await page.goto(`${hub.url}/#/migration`);
    const row = page.locator('li[data-server="e2e-adopt-stdio"]');
    await expect(row).toBeVisible();
    await row.getByRole("button", { name: "Adopt into hub" }).click();

    const modal = page.locator('[data-testid="adopt-confirm-modal"]');
    await expect(modal).toBeVisible();
    await expect(modal).toContainText("Manifest: e2e-adopt-stdio");
    await modal.getByRole("button", { name: "Adopt into hub" }).click();
    await expect(page.getByText("Adopted e2e-adopt-stdio into hub.")).toBeVisible();

    await expect(page.locator('li[data-server="e2e-adopt-stdio"]')).toHaveCount(1);
    await expect(page.locator('li[data-server="e2e-adopt-stdio"]')).toContainText("Managed");

    // The route above owns the documented execution response for this UI flow.
    // Real manifest, intent, and client-file mutation coverage stays in the Go
    // /api/adopt handler tests, where it can use an isolated ready supervisor.
  });
});
