// screens/Projects.test.tsx — component tests for the per-project lens
// (epic area 6 Phase 1). Mirrors Groups.test.tsx's fetch-router idiom: a
// declarative URL→response map so each test describes the wire surface
// without brittle mockResolvedValueOnce ordering. P1 is READ-ONLY, so these
// tests cover the COMPOSITION of the two existing endpoints (/api/workspaces +
// /api/groups) and the operator-visible read behaviors:
//   - canonicalProjectKey pure-fn normalization (the join key, single owner);
//   - LIST render: one card per unique canonical project path + per-mechanism
//     summary counts;
//   - the empty-state on a clean install;
//   - the DETAIL lens (#/projects?path=<key>) 3 sections;
//   - per-endpoint catch-isolation (one failing fetch ≠ whole screen blank).
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, waitFor, cleanup, screen } from "@testing-library/preact";
import { ProjectsScreen, canonicalProjectKey } from "./Projects";
import type { RouterState } from "../hooks/useRouter";

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

// fetchRouter dispatches each fetch call to the matching response based on the
// request URL prefix (same helper shape as Groups.test.tsx).
function fetchRouter(routes: Record<string, () => Response>) {
  return vi.fn(async (input: RequestInfo | URL) => {
    const url = typeof input === "string" ? input : input.toString();
    const keys = Object.keys(routes).sort((a, b) => b.length - a.length);
    for (const prefix of keys) {
      if (url.startsWith(prefix)) return routes[prefix]();
    }
    throw new Error(`unexpected fetch: ${url}`);
  });
}

// wsEntry builds one workspace entry DTO with sensible defaults.
function wsEntry(over: Partial<Record<string, unknown>> = {}) {
  return {
    workspace_key: "k",
    workspace_path: "C:/dev/proj",
    language: "go",
    backend: "mcp-language-server",
    port: 0,
    task_name: "\\mcp-local-hub-go",
    client_entries: {},
    ...over,
  };
}

function workspacesBody(entries: unknown[]) {
  return { workspaces: [], entries };
}
function groupsBody(groups: unknown[]) {
  return { groups, available_servers: [] };
}

function routeWithPath(path: string): RouterState {
  return { screen: "projects", query: `path=${encodeURIComponent(path)}` };
}

describe("canonicalProjectKey", () => {
  it("forward-slash-normalizes backslashes and strips trailing slash", () => {
    expect(canonicalProjectKey("C:\\dev\\proj\\", false)).toBe("C:/dev/proj");
  });
  it("collapses . and .. segments (filepath-clean)", () => {
    // filepath.Clean semantics: `b/..` pops `b`, `.` drops out.
    expect(canonicalProjectKey("/a/b/../c/./d", false)).toBe("/a/c/d");
    expect(canonicalProjectKey("a//b///c", false)).toBe("a/b/c");
  });
  it("lowercases on Windows but preserves case on POSIX", () => {
    expect(canonicalProjectKey("C:/Dev/Proj", true)).toBe("c:/dev/proj");
    expect(canonicalProjectKey("/Dev/Proj", false)).toBe("/Dev/Proj");
  });
  it("returns empty for a blank path", () => {
    expect(canonicalProjectKey("   ", false)).toBe("");
  });
});

describe("ProjectsScreen — list view", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
    window.location.hash = "#/projects";
  });
  afterEach(() => cleanup());

  it("renders the empty-state when no projects are registered", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/workspaces": () => jsonResponse(200, workspacesBody([])),
        "/api/groups": () => jsonResponse(200, groupsBody([])),
      }) as unknown as typeof fetch,
    );

    render(<ProjectsScreen />);
    await waitFor(() => expect(screen.queryByTestId("projects-empty")).toBeTruthy());
    expect(screen.getByTestId("projects-empty").textContent).toContain("No projects yet");
    expect(screen.getByText("Projects")).toBeTruthy();
  });

  it("composes the two endpoints into one card per unique canonical path with per-mechanism counts", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        // Two entries on the SAME path (different casing + trailing slash) must
        // collapse into ONE project card; a second distinct path → second card.
        "/api/workspaces": () =>
          jsonResponse(
            200,
            workspacesBody([
              wsEntry({ workspace_path: "C:/dev/proj", language: "go" }),
              wsEntry({ workspace_path: "C:/dev/proj/", language: "python" }),
              wsEntry({ workspace_path: "C:/dev/other", language: "rust" }),
            ]),
          ),
        "/api/groups": () =>
          jsonResponse(200, groupsBody([{ name: "frontend", servers: ["serena"] }])),
      }) as unknown as typeof fetch,
    );

    render(<ProjectsScreen />);
    await waitFor(() => expect(screen.queryByTestId("projects-list")).toBeTruthy());

    // On Windows hosts the two C:/dev/proj entries collapse; on POSIX test
    // hosts the canonical key keeps case but the trailing-slash strip still
    // collapses them. Either way the proj card exists exactly once.
    const cards = screen.getAllByTestId(/^projects-row-/);
    expect(cards.length).toBe(2);

    // The proj card's summary reflects 2 workspace entries + 1 group.
    const projCard = cards.find((c) => c.textContent?.includes("C:/dev/proj"));
    expect(projCard).toBeTruthy();
    const summary = projCard!.querySelector('[data-testid^="projects-summary-"]');
    expect(summary?.textContent).toContain("2 workspaces");
    expect(summary?.textContent).toContain("1 group");
    // Model B (project-config) is not wired in P1 → always the — em-dash.
    expect(summary?.textContent).toContain("project-config");
  });

  it("renders the list even when /api/groups fails (per-endpoint isolation)", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/workspaces": () =>
          jsonResponse(200, workspacesBody([wsEntry({ workspace_path: "/home/x/p" })])),
        "/api/groups": () => jsonResponse(500, { error: "boom", code: "GROUPS_LIST_FAILED" }),
      }) as unknown as typeof fetch,
    );

    render(<ProjectsScreen />);
    // The project card still renders despite the groups failure (not a full
    // error+Retry screen).
    await waitFor(() => expect(screen.queryByTestId("projects-list")).toBeTruthy());
    expect(screen.queryByTestId("projects-error")).toBeNull();
  });

  it("shows the full error+Retry only when BOTH endpoints fail", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/workspaces": () => jsonResponse(500, { error: "ws boom", code: "X" }),
        "/api/groups": () => jsonResponse(500, { error: "gr boom", code: "Y" }),
      }) as unknown as typeof fetch,
    );

    render(<ProjectsScreen />);
    await waitFor(() => expect(screen.queryByTestId("projects-load-error")).toBeTruthy());
    expect(screen.getByText("Retry")).toBeTruthy();
  });
});

describe("ProjectsScreen — detail lens", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });
  afterEach(() => cleanup());

  it("renders the 3 labelled sections for the selected project path", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/workspaces": () =>
          jsonResponse(
            200,
            workspacesBody([
              wsEntry({
                workspace_path: "/home/x/proj",
                language: "go",
                backend: "mcp-language-server",
                client_entries: { "claude-code": "mcp-language-server-go" },
                lifecycle: "active",
              }),
            ]),
          ),
        "/api/groups": () =>
          jsonResponse(200, groupsBody([{ name: "g1", servers: ["serena"], tools_hidden: { serena: ["x"] } }])),
      }) as unknown as typeof fetch,
    );

    // POSIX canonical key for "/home/x/proj" (case preserved).
    const key = canonicalProjectKey("/home/x/proj", false);
    render(<ProjectsScreen route={routeWithPath(key)} />);

    await waitFor(() => expect(screen.queryByTestId("projects-detail")).toBeTruthy());

    // All three sections present with their provenance badges.
    expect(screen.getByTestId("projects-section-workspace")).toBeTruthy();
    expect(screen.getByTestId("projects-section-config")).toBeTruthy();
    expect(screen.getByTestId("projects-section-groups")).toBeTruthy();
    expect(screen.getByTestId("projects-badge-workspace").textContent).toContain("workspaces.yaml");
    expect(screen.getByTestId("projects-badge-config").textContent).toContain(".mcp.json");
    expect(screen.getByTestId("projects-badge-groups").textContent).toContain("groups.yaml");

    // [A] the go workspace row renders with a per-client via-hub chip + state.
    expect(screen.getByTestId("projects-workspace-row-go")).toBeTruthy();
    expect(screen.getByTestId("projects-chip-go-claude-code").textContent).toContain("via-hub");
    expect(screen.getByTestId("projects-workspace-state-go").textContent).toContain("Running");

    // [B] the not-yet-wired placeholder copy.
    expect(screen.getByTestId("projects-config-placeholder").textContent).toContain(
      "Not managed here yet",
    );

    // [C] the group is listed read-only + the security-fence note (tools_hidden).
    expect(screen.getByTestId("projects-group-g1")).toBeTruthy();
    expect(screen.getByTestId("projects-group-hidden-note-g1").textContent).toContain(
      "not",
    );
    // The "manage in Groups →" cross-link.
    expect(screen.getByTestId("projects-manage-groups")).toBeTruthy();
  });

  it("shows the workspace empty-state when the selected path has no entries", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/workspaces": () => jsonResponse(200, workspacesBody([])),
        "/api/groups": () => jsonResponse(200, groupsBody([])),
      }) as unknown as typeof fetch,
    );

    render(<ProjectsScreen route={routeWithPath("/nonexistent")} />);
    await waitFor(() => expect(screen.queryByTestId("projects-detail")).toBeTruthy());
    expect(screen.getByTestId("projects-workspace-empty")).toBeTruthy();
    expect(screen.getByTestId("projects-groups-empty")).toBeTruthy();
  });
});
