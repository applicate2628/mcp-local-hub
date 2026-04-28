// Codex PR #20 r2 P1 — verify that App.tsx applies appearance attributes at
// bootstrap (i.e. before the user ever visits the Settings screen).
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, waitFor, cleanup } from "@testing-library/preact";
import { App } from "./app";
import * as settingsApi from "./lib/settings-api";
import type { SettingsEnvelope } from "./lib/settings-types";
import { isConfig } from "./lib/settings-types";

// Minimal SettingsEnvelope with non-default appearance values so the test
// can confirm the attributes were written (not just that defaults were kept).
const fakeEnv: SettingsEnvelope = {
  actual_port: 9125,
  settings: [
    {
      key: "appearance.theme",
      section: "appearance",
      type: "enum",
      default: "system",
      value: "dark",
      enum: ["light", "dark", "system"],
      deferred: false,
      help: "",
    },
    {
      key: "appearance.density",
      section: "appearance",
      type: "enum",
      default: "comfortable",
      value: "spacious",
      enum: ["compact", "comfortable", "spacious"],
      deferred: false,
      help: "",
    },
    {
      key: "appearance.shell",
      section: "appearance",
      type: "enum",
      default: "pwsh",
      value: "pwsh",
      enum: ["pwsh", "cmd", "bash", "zsh", "git-bash"],
      deferred: false,
      help: "",
    },
    {
      key: "gui_server.browser_on_launch",
      section: "gui_server",
      type: "bool",
      default: "true",
      value: "true",
      deferred: false,
      help: "",
    },
  ],
};

beforeEach(() => {
  cleanup();
  vi.restoreAllMocks();
  // Stub the settings API so the snapshot hook resolves immediately.
  vi.spyOn(settingsApi, "getSettings").mockResolvedValue(fakeEnv);
  // Remove any previously-set attributes so each test starts clean.
  document.documentElement.removeAttribute("data-theme");
  document.documentElement.removeAttribute("data-density");
});
afterEach(() => { cleanup(); });

describe("App — global appearance attribute application (Codex PR #20 r2 P1)", () => {
  it("applies data-theme and data-density at app bootstrap without visiting Settings", async () => {
    // Start on the default Servers route — Settings is NOT rendered.
    window.location.hash = "#/servers";
    render(<App />);

    // Wait for the App-level snapshot to resolve and write the attributes.
    await waitFor(() => {
      expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
    });
    expect(document.documentElement.getAttribute("data-density")).toBe("spacious");
  });
});

describe("App — lifted snapshot ownership (Codex PR #20 r11 P2)", () => {
  it("applies theme/density even when fetch is slow — single pipeline, no overwrite race", async () => {
    // App is the SOLE owner of useSettingsSnapshot. Settings.tsx no longer
    // creates a duplicate hook instance. So even if App's fetch is slow,
    // there is only one apply pipeline — no competing instance can overwrite
    // the attributes with stale values after Save.
    const goodEnvelope: SettingsEnvelope = fakeEnv;
    const slowSnapshot = new Promise<SettingsEnvelope>((resolve) => {
      setTimeout(() => resolve(goodEnvelope), 100);
    });
    vi.spyOn(settingsApi, "getSettings").mockReturnValue(slowSnapshot);

    window.location.hash = "#/servers";
    render(<App />);

    // Narrow to ConfigSettingDTO to access .value — ActionSettingDTO has no .value.
    const themeEntry = goodEnvelope.settings.find((s) => s.key === "appearance.theme")!;
    const densityEntry = goodEnvelope.settings.find((s) => s.key === "appearance.density")!;
    const expectedTheme = isConfig(themeEntry) ? themeEntry.value : "";
    const expectedDensity = isConfig(densityEntry) ? densityEntry.value : "";

    // Wait up to 500 ms for the (deliberately slow) fetch to resolve and for
    // App's useEffect to write the attributes to <html>.
    await waitFor(
      () => expect(document.documentElement.getAttribute("data-theme")).toBe(expectedTheme),
      { timeout: 500 },
    );
    expect(document.documentElement.getAttribute("data-density")).toBe(expectedDensity);
  });
});
