// internal/gui/e2e/tests/projects-readd-d2.spec.ts
//
// D2 v1 — secret-safe pre-filled cold object-member re-enable. These specs
// drive the Re-add CTA → Add-server pre-fill flow against a LIVE mcphub gui.
//
// THE LOAD-BEARING SECURITY TEST is `never echoes a literal secret …`: it seeds
// a REAL .cursor/mcp.json member carrying a literal secret on disk (so the value
// genuinely exists where the backend could echo it), drives the full
// disable → cold → Re-add → Add-server flow, and FALSIFIES the no-secret-echo
// claim two ways:
//   (a) NO /api/* response body observed during the flow contains the literal;
//   (b) the Add-server form (its YAML preview + every env input) never holds it.
// An EXTRACT-MANIFEST TRAP is installed: if the readd branch ever reached for
// /api/extract-manifest (the dead post-delete path that carries client env
// VERBATIM = a re-leak), the trap returns the literal — and assertion (a)/(b)
// would fail. The flow must never touch it.
//
// The /api/projects aggregate is page.route-stubbed to the SANITIZED (raw=NIL)
// wire shape the production sanitizeScanResult / stripClientEntryRaw posture
// actually returns — exactly as every other projects-*.spec does. The backend
// strip itself is proven by the Go unit tests + projects-toggle.spec; this spec
// proves the GUI Re-add WIRING never re-introduces a literal.

import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { seededHubFor, expect } from "../fixtures/seeded-hub";
import { test as baseTest, expect as baseExpect } from "../fixtures/hub";
import type { Page, Route } from "@playwright/test";

const LIVE_SECRET = "sk-LIVE-SECRET-do-not-echo";
const PROJECT_KEY = "/home/x/proj";
const SERVER = "memo";

// seedCursorWithLiveSecret writes a global ~/.cursor/mcp.json whose member
// carries a LITERAL secret in env + an Authorization header — the worst-case
// value the no-secret-echo invariant must never let reach the wire or the form.
function seedCursorWithLiveSecret(home: string): void {
  const cursorDir = join(home, ".cursor");
  mkdirSync(cursorDir, { recursive: true });
  const config = {
    mcpServers: {
      [SERVER]: {
        command: "node",
        args: ["/opt/memo/server.js"],
        env: { MY_TOKEN: LIVE_SECRET },
        headers: { Authorization: `Bearer ${LIVE_SECRET}` },
      },
    },
  };
  writeFileSync(join(cursorDir, "mcp.json"), JSON.stringify(config, null, 2) + "\n", "utf8");
}

const seededTest = seededHubFor(seedCursorWithLiveSecret);

// proj/scanEntry mirror the SANITIZED (raw=NIL) aggregate wire shape.
function proj(over: Record<string, unknown> = {}) {
  return { key: PROJECT_KEY, workspace_path: PROJECT_KEY, entries: [], ...over };
}
function scanEntry(name: string, presence: Record<string, unknown>, over: Record<string, unknown> = {}) {
  return { name, client_presence: presence, manifest_exists: true, can_migrate: true, ...over };
}

// installExtractTrap fails the test loudly if the readd flow ever calls
// /api/extract-manifest — and, if it somehow does, hands back the literal so the
// no-secret assertions catch the leak too.
function installExtractTrap(page: Page, hits: { count: number }) {
  return page.route("**/api/extract-manifest**", async (route: Route) => {
    hits.count += 1;
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ yaml: `name: 'memo'\nenv:\n  MY_TOKEN: '${LIVE_SECRET}'\n` }),
    });
  });
}

seededTest.describe("D2 — secret-safe cold re-enable Re-add", () => {
  // -------------------------------------------------------------------------
  // THE FALSIFIER: full disable → cold → Re-add → Add-server, with a real
  // on-disk literal secret + an extract-manifest trap. No /api/* body and no
  // form field may ever contain the literal.
  // -------------------------------------------------------------------------
  seededTest("never echoes a literal secret through the disable → Re-add → Add-server flow", async ({
    page,
    hub,
  }) => {
    // Record every /api/* response body seen during the flow so we can scan
    // them for the literal after the run.
    const apiBodies: string[] = [];
    page.on("response", async (resp) => {
      const url = resp.url();
      if (!url.includes("/api/")) return;
      try {
        apiBodies.push(await resp.text());
      } catch {
        // Some responses (204, redirects) have no body — ignore.
      }
    });

    const extractHits = { count: 0 };
    await installExtractTrap(page, extractHits);

    // SANITIZED aggregate: raw is NIL on the wire (exactly what the production
    // strip posture returns) — the member's literal secret never ships here.
    await page.route("**/api/projects", async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
            projects: [proj({ scan: { at: "now", entries: [scanEntry(SERVER, { cursor: {} })] } })],
            groups: [],
          }),
        });
      } else {
        await route.continue();
      }
    });
    // The disable toggle reconciles OFF (member removed) — value-free, never
    // echoing the deleted member's literal.
    await page.route("**/api/projects/toggle", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ scope: "project-object-member", server: SERVER, enabled: false }),
      });
    });
    // Catalog without a match for `memo` → the Re-add lands on the honest
    // name-only branch (blank command/env), NOT a value-bearing prefill.
    await page.route("**/api/catalog", async (route) => {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ catalog: [] }) });
    });

    await page.goto(`${hub.url}/#/projects?path=${encodeURIComponent(PROJECT_KEY)}`);
    await expect(page.getByTestId("projects-client-cursor")).toBeVisible();

    // Disable the cursor object-member → it reconciles OFF → cold Re-add CTA.
    const toggle = page.getByTestId(`projects-toggle-project-object-member-${SERVER}`);
    await expect(toggle).toBeChecked();
    await toggle.click();
    const readd = page.getByTestId(`projects-readd-${SERVER}`);
    await expect(readd).toBeVisible();
    await expect(readd).toHaveAttribute("href", `#/add-server?readd=${SERVER}`);

    // Follow the Re-add CTA into the Add-server form.
    await readd.click();
    await expect(page.locator("h1")).toHaveText("Add server");
    // Name pre-filled from ?readd=; honest banner (not in the catalog).
    await expect(page.locator("#field-name")).toHaveValue(SERVER);
    await expect(page.getByTestId("banner")).toContainText(`Re-adding ${SERVER}`);
    await expect(page.getByTestId("banner")).toContainText("Not found in the catalog");

    // (b) THE FORM never holds the literal: YAML preview + every env input.
    await expect(page.locator('[data-testid="yaml-preview"]')).not.toContainText(LIVE_SECRET);
    const envInputs = page.locator("[data-env-row] input");
    const n = await envInputs.count();
    for (let i = 0; i < n; i++) {
      await expect(envInputs.nth(i)).not.toHaveValue(new RegExp(LIVE_SECRET));
    }

    // The dead extract path was NEVER reached.
    expect(extractHits.count).toBe(0);

    // (a) NO /api/* response body observed during the flow contained the literal.
    const leaked = apiBodies.filter((b) => b.includes(LIVE_SECRET));
    expect(leaked, `a /api/* response leaked the literal secret: ${leaked.slice(0, 1)}`).toHaveLength(0);
  });
});

// The pre-fill ROUTING specs run on the plain (clean-home) hub fixture: they
// drive #/add-server?readd=<name> directly and stub /api/catalog +
// /api/manifest/get for a deterministic verdict.
baseTest.describe("D2 — Add-server ?readd= pre-fill routing", () => {
  // NON-catalog re-add → name-only seed + honest banner; never extract-manifest.
  baseTest("non-catalog ?readd= seeds the name + honest banner, no extract fetch", async ({ page, hub }) => {
    const extractHits = { count: 0 };
    await installExtractTrap(page, extractHits);
    await page.route("**/api/catalog", async (route) => {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ catalog: [{ name: "other", description: "", kind: "global" }] }) });
    });

    await page.goto(`${hub.url}/#/add-server?readd=customsrv`);
    await baseExpect(page.locator("h1")).toHaveText("Add server");
    await baseExpect(page.locator("#field-name")).toHaveValue("customsrv");
    await baseExpect(page.getByTestId("banner")).toContainText("Re-adding customsrv");
    await baseExpect(page.getByTestId("banner")).toContainText("Not found in the catalog");
    // Blank command + no env on the honest branch (the blank form still
    // serializes an EMPTY `command: ''`; assert it is empty, and that no env
    // block was seeded).
    await baseExpect(page.locator('[data-testid="yaml-preview"]')).toContainText("command: ''");
    await baseExpect(page.locator('[data-testid="yaml-preview"]')).not.toContainText("env:");
    baseExpect(extractHits.count).toBe(0);
  });

  // CATALOG-known re-add → prefill from the SHIPPED manifest: command/args
  // present, sensitive env as a secret: ref (NEVER a literal). Never extract.
  baseTest("catalog-known ?readd= prefills from the shipped manifest (command/args present, env as secret: ref)", async ({ page, hub }) => {
    const extractHits = { count: 0 };
    await installExtractTrap(page, extractHits);
    await page.route("**/api/catalog", async (route) => {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ catalog: [{ name: "wolfram", description: "", kind: "global" }] }) });
    });
    let manifestHit = false;
    await page.route("**/api/manifest/get**", async (route) => {
      manifestHit = true;
      // The SHIPPED manifest carries a secret: ref, NOT a literal — the
      // secret-safe source D2 relies on.
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          yaml:
            "name: 'wolfram'\nkind: global\ntransport: stdio-bridge\ncommand: 'node'\n" +
            "base_args:\n  - 'index.js'\nenv:\n  WOLFRAM_LLM_APP_ID: 'secret:wolfram_app_id'\n",
          hash: "h1",
        }),
      });
    });

    await page.goto(`${hub.url}/#/add-server?readd=wolfram`);
    await baseExpect(page.locator("h1")).toHaveText("Add server");
    await baseExpect(page.locator("#field-name")).toHaveValue("wolfram");
    // Command/args come along from the shipped manifest.
    await baseExpect(page.locator('[data-testid="yaml-preview"]')).toContainText("command: 'node'");
    await baseExpect(page.locator('[data-testid="yaml-preview"]')).toContainText("index.js");
    // The sensitive env is a secret: ref, never a resolved literal.
    await baseExpect(page.locator('[data-testid="yaml-preview"]')).toContainText("secret:wolfram_app_id");
    // No honest banner on the catalog-match path.
    await baseExpect(page.getByTestId("banner")).toHaveCount(0);
    baseExpect(manifestHit).toBe(true);
    baseExpect(extractHits.count).toBe(0);
  });

  // A1 REGRESSION: the existing ?server=&from-client= Create-manifest extract
  // prefill still fires (the new readd param is purely additive).
  baseTest("A1 regression: ?server=&from-client= still runs the extract-manifest prefill", async ({ page, hub }) => {
    let extractHit = false;
    await page.route("**/api/extract-manifest**", async (route) => {
      extractHit = true;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ yaml: "name: 'ghost'\nkind: global\ntransport: stdio-bridge\ncommand: 'echo'\n" }),
      });
    });

    await page.goto(`${hub.url}/#/add-server?server=ghost&from-client=cursor`);
    await baseExpect(page.locator("h1")).toHaveText("Add server");
    // The A1 extract prefill ran (its YAML landed in the form).
    await baseExpect(page.locator("#field-name")).toHaveValue("ghost");
    await baseExpect(page.locator('[data-testid="yaml-preview"]')).toContainText("command: 'echo'");
    baseExpect(extractHit).toBe(true);
  });
});
