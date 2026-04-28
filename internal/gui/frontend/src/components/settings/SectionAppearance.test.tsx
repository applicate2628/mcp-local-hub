import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, fireEvent, waitFor, within } from "@testing-library/preact";
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

  it("Codex r6 P2 (updated for r9): keeps saved value visible when snapshot refresh fails", async () => {
    // Successful PUT but refresh fails. Under r9 architecture, the value
    // moves from edits → lastSent on PUT success, so edits is empty and
    // the section is NO LONGER dirty. However, effective() still returns
    // the saved value (via lastSent), so the select shows "dark" correctly.
    // The "click Save again" recovery path is superseded: no pending edits
    // remain and the value is preserved for display via lastSent.
    vi.spyOn(api, "putSetting").mockResolvedValue(undefined);
    const refresh = vi.fn(async () => { throw new Error("network"); });
    const onDirty = vi.fn();
    const { container, findByText } = render(
      <SectionAppearance snapshot={makeSnapshot(refresh)} onDirtyChange={onDirty} />,
    );
    const sel = container.querySelector("#appearance\\.theme") as HTMLSelectElement;
    fireEvent.change(sel, { target: { value: "dark" } });
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(true));
    fireEvent.click(Array.from(container.querySelectorAll("button")).find((b) => b.textContent === "Save")!);
    // Banner indicates partial: "Saved on disk. The live view didn't refresh..."
    expect(await findByText(/Saved on disk/)).toBeTruthy();
    // The select STILL shows "dark" (saved value, now via lastSent fallback).
    await waitFor(() => expect(sel.value).toBe("dark"));
    // r9: dirty is now FALSE — edits is empty, lastSent is not counted as dirty.
    // The value is preserved via lastSent, not by keeping edits populated.
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(false));
    const saveBtn = Array.from(container.querySelectorAll("button")).find((b) => b.textContent === "Save")!;
    // Save is disabled (no pending edits to save).
    expect(saveBtn.disabled).toBe(true);
  });

  it("Codex r9 P2: Reset preserves saved values when refresh failed", async () => {
    // Save successfully PUTs the value, but refresh throws. lastSent retains
    // the value. User clicks Reset → edits cleared but lastSent preserved →
    // effective() still returns the saved value. Section is clean (not dirty).
    vi.spyOn(api, "putSetting").mockResolvedValue(undefined);
    const refresh = vi.fn(async () => { throw new Error("network"); });
    const onDirty = vi.fn();
    const { container, findByText } = render(
      <SectionAppearance snapshot={makeSnapshot(refresh)} onDirtyChange={onDirty} />,
    );
    const sel = container.querySelector("#appearance\\.theme") as HTMLSelectElement;
    fireEvent.change(sel, { target: { value: "dark" } });
    fireEvent.click(Array.from(container.querySelectorAll("button")).find((b) => b.textContent === "Save")!);
    expect(await findByText(/Saved on disk/)).toBeTruthy();
    // Click Reset.
    fireEvent.click(Array.from(container.querySelectorAll("button")).find((b) => b.textContent === "Reset")!);
    // Select STILL shows "dark" (lastSent retains it).
    await waitFor(() => expect(sel.value).toBe("dark"));
    // dirty is false (no pending edits).
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(false));
  });

  it("Codex r12 P2: failure error is NOT shown when user re-edited during in-flight PUT", async () => {
    // User edits theme=dark, clicks Save. PUT for "dark" is held in flight.
    // User re-edits theme=light during the PUT. PUT then rejects with an error.
    // The failure references "dark" — a value the user has already abandoned.
    // The inline error must NOT appear because the CAS check (live edit ≠ sent)
    // tells us the failure is stale relative to the user's current edit.
    let rejectTheme!: (e: Error) => void;
    const themeDelay = new Promise<void>((_, rej) => { rejectTheme = rej; });
    vi.spyOn(api, "putSetting").mockImplementation(async (key) => {
      if (key === "appearance.theme") {
        await themeDelay;
      }
    });
    const refresh = vi.fn(async () => {});
    const onDirty = vi.fn();
    const { container } = render(
      <SectionAppearance snapshot={makeSnapshot(refresh)} onDirtyChange={onDirty} />,
    );
    const scope = within(container as HTMLElement);
    const sel = container.querySelector("#appearance\\.theme") as HTMLSelectElement;
    // First edit: theme = "dark"
    fireEvent.change(sel, { target: { value: "dark" } });
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(true));
    // Click Save (PUT now in-flight, blocked on themeDelay)
    fireEvent.click(Array.from(container.querySelectorAll("button")).find((b) => b.textContent === "Save")!);
    // Re-edit DURING in-flight PUT: theme = "light"
    fireEvent.change(sel, { target: { value: "light" } });
    // PUT for "dark" now fails — but the user is on "light"
    rejectTheme(Object.assign(new Error("server error"), { body: { reason: "bad value" } }));
    await waitFor(() => expect(onDirty).toHaveBeenCalledWith(true));
    // No inline error: the failure was for "dark", but user holds "light".
    // CAS check (live "light" ≠ sent "dark") must suppress the error.
    // Use within(container) to scope the role query to THIS render only.
    await waitFor(() => expect(scope.queryByRole("alert")).toBeNull());
    // The user's "light" value is still present and dirty.
    expect(sel.value).toBe("light");
  });

  it("Codex r12 P3: refresh-failure banner says reload, not 'click Save again'", async () => {
    // PUT succeeds, refresh fails. Banner must not say "click Save again"
    // because Save is now disabled (edits cleared by CAS on success).
    vi.spyOn(api, "putSetting").mockResolvedValue(undefined);
    const refresh = vi.fn(async () => { throw new Error("network"); });
    const { container } = render(
      <SectionAppearance snapshot={makeSnapshot(refresh)} onDirtyChange={() => {}} />,
    );
    const scope = within(container as HTMLElement);
    fireEvent.change(
      container.querySelector("#appearance\\.theme") as HTMLSelectElement,
      { target: { value: "dark" } },
    );
    fireEvent.click(Array.from(container.querySelectorAll("button")).find((b) => b.textContent === "Save")!);
    // Use within(container) so we don't pick up banners from other renders
    // that may still be in the document from prior tests.
    const banner = await scope.findByText(/Saved on disk/);
    expect(banner.textContent).not.toMatch(/click Save again/);
    expect(banner.textContent).toMatch(/reload|revisit/i);
  });

  it("Codex PR P1: edit during in-flight Save preserves newer edit (compare-and-swap)", async () => {
    // User clicks Save, then edits same field BEFORE PUT returns.
    // Newer edit must survive the merge step — old behavior would silently
    // drop it because edits[key] is unconditionally cleared on success.
    let resolveTheme!: () => void;
    const themeDelay = new Promise<void>((res) => { resolveTheme = res; });
    vi.spyOn(api, "putSetting").mockImplementation(async (key) => {
      if (key === "appearance.theme") {
        await themeDelay; // hold open until we resolve manually
      }
    });
    const refresh = vi.fn(async () => {});
    const onDirty = vi.fn();
    const { container } = render(<SectionAppearance snapshot={makeSnapshot(refresh)} onDirtyChange={onDirty} />);
    const sel = container.querySelector("#appearance\\.theme") as HTMLSelectElement;
    // First edit: theme = "dark"
    fireEvent.change(sel, { target: { value: "dark" } });
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(true));
    // Click Save (PUT now in-flight, blocked on themeDelay)
    fireEvent.click(Array.from(container.querySelectorAll("button")).find((b) => b.textContent === "Save")!);
    // Second edit DURING in-flight save: theme = "light" (newer, unsaved)
    fireEvent.change(sel, { target: { value: "light" } });
    // Resolve the PUT — server got "dark", client local is "light"
    resolveTheme();
    await waitFor(() => expect(refresh).toHaveBeenCalled());
    // Assertion: the newer "light" edit MUST still be dirty.
    // - dropdown shows "light" (the newer local value)
    // - Save button enabled (section is dirty)
    // - onDirty(true) is the latest call
    expect(sel.value).toBe("light");
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(true));
    const saveBtn = Array.from(container.querySelectorAll("button")).find((b) => b.textContent === "Save")!;
    expect(saveBtn.disabled).toBe(false);
  });
});
