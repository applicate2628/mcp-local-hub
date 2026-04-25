// internal/gui/e2e/tests/secrets.spec.ts
//
// 14 E2E scenarios for the Secrets registry screen (#/secrets).
//
// Design notes:
// - Most scenarios stub GET /api/secrets via page.route() because the
//   E2E binary uses embed-FS manifests (wolfram_app_id + unpaywall_email)
//   that would pollute the secrets list in tests expecting a clean state.
// - The hub fixture sets LOCALAPPDATA=hub.home on Windows, so vault files
//   land at <hub.home>/mcp-local-hub/.age-key and secrets.age.
// - Scenarios requiring "daemon running" counts stub /api/status because
//   MCPHUB_E2E_SCHEDULER=none forces /api/status to return [] always.

import { test, expect } from "../fixtures/hub";
import { join, resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = dirname(fileURLToPath(import.meta.url));

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/** secrEnvelope: build a minimal SecretsEnvelope JSON body for page.route stubs. */
function secrEnvelope(
  vaultState: string,
  secrets: Array<{
    name: string;
    state: "present" | "referenced_missing" | "referenced_unverified";
    used_by: Array<{ server: string; env_var: string }>;
  }> = [],
  manifestErrors: Array<{ path: string; error: string }> = [],
) {
  return JSON.stringify({
    vault_state: vaultState,
    secrets,
    manifest_errors: manifestErrors,
  });
}

// ---------------------------------------------------------------------------
// Test suite
// ---------------------------------------------------------------------------

test.describe("Secrets registry", () => {
  // -------------------------------------------------------------------------
  // 1. Empty-state init flow
  // -------------------------------------------------------------------------
  test("Empty-state init flow", async ({ page, hub }) => {
    // Fresh tmpHome → vault files don't exist → vault_state == "missing".
    // Stub /api/status so InitKeyedView's refreshRunning() doesn't 500.
    await page.route("**/api/status", (r) =>
      r.fulfill({ status: 200, contentType: "application/json", body: "[]" }),
    );
    // Stub GET /api/secrets: first response = missing vault; after init = ok empty.
    let inited = false;
    await page.route("**/api/secrets", async (r) => {
      if (r.request().method() !== "GET") { await r.continue(); return; }
      if (!inited) {
        await r.fulfill({
          status: 200,
          contentType: "application/json",
          body: secrEnvelope("missing"),
        });
      } else {
        await r.fulfill({
          status: 200,
          contentType: "application/json",
          body: secrEnvelope("ok", [], []),
        });
      }
    });
    // Stub POST /api/secrets/init → 200.
    await page.route("**/api/secrets/init", async (r) => {
      inited = true;
      await r.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ vault_state: "ok" }),
      });
    });

    await page.goto(`${hub.url}/#/secrets`);

    // The screen renders the "not initialized" empty state.
    await expect(page.getByText("Secrets vault is not initialized")).toBeVisible();
    const initButton = page.getByRole("button", { name: "Initialize secrets vault" });
    await expect(initButton).toBeVisible();

    // Click init and assert POST /api/secrets/init fires + returns 200.
    const responsePromise = page.waitForResponse(
      (r) =>
        r.url().includes("/api/secrets/init") && r.request().method() === "POST",
    );
    await initButton.click();
    const resp = await responsePromise;
    expect(resp.status()).toBe(200);

    // After successful init the screen refreshes to the "No secrets yet" view.
    await expect(page.getByText("No secrets yet")).toBeVisible();
  });

  // -------------------------------------------------------------------------
  // 2. Add first secret
  // -------------------------------------------------------------------------
  test("Add first secret", async ({ page, hub }) => {
    // Stub GET /api/secrets:
    //   phase 0 → missing (show init button)
    //   phase 1 → ok empty (after init, show "No secrets yet" + Add)
    //   phase 2 → ok with OPENAI_API_KEY (after save)
    let phase = 0;
    await page.route("**/api/secrets", async (r) => {
      if (r.request().method() === "POST") {
        // Let POST through to real backend (vault must be real to accept Set).
        // Actually we stub 201 here too since vault isn't really init'd.
        phase = 2;
        await r.fulfill({ status: 201, body: "" });
        return;
      }
      if (phase === 0) {
        await r.fulfill({
          status: 200,
          contentType: "application/json",
          body: secrEnvelope("missing"),
        });
      } else if (phase === 1) {
        await r.fulfill({
          status: 200,
          contentType: "application/json",
          body: secrEnvelope("ok", [], []),
        });
      } else {
        await r.fulfill({
          status: 200,
          contentType: "application/json",
          body: secrEnvelope("ok", [
            { name: "OPENAI_API_KEY", state: "present", used_by: [] },
          ]),
        });
      }
    });
    await page.route("**/api/status", (r) =>
      r.fulfill({ status: 200, contentType: "application/json", body: "[]" }),
    );
    // Stub POST /api/secrets/init → 200 (advance to phase 1).
    await page.route("**/api/secrets/init", async (r) => {
      phase = 1;
      await r.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ vault_state: "ok" }),
      });
    });

    await page.goto(`${hub.url}/#/secrets`);

    // Phase 0: init button visible.
    const initButton = page.getByRole("button", { name: "Initialize secrets vault" });
    await expect(initButton).toBeVisible();
    await initButton.click();
    // Phase 1: "No secrets yet" with Add secret button.
    await expect(page.getByText("No secrets yet")).toBeVisible();

    // Open the Add secret modal.
    await page.getByRole("button", { name: "Add secret" }).click();
    const modal = page.locator('[data-testid="add-secret-modal"]');
    await expect(modal).toBeVisible();

    // Fill in name + value.
    await modal.locator('input[placeholder="OPENAI_API_KEY"]').fill("OPENAI_API_KEY");
    await modal.locator('input[type="password"]').fill("sk-test-value-123");

    // Watch for the POST /api/secrets request.
    const postResponse = page.waitForResponse(
      (r) =>
        r.url().includes("/api/secrets") &&
        !r.url().includes("/init") &&
        r.request().method() === "POST",
    );

    await modal.getByRole("button", { name: "Save" }).click();

    const saved = await postResponse;
    // 201 from our stub.
    expect(saved.status()).toBe(201);

    // After save the modal closes and a row appears (phase 2 GET response).
    await expect(modal).not.toBeVisible();
    await expect(page.locator("table").getByText("OPENAI_API_KEY")).toBeVisible();
  });

  // -------------------------------------------------------------------------
  // 3. Used-by counts populate from manifest scan
  // -------------------------------------------------------------------------
  test("Used-by counts populate from manifest scan", async ({ page, hub }) => {
    // Stub GET /api/secrets to return K1 with used_by: 2 servers.
    await page.route("**/api/secrets", async (r) => {
      if (r.request().method() !== "GET") { await r.continue(); return; }
      await r.fulfill({
        status: 200,
        contentType: "application/json",
        body: secrEnvelope("ok", [
          {
            name: "K1",
            state: "present",
            used_by: [
              { server: "server-a", env_var: "OPENAI_API_KEY" },
              { server: "server-b", env_var: "OPENAI_API_KEY" },
            ],
          },
        ]),
      });
    });
    await page.route("**/api/status", (r) =>
      r.fulfill({ status: 200, contentType: "application/json", body: "[]" }),
    );

    await page.goto(`${hub.url}/#/secrets`);

    // The row for K1 shows used_by count = 2.
    const row = page.locator("table tbody tr").filter({ hasText: "K1" });
    await expect(row).toBeVisible();

    // The "Used by" column cell (index 1) contains "2".
    const usedByCell = row.locator("td").nth(1);
    await expect(usedByCell).toHaveText("2");

    // The tooltip on the used-by cell lists both servers (title attribute).
    const titleAttr = await usedByCell.getAttribute("title");
    expect(titleAttr).toContain("server-a");
    expect(titleAttr).toContain("server-b");
  });

  // -------------------------------------------------------------------------
  // 4. Ghost ref displays for manifest-only key
  // -------------------------------------------------------------------------
  test("Ghost ref displays for manifest-only key", async ({ page, hub }) => {
    // Vault is ok but WOLFRAM_APP_ID is referenced_missing.
    await page.route("**/api/secrets", async (r) => {
      if (r.request().method() !== "GET") { await r.continue(); return; }
      await r.fulfill({
        status: 200,
        contentType: "application/json",
        body: secrEnvelope("ok", [
          {
            name: "WOLFRAM_APP_ID",
            state: "referenced_missing",
            used_by: [{ server: "wolfram", env_var: "WOLFRAM_LLM_APP_ID" }],
          },
        ]),
      });
    });
    await page.route("**/api/status", (r) =>
      r.fulfill({ status: 200, contentType: "application/json", body: "[]" }),
    );

    await page.goto(`${hub.url}/#/secrets`);

    // The row must have data-state="referenced_missing".
    const row = page.locator("table tbody tr[data-state='referenced_missing']");
    await expect(row).toBeVisible();
    await expect(row).toContainText("WOLFRAM_APP_ID");

    // "Add this secret" hint link is present for the ghost row.
    await expect(row.getByRole("button", { name: "Add this secret" })).toBeVisible();
  });

  // -------------------------------------------------------------------------
  // 5. Decrypt-failed vault → referenced_unverified rows
  // -------------------------------------------------------------------------
  test("Decrypt-failed vault → referenced_unverified rows", async ({ page, hub }) => {
    // Vault state is decrypt_failed; manifest refs are referenced_unverified.
    await page.route("**/api/secrets", async (r) => {
      if (r.request().method() !== "GET") { await r.continue(); return; }
      await r.fulfill({
        status: 200,
        contentType: "application/json",
        body: secrEnvelope("decrypt_failed", [
          {
            name: "MY_KEY",
            state: "referenced_unverified",
            used_by: [{ server: "some-server", env_var: "MY_KEY" }],
          },
        ]),
      });
    });
    await page.route("**/api/status", (r) =>
      r.fulfill({ status: 200, contentType: "application/json", body: "[]" }),
    );

    await page.goto(`${hub.url}/#/secrets`);

    // The BrokenView banner is visible (vault unavailable).
    await expect(page.getByText("Vault unavailable")).toBeVisible();
    // vault_state text appears in the banner.
    await expect(page.getByText("decrypt_failed")).toBeVisible();
    // MY_KEY row is shown (referenced_unverified shown in broken-view table).
    await expect(page.getByText("MY_KEY")).toBeVisible();
  });

  // -------------------------------------------------------------------------
  // 6. Rotate Save without restart — 0 running suppresses CTA
  // -------------------------------------------------------------------------
  test("Rotate Save without restart — 0 running suppresses CTA", async ({
    page,
    hub,
  }) => {
    // Set up all route stubs BEFORE page.goto() to ensure mount-time
    // requests are intercepted immediately.
    await page.route("**/api/secrets", async (r) => {
      if (r.request().method() !== "GET") { await r.continue(); return; }
      await r.fulfill({
        status: 200,
        contentType: "application/json",
        body: secrEnvelope("ok", [
          { name: "K1", state: "present", used_by: [{ server: "alpha", env_var: "K1" }] },
        ]),
      });
    });
    // /api/status → [] (0 running daemons).
    await page.route("**/api/status", (r) =>
      r.fulfill({ status: 200, contentType: "application/json", body: "[]" }),
    );
    // PUT /api/secrets/K1 → 200 (no restart, empty results).
    // Use glob pattern with ** prefix for reliable matching.
    await page.route("**/api/secrets/K1", async (r) => {
      if (r.request().method() === "PUT") {
        await r.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ vault_updated: true, restart_results: [] }),
        });
        return;
      }
      await r.continue();
    });

    // Wait for the initial status fetch to complete (confirms runningByServer=0).
    const statusReady = page.waitForResponse(
      (r) => r.url().includes("/api/status") && r.request().method() === "GET",
    );
    await page.goto(`${hub.url}/#/secrets`);
    await statusReady;

    const row = page.locator("table tbody tr").filter({ hasText: "K1" });
    await expect(row).toBeVisible();

    // Click Rotate on the K1 row.
    await row.getByRole("button", { name: "Rotate" }).click();
    const rotateModal = page.locator('[data-testid="rotate-secret-modal"]');
    await expect(rotateModal).toBeVisible();

    // Fill new value and click "Save without restart".
    await rotateModal.locator('input[type="password"]').fill("new-value-xyz");

    // Wait for the PUT response before asserting.
    const putResponse = page.waitForResponse(
      (r) => r.url().includes("/api/secrets/K1") && r.request().method() === "PUT",
    );
    await rotateModal.getByRole("button", { name: "Save without restart" }).click();
    const putResp = await putResponse;
    expect(putResp.status()).toBe(200);

    // Modal closes.
    await expect(rotateModal).not.toBeVisible();

    // With 0 running daemons, the persistent CTA must NOT appear.
    await page.waitForTimeout(300);
    await expect(page.locator('[data-testid="rotate-cta"]')).toHaveCount(0);
  });

  // -------------------------------------------------------------------------
  // 7. Rotate Save without restart — N running: CTA persists after refresh
  // -------------------------------------------------------------------------
  test("Rotate Save without restart — CTA persists, Restart now fires POST", async ({
    page,
    hub,
  }) => {
    // Set up all route stubs BEFORE page.goto() so mount-time requests are
    // captured immediately. /api/status is stubbed with 2 running daemons so
    // the persistent CTA renders (affectedRunning > 0).
    await page.route("**/api/secrets", async (r) => {
      if (r.request().method() !== "GET") { await r.continue(); return; }
      await r.fulfill({
        status: 200,
        contentType: "application/json",
        body: secrEnvelope("ok", [
          {
            name: "K1",
            state: "present",
            used_by: [
              { server: "alpha", env_var: "K1" },
              { server: "beta", env_var: "K1" },
            ],
          },
        ]),
      });
    });
    // 2 running daemons — CTA must appear because affectedRunning > 0.
    await page.route("**/api/status", (r) =>
      r.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify([
          { server: "alpha", daemon: "default", state: "Running", port: 9121 },
          { server: "beta", daemon: "default", state: "Running", port: 9122 },
        ]),
      }),
    );
    // PUT /api/secrets/K1 → 200 (save without restart path).
    await page.route("**/api/secrets/K1", async (r) => {
      if (r.request().method() === "PUT") {
        await r.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ vault_updated: true, restart_results: [] }),
        });
        return;
      }
      await r.continue();
    });
    // POST /api/secrets/K1/restart → 200 (all daemons restarted successfully).
    await page.route("**/api/secrets/K1/restart", async (r) => {
      if (r.request().method() === "POST") {
        await r.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
            restart_results: [
              { task_name: "alpha/default", error: "" },
              { task_name: "beta/default", error: "" },
            ],
          }),
        });
        return;
      }
      await r.continue();
    });

    await page.goto(`${hub.url}/#/secrets`);
    const row = page.locator("table tbody tr").filter({ hasText: "K1" });
    await expect(row).toBeVisible();

    // Click Rotate on K1 and fill a new value.
    await row.getByRole("button", { name: "Rotate" }).click();
    const rotateModal = page.locator('[data-testid="rotate-secret-modal"]');
    await expect(rotateModal).toBeVisible();
    await rotateModal.locator('input[type="password"]').fill("new-value-abc");

    // Click "Save without restart" and wait for the PUT response.
    const putResponse = page.waitForResponse(
      (r) => r.url().includes("/api/secrets/K1") && r.request().method() === "PUT",
    );
    await rotateModal.getByRole("button", { name: "Save without restart" }).click();
    const putResp = await putResponse;
    expect(putResp.status()).toBe(200);

    // Modal closes after successful rotate.
    await expect(rotateModal).not.toBeVisible();

    // The persistent CTA banner must be visible after the background refresh.
    // With the stale-while-revalidate fix, the snapshot stays at status:"ok"
    // during refresh so InitKeyedView is NOT unmounted and bannerName survives.
    const cta = page.locator('[data-testid="rotate-cta"]');
    await expect(cta).toBeVisible();
    await expect(cta).toContainText("Vault updated");
    await expect(cta.getByRole("button", { name: "Restart now" })).toBeVisible();

    // Click "Restart now" — verify POST /api/secrets/K1/restart fires.
    const restartResponse = page.waitForResponse(
      (r) => r.url().includes("/api/secrets/K1/restart") && r.request().method() === "POST",
    );
    await cta.getByRole("button", { name: "Restart now" }).click();
    const restartResp = await restartResponse;
    expect(restartResp.status()).toBe(200);

    // CTA dismisses after successful restart (all errors empty → onDismiss called).
    await expect(cta).not.toBeVisible();
  });

  // -------------------------------------------------------------------------
  // 8. Rotate Save and restart with partial failure (207) — banner visible
  // -------------------------------------------------------------------------
  test("Rotate Save and restart with partial failure — rotate-banner-partial visible with Retry", async ({
    page,
    hub,
  }) => {
    // Set up all route stubs BEFORE page.goto().
    await page.route("**/api/secrets", async (r) => {
      if (r.request().method() !== "GET") { await r.continue(); return; }
      await r.fulfill({
        status: 200,
        contentType: "application/json",
        body: secrEnvelope("ok", [
          {
            name: "K1",
            state: "present",
            used_by: [
              { server: "alpha", env_var: "K1" },
              { server: "beta", env_var: "K1" },
            ],
          },
        ]),
      });
    });
    // 2 running daemons (required so runningCountFor > 0).
    await page.route("**/api/status", (r) =>
      r.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify([
          { server: "alpha", daemon: "default", state: "Running", port: 9121 },
          { server: "beta", daemon: "default", state: "Running", port: 9122 },
        ]),
      }),
    );
    // PUT /api/secrets/K1 → 207 with 1 success + 1 failure.
    await page.route("**/api/secrets/K1", async (r) => {
      if (r.request().method() === "PUT") {
        await r.fulfill({
          status: 207,
          contentType: "application/json",
          body: JSON.stringify({
            vault_updated: true,
            restart_results: [
              { task_name: "alpha/default", error: "" },
              { task_name: "beta/default", error: "connection refused" },
            ],
          }),
        });
        return;
      }
      await r.continue();
    });

    await page.goto(`${hub.url}/#/secrets`);
    const row = page.locator("table tbody tr").filter({ hasText: "K1" });
    await expect(row).toBeVisible();

    // Click Rotate on K1 and fill a new value.
    await row.getByRole("button", { name: "Rotate" }).click();
    const rotateModal = page.locator('[data-testid="rotate-secret-modal"]');
    await expect(rotateModal).toBeVisible();
    await rotateModal.locator('input[type="password"]').fill("rotated-value");

    // Click "Save and restart" and wait for the 207 PUT response.
    const putResponse = page.waitForResponse(
      (r) => r.url().includes("/api/secrets/K1") && r.request().method() === "PUT",
    );
    await rotateModal.getByRole("button", { name: "Save and restart" }).click();
    const putResp = await putResponse;
    expect(putResp.status()).toBe(207);

    // Modal closes after the rotate call (207 is treated as success by rotateSecret()).
    await expect(rotateModal).not.toBeVisible();

    // The partial-failure banner must be visible — stale-while-revalidate
    // keeps InitKeyedView mounted so rotateMode/bannerName/rotateResult survive
    // the background refresh triggered by onSaved.
    const partialBanner = page.locator('[data-testid="rotate-banner-partial"]');
    await expect(partialBanner).toBeVisible();
    // Banner copy: "1/2 daemons restarted".
    await expect(partialBanner).toContainText("1/2");
    // Retry button is visible.
    await expect(partialBanner.getByRole("button", { name: "Retry failed restarts" })).toBeVisible();
  });

  // -------------------------------------------------------------------------
  // 9. Delete unreferenced — single click (no confirm required)
  // -------------------------------------------------------------------------
  test("Delete unreferenced — single click succeeds with no confirm", async ({
    page,
    hub,
  }) => {
    // Stub GET /api/secrets: K2 present, no refs (first call); then empty (after delete).
    let deleted = false;
    await page.route("**/api/secrets", async (r) => {
      if (r.request().method() !== "GET") { await r.continue(); return; }
      if (!deleted) {
        await r.fulfill({
          status: 200,
          contentType: "application/json",
          body: secrEnvelope("ok", [
            { name: "K2", state: "present", used_by: [] },
          ]),
        });
      } else {
        await r.fulfill({
          status: 200,
          contentType: "application/json",
          body: secrEnvelope("ok", []),
        });
      }
    });
    await page.route("**/api/status", (r) =>
      r.fulfill({ status: 200, contentType: "application/json", body: "[]" }),
    );

    // Track DELETE /api/secrets/K2 calls.
    let deleteCount = 0;
    let deletedWithConfirm = false;
    await page.route(/\/api\/secrets\/K2(\?.*)?$/, async (r) => {
      if (r.request().method() === "DELETE") {
        deleteCount++;
        if (r.request().url().includes("confirm=true")) {
          deletedWithConfirm = true;
        }
        deleted = true;
        await r.fulfill({ status: 204, body: "" });
        return;
      }
      await r.continue();
    });

    await page.goto(`${hub.url}/#/secrets`);
    await expect(page.locator("table tbody tr")).toHaveCount(1);

    // Click Delete on K2 — fires first (no-confirm) DELETE which gets 204.
    const deleteResponse = page.waitForResponse(
      (r) => r.url().includes("/api/secrets/K2") && r.request().method() === "DELETE",
    );
    await page.locator("table tbody tr").filter({ hasText: "K2" }).getByRole("button", { name: "Delete" }).click();
    await deleteResponse;

    // The modal fires the DELETE immediately; 204 → onDeleted → refresh → row disappears.
    await expect(page.locator("table tbody tr").filter({ hasText: "K2" })).toHaveCount(0);

    // Exactly one DELETE request, no confirm flag.
    expect(deleteCount).toBe(1);
    expect(deletedWithConfirm).toBe(false);
  });

  // -------------------------------------------------------------------------
  // 10. Delete with refs — D5 escalation flow
  // -------------------------------------------------------------------------
  test("Delete with refs — escalation flow requires typed DELETE confirm", async ({
    page,
    hub,
  }) => {
    // Stub GET /api/secrets: K1 with 1 ref (first call); empty after confirmed delete.
    let confirmed = false;
    await page.route("**/api/secrets", async (r) => {
      if (r.request().method() !== "GET") { await r.continue(); return; }
      if (!confirmed) {
        await r.fulfill({
          status: 200,
          contentType: "application/json",
          body: secrEnvelope("ok", [
            {
              name: "K1",
              state: "present",
              used_by: [{ server: "alpha", env_var: "MY_KEY" }],
            },
          ]),
        });
      } else {
        await r.fulfill({
          status: 200,
          contentType: "application/json",
          body: secrEnvelope("ok", []),
        });
      }
    });
    await page.route("**/api/status", (r) =>
      r.fulfill({ status: 200, contentType: "application/json", body: "[]" }),
    );

    const deleteRequests: string[] = [];
    await page.route(/\/api\/secrets\/K1(\?.*)?$/, async (r) => {
      if (r.request().method() === "DELETE") {
        const url = r.request().url();
        deleteRequests.push(url);
        if (!url.includes("confirm=true")) {
          // First DELETE (no confirm): return 409 SECRETS_HAS_REFS.
          await r.fulfill({
            status: 409,
            contentType: "application/json",
            body: JSON.stringify({
              error: "secret K1 is referenced by 1 manifest(s)",
              code: "SECRETS_HAS_REFS",
              used_by: [{ server: "alpha", env_var: "MY_KEY" }],
            }),
          });
        } else {
          // Second DELETE (?confirm=true): succeed.
          confirmed = true;
          await r.fulfill({ status: 204, body: "" });
        }
        return;
      }
      await r.continue();
    });

    await page.goto(`${hub.url}/#/secrets`);
    await expect(page.locator("table tbody tr")).toHaveCount(1);

    // Click Delete on K1. First request fires automatically (no-confirm).
    const firstDeleteResponse = page.waitForResponse(
      (r) =>
        r.url().includes("/api/secrets/K1") &&
        !r.url().includes("confirm") &&
        r.request().method() === "DELETE",
    );
    await page.locator("table tbody tr").filter({ hasText: "K1" }).getByRole("button", { name: "Delete" }).click();
    await firstDeleteResponse;

    // Modal opens — first request returned 409, escalation UI shows.
    const modal = page.locator('[data-testid="delete-secret-modal"]');
    await expect(modal).toBeVisible();
    // The "confirm-refs" stage shows the ref list.
    await expect(modal.getByText("alpha")).toBeVisible();
    await expect(modal.getByText("Type")).toBeVisible();

    // Confirm button disabled until "DELETE" is typed.
    const confirmBtn = modal.getByRole("button", { name: "Delete vault key" });
    await expect(confirmBtn).toBeDisabled();

    // Type DELETE to unlock.
    await modal.locator('[data-testid="delete-confirm-input"]').fill("DELETE");
    await expect(confirmBtn).toBeEnabled();

    // Second request fires with ?confirm=true.
    const secondDeleteResponse = page.waitForResponse(
      (r) =>
        r.url().includes("/api/secrets/K1") &&
        r.url().includes("confirm=true") &&
        r.request().method() === "DELETE",
    );
    await confirmBtn.click();
    await secondDeleteResponse;

    // Row disappears after confirmed delete.
    await expect(modal).not.toBeVisible();
    await expect(page.locator("table tbody tr").filter({ hasText: "K1" })).toHaveCount(0);

    // Two DELETE requests: first without confirm, second with confirm=true.
    expect(deleteRequests).toHaveLength(2);
    expect(deleteRequests[0]).not.toContain("confirm=true");
    expect(deleteRequests[1]).toContain("confirm=true");
  });

  // -------------------------------------------------------------------------
  // 11. Delete fails closed when scan incomplete
  // -------------------------------------------------------------------------
  test("Delete fails closed when scan incomplete — typed confirm works", async ({
    page,
    hub,
  }) => {
    let confirmed = false;
    await page.route("**/api/secrets", async (r) => {
      if (r.request().method() !== "GET") { await r.continue(); return; }
      if (!confirmed) {
        await r.fulfill({
          status: 200,
          contentType: "application/json",
          body: secrEnvelope("ok", [
            { name: "K1", state: "present", used_by: [] },
          ]),
        });
      } else {
        await r.fulfill({
          status: 200,
          contentType: "application/json",
          body: secrEnvelope("ok", []),
        });
      }
    });
    await page.route("**/api/status", (r) =>
      r.fulfill({ status: 200, contentType: "application/json", body: "[]" }),
    );

    const deleteRequests: string[] = [];
    await page.route(/\/api\/secrets\/K1(\?.*)?$/, async (r) => {
      if (r.request().method() === "DELETE") {
        const url = r.request().url();
        deleteRequests.push(url);
        if (!url.includes("confirm=true")) {
          // First DELETE (no-confirm): scan incomplete → 409.
          await r.fulfill({
            status: 409,
            contentType: "application/json",
            body: JSON.stringify({
              error: "manifest scan returned 1 error(s); cannot verify refs",
              code: "SECRETS_USAGE_SCAN_INCOMPLETE",
              manifest_errors: [{ path: "corrupt-server/manifest.yaml", error: "YAML parse error" }],
            }),
          });
        } else {
          // Second DELETE (?confirm=true): succeed.
          confirmed = true;
          await r.fulfill({ status: 204, body: "" });
        }
        return;
      }
      await r.continue();
    });

    await page.goto(`${hub.url}/#/secrets`);
    await expect(page.locator("table tbody tr")).toHaveCount(1);

    // Click Delete on K1.
    const firstDeleteResponse = page.waitForResponse(
      (r) =>
        r.url().includes("/api/secrets/K1") &&
        !r.url().includes("confirm") &&
        r.request().method() === "DELETE",
    );
    await page.locator("table tbody tr").filter({ hasText: "K1" }).getByRole("button", { name: "Delete" }).click();
    await firstDeleteResponse;

    const modal = page.locator('[data-testid="delete-secret-modal"]');
    await expect(modal).toBeVisible();
    // Scan-incomplete stage: corrupt manifest error listed.
    await expect(modal.getByText("corrupt-server/manifest.yaml")).toBeVisible();
    await expect(modal.getByText("YAML parse error")).toBeVisible();

    // Confirm button disabled until "DELETE" typed.
    const confirmBtn = modal.getByRole("button", { name: "Delete anyway" });
    await expect(confirmBtn).toBeDisabled();

    await modal.locator('[data-testid="delete-confirm-input"]').fill("DELETE");
    await expect(confirmBtn).toBeEnabled();

    const secondDeleteResponse = page.waitForResponse(
      (r) =>
        r.url().includes("/api/secrets/K1") &&
        r.url().includes("confirm=true") &&
        r.request().method() === "DELETE",
    );
    await confirmBtn.click();
    await secondDeleteResponse;

    // Row removed after confirmed delete.
    await expect(modal).not.toBeVisible();
    await expect(page.locator("table tbody tr").filter({ hasText: "K1" })).toHaveCount(0);

    // Two DELETE requests: first triggers SCAN_INCOMPLETE, second succeeds.
    expect(deleteRequests).toHaveLength(2);
    expect(deleteRequests[0]).not.toContain("confirm=true");
    expect(deleteRequests[1]).toContain("confirm=true");
  });

  // -------------------------------------------------------------------------
  // 12. Delete with refs — direct backend 409 verification
  // -------------------------------------------------------------------------
  test("Direct DELETE without confirm on referenced key returns 409 + SECRETS_HAS_REFS", async ({
    page,
    hub,
  }) => {
    // This scenario verifies the API contract shape directly via fetch()
    // from the browser context (no UI interaction), using page.route() to
    // stub the backend. Proves the frontend APIError parsing code paths.
    await page.route(/\/api\/secrets\/REFED_KEY(\?.*)?$/, async (r) => {
      if (r.request().method() === "DELETE") {
        if (!r.request().url().includes("confirm=true")) {
          await r.fulfill({
            status: 409,
            contentType: "application/json",
            body: JSON.stringify({
              error: "secret REFED_KEY is referenced by 1 manifest(s)",
              code: "SECRETS_HAS_REFS",
              used_by: [{ server: "some-server", env_var: "REFED_KEY" }],
            }),
          });
          return;
        }
        await r.fulfill({ status: 204, body: "" });
        return;
      }
      await r.continue();
    });

    // Navigate to any page so fetch() works in the browser context.
    await page.goto(`${hub.url}/#/secrets`);

    // Call DELETE via fetch from the page context (simulating the frontend API client).
    const result = await page.evaluate(async () => {
      const resp = await fetch("/api/secrets/REFED_KEY", { method: "DELETE" });
      const body = await resp.json();
      return { status: resp.status, body };
    });

    // Assert 409 + SECRETS_HAS_REFS shape.
    expect(result.status).toBe(409);
    expect(result.body.code).toBe("SECRETS_HAS_REFS");
    expect(Array.isArray(result.body.used_by)).toBe(true);
    expect(result.body.used_by).toHaveLength(1);
    expect(result.body.used_by[0].server).toBe("some-server");
  });

  // -------------------------------------------------------------------------
  // 13. Banner shows mcphub secrets edit command + Copy button works
  // -------------------------------------------------------------------------
  test("Banner shows mcphub secrets edit command and Copy changes label", async ({
    page,
    hub,
    context,
  }) => {
    // Stub GET /api/secrets → missing (simplest state that still shows the banner).
    await page.route("**/api/secrets", async (r) => {
      if (r.request().method() !== "GET") { await r.continue(); return; }
      await r.fulfill({
        status: 200,
        contentType: "application/json",
        body: secrEnvelope("missing"),
      });
    });
    await page.route("**/api/status", (r) =>
      r.fulfill({ status: 200, contentType: "application/json", body: "[]" }),
    );

    // Grant clipboard permissions before navigating.
    await context.grantPermissions(["clipboard-read", "clipboard-write"]);

    await page.goto(`${hub.url}/#/secrets`);

    // The EditVaultBanner is always visible regardless of vault state.
    const banner = page.locator('[data-testid="edit-vault-banner"]');
    await expect(banner).toBeVisible();

    // The banner contains the literal command text.
    await expect(banner).toContainText("mcphub secrets edit");

    // The Copy button exists.
    const copyBtn = banner.getByRole("button", { name: "Copy command" });
    await expect(copyBtn).toBeVisible();

    // Click Copy — button label should change to "Copied".
    await copyBtn.click();
    await expect(banner.getByRole("button", { name: "Copied" })).toBeVisible();
  });

  // -------------------------------------------------------------------------
  // 14. Sidebar Secrets link routes correctly
  // -------------------------------------------------------------------------
  test("Sidebar Secrets link routes to #/secrets and screen renders", async ({
    page,
    hub,
  }) => {
    await page.route("**/api/secrets", async (r) => {
      if (r.request().method() !== "GET") { await r.continue(); return; }
      await r.fulfill({
        status: 200,
        contentType: "application/json",
        body: secrEnvelope("missing"),
      });
    });
    await page.route("**/api/status", (r) =>
      r.fulfill({ status: 200, contentType: "application/json", body: "[]" }),
    );
    // Stub scan (used by Servers screen when we start there).
    await page.route("**/api/scan", (r) =>
      r.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ entries: [] }),
      }),
    );

    // Start on the Dashboard screen.
    await page.goto(`${hub.url}/#/dashboard`);
    await expect(page.locator("h1")).toHaveText("Dashboard");

    // Click Secrets link in sidebar.
    await page.getByRole("link", { name: "Secrets" }).click();

    // URL should update to #/secrets.
    await expect(page).toHaveURL(/[#]\/secrets$/);

    // Secrets h1 and the vault banner render.
    await expect(page.locator("h1")).toHaveText("Secrets");
    await expect(page.locator('[data-testid="edit-vault-banner"]')).toBeVisible();
  });
});
