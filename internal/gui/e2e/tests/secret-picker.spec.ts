// internal/gui/e2e/tests/secret-picker.spec.ts
//
// 10 E2E scenarios for the A3-b SecretPicker affordance embedded in the
// AddServer / EditServer env-variable rows.
//
// Pattern notes (mirrors edit-server.spec.ts):
// - seedManifest writes <name>/manifest.yaml into e2e/bin/servers/ so the
//   GUI binary can serve it via /api/manifest/get?name=<name>. Each test
//   uses a unique name prefix to avoid parallel-worker collisions.
// - initVaultAndAdd calls the real server API to initialize the vault and
//   add secrets into the per-test HOME (LOCALAPPDATA=hub.home on Windows).
// - Vault is at <hub.home>/mcp-local-hub/secrets.age (Windows) or
//   $XDG_DATA_HOME/mcp-local-hub/secrets.age (Linux). Tests that need a
//   degraded vault stub /api/secrets via page.route() instead of corrupting
//   the file to avoid platform path ambiguity.
// - The Environment accordion starts collapsed; tests must click it open
//   before interacting with env rows.

import { test, expect } from "../fixtures/hub";
import * as fs from "node:fs";
import * as path from "node:path";
import { resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = dirname(fileURLToPath(import.meta.url));

// serversDir matches what defaultManifestDir() returns in the E2E env.
// The binary lives at e2e/bin/mcphub.exe; sibling check wins first.
const serversDir = resolve(__dirname, "..", "bin", "servers");

// seedManifest writes <name>/manifest.yaml into e2e/bin/servers/ so the
// GUI binary can serve it via /api/manifest/get?name=<name>.
function seedManifest(name: string, yaml: string): void {
  const dir = path.join(serversDir, name);
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, "manifest.yaml"), yaml, "utf-8");
}

// initVaultAndAdd calls the real server API to initialize the vault (POST
// /api/secrets/init) and then add each secret (POST /api/secrets). The vault
// lands in hub.home because the fixture sets LOCALAPPDATA=hub.home (Windows)
// and XDG_DATA_HOME=hub.home (Linux).
async function initVaultAndAdd(
  baseURL: string,
  secrets: { name: string; value: string }[],
): Promise<void> {
  const initResp = await fetch(new URL("/api/secrets/init", baseURL).toString(), {
    method: "POST",
  });
  if (!initResp.ok) {
    throw new Error(`initVault: POST /api/secrets/init returned ${initResp.status}`);
  }
  for (const s of secrets) {
    const addResp = await fetch(new URL("/api/secrets", baseURL).toString(), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(s),
    });
    if (!addResp.ok) {
      const body = await addResp.text().catch(() => "");
      throw new Error(`initVault: POST /api/secrets returned ${addResp.status}: ${body}`);
    }
  }
}

test.describe("A3-b SecretPicker E2E", () => {
  // -------------------------------------------------------------------------
  // 1. Empty form, vault empty — only '+ Create new secret…'
  // -------------------------------------------------------------------------
  test("scenario 1: empty form, vault empty — only '+ Create new secret…'", async ({
    page,
    hub,
  }) => {
    // Stub /api/secrets to return an empty vault so ghost-ref keys from other
    // tests' seeded manifests do not pollute the dropdown count.
    await page.route("**/api/secrets", async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ vault_state: "ok", secrets: [], manifest_errors: [] }),
        });
      } else {
        await route.continue();
      }
    });

    await page.goto(hub.url + "/#/add-server");
    // Open the Environment accordion before interacting.
    await page.locator(".accordion-header", { hasText: "Environment" }).click();
    await page.click('[data-action="add-env"]');
    await page.fill('[data-env-row="0"] input[placeholder="KEY"]', "API_KEY");
    await page.click('[data-env-row="0"] [aria-label="Pick secret"]');

    const listbox = page.getByRole("listbox");
    await expect(listbox).toBeVisible();
    // Generic create entry must be visible.
    await expect(listbox.locator('[data-action="create-new-secret"]')).toBeVisible();
    // No match badge because vault is empty.
    await expect(listbox.locator(".match-badge")).toHaveCount(0);
  });

  // -------------------------------------------------------------------------
  // 2. Vault seeded with two unrelated keys and one exact-match key — match
  //    badge present + matching key sorts before the unrelated ones.
  // -------------------------------------------------------------------------
  test("scenario 2: vault seeded with two unrelated keys; matching key added — sort + badge", async ({
    page,
    hub,
  }) => {
    // Stub /api/secrets to return exactly three controlled keys so other tests'
    // seeded manifests do not inject ghost-ref keys that change counts.
    const secretsBody = JSON.stringify({
      vault_state: "ok",
      secrets: [
        { name: "wolfram_app_id",  state: "present", used_by: [] },
        { name: "unpaywall_email", state: "present", used_by: [] },
        { name: "openai_api_key",  state: "present", used_by: [] },
      ],
      manifest_errors: [],
    });
    await page.route("**/api/secrets", async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({ status: 200, contentType: "application/json", body: secretsBody });
      } else {
        await route.continue();
      }
    });

    await page.goto(hub.url + "/#/add-server");
    await page.locator(".accordion-header", { hasText: "Environment" }).click();
    await page.click('[data-action="add-env"]');
    await page.fill('[data-env-row="0"] input[placeholder="KEY"]', "OPENAI_API_KEY");
    await page.click('[data-env-row="0"] [aria-label="Pick secret"]');

    // Three items total (two unrelated + one matching).
    const listbox = page.getByRole("listbox");
    await expect(listbox).toBeVisible();
    await expect(listbox.locator('[data-action="key"]')).toHaveCount(3);

    // The match badge must be present for the openai key.
    await expect(listbox.locator(".match-badge")).toHaveCount(1);

    // Matching key must be sorted first.
    const firstItem = listbox.locator('[data-action="key"]').first();
    await expect(firstItem).toContainText("openai_api_key");
  });

  // -------------------------------------------------------------------------
  // 3. Auto-open via 'secret:' typing + filter narrowing + Enter to commit
  // -------------------------------------------------------------------------
  test("scenario 3: auto-open via 'secret:' typing + filter narrowing", async ({
    page,
    hub,
  }) => {
    // Stub /api/secrets to return exactly three controlled keys so other tests'
    // seeded manifests do not inject ghost-ref keys that change counts.
    const secretsBody = JSON.stringify({
      vault_state: "ok",
      secrets: [
        { name: "wolfram_app_id",  state: "present", used_by: [] },
        { name: "openai_api_key",  state: "present", used_by: [] },
        { name: "unpaywall_email", state: "present", used_by: [] },
      ],
      manifest_errors: [],
    });
    await page.route("**/api/secrets", async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({ status: 200, contentType: "application/json", body: secretsBody });
      } else {
        await route.continue();
      }
    });

    await page.goto(hub.url + "/#/add-server");
    await page.locator(".accordion-header", { hasText: "Environment" }).click();
    await page.click('[data-action="add-env"]');

    const valueInput = page.locator('[data-env-row="0"] [role="combobox"]');
    await valueInput.click();
    await valueInput.type("secret:");
    // Dropdown opens automatically on 'secret:' prefix.
    const listbox = page.getByRole("listbox");
    await expect(listbox).toBeVisible();
    // All three keys visible.
    await expect(listbox.locator('[data-action="key"]')).toHaveCount(3);

    // Type 'wolf' to narrow to wolfram_app_id.
    await valueInput.type("wolf");
    await expect(listbox.locator('[data-action="key"]')).toHaveCount(1);
    await expect(listbox.locator('[data-action="key"]').first()).toContainText("wolfram_app_id");

    // Enter commits the selection.
    await page.keyboard.press("Enter");
    await expect(valueInput).toHaveValue("secret:wolfram_app_id");
    await expect(listbox).not.toBeVisible();
  });

  // -------------------------------------------------------------------------
  // 4. EditServer with one broken ref — red combobox + 'Currently referenced'
  //    section with missing badge + 'Create ... in vault' entry
  // -------------------------------------------------------------------------
  test("scenario 4: EditServer broken ref — red border + Currently-referenced section", async ({
    page,
    hub,
  }) => {
    const name = "e2e-picker-s4-broken";
    seedManifest(
      name,
      `name: ${name}
kind: global
transport: stdio-bridge
command: noop
env:
  API_KEY: secret:never_added
daemons:
  - name: default
    port: 9100
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`,
    );
    await initVaultAndAdd(hub.url, [{ name: "unrelated", value: "x" }]);
    await page.goto(hub.url + `/#/edit-server?name=${name}`);

    // Open Environment accordion so env rows render.
    await page.locator(".accordion-header", { hasText: "Environment" }).click();
    await page.waitForSelector('[data-env-row="0"]');

    // Input must carry the 'broken' class (ref is missing from vault).
    await expect(
      page.locator('[data-env-row="0"] [role="combobox"].broken'),
    ).toBeVisible();

    // Open picker — should show 'Currently referenced' section.
    await page.click('[data-env-row="0"] [aria-label="Pick secret"]');
    await expect(page.getByText("Currently referenced")).toBeVisible();
    await expect(page.locator(".missing-badge")).toBeVisible();
    // 'Create ... in vault' contextual entry.
    await expect(page.locator('[data-action="create-contextual"]')).toBeVisible();
    await expect(page.locator('[data-action="create-contextual"]')).toContainText("never_added");
  });

  // -------------------------------------------------------------------------
  // 5. Multiple broken refs (count=3) — compact summary line visible after
  //    opening the Environment accordion.
  // -------------------------------------------------------------------------
  test("scenario 5: multiple broken refs (count=3) — compact summary line", async ({
    page,
    hub,
  }) => {
    const name = "e2e-picker-s5-multi";
    seedManifest(
      name,
      `name: ${name}
kind: global
transport: stdio-bridge
command: noop
env:
  A_KEY: secret:never_added
  B_KEY: secret:also_missing
  C_KEY: secret:old_token
daemons:
  - name: default
    port: 9101
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`,
    );
    // Vault initialized but none of those 3 keys added.
    await initVaultAndAdd(hub.url, []);
    await page.goto(hub.url + `/#/edit-server?name=${name}`);
    await page.locator(".accordion-header", { hasText: "Environment" }).click();
    await page.waitForSelector('[data-env-row="0"]');

    const summary = page.locator(".secret-broken-summary");
    await expect(summary).toContainText("3 secrets referenced but not in vault");
    await expect(summary).toContainText("never_added");
    await expect(summary).toContainText("also_missing");
    await expect(summary).toContainText("old_token");
  });

  // -------------------------------------------------------------------------
  // 6. Create flow happy path — modal save + snapshot refresh + marker
  //    disappears + form dirty state unchanged
  // -------------------------------------------------------------------------
  test("scenario 6: create flow happy path — modal save + snapshot refresh + marker disappears", async ({
    page,
    hub,
  }) => {
    const name = "e2e-picker-s6-create";
    seedManifest(
      name,
      `name: ${name}
kind: global
transport: stdio-bridge
command: noop
env:
  API_KEY: secret:never_added
daemons:
  - name: default
    port: 9102
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`,
    );
    await initVaultAndAdd(hub.url, []);
    await page.goto(hub.url + `/#/edit-server?name=${name}`);
    await page.locator(".accordion-header", { hasText: "Environment" }).click();
    await page.waitForSelector('[data-env-row="0"]');

    // Capture save button text before any modal interaction.
    const saveButton = page.locator('[data-action="save"]');
    const initialSaveText = await saveButton.textContent();

    // Open picker and trigger contextual create.
    await page.click('[data-env-row="0"] [aria-label="Pick secret"]');
    await page.click('[data-action="create-contextual"]');

    // Modal opens pre-filled with 'never_added'.
    const modal = page.locator('[data-testid="add-secret-modal"]');
    await expect(modal).toBeVisible();
    await modal.locator('input[type="password"]').fill("the-secret-value");
    await modal.locator('button[type="submit"]').click();

    // Modal closes after save.
    await expect(modal).not.toBeVisible({ timeout: 8_000 });

    // Reopen picker — the 'missing-badge' should be gone (snapshot refreshed).
    await page.click('[data-env-row="0"] [aria-label="Pick secret"]');
    await expect(page.locator(".missing-badge")).toHaveCount(0, { timeout: 5_000 });
    await expect(
      page.locator('[data-action="key"]').filter({ hasText: "never_added" }),
    ).toBeVisible();

    // Save button text must be unchanged — form dirty state not altered.
    const finalSaveText = await saveButton.textContent();
    expect(finalSaveText).toBe(initialSaveText);
  });

  // -------------------------------------------------------------------------
  // 7. Create 409 conflict via contextual race — modal stays open + error
  //    shown + Cancel triggers refresh + marker clears
  // -------------------------------------------------------------------------
  test("scenario 7: create 409 conflict — modal stays open, Cancel triggers refresh + marker clears", async ({
    page,
    hub,
  }) => {
    const name = "e2e-picker-s7-409";
    seedManifest(
      name,
      `name: ${name}
kind: global
transport: stdio-bridge
command: noop
env:
  API_KEY: secret:never_added
daemons:
  - name: default
    port: 9103
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`,
    );
    await initVaultAndAdd(hub.url, []);
    await page.goto(hub.url + `/#/edit-server?name=${name}`);
    await page.locator(".accordion-header", { hasText: "Environment" }).click();
    await page.waitForSelector('[data-env-row="0"]');
    await expect(
      page.locator('[data-env-row="0"] [role="combobox"].broken'),
    ).toBeVisible();

    let postReceived = false;
    await page.route("**/api/secrets", async (route, request) => {
      if (request.method() === "POST") {
        postReceived = true;
        await route.fulfill({
          status: 409,
          contentType: "application/json",
          body: JSON.stringify({
            error: "secret already exists",
            code: "SECRETS_DUPLICATE",
          }),
        });
      } else if (request.method() === "GET" && postReceived) {
        // After the 409, Cancel will trigger a refresh; return the secret
        // as now present so the broken marker clears.
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
            vault_state: "ok",
            secrets: [{ name: "never_added", state: "present", used_by: [] }],
            manifest_errors: [],
          }),
        });
      } else {
        await route.continue();
      }
    });

    await page.click('[data-env-row="0"] [aria-label="Pick secret"]');
    await page.click('[data-action="create-contextual"]');

    const modal = page.locator('[data-testid="add-secret-modal"]');
    await expect(modal).toBeVisible();
    await modal.locator('input[type="password"]').fill("v");
    await modal.locator('button[type="submit"]').click();

    // Modal stays open because of the 409.
    await expect(modal).toBeVisible();
    await expect(page.getByText("secret already exists")).toBeVisible();

    // Cancel closes the modal. The component refreshes on close, returning
    // the stubbed "present" response, so broken marker must clear.
    await modal.locator('button:has-text("Cancel")').click();
    await expect(modal).not.toBeVisible();
    await expect(
      page.locator('[data-env-row="0"] [role="combobox"].broken'),
    ).not.toBeVisible({ timeout: 5_000 });
  });

  // -------------------------------------------------------------------------
  // 8. Vault decrypt_failed — yellow 'unverified' markers, disabled Create,
  //    summary message with 'Vault not readable'
  // -------------------------------------------------------------------------
  test("scenario 8: vault decrypt_failed — yellow markers, disabled Create, summary message", async ({
    page,
    hub,
  }) => {
    const name = "e2e-picker-s8-decrypt";
    seedManifest(
      name,
      `name: ${name}
kind: global
transport: stdio-bridge
command: noop
env:
  API_KEY: secret:foo
daemons:
  - name: default
    port: 9104
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`,
    );
    // Stub /api/secrets to return decrypt_failed before navigation.
    await page.route("**/api/secrets", async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
            vault_state: "decrypt_failed",
            secrets: [],
            manifest_errors: [],
          }),
        });
      } else {
        await route.continue();
      }
    });

    await page.goto(hub.url + `/#/edit-server?name=${name}`);
    await page.locator(".accordion-header", { hasText: "Environment" }).click();
    await page.waitForSelector('[data-env-row="0"]');

    // Input carries 'unverified' class (vault unreachable).
    await expect(
      page.locator('[data-env-row="0"] [role="combobox"].unverified'),
    ).toBeVisible();

    // Summary line mentions vault not readable.
    await expect(page.locator(".secret-broken-summary")).toContainText("Vault not readable");

    // Open picker — disabled create entry must be present, modal must NOT open.
    await page.click('[data-env-row="0"] [aria-label="Pick secret"]');
    await expect(page.locator('[data-action="create-disabled"]')).toBeVisible();
    // Use force:true because the element carries aria-disabled="true".
    // The intent of the test is to verify the modal does NOT open on interaction.
    await page.click('[data-action="create-disabled"]', { force: true });
    await expect(page.locator('[data-testid="add-secret-modal"]')).not.toBeVisible();
  });

  // -------------------------------------------------------------------------
  // 9. Editing prefix only — no broken marker; continued typing narrows list
  // -------------------------------------------------------------------------
  test("scenario 9: editing prefix only — no broken marker; continued typing narrows", async ({
    page,
    hub,
  }) => {
    // Stub /api/secrets to return exactly three controlled keys so other tests'
    // seeded manifests do not inject ghost-ref keys that change counts.
    const secretsBody = JSON.stringify({
      vault_state: "ok",
      secrets: [
        { name: "foo",  state: "present", used_by: [] },
        { name: "fizz", state: "present", used_by: [] },
        { name: "bar",  state: "present", used_by: [] },
      ],
      manifest_errors: [],
    });
    await page.route("**/api/secrets", async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({ status: 200, contentType: "application/json", body: secretsBody });
      } else {
        await route.continue();
      }
    });

    await page.goto(hub.url + "/#/add-server");
    await page.locator(".accordion-header", { hasText: "Environment" }).click();
    await page.click('[data-action="add-env"]');

    const valueInput = page.locator('[data-env-row="0"] [role="combobox"]');
    await valueInput.click();
    await valueInput.type("secret:");

    // Dropdown opens; no broken/unverified marker while editing a query.
    await expect(valueInput).not.toHaveClass(/broken|unverified/);
    const listbox = page.getByRole("listbox");
    await expect(listbox).toBeVisible();
    // All 3 available keys shown.
    await expect(listbox.locator('[data-action="key"]')).toHaveCount(3);

    // Narrow to 'f' prefix → foo + fizz.
    await valueInput.type("f");
    await expect(listbox.locator('[data-action="key"]')).toHaveCount(2);
    await expect(valueInput).not.toHaveClass(/broken|unverified/);
  });

  // -------------------------------------------------------------------------
  // 10. Save success but post-save refresh 503 — picker transitions to
  //     unverified + Retry entry; after retry-route succeeds, marker clears.
  // -------------------------------------------------------------------------
  test("scenario 10: save success but refresh fails — picker transitions to unverified + Retry", async ({
    page,
    hub,
  }) => {
    const name = "e2e-picker-s10-refresh-fail";
    seedManifest(
      name,
      `name: ${name}
kind: global
transport: stdio-bridge
command: noop
env:
  API_KEY: secret:never_added
daemons:
  - name: default
    port: 9105
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`,
    );
    await initVaultAndAdd(hub.url, []);
    await page.goto(hub.url + `/#/edit-server?name=${name}`);
    await page.locator(".accordion-header", { hasText: "Environment" }).click();
    await page.waitForSelector('[data-env-row="0"]');

    let saveReceived = false;
    await page.route("**/api/secrets", async (route, request) => {
      if (request.method() === "POST") {
        saveReceived = true;
        await route.fulfill({
          status: 201,
          contentType: "application/json",
          body: "{}",
        });
      } else if (request.method() === "GET" && saveReceived) {
        // POST succeeded but the immediate refresh fails.
        await route.fulfill({
          status: 503,
          contentType: "application/json",
          body: JSON.stringify({ error: "down" }),
        });
      } else {
        await route.continue();
      }
    });

    await page.click('[data-env-row="0"] [aria-label="Pick secret"]');
    await page.click('[data-action="create-contextual"]');
    const modal = page.locator('[data-testid="add-secret-modal"]');
    await expect(modal).toBeVisible();
    await modal.locator('input[type="password"]').fill("v");
    await modal.locator('button[type="submit"]').click();
    // Modal closes because POST succeeded.
    await expect(modal).not.toBeVisible({ timeout: 8_000 });

    // After failed refresh the input transitions to 'unverified'.
    await expect(
      page.locator('[data-env-row="0"] [role="combobox"].unverified'),
    ).toBeVisible({ timeout: 5_000 });

    // Open picker — Retry entry must be shown.
    await page.click('[data-env-row="0"] [aria-label="Pick secret"]');
    await expect(page.locator('[data-action="retry-load-vault"]')).toBeVisible();

    // Replace route with a successful response.
    await page.unroute("**/api/secrets");
    await page.route("**/api/secrets", async (route, request) => {
      if (request.method() === "GET") {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
            vault_state: "ok",
            secrets: [{ name: "never_added", state: "present", used_by: [] }],
            manifest_errors: [],
          }),
        });
      } else {
        await route.continue();
      }
    });
    await page.click('[data-action="retry-load-vault"]');

    // After successful retry the 'unverified' marker must clear.
    await expect(
      page.locator('[data-env-row="0"] [role="combobox"].unverified'),
    ).not.toBeVisible({ timeout: 5_000 });
    await expect(
      page.locator('[data-env-row="0"] [role="combobox"].broken'),
    ).not.toBeVisible();
  });
});
