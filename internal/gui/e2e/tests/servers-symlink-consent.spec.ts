// internal/gui/e2e/tests/servers-symlink-consent.spec.ts
//
// E2E for the A3 PR-2 "Resolve symlink → write to real target" affordance on a
// `config-error-symlink` Servers-matrix cell. The whole flow is driven via
// page.route() stubs (mirrors catalog-readiness.spec.ts / servers.spec.ts) so
// it is DETERMINISTIC and runnable on the Windows CI runner WITHOUT creating a
// real symlink (symlink creation needs elevation on Windows). The production
// resolve/write pipeline is exercised by the Go unit tests
// (internal/api/client_write_consent_surface_test.go +
// internal/gui/resolve_symlink_write_test.go); this spec proves the GUI WIRING:
// the affordance renders on a symlink cell, the confirm modal shows the PINNED
// real path, and confirming POSTs the two-phase resolve→write with the pinned
// path the operator saw.
//
// Stubs are registered BEFORE page.goto so the first load already sees them.

import { test, expect } from "../fixtures/hub";

// A scan where codex-cli's config is a symlink: top-level
// client_config_presence "error-symlink" (codex-cli NOT in any server's
// client_presence) → perClientRouting maps the cell to config-error-symlink.
const SCAN_SYMLINK = {
  at: "2026-06-21T00:00:00Z",
  entries: [
    {
      name: "memory",
      manifest_exists: true,
      can_migrate: true,
      client_presence: {},
    },
  ],
  client_config_presence: {
    "codex-cli": "error-symlink",
  },
};

const RESOLVE_RESPONSE = {
  client: "codex-cli",
  original_path: "/home/u/.codex/config.toml",
  resolved_target: "/e/env/Agents/.codex/config.toml",
  pinned_real_path: "/e/env/Agents/.codex",
  content_hash: "deadbeef",
  is_symlink: true,
};

test.describe("Servers symlink-consent affordance (A3 PR-2)", () => {
  test("config-error-symlink cell → confirm modal shows pinned path → confirm POSTs the consent write", async ({ page, hub }) => {
    await page.route("**/api/scan", (r) =>
      r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(SCAN_SYMLINK) }),
    );
    await page.route("**/api/status", (r) =>
      r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) }),
    );

    // Capture the two-phase POST bodies.
    const bodies: Array<Record<string, unknown>> = [];
    await page.route("**/api/resolve-symlink-and-write", async (r) => {
      const body = JSON.parse(r.request().postData() ?? "{}");
      bodies.push(body);
      if (body.confirm) {
        await r.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
            client: "codex-cli",
            original_path: RESOLVE_RESPONSE.original_path,
            written_path: RESOLVE_RESPONSE.resolved_target,
            written: true,
          }),
        });
      } else {
        await r.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(RESOLVE_RESPONSE),
        });
      }
    });

    await page.goto(`${hub.url}/#/servers`);
    await page.waitForSelector("table.servers-matrix");

    // The affordance renders on the symlink cell.
    const resolveBtn = page.getByTestId("resolve-symlink-codex-cli");
    await expect(resolveBtn).toBeVisible();
    await resolveBtn.click();

    // The confirm modal shows the PINNED real target the operator consents to.
    const modal = page.getByTestId("resolve-symlink-modal-codex-cli");
    await expect(modal).toBeVisible();
    await expect(page.getByTestId("resolve-symlink-pinned-codex-cli")).toHaveText(
      RESOLVE_RESPONSE.resolved_target,
    );

    // Confirm → the WRITE-phase POST fires with the pinned path + hash.
    await page.getByTestId("resolve-symlink-confirm-codex-cli").click();

    await expect.poll(() => bodies.some((b) => b.confirm === true)).toBe(true);
    const writeBody = bodies.find((b) => b.confirm === true)!;
    expect(writeBody).toMatchObject({
      client: "codex-cli",
      confirm: true,
      pinned_real_path: RESOLVE_RESPONSE.pinned_real_path,
      content_hash: RESOLVE_RESPONSE.content_hash,
    });
  });

  test("cancel closes the modal without a write POST", async ({ page, hub }) => {
    await page.route("**/api/scan", (r) =>
      r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(SCAN_SYMLINK) }),
    );
    await page.route("**/api/status", (r) =>
      r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) }),
    );

    const bodies: Array<Record<string, unknown>> = [];
    await page.route("**/api/resolve-symlink-and-write", async (r) => {
      const body = JSON.parse(r.request().postData() ?? "{}");
      bodies.push(body);
      await r.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(RESOLVE_RESPONSE),
      });
    });

    await page.goto(`${hub.url}/#/servers`);
    await page.waitForSelector("table.servers-matrix");
    await page.getByTestId("resolve-symlink-codex-cli").click();
    await expect(page.getByTestId("resolve-symlink-modal-codex-cli")).toBeVisible();
    await page.getByTestId("resolve-symlink-cancel-codex-cli").click();
    await expect(page.getByTestId("resolve-symlink-modal-codex-cli")).toBeHidden();

    // No confirm:true write was sent.
    expect(bodies.some((b) => b.confirm === true)).toBe(false);
  });
});
