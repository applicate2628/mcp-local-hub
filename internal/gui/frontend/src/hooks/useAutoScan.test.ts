import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook, act } from "@testing-library/preact";
import { useAutoScan, SCAN_POLL_MS } from "./useAutoScan";

// Helper: force document.hidden + visibilityState and fire the event.
// happy-dom exposes `document` but `hidden` is a getter; redefine it so
// the hook's `document.hidden` guard reads our value, then dispatch the
// real `visibilitychange` event the hook listens for.
function setHidden(hidden: boolean) {
  Object.defineProperty(document, "hidden", {
    configurable: true,
    get: () => hidden,
  });
  Object.defineProperty(document, "visibilityState", {
    configurable: true,
    get: () => (hidden ? "hidden" : "visible"),
  });
  document.dispatchEvent(new Event("visibilitychange"));
}

describe("useAutoScan", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    setHidden(false);
  });
  afterEach(() => {
    setHidden(false);
    vi.restoreAllMocks();
  });

  it("runs the initial fetch on mount", async () => {
    const run = vi.fn(async () => {});
    renderHook(() => useAutoScan(run, false));
    // Flush the microtask from the async initial run.
    await act(async () => {});
    expect(run).toHaveBeenCalledTimes(1);
  });

  it("re-fetches after SCAN_POLL_MS via the interval", async () => {
    const run = vi.fn(async () => {});
    renderHook(() => useAutoScan(run, false));
    await act(async () => {});
    expect(run).toHaveBeenCalledTimes(1); // initial

    await act(async () => {
      vi.advanceTimersByTime(SCAN_POLL_MS);
    });
    expect(run).toHaveBeenCalledTimes(2); // one poll tick

    await act(async () => {
      vi.advanceTimersByTime(SCAN_POLL_MS);
    });
    expect(run).toHaveBeenCalledTimes(3); // second poll tick
  });

  it("skips the interval tick while document.hidden is true", async () => {
    const run = vi.fn(async () => {});
    renderHook(() => useAutoScan(run, false));
    await act(async () => {});
    expect(run).toHaveBeenCalledTimes(1); // initial (tab visible at mount)

    // Hide the tab — the becoming-hidden visibilitychange must NOT fetch,
    // and subsequent interval ticks must be skipped.
    act(() => setHidden(true));
    await act(async () => {
      vi.advanceTimersByTime(SCAN_POLL_MS * 3);
    });
    expect(run).toHaveBeenCalledTimes(1); // no extra fetches while hidden
  });

  it("does one immediate refresh when the tab becomes visible again", async () => {
    const run = vi.fn(async () => {});
    renderHook(() => useAutoScan(run, false));
    await act(async () => {});
    expect(run).toHaveBeenCalledTimes(1);

    act(() => setHidden(true));
    await act(async () => {
      vi.advanceTimersByTime(SCAN_POLL_MS);
    });
    expect(run).toHaveBeenCalledTimes(1); // still 1 — hidden

    // Becoming visible fires one immediate refresh.
    await act(async () => {
      setHidden(false);
    });
    expect(run).toHaveBeenCalledTimes(2);
  });

  it("skips polling and on-visible refresh while paused", async () => {
    const run = vi.fn(async () => {});
    renderHook(() => useAutoScan(run, true)); // paused from mount
    await act(async () => {});
    expect(run).toHaveBeenCalledTimes(1); // initial fetch still fires

    await act(async () => {
      vi.advanceTimersByTime(SCAN_POLL_MS * 2);
    });
    expect(run).toHaveBeenCalledTimes(1); // paused → no poll ticks

    // Visibility flip while paused must not refetch either.
    act(() => setHidden(true));
    await act(async () => {
      setHidden(false);
    });
    expect(run).toHaveBeenCalledTimes(1);
  });

  it("resumes polling once paused flips back to false", async () => {
    const run = vi.fn(async () => {});
    const { rerender } = renderHook(
      ({ paused }: { paused: boolean }) => useAutoScan(run, paused),
      { initialProps: { paused: true } },
    );
    await act(async () => {});
    expect(run).toHaveBeenCalledTimes(1);

    await act(async () => {
      vi.advanceTimersByTime(SCAN_POLL_MS);
    });
    expect(run).toHaveBeenCalledTimes(1); // paused

    rerender({ paused: false });
    await act(async () => {
      vi.advanceTimersByTime(SCAN_POLL_MS);
    });
    expect(run).toHaveBeenCalledTimes(2); // resumed
  });

  it("rescan() triggers an immediate fetch and resets the interval", async () => {
    const run = vi.fn(async () => {});
    const { result } = renderHook(() => useAutoScan(run, false));
    await act(async () => {});
    expect(run).toHaveBeenCalledTimes(1);

    // Halfway to the next poll, the operator clicks Rescan.
    await act(async () => {
      vi.advanceTimersByTime(SCAN_POLL_MS / 2);
    });
    await act(async () => {
      result.current.rescan();
    });
    expect(run).toHaveBeenCalledTimes(2); // immediate fetch from rescan

    // The interval was reset, so the original remaining half-period does
    // NOT fire; a full period from the rescan does.
    await act(async () => {
      vi.advanceTimersByTime(SCAN_POLL_MS / 2);
    });
    expect(run).toHaveBeenCalledTimes(2); // no tick yet (timer reset)

    await act(async () => {
      vi.advanceTimersByTime(SCAN_POLL_MS / 2);
    });
    expect(run).toHaveBeenCalledTimes(3); // full period after rescan
  });

  it("exposes a live 'ago' elapsed-seconds value", async () => {
    const run = vi.fn(async () => {});
    const { result } = renderHook(() => useAutoScan(run, false));
    await act(async () => {});
    // First scan just resolved → agoSeconds ~0.
    expect(result.current.agoSeconds).toBe(0);
    expect(result.current.lastScanAt).not.toBeNull();

    await act(async () => {
      vi.advanceTimersByTime(3_000);
    });
    expect(result.current.agoSeconds).toBe(3);
  });

  it("clears the interval and visibility listener on unmount", async () => {
    const run = vi.fn(async () => {});
    const removeSpy = vi.spyOn(document, "removeEventListener");
    const { unmount } = renderHook(() => useAutoScan(run, false));
    await act(async () => {});
    const callsBefore = run.mock.calls.length;
    unmount();
    expect(removeSpy).toHaveBeenCalledWith(
      "visibilitychange",
      expect.any(Function),
    );
    await act(async () => {
      vi.advanceTimersByTime(SCAN_POLL_MS * 3);
    });
    expect(run.mock.calls.length).toBe(callsBefore); // no fetches after unmount
  });
});
