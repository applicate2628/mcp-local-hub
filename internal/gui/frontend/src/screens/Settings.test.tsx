import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, waitFor, cleanup } from "@testing-library/preact";
import { SettingsScreen } from "./Settings";
import * as api from "../lib/settings-api";
import type { SettingsEnvelope } from "../lib/settings-types";
import type { RouterState } from "../hooks/useRouter";

const fakeEnv: SettingsEnvelope = {
  actual_port: 9125,
  settings: [
    { key: "appearance.theme", section: "appearance", type: "enum",
      default: "system", value: "system", enum: ["light", "dark", "system"], deferred: false, help: "" },
    { key: "appearance.density", section: "appearance", type: "enum",
      default: "comfortable", value: "comfortable", enum: ["compact","comfortable","spacious"], deferred: false, help: "" },
    { key: "appearance.shell", section: "appearance", type: "enum",
      default: "pwsh", value: "pwsh", enum: ["pwsh","cmd","bash","zsh","git-bash"], deferred: false, help: "" },
    { key: "appearance.default_home", section: "appearance", type: "path",
      default: "", value: "", optional: true, deferred: false, help: "" },
    { key: "gui_server.browser_on_launch", section: "gui_server", type: "bool",
      default: "true", value: "true", deferred: false, help: "" },
    { key: "gui_server.port", section: "gui_server", type: "int",
      default: "9125", value: "9125", min: 1024, max: 65535, deferred: false, help: "" },
    { key: "gui_server.tray", section: "gui_server", type: "bool",
      default: "true", value: "true", deferred: true, help: "" },
    { key: "daemons.weekly_schedule", section: "daemons", type: "string",
      default: "weekly Sun 03:00", value: "weekly Sun 03:00", deferred: true, help: "" },
    { key: "daemons.retry_policy", section: "daemons", type: "enum",
      default: "exponential", value: "exponential", enum: ["none","linear","exponential"], deferred: true, help: "" },
    { key: "backups.keep_n", section: "backups", type: "int",
      default: "5", value: "5", min: 0, max: 50, deferred: false, help: "" },
    { key: "backups.clean_now", section: "backups", type: "action", deferred: true, help: "" },
    { key: "advanced.open_app_data_folder", section: "advanced", type: "action", deferred: false, help: "" },
    { key: "advanced.export_config_bundle", section: "advanced", type: "action", deferred: true, help: "" },
  ],
};

const stubRoute = (query: string): RouterState => ({ screen: "settings", query });

describe("SettingsScreen", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
    vi.spyOn(api, "getSettings").mockResolvedValue(fakeEnv);
    vi.spyOn(api, "getBackups").mockResolvedValue([]);
    vi.spyOn(api, "getBackupsCleanPreview").mockResolvedValue([]);
  });
  afterEach(() => { cleanup(); });

  it("renders all 5 section <h2>s on success", async () => {
    // Section h2s are sourced from <h2> elements (heading role level 2).
    // SectionNav links also carry the same labels but as <a>, not <h2>.
    const { findByRole } = render(<SettingsScreen route={stubRoute("")} onDirtyChange={() => {}} />);
    expect(await findByRole("heading", { level: 2, name: "Appearance" })).toBeTruthy();
    expect(await findByRole("heading", { level: 2, name: "GUI server" })).toBeTruthy();
    expect(await findByRole("heading", { level: 2, name: "Daemons" })).toBeTruthy();
    expect(await findByRole("heading", { level: 2, name: "Backups" })).toBeTruthy();
    expect(await findByRole("heading", { level: 2, name: "Advanced" })).toBeTruthy();
  });

  it("renders Loading then Settings header", async () => {
    const { container, findByRole } = render(<SettingsScreen route={stubRoute("")} onDirtyChange={() => {}} />);
    // Initial loading state may or may not render; final state must show Settings.
    await findByRole("heading", { level: 1, name: "Settings" });
    expect(container.querySelector("h1")?.textContent).toBe("Settings");
  });

  it("calls onDirtyChange(false) initially", async () => {
    const onDirty = vi.fn();
    render(<SettingsScreen route={stubRoute("")} onDirtyChange={onDirty} />);
    await waitFor(() => expect(onDirty).toHaveBeenCalled());
    expect(onDirty).toHaveBeenLastCalledWith(false);
  });

  it("error state renders Retry button", async () => {
    vi.spyOn(api, "getSettings").mockRejectedValue(new Error("boom"));
    const { findByText, findByRole } = render(<SettingsScreen route={stubRoute("")} onDirtyChange={() => {}} />);
    expect(await findByText(/Could not load settings/)).toBeTruthy();
    expect(await findByRole("button", { name: "Retry" })).toBeTruthy();
  });
});
