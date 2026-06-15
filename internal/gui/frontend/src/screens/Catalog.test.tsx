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
// ({id, name, summary, categories, homepage, transport}). The Store
// install view renders these in the Marketplace section below the
// shipped-server store. `transport` ("stdio" | "http") drives the
// two-tier install affordance (stdio → hub only; http → hub + direct).
function mpEntry(
  id: string,
  name = id,
  summary = `summary for ${id}`,
  categories: string[] = [],
  homepage = "",
  transport = "stdio",
) {
  return { id, name, summary, categories, homepage, transport };
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
    // The description now lives in an on-demand InfoTip (ⓘ) next to the
    // card title — its prose is carried on the trigger's `title` attribute
    // (and revealed in the popover on click), not as inline body text. The
    // data-testid is preserved on the trigger so coverage stays anchored.
    expect(screen.getByTestId("catalog-desc-serena").getAttribute("title")).toContain(
      "Semantic code toolkit",
    );
    expect(screen.getByTestId("catalog-desc-memory").getAttribute("title")).toContain(
      "Persistent knowledge-graph memory",
    );
    // The prose is NOT dumped inline under the title anymore (compact card).
    expect(screen.getByTestId("catalog-desc-serena").textContent).not.toContain(
      "Semantic code toolkit",
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

  // ---- §B #1 Store: marketplace one-click install ----

  it("renders the marketplace section with one-click install entries from /api/marketplace", async () => {
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
                "stdio",
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
    // Summary now lives in an on-demand InfoTip (ⓘ) next to the entry name —
    // its prose is on the trigger's `title` attribute, not inline body text.
    // The one-click "Add to hub" action replaces the old read-only
    // Generate-CLI hint.
    expect(
      screen.getByTestId("catalog-marketplace-summary-filesystem").getAttribute("title"),
    ).toContain("Read/write files within allowed roots.");
    expect(screen.getByTestId("catalog-marketplace-hub-filesystem").textContent).toContain(
      "Add to hub",
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
  });

  it("a marketplace entry already running per /api/status shows Installed, no install affordance", async () => {
    // `fetch` is a shipped hub daemon AND a marketplace catalog entry. When
    // /api/status reports it running, the marketplace card must render an
    // "Installed" badge instead of an install button — otherwise clicking
    // install hits NAME_CONFLICT and absurdly offers to install "fetch-2".
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/catalog": () => jsonResponse(200, { catalog: [entry("memory")] }),
        // fetch is running; not-installed is not.
        "/api/status": () =>
          jsonResponse(200, [
            { server: "fetch", daemon: "default", port: 9131, state: "Running" } as DaemonStatus,
          ]),
        "/api/marketplace": () =>
          jsonResponse(200, {
            entries: [
              mpEntry("fetch", "Fetch", "Fetch a URL and convert to markdown.", [], "", "stdio"),
              mpEntry("not-installed", "Not Installed", "Some other server.", [], "", "stdio"),
            ],
          }),
      }) as unknown as typeof fetch,
    );

    render(<CatalogScreen />);
    await waitFor(() => {
      expect(screen.queryByTestId("catalog-marketplace-cards")).toBeTruthy();
    });
    // fetch → Installed badge, NO install affordance (no "Add to hub" button,
    // no direct toggle).
    expect(screen.getByTestId("catalog-marketplace-installed-badge-fetch").textContent).toBe(
      "installed",
    );
    expect(screen.queryByTestId("catalog-marketplace-hub-fetch")).toBeNull();
    expect(screen.queryByTestId("catalog-marketplace-install-fetch")).toBeNull();
    expect(screen.queryByTestId("catalog-marketplace-direct-toggle-fetch")).toBeNull();
    // not-installed → install affordance present, NO Installed badge.
    expect(screen.queryByTestId("catalog-marketplace-installed-badge-not-installed")).toBeNull();
    expect(screen.getByTestId("catalog-marketplace-hub-not-installed").textContent).toContain(
      "Add to hub",
    );
  });

  it("a stdio entry shows HUB-ONLY (no Install-directly toggle)", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/catalog": () => jsonResponse(200, { catalog: [entry("memory")] }),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/marketplace": () =>
          jsonResponse(200, { entries: [mpEntry("git", "Git", "x", [], "", "stdio")] }),
      }) as unknown as typeof fetch,
    );

    render(<CatalogScreen />);
    await waitFor(() => {
      expect(screen.queryByTestId("catalog-marketplace-hub-git")).toBeTruthy();
    });
    // Hub action present; direct-mode toggle ABSENT for a stdio entry (the
    // two-tier rule — stdio installs only via the shared hub daemon).
    expect(screen.getByTestId("catalog-marketplace-hub-git").textContent).toContain("Add to hub");
    expect(screen.queryByTestId("catalog-marketplace-direct-toggle-git")).toBeNull();
  });

  it("an http entry shows BOTH hub + direct modes", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/catalog": () => jsonResponse(200, { catalog: [entry("memory")] }),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/marketplace": () =>
          jsonResponse(200, { entries: [mpEntry("remote", "Remote", "x", [], "", "http")] }),
      }) as unknown as typeof fetch,
    );

    render(<CatalogScreen />);
    await waitFor(() => {
      expect(screen.queryByTestId("catalog-marketplace-hub-remote")).toBeTruthy();
    });
    // Both modes are offered for an http entry.
    expect(screen.queryByTestId("catalog-marketplace-hub-remote")).toBeTruthy();
    expect(screen.queryByTestId("catalog-marketplace-direct-toggle-remote")).toBeTruthy();
    // The client multiselect is collapsed until the toggle is clicked.
    expect(screen.queryByTestId("catalog-marketplace-direct-panel-remote")).toBeNull();
  });

  it("direct toggle aria-controls only references the panel when expanded (no dangling ref)", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/catalog": () => jsonResponse(200, { catalog: [entry("memory")] }),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/marketplace": () =>
          jsonResponse(200, { entries: [mpEntry("remote", "Remote", "x", [], "", "http")] }),
      }) as unknown as typeof fetch,
    );

    render(<CatalogScreen />);
    const toggle = await screen.findByTestId("catalog-marketplace-direct-toggle-remote");
    // Collapsed: aria-expanded=false AND no aria-controls (the panel is not in the
    // DOM, so a static aria-controls would dangle — review fix).
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(toggle.getAttribute("aria-controls")).toBeNull();
    // Expanded: aria-expanded=true AND aria-controls resolves to the rendered panel.
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    const panelId = toggle.getAttribute("aria-controls");
    expect(panelId).toBe("catalog-marketplace-direct-panel-remote");
    expect(document.getElementById(panelId!)).toBeTruthy();
  });

  it("hub install POSTs {id, mode:'hub'} and reflects the 201 success", async () => {
    const bodies: unknown[] = [];
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/catalog": () => jsonResponse(200, { catalog: [entry("memory")] }),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/marketplace/install": (init) => {
          bodies.push(JSON.parse(String(init?.body)));
          return jsonResponse(201, { name: "git", port: 9201, mode: "hub" });
        },
        "/api/marketplace": () =>
          jsonResponse(200, { entries: [mpEntry("git", "Git", "x", [], "", "stdio")] }),
      }) as unknown as typeof fetch,
    );

    render(<CatalogScreen />);
    const hubBtn = await screen.findByTestId("catalog-marketplace-hub-git");
    fireEvent.click(hubBtn);

    await waitFor(() => {
      expect(screen.queryByTestId("catalog-marketplace-installed-git")).toBeTruthy();
    });
    expect(screen.getByTestId("catalog-marketplace-installed-git").textContent).toContain("git");
    expect(screen.getByTestId("catalog-marketplace-installed-git").textContent).toContain("9201");
    // The POST body carried the id + hub mode.
    expect(bodies[0]).toMatchObject({ id: "git", mode: "hub" });
  });

  it("hub install 409 NAME_CONFLICT offers a one-click suggested-name retry", async () => {
    const bodies: unknown[] = [];
    let installCall = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/catalog": () => jsonResponse(200, { catalog: [entry("memory")] }),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/marketplace/install": (init) => {
          bodies.push(JSON.parse(String(init?.body)));
          installCall += 1;
          // First attempt: name conflict. Retry under the suggested name: 201.
          return installCall === 1
            ? jsonResponse(409, { error_code: "NAME_CONFLICT", suggested_name: "git-2" })
            : jsonResponse(201, { name: "git-2", port: 9202, mode: "hub" });
        },
        "/api/marketplace": () =>
          jsonResponse(200, { entries: [mpEntry("git", "Git", "x", [], "", "stdio")] }),
      }) as unknown as typeof fetch,
    );

    render(<CatalogScreen />);
    fireEvent.click(await screen.findByTestId("catalog-marketplace-hub-git"));

    // The conflict surfaces the suggested name + a one-click retry button.
    const retry = await screen.findByTestId("catalog-marketplace-conflict-retry-git");
    expect(retry.textContent).toContain("git-2");
    fireEvent.click(retry);

    // Retry under the suggested name succeeds.
    await waitFor(() => {
      expect(screen.queryByTestId("catalog-marketplace-installed-git")).toBeTruthy();
    });
    expect(screen.getByTestId("catalog-marketplace-installed-git").textContent).toContain("git-2");
    // Second POST carried name=git-2 (the suggested-name retry shape).
    expect(bodies[1]).toMatchObject({ id: "git", mode: "hub", name: "git-2" });
  });

  it("direct mode: client multiselect + POST {mode:'direct', clients:[…]} shape", async () => {
    const bodies: unknown[] = [];
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/catalog": () => jsonResponse(200, { catalog: [entry("memory")] }),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/marketplace/install": (init) => {
          bodies.push(JSON.parse(String(init?.body)));
          return jsonResponse(200, {
            clients_updated: ["claude-code", "cursor"],
            clients_failed: [],
            mode: "direct",
          });
        },
        "/api/marketplace": () =>
          jsonResponse(200, { entries: [mpEntry("remote", "Remote", "x", [], "", "http")] }),
      }) as unknown as typeof fetch,
    );

    render(<CatalogScreen />);
    // Open the direct-mode panel.
    fireEvent.click(await screen.findByTestId("catalog-marketplace-direct-toggle-remote"));
    const panel = await screen.findByTestId("catalog-marketplace-direct-panel-remote");
    expect(panel).toBeTruthy();

    // The Install button is disabled until at least one client is picked.
    const installBtn = screen.getByTestId(
      "catalog-marketplace-direct-install-remote",
    ) as HTMLButtonElement;
    expect(installBtn.disabled).toBe(true);

    // Pick two clients, then install.
    fireEvent.click(screen.getByTestId("catalog-marketplace-client-remote-claude-code"));
    fireEvent.click(screen.getByTestId("catalog-marketplace-client-remote-cursor"));
    await waitFor(() => {
      expect((screen.getByTestId("catalog-marketplace-direct-install-remote") as HTMLButtonElement).disabled).toBe(false);
    });
    fireEvent.click(screen.getByTestId("catalog-marketplace-direct-install-remote"));

    // The result lists the updated clients.
    await waitFor(() => {
      expect(screen.queryByTestId("catalog-marketplace-direct-result-remote")).toBeTruthy();
    });
    expect(screen.getByTestId("catalog-marketplace-direct-updated-remote").textContent).toContain(
      "claude-code",
    );
    // The POST carried mode:direct + the selected client list.
    expect(bodies[0]).toMatchObject({ id: "remote", mode: "direct" });
    const sent = bodies[0] as { clients?: string[] };
    expect(sent.clients).toEqual(expect.arrayContaining(["claude-code", "cursor"]));
  });

  it("direct mode 207 partial renders both updated + failed clients", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/catalog": () => jsonResponse(200, { catalog: [entry("memory")] }),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/marketplace/install": () =>
          jsonResponse(207, {
            clients_updated: ["claude-code"],
            clients_failed: [{ client: "vscode", error: "config file is a symlink" }],
            mode: "direct",
          }),
        "/api/marketplace": () =>
          jsonResponse(200, { entries: [mpEntry("remote", "Remote", "x", [], "", "http")] }),
      }) as unknown as typeof fetch,
    );

    render(<CatalogScreen />);
    fireEvent.click(await screen.findByTestId("catalog-marketplace-direct-toggle-remote"));
    fireEvent.click(await screen.findByTestId("catalog-marketplace-client-remote-claude-code"));
    fireEvent.click(screen.getByTestId("catalog-marketplace-direct-install-remote"));

    await waitFor(() => {
      expect(screen.queryByTestId("catalog-marketplace-direct-failed-remote")).toBeTruthy();
    });
    expect(screen.getByTestId("catalog-marketplace-direct-updated-remote").textContent).toContain(
      "claude-code",
    );
    expect(screen.getByTestId("catalog-marketplace-direct-failed-remote").textContent).toContain(
      "vscode",
    );
    expect(screen.getByTestId("catalog-marketplace-direct-failed-remote").textContent).toContain(
      "config file is a symlink",
    );
  });

  it("surfaces an inline error when the install POST fails (e.g. 502 catalog unavailable)", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/catalog": () => jsonResponse(200, { catalog: [entry("memory")] }),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/marketplace/install": () =>
          jsonResponse(502, { error: "marketplace catalog unavailable", code: "CATALOG_UNAVAILABLE" }),
        "/api/marketplace": () =>
          jsonResponse(200, { entries: [mpEntry("git", "Git", "x", [], "", "stdio")] }),
      }) as unknown as typeof fetch,
    );

    render(<CatalogScreen />);
    fireEvent.click(await screen.findByTestId("catalog-marketplace-hub-git"));

    await waitFor(() => {
      expect(screen.queryByTestId("catalog-marketplace-error-git")).toBeTruthy();
    });
    expect(screen.getByTestId("catalog-marketplace-error-git").textContent).toContain(
      "marketplace catalog unavailable",
    );
    // The row survives: the hub button is present + re-enabled for a retry.
    const hubBtn = screen.getByTestId("catalog-marketplace-hub-git") as HTMLButtonElement;
    expect(hubBtn.disabled).toBe(false);
  });

  it("does NOT render a homepage link when the marketplace homepage is not http(s) (untrusted-registry XSS guard)", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/catalog": () => jsonResponse(200, { catalog: [entry("memory")] }),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/marketplace": () =>
          jsonResponse(200, {
            entries: [mpEntry("evil", "Evil", "x", [], "javascript:alert(1)")],
          }),
      }) as unknown as typeof fetch,
    );

    render(<CatalogScreen />);
    await waitFor(() => {
      expect(screen.queryByTestId("catalog-marketplace-card-evil")).toBeTruthy();
    });
    // The hostile non-http(s) homepage must NOT produce a rendered link.
    expect(screen.queryByTestId("catalog-marketplace-homepage-evil")).toBeNull();
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
