import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, fireEvent, waitFor } from "@testing-library/preact";
import { SectionGuiServer } from "./SectionGuiServer";
import * as api from "../../lib/settings-api";
import type { SettingsSnapshot, SettingsEnvelope } from "../../lib/settings-types";

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

function snap(env: SettingsEnvelope, refresh = vi.fn(async () => {})): SettingsSnapshot {
  return { status: "ok", data: env, error: null, refresh };
}

describe("SectionGuiServer", () => {
  beforeEach(() => vi.restoreAllMocks());

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
});
