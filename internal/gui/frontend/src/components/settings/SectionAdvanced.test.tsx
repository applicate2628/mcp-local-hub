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
    // Each fetch call needs its OWN Response — a Response body stream
    // can only be consumed once. SectionAdvanced's mount-time fetch
    // of /api/settings/state-read-relax would otherwise drain the
    // single mocked Response and starve the export-click's read.
    const blob = new Blob(["PK"], { type: "application/zip" });
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.includes("/api/settings/state-read-relax")) {
        return new Response(JSON.stringify({ enabled: false }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      return new Response(blob, { status: 200, headers: { "Content-Type": "application/zip" } });
    });
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
