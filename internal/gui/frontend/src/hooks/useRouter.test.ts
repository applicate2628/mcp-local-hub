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
