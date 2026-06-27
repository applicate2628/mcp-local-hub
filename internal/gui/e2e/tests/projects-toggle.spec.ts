// internal/gui/e2e/tests/projects-toggle.spec.ts
//
// E2E scenarios for the per-project-GUI Phase 3b detail-lens per-row toggles
// (work-items/decisions/2026-06-27-per-project-gui-p3b-uxdesign.md). Like
// catalog-readiness.spec.ts, the whole flow is driven via page.route() stubs so
// the verdict is DETERMINISTIC regardless of what is registered on the runner —
// /api/projects, /api/projects/toggle, and /api/server/readiness are all stubbed.
// The PRODUCTION write owners are exercised by the Go unit tests; this spec
// proves the GUI WIRING end-to-end against the real Preact build + hash router:
//   - toggle happy-path (optimistic flip → 200 → ✓)
//   - error revert + §3.1 plain copy (raw code only in tooltip) + Retry
//   - reconcile-to-response (response.enabled clamps, not the requested intent)
//   - both-scopes claude card render (Project toggle / Local read-only / shadow once)
//   - array-move scope assertion (claude Local row is read-only, no write owner D1;
//     the claude Project toggle posts scope project-object-member with a value)
//   - warm re-enable replays the held value; cold re-enable → Re-add CTA
//   - consent-on-enable gate (readiness blockers disable Confirm; O-1 skip)
//   - group name-gate (a non-routable enable → UNKNOWN_SERVER plain copy, no Retry)
//   - per-row isolation (one row toggling leaves the others interactive)
//   - section-scoped ScanError (config section error, rest of the screen renders)
//
// Stubs are registered BEFORE page.goto so the first load already sees them.

import { test, expect } from "../fixtures/hub";
import type { Page, Route } from "@playwright/test";

const KEY = "/home/x/proj";

// stubAggregate registers the GET /api/projects aggregate with the given body.
async function stubAggregate(page: Page, body: unknown) {
  await page.route("**/api/projects", async (route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
    } else {
      await route.continue();
    }
  });
}

// stubReadiness registers GET /api/server/readiness. Default 404 = O-1 (no report
// obtainable → the gate is skipped, the toggle proceeds).
async function stubReadiness(page: Page, handler?: (route: Route) => Promise<void>) {
  await page.route("**/api/server/readiness**", async (route) => {
    if (route.request().method() !== "GET") {
      await route.continue();
      return;
    }
    if (handler) {
      await handler(route);
      return;
    }
    await route.fulfill({ status: 404, contentType: "application/json", body: JSON.stringify({ error: "no manifest" }) });
  });
}

// proj/scanEntry builders mirror the wire shapes.
function proj(over: Record<string, unknown> = {}) {
  return { key: KEY, workspace_path: KEY, entries: [], ...over };
}
function scanEntry(name: string, presence: Record<string, unknown>, over: Record<string, unknown> = {}) {
  return { name, client_presence: presence, manifest_exists: true, can_migrate: true, ...over };
}

test.describe("Projects P3b — per-row toggles (epic area 6)", () => {
  // -------------------------------------------------------------------------
  // 1. Happy path + reconcile-to-response: disable a cursor member → 200
  //    enabled:false → row OFF + ✓ flash. The toggle POST carries scope
  //    project-object-member.
  // -------------------------------------------------------------------------
  test("scenario 1: toggle happy-path posts the right scope and reconciles to response.enabled", async ({
    page,
    hub,
  }) => {
    await stubAggregate(
      page,
      { projects: [proj({ scan: { at: "now", entries: [scanEntry("memory", { cursor: { raw: { command: "x" } } })] } })], groups: [] },
    );
    let toggleBody: Record<string, unknown> | null = null;
    await page.route("**/api/projects/toggle", async (route) => {
      toggleBody = JSON.parse(route.request().postData() ?? "{}");
      // Reconcile to OFF (the disable result).
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ scope: "project-object-member", server: "memory", enabled: false }) });
    });

    await page.goto(`${hub.url}/#/projects?path=${encodeURIComponent(KEY)}`);
    await expect(page.getByTestId("projects-client-cursor")).toBeVisible();
    const toggle = page.getByTestId("projects-toggle-project-object-member-memory");
    await expect(toggle).toBeChecked(); // present → enabled

    await toggle.uncheck();
    await expect(toggle).not.toBeChecked();
    await expect(page.getByTestId("projects-toggle-ok-project-object-member-memory")).toBeVisible();
    // SINGLE-OWNER scope: cursor object-member → scope project-object-member.
    expect(toggleBody!.scope).toBe("project-object-member");
    expect(toggleBody!.client).toBe("cursor");
    expect(toggleBody!.enable).toBe(false);
  });

  // -------------------------------------------------------------------------
  // 2. Error revert + §3.1 plain copy (raw code only in tooltip) + Retry.
  // -------------------------------------------------------------------------
  test("scenario 2: a 500 reverts the optimistic flip and shows plain copy + Retry (code in tooltip)", async ({
    page,
    hub,
  }) => {
    await stubAggregate(
      page,
      { projects: [proj({ scan: { at: "now", entries: [scanEntry("memory", { cursor: { raw: { command: "x" } } })] } })], groups: [] },
    );
    await page.route("**/api/projects/toggle", async (route) => {
      await route.fulfill({ status: 500, contentType: "application/json", body: JSON.stringify({ error: "disk full", code: "PROJECT_TOGGLE_FAILED" }) });
    });

    await page.goto(`${hub.url}/#/projects?path=${encodeURIComponent(KEY)}`);
    const toggle = page.getByTestId("projects-toggle-project-object-member-memory");
    await toggle.uncheck();
    // REVERT back to ON.
    await expect(toggle).toBeChecked();
    const err = page.getByTestId("projects-toggle-error-project-object-member-memory");
    await expect(err).toContainText("couldn't be saved");
    await expect(err).not.toContainText("PROJECT_TOGGLE_FAILED"); // raw code not on the visible row
    // Raw code lives only in the tooltip.
    await expect(err.locator("[title='PROJECT_TOGGLE_FAILED']")).toBeAttached();
    await expect(page.getByTestId("projects-toggle-retry-project-object-member-memory")).toBeVisible();
  });

  // -------------------------------------------------------------------------
  // 3. Both-scopes claude card: Project toggle + Local read-only + shadow once.
  //    The Local row has NO toggle (D1 read-only); the Project toggle posts the
  //    object-member scope with the held value.
  // -------------------------------------------------------------------------
  test("scenario 3: claude both-scopes — Project toggle, Local read-only, shadow rendered once", async ({
    page,
    hub,
  }) => {
    await stubAggregate(page, {
      projects: [
        proj({
          scan: {
            at: "now",
            entries: [
              scanEntry("approved", { "claude-code": { raw: { command: "y" } } }, { project_enabled: true }),
              scanEntry("shadowed", { "claude-code": {} }, { project_shadowed_by_local: true }),
            ],
            project_scope: { local_servers: ["shadowed", "localonly"] },
          },
        }),
      ],
      groups: [],
    });
    await stubReadiness(page); // 404 → O-1 skip
    let toggleBody: Record<string, unknown> | null = null;
    await page.route("**/api/projects/toggle", async (route) => {
      toggleBody = JSON.parse(route.request().postData() ?? "{}");
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ scope: "project-object-member", server: "approved", enabled: false }) });
    });

    await page.goto(`${hub.url}/#/projects?path=${encodeURIComponent(KEY)}`);
    await expect(page.getByTestId("projects-client-claude-code")).toBeVisible();
    // Project subsection: a live toggle for the approved entry.
    const approvedToggle = page.getByTestId("projects-toggle-project-object-member-approved");
    await expect(approvedToggle).toBeChecked();
    // Shadow rendered ONCE: muted anchor in Project + authoritative cross-ref in Local.
    await expect(page.getByTestId("projects-shadow-shadowed")).toBeVisible();
    await expect(page.getByTestId("projects-shadow-authoritative-shadowed")).toBeVisible();
    // No competing toggle for the shadowed entry.
    await expect(page.getByTestId("projects-toggle-project-object-member-shadowed")).toHaveCount(0);
    // Local subsection: localonly is read-only (no toggle / no write owner — D1).
    const localRow = page.getByTestId("projects-claude-local-localonly");
    await expect(localRow).toContainText("read-only");
    await expect(localRow.locator("input[type=checkbox]")).toHaveCount(0);

    // The Project toggle posts object-member scope with the held value (warm path
    // — the scan carried raw {command:y}). Disable then re-enable to assert value.
    await approvedToggle.uncheck();
    await expect(approvedToggle).not.toBeChecked();
    await approvedToggle.check();
    await expect.poll(() => toggleBody?.enable).toBe(true);
    expect(toggleBody!.scope).toBe("project-object-member");
    expect(toggleBody!.client).toBe("claude-code");
    expect(toggleBody!.value).toEqual({ command: "y" });
  });

  // -------------------------------------------------------------------------
  // 4. Cold object-member re-enable → Re-add CTA (never a value-less enable POST).
  // -------------------------------------------------------------------------
  test("scenario 4: cold object-member (disabled, no held value) renders Re-add, no enable toggle", async ({
    page,
    hub,
  }) => {
    await stubAggregate(page, {
      projects: [
        proj({
          scan: { at: "now", entries: [scanEntry("cold", { "claude-code": {} }, { project_enabled: false })] },
        }),
      ],
      groups: [],
    });
    let toggleHit = false;
    await page.route("**/api/projects/toggle", async (route) => {
      toggleHit = true;
      await route.fulfill({ status: 200, contentType: "application/json", body: "{}" });
    });

    await page.goto(`${hub.url}/#/projects?path=${encodeURIComponent(KEY)}`);
    await expect(page.getByTestId("projects-client-claude-code")).toBeVisible();
    const readd = page.getByTestId("projects-readd-cold");
    await expect(readd).toBeVisible();
    await expect(readd).toHaveAttribute("href", "#/add-server");
    // No enable toggle was rendered for the cold row, and no value-less POST fired.
    await expect(page.getByTestId("projects-toggle-project-object-member-cold")).toHaveCount(0);
    expect(toggleHit).toBe(false);
  });

  // -------------------------------------------------------------------------
  // 5. Consent-on-enable gate: readiness blockers disable Confirm; Confirm posts.
  // -------------------------------------------------------------------------
  test("scenario 5: enable runs the readiness consent gate; blockers disable Confirm", async ({
    page,
    hub,
  }) => {
    // A group member enable (no value needed) triggers the gate. Readiness has a
    // blocker → the gate opens with Confirm disabled.
    await stubAggregate(page, {
      projects: [proj({ scan: { at: "now", entries: [] } })],
      groups: [{ name: "g1", servers: ["gdb-mcp"], tools_hidden: {} }],
    });
    let blockerPresent = true;
    await stubReadiness(page, async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          server: "gdb-mcp",
          ready: !blockerPresent,
          requirements: [
            { name: "binary: gdb", ok: !blockerPresent, optional: false, reason: "not on PATH", fix: "Install gdb." },
          ],
        }),
      });
    });
    let togglePosted = false;
    await page.route("**/api/projects/toggle", async (route) => {
      togglePosted = true;
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ scope: "group-servers", server: "gdb-mcp", enabled: false }) });
    });

    await page.goto(`${hub.url}/#/projects?path=${encodeURIComponent(KEY)}`);
    // The group member starts as a member (enabled) → uncheck then re-check to
    // exercise the ENABLE consent path.
    const toggle = page.getByTestId("projects-toggle-group-servers-gdb-mcp");
    await toggle.uncheck(); // disable never gates
    await expect.poll(() => togglePosted).toBe(true);
    togglePosted = false;
    // Re-enable → consent gate opens with the blocker; Confirm disabled.
    await toggle.check();
    const gate = page.getByTestId("projects-consent-gate-gdb-mcp");
    await expect(gate).toBeVisible();
    await expect(page.getByTestId("projects-consent-confirm-gdb-mcp")).toBeDisabled();
    expect(togglePosted).toBe(false); // no POST while gated
  });

  // -------------------------------------------------------------------------
  // 6. O-1: no readiness report (404) → the gate is SKIPPED, the enable proceeds.
  // -------------------------------------------------------------------------
  test("scenario 6: O-1 — a 404 readiness (no report) skips the gate and enables", async ({
    page,
    hub,
  }) => {
    await stubAggregate(page, {
      projects: [proj({ scan: { at: "now", entries: [] } })],
      groups: [{ name: "g1", servers: ["projonly"], tools_hidden: {} }],
    });
    await stubReadiness(page); // 404 → O-1
    let enablePosted = false;
    await page.route("**/api/projects/toggle", async (route) => {
      const body = JSON.parse(route.request().postData() ?? "{}");
      if (body.enable === true) enablePosted = true;
      // Echo the requested intent so disable→OFF, then enable→ON actually fires
      // a state change (reconcile-to-response would otherwise no-op the re-check).
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ scope: "group-servers", server: "projonly", enabled: body.enable === true }) });
    });

    await page.goto(`${hub.url}/#/projects?path=${encodeURIComponent(KEY)}`);
    const toggle = page.getByTestId("projects-toggle-group-servers-projonly");
    await toggle.uncheck();
    await expect(toggle).not.toBeChecked();
    await toggle.check(); // ENABLE — O-1 must skip the gate and POST directly
    // No gate appears (O-1 skip); the enable toggle posts directly.
    await expect(page.getByTestId("projects-consent-gate-projonly")).toHaveCount(0);
    await expect.poll(() => enablePosted).toBe(true);
  });

  // -------------------------------------------------------------------------
  // 7. Group name-gate: a backend UNKNOWN_SERVER on enable → plain copy, no Retry.
  // -------------------------------------------------------------------------
  test("scenario 7: group enable rejected as UNKNOWN_SERVER shows plain copy + no Retry", async ({
    page,
    hub,
  }) => {
    await stubAggregate(page, {
      projects: [proj({ scan: { at: "now", entries: [] } })],
      groups: [{ name: "g1", servers: ["stale"], tools_hidden: {} }],
    });
    await stubReadiness(page); // O-1 skip so we reach the toggle POST
    await page.route("**/api/projects/toggle", async (route) => {
      const body = JSON.parse(route.request().postData() ?? "{}");
      if (body.enable) {
        await route.fulfill({ status: 400, contentType: "application/json", body: JSON.stringify({ error: "not routable", code: "PROJECT_TOGGLE_UNKNOWN_SERVER" }) });
      } else {
        await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ scope: "group-servers", server: "stale", enabled: false }) });
      }
    });

    await page.goto(`${hub.url}/#/projects?path=${encodeURIComponent(KEY)}`);
    const toggle = page.getByTestId("projects-toggle-group-servers-stale");
    await toggle.uncheck();
    await expect(toggle).not.toBeChecked();
    await toggle.check(); // enable → UNKNOWN_SERVER
    await expect(toggle).not.toBeChecked(); // revert
    const err = page.getByTestId("projects-toggle-error-group-servers-stale");
    await expect(err).toContainText("known routable server");
    // UNKNOWN_SERVER offers NO Retry (a wrong name won't get righter on retry).
    await expect(page.getByTestId("projects-toggle-retry-group-servers-stale")).toHaveCount(0);
  });

  // -------------------------------------------------------------------------
  // 8. Per-row isolation: a slow toggle on row A leaves row B interactive.
  // -------------------------------------------------------------------------
  test("scenario 8: a busy row disables only its own control, not the others", async ({ page, hub }) => {
    await stubAggregate(page, {
      projects: [
        proj({
          scan: {
            at: "now",
            entries: [
              scanEntry("rowA", { cursor: { raw: { command: "a" } } }),
              scanEntry("rowB", { cursor: { raw: { command: "b" } } }),
            ],
          },
        }),
      ],
      groups: [],
    });
    // rowA's toggle hangs (never resolves until released) so it stays "busy".
    let releaseA: (() => void) | null = null;
    await page.route("**/api/projects/toggle", async (route) => {
      const body = JSON.parse(route.request().postData() ?? "{}");
      if (body.server === "rowA") {
        await new Promise<void>((res) => (releaseA = res));
        await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ scope: "project-object-member", server: "rowA", enabled: false }) });
      } else {
        await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ scope: "project-object-member", server: "rowB", enabled: false }) });
      }
    });

    await page.goto(`${hub.url}/#/projects?path=${encodeURIComponent(KEY)}`);
    const a = page.getByTestId("projects-toggle-project-object-member-rowA");
    const b = page.getByTestId("projects-toggle-project-object-member-rowB");
    await a.uncheck(); // rowA → busy (POST hangs)
    await expect(page.getByTestId("projects-toggle-spinner-project-object-member-rowA")).toBeVisible();
    await expect(a).toBeDisabled();
    // rowB stays interactive while rowA is busy.
    await expect(b).toBeEnabled();
    await b.uncheck();
    await expect(b).not.toBeChecked();
    // Release rowA so the test settles cleanly.
    if (releaseA) (releaseA as () => void)();
  });

  // -------------------------------------------------------------------------
  // 9. Section-scoped ScanError: the config section shows its error; the rest
  //    of the screen (workspace + groups) still renders.
  // -------------------------------------------------------------------------
  test("scenario 9: a per-project scan error is section-scoped, not whole-screen", async ({ page, hub }) => {
    await stubAggregate(page, {
      projects: [proj({ scan: undefined, scan_error: "PROJECT_ROOT_INVALID" })],
      groups: [{ name: "g1", servers: ["serena"], tools_hidden: {} }],
    });

    await page.goto(`${hub.url}/#/projects?path=${encodeURIComponent(KEY)}`);
    await expect(page.getByTestId("projects-detail")).toBeVisible();
    // The whole screen still renders the other sections.
    await expect(page.getByTestId("projects-section-workspace")).toBeVisible();
    await expect(page.getByTestId("projects-section-groups")).toBeVisible();
    // The config section carries the section-scoped error.
    await expect(page.getByTestId("projects-section-config-error")).toContainText("could not be read");
  });
});
