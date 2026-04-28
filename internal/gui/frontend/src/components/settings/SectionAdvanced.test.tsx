import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, fireEvent, waitFor } from "@testing-library/preact";
import { SectionAdvanced } from "./SectionAdvanced";
import * as api from "../../lib/settings-api";
import type { SettingsSnapshot } from "../../lib/settings-types";

const snap: SettingsSnapshot = {
  status: "ok",
  data: { actual_port: 9125, settings: [] },
  error: null,
  refresh: vi.fn(async () => {}),
};

describe("SectionAdvanced", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("Open folder button calls postAction", async () => {
    const spy = vi.spyOn(api, "postAction").mockResolvedValue({ opened: "/x" });
    const { container } = render(<SectionAdvanced snapshot={snap} />);
    const btn = container.querySelector('[data-test-id="open-folder"]') as HTMLButtonElement;
    fireEvent.click(btn);
    await waitFor(() => expect(spy).toHaveBeenCalledWith("advanced.open_app_data_folder"));
  });

  it("error from postAction surfaces inline", async () => {
    vi.spyOn(api, "postAction").mockRejectedValue(Object.assign(new Error("nope"), { body: { reason: "not found" } }));
    const { container, findByText } = render(<SectionAdvanced snapshot={snap} />);
    const btn = container.querySelector('[data-test-id="open-folder"]') as HTMLButtonElement;
    fireEvent.click(btn);
    expect(await findByText(/Could not open folder: not found/)).toBeTruthy();
  });

  it("Export bundle button is disabled with (coming in A4-b)", () => {
    const { container } = render(<SectionAdvanced snapshot={snap} />);
    const btn = container.querySelector('[data-test-id="export-bundle-disabled"]') as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
    expect(btn.textContent).toMatch(/coming in A4-b/);
  });
});
