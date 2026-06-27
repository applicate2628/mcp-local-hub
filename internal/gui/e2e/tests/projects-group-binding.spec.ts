// internal/gui/e2e/tests/projects-group-binding.spec.ts
//
// E2E for the per-project-GUI Phase 3c group↔project binding (design §10.1).
// Like projects-toggle.spec.ts, the whole flow is driven via page.route() stubs
// so the verdict is DETERMINISTIC regardless of what is registered on the
// runner. The PRODUCTION write owner + filter are exercised by the Go unit
// tests; this spec proves the GUI WIRING end-to-end against the real Preact
// build + hash router:
//   - the replaced copy (no "not yet bound to a project (coming later)")
//   - bound-vs-global render from group.project_path
//   - "Bind to this project" POSTs the project key + reloads the aggregate
//   - "Unbind (make global)" POSTs an EMPTY project_path
//   - a bind failure shows plain copy + Retry (raw code only in tooltip)
//
// Stubs are registered BEFORE page.goto so the first load already sees them.

import { test, expect } from "../fixtures/hub";
import type { Page } from "@playwright/test";

const KEY = "/home/x/proj";

function proj(over: Record<string, unknown> = {}) {
  return { key: KEY, workspace_path: KEY, entries: [], ...over };
}

// stubAggregate registers GET /api/projects with the given per-project dto.groups
// (the binding-filtered set the detail lens reads).
async function stubAggregate(page: Page, dtoGroups: unknown[]) {
  await page.route("**/api/projects", async (route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ projects: [proj({ scan: { at: "now", entries: [] }, groups: dtoGroups })], groups: [] }),
      });
    } else {
      await route.continue();
    }
  });
}

test.describe("Projects P3c — group↔project binding (§10.1)", () => {
  test("scenario 1: the P1 placeholder copy is gone; the new binding copy + bound/global labels render", async ({
    page,
    hub,
  }) => {
    await stubAggregate(page, [
      { name: "glob", servers: [], project_path: "" },
      { name: "mine", servers: [], project_path: KEY },
    ]);
    await page.goto(`${hub.url}/#/projects?path=${encodeURIComponent(KEY)}`);
    const sec = page.getByTestId("projects-section-groups");
    await expect(sec).toBeVisible();
    await expect(sec).not.toContainText("not yet bound to a project");
    await expect(sec).not.toContainText("coming later");
    await expect(sec).toContainText("Bind a group here to scope it to this project");
    // bound-vs-global labels.
    await expect(page.getByTestId("projects-group-binding-state-glob")).toContainText("global (all projects)");
    await expect(page.getByTestId("projects-group-binding-state-mine")).toContainText("bound to this project");
  });

  test("scenario 2: 'Bind to this project' POSTs the project key, then the aggregate reloads", async ({
    page,
    hub,
  }) => {
    let bindBody: Record<string, unknown> | null = null;
    let aggGets = 0;
    await page.route("**/api/projects", async (route) => {
      if (route.request().method() === "GET") {
        aggGets++;
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ projects: [proj({ scan: { at: "now", entries: [] }, groups: [{ name: "glob", servers: [], project_path: "" }] })], groups: [] }),
        });
      } else {
        await route.continue();
      }
    });
    await page.route("**/api/projects/group-binding", async (route) => {
      bindBody = JSON.parse(route.request().postData() ?? "{}");
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ group: "glob", project_path: KEY }) });
    });

    await page.goto(`${hub.url}/#/projects?path=${encodeURIComponent(KEY)}`);
    const bindBtn = page.getByTestId("projects-group-bind-glob");
    await expect(bindBtn).toContainText("Bind to this project");
    const gotsBefore = aggGets;
    await bindBtn.click();
    await expect.poll(() => bindBody && (bindBody as Record<string, unknown>).group).toBe("glob");
    expect((bindBody as unknown as Record<string, unknown>).project_path).toBe(KEY);
    // Reload fired after the successful bind (so the filtered list re-derives).
    await expect.poll(() => aggGets).toBeGreaterThan(gotsBefore);
  });

  test("scenario 3: 'Unbind (make global)' POSTs an EMPTY project_path", async ({ page, hub }) => {
    let bindBody: Record<string, unknown> | null = null;
    await stubAggregate(page, [{ name: "mine", servers: [], project_path: KEY }]);
    await page.route("**/api/projects/group-binding", async (route) => {
      bindBody = JSON.parse(route.request().postData() ?? "{}");
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ group: "mine", project_path: "" }) });
    });

    await page.goto(`${hub.url}/#/projects?path=${encodeURIComponent(KEY)}`);
    const unbindBtn = page.getByTestId("projects-group-bind-mine");
    await expect(unbindBtn).toContainText("Unbind");
    await unbindBtn.click();
    await expect.poll(() => bindBody && (bindBody as Record<string, unknown>).group).toBe("mine");
    expect((bindBody as unknown as Record<string, unknown>).project_path).toBe("");
  });

  test("scenario 4: a bind 500 shows plain copy + Retry (raw code only in tooltip)", async ({
    page,
    hub,
  }) => {
    await stubAggregate(page, [{ name: "glob", servers: [], project_path: "" }]);
    await page.route("**/api/projects/group-binding", async (route) => {
      await route.fulfill({ status: 500, contentType: "application/json", body: JSON.stringify({ error: "internal error", code: "PROJECT_GROUP_BINDING_FAILED" }) });
    });

    await page.goto(`${hub.url}/#/projects?path=${encodeURIComponent(KEY)}`);
    await page.getByTestId("projects-group-bind-glob").click();
    const err = page.getByTestId("projects-group-bind-error-glob");
    await expect(err).toBeVisible();
    await expect(err).toContainText("couldn't be saved");
    // Raw code is NOT on the visible row (tooltip title only).
    await expect(err).not.toContainText("PROJECT_GROUP_BINDING_FAILED");
    await expect(page.getByTestId("projects-group-bind-retry-glob")).toBeVisible();
  });
});
