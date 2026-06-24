import { test, expect } from "../fixtures/hub";

// Projects screen e2e (epic area 6 Phase 1). Drives the new read-only
// per-project lens against a LIVE mcphub gui. P1 composes the two existing
// read endpoints (/api/workspaces + /api/groups) entirely client-side — there
// is NO new backend route. On a clean tmpHome both workspaces.yaml and
// groups.yaml are absent, so /api/workspaces returns {workspaces:[],entries:[]}
// and /api/groups returns an empty groups list → the Projects empty-state.
//
// The fixture sets MCPHUB_E2E_SCHEDULER=none + MCPHUB_E2E_SUPERVISOR=none (see
// fixtures/hub.ts) so /api/status is [] and no supervisor spawn blocks startup.
// Mirrors about.spec.ts / groups.spec.ts (the simplest shell+empty-state specs).

test.describe("projects", () => {
  test("sidebar Projects link routes here, highlights active, and shows the empty-state", async ({
    page,
    hub,
  }) => {
    await page.goto(`${hub.url}/`);
    const projectsLink = page.locator(".sidebar nav a", { hasText: "Projects" });
    await projectsLink.click();
    await expect(projectsLink).toHaveClass(/active/);
    await expect(page.locator("h1")).toHaveText("Projects");
    // Clean tmpHome → workspaces.yaml absent → no projects → empty-state.
    await expect(page.getByTestId("projects-empty")).toBeVisible();
    await expect(page.getByTestId("projects-empty")).toContainText("No projects yet");
  });

  test("composes /api/workspaces and /api/groups on load", async ({ page, hub }) => {
    // Both reads fire on mount; assert the workspaces compose-source is hit.
    const wsReq = page.waitForRequest(
      (r) => r.url() === `${hub.url}/api/workspaces`,
      { timeout: 5_000 },
    );
    const grReq = page.waitForRequest(
      (r) => r.url() === `${hub.url}/api/groups`,
      { timeout: 5_000 },
    );
    await page.goto(`${hub.url}/#/projects`);
    expect((await wsReq).method()).toBe("GET");
    expect((await grReq).method()).toBe("GET");
    // The loaded list container renders (empty-state lives inside it).
    await expect(page.getByTestId("projects-loaded")).toBeVisible();
  });
});
