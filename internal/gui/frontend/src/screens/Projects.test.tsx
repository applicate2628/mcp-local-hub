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
    expect(canonicalProjectKey("C:\\dev\\proj\\")).toBe("C:/dev/proj");
  });
  it("collapses . and .. segments (filepath-clean)", () => {
    // filepath.Clean semantics: `b/..` pops `b`, `.` drops out.
    expect(canonicalProjectKey("/a/b/../c/./d")).toBe("/a/c/d");
    expect(canonicalProjectKey("a//b///c")).toBe("a/b/c");
  });
  it("PRESERVES case (no browser-OS case-fold)", () => {
    // navigator.platform reflects the BROWSER's OS, not the hub's; a POSIX hub
    // opened from a Windows browser must NOT collapse case-sensitive POSIX
    // paths. The P1 path↔path join is same-source/same-case, so a
    // case-sensitive compare is correct and case is preserved everywhere.
    expect(canonicalProjectKey("C:/Dev/Proj")).toBe("C:/Dev/Proj");
    expect(canonicalProjectKey("/Dev/Proj")).toBe("/Dev/Proj");
    // Two POSIX paths differing only in case stay DISTINCT keys (the bug the
    // browser-OS fold would have introduced).
    expect(canonicalProjectKey("/home/A/Repo")).not.toBe(
      canonicalProjectKey("/home/a/repo"),
    );
  });
  it("returns empty for a blank path", () => {
    expect(canonicalProjectKey("   ")).toBe("");
  });
  it("preserves the double leading slash of a UNC path", () => {
    // \\server\share\proj → //server/share/proj — the host must NOT collapse
    // into a path segment (a single-slash collapse would corrupt the key).
    expect(canonicalProjectKey("\\\\server\\share\\proj")).toBe(
      "//server/share/proj",
    );
    // ..-resolution still works under the UNC root and the // is kept.
    expect(canonicalProjectKey("//server/share/a/../b")).toBe(
      "//server/share/b",
    );
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

    // The two C:/dev/proj entries (one with a trailing slash) collapse via the
    // trailing-slash strip into ONE card regardless of host OS — case is
    // preserved (no browser-OS fold) but the paths are byte-identical after
    // normalization. The second distinct path → a second card.
    const cards = screen.getAllByTestId(/^projects-row-/);
    expect(cards.length).toBe(2);

    // The proj card's summary reflects 2 workspace tool registrations + 1 group.
    const projCard = cards.find((c) => c.textContent?.includes("C:/dev/proj"));
    expect(projCard).toBeTruthy();
    const summary = projCard!.querySelector('[data-testid^="projects-summary-"]');
    expect(summary?.textContent).toContain("2 workspace tools");
    expect(summary?.textContent).toContain("1 group");
    // Model B (project-config) is not wired in P1 → always the — em-dash.
    expect(summary?.textContent).toContain("project-config");
  });

  it("renders the list with an UNKNOWN group indicator (not '0 groups') when /api/groups fails", async () => {
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

    // The groups-load failure is surfaced (not silently dropped) AND the
    // per-card group count reads "? groups (load failed)", NOT a misleading
    // "0 groups" indistinguishable from a real-empty groups.yaml.
    const grErr = screen.getByTestId("projects-groups-error");
    expect(grErr.textContent).toContain("Could not load groups");
    const cards = screen.getAllByTestId(/^projects-row-/);
    const summary = cards[0].querySelector('[data-testid^="projects-summary-"]');
    expect(summary?.textContent).toContain("? groups (load failed)");
    expect(summary?.textContent).not.toContain("0 group");
  });

  it("shows an UNAVAILABLE list-state (not the empty-state) when /api/workspaces fails but /api/groups succeeds", async () => {
    // The workspace registry failed → `projects` is [] for a NON-empty reason.
    // The list must NOT show "No projects yet" (which would falsely claim a
    // clean install and send the operator to re-register existing projects);
    // it must render the explicit workspaces-load-failure state + Retry.
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/workspaces": () => jsonResponse(500, { error: "ws registry boom", code: "X" }),
        "/api/groups": () => jsonResponse(200, groupsBody([{ name: "g", servers: [] }])),
      }) as unknown as typeof fetch,
    );

    render(<ProjectsScreen />);
    // Not a full both-failed error screen (groups loaded OK).
    await waitFor(() => expect(screen.queryByTestId("projects-loaded")).toBeTruthy());
    expect(screen.queryByTestId("projects-error")).toBeNull();

    // The empty-state must NOT be shown — instead the unavailable state with the
    // workspace-registry error.
    expect(screen.queryByTestId("projects-empty")).toBeNull();
    const unavailable = screen.getByTestId("projects-workspaces-error");
    expect(unavailable.textContent).toContain("Projects unavailable");
    expect(unavailable.textContent).toContain("ws registry boom");
    expect(unavailable.textContent).not.toContain("No projects yet");
    // Retry is offered.
    expect(screen.getByText("Retry")).toBeTruthy();
  });

  it("shows the full error+Retry with BOTH errors only when BOTH endpoints fail", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/workspaces": () => jsonResponse(500, { error: "ws boom", code: "X" }),
        "/api/groups": () => jsonResponse(500, { error: "gr boom", code: "Y" }),
      }) as unknown as typeof fetch,
    );

    render(<ProjectsScreen />);
    await waitFor(() => expect(screen.queryByTestId("projects-load-error")).toBeTruthy());
    expect(screen.getByText("Retry")).toBeTruthy();
    // BOTH the workspace error AND the groups error are surfaced — the groups
    // error must not be dropped from the both-failed block.
    expect(screen.getByTestId("projects-load-error-workspaces").textContent).toContain(
      "ws boom",
    );
    expect(screen.getByTestId("projects-load-error-groups").textContent).toContain(
      "gr boom",
    );
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
    const key = canonicalProjectKey("/home/x/proj");
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

  it("renders legacy-lsp as the red 'legacy' chip (not green via-hub) and an unknown backend as no chip", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/workspaces": () =>
          jsonResponse(
            200,
            workspacesBody([
              // The deprecated direct-LSP backend must read as the OLD path.
              wsEntry({
                workspace_path: "/home/x/legacyproj",
                language: "go",
                backend: "legacy-lsp",
                client_entries: { "claude-code": "legacy-lsp-go" },
              }),
              // An unknown non-empty backend must NOT be fabricated into a
              // green via-hub chip — it renders no chip ("—").
              wsEntry({
                workspace_path: "/home/x/legacyproj",
                language: "ruby",
                backend: "some-future-backend",
                client_entries: { "claude-code": "x" },
              }),
            ]),
          ),
        "/api/groups": () => jsonResponse(200, groupsBody([])),
      }) as unknown as typeof fetch,
    );

    const key = canonicalProjectKey("/home/x/legacyproj");
    render(<ProjectsScreen route={routeWithPath(key)} />);
    await waitFor(() => expect(screen.queryByTestId("projects-detail")).toBeTruthy());

    // legacy-lsp → the red lsp-chip-legacy class with the "legacy" label.
    const legacyChip = screen.getByTestId("projects-chip-go-claude-code");
    expect(legacyChip.className).toContain("lsp-chip-legacy");
    expect(legacyChip.className).not.toContain("lsp-chip-via-hub");
    expect(legacyChip.textContent).toContain("legacy");

    // unknown backend → no chip at all (the routing cell shows the "—" empty).
    expect(screen.queryByTestId("projects-chip-ruby-claude-code")).toBeNull();
    const rubyRow = screen.getByTestId("projects-workspace-row-ruby");
    expect(rubyRow.querySelector(".lsp-cell-empty")?.textContent).toBe("—");
  });

  it("renders 'failed'/'missing' lifecycle as a DOWN state (state-down ✕), not OK/neutral", async () => {
    // The registry lifecycle words "failed" (materialization error) and
    // "missing" (LSP binary not on PATH) are real failures — they must read as
    // the error cross, never the benign open circle. last_error is surfaced as
    // the cell tooltip.
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/workspaces": () =>
          jsonResponse(
            200,
            workspacesBody([
              wsEntry({
                workspace_path: "/home/x/brokenproj",
                language: "go",
                backend: "mcp-language-server",
                lifecycle: "failed",
                last_error: "gopls exited 1",
              }),
              wsEntry({
                workspace_path: "/home/x/brokenproj",
                language: "python",
                backend: "mcp-language-server",
                lifecycle: "missing",
              }),
              // A genuinely-healthy active row stays state-ok for contrast.
              wsEntry({
                workspace_path: "/home/x/brokenproj",
                language: "rust",
                backend: "mcp-language-server",
                lifecycle: "active",
              }),
            ]),
          ),
        "/api/groups": () => jsonResponse(200, groupsBody([])),
      }) as unknown as typeof fetch,
    );

    const key = canonicalProjectKey("/home/x/brokenproj");
    render(<ProjectsScreen route={routeWithPath(key)} />);
    await waitFor(() => expect(screen.queryByTestId("projects-detail")).toBeTruthy());

    // lifecycle:failed → state-down (NOT state-ok / neutral) + Failed label +
    // last_error tooltip.
    const failedCell = screen.getByTestId("projects-workspace-state-go");
    expect(failedCell.className).toContain("state-down");
    expect(failedCell.className).not.toContain("state-ok");
    expect(failedCell.textContent).toContain("Failed");
    expect(failedCell.getAttribute("title")).toBe("gopls exited 1");

    // lifecycle:missing → also state-down (real failure, not benign idle).
    const missingCell = screen.getByTestId("projects-workspace-state-python");
    expect(missingCell.className).toContain("state-down");
    expect(missingCell.className).not.toContain("state-ok");
    expect(missingCell.textContent).toContain("Failed");

    // lifecycle:active → state-ok (contrast: a healthy row is NOT down).
    const activeCell = screen.getByTestId("projects-workspace-state-rust");
    expect(activeCell.className).toContain("state-ok");
    expect(activeCell.className).not.toContain("state-down");
    expect(activeCell.textContent).toContain("Running");
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
