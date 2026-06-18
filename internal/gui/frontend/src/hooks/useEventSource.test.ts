import { describe, it, expect, vi, afterEach } from "vitest";
import { renderHook, act } from "@testing-library/preact";
import { useEventSource } from "./useEventSource";

// happy-dom does not ship EventSource. This stub captures onopen/onerror
// so the test can drive the same property callbacks a real EventSource
// fires, plus tracks close() for the lifecycle assertion.
const stubInstances = new Set<StubEventSource>();
class StubEventSource {
  url: string;
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  closed = false;
  constructor(url: string) {
    this.url = url;
    stubInstances.add(this);
  }
  addEventListener(): void {}
  removeEventListener(): void {}
  close(): void {
    this.closed = true;
    stubInstances.delete(this);
  }
  // Test helpers — fire the property callbacks the way the browser would.
  fireOpen(): void {
    this.onopen?.(new Event("open"));
  }
  fireError(): void {
    this.onerror?.(new Event("error"));
  }
}
(globalThis as unknown as { EventSource: typeof StubEventSource }).EventSource =
  StubEventSource;

function onlyInstance(): StubEventSource {
  const list = Array.from(stubInstances);
  expect(list.length).toBe(1);
  return list[0]!;
}

describe("useEventSource — connection state", () => {
  afterEach(() => {
    stubInstances.clear();
    vi.restoreAllMocks();
  });

  it("starts in 'connecting' before any onopen", () => {
    const { result } = renderHook(() => useEventSource("/api/events", {}));
    expect(result.current).toBe("connecting");
  });

  it("reports 'open' after onopen fires", () => {
    const { result } = renderHook(() => useEventSource("/api/events", {}));
    act(() => {
      onlyInstance().fireOpen();
    });
    expect(result.current).toBe("open");
  });

  it("reports 'reconnecting' after an onerror event", () => {
    const { result } = renderHook(() => useEventSource("/api/events", {}));
    act(() => {
      onlyInstance().fireOpen();
    });
    expect(result.current).toBe("open");
    act(() => {
      onlyInstance().fireError();
    });
    expect(result.current).toBe("reconnecting");
  });

  it("returns to 'open' on the next onopen after a reconnecting state", () => {
    const { result } = renderHook(() => useEventSource("/api/events", {}));
    act(() => {
      onlyInstance().fireError();
    });
    expect(result.current).toBe("reconnecting");
    // Native EventSource auto-retries; the next successful connect fires
    // onopen again, which must flip the state back to live.
    act(() => {
      onlyInstance().fireOpen();
    });
    expect(result.current).toBe("open");
  });

  it("closes the stream on unmount", () => {
    const { unmount } = renderHook(() => useEventSource("/api/events", {}));
    const es = onlyInstance();
    expect(es.closed).toBe(false);
    unmount();
    expect(es.closed).toBe(true);
  });
});
