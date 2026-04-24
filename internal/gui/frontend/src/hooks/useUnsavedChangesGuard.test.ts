import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook } from "@testing-library/preact";
import { useUnsavedChangesGuard } from "./useUnsavedChangesGuard";

describe("useUnsavedChangesGuard", () => {
  let beforeUnloadHandler: ((e: BeforeUnloadEvent) => void) | null = null;
  beforeEach(() => {
    beforeUnloadHandler = null;
    vi.spyOn(window, "addEventListener").mockImplementation((ev, h) => {
      if (ev === "beforeunload") beforeUnloadHandler = h as (e: BeforeUnloadEvent) => void;
    });
  });
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("does not call preventDefault when clean", () => {
    renderHook(() => useUnsavedChangesGuard(false));
    const e = new Event("beforeunload") as BeforeUnloadEvent;
    e.preventDefault = vi.fn();
    (e as any).returnValue = undefined;
    beforeUnloadHandler?.(e);
    expect(e.preventDefault).not.toHaveBeenCalled();
  });

  it("calls preventDefault + sets returnValue when dirty", () => {
    renderHook(() => useUnsavedChangesGuard(true));
    const e = new Event("beforeunload") as BeforeUnloadEvent;
    e.preventDefault = vi.fn();
    (e as any).returnValue = undefined;
    beforeUnloadHandler?.(e);
    expect(e.preventDefault).toHaveBeenCalled();
    expect((e as any).returnValue).toBeTruthy();
  });
});
