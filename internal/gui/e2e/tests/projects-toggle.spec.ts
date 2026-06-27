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
//     the claude Project toggle posts scope claude-local-membership — the APPROVAL
//     ARRAY-MOVE, value-free — NOT the object-member member-delete; FIX 1)
//   - object-member re-enable is ALWAYS cold → Re-add CTA (the warm value-replay
//     path was removed: the aggregate NILs every raw blob; FIX 2). A disabled claude
//     Project row is a value-free array-move toggle, NOT cold (its def stays put).
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
  //    enabled:false → object-member member removed → row flips to the COLD
  //    Re-add CTA (FIX 2 — re-enable is always cold). The toggle POST carries
  //    scope project-object-member and NO value (warm path removed). Against the
  //    sanitized (raw=null) wire shape /api/projects actually returns.
  // -------------------------------------------------------------------------
  test("scenario 1: cursor disable posts project-object-member (no value) and goes cold (Re-add)", async ({
    page,
    hub,
  }) => {
    await stubAggregate(
      page,
      // raw NIL on the wire — exactly what the sanitized aggregate returns.
      { projects: [proj({ scan: { at: "now", entries: [scanEntry("memory", { cursor: {} })] } })], groups: [] },
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
    // FIX 6: the live object-member control warns the disable is destructive.
    await expect(toggle).toHaveAttribute("title", /re-adding it/);

    // Use click() (not uncheck()): the row TRANSITIONS away after the disable
    // reconciles — the checkbox is replaced by the cold Re-add CTA — so uncheck()'s
    // post-click "is now unchecked" assertion can't re-bind to the detached node.
    await toggle.click();
    // Reconciled OFF → object-member member gone → the row flips to the cold
    // Re-add CTA (no value-less enable toggle remains).
    await expect(page.getByTestId("projects-readd-memory")).toBeVisible();
    await expect(page.getByTestId("projects-readd-memory")).toHaveAttribute("href", "#/add-server");
    await expect(page.getByTestId("projects-toggle-project-object-member-memory")).toHaveCount(0);
    // SINGLE-OWNER scope: cursor object-member → scope project-object-member, no value.
    expect(toggleBody!.scope).toBe("project-object-member");
    expect(toggleBody!.client).toBe("cursor");
    expect(toggleBody!.enable).toBe(false);
    expect(toggleBody!.value).toBeUndefined();
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
      { projects: [proj({ scan: { at: "now", entries: [scanEntry("memory", { cursor: {} })] } })], groups: [] },
    );
    await page.route("**/api/projects/toggle", async (route) => {
      await route.fulfill({ status: 500, contentType: "application/json", body: JSON.stringify({ error: "disk full", code: "PROJECT_TOGGLE_FAILED" }) });
    });

    await page.goto(`${hub.url}/#/projects?path=${encodeURIComponent(KEY)}`);
    const toggle = page.getByTestId("projects-toggle-project-object-member-memory");
    // Use click() (not uncheck()): the 500 REVERTS the optimistic flip back to ON,
    // so uncheck()'s "is now unchecked" post-assertion would never settle.
    await toggle.click();
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
  //    APPROVAL ARRAY-MOVE scope claude-local-membership (FIX 1 — NOT the
  //    object-member member-delete that would data-loss the shared .mcp.json def
  //    and spring back ON on reload) with NO value (the array move is value-free).
  // -------------------------------------------------------------------------
  test("scenario 3: claude both-scopes — Project toggle posts claude-local-membership (array-move), Local read-only, shadow once", async ({
    page,
    hub,
  }) => {
    await stubAggregate(page, {
      projects: [
        proj({
          scan: {
            at: "now",
            // raw NIL on the wire (sanitizeScanResult strips it).
            entries: [
              scanEntry("approved", { "claude-code": {} }, { project_enabled: true }),
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
      const enable = toggleBody?.enable === true;
      // Array-move read-back echoes the requested intent (the def is never deleted).
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ scope: "claude-local-membership", server: "approved", enabled: enable }) });
    });

    await page.goto(`${hub.url}/#/projects?path=${encodeURIComponent(KEY)}`);
    await expect(page.getByTestId("projects-client-claude-code")).toBeVisible();
    // Project subsection: a live ARRAY-MOVE toggle for the approved entry (FIX 1).
    const approvedToggle = page.getByTestId("projects-toggle-claude-local-membership-approved");
    await expect(approvedToggle).toBeChecked();
    // It must NOT use the object-member member-delete scope.
    await expect(page.getByTestId("projects-toggle-project-object-member-approved")).toHaveCount(0);
    // Shadow rendered ONCE: muted anchor in Project + authoritative cross-ref in Local.
    await expect(page.getByTestId("projects-shadow-shadowed")).toBeVisible();
    await expect(page.getByTestId("projects-shadow-authoritative-shadowed")).toBeVisible();
    // No competing toggle for the shadowed entry.
    await expect(page.getByTestId("projects-toggle-claude-local-membership-shadowed")).toHaveCount(0);
    // Local subsection: localonly is read-only (no toggle / no write owner — D1).
    const localRow = page.getByTestId("projects-claude-local-localonly");
    await expect(localRow).toContainText("read-only");
    await expect(localRow.locator("input[type=checkbox]")).toHaveCount(0);

    // The Project toggle posts the array-move scope with NO value. Because the
    // .mcp.json def is never deleted, the row STAYS a toggle after a disable (no
    // cold Re-add CTA) — disable then re-enable to prove the round-trip.
    await approvedToggle.uncheck();
    await expect(approvedToggle).not.toBeChecked();
    await expect.poll(() => toggleBody?.enable).toBe(false);
    expect(toggleBody!.scope).toBe("claude-local-membership");
    expect(toggleBody!.client).toBe("claude-code");
    expect(toggleBody!.value).toBeUndefined();
    // No cold CTA for an array-move row — it re-enables by a plain toggle.
    await expect(page.getByTestId("projects-readd-approved")).toHaveCount(0);
    await approvedToggle.check();
    await expect.poll(() => toggleBody?.enable).toBe(true);
    expect(toggleBody!.scope).toBe("claude-local-membership");
    expect(toggleBody!.value).toBeUndefined();
  });

  // -------------------------------------------------------------------------
  // 4. A DISABLED claude Project row is the ARRAY-MOVE substrate, NOT cold (FIX 1).
  //    It renders a value-free OFF toggle (NO Re-add CTA — the .mcp.json def stays
  //    put, decision 5), and stays OFF after the page-load reseed (spring-back
  //    regression). Re-enabling it posts claude-local-membership with NO value.
  //    (Cold re-enable for object-members is exercised by scenario 1's cursor
  //    disable; an object-member is never cold on the initial load because a
  //    removed member simply doesn't appear in the scan.)
  // -------------------------------------------------------------------------
  test("scenario 4: a disabled claude Project row is a value-free array-move toggle (not cold), stays OFF, re-enables via claude-local-membership", async ({
    page,
    hub,
  }) => {
    await stubAggregate(page, {
      projects: [
        proj({
          scan: {
            at: "now",
            entries: [scanEntry("offsrv", { "claude-code": {} }, { project_enabled: false })],
            project_scope: { local_servers: [], disabled_mcpjson_servers: ["offsrv"] },
          },
        }),
      ],
      groups: [],
    });
    await stubReadiness(page); // 404 → O-1 skip
    let toggleBody: Record<string, unknown> | null = null;
    await page.route("**/api/projects/toggle", async (route) => {
      toggleBody = JSON.parse(route.request().postData() ?? "{}");
      const enable = toggleBody?.enable === true;
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ scope: "claude-local-membership", server: "offsrv", enabled: enable }) });
    });

    await page.goto(`${hub.url}/#/projects?path=${encodeURIComponent(KEY)}`);
    await expect(page.getByTestId("projects-client-claude-code")).toBeVisible();
    // SPRING-BACK regression: project_enabled:false re-seeds the row OFF and it
    // STAYS off (the approval array persisted the disable) — NOT springing back ON.
    const toggle = page.getByTestId("projects-toggle-claude-local-membership-offsrv");
    await expect(toggle).not.toBeChecked();
    // It is an array-move toggle, NOT a cold Re-add CTA (the def is never deleted).
    await expect(page.getByTestId("projects-readd-offsrv")).toHaveCount(0);
    // Re-enable: value-free array-move POST (no member value), no cold refusal.
    await toggle.check();
    await expect.poll(() => toggleBody?.enable).toBe(true);
    expect(toggleBody!.scope).toBe("claude-local-membership");
    expect(toggleBody!.client).toBe("claude-code");
    expect(toggleBody!.value).toBeUndefined();
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
    // P3c: group lives on the per-project dto.groups (the detail lens reads it).
    await stubAggregate(page, {
      projects: [proj({ scan: { at: "now", entries: [] }, groups: [{ name: "g1", servers: ["gdb-mcp"], tools_hidden: {}, project_path: "" }] })],
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
    // P3c: group lives on the per-project dto.groups (the detail lens reads it).
    await stubAggregate(page, {
      projects: [proj({ scan: { at: "now", entries: [] }, groups: [{ name: "g1", servers: ["projonly"], tools_hidden: {}, project_path: "" }] })],
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
    // P3c: the detail lens reads the PER-PROJECT binding-filtered dto.groups, so
    // the group must live on the project DTO (not the top-level groups).
    await stubAggregate(page, {
      projects: [proj({ scan: { at: "now", entries: [] }, groups: [{ name: "g1", servers: ["stale"], tools_hidden: {}, project_path: "" }] })],
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
    // click() (not check()): the enable is rejected with a 400 and the optimistic
    // flip REVERTS to OFF, so check()'s "is now checked" post-assertion can't settle.
    await toggle.click(); // enable → UNKNOWN_SERVER
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
            // raw NIL on the wire (sanitized aggregate shape).
            entries: [
              scanEntry("rowA", { cursor: {} }),
              scanEntry("rowB", { cursor: {} }),
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
    // click() (not uncheck()): rowA's POST HANGS, so the optimistic flip leaves the
    // control busy+disabled — uncheck()'s post-click actionability re-check would
    // time out against the now-disabled input. A plain click fires the toggle.
    await a.click(); // rowA → busy (POST hangs)
    await expect(page.getByTestId("projects-toggle-spinner-project-object-member-rowA")).toBeVisible();
    await expect(a).toBeDisabled();
    // rowB stays interactive while rowA is busy: click it → its POST resolves →
    // the object-member reconciles OFF → the row flips to the cold Re-add CTA.
    // Reaching that proves rowB was never blocked by rowA's in-flight toggle.
    await expect(b).toBeEnabled();
    // click() (not uncheck()): rowB's disable reconciles OFF → the object-member
    // flips to the cold Re-add CTA, detaching the checkbox before uncheck()'s
    // post-assertion could re-bind.
    await b.click();
    await expect(page.getByTestId("projects-readd-rowB")).toBeVisible();
    // rowA is still busy (its spinner is up, its POST hasn't been released).
    await expect(page.getByTestId("projects-toggle-spinner-project-object-member-rowA")).toBeVisible();
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
