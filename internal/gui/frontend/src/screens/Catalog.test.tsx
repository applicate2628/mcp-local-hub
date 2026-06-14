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

// Catalog entry builder — the GET /api/catalog row shape ({name,
// description, kind}). Defaults keep each test terse; override per case.
function entry(name: string, description = `desc for ${name}`, kind = "global") {
  return { name, description, kind };
}

// Marketplace entry builder — the GET /api/marketplace row shape
// ({id, name, summary, categories, homepage}). The read-only browse view
// renders these in the Marketplace section below the shipped-server store.
function mpEntry(
  id: string,
  name = id,
  summary = `summary for ${id}`,
  categories: string[] = [],
  homepage = "",
) {
  return { id, name, summary, categories, homepage };
}

// Default empty-marketplace route so the shipped-server tests that don't
// care about the marketplace section don't trip fetchRouter's
// "unexpected fetch" guard on the component's /api/marketplace load.
const emptyMarketplace = () => jsonResponse(200, { entries: [] });

describe("CatalogScreen", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
    window.location.hash = "#/catalog";
  });
  afterEach(() => {
    cleanup();
  });

  it("renders the server list + descriptions from /api/catalog + /api/status", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/catalog": () =>
          jsonResponse(200, {
            catalog: [
              entry("serena", "Semantic code toolkit — LSP-backed symbol search."),
              entry("memory", "Persistent knowledge-graph memory across sessions."),
              entry("time"),
            ],
          }),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/marketplace": emptyMarketplace,
      }) as unknown as typeof fetch,
    );

    render(<CatalogScreen />);
    await waitFor(() => {
      expect(screen.queryByTestId("catalog-cards")).toBeTruthy();
    });
    expect(screen.queryByTestId("catalog-card-serena")).toBeTruthy();
    expect(screen.queryByTestId("catalog-card-memory")).toBeTruthy();
    expect(screen.queryByTestId("catalog-card-time")).toBeTruthy();
    // The one-line description renders under each server name.
    expect(screen.getByTestId("catalog-desc-serena").textContent).toContain(
      "Semantic code toolkit",
    );
    expect(screen.getByTestId("catalog-desc-memory").textContent).toContain(
      "Persistent knowledge-graph memory",
    );
  });

  it("shows an installed badge for a server in /api/status and an Install button for one that isn't", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/catalog": () =>
          jsonResponse(200, { catalog: [entry("memory"), entry("time")] }),
        // memory is running; time is not installed.
        "/api/status": () => jsonResponse(200, [runningMemory]),
        "/api/marketplace": emptyMarketplace,
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
        "/api/catalog": () => jsonResponse(200, { catalog: [entry("time")] }),
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
        "/api/marketplace": emptyMarketplace,
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
        "/api/catalog": () => jsonResponse(200, { catalog: [entry("time")] }),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/install": () =>
          jsonResponse(500, { error: "supervisor unavailable", code: "INSTALL_FAILED" }),
        "/api/marketplace": emptyMarketplace,
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

  it("renders the empty-state when /api/catalog returns an empty list", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/catalog": () => jsonResponse(200, { catalog: [] }),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/marketplace": emptyMarketplace,
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

  it("renders an error banner when /api/catalog fails", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/catalog": () =>
          jsonResponse(500, { error: "internal error listing catalog", code: "CATALOG_LIST_FAILED" }),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/marketplace": emptyMarketplace,
      }) as unknown as typeof fetch,
    );

    render(<CatalogScreen />);
    const banner = await screen.findByTestId("catalog-error");
    expect(banner.textContent).toContain("internal error listing catalog");
  });

  // ---- §10 v2b: search/filter ----

  it("filters the shipped-server cards by name + description as the operator types", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/catalog": () =>
          jsonResponse(200, {
            catalog: [
              entry("serena", "Semantic code toolkit — LSP-backed symbol search."),
              entry("memory", "Persistent knowledge-graph memory across sessions."),
              entry("time", "Clock and timezone helpers."),
            ],
          }),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/marketplace": emptyMarketplace,
      }) as unknown as typeof fetch,
    );

    render(<CatalogScreen />);
    const search = (await screen.findByTestId("catalog-search")) as HTMLInputElement;

    // Match by name substring → only serena remains.
    fireEvent.input(search, { target: { value: "seren" } });
    await waitFor(() => {
      expect(screen.queryByTestId("catalog-card-memory")).toBeNull();
    });
    expect(screen.queryByTestId("catalog-card-serena")).toBeTruthy();
    expect(screen.queryByTestId("catalog-card-time")).toBeNull();

    // Match by description substring (case-insensitive) → only memory.
    fireEvent.input(search, { target: { value: "KNOWLEDGE-GRAPH" } });
    await waitFor(() => {
      expect(screen.queryByTestId("catalog-card-memory")).toBeTruthy();
    });
    expect(screen.queryByTestId("catalog-card-serena")).toBeNull();

    // No matches → the no-matches empty state, no card grid.
    fireEvent.input(search, { target: { value: "zzz-nope" } });
    await waitFor(() => {
      expect(screen.queryByTestId("catalog-no-matches")).toBeTruthy();
    });
    expect(screen.queryByTestId("catalog-cards")).toBeNull();

    // Clearing the query restores every card.
    fireEvent.input(search, { target: { value: "" } });
    await waitFor(() => {
      expect(screen.queryByTestId("catalog-card-time")).toBeTruthy();
    });
    expect(screen.queryByTestId("catalog-card-serena")).toBeTruthy();
    expect(screen.queryByTestId("catalog-card-memory")).toBeTruthy();
  });

  // ---- §10 v2b: uninstall-from-card ----

  it("uninstalls an installed server via DELETE /api/install/:server after confirm", async () => {
    let statusCallCount = 0;
    const deleteCalls: string[] = [];
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/catalog": () => jsonResponse(200, { catalog: [entry("memory")] }),
        "/api/status": () => {
          statusCallCount += 1;
          // First load: memory running (installed). After uninstall: gone.
          return statusCallCount === 1
            ? jsonResponse(200, [runningMemory])
            : jsonResponse(200, [] as DaemonStatus[]);
        },
        // DELETE /api/install/memory returns 200 with the uninstall report.
        "/api/install/": (init) => {
          deleteCalls.push(init?.method ?? "");
          return jsonResponse(200, {
            uninstall_results: {
              server: "memory",
              tasks_deleted: ["\\mcp-local-hub-memory-default"],
              task_delete_warns: [],
              clients_updated: [],
              client_warns: [],
            },
          });
        },
        "/api/marketplace": emptyMarketplace,
      }) as unknown as typeof fetch,
    );
    const fetchSpy = globalThis.fetch as unknown as ReturnType<typeof vi.fn>;

    render(<CatalogScreen />);
    // memory starts installed → Uninstall affordance present.
    const uninstallBtn = await screen.findByTestId("catalog-uninstall-memory");
    fireEvent.click(uninstallBtn);

    // Confirm gate appears; click it to issue the DELETE.
    const confirmBtn = await screen.findByTestId("catalog-uninstall-confirm-memory");
    fireEvent.click(confirmBtn);

    // After the DELETE + status refresh, memory is no longer installed →
    // the Install button reappears (and the installed badge is gone).
    await waitFor(() => {
      expect(screen.queryByTestId("catalog-install-memory")).toBeTruthy();
    });
    expect(screen.queryByTestId("catalog-installed-memory")).toBeNull();

    // The DELETE carried the server name on the path.
    const calls = fetchSpy.mock.calls.map((c) =>
      typeof c[0] === "string" ? c[0] : c[0]?.toString(),
    );
    expect(calls).toContain("/api/install/memory");
    expect(deleteCalls).toEqual(["DELETE"]);
  });

  it("cancelling the uninstall confirm leaves the server installed and fires no DELETE", async () => {
    const deleteCalls: string[] = [];
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/catalog": () => jsonResponse(200, { catalog: [entry("memory")] }),
        "/api/status": () => jsonResponse(200, [runningMemory]),
        "/api/install/": (init) => {
          deleteCalls.push(init?.method ?? "");
          return jsonResponse(200, { uninstall_results: {} });
        },
        "/api/marketplace": emptyMarketplace,
      }) as unknown as typeof fetch,
    );

    render(<CatalogScreen />);
    const uninstallBtn = await screen.findByTestId("catalog-uninstall-memory");
    fireEvent.click(uninstallBtn);
    const cancelBtn = await screen.findByTestId("catalog-uninstall-cancel-memory");
    fireEvent.click(cancelBtn);

    // Back to the plain Uninstall affordance; still installed; no DELETE.
    await waitFor(() => {
      expect(screen.queryByTestId("catalog-uninstall-memory")).toBeTruthy();
    });
    expect(screen.queryByTestId("catalog-installed-memory")).toBeTruthy();
    expect(deleteCalls).toEqual([]);
  });

  it("shows an inline error and keeps the row when the uninstall DELETE fails", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/catalog": () => jsonResponse(200, { catalog: [entry("memory")] }),
        "/api/status": () => jsonResponse(200, [runningMemory]),
        "/api/install/": () =>
          jsonResponse(500, { error: "load manifest memory: not found", code: "UNINSTALL_FAILED" }),
        "/api/marketplace": emptyMarketplace,
      }) as unknown as typeof fetch,
    );

    render(<CatalogScreen />);
    fireEvent.click(await screen.findByTestId("catalog-uninstall-memory"));
    fireEvent.click(await screen.findByTestId("catalog-uninstall-confirm-memory"));

    await waitFor(() => {
      expect(screen.queryByTestId("catalog-uninstall-error-memory")).toBeTruthy();
    });
    expect(screen.getByTestId("catalog-uninstall-error-memory").textContent).toContain(
      "load manifest memory: not found",
    );
    // Row survives: still installed, Uninstall affordance still present.
    expect(screen.queryByTestId("catalog-installed-memory")).toBeTruthy();
    expect(screen.queryByTestId("catalog-uninstall-memory")).toBeTruthy();
  });

  // ---- §10 v2b: read-only marketplace browse ----

  it("renders the marketplace section with entries from /api/marketplace", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/catalog": () => jsonResponse(200, { catalog: [entry("memory")] }),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/marketplace": () =>
          jsonResponse(200, {
            entries: [
              mpEntry(
                "filesystem",
                "Filesystem",
                "Read/write files within allowed roots.",
                ["files", "core"],
                "https://example.com/fs",
              ),
              mpEntry("git", "Git", "Git repository tooling."),
            ],
          }),
      }) as unknown as typeof fetch,
    );

    render(<CatalogScreen />);
    await waitFor(() => {
      expect(screen.queryByTestId("catalog-marketplace-cards")).toBeTruthy();
    });
    // Summary renders; the Generate hint points at the CLI flow; no install.
    expect(screen.getByTestId("catalog-marketplace-summary-filesystem").textContent).toContain(
      "Read/write files within allowed roots.",
    );
    expect(screen.getByTestId("catalog-marketplace-generate-filesystem").textContent).toContain(
      "mcphub marketplace generate filesystem",
    );
    // Homepage link carries the safe external-link attrs.
    const homepage = screen.getByTestId(
      "catalog-marketplace-homepage-filesystem",
    ) as HTMLAnchorElement;
    expect(homepage.getAttribute("href")).toBe("https://example.com/fs");
    expect(homepage.getAttribute("rel")).toBe("noopener noreferrer");
    expect(homepage.getAttribute("target")).toBe("_blank");
    // A second entry renders too.
    expect(screen.queryByTestId("catalog-marketplace-card-git")).toBeTruthy();
    // There is NO install button on a marketplace card (read-only).
    expect(screen.queryByTestId("catalog-install-filesystem")).toBeNull();
  });

  it("renders the marketplace empty notice when /api/marketplace is empty", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/catalog": () => jsonResponse(200, { catalog: [entry("memory")] }),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/marketplace": emptyMarketplace,
      }) as unknown as typeof fetch,
    );

    render(<CatalogScreen />);
    await waitFor(() => {
      expect(screen.queryByTestId("catalog-marketplace-empty")).toBeTruthy();
    });
    expect(screen.queryByTestId("catalog-marketplace-cards")).toBeNull();
  });
});
