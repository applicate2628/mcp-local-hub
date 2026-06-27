// internal/gui/e2e/tests/projects-readd-d2.spec.ts
//
// D2 r2 — secret-safe pre-filled cold object-member re-enable, now sourced
// through the SINGLE embed-only endpoint GET /api/catalog/manifest. These specs
// drive the Re-add CTA → Add-server pre-fill flow against a LIVE mcphub gui.
//
// THE LOAD-BEARING SECURITY TEST is `never echoes a literal secret …`: it seeds
// a REAL .cursor/mcp.json member carrying a literal secret on disk (so the value
// genuinely exists where the backend could echo it), drives the full
// disable → cold → Re-add → Add-server flow, and FALSIFIES the no-secret-echo
// claim two ways:
//   (a) NO /api/* response body observed during the flow contains the literal;
//   (b) the Add-server form (its YAML preview + every env input) never holds it.
// The invariant is now embed-only-BY-CONSTRUCTION: the prefill's ONLY source is
// /api/catalog/manifest (embed-only with a membership gate that excludes any
// disk-only name BEFORE the loader), so a disk manifest's literal can never be
// sourced. An EXTRACT-MANIFEST TRAP is still installed: if the readd branch ever
// reached for /api/extract-manifest (the dead post-delete path that carries
// client env VERBATIM = a re-leak), the trap returns the literal — and assertion
// (a)/(b) would fail. The flow must never touch it, nor the disk-only edit
// contract /api/manifest/get.
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
    // The embed-only catalog-manifest lookup 404s for `memo` (not in the
    // embedded set) → the Re-add lands on the honest name-only branch (blank
    // command/env), NOT a value-bearing prefill.
    await page.route("**/api/catalog/manifest**", async (route) => {
      await route.fulfill({
        status: 404,
        contentType: "application/json",
        body: JSON.stringify({ error: "not found", code: "CATALOG_MANIFEST_NOT_FOUND" }),
      });
    });
    // A DISK-READ TRAP: the readd flow must NEVER hit the disk-only edit
    // contract /api/manifest/get (which could read a hand-planted on-disk
    // manifest carrying a literal). If it ever does, hand back the literal so
    // the no-secret assertions catch the leak too.
    let manifestGetHit = false;
    await page.route("**/api/manifest/get**", async (route) => {
      manifestGetHit = true;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ yaml: `name: 'memo'\nenv:\n  MY_TOKEN: '${LIVE_SECRET}'\n`, hash: "h" }),
      });
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
    // F3: the honest no-match copy (the catalog was read; it does not list memo).
    await expect(page.getByTestId("banner")).toContainText("isn't in the catalog");
    // F1: a non-catalog re-add is a NORMAL outcome, so the banner is the neutral
    // info kind (renders .banner.info), never the red .banner.error.
    await expect(page.getByTestId("banner")).toHaveClass(/banner info/);

    // (b) THE FORM never holds the literal: YAML preview + every env input.
    await expect(page.locator('[data-testid="yaml-preview"]')).not.toContainText(LIVE_SECRET);
    const envInputs = page.locator("[data-env-row] input");
    const n = await envInputs.count();
    for (let i = 0; i < n; i++) {
      await expect(envInputs.nth(i)).not.toHaveValue(new RegExp(LIVE_SECRET));
    }

    // The dead extract path was NEVER reached, and neither was the disk-only
    // edit contract — the embed-only endpoint is the sole prefill source.
    expect(extractHits.count).toBe(0);
    expect(manifestGetHit, "the readd flow must never call the disk-only /api/manifest/get").toBe(false);

    // (a) NO /api/* response body observed during the flow contained the literal.
    const leaked = apiBodies.filter((b) => b.includes(LIVE_SECRET));
    expect(leaked, `a /api/* response leaked the literal secret: ${leaked.slice(0, 1)}`).toHaveLength(0);
  });

  // -------------------------------------------------------------------------
  // F3 (QA-gap): a catalog READ FAILURE (500) must degrade to the distinct
  // read-failure copy — NOT the "isn't in the catalog" no-match copy — and the
  // form must still seed BLANK (no literal-secret echo). Reuses the seeded
  // on-disk literal + the extract-manifest trap so the read-failure degrade is
  // ALSO proven secret-safe end-to-end.
  // -------------------------------------------------------------------------
  seededTest("catalog read-failure (500) degrades to the read-failure banner + blank form, no secret echo", async ({
    page,
    hub,
  }) => {
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

    // SANITIZED aggregate (raw=NIL) so the disable → cold → Re-add path is the
    // same as the falsifier; only the catalog read fails here.
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
    await page.route("**/api/projects/toggle", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ scope: "project-object-member", server: SERVER, enabled: false }),
      });
    });
    // THE READ FAILURE: the embed-only catalog-manifest lookup 500s, so the
    // readd flow cannot know whether `memo` is embedded → it degrades to the
    // read-failure copy (NOT the 404 no-match copy).
    await page.route("**/api/catalog/manifest**", async (route) => {
      await route.fulfill({ status: 500, contentType: "application/json", body: JSON.stringify({ error: "catalog manifest unavailable", code: "CATALOG_MANIFEST_GET_FAILED" }) });
    });

    await page.goto(`${hub.url}/#/projects?path=${encodeURIComponent(PROJECT_KEY)}`);
    await expect(page.getByTestId("projects-client-cursor")).toBeVisible();

    const toggle = page.getByTestId(`projects-toggle-project-object-member-${SERVER}`);
    await expect(toggle).toBeChecked();
    await toggle.click();
    const readd = page.getByTestId(`projects-readd-${SERVER}`);
    await expect(readd).toBeVisible();

    await readd.click();
    await expect(page.locator("h1")).toHaveText("Add server");
    await expect(page.locator("#field-name")).toHaveValue(SERVER);

    // F3: the read-failure copy — it must NOT assert "isn't in the catalog"
    // (the lookup failed; membership is unknown), and it must name the cause.
    await expect(page.getByTestId("banner")).toContainText(`Re-adding ${SERVER}`);
    await expect(page.getByTestId("banner")).toContainText("catalog lookup failed");
    await expect(page.getByTestId("banner")).not.toContainText("isn't in the catalog");
    // F1: a read-failure degrade is a NORMAL outcome → neutral info kind.
    await expect(page.getByTestId("banner")).toHaveClass(/banner info/);

    // Blank form: command empty, no env block — the secret-safe seed.
    await expect(page.locator('[data-testid="yaml-preview"]')).toContainText("command: ''");
    await expect(page.locator('[data-testid="yaml-preview"]')).not.toContainText("env:");

    // The form never holds the literal (YAML preview + every env input).
    await expect(page.locator('[data-testid="yaml-preview"]')).not.toContainText(LIVE_SECRET);
    const envInputs = page.locator("[data-env-row] input");
    const n = await envInputs.count();
    for (let i = 0; i < n; i++) {
      await expect(envInputs.nth(i)).not.toHaveValue(new RegExp(LIVE_SECRET));
    }

    // The dead extract path was NEVER reached on the read-failure degrade.
    expect(extractHits.count).toBe(0);
    // No /api/* response body leaked the literal during the read-failure flow.
    const leaked = apiBodies.filter((b) => b.includes(LIVE_SECRET));
    expect(leaked, `a /api/* response leaked the literal secret: ${leaked.slice(0, 1)}`).toHaveLength(0);
  });
});

// The pre-fill ROUTING specs run on the plain (clean-home) hub fixture: they
// drive #/add-server?readd=<name> directly and stub the SINGLE embed-only
// endpoint /api/catalog/manifest for a deterministic verdict.
baseTest.describe("D2 — Add-server ?readd= pre-fill routing", () => {
  // NON-catalog re-add (404 from /api/catalog/manifest) → name-only seed +
  // honest banner; never extract-manifest, never the disk-only /api/manifest/get.
  baseTest("non-catalog ?readd= seeds the name + honest banner, no extract/disk fetch", async ({ page, hub }) => {
    const extractHits = { count: 0 };
    await installExtractTrap(page, extractHits);
    let manifestGetHit = false;
    await page.route("**/api/manifest/get**", async (route) => {
      manifestGetHit = true;
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ yaml: "", hash: "" }) });
    });
    await page.route("**/api/catalog/manifest**", async (route) => {
      await route.fulfill({ status: 404, contentType: "application/json", body: JSON.stringify({ error: "not found", code: "CATALOG_MANIFEST_NOT_FOUND" }) });
    });

    await page.goto(`${hub.url}/#/add-server?readd=customsrv`);
    await baseExpect(page.locator("h1")).toHaveText("Add server");
    await baseExpect(page.locator("#field-name")).toHaveValue("customsrv");
    await baseExpect(page.getByTestId("banner")).toContainText("Re-adding customsrv");
    // F3: honest no-match copy + F1 neutral info kind (not red error).
    await baseExpect(page.getByTestId("banner")).toContainText("isn't in the catalog");
    await baseExpect(page.getByTestId("banner")).toHaveClass(/banner info/);
    // Blank command + no env on the honest branch (the blank form still
    // serializes an EMPTY `command: ''`; assert it is empty, and that no env
    // block was seeded).
    await baseExpect(page.locator('[data-testid="yaml-preview"]')).toContainText("command: ''");
    await baseExpect(page.locator('[data-testid="yaml-preview"]')).not.toContainText("env:");
    baseExpect(extractHits.count).toBe(0);
    baseExpect(manifestGetHit).toBe(false);
  });

  // CATALOG-known re-add → prefill from the EMBED manifest via the single
  // /api/catalog/manifest endpoint: command/args present, sensitive env as a
  // secret: ref (NEVER a literal). Never extract, never the disk-only edit get.
  baseTest("catalog-known ?readd= prefills from the embed manifest (command/args present, env as secret: ref)", async ({ page, hub }) => {
    const extractHits = { count: 0 };
    await installExtractTrap(page, extractHits);
    let manifestGetHit = false;
    await page.route("**/api/manifest/get**", async (route) => {
      manifestGetHit = true;
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ yaml: "", hash: "" }) });
    });
    let catalogManifestHit = false;
    await page.route("**/api/catalog/manifest**", async (route) => {
      catalogManifestHit = true;
      // The EMBED manifest carries a secret: ref, NOT a literal — the
      // secret-safe source D2 relies on. (No hash field: read-for-prefill
      // contract, not the optimistic-concurrency edit one.)
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          yaml:
            "name: 'wolfram'\nkind: global\ntransport: stdio-bridge\ncommand: 'node'\n" +
            "base_args:\n  - 'index.js'\nenv:\n  WOLFRAM_LLM_APP_ID: 'secret:wolfram_app_id'\n",
        }),
      });
    });

    await page.goto(`${hub.url}/#/add-server?readd=wolfram`);
    await baseExpect(page.locator("h1")).toHaveText("Add server");
    await baseExpect(page.locator("#field-name")).toHaveValue("wolfram");
    // Command/args come along from the embed manifest.
    await baseExpect(page.locator('[data-testid="yaml-preview"]')).toContainText("command: 'node'");
    await baseExpect(page.locator('[data-testid="yaml-preview"]')).toContainText("index.js");
    // The sensitive env is a secret: ref, never a resolved literal.
    await baseExpect(page.locator('[data-testid="yaml-preview"]')).toContainText("secret:wolfram_app_id");
    // F4: the catalog-match prefill is no longer silent — a neutral info notice
    // explains WHY the form is pre-filled and nudges the operator to set secrets.
    await baseExpect(page.getByTestId("banner")).toContainText("Pre-filled from the catalog for wolfram");
    await baseExpect(page.getByTestId("banner")).toHaveClass(/banner info/);
    baseExpect(catalogManifestHit).toBe(true);
    baseExpect(manifestGetHit).toBe(false);
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
