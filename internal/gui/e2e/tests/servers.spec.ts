import { test, expect } from "../fixtures/hub";

test.describe("servers", () => {
  test("matrix renders headers with correct column set", async ({ page, hub }) => {
    // v0.6 Workstream B (§3.1): with no supervisor under the e2e fixture,
    // the real /api/status now fails loud (500) instead of falling back to
    // the empty scheduler scan, so the Servers screen would render its
    // "Failed to load" banner instead of the matrix. This test verifies the
    // matrix STRUCTURE, so stub /api/status → [] (the same convention the
    // load-path tests below already use) to exercise the populated render.
    await page.route("**/api/status", (r) =>
      r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) }),
    );
    await page.goto(`${hub.url}/#/servers`);
    await expect(page.locator("h1")).toHaveText("Servers");
    const matrix = page.locator("table.servers-matrix");
    await expect(matrix).toBeVisible();
    const headerCells = matrix.locator("thead th");
    await expect(headerCells).toHaveCount(10);
    await expect(headerCells.nth(0)).toHaveText("Server");
    // v0.4.5 init-button: client header text content can contain the
    // optional "Initialize" affordance for clients whose parent dir
    // exists but config file does not (e.g. claude-code's parent is
    // HOME=tmpHome which always exists). Match on the inner span
    // specifically so the test stays robust to that environmental
    // detail.
    await expect(headerCells.nth(1).locator(".matrix-col-header > span")).toHaveText("claude-code");
    await expect(headerCells.nth(2).locator(".matrix-col-header > span")).toHaveText("codex-cli");
    await expect(headerCells.nth(3).locator(".matrix-col-header > span")).toHaveText("cursor");
    await expect(headerCells.nth(4).locator(".matrix-col-header > span")).toHaveText("vscode");
    await expect(headerCells.nth(5).locator(".matrix-col-header > span")).toHaveText("gemini-cli");
    await expect(headerCells.nth(6).locator(".matrix-col-header > span")).toHaveText("qwen-cli");
    await expect(headerCells.nth(7).locator(".matrix-col-header > span")).toHaveText("antigravity");
    await expect(headerCells.nth(8)).toHaveText("Port");
    await expect(headerCells.nth(9)).toHaveText("State");
  });

  test("empty body when tmpHome has no client configs", async ({ page, hub }) => {
    // Workstream B (§3.1): stub /api/status → [] so the matrix renders its
    // empty body rather than the supervisor-down fail-loud banner (the e2e
    // fixture has no supervisor; the real status path now 500s). See the
    // header-columns test above for the rationale.
    await page.route("**/api/status", (r) =>
      r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) }),
    );
    await page.goto(`${hub.url}/#/servers`);
    const matrix = page.locator("table.servers-matrix");
    await expect(matrix).toBeVisible();
    await expect(matrix.locator("tbody tr")).toHaveCount(0);
    await expect(page.getByText("Loading…")).toHaveCount(0);
  });

  test("Apply button is disabled with no dirty cells", async ({ page, hub }) => {
    // Workstream B (§3.1): stub /api/status → [] so the matrix toolbar
    // (and its Apply button) renders instead of the supervisor-down
    // fail-loud banner under the no-supervisor e2e fixture.
    await page.route("**/api/status", (r) =>
      r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) }),
    );
    await page.goto(`${hub.url}/#/servers`);
    const applyBtn = page.getByRole("button", { name: "Apply changes" });
    await expect(applyBtn).toBeVisible();
    await expect(applyBtn).toBeDisabled();
  });

  // B1 scenario 1: load path. Uncheck a via-hub cell, Apply, assert
  // /api/demigrate fires with {servers, clients} narrowed to that cell,
  // AND the cell reflects "direct" after the post-Apply reload (per
  // §6.3 scenario 1 in the design memo — the post-reload state
  // assertion is what proves success-pruning + always-reload actually
  // compose to the expected UI outcome).
  test("uncheck via-hub + Apply posts /api/demigrate narrowed to that cell + post-reload reflects direct", async ({ page, hub }) => {
    // Stateful /api/scan: returns via-hub on first call (initial mount),
    // returns direct ("stdio") after the demigrate flips the backend.
    // A non-hub transport is what routing.ts:29-40 classifies as "direct".
    let demigrateCompleted = false;
    const viaHubBody = {
      entries: [
        {
          name: "demo",
          manifest_exists: true,
          client_presence: {
            "claude-code": { transport: "relay", endpoint: "" },
            "codex-cli":   { transport: "absent", endpoint: "" },
          },
        },
      ],
    };
    const directBody = {
      entries: [
        {
          name: "demo",
          manifest_exists: true,
          client_presence: {
            "claude-code": { transport: "stdio", endpoint: "" },
            "codex-cli":   { transport: "absent", endpoint: "" },
          },
        },
      ],
    };
    // Count /api/scan calls to prove the post-Apply reload actually ran
    // (§4 D6: always-reload). Without this counter, scenario 1's
    // post-reload assertions could pass without any reload happening at
    // all — the local checkbox state + Apply-disabled + via-hub-enabled
    // would all hold after success-prune alone.
    let scanCallCount = 0;
    await page.route("**/api/scan", async (r) => {
      scanCallCount++;
      const body = demigrateCompleted ? directBody : viaHubBody;
      await r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
    });
    await page.route("**/api/status", (r) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) }));

    // Intercept /api/demigrate to capture the body + flip the scan state.
    let demigrateBody: string | null = null;
    await page.route("**/api/demigrate", async (r) => {
      demigrateBody = r.request().postData();
      demigrateCompleted = true;
      await r.fulfill({ status: 204, body: "" });
    });

    await page.goto(`${hub.url}/#/servers`);
    await page.waitForSelector('table.servers-matrix');
    const initialScanCount = scanCallCount; // snapshot after mount reload

    // Uncheck the via-hub cell.
    const claudeCell = page.locator('table.servers-matrix tr').filter({ hasText: "demo" }).locator('input[type="checkbox"]').nth(0);
    await expect(claudeCell).toBeChecked(); // sanity: starts as via-hub
    // Title must match the new B1 tooltip copy, not the obsolete
    // "mcphub rollback --client" text.
    await expect(claudeCell).toHaveAttribute("title", /Uncheck and Apply/);
    await claudeCell.uncheck();
    await page.locator('#servers-toolbar button', { hasText: "Apply" }).click();

    // Assert the POST body shape.
    await expect.poll(() => demigrateBody).not.toBeNull();
    expect(JSON.parse(demigrateBody!)).toEqual({ servers: ["demo"], clients: ["claude-code"] });

    // The post-Apply /api/scan reload MUST have run — §4 D6 always-reload.
    // Without this counter check, the assertions below could all pass even
    // if no reload fired.
    await expect.poll(() => scanCallCount).toBeGreaterThan(initialScanCount);

    // Post-reload state: the claude-code cell now reflects direct (title
    // changes to the "unsupported/not-installed/direct" branch rather than
    // the via-hub branch). Asserting title disappears (or changes) proves
    // the reload updated the scan state — a pure local-flip without reload
    // would still show the via-hub title attribute.
    await expect(claudeCell).not.toHaveAttribute("title", /Uncheck and Apply/);
    await expect(claudeCell).not.toBeChecked();
    await expect(claudeCell).toBeEnabled();
    // Apply button is disabled again (dirty.size === 0 after success-prune).
    await expect(page.locator('#servers-toolbar button', { hasText: "Apply" })).toBeDisabled();
  });

  // B1 scenario 2: mixed Apply must POST /api/demigrate BEFORE /api/migrate
  // across the whole Apply (§4 D4 order invariant). Otherwise the migrate
  // writes a fresh backup that the demigrate would then read as polluted.
  test("mixed Apply dispatches demigrate before migrate", async ({ page, hub }) => {
    const scanBody = {
      entries: [
        {
          name: "a",
          manifest_exists: true,
          client_presence: {
            "claude-code": { transport: "stdio", endpoint: "" }, // direct stdio → queued migrate
          },
        },
        {
          name: "b",
          manifest_exists: true,
          client_presence: {
            "claude-code": { transport: "relay", endpoint: "" }, // via-hub → queued demigrate
          },
        },
      ],
    };
    await page.route("**/api/scan", (r) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(scanBody) }));
    await page.route("**/api/status", (r) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) }));

    const log: { url: string; at: number }[] = [];
    await page.route("**/api/demigrate", async (r) => { log.push({ url: "demigrate", at: Date.now() }); await r.fulfill({ status: 204, body: "" }); });
    await page.route("**/api/migrate",   async (r) => { log.push({ url: "migrate",   at: Date.now() }); await r.fulfill({ status: 204, body: "" }); });

    await page.goto(`${hub.url}/#/servers`);
    await page.waitForSelector('table.servers-matrix');

    // Check the direct cell (a,claude-code) and uncheck the via-hub cell (b,claude-code).
    await page.locator('table.servers-matrix tr').filter({ hasText: "a" }).locator('input[type="checkbox"]').first().check();
    await page.locator('table.servers-matrix tr').filter({ hasText: "b" }).locator('input[type="checkbox"]').first().uncheck();
    await page.locator('#servers-toolbar button', { hasText: "Apply" }).click();

    await expect.poll(() => log.length).toBeGreaterThanOrEqual(2);
    expect(log[0].url).toBe("demigrate");
    expect(log[1].url).toBe("migrate");
  });

  // B1 scenario 3: a demigrate failure must still trigger a reload (§4 D6
  // revised) and retain the failed entry in dirty for retry.
  test("demigrate failure always-reloads and retains failed entry in dirty", async ({ page, hub }) => {
    const scanBody = {
      entries: [
        {
          name: "demo",
          manifest_exists: true,
          client_presence: {
            "claude-code": { transport: "relay", endpoint: "" },
          },
        },
      ],
    };

    // Single /api/scan route that both fulfills AND increments the counter.
    // Installing a second page.route for the same URL would make only one of
    // them respond (Playwright route precedence is stack-based and
    // implementation-defined); keep it one route.
    let scanCallCount = 0;
    await page.route("**/api/scan", async (r) => {
      scanCallCount++;
      await r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(scanBody) });
    });
    await page.route("**/api/status", (r) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) }));
    await page.route("**/api/demigrate", (r) => r.fulfill({ status: 500, contentType: "application/json", body: JSON.stringify({ error: "disk full" }) }));

    await page.goto(`${hub.url}/#/servers`);
    await page.waitForSelector('table.servers-matrix');
    const initialScanCount = scanCallCount;

    await page.locator('table.servers-matrix input[type="checkbox"]').first().uncheck();
    await page.locator('#servers-toolbar button', { hasText: "Apply" }).click();

    // Bug-bash B1 closure (#7): toolbar shows a count + retry hint;
    // per-row error text lives in the new <ul data-testid=apply-failed-rows>
    // beneath the toolbar so multi-row failures don't render as a wall
    // of text concatenated with `; `.
    await expect(page.locator('#servers-toolbar .error')).toContainText("Failed:");
    await expect(page.locator('[data-testid="apply-failed-rows"]')).toContainText("demo/demigrate/claude-code");
    await expect(page.locator('[data-testid="apply-failed-rows"]')).toContainText("disk full");

    // Reload MUST have run (scan called again since Apply click).
    await expect.poll(() => scanCallCount).toBeGreaterThan(initialScanCount);

    // Apply button stays enabled because dirty retained the failed entry.
    await expect(page.locator('#servers-toolbar button', { hasText: "Apply" })).toBeEnabled();
  });

  // B1 scenario 4: tooltip copy on via-hub cells no longer points at the
  // obsolete `mcphub rollback --client` text.
  test("via-hub cell tooltip describes the Uncheck-and-Apply semantic", async ({ page, hub }) => {
    const scanBody = {
      entries: [
        {
          name: "demo",
          manifest_exists: true,
          client_presence: { "claude-code": { transport: "relay", endpoint: "" } },
        },
      ],
    };
    await page.route("**/api/scan", (r) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(scanBody) }));
    await page.route("**/api/status", (r) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) }));

    await page.goto(`${hub.url}/#/servers`);
    await page.waitForSelector('table.servers-matrix');

    const checkbox = page.locator('table.servers-matrix input[type="checkbox"]').first();
    await expect(checkbox).toHaveAttribute("title", /Uncheck and Apply to roll this binding back/);
    // Negative assertion: the old copy must not survive.
    const title = await checkbox.getAttribute("title");
    expect(title).not.toContain("mcphub rollback --client");
  });

  // B1 scenario 5 (the big one): per-client gate prevents migrate from
  // writing a polluted backup that a retry-demigrate would then misread.
  // Dirty retains failed + gated entries across the first Apply; the
  // second Apply fires exactly one demigrate + one migrate in correct
  // order and does NOT re-fire the truly-successful migrate.
  test("per-client gate: dirty retains failed+gated, second Apply fires 2 POSTs in order", async ({ page, hub }) => {
    const scanBody = {
      entries: [
        {
          name: "A",
          manifest_exists: true,
          client_presence: {
            "claude-code": { transport: "relay", endpoint: "" }, // via-hub → queued demigrate (WILL FAIL)
          },
        },
        {
          name: "B",
          manifest_exists: true,
          client_presence: {
            "claude-code": { transport: "stdio", endpoint: "" }, // direct stdio → queued migrate (GATED)
            "codex-cli":   { transport: "stdio", endpoint: "" }, // direct stdio → queued migrate (SUCCESS)
          },
        },
      ],
    };
    await page.route("**/api/scan", (r) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(scanBody) }));
    await page.route("**/api/status", (r) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) }));

    const log: { url: string; body: string | null; at: number }[] = [];
    let demigrateShouldFail = true;

    await page.route("**/api/demigrate", async (r) => {
      const body = r.request().postData();
      log.push({ url: "demigrate", body, at: Date.now() });
      if (demigrateShouldFail) {
        await r.fulfill({ status: 500, contentType: "application/json", body: JSON.stringify({ error: "disk full" }) });
      } else {
        await r.fulfill({ status: 204, body: "" });
      }
    });
    await page.route("**/api/migrate", async (r) => {
      const body = r.request().postData();
      log.push({ url: "migrate", body, at: Date.now() });
      await r.fulfill({ status: 204, body: "" });
    });

    await page.goto(`${hub.url}/#/servers`);
    await page.waitForSelector('table.servers-matrix');

    // Uncheck (A, claude-code) — queued demigrate.
    await page.locator('table.servers-matrix tr').filter({ hasText: "A" }).locator('input[type="checkbox"]').nth(0).uncheck();
    // Check (B, claude-code) — queued migrate (will be GATED).
    await page.locator('table.servers-matrix tr').filter({ hasText: "B" }).locator('input[type="checkbox"]').nth(0).check();
    // Check (B, codex-cli) — queued migrate (will SUCCEED).
    await page.locator('table.servers-matrix tr').filter({ hasText: "B" }).locator('input[type="checkbox"]').nth(1).check();

    // FIRST Apply.
    await page.locator('#servers-toolbar button', { hasText: "Apply" }).click();

    // Wait for Apply to fully settle: "Failed:" banner appears AND Apply
    // button is re-enabled (setApplying(false) has run). Then assert exact
    // POST count + per-cell shape. Phase 1 per-cell: ONE /api/demigrate
    // for (A, claude-code). Phase 2 per-cell with gate: migrate for
    // (B, claude-code) SKIPPED (gated — failedDemigrateClients has
    // claude-code), migrate for (B, codex-cli) FIRES as its own POST.
    // Total: 2 POSTs, each with a single-element clients array.
    await expect(page.locator('#servers-toolbar .error')).toContainText("Failed:");
    await expect(page.locator('#servers-toolbar button', { hasText: "Apply" })).toBeEnabled();
    expect(log).toHaveLength(2);
    expect(log[0].url).toBe("demigrate");
    expect(JSON.parse(log[0].body!)).toEqual({ servers: ["A"], clients: ["claude-code"] });
    expect(log[1].url).toBe("migrate");
    expect(JSON.parse(log[1].body!)).toEqual({ servers: ["B"], clients: ["codex-cli"] });

    // Un-stub demigrate so the retry succeeds.
    demigrateShouldFail = false;
    log.length = 0;

    // SECOND Apply (no re-toggling; dirty still has the retained entries —
    // the failed demigrate(A) and the gated migrate(B, claude-code)).
    await page.locator('#servers-toolbar button', { hasText: "Apply" }).click();

    // Wait for the success path to durably complete: both retained
    // dirty entries fire their /api/demigrate + /api/migrate POSTs.
    // Poll on the captured log length — it ticks from 0 to 2 as the
    // applyChanges loop fires each cell sequentially. After both
    // arrive, applying becomes false and dirty.size becomes 0
    // (success-prune), so the Apply button is durably disabled.
    //
    // Full-suite flake fix: the prior version of this test asserted
    // `toContainText("Applied.")` on the toolbar span. That span's
    // "Applied. Refreshing…" text is transient — the scan onload in
    // Servers.tsx clears it via the setApplyMsg passthrough as soon
    // as the post-Apply scan refetch lands; Playwright's 100ms
    // polling often misses the visible window under full-suite
    // timing pressure. Asserting `toBeDisabled()` alone also races:
    // the button is disabled DURING the Apply flight (applying=true)
    // and again AFTER success (dirty.size === 0), so Playwright can
    // pass the assertion mid-flight before all POSTs are observed.
    // Polling on log.length is the durable backend-observed signal —
    // exactly one demigrate + one migrate must arrive before the
    // exact-count assertion below.
    await expect.poll(() => log.length, { timeout: 5000 }).toBeGreaterThanOrEqual(2);
    await expect(page.locator('#servers-toolbar button', { hasText: "Apply" })).toBeDisabled();

    // Exact-count assertion: one demigrate(A, claude-code) that now succeeds,
    // one migrate(B, claude-code) that fires because the blocking demigrate
    // succeeded. (B, codex-cli) does NOT re-fire — pruned on first Apply
    // as truly-successful.
    expect(log).toHaveLength(2);
    expect(log[0].url).toBe("demigrate");
    expect(log[1].url).toBe("migrate");
    expect(JSON.parse(log[1].body!)).toEqual({ servers: ["B"], clients: ["claude-code"] });
  });
});
