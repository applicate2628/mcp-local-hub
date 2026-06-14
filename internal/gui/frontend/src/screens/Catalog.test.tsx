// internal/gui/frontend/src/screens/Catalog.test.tsx
//
// Catalog screen (v1 GUI MCP Store): browse supported/shipped MCP
// servers and install any with one click. Mirrors the Servers.test.tsx
// fetch-router idiom — declarative route → response mapping so tests
// describe the wire surface without brittle mockResolvedValueOnce order.
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, waitFor, cleanup, fireEvent, screen } from "@testing-library/preact";
import { CatalogScreen } from "./Catalog";
import type { DaemonStatus } from "../types";

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

// 204 No Content — the /api/install success shape (no JSON body).
function noContentResponse(): Response {
  return new Response(null, { status: 204 });
}

// fetchRouter dispatches each fetch call to the matching response based
// on the request URL prefix (same helper shape as Servers.test.tsx).
function fetchRouter(routes: Record<string, (init?: RequestInit) => Response>) {
  return vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input.toString();
    for (const prefix of Object.keys(routes)) {
      if (url.startsWith(prefix)) {
        return routes[prefix](init);
      }
    }
    throw new Error(`unexpected fetch: ${url}`);
  });
}

const runningMemory: DaemonStatus = {
  server: "memory",
  daemon: "default",
  port: 9123,
  state: "Running",
};

describe("CatalogScreen", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
    window.location.hash = "#/catalog";
  });
  afterEach(() => {
    cleanup();
  });

  it("renders the server list from /api/manifests + /api/status", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/manifests": () =>
          jsonResponse(200, { manifests: ["serena", "memory", "time"] }),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
      }) as unknown as typeof fetch,
    );

    render(<CatalogScreen />);
    await waitFor(() => {
      expect(screen.queryByTestId("catalog-cards")).toBeTruthy();
    });
    expect(screen.queryByTestId("catalog-card-serena")).toBeTruthy();
    expect(screen.queryByTestId("catalog-card-memory")).toBeTruthy();
    expect(screen.queryByTestId("catalog-card-time")).toBeTruthy();
  });

  it("shows an installed badge for a server in /api/status and an Install button for one that isn't", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/manifests": () =>
          jsonResponse(200, { manifests: ["memory", "time"] }),
        // memory is running; time is not installed.
        "/api/status": () => jsonResponse(200, [runningMemory]),
      }) as unknown as typeof fetch,
    );

    render(<CatalogScreen />);
    await waitFor(() => {
      expect(screen.queryByTestId("catalog-installed-memory")).toBeTruthy();
    });
    // memory → installed badge, no Install button.
    expect(screen.getByTestId("catalog-installed-memory").textContent).toBe("installed");
    expect(screen.queryByTestId("catalog-install-memory")).toBeNull();
    // time → Install button, no installed badge.
    const installBtn = screen.getByTestId("catalog-install-time") as HTMLButtonElement;
    expect(installBtn.textContent).toBe("Install");
    expect(installBtn.disabled).toBe(false);
    expect(screen.queryByTestId("catalog-installed-time")).toBeNull();
  });

  it("clicking Install POSTs /api/install?name=X and reflects success", async () => {
    let statusCallCount = 0;
    const installCalls: string[] = [];
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/manifests": () => jsonResponse(200, { manifests: ["time"] }),
        "/api/status": () => {
          statusCallCount += 1;
          // First load: time absent. After install: time running so the
          // refresh resolves it into the installed set authoritatively.
          return statusCallCount === 1
            ? jsonResponse(200, [] as DaemonStatus[])
            : jsonResponse(200, [
                { server: "time", daemon: "default", port: 9131, state: "Running" } as DaemonStatus,
              ]);
        },
        "/api/install": (init) => {
          // The URL prefix matched; capture nothing from init (the name is
          // on the query string), record the call, and return 204.
          installCalls.push(init?.method ?? "");
          return noContentResponse();
        },
      }) as unknown as typeof fetch,
    );

    const fetchSpy = globalThis.fetch as unknown as ReturnType<typeof vi.fn>;

    render(<CatalogScreen />);
    const installBtn = await screen.findByTestId("catalog-install-time");
    fireEvent.click(installBtn);

    // The installed badge appears once the POST returns 204.
    await waitFor(() => {
      expect(screen.queryByTestId("catalog-installed-time")).toBeTruthy();
    });

    // The POST carried the server name on the query string.
    const calls = fetchSpy.mock.calls.map((c) =>
      typeof c[0] === "string" ? c[0] : c[0]?.toString(),
    );
    expect(calls).toContain("/api/install?name=time");
    expect(installCalls).toEqual(["POST"]);
  });

  it("shows an inline error and keeps the Install button when /api/install fails", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/manifests": () => jsonResponse(200, { manifests: ["time"] }),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/install": () =>
          jsonResponse(500, { error: "supervisor unavailable", code: "INSTALL_FAILED" }),
      }) as unknown as typeof fetch,
    );

    render(<CatalogScreen />);
    const installBtn = (await screen.findByTestId("catalog-install-time")) as HTMLButtonElement;
    fireEvent.click(installBtn);

    await waitFor(() => {
      expect(screen.queryByTestId("catalog-error-time")).toBeTruthy();
    });
    expect(screen.getByTestId("catalog-error-time").textContent).toContain(
      "supervisor unavailable",
    );
    // Row did not crash: the Install button is still present and re-enabled.
    const retryBtn = screen.getByTestId("catalog-install-time") as HTMLButtonElement;
    expect(retryBtn.textContent).toBe("Install");
    expect(retryBtn.disabled).toBe(false);
  });

  it("renders the empty-state when /api/manifests returns an empty list", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/manifests": () => jsonResponse(200, { manifests: [] }),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
      }) as unknown as typeof fetch,
    );

    render(<CatalogScreen />);
    await waitFor(() => {
      expect(screen.queryByTestId("catalog-empty")).toBeTruthy();
    });
    expect(screen.getByTestId("catalog-empty").textContent).toContain(
      "No supported servers found",
    );
    // No card grid in the empty state.
    expect(screen.queryByTestId("catalog-cards")).toBeNull();
  });

  it("renders an error banner when /api/manifests fails", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/manifests": () =>
          jsonResponse(500, { error: "internal error listing manifests", code: "MANIFEST_LIST_FAILED" }),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
      }) as unknown as typeof fetch,
    );

    render(<CatalogScreen />);
    const banner = await screen.findByTestId("catalog-error");
    expect(banner.textContent).toContain("internal error listing manifests");
  });
});
