import { test, expect } from "../fixtures/hub";

// Projects screen e2e (epic area 6). Drives the per-project lens against a LIVE
// mcphub gui. PHASE 3b switched the data source to the SINGLE GET /api/projects
// aggregate (design decision 6). On a clean tmpHome the workspace registry +
// groups.yaml are absent, so /api/projects returns {projects:[],groups:[]} → the
// Projects empty-state.
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
    // Clean tmpHome → registry absent → no projects → empty-state.
    await expect(page.getByTestId("projects-empty")).toBeVisible();
    await expect(page.getByTestId("projects-empty")).toContainText("No projects yet");
  });

  test("reads the single /api/projects aggregate on load (not the two P1 endpoints)", async ({
    page,
    hub,
  }) => {
    // P3b reads ONE aggregate on mount. Assert it fires as a GET and that the
    // loaded list container renders (empty-state lives inside it).
    const aggReq = page.waitForRequest(
      (r) => r.url() === `${hub.url}/api/projects`,
      { timeout: 5_000 },
    );
    await page.goto(`${hub.url}/#/projects`);
    expect((await aggReq).method()).toBe("GET");
    await expect(page.getByTestId("projects-loaded")).toBeVisible();
  });
});
