import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, fireEvent, waitFor } from "@testing-library/preact";
import { SectionAppearance } from "./SectionAppearance";
import * as api from "../../lib/settings-api";
import type { SettingsSnapshot, SettingsEnvelope } from "../../lib/settings-types";

const env: SettingsEnvelope = {
  actual_port: 9125,
  settings: [
    { key: "appearance.theme", section: "appearance", type: "enum",
      default: "system", value: "system", enum: ["light","dark","system"], deferred: false, help: "" },
    { key: "appearance.density", section: "appearance", type: "enum",
      default: "comfortable", value: "comfortable", enum: ["compact","comfortable","spacious"], deferred: false, help: "" },
    { key: "appearance.shell", section: "appearance", type: "enum",
      default: "pwsh", value: "pwsh", enum: ["pwsh","cmd","bash","zsh","git-bash"], deferred: false, help: "" },
    { key: "appearance.default_home", section: "appearance", type: "path",
      default: "", value: "", optional: true, deferred: false, help: "" },
  ],
};

function makeSnapshot(refresh = vi.fn(async () => {})): SettingsSnapshot {
  return { status: "ok", data: env, error: null, refresh };
}

describe("SectionAppearance", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("renders 4 fields in registry order", () => {
    const { container } = render(<SectionAppearance snapshot={makeSnapshot()} onDirtyChange={() => {}} />);
    expect(container.querySelectorAll(".settings-field")).toHaveLength(4);
  });

  it("editing theme dirties the section + Save enables", async () => {
    const onDirty = vi.fn();
    const { container } = render(<SectionAppearance snapshot={makeSnapshot()} onDirtyChange={onDirty} />);
    const select = container.querySelector("#appearance\\.theme") as HTMLSelectElement;
    fireEvent.change(select, { target: { value: "dark" } });
    await waitFor(() => expect(onDirty).toHaveBeenCalledWith(true));
    const saveBtn = Array.from(container.querySelectorAll("button")).find((b) => b.textContent === "Save")!;
    expect(saveBtn.disabled).toBe(false);
  });

  it("Reset reverts edits", async () => {
    const onDirty = vi.fn();
    const { container } = render(<SectionAppearance snapshot={makeSnapshot()} onDirtyChange={onDirty} />);
    const select = container.querySelector("#appearance\\.theme") as HTMLSelectElement;
    fireEvent.change(select, { target: { value: "dark" } });
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(true));
    const resetBtn = Array.from(container.querySelectorAll("button")).find((b) => b.textContent === "Reset")!;
    fireEvent.click(resetBtn);
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(false));
    expect(select.value).toBe("system");
  });

  it("Save calls putSetting for each dirty key + clears dirty on success", async () => {
    const putSpy = vi.spyOn(api, "putSetting").mockResolvedValue(undefined);
    const refresh = vi.fn(async () => {});
    const onDirty = vi.fn();
    const { container } = render(<SectionAppearance snapshot={makeSnapshot(refresh)} onDirtyChange={onDirty} />);
    const sel = container.querySelector("#appearance\\.theme") as HTMLSelectElement;
    fireEvent.change(sel, { target: { value: "dark" } });
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(true));
    const saveBtn = Array.from(container.querySelectorAll("button")).find((b) => b.textContent === "Save")!;
    fireEvent.click(saveBtn);
    await waitFor(() => expect(putSpy).toHaveBeenCalledWith("appearance.theme", "dark"));
    await waitFor(() => expect(refresh).toHaveBeenCalled());
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(false));
  });

  it("partial save: failed key stays dirty + error inline (Codex r1 P2.3)", async () => {
    vi.spyOn(api, "putSetting").mockImplementation(async (key) => {
      if (key === "appearance.density") {
        const err: any = new Error("invalid value");
        err.body = { reason: "not in enum" };
        throw err;
      }
    });
    const refresh = vi.fn(async () => {});
    const onDirty = vi.fn();
    const { container, findByText } = render(<SectionAppearance snapshot={makeSnapshot(refresh)} onDirtyChange={onDirty} />);
    fireEvent.change(container.querySelector("#appearance\\.theme") as HTMLSelectElement, { target: { value: "dark" } });
    fireEvent.change(container.querySelector("#appearance\\.density") as HTMLSelectElement, { target: { value: "compact" } });
    fireEvent.click(Array.from(container.querySelectorAll("button")).find((b) => b.textContent === "Save")!);
    expect(await findByText(/Saved 1 of 2 settings/)).toBeTruthy();
    // Density still dirty (failed) → onDirty(true) at end; theme cleaned.
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(true));
    expect((container.querySelector("[role=alert]") as HTMLElement).textContent).toMatch(/not in enum/);
  });

  it("retry-success: save() clears stale error WITHOUT intervening edit (Codex r7 P2 + r8 P2)", async () => {
    // Two dirty keys. First Save: BOTH fail (transient backend error).
    // Second Save WITHOUT any intervening edit: density's mock now
    // succeeds (attempt 2), shell's mock still fails. The test verifies
    // that save() itself clears errors[density] on success — without an
    // intervening setLocal() that would clear it via the edit path.
    let densityAttempt = 0;
    vi.spyOn(api, "putSetting").mockImplementation(async (key) => {
      if (key === "appearance.density") {
        densityAttempt++;
        if (densityAttempt === 1) {
          const err: any = new Error("invalid value");
          err.body = { reason: "transient" };
          throw err;
        }
        return; // attempt 2 succeeds
      }
      if (key === "appearance.shell") {
        const err: any = new Error("invalid value");
        err.body = { reason: "still bad" };
        throw err;
      }
    });
    const refresh = vi.fn(async () => {});
    const { container, findByText } = render(
      <SectionAppearance snapshot={makeSnapshot(refresh)} onDirtyChange={() => {}} />,
    );
    fireEvent.change(container.querySelector("#appearance\\.density") as HTMLSelectElement, { target: { value: "compact" } });
    fireEvent.change(container.querySelector("#appearance\\.shell") as HTMLSelectElement, { target: { value: "bash" } });
    // First Save: BOTH fail → 2 alerts.
    fireEvent.click(Array.from(container.querySelectorAll("button")).find((b) => b.textContent === "Save")!);
    expect(await findByText(/Saved 0 of 2 settings/)).toBeTruthy();
    await waitFor(() => expect(container.querySelectorAll("[role=alert]").length).toBe(2));
    // Second Save WITHOUT editing — density succeeds, shell still fails.
    fireEvent.click(Array.from(container.querySelectorAll("button")).find((b) => b.textContent === "Save")!);
    // After save: only ONE alert remains (shell). Density's stale alert MUST
    // be cleared by save() itself, not by setLocal-on-edit.
    await waitFor(() => expect(container.querySelectorAll("[role=alert]").length).toBe(1));
    // Density's field has no error binding any more.
    const densityField = container.querySelector("#appearance\\.density");
    expect(densityField?.getAttribute("aria-describedby")).toBeNull();
    // Shell still has the error binding.
    const shellField = container.querySelector("#appearance\\.shell");
    expect(shellField?.getAttribute("aria-describedby")).toBe("appearance.shell-error");
  });
});
