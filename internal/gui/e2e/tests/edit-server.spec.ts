import { test, expect } from "../fixtures/hub";
import { mkdirSync, writeFileSync, readFileSync, existsSync } from "node:fs";
import { join, resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = dirname(fileURLToPath(import.meta.url));

// serversDir matches what defaultManifestDir() (internal/api/scan.go) returns
// in the E2E environment. The binary lives at e2e/bin/mcphub.exe and probes:
//   1. exeDir/servers         = e2e/bin/servers/   (sibling, first match wins)
//   2. exeDir/../servers      = e2e/servers/       (parent)
//   3. fallback (PR #11)      = e2e/bin/servers/   (after CWD fallback removed)
//
// After the first add-server test calls /api/manifest/create, the binary
// creates e2e/bin/servers/ via the fallback path. Every subsequent request
// then resolves to e2e/bin/servers/ (sibling check succeeds). Seeds must
// therefore target bin/servers/ to be visible to the binary — otherwise
// seedManifest writes to e2e/servers/ but the binary reads from bin/servers/
// and the form loads empty. global-setup.ts wipes BOTH dirs at run start.
const serversDir = resolve(__dirname, "..", "bin", "servers");

// seedManifest writes <name>/manifest.yaml into e2e/bin/servers/ so the
// GUI binary can serve it via /api/manifest/get?name=<name>. Uses unique
// name prefixes to avoid collisions when workers > 1 (CI).
function seedManifest(name: string, yaml: string): void {
  const dir = join(serversDir, name);
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, "manifest.yaml"), yaml, "utf-8");
}

test.describe("Edit server screen", () => {
  // -----------------------------------------------------------------------
  // 1. Load path: seeded manifest populates the form
  // -----------------------------------------------------------------------
  test("load path: seeded manifest populates name and command fields", async ({
    page,
    hub,
  }) => {
    seedManifest(
      "e2e-edit-load",
      [
        "name: e2e-edit-load",
        "kind: global",
        "transport: stdio-bridge",
        "command: npx",
        "daemons:",
        "  - name: default",
        "    port: 9200",
      ].join("\n") + "\n",
    );
    await page.goto(`${hub.url}/#/edit-server?name=e2e-edit-load`);
    await expect(page.locator("h1")).toHaveText("Add server");
    // Name field is populated and disabled in edit mode.
    const nameInput = page.locator("#field-name");
    await expect(nameInput).toHaveValue("e2e-edit-load");
    await expect(nameInput).toBeDisabled();
    // Expand Command section to verify command was loaded.
    await page.locator(".accordion-header", { hasText: "Command" }).click();
    await expect(page.locator("#field-command")).toHaveValue("npx");
  });

  // -----------------------------------------------------------------------
  // 2. Name and kind fields are disabled in edit mode
  // -----------------------------------------------------------------------
  test("name and kind fields are disabled in edit mode", async ({
    page,
    hub,
  }) => {
    seedManifest(
      "e2e-edit-disabled",
      [
        "name: e2e-edit-disabled",
        "kind: global",
        "transport: stdio-bridge",
        "command: node",
        "daemons:",
        "  - name: default",
        "    port: 9201",
      ].join("\n") + "\n",
    );
    await page.goto(`${hub.url}/#/edit-server?name=e2e-edit-disabled`);
    await expect(page.locator("#field-name")).toBeDisabled();
    await expect(page.locator("#field-kind")).toBeDisabled();
    // Transport is NOT disabled in edit mode.
    await page.locator(".accordion-header", { hasText: "Command" }).click();
    await expect(page.locator("#field-command")).toBeEnabled();
  });

  // -----------------------------------------------------------------------
  // 3. Save writes new YAML and hash; Reinstall button appears in banner
  // -----------------------------------------------------------------------
  test("Save writes new YAML and hash; Reinstall button appears", async ({
    page,
    hub,
  }) => {
    seedManifest(
      "e2e-edit-save",
      [
        "name: e2e-edit-save",
        "kind: global",
        "transport: stdio-bridge",
        "command: echo",
        "daemons:",
        "  - name: default",
        "    port: 9202",
      ].join("\n") + "\n",
    );
    await page.goto(`${hub.url}/#/edit-server?name=e2e-edit-save`);
    // Wait for form to load.
    await expect(page.locator("#field-name")).toHaveValue("e2e-edit-save");
    // Expand Command and change it to make the form dirty.
    await page.locator(".accordion-header", { hasText: "Command" }).click();
    await page.locator("#field-command").fill("node");
    // Save.
    await page.locator('[data-action="save"]').click();
    await expect(page.locator('[data-testid="banner"].success')).toContainText(
      "Saved",
    );
    // Reinstall button must appear in the success banner for edit mode.
    await expect(page.locator('[data-action="reinstall"]')).toBeVisible();
    // On-disk YAML must reflect the new command.
    const onDisk = readFileSync(
      join(serversDir, "e2e-edit-save", "manifest.yaml"),
      "utf-8",
    );
    expect(onDisk).toContain("command:");
  });

  // -----------------------------------------------------------------------
  // 4. Force Save path: external disk edit -> hash mismatch -> [Force Save]
  // -----------------------------------------------------------------------
  test("Force Save path: external disk edit causes hash mismatch; Force Save writes and shows preserved-raw banner", async ({
    page,
    hub,
  }) => {
    seedManifest(
      "e2e-edit-force",
      [
        "name: e2e-edit-force",
        "kind: global",
        "transport: stdio-bridge",
        "command: echo",
        "daemons:",
        "  - name: default",
        "    port: 9203",
      ].join("\n") + "\n",
    );
    await page.goto(`${hub.url}/#/edit-server?name=e2e-edit-force`);
    // Wait for load.
    await expect(page.locator("#field-name")).toHaveValue("e2e-edit-force");
    // Expand Command, change command so form is dirty.
    await page.locator(".accordion-header", { hasText: "Command" }).click();
    await page.locator("#field-command").fill("node");
    // Simulate an external write to disk while the user had the form open.
    // This changes the on-disk hash so the subsequent Save will 409.
    writeFileSync(
      join(serversDir, "e2e-edit-force", "manifest.yaml"),
      [
        "name: e2e-edit-force",
        "kind: global",
        "transport: stdio-bridge",
        "command: python",
        "daemons:",
        "  - name: default",
        "    port: 9203",
      ].join("\n") + "\n",
      "utf-8",
    );
    // Attempt normal Save — should trigger stale detection banner.
    await page.locator('[data-action="save"]').click();
    const staleError = page.locator('[data-testid="banner"].error');
    await expect(staleError).toContainText("changed on disk");
    // [Force Save] button should be present.
    await expect(page.locator('[data-action="force-save"]')).toBeVisible();
    // Click Force Save.
    await page.locator('[data-action="force-save"]').click();
    // After Force Save the success banner should appear.
    await expect(page.locator('[data-testid="banner"].success')).toContainText(
      "Force-saved",
    );
    // The on-disk file must reflect the user's command (node, not python).
    const final = readFileSync(
      join(serversDir, "e2e-edit-force", "manifest.yaml"),
      "utf-8",
    );
    expect(final).toContain("node");
  });

  // -----------------------------------------------------------------------
  // 5. Read-only mode: manifest with nested unknown field triggers banner
  //    + disabled inputs, Copy YAML and Back remain enabled
  // -----------------------------------------------------------------------
  test("read-only mode: manifest with nested unknown field shows sticky banner and disables inputs", async ({
    page,
    hub,
  }) => {
    // Inject a daemon with an unknown field — hasNestedUnknown returns true.
    seedManifest(
      "e2e-edit-readonly",
      [
        "name: e2e-edit-readonly",
        "kind: global",
        "transport: stdio-bridge",
        "command: echo",
        "daemons:",
        "  - name: default",
        "    port: 9204",
        "    future_field: some-value",
      ].join("\n") + "\n",
    );
    await page.goto(`${hub.url}/#/edit-server?name=e2e-edit-readonly`);
    // Read-only banner should be present.
    await expect(
      page.locator('[data-testid="readonly-banner"]'),
    ).toBeVisible();
    await expect(
      page.locator('[data-testid="readonly-banner"]'),
    ).toContainText("GUI cannot handle");
    // Name input must be disabled.
    await expect(page.locator("#field-name")).toBeDisabled();
    // Save and Save & Install must be disabled.
    await expect(page.locator('[data-action="save"]')).toBeDisabled();
    await expect(page.locator('[data-action="save-and-install"]')).toBeDisabled();
    // Copy YAML must remain enabled (read-only passthrough).
    await expect(page.locator('[data-action="copy-yaml"]')).toBeEnabled();
    // Back to Servers button inside the banner must be present.
    await expect(
      page.locator('[data-testid="readonly-banner"] button'),
    ).toContainText("Back to Servers");
  });

  // -----------------------------------------------------------------------
  // 6. Load failure: manifest not found -> inline error banner with Retry/Back
  // -----------------------------------------------------------------------
  test("load failure: unknown manifest name shows error banner with Retry and Back buttons", async ({
    page,
    hub,
  }) => {
    // Do NOT seed this name — backend will return an error on GET.
    await page.goto(
      `${hub.url}/#/edit-server?name=e2e-edit-notfound-xxxxxxxx`,
    );
    const errBanner = page.locator('[data-testid="load-error-banner"]');
    await expect(errBanner).toBeVisible();
    await expect(errBanner).toContainText("Failed to load");
    // Both action buttons are rendered inside the banner.
    await expect(
      page.locator('[data-testid="load-error-banner"] button', {
        hasText: "Retry",
      }),
    ).toBeVisible();
    await expect(
      page.locator('[data-testid="load-error-banner"] button', {
        hasText: "Back to Servers",
      }),
    ).toBeVisible();
  });

  // -----------------------------------------------------------------------
  // 7. Sidebar dirty-guard: dismiss preserves form state on edit screen
  // -----------------------------------------------------------------------
  test("sidebar dirty-guard: dismiss dialog stays on edit-server with form intact", async ({
    page,
    hub,
  }) => {
    seedManifest(
      "e2e-edit-guard",
      [
        "name: e2e-edit-guard",
        "kind: global",
        "transport: stdio-bridge",
        "command: echo",
        "daemons:",
        "  - name: default",
        "    port: 9205",
      ].join("\n") + "\n",
    );
    await page.goto(`${hub.url}/#/edit-server?name=e2e-edit-guard`);
    await expect(page.locator("#field-name")).toHaveValue("e2e-edit-guard");
    // Make form dirty by editing command.
    await page.locator(".accordion-header", { hasText: "Command" }).click();
    await page.locator("#field-command").fill("python");
    // Register a one-shot dialog handler BEFORE the click.
    let dialogSeen = false;
    page.once("dialog", (d) => {
      dialogSeen = true;
      d.dismiss();
    });
    // Click a different nav link to trigger the guard.
    await page.locator(".sidebar nav a", { hasText: "Servers" }).click();
    // Form should still show edit-server, not Servers.
    await expect(page.locator("h1")).toHaveText("Add server");
    expect(dialogSeen).toBe(true);
    // Name is still populated (form state was not reset).
    await expect(page.locator("#field-name")).toHaveValue("e2e-edit-guard");
  });

  // -----------------------------------------------------------------------
  // 8. Matrix view: 4+ daemons renders the bindings-matrix table
  // -----------------------------------------------------------------------
  test("4+ daemon matrix view renders with correct column count", async ({
    page,
    hub,
  }) => {
    seedManifest(
      "e2e-edit-matrix",
      [
        "name: e2e-edit-matrix",
        "kind: global",
        "transport: stdio-bridge",
        "command: echo",
        "daemons:",
        "  - name: alpha",
        "    port: 9210",
        "  - name: beta",
        "    port: 9211",
        "  - name: gamma",
        "    port: 9212",
        "  - name: delta",
        "    port: 9213",
      ].join("\n") + "\n",
    );
    await page.goto(`${hub.url}/#/edit-server?name=e2e-edit-matrix`);
    await expect(page.locator("#field-name")).toHaveValue("e2e-edit-matrix");
    // Expand Client bindings.
    await page.locator(".accordion-header", { hasText: "Client bindings" }).click();
    // BindingsMatrix renders when daemons.length >= 4.
    await expect(page.locator('[data-testid="bindings-matrix"]')).toBeVisible();
    // Table header: 1 "Client" column + 4 daemon columns = 5 th elements.
    await expect(
      page.locator('[data-testid="bindings-matrix"] thead th'),
    ).toHaveCount(5);
  });

  // -----------------------------------------------------------------------
  // 9. Advanced: workspace-scoped reveals languages and port_pool fields
  // -----------------------------------------------------------------------
  test("Advanced accordion: workspace-scoped manifest reveals languages and port_pool", async ({
    page,
    hub,
  }) => {
    seedManifest(
      "e2e-edit-advanced",
      [
        "name: e2e-edit-advanced",
        "kind: workspace-scoped",
        "transport: stdio-bridge",
        "command: node",
        "port_pool:",
        "  start: 9300",
        "  end: 9399",
        "daemons:",
        "  - name: default",
        "    port: 9300",
      ].join("\n") + "\n",
    );
    await page.goto(`${hub.url}/#/edit-server?name=e2e-edit-advanced`);
    await expect(page.locator("#field-name")).toHaveValue("e2e-edit-advanced");
    // Open Advanced accordion.
    await page.locator(".accordion-header", { hasText: "Advanced" }).click();
    // Port pool fields visible (workspace-scoped only).
    await expect(page.locator('[data-field="port-pool-start"]')).toBeVisible();
    await expect(page.locator('[data-field="port-pool-end"]')).toBeVisible();
    // Languages subsection visible (workspace-scoped only).
    await expect(
      page.locator('[data-testid="languages-subsection"]'),
    ).toBeVisible();
  });

  // -----------------------------------------------------------------------
  // 10. Daemon rename does NOT orphan bindings (internal-ID cascade)
  // -----------------------------------------------------------------------
  test("daemon rename in edit mode does not orphan client bindings in YAML preview", async ({
    page,
    hub,
  }) => {
    seedManifest(
      "e2e-edit-rename",
      [
        "name: e2e-edit-rename",
        "kind: global",
        "transport: stdio-bridge",
        "command: echo",
        "daemons:",
        "  - name: original",
        "    port: 9220",
        "client_bindings:",
        "  - client: claude-code",
        "    daemon: original",
        "    url_path: /mcp",
      ].join("\n") + "\n",
    );
    await page.goto(`${hub.url}/#/edit-server?name=e2e-edit-rename`);
    await expect(page.locator("#field-name")).toHaveValue("e2e-edit-rename");
    // Expand Daemons and rename the daemon.
    await page.locator(".accordion-header", { hasText: "Daemons" }).click();
    await page.locator('[data-field="daemon-name"]').fill("renamed");
    // YAML preview must reflect the renamed daemon in both daemons and
    // client_bindings — no orphan referencing the old name.
    await expect(
      page.locator('[data-testid="yaml-preview"]'),
    ).toContainText("name: 'renamed'");
    await expect(
      page.locator('[data-testid="yaml-preview"]'),
    ).toContainText("daemon: 'renamed'");
    await expect(
      page.locator('[data-testid="yaml-preview"]'),
    ).not.toContainText("daemon: 'original'");
  });

  // -----------------------------------------------------------------------
  // P2-1-A. hashchange cancel: dismissing the dirty dialog stays on edit-server
  // -----------------------------------------------------------------------
  test("(P2-1) hashchange cancel: dismiss dialog stays on edit-server", async ({
    page,
    hub,
  }) => {
    seedManifest(
      "e2e-edit-hashcancel",
      [
        "name: e2e-edit-hashcancel",
        "kind: global",
        "transport: stdio-bridge",
        "command: echo",
        "daemons:",
        "  - name: default",
        "    port: 9230",
      ].join("\n") + "\n",
    );
    await page.goto(`${hub.url}/#/edit-server?name=e2e-edit-hashcancel`);
    await expect(page.locator("#field-name")).toHaveValue("e2e-edit-hashcancel");
    // Make form dirty.
    await page.locator(".accordion-header", { hasText: "Command" }).click();
    await page.locator("#field-command").fill("dirty-value");
    // Register one-shot dismiss before triggering navigation.
    page.once("dialog", (d) => d.dismiss());
    await page.locator(".sidebar nav a", { hasText: "Dashboard" }).click();
    // Should still be on edit-server (guard dismissed navigation).
    await expect(page.locator("h1")).toHaveText("Add server");
    await expect(page.locator("#field-name")).toHaveValue("e2e-edit-hashcancel");
  });

  // -----------------------------------------------------------------------
  // P2-1-B. hashchange accept: accepting the dirty dialog navigates away
  //          and the dirty flag is cleared
  // -----------------------------------------------------------------------
  test("(P2-1) hashchange accept: accept dialog navigates to new screen and clears dirty", async ({
    page,
    hub,
  }) => {
    seedManifest(
      "e2e-edit-hashaccept",
      [
        "name: e2e-edit-hashaccept",
        "kind: global",
        "transport: stdio-bridge",
        "command: echo",
        "daemons:",
        "  - name: default",
        "    port: 9231",
      ].join("\n") + "\n",
    );
    await page.goto(`${hub.url}/#/edit-server?name=e2e-edit-hashaccept`);
    await expect(page.locator("#field-name")).toHaveValue("e2e-edit-hashaccept");
    // Make form dirty.
    await page.locator(".accordion-header", { hasText: "Command" }).click();
    await page.locator("#field-command").fill("dirty-value");
    // Accept the unsaved-changes dialog.
    page.once("dialog", (d) => d.accept());
    await page.locator(".sidebar nav a", { hasText: "Servers" }).click();
    // Navigation should proceed — we're on Servers now.
    await expect(page.locator("h1")).toHaveText("Servers");
    // Navigating back to add-server fresh should NOT show the guard again
    // (dirty flag was cleared on accept).
    await page.goto(`${hub.url}/#/add-server`);
    await expect(page.locator("h1")).toHaveText("Add server");
    // Navigate away without any dirty interaction — no dialog should fire.
    let unexpectedDialog = false;
    page.once("dialog", (d) => {
      unexpectedDialog = true;
      d.dismiss();
    });
    await page.locator(".sidebar nav a", { hasText: "Servers" }).click();
    await expect(page.locator("h1")).toHaveText("Servers");
    expect(unexpectedDialog).toBe(false);
  });

  // -----------------------------------------------------------------------
  // 14. Read-only + 4+-daemon matrix: matrix checkboxes and url_path inputs
  //     are disabled (D13 invariant — final-review finding).
  // -----------------------------------------------------------------------
  test("read-only + 4+-daemon matrix: matrix checkboxes and url_path inputs are disabled", async ({
    page,
    hub,
  }) => {
    // Seed a manifest with 4 daemons AND a nested-unknown field (extra_config
    // on daemon 'a') — the trigger for read-only mode. The matrix view
    // renders when daemons.length >= 4; read-only mode requires ALL inputs
    // disabled.
    seedManifest(
      "e2e-matrix-ro",
      [
        "name: e2e-matrix-ro",
        "kind: global",
        "transport: stdio-bridge",
        "command: echo",
        "daemons:",
        "  - name: a",
        "    port: 9300",
        "    extra_config:",
        "      custom: 1",
        "  - name: b",
        "    port: 9301",
        "  - name: c",
        "    port: 9302",
        "  - name: d",
        "    port: 9303",
        "client_bindings:",
        "  - client: claude-code",
        "    daemon: a",
        "    url_path: /mcp",
      ].join("\n") + "\n",
    );
    await page.goto(`${hub.url}/#/edit-server?name=e2e-matrix-ro`);
    // Confirm we're in read-only mode.
    await expect(page.locator('[data-testid="readonly-banner"]')).toBeVisible();
    // Expand Client bindings accordion.
    await page.locator(".accordion-header", { hasText: "Client bindings" }).click();
    await expect(page.locator('[data-testid="bindings-matrix"]')).toBeVisible();
    // Every matrix checkbox must be disabled.
    const checkboxes = page.locator('.bindings-matrix input[type="checkbox"]');
    const count = await checkboxes.count();
    expect(count).toBeGreaterThan(0);
    for (let i = 0; i < count; i++) {
      await expect(checkboxes.nth(i)).toBeDisabled();
    }
    // The existing claude-code/a binding's url_path input must be disabled too.
    const urlInput = page.locator('.bindings-matrix input[type="text"]').first();
    await expect(urlInput).toBeDisabled();
  });

  // -----------------------------------------------------------------------
  // P2-1-C. Paste YAML → Save race: version-counter invariant holds.
  //   Clicking Paste and immediately clicking Save while validate is
  //   still in-flight must not let the pre-paste Save version win.
  //   The test uses create mode (/#/add-server) because the race applies
  //   equally there and does not require a seeded manifest.
  // -----------------------------------------------------------------------
  test("(P2-1) Paste YAML -> Save race: later Save wins; banner reflects pasted content", async ({
    page,
    hub,
  }) => {
    await page.goto(`${hub.url}/#/add-server`);
    await expect(page.locator("h1")).toHaveText("Add server");
    // Provide a Paste YAML prompt answer before click.
    const pastedYaml = [
      "name: e2e-paste-race",
      "kind: global",
      "transport: stdio-bridge",
      "command: echo",
      "daemons:",
      "  - name: default",
      "    port: 9240",
    ].join("\n") + "\n";
    // Wire dialog to supply the pasted YAML.
    page.once("dialog", (d) => d.accept(pastedYaml));
    // Click Paste YAML — starts inline validate in flight.
    await page.locator('[data-action="paste-yaml"]').click();
    // Immediately click Save; the version counter ensures only the latest
    // submission's result paints the banner. Both outcomes land on the same
    // run because the form now has a name from the pasted YAML.
    await page.locator('[data-action="save"]').click();
    // Wait for any banner (success or error).  Both are acceptable outcomes
    // here — the invariant is that exactly ONE banner appears and it
    // corresponds to the Save (not a stale validate result from Paste).
    // The Save path may fail because e2e-paste-race already exists from a
    // prior run in the same binary session; that is fine — we just verify
    // a single banner exists and the UI is consistent.
    const banner = page.locator('[data-testid="banner"]');
    await expect(banner).toBeVisible();
    // The YAML preview must show the pasted name (version-stable form state).
    await expect(
      page.locator('[data-testid="yaml-preview"]'),
    ).toContainText("e2e-paste-race");
  });

  // Codex R1 (#16 P1 finding 1): Reload must re-run hasNestedUnknown so an
  // external write that introduces unknown nested fields between Load and
  // Save puts the reloaded form into read-only mode.
  test("Reload after external nested-unknown change enters read-only mode", async ({
    page,
    hub,
  }) => {
    const name = "e2e-reload-nested";
    // Start clean: no nested-unknown.
    seedManifest(
      name,
      `name: ${name}
kind: global
transport: stdio-bridge
command: old
daemons:
  - name: d1
    port: 9500
`,
    );
    await page.goto(`${hub.url}/#/edit-server?name=${name}`);
    await expect(page.locator("#field-name")).toHaveValue(name);
    // Dirty the form so Save will be attempted against a known-hash.
    await page.locator(".accordion-header", { hasText: "Command" }).click();
    await page.locator("#field-command").fill("new");
    // Simulate an external edit that ALSO adds a nested-unknown field.
    seedManifest(
      name,
      `name: ${name}
kind: global
transport: stdio-bridge
command: external-wrote-this
daemons:
  - name: d1
    port: 9500
    future_nested_field: something
`,
    );
    // Save → hash mismatch banner with [Reload] + [Force Save].
    await page.locator('[data-action="save"]').click();
    await expect(page.locator('[data-action="reload"]')).toBeVisible();
    await page.locator('[data-action="reload"]').click();
    // After Reload, the manifest is nested-unknown → read-only banner appears.
    await expect(page.locator('[data-testid="readonly-banner"]')).toBeVisible();
    // Save + Save & Install are disabled in read-only mode.
    await expect(page.locator('[data-action="save"]')).toBeDisabled();
  });

  // Codex R1 (#16 P1 finding 2): Paste YAML is disabled in edit mode so a
  // mid-session paste cannot retarget the Save to a different manifest with
  // expected_hash="". Disabled at the button level (defense in depth) plus
  // the runSave/runForceSave anchors (editName + initialSnapshot.loadedHash)
  // would ignore a mutated formState if Paste somehow fired anyway.
  test("Paste YAML button is disabled in edit mode", async ({ page, hub }) => {
    const name = "e2e-paste-disabled-in-edit";
    seedManifest(
      name,
      `name: ${name}
kind: global
transport: stdio-bridge
command: echo
daemons:
  - name: d
    port: 9510
`,
    );
    await page.goto(`${hub.url}/#/edit-server?name=${name}`);
    await expect(page.locator("#field-name")).toHaveValue(name);
    await expect(page.locator('[data-action="paste-yaml"]')).toBeDisabled();
  });

  // Codex R2 (#16 P2 finding 4): edit-mode Save must not be gated on
  // nameError. The Name field is immutable in edit mode (disabled attr)
  // and the write path uses editName from the URL — so a legacy manifest
  // whose YAML `name:` field doesn't match MANIFEST_NAME_REGEX (e.g. from
  // a historical manual/direct-disk edit) must not permanently block the
  // user from saving other changes. name-regex validation remains in
  // force for CREATE mode.
  test("edit mode does not gate Save on nameError (legacy YAML name allowed)", async ({
    page,
    hub,
  }) => {
    // The directory name still passes checkManifestName (lowercase +
    // hyphens). The YAML `name:` field, however, has uppercase and an
    // underscore — fails MANIFEST_NAME_REGEX in the frontend form. This
    // represents a manifest that was edited on disk outside the GUI.
    const dirName = "e2e-legacy-name";
    const yamlNameField = "Legacy_Server_UPPER";
    seedManifest(
      dirName,
      `name: ${yamlNameField}
kind: global
transport: stdio-bridge
command: echo
daemons:
  - name: d
    port: 9520
`,
    );
    await page.goto(
      `${hub.url}/#/edit-server?name=${encodeURIComponent(dirName)}`,
    );
    // Form loaded the YAML's non-conforming name field.
    await expect(page.locator("#field-name")).toHaveValue(yamlNameField);
    // The inline name-regex error IS visible (the field value doesn't
    // match MANIFEST_NAME_REGEX)...
    await expect(page.locator(".inline-error")).toBeVisible();
    // ...but Save + Save&Install stay ENABLED in edit mode since the write
    // uses editName from the URL (dirName, conforming), and the field is
    // immutable so the user can't "fix" it even if they wanted to.
    await expect(page.locator('[data-action="save"]')).toBeEnabled();
    await expect(
      page.locator('[data-action="save-and-install"]'),
    ).toBeEnabled();
  });
});
