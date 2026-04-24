import { describe, it, expect, beforeEach, vi } from "vitest";
import { renderHook, act, cleanup } from "@testing-library/preact";
import { useRouter } from "./useRouter";

describe("useRouter guard (A2b)", () => {
  beforeEach(() => {
    // cleanup() flushes any pending Preact useEffect cleanup (including
    // removeEventListener) from prior tests before resetting hash state.
    // Without this, a stale declined-guard listener from a previous test
    // reverts the URL during the next test's dispatch, corrupting parse().
    cleanup();
    window.location.hash = "";
  });

  it("calls guard on hashchange when installed", () => {
    const guard = vi.fn(() => true);
    renderHook(() => useRouter("servers", guard));
    act(() => {
      window.location.hash = "#/migration";
      window.dispatchEvent(new HashChangeEvent("hashchange", {
        oldURL: "http://localhost/",
        newURL: "http://localhost/#/migration",
      }));
    });
    expect(guard).toHaveBeenCalled();
  });

  it("reverts hash via replaceState when guard returns false; screen stays", () => {
    window.location.hash = "#/add-server";
    const guard = vi.fn(() => false);
    const { result } = renderHook(() => useRouter("servers", guard));
    // Initial screen derived from hash = add-server.
    expect(result.current.screen).toBe("add-server");
    act(() => {
      window.history.pushState(null, "", "#/migration");
      window.dispatchEvent(new HashChangeEvent("hashchange", {
        oldURL: "http://localhost/#/add-server",
        newURL: "http://localhost/#/migration",
      }));
    });
    // Guard declined → internal state stays on add-server.
    expect(result.current.screen).toBe("add-server");
  });

  it("returns defaultScreen when hash is empty", () => {
    window.location.hash = "";
    const { result } = renderHook(() => useRouter("servers"));
    expect(result.current.screen).toBe("servers");
    expect(result.current.query).toBe("");
  });

  it("parses query-string after '?'", () => {
    window.location.hash = "#/edit-server?name=demo";
    const { result } = renderHook(() => useRouter("servers"));
    expect(result.current.screen).toBe("edit-server");
    expect(result.current.query).toBe("name=demo");
  });
});

describe("useRouter same-key-different-query (A2b P2-2)", () => {
  beforeEach(() => {
    cleanup();
    window.location.hash = "";
  });

  it("calls guard when query changes even if screen key stays the same", () => {
    window.location.hash = "#/edit-server?name=a";
    const guard = vi.fn(() => true);
    renderHook(() => useRouter("servers", guard));
    guard.mockClear();
    act(() => {
      window.history.pushState(null, "", "#/edit-server?name=b");
      window.dispatchEvent(new HashChangeEvent("hashchange", {
        oldURL: "http://localhost/#/edit-server?name=a",
        newURL: "http://localhost/#/edit-server?name=b",
      }));
    });
    expect(guard).toHaveBeenCalledWith({ screen: "edit-server", query: "name=b" });
  });

  it("query state does NOT change when guard returns false on same-key nav", () => {
    window.location.hash = "#/edit-server?name=a";
    const guard = vi.fn(() => false);
    const { result } = renderHook(() => useRouter("servers", guard));
    expect(result.current.query).toBe("name=a");
    act(() => {
      window.history.pushState(null, "", "#/edit-server?name=b");
      window.dispatchEvent(new HashChangeEvent("hashchange", {
        oldURL: "http://localhost/#/edit-server?name=a",
        newURL: "http://localhost/#/edit-server?name=b",
      }));
    });
    // Declined — internal state stays on a.
    expect(result.current.query).toBe("name=a");
  });
});

describe("useRouter guard stability (R2b-Q3)", () => {
  beforeEach(() => {
    cleanup();
    window.location.hash = "";
  });

  it("uses the latest guard on hashchange even when caller passes a fresh inline arrow each render", async () => {
    // Simulate a caller that re-renders with a new arrow each time:
    // first render installs a "allow all" guard, then we re-render with
    // a "block all" guard and verify the NEW guard is consulted.
    const guard1 = vi.fn(() => true);
    const guard2 = vi.fn(() => false);

    const { result, rerender } = renderHook(
      ({ g }: { g: () => boolean }) => useRouter("servers", g),
      { initialProps: { g: guard1 } },
    );

    // First nav accepted by guard1.
    act(() => {
      window.history.pushState(null, "", "#/migration");
      window.dispatchEvent(new HashChangeEvent("hashchange", {
        oldURL: "http://localhost/",
        newURL: "http://localhost/#/migration",
      }));
    });
    expect(guard1).toHaveBeenCalled();
    expect(result.current.screen).toBe("migration");

    // Re-render with guard2. If the hook subscribed per-render, we'd
    // see a re-subscription; with the ref-based design the listener is
    // the same instance and reads the latest guardRef.current.
    rerender({ g: guard2 });

    // Second nav is declined by guard2.
    act(() => {
      window.history.pushState(null, "", "#/servers");
      window.dispatchEvent(new HashChangeEvent("hashchange", {
        oldURL: "http://localhost/#/migration",
        newURL: "http://localhost/#/servers",
      }));
    });
    expect(guard2).toHaveBeenCalled();
    // Declined → internal state stays on migration.
    expect(result.current.screen).toBe("migration");
  });
});

describe("useRouter empty oldURL safety (R2b-Q4)", () => {
  beforeEach(() => {
    cleanup();
    window.location.hash = "#/servers";
  });

  it("does not throw when the declined hashchange has empty oldURL", () => {
    const guard = vi.fn(() => false);
    const { result } = renderHook(() => useRouter("servers", guard));
    // Trigger a hashchange whose oldURL is an empty string (happy-dom
    // sends "" on the first nav from a blank page). Before the Q4 fix
    // this threw TypeError inside the listener and swallowed silently,
    // leaving the URL in the declined state.
    expect(() => {
      act(() => {
        window.history.pushState(null, "", "#/add-server");
        window.dispatchEvent(new HashChangeEvent("hashchange", {
          oldURL: "",
          newURL: "http://localhost/#/add-server",
        }));
      });
    }).not.toThrow();
    // Guard was consulted; state did not advance.
    expect(guard).toHaveBeenCalled();
    expect(result.current.screen).toBe("servers");
  });
});
