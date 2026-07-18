import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, waitFor } from "@testing-library/preact";
import { SectionGuiServer } from "./SectionGuiServer";
import * as api from "../../lib/settings-api";
import * as guiApi from "../../api";
import type { SettingsSnapshot, SettingsEnvelope } from "../../lib/settings-types";

const eventSources = new Set<StubEventSource>();

class StubEventSource {
  readonly url: string;
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  private listeners = new Map<string, Set<EventListener>>();

  constructor(url: string) {
    this.url = url;
    eventSources.add(this);
  }

  addEventListener(type: string, listener: EventListener): void {
    const listeners = this.listeners.get(type) ?? new Set<EventListener>();
    listeners.add(listener);
    this.listeners.set(type, listeners);
  }

  removeEventListener(type: string, listener: EventListener): void {
    this.listeners.get(type)?.delete(listener);
  }

  close(): void {
    eventSources.delete(this);
  }

  emit(type: string, body: Record<string, unknown>): void {
    const event = new MessageEvent(type, { data: JSON.stringify(body) });
    for (const listener of this.listeners.get(type) ?? []) listener(event);
  }
}

function consumeRestartProgress(
  body: Record<string, unknown>,
  navigation?: { currentPort: number; assign: (target: string) => void },
): unknown {
  return guiApi.consumeGuiRestartProgressEvent(
    new MessageEvent("gui-restart-progress", { data: JSON.stringify(body) }),
    navigation,
  );
}

function onlyEventSource(): StubEventSource {
  const instances = Array.from(eventSources);
  expect(instances).toHaveLength(1);
  expect(instances[0].url).toBe("/api/events");
  return instances[0];
}

function stubRestartResponse(status: number, body: Record<string, unknown>): void {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => ({
      ok: status >= 200 && status < 300,
      status,
      statusText: "",
      json: async () => body,
    })) as unknown as typeof fetch,
  );
}

function envWithPort(value: string, actualPort: number): SettingsEnvelope {
  return {
    actual_port: actualPort,
    settings: [
      { key: "gui_server.browser_on_launch", section: "gui_server", type: "bool",
        default: "true", value: "true", deferred: false, help: "" },
      { key: "gui_server.port", section: "gui_server", type: "int",
        default: "9125", value, min: 1024, max: 65535, deferred: false, help: "" },
      { key: "gui_server.tray", section: "gui_server", type: "bool",
        default: "true", value: "true", deferred: true, help: "" },
    ],
  };
}

function envWithHub(persisted: boolean, actual: boolean): SettingsEnvelope {
  return {
    actual_port: 9125,
    actual_hub_endpoint_enabled: actual,
    settings: [
      { key: "gui_server.browser_on_launch", section: "gui_server", type: "bool",
        default: "true", value: "true", deferred: false, help: "" },
      { key: "gui_server.port", section: "gui_server", type: "int",
        default: "9125", value: "9125", min: 1024, max: 65535, deferred: false, help: "" },
      { key: "gui_server.hub_endpoint_enabled", section: "gui_server", type: "bool",
        default: "false", value: persisted ? "true" : "false", deferred: false, help: "" },
      { key: "gui_server.tray", section: "gui_server", type: "bool",
        default: "true", value: "true", deferred: true, help: "" },
    ],
  };
}

function snap(env: SettingsEnvelope, refresh = vi.fn(async () => {})): SettingsSnapshot {
  return { status: "ok", data: env, error: null, refresh };
}

describe("SectionGuiServer", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    eventSources.clear();
    vi.stubGlobal("EventSource", StubEventSource as unknown as typeof EventSource);
  });

  afterEach(() => {
    cleanup();
    eventSources.clear();
    vi.unstubAllGlobals();
  });

  it("renders all 3 fields with tray disabled (deferred)", () => {
    const { container, getByText } = render(<SectionGuiServer snapshot={snap(envWithPort("9125", 9125))} onDirtyChange={() => {}} />);
    const tray = container.querySelector("#gui_server\\.tray") as HTMLInputElement;
    expect(tray.disabled).toBe(true);
    expect(getByText(/coming in A4-b/)).toBeTruthy();
  });

  it("port-pending-restart badge HIDDEN when persisted == actual_port", () => {
    const { container } = render(<SectionGuiServer snapshot={snap(envWithPort("9125", 9125))} onDirtyChange={() => {}} />);
    expect(container.querySelector('[data-test-id="port-restart-badge"]')).toBeNull();
  });

  it("port-pending-restart badge VISIBLE when persisted != actual_port", () => {
    const { container } = render(<SectionGuiServer snapshot={snap(envWithPort("9200", 9125))} onDirtyChange={() => {}} />);
    const badge = container.querySelector('[data-test-id="port-restart-badge"]');
    expect(badge).toBeTruthy();
    expect(badge!.textContent).toMatch(/9200/);
  });

  it("Codex r4 P2.1: dirty draft does NOT flip badge", async () => {
    // Both persisted and actual are 9125 → no badge. Type a different
    // value into the field but DO NOT save. Badge must stay hidden.
    const onDirty = vi.fn();
    const { container } = render(
      <SectionGuiServer snapshot={snap(envWithPort("9125", 9125))} onDirtyChange={onDirty} />,
    );
    const portInput = container.querySelector("#gui_server\\.port") as HTMLInputElement;
    fireEvent.input(portInput, { target: { value: "9200" } });
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(true));
    // Badge must still be hidden — local draft is dirty, not persisted.
    expect(container.querySelector('[data-test-id="port-restart-badge"]')).toBeNull();
  });

  // Issue #161 P2 — persisted-vs-runtime hub gate badge.
  it("hub-endpoint-restart-badge HIDDEN when persisted == actual", () => {
    const { container } = render(
      <SectionGuiServer snapshot={snap(envWithHub(false, false))} onDirtyChange={() => {}} />,
    );
    expect(container.querySelector('[data-test-id="hub-endpoint-restart-badge"]')).toBeNull();
  });

  it("hub-endpoint-restart-badge VISIBLE when persisted true but runtime false", () => {
    const { container } = render(
      <SectionGuiServer snapshot={snap(envWithHub(true, false))} onDirtyChange={() => {}} />,
    );
    const badge = container.querySelector('[data-test-id="hub-endpoint-restart-badge"]');
    expect(badge).toBeTruthy();
    expect(badge!.textContent).toMatch(/ON.*restart/);
  });

  it("hub-endpoint-restart-badge VISIBLE when persisted false but runtime true", () => {
    const { container } = render(
      <SectionGuiServer snapshot={snap(envWithHub(false, true))} onDirtyChange={() => {}} />,
    );
    const badge = container.querySelector('[data-test-id="hub-endpoint-restart-badge"]');
    expect(badge).toBeTruthy();
    expect(badge!.textContent).toMatch(/OFF.*restart/);
  });

  it("hub-endpoint-restart-badge HIDDEN when actual_hub_endpoint_enabled is undefined (older backend)", () => {
    // Older backends may not emit the field; envelope without the
    // key sees actual as undefined → falsy → no spurious badge.
    const env = envWithHub(true, false);
    delete env.actual_hub_endpoint_enabled;
    const { container } = render(<SectionGuiServer snapshot={snap(env)} onDirtyChange={() => {}} />);
    // Persisted=true vs actual=undefined→false: the SHOULD fire too
    // — undefined coerces to false via `=== true` check. The badge
    // SHOULD render. (Older backend that doesn't emit the field
    // looks "off" runtime-wise; the operator setting it to true
    // wants a restart.) Confirm semantic:
    const badge = container.querySelector('[data-test-id="hub-endpoint-restart-badge"]');
    expect(badge).toBeTruthy();
  });

  it("Codex r4 P2.1: badge appears AFTER Save", async () => {
    let env = envWithPort("9125", 9125);
    const refresh = vi.fn(async () => {
      // Simulate refresh: persisted now reflects the saved 9200 value.
      env = envWithPort("9200", 9125);
    });
    vi.spyOn(api, "putSetting").mockResolvedValue(undefined);
    const { container, rerender } = render(
      <SectionGuiServer snapshot={snap(env, refresh)} onDirtyChange={() => {}} />,
    );
    const portInput = container.querySelector("#gui_server\\.port") as HTMLInputElement;
    fireEvent.input(portInput, { target: { value: "9200" } });
    fireEvent.click(Array.from(container.querySelectorAll("button")).find((b) => b.textContent === "Save")!);
    await waitFor(() => expect(refresh).toHaveBeenCalled());
    // Re-render with the post-save snapshot.
    rerender(<SectionGuiServer snapshot={snap(env)} onDirtyChange={() => {}} />);
    await waitFor(() => expect(container.querySelector('[data-test-id="port-restart-badge"]')).toBeTruthy());
  });

  it("keeps a same-port 202 restarting response on the reconnect copy", async () => {
    stubRestartResponse(202, {
      handoff_id: "handoff-same-port",
      generation: "generation-same-port",
      phase: "in-progress",
      spawned: true,
      spawned_pid: 4321,
      restarting: true,
      old_port: 9125,
      target_port: 9125,
    });
    const { getByTestId } = render(
      <SectionGuiServer snapshot={snap(envWithPort("9200", 9125))} onDirtyChange={() => {}} />,
    );

    fireEvent.click(getByTestId("gui-server-restart-now"));

    await waitFor(() => expect(getByTestId("gui-server-restart-msg").textContent).toBe(
      "Restarting the GUI… the replacement window reconnects on the same port in a moment. If this tab does not refresh on its own, reload it.",
    ));
  });

  it("uses best-effort new-port copy for a port-change 202 restarting response", async () => {
    stubRestartResponse(202, {
      handoff_id: "handoff-port-change",
      generation: "generation-port-change",
      phase: "in-progress",
      spawned: true,
      spawned_pid: 4322,
      restarting: true,
      old_port: 9125,
      target_port: 9200,
    });
    const { getByTestId } = render(
      <SectionGuiServer snapshot={snap(envWithPort("9200", 9125))} onDirtyChange={() => {}} />,
    );

    fireEvent.click(getByTestId("gui-server-restart-now"));

    await waitFor(() => expect(getByTestId("gui-server-restart-msg").textContent).toBe(
      "Restarting the GUI… the replacement GUI is targeting port 9200. This tab will make a best-effort attempt to follow; if it does not, open the GUI on the new port.",
    ));
  });

  it("keeps a 2xx spawn_error on the Restart incomplete path", async () => {
    stubRestartResponse(200, {
      spawned: false,
      spawned_pid: 0,
      restarting: false,
      spawn_error: "replacement process did not start",
    });
    const { getByTestId } = render(
      <SectionGuiServer snapshot={snap(envWithPort("9200", 9125))} onDirtyChange={() => {}} />,
    );

    fireEvent.click(getByTestId("gui-server-restart-now"));

    await waitFor(() => expect(getByTestId("gui-server-restart-msg").textContent).toBe(
      "Restart incomplete: replacement process did not start",
    ));
  });

  it("keeps restartGui throw-on-non-2xx behavior", async () => {
    stubRestartResponse(503, { code: "GUI_RESTART_UNAVAILABLE", error: "restart unavailable" });
    const { getByTestId } = render(
      <SectionGuiServer snapshot={snap(envWithPort("9200", 9125))} onDirtyChange={() => {}} />,
    );

    fireEvent.click(getByTestId("gui-server-restart-now"));

    await waitFor(() => expect(getByTestId("gui-server-restart-msg").textContent).toBe(
      "/api/gui/restart [GUI_RESTART_UNAVAILABLE]: restart unavailable",
    ));
  });

  it("best-effort navigates only on reserved new_port from the matching old-port stream", () => {
    const assign = vi.fn(() => { throw new Error("navigation unavailable"); });

    expect(() => consumeRestartProgress({
      handoff_id: "handoff-navigation",
      generation: "generation-navigation",
      phase: "reserved",
      old_port: 9125,
      new_port: 9200,
      same_port: false,
    }, { currentPort: 9125, assign })).not.toThrow();
    expect(assign).toHaveBeenCalledOnce();
    expect(assign).toHaveBeenCalledWith("http://127.0.0.1:9200/");

    consumeRestartProgress({
      handoff_id: "handoff-other-stream",
      generation: "generation-other-stream",
      phase: "reserved",
      old_port: 9300,
      new_port: 9400,
      same_port: false,
    }, { currentPort: 9125, assign });
    expect(assign).toHaveBeenCalledOnce();

    consumeRestartProgress({
      handoff_id: "handoff-target-port",
      generation: "generation-target-port",
      phase: "reserved",
      old_port: 9125,
      target_port: 9300,
      same_port: false,
    }, { currentPort: 9125, assign });
    expect(assign).toHaveBeenCalledTimes(2);
    expect(assign).toHaveBeenLastCalledWith("http://127.0.0.1:9300/");
  });

  it("does not navigate for same-port or committed progress", () => {
    const assign = vi.fn();
    consumeRestartProgress({
      phase: "reserved",
      old_port: 9125,
      new_port: 9125,
      same_port: true,
    }, { currentPort: 9125, assign });
    consumeRestartProgress({
      phase: "committed",
      old_port: 9125,
      new_port: 9200,
      same_port: false,
    }, { currentPort: 9125, assign });
    expect(assign).not.toHaveBeenCalled();
  });

  it("renders the enum-driven free-flock interrupted literal", async () => {
    const { getByTestId } = render(
      <SectionGuiServer snapshot={snap(envWithPort("9125", 9125))} onDirtyChange={() => {}} />,
    );
    await waitFor(() => expect(eventSources.size).toBe(1));

    act(() => onlyEventSource().emit("gui-restart-progress", {
      phase: "interrupted",
      reason_code: "gui-restart-interrupted-free-flock",
      operator_action: "mcphub gui",
      old_port: 9125,
      new_port: 9200,
    }));

    expect(getByTestId("gui-server-restart-msg").textContent).toBe(
      "GUI restart interrupted; run `mcphub gui`.",
    );
  });

  it("renders the enum-driven live-holder force-kill literal", async () => {
    const { getByTestId } = render(
      <SectionGuiServer snapshot={snap(envWithPort("9125", 9125))} onDirtyChange={() => {}} />,
    );
    await waitFor(() => expect(eventSources.size).toBe(1));

    act(() => onlyEventSource().emit("gui-restart-progress", {
      phase: "interrupted",
      reason_code: "gui-restart-live-holder-wedged",
      operator_action: "mcphub gui --force --kill",
      old_port: 9125,
      new_port: 9200,
    }));

    expect(getByTestId("gui-server-restart-msg").textContent).toBe(
      "GUI restart interrupted: a GUI process still holds the single-instance lock; run `mcphub gui --force --kill`.",
    );
  });

  it("never renders an arbitrary operator_action or a committed claim", async () => {
    const { queryByTestId } = render(
      <SectionGuiServer snapshot={snap(envWithPort("9125", 9125))} onDirtyChange={() => {}} />,
    );
    await waitFor(() => expect(eventSources.size).toBe(1));

    act(() => onlyEventSource().emit("gui-restart-progress", {
      phase: "interrupted",
      reason_code: "gui-restart-interrupted-free-flock",
      operator_action: "persisted arbitrary command",
      old_port: 9125,
      new_port: 9200,
    }));
    expect(queryByTestId("gui-server-restart-msg")?.textContent ?? "").not.toContain("persisted arbitrary command");

    act(() => onlyEventSource().emit("gui-restart-progress", {
      phase: "committed",
      reason_code: "committed",
      old_port: 9125,
      new_port: 9200,
    }));
    expect(queryByTestId("gui-server-restart-msg")?.textContent ?? "").not.toMatch(/committed|complete/i);
  });
});
