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

describe("SectionAdvanced - Autorun toggle", () => {
  beforeEach(() => vi.restoreAllMocks());
  afterEach(() => vi.unstubAllGlobals());

  it("initial state reflects GET response (enabled=true)", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.includes("/api/settings/state-read-relax")) {
        return new Response(JSON.stringify({ enabled: true }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      return new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } });
    });
    const { container } = render(<SectionAdvanced snapshot={snap} />);
    const toggle = container.querySelector('[data-testid="state-relax-toggle"]') as HTMLInputElement;
    expect(toggle).toBeTruthy();
    await waitFor(() => expect(toggle.checked).toBe(true));
    expect(toggle.disabled).toBe(false);
  });

  it("click POSTs {enabled:false} when currently enabled and surfaces restart hint", async () => {
    const calls: Array<{ url: string; method?: string; body?: string }> = [];
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.includes("/api/settings/state-read-relax")) {
        const method = init?.method ?? "GET";
        calls.push({ url, method, body: init?.body as string | undefined });
        if (method === "POST") {
          return new Response(JSON.stringify({ enabled: false, restart_required: true }), {
            status: 200,
            headers: { "Content-Type": "application/json" },
          });
        }
        return new Response(JSON.stringify({ enabled: true }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      return new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } });
    });

    const { container } = render(<SectionAdvanced snapshot={snap} />);
    const toggle = container.querySelector('[data-testid="state-relax-toggle"]') as HTMLInputElement;
    await waitFor(() => expect(toggle.checked).toBe(true));

    fireEvent.click(toggle);

    await waitFor(() => {
      const post = calls.find((c) => c.method === "POST");
      expect(post).toBeTruthy();
      expect(post!.body).toBe(JSON.stringify({ enabled: false }));
    });

    await waitFor(() => {
      const msg = container.querySelector('[data-testid="state-relax-msg"]') as HTMLElement | null;
      expect(msg).toBeTruthy();
      expect(msg!.textContent ?? "").toMatch(/Disabled.*Restart mcphub/);
    });
  });

  it("disabled state when backend returns 501 (POSIX path)", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.includes("/api/settings/state-read-relax")) {
        return new Response("", { status: 501 });
      }
      return new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } });
    });
    const { container } = render(<SectionAdvanced snapshot={snap} />);
    const toggle = container.querySelector('[data-testid="state-relax-toggle"]') as HTMLInputElement;
    expect(toggle).toBeTruthy();
    await waitFor(() => expect(toggle.disabled).toBe(true));
    await waitFor(() =>
      expect(container.textContent ?? "").toMatch(/Not supported on this OS/)
    );
  });

  it("GET network error surfaces in state-relax-msg and toggle remains unchecked/disabled", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.includes("/api/settings/state-read-relax")) {
        throw new Error("connection refused");
      }
      return new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } });
    });
    const { container } = render(<SectionAdvanced snapshot={snap} />);
    const toggle = container.querySelector('[data-testid="state-relax-toggle"]') as HTMLInputElement;
    expect(toggle).toBeTruthy();
    await waitFor(() => {
      const msg = container.querySelector('[data-testid="state-relax-msg"]') as HTMLElement | null;
      expect(msg).toBeTruthy();
      expect(msg!.textContent ?? "").toMatch(/GET error: connection refused/);
    });
    expect(toggle.checked).toBe(false);
    expect(toggle.disabled).toBe(true);
  });
});