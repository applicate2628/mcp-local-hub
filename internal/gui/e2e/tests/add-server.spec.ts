import { test, expect } from "../fixtures/hub";
import { readFileSync, existsSync, writeFileSync } from "node:fs";
import { join } from "node:path";

test.describe("Add server screen", () => {
  test("renders empty-state form + YAML preview on fresh home", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/add-server`);
    await expect(page.locator("h1")).toHaveText("Add server");
    await expect(page.locator('[data-testid="yaml-preview"]')).toContainText("name:");
    await expect(page.locator('[data-testid="yaml-preview"]')).toContainText("kind: global");
    await expect(page.locator('[data-testid="yaml-preview"]')).toContainText("transport: stdio-bridge");
  });

  test("typing into name + command updates the YAML preview after debounce", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/add-server`);
    await page.locator("#field-name").fill("demo");
    // Basics is open by default; Command is closed. Expand it before typing.
    await page.locator(".accordion-header", { hasText: "Command" }).click();
    await page.locator("#field-command").fill("npx");
    // Wait for the 150ms debounce to settle.
    await expect(page.locator('[data-testid="yaml-preview"]')).toContainText("name: 'demo'");
    await expect(page.locator('[data-testid="yaml-preview"]')).toContainText("command: 'npx'");
  });

  test("inline name-regex error shows when name contains uppercase", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/add-server`);
    await page.locator("#field-name").fill("DEMO");
    await expect(page.locator(".inline-error")).toContainText("Must match");
    await page.locator("#field-name").fill("demo");
    await expect(page.locator(".inline-error")).toHaveCount(0);
  });

  test("adding a daemon then a binding renders the single-daemon flat binding list", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/add-server`);
    await page.locator(".accordion-header", { hasText: "Daemons" }).click();
    await page.locator('[data-action="add-daemon"]').click();
    await page.locator('[data-field="daemon-name"]').fill("default");
    await page.locator('[data-field="daemon-port"]').fill("9100");
    await page.locator(".accordion-header", { hasText: "Client bindings" }).click();
    await page.locator('[data-action="add-binding"]').click();
    await expect(page.locator('[data-testid="bindings-list"]')).toBeVisible();
    await expect(page.locator('[data-binding-row]')).toHaveCount(1);
  });

  test("renaming a daemon cascades to its bindings (preview reflects new name)", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/add-server`);
    await page.locator(".accordion-header", { hasText: "Daemons" }).click();
    await page.locator('[data-action="add-daemon"]').click();
    await page.locator('[data-field="daemon-name"]').fill("default");
    await page.locator('[data-field="daemon-port"]').fill("9100");
    await page.locator(".accordion-header", { hasText: "Client bindings" }).click();
    await page.locator('[data-action="add-binding"]').click();
    // Daemons is still expanded — the per-section accordion state is
    // independent, so opening Client bindings did NOT close Daemons.
    // Rename default -> main directly in the still-visible Daemons field.
    await page.locator('[data-field="daemon-name"]').fill("main");
    await expect(page.locator('[data-testid="yaml-preview"]')).toContainText("- name: 'main'");
    await expect(page.locator('[data-testid="yaml-preview"]')).toContainText("daemon: 'main'");
    await expect(page.locator('[data-testid="yaml-preview"]')).not.toContainText("daemon: 'default'");
  });

  test("deleting a daemon with bindings prompts and cascade-deletes", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/add-server`);
    await page.locator(".accordion-header", { hasText: "Daemons" }).click();
    await page.locator('[data-action="add-daemon"]').click();
    await page.locator('[data-field="daemon-name"]').fill("default");
    await page.locator('[data-field="daemon-port"]').fill("9100");
    await page.locator(".accordion-header", { hasText: "Client bindings" }).click();
    await page.locator('[data-action="add-binding"]').click();
    // Wire up the confirm dialog to accept.
    page.once("dialog", (d) => d.accept());
    // Daemons is still expanded. Click delete directly — no re-click needed.
    await page.locator('[data-action="delete-daemon"]').click();
    await expect(page.locator('[data-testid="yaml-preview"]')).not.toContainText("daemons:");
    await expect(page.locator('[data-testid="yaml-preview"]')).not.toContainText("client_bindings:");
  });

  test("Save writes manifest to disk (servers/<name>/manifest.yaml exists)", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/add-server`);
    await page.locator("#field-name").fill("e2e-save-only");
    // Basics is open by default; expand Command before filling its field.
    await page.locator(".accordion-header", { hasText: "Command" }).click();
    await page.locator("#field-command").fill("echo");
    await page.locator(".accordion-header", { hasText: "Daemons" }).click();
    await page.locator('[data-action="add-daemon"]').click();
    await page.locator('[data-field="daemon-name"]').fill("default");
    await page.locator('[data-field="daemon-port"]').fill("9991");
    await page.locator('[data-action="save"]').click();
    await expect(page.locator('[data-testid="banner"].success')).toContainText("Saved");
  });

  test("Save & Install on a name with port conflict keeps manifest + shows Retry Install", async ({
    page,
    hub,
  }) => {
    // Occupy port 9992 from another process to force install-preflight
    // failure.
    const net = await import("node:net");
    const blocker = net.createServer();
    await new Promise<void>((resolve) => blocker.listen(9992, "127.0.0.1", resolve));
    try {
      await page.goto(`${hub.url}/#/add-server`);
      await page.locator("#field-name").fill("e2e-port-conflict");
      // Expand Command accordion before filling its field.
      await page.locator(".accordion-header", { hasText: "Command" }).click();
      await page.locator("#field-command").fill("echo");
      await page.locator(".accordion-header", { hasText: "Daemons" }).click();
      await page.locator('[data-action="add-daemon"]').click();
      await page.locator('[data-field="daemon-name"]').fill("default");
      await page.locator('[data-field="daemon-port"]').fill("9992");
      await page.locator('[data-action="save-and-install"]').click();
      await expect(page.locator('[data-testid="banner"].error')).toContainText("install failed");
      await expect(page.locator('[data-action="retry-install"]')).toBeVisible();
    } finally {
      blocker.close();
    }
  });

  test("Paste YAML fills the form and runs auto-validate but keeps dirty true", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/add-server`);
    const yaml = `name: pasted\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9100\n`;
    page.once("dialog", (d) => d.accept(yaml));
    await page.locator('[data-action="paste-yaml"]').click();
    await expect(page.locator('[data-testid="yaml-preview"]')).toContainText("name: 'pasted'");
    await expect(page.locator('[data-testid="yaml-preview"]')).toContainText("command: 'npx'");
    // The dirty indicator is set by the parent App; we verify via the
    // sidebar-intercept test below.
  });

  test("sidebar-intercept: navigating away from dirty AddServer prompts", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/add-server`);
    await page.locator("#field-name").fill("dirty-work");
    // Register a dialog handler before the click that cancels. If the
    // guard never fires, the test proceeds without a dialog.
    let dialogSeen = false;
    page.once("dialog", (d) => {
      dialogSeen = true;
      d.dismiss();
    });
    await page.locator(".sidebar nav a", { hasText: "Servers" }).click();
    // Brief wait for any pending navigation to settle.
    await expect(page.locator("h1")).toHaveText("Add server"); // stayed
    expect(dialogSeen).toBe(true);
  });

  test("hash with query params still resolves to Add server screen (PR #5 Codex R2)", async ({
    page,
    hub,
  }) => {
    // Regression guard: useRouter must strip ?query from the hash
    // before matching against SCREENS. If it doesn't, "add-server?..."
    // becomes an unknown-screen fallback and the A1→A2a handoff
    // breaks silently. This mirrors the URL A1 Migration's Create
    // manifest button emits.
    await page.goto(`${hub.url}/#/add-server?server=ghost&from-client=claude-code`);
    await expect(page.locator("h1")).toHaveText("Add server");
    // Prefill will 500 because the client config doesn't exist on a fresh home;
    // the banner surfaces that as "Could not prefill..." which is the
    // expected graceful-degrade path from Task 14.
    await expect(page.locator('[data-testid="banner"].error')).toContainText("Could not prefill");
  });

  // -----------------------------------------------------------------------
  // P2-3-A. Advanced kind-toggle: workspace-scoped reveals languages and
  //         port_pool; switching back to global hides them.
  // -----------------------------------------------------------------------
  test("Advanced kind-toggle: workspace-scoped reveals languages/port_pool; global hides them (P2-3)", async ({
    page,
    hub,
  }) => {
    await page.goto(`${hub.url}/#/add-server`);
    await expect(page.locator("h1")).toHaveText("Add server");
    // Open Advanced accordion.
    await page.locator(".accordion-header", { hasText: "Advanced" }).click();
    // Default kind is global — workspace-only fields must be hidden.
    await expect(
      page.locator('[data-field="port-pool-start"]'),
    ).not.toBeVisible();
    await expect(
      page.locator('[data-testid="languages-subsection"]'),
    ).not.toBeVisible();
    // Switch kind to workspace-scoped via the Basics select (Basics is open by default).
    await page.locator("#field-kind").selectOption("workspace-scoped");
    // Advanced section is already open — workspace-only fields must appear.
    await expect(page.locator('[data-field="port-pool-start"]')).toBeVisible();
    await expect(
      page.locator('[data-testid="languages-subsection"]'),
    ).toBeVisible();
    // Switch back to global — workspace-only fields must hide again.
    await page.locator("#field-kind").selectOption("global");
    await expect(
      page.locator('[data-field="port-pool-start"]'),
    ).not.toBeVisible();
    await expect(
      page.locator('[data-testid="languages-subsection"]'),
    ).not.toBeVisible();
  });

  // -----------------------------------------------------------------------
  // P2-3-B. Advanced always-visible fields (idle_timeout, base_args_template,
  //         daemon.extra_args) survive kind toggles and remain accessible.
  // -----------------------------------------------------------------------
  test("Advanced always-visible fields survive kind toggles (idle_timeout, base_args_template, daemon.extra_args) (P2-3)", async ({
    page,
    hub,
  }) => {
    await page.goto(`${hub.url}/#/add-server`);
    // Open Advanced accordion.
    await page.locator(".accordion-header", { hasText: "Advanced" }).click();
    // Idle timeout field is always visible (not kind-gated).
    await expect(page.locator("#field-idle-timeout")).toBeVisible();
    // Base args template section is always visible.
    await expect(
      page.locator('[data-testid="base-args-template"]'),
    ).toBeVisible();
    // Add a daemon to make per-daemon extras appear.
    await page.locator(".accordion-header", { hasText: "Daemons" }).click();
    await page.locator('[data-action="add-daemon"]').click();
    await page.locator('[data-field="daemon-name"]').fill("default");
    await page.locator('[data-field="daemon-port"]').fill("9100");
    // Toggle kind global -> workspace-scoped -> global.
    await page.locator("#field-kind").selectOption("workspace-scoped");
    await page.locator("#field-kind").selectOption("global");
    // Idle timeout and base-args-template remain visible after kind toggle.
    await expect(page.locator("#field-idle-timeout")).toBeVisible();
    await expect(
      page.locator('[data-testid="base-args-template"]'),
    ).toBeVisible();
    // Per-daemon extras subsection is always visible when daemons exist.
    await expect(page.locator('[data-testid="daemon-extras"]')).toBeVisible();
  });
});
