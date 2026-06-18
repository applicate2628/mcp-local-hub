import { test, expect } from "../fixtures/hub";

// Groups screen e2e (groups Phase 5b-2). Drives the Groups authoring screen
// against a LIVE mcphub gui — the /api/groups handler runs in-process and the
// GUI process owns it, so unlike Catalog's marketplace specs there is NO need
// to stub the route: on a clean tmpHome groups.yaml is absent (empty list) and
// available_servers comes from the embed-first ManifestList (the shipped server
// set baked into the binary, e.g. memory / time / serena). The create POST
// writes a real groups.yaml under the per-test temp state dir, so the screen
// behavior is exercised end-to-end through the hardened state-file write.
//
// The fixture sets MCPHUB_E2E_SCHEDULER=none + MCPHUB_E2E_SUPERVISOR=none (see
// fixtures/hub.ts) so /api/status is [] and no supervisor spawn blocks startup.

test.describe("groups", () => {
  test("sidebar Groups link routes here, highlights active, and shows the empty-state", async ({
    page,
    hub,
  }) => {
    await page.goto(`${hub.url}/`);
    const groupsLink = page.locator(".sidebar nav a", { hasText: "Groups" });
    await groupsLink.click();
    await expect(groupsLink).toHaveClass(/active/);
    await expect(page.locator("h1")).toHaveText("Groups");
    // Clean tmpHome → groups.yaml absent → empty-state.
    await expect(page.getByTestId("groups-empty")).toBeVisible();
  });

  test("the available-server picker is populated from the embedded server set", async ({
    page,
    hub,
  }) => {
    await page.goto(`${hub.url}/#/groups`);
    await page.getByTestId("groups-new").click();
    // The embed-first ManifestList ships `memory` and `time`; the picker
    // offers them without any installed-server seed.
    await expect(page.getByTestId("groups-server-checkbox-memory")).toBeVisible();
    await expect(page.getByTestId("groups-server-checkbox-time")).toBeVisible();
  });

  test("creates a group via the form → it persists and appears in the list", async ({
    page,
    hub,
  }) => {
    await page.goto(`${hub.url}/#/groups`);

    // Capture the create POST to assert the body shape went over the wire.
    const postPromise = page.waitForRequest(
      (r) => r.url() === `${hub.url}/api/groups` && r.method() === "POST",
      { timeout: 5_000 },
    );

    await page.getByTestId("groups-new").click();
    await page.getByTestId("groups-name-input").fill("frontend");
    await page.getByTestId("groups-server-checkbox-memory").check();
    // Hide a tool on the selected server (the fine-grained per-tool filter).
    await page.getByTestId("groups-hidden-input-memory").fill("delete_entities");
    await page.getByTestId("groups-save").click();

    const post = await postPromise;
    const body = JSON.parse(post.postData() ?? "{}");
    expect(body).toMatchObject({
      name: "frontend",
      servers: ["memory"],
      tools_hidden: { memory: ["delete_entities"] },
    });

    // The row appears after the save reloads the list (real persistence).
    await expect(page.getByTestId("groups-row-frontend")).toBeVisible();
    await expect(page.getByTestId("groups-row-servers-frontend")).toContainText("memory");

    // A fresh GET on reload still shows it (proves it persisted to groups.yaml).
    await page.reload();
    await expect(page.getByTestId("groups-row-frontend")).toBeVisible();
  });

  test("rejects an unknown server with an inline error (authoring-boundary strictness)", async ({
    page,
    hub,
  }) => {
    // The picker only offers known servers, so to exercise the
    // GROUPS_UNKNOWN_SERVER gate we POST a non-member server directly. This
    // proves the backend strictness surfaces as the screen's inline error
    // path would render it — the API contract guard the screen depends on.
    const resp = await page.request.post(`${hub.url}/api/groups`, {
      data: { name: "g", servers: ["definitely-not-a-real-server"] },
    });
    expect(resp.status()).toBe(400);
    const body = await resp.json();
    expect(body.code).toBe("GROUPS_UNKNOWN_SERVER");
  });

  test("deletes a group via the ConfirmModal", async ({ page, hub }) => {
    // Seed a group directly so the test is independent of the create flow.
    const seed = await page.request.post(`${hub.url}/api/groups`, {
      data: { name: "infra", servers: ["time"] },
    });
    expect(seed.ok()).toBeTruthy();

    await page.goto(`${hub.url}/#/groups`);
    await expect(page.getByTestId("groups-row-infra")).toBeVisible();

    await page.getByTestId("groups-delete-infra").click();
    // The ConfirmModal (native <dialog>) appears; confirm.
    await page.getByTestId("groups-confirm-delete-confirm").click();

    // Back to the empty-state (the only seeded group is gone).
    await expect(page.getByTestId("groups-empty")).toBeVisible();
    // Persisted: a reload does not resurrect it.
    await page.reload();
    await expect(page.getByTestId("groups-row-infra")).toHaveCount(0);
  });
});
