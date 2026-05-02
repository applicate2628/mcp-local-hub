// Phase 3B-II A4-a — Settings screen E2E. Memo §11.3 (16 scenarios).
// Uses the repo's existing fixture API: `import { test, expect } from "../fixtures/hub";`
// and the per-test `{ page, hub }` destructure pattern. Codex r1 P1.1.
import { test, expect } from "../fixtures/hub";
import * as fs from "node:fs/promises";
import * as path from "node:path";

const LIVE_BY_CLIENT: Record<string, string> = {
  "claude-code": ".claude.json",
  "codex-cli": ".codex/config.toml",
  "gemini-cli": ".gemini/settings.json",
  "antigravity": ".gemini/antigravity/mcp_config.json",
};

// Seed N timestamped backups for `client` under hub.home. Backups land
// next to the live config and use the canonical filename pattern from
// internal/api/backups.go: `<liveBase>.bak-mcp-local-hub-<timestamp>`.
async function seedBackups(home: string, client: keyof typeof LIVE_BY_CLIENT, count: number): Promise<void> {
  const live = LIVE_BY_CLIENT[client];
  if (!live) throw new Error(`unknown client ${client}`);
  const fullLive = path.join(home, live);
  await fs.mkdir(path.dirname(fullLive), { recursive: true });
  // Touch the live file so BackupsList's clientFiles(home) lookup includes it.
  try { await fs.access(fullLive); } catch { await fs.writeFile(fullLive, "{}"); }
  const baseName = path.basename(live);
  const dir = path.dirname(fullLive);
  for (let i = 0; i < count; i++) {
    const ts = new Date(Date.now() - i * 86400_000).toISOString().replace(/[:.]/g, "-");
    const bak = path.join(dir, `${baseName}.bak-mcp-local-hub-${ts}`);
    await fs.writeFile(bak, "{}");
  }
}

// Read the persisted gui-preferences.yaml using the same path resolution
// rules as internal/api/settings.go::SettingsPath. Hub fixture sets
// LOCALAPPDATA + XDG_DATA_HOME to `home`, so on Windows the file lands
// at <home>/mcp-local-hub/gui-preferences.yaml.
async function readSettingsYaml(home: string): Promise<string> {
  const candidates = [
    path.join(home, "mcp-local-hub", "gui-preferences.yaml"),                 // LOCALAPPDATA / XDG_DATA_HOME
    path.join(home, ".local", "share", "mcp-local-hub", "gui-preferences.yaml"), // POSIX fallback
  ];
  for (const p of candidates) {
    try { return await fs.readFile(p, "utf8"); } catch { /* try next */ }
  }
  throw new Error("gui-preferences.yaml not found under any known path under " + home);
}

test("Settings sidebar link navigates to settings screen", async ({ page, hub }) => {
  await page.goto(hub.url);
  await page.click('a[href="#/servers"]'); // start somewhere
  await page.click('a[href="#/settings"]');
  await expect(page.locator("h1", { hasText: "Settings" })).toBeVisible();
  expect(page.url()).toContain("#/settings");
});

test("All 5 section headers render", async ({ page, hub }) => {
  await page.goto(hub.url + "#/settings");
  for (const name of ["Appearance", "GUI server", "Daemons", "Backups", "Advanced"]) {
    await expect(page.locator("h2", { hasText: new RegExp(`^${name}$`) })).toBeVisible();
  }
});

test("Deep-link query-string scrolls Backups into view (Codex r1 P1.1)", async ({ page, hub }) => {
  await page.goto(hub.url + "#/settings?section=backups");
  const target = page.locator('section[data-section="backups"]');
  await expect(target).toBeInViewport();
});

test("Sticky inner-nav active state changes on scroll", async ({ page, hub }) => {
  await page.setViewportSize({ width: 1024, height: 600 }); // force scrolling — sections can't all fit on screen
  await page.goto(hub.url + "#/settings");
  // Wait for the IntersectionObserver to fire on load and mark "appearance" active.
  await page.waitForFunction(() => {
    const a = document.querySelector('.settings-section-nav a[href="#/settings?section=appearance"]');
    return a?.classList.contains("active");
  }, null, { timeout: 5000 });
  // Scroll the <main> scroll container (not window) to the bottom so later sections
  // enter the IO's active band. The layout is grid with <main overflow:auto>, so
  // scrolling the window has no effect — the overflow container is <main>.
  await page.evaluate(() => {
    const main = document.getElementById("screen-root");
    if (main) main.scrollTo({ top: main.scrollHeight, behavior: "instant" });
  });
  // Wait for the active link to move away from "appearance".
  await page.waitForFunction(() => {
    const a = document.querySelector('.settings-section-nav a[href="#/settings?section=appearance"]');
    return !a?.classList.contains("active");
  }, null, { timeout: 5000 });
});

test("Save Appearance round-trips to gui-preferences.yaml", async ({ page, hub }) => {
  await page.goto(hub.url + "#/settings");
  await page.locator("#appearance\\.theme").selectOption("dark");
  await page.locator('section[data-section="appearance"] button:has-text("Save")').click();
  await expect(page.locator(".save-banner.ok")).toBeVisible();
  await page.reload();
  await page.click('a[href="#/settings"]');
  await expect(page.locator("#appearance\\.theme")).toHaveValue("dark");
  const yaml = await readSettingsYaml(hub.home);
  expect(yaml).toMatch(/appearance\.theme:\s*dark/);
});

test("Save validation failure shows inline error + keeps key dirty", async ({ page, hub }) => {
  await page.goto(hub.url + "#/settings?section=gui_server");
  await page.locator("#gui_server\\.port").fill("99");
  await page.locator('section[data-section="gui_server"] button:has-text("Save")').click();
  await expect(page.locator(".save-banner.partial")).toBeVisible();
  await expect(page.locator('#gui_server\\.port-error[role="alert"]')).toBeVisible();
});

test("Port pending-restart badge appears after Save (Codex r3 P2.1 + r4 P2.1)", async ({ page, hub }) => {
  // The binary runs on --port 0 (ephemeral), so on initial load the persisted
  // default (9125) != actual port → badge is CORRECTLY visible from the start.
  // The semantic is "persisted vs running", not "draft vs running".
  await page.goto(hub.url + "#/settings?section=gui_server");
  const badge = page.locator('[data-test-id="port-restart-badge"]');
  // Badge is visible on load because persisted=9125 != actual=ephemeral.
  await expect(badge).toBeVisible();
  const badgeTextBefore = await badge.textContent();
  // The badge should mention the persisted default (9125).
  expect(badgeTextBefore).toContain("9125");
  // Type a new port into the draft field.
  await page.locator("#gui_server\\.port").fill("9200");
  // Codex r4 P2.1: dirty draft does NOT flip badge — persisted is still 9125.
  await expect(badge).toBeVisible();
  expect(await badge.textContent()).toContain("9125");
  // Save → persisted becomes 9200.
  await page.locator('section[data-section="gui_server"] button:has-text("Save")').click();
  await expect(page.locator(".save-banner.ok")).toBeVisible();
  // Badge now mentions 9200 (persisted=9200 != actual=ephemeral).
  await expect(badge).toBeVisible();
  expect(await badge.textContent()).toContain("9200");
});

// "Daemons read-only with Configured ... (effective in A4-b)" test removed —
// A4-b PR #1 Task 1 flipped Deferred:false on weekly_schedule + retry_policy
// and Task 11 rewrote SectionDaemons to be editable. The new editable surface
// is covered end-to-end by the A4-b PR #1 T1-T5 scenarios at the bottom of
// this file (membership table render, toggle persistence, Select all/Clear all,
// cron valid + invalid).

test("Backups list renders 4 client groups", async ({ page, hub }) => {
  await page.goto(hub.url + "#/settings?section=backups");
  await expect(page.locator(".backups-client-group")).toHaveCount(4);
  for (const c of ["claude-code", "codex-cli", "gemini-cli", "antigravity"]) {
    await expect(page.locator(".backups-client-group summary", { hasText: c })).toBeVisible();
  }
});

test("Backups preview marks would-prune rows", async ({ page, hub }) => {
  await seedBackups(hub.home, "claude-code", 7);
  await page.goto(hub.url + "#/settings?section=backups");
  // Set keep_n=3 → expect 4 rows (oldest) tagged eligible.
  // Use evaluate + dispatchEvent("input") because Playwright's fill() on
  // <input type="range"> does not reliably fire the onInput event in headless
  // Chromium; the framework uses onInput to update draft state, so without
  // the explicit event the preview API is never called with keep_n=3.
  await page.locator("#backups-keep-n-slider").evaluate((el: HTMLInputElement) => {
    el.value = "3";
    el.dispatchEvent(new Event("input", { bubbles: true }));
  });
  // Wait for debounced preview (250ms debounce + RTT margin).
  await page.waitForTimeout(500);
  const eligible = page.locator('[data-testid="eligible-badge"]');
  await expect(eligible.first()).toBeVisible();
  expect(await eligible.count()).toBeGreaterThanOrEqual(4);
});

// "Disabled Clean now button has the locked tooltip" test removed — A4-b PR #1
// Task 1 flipped Deferred:false on backups.clean_now and Task 10 wired it via
// ConfirmModal. The new active flow is covered by T6 in the A4-b PR #1 block
// (clean-now ConfirmModal Cancel preserves; Confirm invokes endpoint).

test("Open app-data folder action triggers POST (mocked, no real spawn)", async ({ page, hub }) => {
  // Codex r2 P2: intercept the POST so the real backend never actually
  // shells out to explorer.exe / open / xdg-open during the E2E run.
  // The test asserts only that the GUI issues the right POST.
  await page.route("**/api/settings/advanced.open_app_data_folder", async (route, req) => {
    if (req.method() !== "POST") {
      await route.fallback();
      return;
    }
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ opened: "/mocked/path" }),
    });
  });
  await page.goto(hub.url + "#/settings?section=advanced");
  const postPromise = page.waitForRequest((req) =>
    req.method() === "POST" && req.url().endsWith("/api/settings/advanced.open_app_data_folder"),
  );
  await page.locator('[data-test-id="open-folder"]').click();
  await postPromise;
});

test("Discard-key remount: confirmed in-screen discard resets section state (Codex r2 P1, memo §10.4)", async ({ page, hub }) => {
  // Edit Appearance, navigate intra-Settings, confirm discard, verify
  // that local draft is gone (the saved snapshot value is restored).
  await page.goto(hub.url + "#/settings?section=appearance");
  await page.locator("#appearance\\.theme").selectOption("dark");
  // Intra-Settings hash navigation triggers the dirty-guard. Confirm.
  page.once("dialog", (d) => d.accept());
  await page.locator('a[href="#/settings?section=backups"]').click();
  // Hop back to Appearance and assert the draft is gone.
  await page.locator('a[href="#/settings?section=appearance"]').click();
  await expect(page.locator("#appearance\\.theme")).toHaveValue("system");
});

test("Dirty-guard prompts when navigating away from dirty Settings", async ({ page, hub }) => {
  await page.goto(hub.url + "#/settings");
  await page.locator("#appearance\\.theme").selectOption("dark");
  page.once("dialog", (d) => d.dismiss());
  await page.locator('a[href="#/servers"]').click();
  expect(page.url()).toContain("#/settings");
});

test("Per-section Save isolation", async ({ page, hub }) => {
  await page.goto(hub.url + "#/settings");
  await page.locator("#appearance\\.theme").selectOption("dark");
  await page.locator("#gui_server\\.browser_on_launch").click();
  await page.locator('section[data-section="appearance"] button:has-text("Save")').click();
  await expect(page.locator('section[data-section="appearance"] .save-banner.ok')).toBeVisible();
  const guiSaveBtn = page.locator('section[data-section="gui_server"] button:has-text("Save")');
  await expect(guiSaveBtn).toBeEnabled();
});

test("Deferred field 'tray' rendered disabled with (coming in A4-b)", async ({ page, hub }) => {
  await page.goto(hub.url + "#/settings?section=gui_server");
  await expect(page.locator("#gui_server\\.tray")).toBeDisabled();
  await expect(page.locator('section[data-section="gui_server"]')).toContainText("coming in A4-b");
});

// ---------------------------------------------------------------------------
// A4-b PR #1 scenarios (Task 15)
// ---------------------------------------------------------------------------
//
// Seed helper: workspaces.yaml lives at <home>/mcp-local-hub/workspaces.yaml
// on Windows because the hub fixture sets LOCALAPPDATA=home.
// DefaultRegistryPath() checks LOCALAPPDATA first on Windows.

type WorkspaceRow = {
  key: string;
  path: string;
  lang: string;
  weekly: boolean;
};

async function seedWorkspacesYAML(home: string, rows: WorkspaceRow[]): Promise<void> {
  const lines: string[] = ["version: 1", "workspaces:"];
  for (const r of rows) {
    const port = 9100 + Math.floor(Math.random() * 100);
    const taskName = `t-${r.key.replace(/[^a-z0-9]/gi, "_")}-${r.lang}`;
    lines.push(`  - workspace_key: ${r.key}`);
    lines.push(`    workspace_path: ${r.path}`);
    lines.push(`    language: ${r.lang}`);
    lines.push(`    backend: mcp-language-server`);
    lines.push(`    port: ${port}`);
    lines.push(`    task_name: ${taskName}`);
    lines.push(`    client_entries: {}`);
    lines.push(`    weekly_refresh: ${r.weekly}`);
  }
  // LOCALAPPDATA=home on Windows → mcp-local-hub subdir is the registry dir
  const dir = path.join(home, "mcp-local-hub");
  await fs.mkdir(dir, { recursive: true });
  await fs.writeFile(path.join(dir, "workspaces.yaml"), lines.join("\n") + "\n");
}

test.describe("A4-b PR #1: Settings lifecycle", () => {
  // Test 1 — Membership table renders mixed initial state
  test("Membership table renders mixed initial state (seed 2 rows with weekly true/false)", async ({ page, hub }) => {
    await seedWorkspacesYAML(hub.home, [
      { key: "D:/dev/proj-alpha", path: "D:/dev/proj-alpha", lang: "python",     weekly: true  },
      { key: "D:/dev/proj-beta",  path: "D:/dev/proj-beta",  lang: "typescript", weekly: false },
    ]);
    await page.goto(hub.url + "#/settings?section=daemons");
    // Wait for membership table to finish loading (replaces "Loading workspaces…")
    await expect(page.locator('[data-testid="weekly-membership-table"]')).not.toContainText("Loading workspaces");
    // Row for proj-alpha / python should be checked
    const alpha = page.locator('[data-testid="membership-D:/dev/proj-alpha-python"]');
    await expect(alpha).toBeChecked();
    // Row for proj-beta / typescript should be unchecked
    const beta = page.locator('[data-testid="membership-D:/dev/proj-beta-typescript"]');
    await expect(beta).not.toBeChecked();
  });

  // Test 2 — Toggle one row + Save → reload → state persisted
  test("Toggle one row + Save persists to disk (reload round-trip)", async ({ page, hub }) => {
    await seedWorkspacesYAML(hub.home, [
      { key: "D:/dev/ws-toggle", path: "D:/dev/ws-toggle", lang: "go", weekly: true },
    ]);
    await page.goto(hub.url + "#/settings?section=daemons");
    // Wait for membership table rows
    await expect(page.locator('[data-testid="weekly-membership-table"]')).not.toContainText("Loading workspaces");
    const checkbox = page.locator('[data-testid="membership-D:/dev/ws-toggle-go"]');
    await expect(checkbox).toBeChecked();
    // Toggle from true → false
    await checkbox.click();
    await expect(checkbox).not.toBeChecked();
    // Save (daemons Save button)
    await page.locator('[data-testid="daemons-save"]').click();
    await expect(page.locator('[data-testid="daemons-save-banner"]')).toBeVisible();
    await expect(page.locator('[data-testid="daemons-save-banner"]')).toContainText("Saved");
    // Reload and re-open settings section
    await page.goto(hub.url + "#/settings?section=daemons");
    await expect(page.locator('[data-testid="weekly-membership-table"]')).not.toContainText("Loading workspaces");
    // State must be persisted as false after reload
    await expect(page.locator('[data-testid="membership-D:/dev/ws-toggle-go"]')).not.toBeChecked();
  });

  // Test 3 — Select all / Clear all bulk affordance
  test("Select all flips every row to enabled; Clear all flips every row to disabled", async ({ page, hub }) => {
    await seedWorkspacesYAML(hub.home, [
      { key: "D:/dev/ws-a", path: "D:/dev/ws-a", lang: "rust",   weekly: false },
      { key: "D:/dev/ws-b", path: "D:/dev/ws-b", lang: "python", weekly: true  },
    ]);
    await page.goto(hub.url + "#/settings?section=daemons");
    await expect(page.locator('[data-testid="weekly-membership-table"]')).not.toContainText("Loading workspaces");
    // Click "Select all" — both rows should become checked
    await page.locator('[data-testid="weekly-membership-select-all"]').click();
    await expect(page.locator('[data-testid="membership-D:/dev/ws-a-rust"]')).toBeChecked();
    await expect(page.locator('[data-testid="membership-D:/dev/ws-b-python"]')).toBeChecked();
    // Click "Clear all" — both rows should become unchecked
    await page.locator('[data-testid="weekly-membership-clear-all"]').click();
    await expect(page.locator('[data-testid="membership-D:/dev/ws-a-rust"]')).not.toBeChecked();
    await expect(page.locator('[data-testid="membership-D:/dev/ws-b-python"]')).not.toBeChecked();
  });

  // Test 4 — Cron edit valid + Save → "Saved." banner
  // The E2E environment runs MCPHUB_E2E_SCHEDULER=none which makes the
  // real scheduler reject SwapWeeklyTrigger. We intercept the PUT route
  // and return a mocked 200 so the frontend's "Saved." success path is
  // exercised end-to-end, matching the same pattern used by the open-
  // app-data-folder test (line 181 above).
  test("Cron edit valid + Save shows Saved banner (schedule route mocked OK)", async ({ page, hub }) => {
    await page.route("**/api/daemons/weekly-schedule", async (route, req) => {
      if (req.method() !== "PUT") { await route.fallback(); return; }
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ updated: true, schedule: "weekly Tue 14:30", restore_status: "n/a" }),
      });
    });
    await page.goto(hub.url + "#/settings?section=daemons");
    const schedInput = page.locator('[data-testid="daemons-weekly-schedule-input"]');
    // Wait for snapshot to anchor the input before editing — filling before
    // the re-anchor useEffect fires causes the persisted default to overwrite
    // the typed value, leaving schedDirty=false and the Save button disabled.
    await expect(schedInput).not.toHaveValue("");
    // Change the schedule input
    await schedInput.fill("weekly Tue 14:30");
    const saveBtn = page.locator('[data-testid="daemons-save"]');
    // Wait for Preact to re-render with dirty=true before clicking.
    await expect(saveBtn).toBeEnabled();
    await saveBtn.click();
    // Expect the "Saved." success banner
    const banner = page.locator('[data-testid="daemons-save-banner"]');
    await expect(banner).toBeVisible();
    await expect(banner).toContainText("Saved");
  });

  // Test 5 — Cron edit invalid string → inline parse-error visible with canonical example
  // The real PUT /api/daemons/weekly-schedule returns 400 parse_error for
  // unsupported formats. "daily 03:00" is not accepted by ParseSchedule
  // (only "weekly DAY HH:MM" is valid per memo D7). The response carries
  // {error:"parse_error", detail:"...", example:"weekly Sun 03:00"}.
  test("Cron edit invalid string shows inline parse-error with canonical example", async ({ page, hub }) => {
    await page.goto(hub.url + "#/settings?section=daemons");
    const schedInput = page.locator('[data-testid="daemons-weekly-schedule-input"]');
    // Wait for snapshot to load: the input starts empty during load and is
    // populated via useEffect once the settings snapshot resolves. Filling
    // before that fires causes SectionDaemons's re-anchor effect to overwrite
    // our typed value with the persisted default, leaving schedDirty=false and
    // the Save button disabled. Waiting for a non-empty value ensures the anchor
    // is stable before we modify it.
    await expect(schedInput).not.toHaveValue("");
    // Now fill with an invalid schedule
    await schedInput.fill("daily 03:00");
    const saveBtn = page.locator('[data-testid="daemons-save"]');
    // Wait for Preact to re-render with dirty=true before clicking.
    await expect(saveBtn).toBeEnabled();
    await saveBtn.click();
    // The inline error element should appear under the schedule input
    const inlineError = page.locator('[id="daemons-weekly-schedule-error"]');
    await expect(inlineError).toBeVisible();
    // The canonical example from memo D8 must appear in the error text
    await expect(inlineError).toContainText("weekly Sun 03:00");
  });

  // Test 6 — Clean-now ConfirmModal → Cancel preserves backups (no fetch); Confirm invokes endpoint
  //
  // Implementation note: SectionBackups and SectionAdvancedDiagnostics both
  // render a <ConfirmModal data-testid="confirm-modal"> in the DOM. To avoid
  // Playwright's strict-mode "resolved to 2 elements" error we scope the
  // locator to the Backups section using `.within()` style — filter on the
  // modal that is inside section[data-section="backups"].
  test("Clean-now Cancel closes modal without invoking endpoint; Confirm invokes it", async ({ page, hub }) => {
    await page.goto(hub.url + "#/settings?section=backups");
    // Scope all modal locators to the backups section to avoid ambiguity
    // with the diagnostics ConfirmModal that is also in the DOM.
    const backupsSection = page.locator('section[data-section="backups"]');
    // Dialogs rendered by SectionBackups are siblings of the section in the
    // DOM (Preact renders <dialog> into the nearest parent that is not the
    // modal itself). Use page-level locator scoped by the "open" attribute
    // to target only the backups clean dialog once it opens.

    // --- Cancel path: verify no POST fires ---
    // cleanBackups() in settings-api.ts calls POST /api/backups/clean
    let cleanCalled = false;
    await page.route("**/api/backups/clean", async (route, req) => {
      if (req.method() === "POST") {
        cleanCalled = true;
        await route.fallback();
        return;
      }
      await route.fallback();
    });
    await backupsSection.locator('[data-testid="clean-now-button"]').click();
    // Wait for the open dialog that contains "Delete eligible backups?" text
    const openModal = page.locator('[data-testid="confirm-modal"][open]');
    await expect(openModal).toContainText("Delete eligible backups?");
    await openModal.locator('[data-testid="confirm-modal-cancel"]').click();
    await expect(openModal).not.toBeVisible();
    expect(cleanCalled).toBe(false);

    // --- Confirm path: verify endpoint is invoked ---
    // Intercept and mock the POST so we don't actually delete files.
    // Note: POST /api/backups/clean is called by cleanBackups() in settings-api.ts.
    let confirmFired = false;
    await page.route("**/api/backups/clean", async (route, req) => {
      if (req.method() === "POST") {
        confirmFired = true;
        await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ cleaned: 0 }) });
        return;
      }
      await route.fallback();
    });
    await backupsSection.locator('[data-testid="clean-now-button"]').click();
    await expect(openModal).toContainText("Delete eligible backups?");
    const confirmReq = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().endsWith("/api/backups/clean"),
    );
    await openModal.locator('[data-testid="confirm-modal-confirm"]').click();
    await confirmReq;
    expect(confirmFired).toBe(true);
  });

  // Test 7 — Export bundle button → response stream → file download triggered
  // The real POST /api/export-config-bundle returns a ZIP stream with
  // Content-Disposition: attachment; filename="mcphub-bundle-<ts>.zip".
  // We mock the route to return a minimal ZIP stub so Playwright can
  // observe the download event without depending on the real bundle size.
  test("Export bundle button triggers file download with mcphub-bundle- filename", async ({ page, hub }) => {
    // Intercept to return a minimal stub so download fires reliably in headless
    await page.route("**/api/export-config-bundle", async (route, req) => {
      if (req.method() !== "POST") { await route.fallback(); return; }
      // Minimal valid ZIP local file header magic bytes + empty central directory
      const minZip = Buffer.from(
        "504b0506" + "0000000000000000000000000000",
        "hex",
      );
      await route.fulfill({
        status: 200,
        headers: {
          "Content-Type": "application/zip",
          "Content-Disposition": `attachment; filename="mcphub-bundle-test.zip"`,
        },
        body: minZip,
      });
    });
    await page.goto(hub.url + "#/settings?section=advanced");
    const downloadPromise = page.waitForEvent("download");
    await page.locator('[data-testid="export-bundle"]').click();
    const download = await downloadPromise;
    // The frontend synthesizes the filename from the current timestamp so
    // we cannot assert the exact name; we assert prefix + extension.
    expect(download.suggestedFilename()).toMatch(/^mcphub-bundle-.+\.zip$/);
  });

  // Test 8 — Force-kill probe → Healthy verdict → no kill button
  // In E2E the running mcphub process IS the single-instance holder and
  // answers /api/ping (ping_match===true), so Probe returns Healthy.
  // The canKill() client gate (pid_alive===true && ping_match===false)
  // is NOT satisfied → kill button must NOT render.
  test("Force-kill probe Healthy verdict shows verdict strip but no kill button", async ({ page, hub }) => {
    await page.goto(hub.url + "#/settings?section=advanced");
    await page.locator('[data-testid="probe-button"]').click();
    // Wait for verdict strip to appear
    await expect(page.locator('[data-testid="verdict-strip"]')).toBeVisible();
    // Healthy instance → kill button must be absent
    await expect(page.locator('[data-testid="kill-button"]')).toHaveCount(0);
  });
});
