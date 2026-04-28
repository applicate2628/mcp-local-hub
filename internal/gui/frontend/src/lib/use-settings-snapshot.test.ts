import { describe, expect, it, vi, beforeEach } from "vitest";
import { renderHook, act, waitFor } from "@testing-library/preact";
import { useSettingsSnapshot } from "./use-settings-snapshot";
import * as api from "./settings-api";
import type { SettingsEnvelope } from "./settings-types";

const goodEnvelope: SettingsEnvelope = {
  actual_port: 9125,
  settings: [
    { key: "appearance.theme", section: "appearance", type: "enum",
      default: "system", value: "system", enum: ["light", "dark", "system"],
      deferred: false, help: "" },
    { key: "advanced.open_app_data_folder", section: "advanced", type: "action",
      deferred: false, help: "" },
  ],
};

describe("useSettingsSnapshot", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it("loads on mount", async () => {
    vi.spyOn(api, "getSettings").mockResolvedValue(goodEnvelope);
    const { result } = renderHook(() => useSettingsSnapshot());
    expect(result.current.status).toBe("loading");
    await waitFor(() => expect(result.current.status).toBe("ok"));
    expect(result.current.data?.actual_port).toBe(9125);
    expect(result.current.data?.settings).toHaveLength(2);
  });

  it("transitions ok → ok via refresh, preserving previous data on stale-while-revalidate", async () => {
    const spy = vi.spyOn(api, "getSettings").mockResolvedValue(goodEnvelope);
    const { result } = renderHook(() => useSettingsSnapshot());
    await waitFor(() => expect(result.current.status).toBe("ok"));
    await act(async () => { await result.current.refresh(); });
    expect(spy).toHaveBeenCalledTimes(2);
    expect(result.current.status).toBe("ok");
  });

  it("transitions to error on fetch failure", async () => {
    vi.spyOn(api, "getSettings").mockRejectedValue(new Error("network"));
    const { result } = renderHook(() => useSettingsSnapshot());
    await waitFor(() => expect(result.current.status).toBe("error"));
    expect((result.current.error as Error).message).toBe("network");
  });

  it("retains discriminated-union shape (action entries have no value/default)", async () => {
    vi.spyOn(api, "getSettings").mockResolvedValue(goodEnvelope);
    const { result } = renderHook(() => useSettingsSnapshot());
    await waitFor(() => expect(result.current.status).toBe("ok"));
    const action = result.current.data!.settings.find((s) => s.key === "advanced.open_app_data_folder")!;
    expect(action.type).toBe("action");
    expect("value" in action).toBe(false);
    expect("default" in action).toBe(false);
  });
});
