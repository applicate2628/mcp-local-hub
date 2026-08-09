import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, fireEvent, waitFor, cleanup } from "@testing-library/preact";
import userEvent from "@testing-library/user-event";
import { SectionAdvanced } from "./SectionAdvanced";
import * as api from "../../lib/settings-api";
import * as appApi from "../../api";
import type { SettingsEnvelope, SettingsSnapshot } from "../../lib/settings-types";

function directSnapshot(
  data: SettingsEnvelope,
  refresh?: () => Promise<void>,
): SettingsSnapshot {
  return {
    status: "ok",
    data,
    error: null,
    refresh: refresh ?? vi.fn(async () => {}),
  };
}

const snap = directSnapshot({ actual_port: 9125, settings: [] });

function portSnap(
  refresh?: () => Promise<void>,
  deferred = false,
): SettingsSnapshot {
  return directSnapshot({
    actual_port: 9125,
    settings: [{
      key: "mcp_front.port",
      section: "advanced",
      type: "int",
      default: "9137",
      value: "9137",
      min: 1024,
      max: 65535,
      deferred,
      help: "Port used by the supervisor-managed MCP front daemon.",
    }],
  }, refresh);
}

function stubRelaxFetch(): void {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(
    JSON.stringify({ enabled: false }),
    { status: 200, headers: { "Content-Type": "application/json" } },
  ));
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

describe("SectionAdvanced", () => {
  beforeEach(() => vi.restoreAllMocks());
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("Open folder button calls postAction", async () => {
    stubRelaxFetch();
    const spy = vi.spyOn(api, "postAction").mockResolvedValue({ opened: "/x" });
    const { container } = render(<SectionAdvanced snapshot={snap} />);
    const btn = container.querySelector('[data-test-id="open-folder"]') as HTMLButtonElement;
    fireEvent.click(btn);
    await waitFor(() => expect(spy).toHaveBeenCalledWith("advanced.open_app_data_folder"));
  });

  it("error from postAction surfaces inline", async () => {
    stubRelaxFetch();
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

  it("projects registry value, default, bounds, and help into an accessible native number input", () => {
    stubRelaxFetch();
    const { getByRole, getByText, getByTitle } = render(
      <SectionAdvanced snapshot={portSnap()} onDirtyChange={() => {}} />,
    );

    const input = getByRole("spinbutton", { name: /MCP front port/ }) as HTMLInputElement;
    expect(input.type).toBe("number");
    expect(input.value).toBe("9137");
    expect(input.min).toBe("1024");
    expect(input.max).toBe("65535");
    expect(input.step).toBe("1");
    expect(getByText("Default 9137; allowed 1024–65535.")).toBeTruthy();
    expect(getByTitle("Port used by the supervisor-managed MCP front daemon.")).toBeTruthy();
  });

  it.each([
    ["unchanged", false],
    ["deferred", true],
  ])("performs zero PUTs for an %s port control", async (_state, deferred) => {
    stubRelaxFetch();
    const putSpy = vi.spyOn(api, "putSetting").mockResolvedValue(undefined);
    const { getByRole } = render(
      <SectionAdvanced snapshot={portSnap(undefined, deferred)} onDirtyChange={() => {}} />,
    );

    const input = getByRole("spinbutton", { name: /MCP front port/ }) as HTMLInputElement;
    const save = getByRole("button", { name: "Save" }) as HTMLButtonElement;
    expect(input.disabled).toBe(deferred);
    expect(save.disabled).toBe(true);

    fireEvent.click(save);
    await Promise.resolve();
    expect(putSpy).not.toHaveBeenCalled();
  });

  it("saves a keyboard-edited valid port through the shared settings flow and refreshes the snapshot", async () => {
    stubRelaxFetch();
    const putSpy = vi.spyOn(api, "putSetting").mockResolvedValue(undefined);
    const refresh = vi.fn(async () => {});
    const onDirty = vi.fn();
    const user = userEvent.setup();
    const { getByRole, findByText } = render(
      <SectionAdvanced snapshot={portSnap(refresh)} onDirtyChange={onDirty} />,
    );
    const input = getByRole("spinbutton", { name: /MCP front port/ }) as HTMLInputElement;

    await user.clear(input);
    await user.type(input, "9142");
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(true));

    const save = getByRole("button", { name: "Save" });
    save.focus();
    await user.keyboard("{Enter}");

    await waitFor(() => expect(putSpy).toHaveBeenCalledTimes(1));
    expect(putSpy).toHaveBeenNthCalledWith(1, "mcp_front.port", "9142");
    await waitFor(() => expect(refresh).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(false));
    expect(await findByText("Saved.")).toBeTruthy();
    expect(onDirty.mock.calls.map(([dirty]) => dirty)).toEqual([false, true, false]);
  });

  it("blocks a forced stale Restart after same-task port input", async () => {
    stubRelaxFetch();
    const putSpy = vi.spyOn(api, "putSetting").mockResolvedValue(undefined);
    const restartSpy = vi.spyOn(appApi, "restartSupervisor").mockResolvedValue({
      killed_pid: 100,
      killed: true,
      spawned_pid: 101,
      spawned: true,
    });
    const { getByRole } = render(<SectionAdvanced snapshot={portSnap()} />);
    const input = getByRole("spinbutton", { name: /MCP front port/ }) as HTMLInputElement;
    const restart = getByRole("button", { name: "Restart supervisor now" }) as HTMLButtonElement;

    fireEvent.input(input, { target: { value: "9142" } });
    restart.disabled = false;
    fireEvent.click(restart);
    await Promise.resolve();
    expect(restartSpy).not.toHaveBeenCalled();
    expect(putSpy).not.toHaveBeenCalled();
  });

  it("blocks a forced stale Restart while the port PUT is unresolved", async () => {
    stubRelaxFetch();
    const put = deferred<void>();
    const putSpy = vi.spyOn(api, "putSetting").mockReturnValue(put.promise);
    const restartSpy = vi.spyOn(appApi, "restartSupervisor").mockResolvedValue({
      killed_pid: 100,
      killed: true,
      spawned_pid: 101,
      spawned: true,
    });
    const { getByRole } = render(<SectionAdvanced snapshot={portSnap()} />);
    const input = getByRole("spinbutton", { name: /MCP front port/ }) as HTMLInputElement;
    const restart = getByRole("button", { name: "Restart supervisor now" }) as HTMLButtonElement;

    fireEvent.input(input, { target: { value: "9142" } });
    await waitFor(() => expect((getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(getByRole("button", { name: "Save" }));
    await waitFor(() => expect(putSpy).toHaveBeenCalledTimes(1));
    restart.disabled = false;
    fireEvent.click(restart);
    expect(restartSpy).not.toHaveBeenCalled();
    put.resolve(undefined);
  });

  it("blocks forced port Input, Save, and Reset while Restart is unresolved", async () => {
    stubRelaxFetch();
    const restart = deferred<Awaited<ReturnType<typeof appApi.restartSupervisor>>>();
    const restartSpy = vi.spyOn(appApi, "restartSupervisor").mockReturnValue(restart.promise);
    const putSpy = vi.spyOn(api, "putSetting").mockResolvedValue(undefined);
    const { getByRole } = render(<SectionAdvanced snapshot={portSnap()} />);
    const input = getByRole("spinbutton", { name: /MCP front port/ }) as HTMLInputElement;
    const save = getByRole("button", { name: "Save" }) as HTMLButtonElement;
    const reset = getByRole("button", { name: "Reset" }) as HTMLButtonElement;
    const restartButton = getByRole("button", { name: "Restart supervisor now" }) as HTMLButtonElement;

    fireEvent.click(restartButton);
    expect(restartSpy).toHaveBeenCalledTimes(1);
    expect(input.disabled).toBe(true);

    input.disabled = false;
    save.disabled = false;
    reset.disabled = false;
    fireEvent.input(input, { target: { value: "9142" } });
    fireEvent.click(save);
    fireEvent.click(reset);

    expect(putSpy).not.toHaveBeenCalled();
    expect(restartSpy).toHaveBeenCalledTimes(1);

    restart.resolve({ killed_pid: 100, killed: true, spawned_pid: 101, spawned: true });
    await waitFor(() => expect(restartButton.disabled).toBe(false));

    fireEvent.input(input, { target: { value: "9142" } });
    await waitFor(() => expect(save.disabled).toBe(false));
  });

  it("makes one PUT and refresh for two forced Save clicks before disabled render", async () => {
    stubRelaxFetch();
    const put = deferred<void>();
    const putSpy = vi.spyOn(api, "putSetting").mockReturnValue(put.promise);
    const refresh = vi.fn(async () => {});
    const { getByRole } = render(<SectionAdvanced snapshot={portSnap(refresh)} />);
    const input = getByRole("spinbutton", { name: /MCP front port/ }) as HTMLInputElement;
    const save = getByRole("button", { name: "Save" }) as HTMLButtonElement;

    fireEvent.input(input, { target: { value: "9142" } });
    await waitFor(() => expect(save.disabled).toBe(false));
    fireEvent.click(save);
    save.disabled = false;
    fireEvent.click(save);

    expect(putSpy).toHaveBeenCalledTimes(1);
    put.resolve(undefined);
    await waitFor(() => expect(refresh).toHaveBeenCalledTimes(1));
  });

  it("blocks forced Reset and Restart until an unresolved Save settles", async () => {
    stubRelaxFetch();
    const put = deferred<void>();
    const putSpy = vi.spyOn(api, "putSetting").mockReturnValue(put.promise);
    const restartSpy = vi.spyOn(appApi, "restartSupervisor").mockResolvedValue({
      killed_pid: 100,
      killed: true,
      spawned_pid: 101,
      spawned: true,
    });
    const { getByRole } = render(<SectionAdvanced snapshot={portSnap()} />);
    const input = getByRole("spinbutton", { name: /MCP front port/ }) as HTMLInputElement;
    const save = getByRole("button", { name: "Save" }) as HTMLButtonElement;
    const reset = getByRole("button", { name: "Reset" }) as HTMLButtonElement;
    const restart = getByRole("button", { name: "Restart supervisor now" }) as HTMLButtonElement;

    fireEvent.input(input, { target: { value: "9142" } });
    await waitFor(() => expect(save.disabled).toBe(false));
    fireEvent.click(save);
    await waitFor(() => expect(putSpy).toHaveBeenCalledTimes(1));

    reset.disabled = false;
    restart.disabled = false;
    fireEvent.click(reset);
    fireEvent.click(restart);
    expect(putSpy).toHaveBeenCalledTimes(1);
    expect(restartSpy).not.toHaveBeenCalled();

    put.resolve(undefined);
    await waitFor(() => expect(save.disabled).toBe(true));
  });

  it("keeps Restart blocked after a failed port PUT", async () => {
    stubRelaxFetch();
    vi.spyOn(api, "putSetting").mockRejectedValue(Object.assign(new Error("invalid port"), {
      body: { reason: "must be between 1024 and 65535" },
    }));
    const restartSpy = vi.spyOn(appApi, "restartSupervisor").mockResolvedValue({
      killed_pid: 100,
      killed: true,
      spawned_pid: 101,
      spawned: true,
    });
    const { getByRole, findByText } = render(<SectionAdvanced snapshot={portSnap()} />);
    const input = getByRole("spinbutton", { name: /MCP front port/ }) as HTMLInputElement;
    const restart = getByRole("button", { name: "Restart supervisor now" }) as HTMLButtonElement;

    fireEvent.input(input, { target: { value: "80" } });
    await waitFor(() => expect((getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(getByRole("button", { name: "Save" }));
    await findByText("must be between 1024 and 65535");
    restart.disabled = false;
    fireEvent.click(restart);
    expect(restartSpy).not.toHaveBeenCalled();
  });

  it("surfaces backend port validation failure inline and keeps the draft dirty", async () => {
    stubRelaxFetch();
    vi.spyOn(api, "putSetting").mockRejectedValue(Object.assign(new Error("invalid port"), {
      body: { reason: "must be between 1024 and 65535" },
    }));
    const onDirty = vi.fn();
    const { getByRole, findByText } = render(
      <SectionAdvanced snapshot={portSnap()} onDirtyChange={onDirty} />,
    );
    const input = getByRole("spinbutton", { name: /MCP front port/ }) as HTMLInputElement;

    fireEvent.input(input, { target: { value: "80" } });
    fireEvent.click(getByRole("button", { name: "Save" }));

    const error = await findByText("must be between 1024 and 65535");
    expect(error.getAttribute("role")).toBe("alert");
    expect(input.getAttribute("aria-invalid")).toBe("true");
    expect(input.getAttribute("aria-describedby")).toContain("mcp_front.port-error");
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(true));
  });

  it("Reset restores the persisted port and clears the Advanced dirty state", async () => {
    stubRelaxFetch();
    const putSpy = vi.spyOn(api, "putSetting").mockResolvedValue(undefined);
    const onDirty = vi.fn();
    const { getByRole } = render(
      <SectionAdvanced snapshot={portSnap()} onDirtyChange={onDirty} />,
    );
    const input = getByRole("spinbutton", { name: /MCP front port/ }) as HTMLInputElement;

    fireEvent.input(input, { target: { value: "9143" } });
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(true));
    fireEvent.click(getByRole("button", { name: "Reset" }));

    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(false));
    expect(input.value).toBe("9137");
    expect(putSpy).not.toHaveBeenCalled();
    expect(onDirty.mock.calls.map(([dirty]) => dirty)).toEqual([false, true, false]);
  });

  it("Reset performs zero PUTs and permits one later explicit Restart", async () => {
    stubRelaxFetch();
    const putSpy = vi.spyOn(api, "putSetting").mockResolvedValue(undefined);
    const restartSpy = vi.spyOn(appApi, "restartSupervisor").mockResolvedValue({
      killed_pid: 100,
      killed: true,
      spawned_pid: 101,
      spawned: true,
    });
    const { getByRole } = render(<SectionAdvanced snapshot={portSnap()} />);
    const input = getByRole("spinbutton", { name: /MCP front port/ }) as HTMLInputElement;
    const restart = getByRole("button", { name: "Restart supervisor now" }) as HTMLButtonElement;

    fireEvent.input(input, { target: { value: "9143" } });
    await waitFor(() => expect((getByRole("button", { name: "Reset" }) as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(getByRole("button", { name: "Reset" }));
    await waitFor(() => expect(restart.disabled).toBe(false));
    fireEvent.click(restart);

    await waitFor(() => expect(restartSpy).toHaveBeenCalledTimes(1));
    expect(putSpy).not.toHaveBeenCalled();
  });

  it("makes one backend call for two same-task Restart clicks", async () => {
    stubRelaxFetch();
    const restart = deferred<Awaited<ReturnType<typeof appApi.restartSupervisor>>>();
    const restartSpy = vi.spyOn(appApi, "restartSupervisor").mockReturnValue(restart.promise);
    const { getByRole } = render(<SectionAdvanced snapshot={portSnap()} />);
    const button = getByRole("button", { name: "Restart supervisor now" }) as HTMLButtonElement;

    fireEvent.click(button);
    button.disabled = false;
    fireEvent.click(button);

    expect(restartSpy).toHaveBeenCalledTimes(1);
    restart.resolve({ killed_pid: 100, killed: true, spawned_pid: 101, spawned: true });
  });
});

describe("SectionAdvanced - Autorun toggle", () => {
  beforeEach(() => vi.restoreAllMocks());
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

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
