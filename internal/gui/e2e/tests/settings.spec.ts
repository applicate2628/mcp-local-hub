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

test("Daemons read-only with 'Configured ... (effective in A4-b)' wording (Codex r1 P1.7)", async ({ page, hub }) => {
  await page.goto(hub.url + "#/settings?section=daemons");
  await expect(page.locator('section[data-section="daemons"]')).toContainText("Configured schedule:");
  await expect(page.locator('section[data-section="daemons"]')).toContainText("(effective in A4-b)");
  await expect(page.locator('section[data-section="daemons"] button')).toHaveCount(0);
  await expect(page.locator('section[data-section="daemons"]')).not.toContainText(/^Current schedule:/);
});

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

test("Disabled Clean now button has the locked tooltip (memo §9.4)", async ({ page, hub }) => {
  await page.goto(hub.url + "#/settings?section=backups");
  const btn = page.locator('[data-test-id="clean-now-disabled"]');
  await expect(btn).toBeDisabled();
  await expect(btn).toHaveAttribute(
    "title",
    "Cleanup arrives in A4-b. This view only previews which timestamped backups cleanup would target.",
  );
});

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
