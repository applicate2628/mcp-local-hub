import { describe, expect, it, beforeEach, vi, afterEach } from "vitest";
import {
  pushToast,
  dismissToast,
  subscribeToasts,
  clearAllToasts,
  type Toast,
} from "./toast-store";

describe("toast-store", () => {
  beforeEach(() => {
    clearAllToasts();
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    clearAllToasts();
  });

  it("pushToast enqueues a toast and notifies subscribers", () => {
    let snapshot: readonly Toast[] = [];
    const unsub = subscribeToasts((t) => (snapshot = t));
    expect(snapshot).toHaveLength(0); // immediate empty push on subscribe
    pushToast("success", "Saved.");
    expect(snapshot).toHaveLength(1);
    expect(snapshot[0].variant).toBe("success");
    expect(snapshot[0].message).toBe("Saved.");
    unsub();
  });

  it("assigns monotonically increasing ids", () => {
    const a = pushToast("info", "a");
    const b = pushToast("info", "b");
    expect(b).toBeGreaterThan(a);
  });

  it("subscribe immediately receives the current snapshot (existing toasts paint)", () => {
    pushToast("warning", "already here");
    let snapshot: readonly Toast[] = [];
    const unsub = subscribeToasts((t) => (snapshot = t));
    expect(snapshot).toHaveLength(1);
    expect(snapshot[0].message).toBe("already here");
    unsub();
  });

  it("dismissToast removes one toast by id and is idempotent", () => {
    let snapshot: readonly Toast[] = [];
    const unsub = subscribeToasts((t) => (snapshot = t));
    const id = pushToast("danger", "boom");
    expect(snapshot).toHaveLength(1);
    dismissToast(id);
    expect(snapshot).toHaveLength(0);
    // Dismissing again is a no-op (no throw, no spurious emit).
    dismissToast(id);
    expect(snapshot).toHaveLength(0);
    unsub();
  });

  it("auto-dismisses success toasts after the default timeout", () => {
    let snapshot: readonly Toast[] = [];
    const unsub = subscribeToasts((t) => (snapshot = t));
    pushToast("success", "auto gone");
    expect(snapshot).toHaveLength(1);
    vi.advanceTimersByTime(4000);
    expect(snapshot).toHaveLength(0);
    unsub();
  });

  it("danger toasts are sticky (never auto-dismiss)", () => {
    let snapshot: readonly Toast[] = [];
    const unsub = subscribeToasts((t) => (snapshot = t));
    pushToast("danger", "stays until acknowledged");
    vi.advanceTimersByTime(60_000);
    expect(snapshot).toHaveLength(1);
    unsub();
  });

  it("honors an explicit timeoutMs override (0 = sticky)", () => {
    let snapshot: readonly Toast[] = [];
    const unsub = subscribeToasts((t) => (snapshot = t));
    pushToast("success", "sticky override", { timeoutMs: 0 });
    vi.advanceTimersByTime(60_000);
    expect(snapshot).toHaveLength(1);
    unsub();
  });

  it("clearAllToasts drops every toast and cancels pending timers", () => {
    let snapshot: readonly Toast[] = [];
    const unsub = subscribeToasts((t) => (snapshot = t));
    pushToast("success", "one");
    pushToast("info", "two");
    expect(snapshot).toHaveLength(2);
    clearAllToasts();
    expect(snapshot).toHaveLength(0);
    // No timer left to fire and re-trigger an emit.
    vi.advanceTimersByTime(10_000);
    expect(snapshot).toHaveLength(0);
    unsub();
  });

  it("unsubscribe stops further notifications", () => {
    let count = 0;
    const unsub = subscribeToasts(() => (count += 1));
    const afterSubscribe = count; // 1 from the immediate push
    unsub();
    pushToast("info", "after unsub");
    expect(count).toBe(afterSubscribe);
  });
});
