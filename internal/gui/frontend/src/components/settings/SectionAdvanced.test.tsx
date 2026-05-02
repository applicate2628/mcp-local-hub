import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
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
  afterEach(() => vi.unstubAllGlobals());

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

  it("Export bundle button fetches /api/export-config-bundle and triggers download", async () => {
    const blob = new Blob(["PK"], { type: "application/zip" });
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(blob, { status: 200, headers: { "Content-Type": "application/zip" } })
    );
    const createObjectURLSpy = vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:fake");
    const revokeObjectURLSpy = vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});

    const { container } = render(<SectionAdvanced snapshot={snap} />);
    const btn = container.querySelector('[data-testid="export-bundle"]') as HTMLButtonElement;
    expect(btn).toBeTruthy();
    expect(btn.disabled).toBe(false);
    expect(btn.textContent).not.toMatch(/coming in A4-b/);

    fireEvent.click(btn);
    await waitFor(() => expect(fetchSpy).toHaveBeenCalledWith("/api/export-config-bundle", { method: "POST" }));
    await waitFor(() => expect(createObjectURLSpy).toHaveBeenCalled());
    await waitFor(() => expect(revokeObjectURLSpy).toHaveBeenCalled());
  });

  it("shows error banner when exportBundle fetch throws (P2-B)", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    const { container } = render(<SectionAdvanced snapshot={snap} />);
    const btn = container.querySelector('[data-testid="export-bundle"]') as HTMLButtonElement;
    fireEvent.click(btn);
    await waitFor(() =>
      expect(container.querySelector('[role="alert"]')?.textContent).toMatch(/network down/)
    );
  });
});
