import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, fireEvent, waitFor } from "@testing-library/preact";
import { SectionBackups } from "./SectionBackups";
import * as api from "../../lib/settings-api";
import { BACKUPS_COPY } from "./backups-copy";
import type { SettingsSnapshot, SettingsEnvelope } from "../../lib/settings-types";

const env: SettingsEnvelope = {
  actual_port: 9125,
  settings: [
    { key: "backups.keep_n", section: "backups", type: "int",
      default: "5", value: "5", min: 0, max: 50, deferred: false, help: "" },
    { key: "backups.clean_now", section: "backups", type: "action", deferred: true, help: "" },
  ],
};
const snap = (refresh = vi.fn(async () => {})): SettingsSnapshot =>
  ({ status: "ok", data: env, error: null, refresh });

describe("SectionBackups", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    vi.spyOn(api, "getBackups").mockResolvedValue([]);
    vi.spyOn(api, "getBackupsCleanPreview").mockResolvedValue([]);
  });

  it("renders all 6 verbatim Codex copy strings (memo §9.4)", async () => {
    const { findByText } = render(<SectionBackups snapshot={snap()} onDirtyChange={() => {}} />);
    expect(await findByText(new RegExp(BACKUPS_COPY.sliderLabel))).toBeTruthy();
    expect(await findByText(BACKUPS_COPY.helperText)).toBeTruthy();
    expect(await findByText(BACKUPS_COPY.groupNote)).toBeTruthy();
    // Tooltip: title attribute on the disabled Clean button.
    const btn = document.querySelector('[data-test-id="clean-now-disabled"]') as HTMLButtonElement;
    expect(btn.title).toBe(BACKUPS_COPY.cleanTooltip);
    expect(btn.disabled).toBe(true);
    // The eligible-badge + preview-unavailable copy come from BackupsList
    // and are tested in BackupsList.test.tsx; this test asserts the
    // section-level surface only.
  });

  it("slider drag dirties the section", async () => {
    const onDirty = vi.fn();
    const { container } = render(<SectionBackups snapshot={snap()} onDirtyChange={onDirty} />);
    const slider = container.querySelector("input[type=range]") as HTMLInputElement;
    fireEvent.input(slider, { target: { value: "10" } });
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(true));
  });

  it("Save calls putSetting + refreshes snapshot", async () => {
    const putSpy = vi.spyOn(api, "putSetting").mockResolvedValue(undefined);
    const refresh = vi.fn(async () => {});
    const { container } = render(<SectionBackups snapshot={snap(refresh)} onDirtyChange={() => {}} />);
    const slider = container.querySelector("input[type=range]") as HTMLInputElement;
    fireEvent.input(slider, { target: { value: "12" } });
    fireEvent.click(Array.from(container.querySelectorAll("button")).find((b) => b.textContent === "Save")!);
    await waitFor(() => expect(putSpy).toHaveBeenCalledWith("backups.keep_n", "12"));
    await waitFor(() => expect(refresh).toHaveBeenCalled());
  });

  it("disabled Clean now button has the locked tooltip", () => {
    const { container } = render(<SectionBackups snapshot={snap()} onDirtyChange={() => {}} />);
    const btn = container.querySelector('[data-test-id="clean-now-disabled"]') as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
    expect(btn.title).toBe(BACKUPS_COPY.cleanTooltip);
  });
});
