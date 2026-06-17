// Codex PR #20 r2 P1 — verify that App.tsx applies appearance attributes at
// bootstrap (i.e. before the user ever visits the Settings screen).
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, waitFor, cleanup } from "@testing-library/preact";
import userEvent from "@testing-library/user-event";
import { App } from "./app";
import * as settingsApi from "./lib/settings-api";
import type { SettingsEnvelope } from "./lib/settings-types";
import { isConfig } from "./lib/settings-types";

// happy-dom does not ship EventSource. Servers + Migration now subscribe
// to /api/events for the tray "rescan-clients" event (PR #N), so a bare
// render(<App/>) on those routes hits `new EventSource(...)` and crashes.
// Stub matches the Dashboard test's pattern: minimal API surface, no I/O.
class StubEventSource {
  url: string;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  constructor(url: string) { this.url = url; }
  addEventListener(_t: string, _l: (ev: MessageEvent) => void) {}
  removeEventListener(_t: string, _l: (ev: MessageEvent) => void) {}
  close() {}
}
(globalThis as unknown as { EventSource: typeof StubEventSource }).EventSource = StubEventSource;

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

describe("App — sidebar keyboard navigation (a11y residual)", () => {
  it("exposes the nav as a labelled landmark with aria-current on the active link", async () => {
    window.location.hash = "#/servers";
    const { container } = render(<App />);

    const nav = await waitFor(() => {
      const el = container.querySelector("nav");
      if (!el) throw new Error("nav not yet rendered");
      return el;
    });
    expect(nav.getAttribute("aria-label")).toBe("Primary");

    // Exactly one link is the active screen and it carries aria-current=page.
    const current = nav.querySelectorAll('a[aria-current="page"]');
    expect(current.length).toBe(1);
    expect(current[0].textContent).toBe("Servers");
    expect(current[0].classList.contains("active")).toBe(true);

    // Inactive links must NOT carry aria-current (so AT announces one page).
    const links = Array.from(nav.querySelectorAll("a"));
    const others = links.filter((a) => a.getAttribute("aria-current") !== "page");
    expect(others.length).toBe(links.length - 1);
    others.forEach((a) => expect(a.getAttribute("aria-current")).toBeNull());
  });

  it("Arrow/Home/End keys rove focus between sibling nav links without navigating", async () => {
    const user = userEvent.setup();
    window.location.hash = "#/servers";
    const { container } = render(<App />);

    const nav = await waitFor(() => {
      const el = container.querySelector("nav");
      if (!el) throw new Error("nav not yet rendered");
      return el;
    });
    const links = Array.from(nav.querySelectorAll<HTMLAnchorElement>("a"));
    expect(links.length).toBeGreaterThan(2);

    // Focus the first link, then arrow down → second link gains focus.
    links[0].focus();
    expect(document.activeElement).toBe(links[0]);
    await user.keyboard("{ArrowDown}");
    expect(document.activeElement).toBe(links[1]);

    // ArrowUp from the first wraps to the last.
    links[0].focus();
    await user.keyboard("{ArrowUp}");
    expect(document.activeElement).toBe(links[links.length - 1]);

    // End jumps to the last, Home back to the first.
    links[0].focus();
    await user.keyboard("{End}");
    expect(document.activeElement).toBe(links[links.length - 1]);
    await user.keyboard("{Home}");
    expect(document.activeElement).toBe(links[0]);

    // Roving moved focus only — the route (hash) did not change.
    expect(window.location.hash).toBe("#/servers");
  });
});
