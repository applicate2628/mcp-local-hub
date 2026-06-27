// screens/Projects.test.tsx — component tests for the per-project lens.
//
// PHASE 3b switched the data source to the SINGLE GET /api/projects aggregate
// (design decision 6) and wired the IMMEDIATE per-row toggle. These tests cover
// the new behaviors:
//   - canonicalProjectKey pure-fn normalization (the join key, single owner);
//   - LIST render from the aggregate (one card per project + summary counts);
//   - empty-state on a clean install; hard-fail error+Retry on aggregate failure;
//   - DETAIL lens (#/projects?path=<key>) 4 mechanism sections;
//   - the per-row toggle state machine (optimistic flip → reconcile-to-response,
//     error revert + §3.1 plain copy, per-row isolation);
//   - both-scopes claude card (Project toggle / Local read-only / shadow once);
//   - warm/cold object-member re-enable (warm replays held value; cold → Re-add).
//
// Mirrors Servers.test.tsx's fetch-router idiom (a declarative URL→response map)
// + happy-dom EventSource stub conventions.
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, waitFor, cleanup, screen, fireEvent } from "@testing-library/preact";
import { ProjectsScreen, canonicalProjectKey } from "./Projects";
import type { RouterState } from "../hooks/useRouter";

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

// fetchRouter dispatches each fetch call (method-aware) to the matching response
// based on URL prefix. Returns the vi.fn so a test can assert call args.
function fetchRouter(
  routes: Record<string, (init?: RequestInit) => Response | Promise<Response>>,
) {
  return vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input.toString();
    const keys = Object.keys(routes).sort((a, b) => b.length - a.length);
    for (const prefix of keys) {
      if (url.startsWith(prefix)) return routes[prefix](init);
    }
    throw new Error(`unexpected fetch: ${url}`);
  });
}

function routeWithPath(path: string): RouterState {
  return { screen: "projects", query: `path=${encodeURIComponent(path)}` };
}

// aggregate builds a /api/projects response body.
function aggregate(over: {
  projects?: unknown[];
  groups?: unknown[];
  groups_error?: string;
} = {}) {
  return {
    projects: over.projects ?? [],
    groups: over.groups ?? [],
    ...(over.groups_error ? { groups_error: over.groups_error } : {}),
  };
}

// proj builds one project aggregate DTO.
function proj(over: Partial<Record<string, unknown>> = {}) {
  return {
    key: "/home/x/proj",
    workspace_path: "/home/x/proj",
    entries: [],
    ...over,
  };
}

// scanEntry builds one ScanEntry (claude/cursor/vscode object member).
function scanEntry(name: string, presence: Record<string, unknown>, over: Record<string, unknown> = {}) {
  return { name, client_presence: presence, manifest_exists: true, can_migrate: true, ...over };
}

describe("canonicalProjectKey", () => {
  it("forward-slash-normalizes backslashes and strips trailing slash", () => {
    expect(canonicalProjectKey("C:\\dev\\proj\\")).toBe("C:/dev/proj");
  });
  it("collapses . and .. segments (filepath-clean)", () => {
    expect(canonicalProjectKey("/a/b/../c/./d")).toBe("/a/c/d");
    expect(canonicalProjectKey("a//b///c")).toBe("a/b/c");
  });
  it("PRESERVES case (no browser-OS case-fold)", () => {
    expect(canonicalProjectKey("C:/Dev/Proj")).toBe("C:/Dev/Proj");
    expect(canonicalProjectKey("/home/A/Repo")).not.toBe(canonicalProjectKey("/home/a/repo"));
  });
  it("preserves the double leading slash of a UNC path", () => {
    expect(canonicalProjectKey("\\\\server\\share\\proj")).toBe("//server/share/proj");
    expect(canonicalProjectKey("//server/share/a/../b")).toBe("//server/share/b");
  });
});

describe("ProjectsScreen — list view (aggregate)", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
    window.location.hash = "#/projects";
  });
  afterEach(() => cleanup());

  it("renders the empty-state when the aggregate has no projects", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({ "/api/projects": () => jsonResponse(200, aggregate()) }) as unknown as typeof fetch,
    );
    render(<ProjectsScreen />);
    await waitFor(() => expect(screen.queryByTestId("projects-empty")).toBeTruthy());
    expect(screen.getByTestId("projects-empty").textContent).toContain("No projects yet");
  });

  it("renders one card per aggregate project with summary counts", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/projects": () =>
          jsonResponse(
            200,
            aggregate({
              projects: [
                proj({
                  key: "/home/x/proj",
                  entries: [{ workspace_key: "k", workspace_path: "/home/x/proj", language: "go", backend: "mcp-language-server", port: 0, task_name: "t" }],
                  scan: { at: "now", entries: [scanEntry("memory", { cursor: {} })] },
                }),
                proj({ key: "/home/x/other", workspace_path: "/home/x/other" }),
              ],
              groups: [{ name: "frontend", servers: ["serena"] }],
            }),
          ),
      }) as unknown as typeof fetch,
    );
    render(<ProjectsScreen />);
    await waitFor(() => expect(screen.queryByTestId("projects-list")).toBeTruthy());
    const cards = screen.getAllByTestId(/^projects-row-/);
    expect(cards.length).toBe(2);
    const projCard = cards.find((c) => c.textContent?.includes("/home/x/proj"));
    const summary = projCard!.querySelector('[data-testid^="projects-summary-"]');
    expect(summary?.textContent).toContain("1 workspace tool");
    expect(summary?.textContent).toContain("1 config server");
    expect(summary?.textContent).toContain("1 group");
  });

  it("surfaces a hard-fail error+Retry when the aggregate fetch fails", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/projects": () => jsonResponse(500, { error: "boom", code: "PROJECTS_REGISTRY_FAILED" }),
      }) as unknown as typeof fetch,
    );
    render(<ProjectsScreen />);
    await waitFor(() => expect(screen.queryByTestId("projects-load-error")).toBeTruthy());
    expect(screen.getByTestId("projects-load-error").textContent).toContain("boom");
    expect(screen.getByText("Retry")).toBeTruthy();
  });

  it("shows '? groups (load failed)' when groups_error is set but projects render", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/projects": () =>
          jsonResponse(200, aggregate({ projects: [proj()], groups_error: "GROUPS_LIST_FAILED" })),
      }) as unknown as typeof fetch,
    );
    render(<ProjectsScreen />);
    await waitFor(() => expect(screen.queryByTestId("projects-list")).toBeTruthy());
    expect(screen.getByTestId("projects-groups-error").textContent).toContain("Could not load groups");
    const summary = screen.getAllByTestId(/^projects-summary-/)[0];
    expect(summary.textContent).toContain("? groups (load failed)");
  });
});

describe("ProjectsScreen — detail lens (sections + toggles)", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });
  afterEach(() => cleanup());

  function mountDetail(dto: Record<string, unknown>, groups: unknown[] = [], toggleImpl?: (init?: RequestInit) => Response | Promise<Response>) {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/projects/toggle": toggleImpl ?? (() => jsonResponse(200, { scope: "project-object-member", server: "x", enabled: false })),
        "/api/projects": () => jsonResponse(200, aggregate({ projects: [dto], groups })),
        "/api/server/readiness": () => jsonResponse(404, { error: "not found" }),
      }) as unknown as typeof fetch,
    );
  }

  it("renders the 3 mechanism sections + provenance badges", async () => {
    mountDetail(
      proj({
        key: "/home/x/proj",
        entries: [{ workspace_key: "k", workspace_path: "/home/x/proj", language: "go", backend: "mcp-language-server", port: 0, task_name: "t", lifecycle: "active", client_entries: { "claude-code": "x" } }],
        scan: { at: "now", entries: [] },
      }),
      [{ name: "g1", servers: ["serena"], tools_hidden: { serena: ["x"] } }],
    );
    render(<ProjectsScreen route={routeWithPath("/home/x/proj")} />);
    await waitFor(() => expect(screen.queryByTestId("projects-detail")).toBeTruthy());
    expect(screen.getByTestId("projects-section-workspace")).toBeTruthy();
    expect(screen.getByTestId("projects-section-config")).toBeTruthy();
    expect(screen.getByTestId("projects-section-groups")).toBeTruthy();
    expect(screen.getByTestId("projects-badge-config").textContent).toContain(".mcp.json");
    // workspace row + its toggle exist.
    expect(screen.getByTestId("projects-toggle-workspace-lsp-go")).toBeTruthy();
    // group member toggle.
    expect(screen.getByTestId("projects-toggle-group-servers-serena")).toBeTruthy();
    // tools_hidden security-fence note kept.
    expect(screen.getByTestId("projects-group-hidden-note-g1").textContent).toContain("not");
  });

  it("toggle reconciles to response.enabled, not the requested intent (clamp)", async () => {
    // The operator clicks ON, but the backend read-back clamps to OFF (e.g. an
    // idempotent self-correct). The row MUST reflect response.enabled (OFF).
    mountDetail(
      proj({
        key: "/home/x/proj",
        scan: { at: "now", entries: [scanEntry("memory", { cursor: { raw: { command: "x" } } })] },
      }),
      [],
      () => jsonResponse(200, { scope: "project-object-member", server: "memory", enabled: false }),
    );
    render(<ProjectsScreen route={routeWithPath("/home/x/proj")} />);
    await waitFor(() => expect(screen.queryByTestId("projects-client-cursor")).toBeTruthy());
    const toggle = screen.getByTestId("projects-toggle-project-object-member-memory") as HTMLInputElement;
    expect(toggle.checked).toBe(true); // initially present → enabled
    // Disable (held value captured). Backend echoes enabled:false → reconciles OFF.
    fireEvent.change(toggle, { target: { checked: false } });
    await waitFor(() =>
      expect(
        (screen.getByTestId("projects-toggle-project-object-member-memory") as HTMLInputElement).checked,
      ).toBe(false),
    );
    // ✓ flash appears after the successful reconcile.
    await waitFor(() =>
      expect(screen.queryByTestId("projects-toggle-ok-project-object-member-memory")).toBeTruthy(),
    );
  });

  it("toggle failure REVERTS the optimistic flip and shows the §3.1 plain copy + Retry", async () => {
    mountDetail(
      proj({
        key: "/home/x/proj",
        scan: { at: "now", entries: [scanEntry("memory", { cursor: { raw: { command: "x" } } })] },
      }),
      [],
      () => jsonResponse(500, { error: "disk full", code: "PROJECT_TOGGLE_FAILED" }),
    );
    render(<ProjectsScreen route={routeWithPath("/home/x/proj")} />);
    await waitFor(() => expect(screen.queryByTestId("projects-client-cursor")).toBeTruthy());
    const toggle = screen.getByTestId("projects-toggle-project-object-member-memory") as HTMLInputElement;
    fireEvent.change(toggle, { target: { checked: false } });
    // REVERT: the optimistic OFF flips back to ON (the pre-toggle state).
    await waitFor(() =>
      expect(
        (screen.getByTestId("projects-toggle-project-object-member-memory") as HTMLInputElement).checked,
      ).toBe(true),
    );
    const err = screen.getByTestId("projects-toggle-error-project-object-member-memory");
    // Plain copy, NOT the raw code on the visible row.
    expect(err.textContent).toContain("couldn't be saved");
    expect(err.textContent).not.toContain("PROJECT_TOGGLE_FAILED");
    // Retry offered.
    expect(screen.getByTestId("projects-toggle-retry-project-object-member-memory")).toBeTruthy();
  });

  it("both-scopes claude card: Project toggle + Local read-only + shadow rendered once", async () => {
    mountDetail(
      proj({
        key: "/home/x/proj",
        scan: {
          at: "now",
          entries: [
            // Non-shadowed approved claude .mcp.json entry → live toggle.
            scanEntry("approved", { "claude-code": { raw: { command: "y" } } }, { project_enabled: true }),
            // Shadowed entry → rendered ONCE Local-owned (muted ⊘ in Project).
            scanEntry("shadowed", { "claude-code": {} }, { project_shadowed_by_local: true }),
          ],
          project_scope: { local_servers: ["shadowed", "localonly"] },
        },
      }),
    );
    render(<ProjectsScreen route={routeWithPath("/home/x/proj")} />);
    await waitFor(() => expect(screen.queryByTestId("projects-client-claude-code")).toBeTruthy());
    // Project subsection: live toggle for the approved entry.
    expect(screen.getByTestId("projects-toggle-project-object-member-approved")).toBeTruthy();
    // Shadow: ONE muted anchor in Project + the authoritative cross-ref in Local.
    expect(screen.getByTestId("projects-shadow-shadowed")).toBeTruthy();
    expect(screen.getByTestId("projects-shadow-authoritative-shadowed")).toBeTruthy();
    // The shadowed entry has NO competing toggle in the Project subsection.
    expect(screen.queryByTestId("projects-toggle-project-object-member-shadowed")).toBeNull();
    // Local subsection lists both local servers read-only.
    expect(screen.getByTestId("projects-claude-local-localonly")).toBeTruthy();
    expect(screen.getByTestId("projects-claude-local-localonly").textContent).toContain("read-only");
  });

  it("warm re-enable replays the held value; cold re-enable shows the Re-add CTA", async () => {
    // Warm: a cursor member with a raw value in the scan. Disabling captures it;
    // re-enabling replays it as the POST value (asserted via the captured body).
    const captured: { body: Record<string, unknown> } = { body: {} };
    mountDetail(
      proj({
        key: "/home/x/proj",
        scan: { at: "now", entries: [scanEntry("warm", { cursor: { raw: { command: "held-cmd" } } })] },
      }),
      [],
      async (init) => {
        captured.body = JSON.parse((init?.body as string) ?? "{}");
        const enable = captured.body.enable === true;
        return jsonResponse(200, { scope: "project-object-member", server: "warm", enabled: enable });
      },
    );
    render(<ProjectsScreen route={routeWithPath("/home/x/proj")} />);
    await waitFor(() => expect(screen.queryByTestId("projects-client-cursor")).toBeTruthy());
    const toggle = screen.getByTestId("projects-toggle-project-object-member-warm") as HTMLInputElement;
    // Disable then re-enable — the enable POST must carry the held value.
    fireEvent.change(toggle, { target: { checked: false } });
    await waitFor(() => expect((screen.getByTestId("projects-toggle-project-object-member-warm") as HTMLInputElement).checked).toBe(false));
    fireEvent.change(screen.getByTestId("projects-toggle-project-object-member-warm"), { target: { checked: true } });
    await waitFor(() => expect(captured.body.enable).toBe(true));
    expect(captured.body.value).toEqual({ command: "held-cmd" });
  });

  it("cold object-member (no held value) renders the Re-add CTA, not a value-less enable toggle", async () => {
    // A claude .mcp.json entry that is DISABLED (project_enabled:false) and has no
    // raw value in the scan → cold: the toggle is off + we render a Re-add CTA so
    // an enable POST is never sent without a value (CORE RULING / D2).
    mountDetail(
      proj({
        key: "/home/x/proj",
        scan: {
          at: "now",
          entries: [scanEntry("cold", { "claude-code": {} }, { project_enabled: false })],
        },
      }),
    );
    render(<ProjectsScreen route={routeWithPath("/home/x/proj")} />);
    await waitFor(() => expect(screen.queryByTestId("projects-client-claude-code")).toBeTruthy());
    // No held value + off → Re-add CTA, no enable toggle for this row.
    expect(screen.getByTestId("projects-readd-cold")).toBeTruthy();
    expect(screen.queryByTestId("projects-toggle-project-object-member-cold")).toBeNull();
  });

  it("section-scoped scan error renders inside the config section, not the whole screen", async () => {
    mountDetail(proj({ key: "/home/x/proj", scan: undefined, scan_error: "PROJECT_ROOT_INVALID" }));
    render(<ProjectsScreen route={routeWithPath("/home/x/proj")} />);
    await waitFor(() => expect(screen.queryByTestId("projects-detail")).toBeTruthy());
    // The whole screen still renders (workspace + groups sections present).
    expect(screen.getByTestId("projects-section-workspace")).toBeTruthy();
    expect(screen.getByTestId("projects-section-groups")).toBeTruthy();
    // The config section shows the section-scoped error.
    expect(screen.getByTestId("projects-section-config-error").textContent).toContain("could not be read");
  });
});
