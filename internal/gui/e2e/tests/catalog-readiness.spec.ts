// internal/gui/e2e/tests/catalog-readiness.spec.ts
//
// E2E scenarios for the Catalog pre-install readiness gate (epic
// install-and-it-works, area 2 — env-secrets-onboarding). Clicking Install on a
// shipped catalog row opens a readiness panel BEFORE the POST /api/install so
// the operator sees blockers (with guided fixes) and optional-secret prompts.
//
// Design notes (mirrors secret-picker.spec.ts / secrets.spec.ts):
// - The whole flow is driven via page.route() stubs so the readiness verdict is
//   DETERMINISTIC regardless of what launchers / secrets exist on the CI runner.
//   /api/catalog, /api/status, /api/marketplace, /api/server/readiness, and
//   /api/install are all stubbed; the production handlers are exercised by the
//   Go unit tests, not here. This spec proves the GUI WIRING (gate opens, blocker
//   disables Confirm, the secret affordance opens AddSecretModal + deep-links to
//   Secrets) end-to-end against the real Preact build + hash router.
// - Stubs are registered BEFORE page.goto so the first load already sees them.

import { test, expect } from "../fixtures/hub";

// routeJSON registers a GET stub returning `body` (200) for a glob.
async function routeJSON(page: import("@playwright/test").Page, glob: string, body: unknown) {
  await page.route(glob, async (route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(body),
      });
    } else {
      await route.continue();
    }
  });
}

// Stub the always-loaded Catalog screen GETs (catalog list, status, marketplace)
// so the screen renders a single deterministic row and an empty marketplace.
async function stubCatalogShell(
  page: import("@playwright/test").Page,
  catalog: Array<{ name: string; description?: string; kind?: string }>,
) {
  await routeJSON(page, "**/api/catalog**", { catalog });
  await routeJSON(page, "**/api/status**", []);
  await routeJSON(page, "**/api/marketplace**", { entries: [] });
}

test.describe("Catalog pre-install readiness gate (epic area 2)", () => {
  // -------------------------------------------------------------------------
  // 1. Blocker (missing launcher) → blocker + Fix shown, Confirm Install disabled
  // -------------------------------------------------------------------------
  test("scenario 1: a blocker shows the Fix and disables Confirm install", async ({ page, hub }) => {
    await stubCatalogShell(page, [{ name: "gdb-mcp", description: "GDB debugger bridge", kind: "global" }]);
    // Readiness reports a non-optional unmet launcher → a real blocker.
    await routeJSON(page, "**/api/server/readiness**", {
      server: "gdb-mcp",
      ready: false,
      requirements: [
        {
          name: "binary: gdb",
          ok: false,
          optional: false,
          reason: "\"gdb\" not found on PATH",
          fix: "Install gdb and ensure it is on PATH, then re-run install.",
        },
      ],
    });

    await page.goto(hub.url + "/#/catalog");
    await page.getByTestId("catalog-install-gdb-mcp").click();

    // The gate opens with the readiness panel.
    await expect(page.getByTestId("catalog-readiness-gate-gdb-mcp")).toBeVisible();
    // The guided Fix is rendered verbatim.
    await expect(page.getByText("Install gdb and ensure it is on PATH")).toBeVisible();
    // Confirm Install is DISABLED while the blocker is present.
    const confirm = page.getByTestId("catalog-install-confirm-gdb-mcp");
    await expect(confirm).toBeDisabled();
    await expect(confirm).toContainText("blocker");
  });

  // -------------------------------------------------------------------------
  // 2. Optional unset secret → prompt rendered; set via inline modal → re-check;
  //    Install stays enabled (advisory, non-blocking).
  // -------------------------------------------------------------------------
  test("scenario 2: optional-secret prompt + set via inline AddSecretModal", async ({ page, hub }) => {
    await stubCatalogShell(page, [{ name: "wolfram", description: "WolframAlpha", kind: "global" }]);

    // First readiness fetch: the optional secret is unset (advisory). After the
    // modal POSTs the secret, the SECOND fetch reports it set.
    let secretSet = false;
    await page.route("**/api/server/readiness**", async (route) => {
      if (route.request().method() !== "GET") {
        await route.continue();
        return;
      }
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          server: "wolfram",
          ready: true,
          requirements: [
            {
              name: "secret: WOLFRAM_APP_ID",
              ok: secretSet,
              optional: true,
              reason: secretSet ? undefined : "could not be resolved from the vault (optional)",
              fix: "Enter WOLFRAM_APP_ID at install, or set it later via the Secrets screen.",
            },
          ],
        }),
      });
    });
    // AddSecretModal POSTs the secret; the GET vault list is irrelevant here.
    await page.route("**/api/secrets", async (route) => {
      const m = route.request().method();
      if (m === "POST") {
        secretSet = true;
        await route.fulfill({ status: 201, contentType: "application/json", body: "{}" });
      } else if (m === "GET") {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ vault_state: "ok", secrets: [], manifest_errors: [] }),
        });
      } else {
        await route.continue();
      }
    });

    await page.goto(hub.url + "/#/catalog");
    await page.getByTestId("catalog-install-wolfram").click();

    // The optional-secret prompt renders the "Set <key>" + "Open Secrets" actions.
    await expect(page.getByTestId("catalog-secret-set-WOLFRAM_APP_ID")).toBeVisible();
    await expect(page.getByTestId("catalog-secret-open-secrets-WOLFRAM_APP_ID")).toBeVisible();
    // An optional unset secret is advisory → Confirm install is ENABLED already.
    await expect(page.getByTestId("catalog-install-confirm-wolfram")).toBeEnabled();

    // Click "Set WOLFRAM_APP_ID" → AddSecretModal opens pre-filled + name locked.
    await page.getByTestId("catalog-secret-set-WOLFRAM_APP_ID").click();
    const modal = page.getByTestId("add-secret-modal");
    await expect(modal).toBeVisible();
    const nameInput = modal.locator('input[type="text"]');
    await expect(nameInput).toHaveValue("WOLFRAM_APP_ID");
    await expect(nameInput).toBeDisabled();

    // Fill the value + save → modal closes; the gate re-fetches readiness so the
    // secret row flips to satisfied (no more "Set" affordance).
    await modal.locator('input[type="password"]').fill("the-app-id");
    await modal.locator('button[type="submit"]').click();
    await expect(modal).not.toBeVisible({ timeout: 8_000 });
    await expect(page.getByTestId("catalog-secret-set-WOLFRAM_APP_ID")).toHaveCount(0, {
      timeout: 5_000,
    });
  });

  // -------------------------------------------------------------------------
  // 3. "Open Secrets" deep-link → #/secrets?key=<key> with the key prefilled.
  // -------------------------------------------------------------------------
  test("scenario 3: Open Secrets deep-links to #/secrets?key= with the key prefilled", async ({
    page,
    hub,
  }) => {
    await stubCatalogShell(page, [{ name: "wolfram", description: "WolframAlpha", kind: "global" }]);
    await routeJSON(page, "**/api/server/readiness**", {
      server: "wolfram",
      ready: true,
      requirements: [
        { name: "secret: WOLFRAM_APP_ID", ok: false, optional: true, reason: "not set" },
      ],
    });
    // Secrets screen needs an initialized (ok) but empty vault to render the
    // Add-secret modal; stub it so the deep-link opens the modal deterministically.
    await routeJSON(page, "**/api/secrets", { vault_state: "ok", secrets: [], manifest_errors: [] });

    await page.goto(hub.url + "/#/catalog");
    await page.getByTestId("catalog-install-wolfram").click();

    // Click the "Open Secrets" link → hash navigates to #/secrets?key=WOLFRAM_APP_ID.
    await page.getByTestId("catalog-secret-open-secrets-WOLFRAM_APP_ID").click();
    await expect.poll(() => new URL(page.url()).hash).toBe("#/secrets?key=WOLFRAM_APP_ID");

    // The Secrets screen auto-opens AddSecretModal pre-filled with that key.
    const modal = page.getByTestId("add-secret-modal");
    await expect(modal).toBeVisible();
    await expect(modal.locator('input[type="text"]')).toHaveValue("WOLFRAM_APP_ID");
    await expect(modal.locator('input[type="text"]')).toBeDisabled();
  });

  // -------------------------------------------------------------------------
  // 4. Cancel closes the gate without installing.
  // -------------------------------------------------------------------------
  test("scenario 4: Cancel closes the readiness gate without installing", async ({ page, hub }) => {
    await stubCatalogShell(page, [{ name: "time", description: "Time server", kind: "global" }]);
    await routeJSON(page, "**/api/server/readiness**", { server: "time", ready: true, requirements: [] });
    let installPosted = false;
    await page.route("**/api/install**", async (route) => {
      if (route.request().method() === "POST") {
        installPosted = true;
        await route.fulfill({ status: 204, body: "" });
      } else {
        await route.continue();
      }
    });

    await page.goto(hub.url + "/#/catalog");
    await page.getByTestId("catalog-install-time").click();
    await expect(page.getByTestId("catalog-readiness-gate-time")).toBeVisible();
    await page.getByTestId("catalog-install-cancel-time").click();

    // Gate closes; the plain Install button is back; no install POST fired.
    await expect(page.getByTestId("catalog-readiness-gate-time")).toHaveCount(0);
    await expect(page.getByTestId("catalog-install-time")).toBeVisible();
    expect(installPosted).toBe(false);
  });
});
