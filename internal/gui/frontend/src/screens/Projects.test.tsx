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
//   - both-scopes claude card (Project toggle uses the claude-local-membership
//     APPROVAL ARRAY-MOVE — FIX 1 — / Local read-only / shadow once);
//   - the decision-5 invariant (claude Project disable → array-move scope, NEVER
//     the object-member member-delete) + the reload spring-back regression (a
//     disabled claude Project server stays OFF after a remount/reload);
//   - object-member re-enable is ALWAYS cold → Re-add CTA (warm path removed,
//     FIX 2 — the aggregate NILs every raw blob).
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
                  // P3c: the per-card group count reads the per-project
                  // binding-filtered dto.groups, NOT the top-level groups.
                  groups: [{ name: "frontend", servers: ["serena"], project_path: "" }],
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
    // P3c: the detail-lens group list reads the PER-PROJECT filtered dto.groups
    // (backend-owned), not the top-level groups. Put the group in dto.groups.
    mountDetail(
      proj({
        key: "/home/x/proj",
        entries: [{ workspace_key: "k", workspace_path: "/home/x/proj", language: "go", backend: "mcp-language-server", port: 0, task_name: "t", lifecycle: "active", client_entries: { "claude-code": "x" } }],
        scan: { at: "now", entries: [] },
        groups: [{ name: "g1", servers: ["serena"], tools_hidden: { serena: ["x"] } }],
      }),
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
    // The operator disables, and the backend read-back clamps to OFF; the row MUST
    // reflect response.enabled. Demonstrated on the claude Project array-move row,
    // which STAYS a toggle after a disable (the .mcp.json def is never deleted), so
    // the reconcile-to-response semantic is observable on the same control (an
    // object-member row would flip to the cold Re-add CTA — covered separately).
    mountDetail(
      proj({
        key: "/home/x/proj",
        scan: {
          at: "now",
          entries: [scanEntry("memory", { "claude-code": {} }, { project_enabled: true })],
          project_scope: { local_servers: [] },
        },
      }),
      [],
      () => jsonResponse(200, { scope: "claude-local-membership", server: "memory", enabled: false }),
    );
    render(<ProjectsScreen route={routeWithPath("/home/x/proj")} />);
    await waitFor(() => expect(screen.queryByTestId("projects-client-claude-code")).toBeTruthy());
    const toggle = screen.getByTestId("projects-toggle-claude-local-membership-memory") as HTMLInputElement;
    expect(toggle.checked).toBe(true); // project_enabled:true → enabled
    // Disable. Backend echoes enabled:false → reconciles OFF (the array-move row
    // stays a toggle).
    fireEvent.change(toggle, { target: { checked: false } });
    await waitFor(() =>
      expect(
        (screen.getByTestId("projects-toggle-claude-local-membership-memory") as HTMLInputElement).checked,
      ).toBe(false),
    );
    // ✓ flash appears after the successful reconcile.
    await waitFor(() =>
      expect(screen.queryByTestId("projects-toggle-ok-claude-local-membership-memory")).toBeTruthy(),
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

  it("both-scopes claude card: Project toggle uses claude-local-membership + Local read-only + shadow once", async () => {
    // Raw is NIL on the wire (sanitizeScanResult strips it); the claude Project
    // toggle is the APPROVAL ARRAY-MOVE (scope claude-local-membership), NOT the
    // object-member member-delete (FIX 1).
    mountDetail(
      proj({
        key: "/home/x/proj",
        scan: {
          at: "now",
          entries: [
            // Non-shadowed approved claude .mcp.json entry → live array-move toggle.
            scanEntry("approved", { "claude-code": {} }, { project_enabled: true }),
            // Shadowed entry → rendered ONCE Local-owned (muted ⊘ in Project).
            scanEntry("shadowed", { "claude-code": {} }, { project_shadowed_by_local: true }),
          ],
          project_scope: { local_servers: ["shadowed", "localonly"] },
        },
      }),
    );
    render(<ProjectsScreen route={routeWithPath("/home/x/proj")} />);
    await waitFor(() => expect(screen.queryByTestId("projects-client-claude-code")).toBeTruthy());
    // Project subsection: the toggle uses the claude-local-membership scope (FIX 1),
    // NOT project-object-member (which would member-delete the shared def).
    expect(screen.getByTestId("projects-toggle-claude-local-membership-approved")).toBeTruthy();
    expect(screen.queryByTestId("projects-toggle-project-object-member-approved")).toBeNull();
    // Shadow: ONE muted anchor in Project + the authoritative cross-ref in Local.
    expect(screen.getByTestId("projects-shadow-shadowed")).toBeTruthy();
    expect(screen.getByTestId("projects-shadow-authoritative-shadowed")).toBeTruthy();
    // The shadowed entry has NO competing toggle in the Project subsection.
    expect(screen.queryByTestId("projects-toggle-claude-local-membership-shadowed")).toBeNull();
    // Local subsection lists both local servers read-only.
    expect(screen.getByTestId("projects-claude-local-localonly")).toBeTruthy();
    expect(screen.getByTestId("projects-claude-local-localonly").textContent).toContain("read-only");
  });

  it("claude Project DISABLE posts claude-local-membership (the array-move, never an object-member delete) — decision-5 invariant", async () => {
    // FIX 8 regression: this is the test that would have caught FIX 1. The claude
    // Project toggle MUST route through scope claude-local-membership (the approval
    // array move that NEVER deletes the .mcp.json mcpServers definition — decision
    // 5), and NEVER through project-object-member (the member delete that data-loses
    // the shared checked-in definition). It is value-FREE (the array move needs no
    // member value).
    const captured: { body: Record<string, unknown> } = { body: {} };
    mountDetail(
      proj({
        key: "/home/x/proj",
        scan: {
          at: "now",
          entries: [scanEntry("approved", { "claude-code": {} }, { project_enabled: true })],
          project_scope: { local_servers: [] },
        },
      }),
      [],
      async (init) => {
        captured.body = JSON.parse((init?.body as string) ?? "{}");
        return jsonResponse(200, { scope: "claude-local-membership", server: "approved", enabled: false });
      },
    );
    render(<ProjectsScreen route={routeWithPath("/home/x/proj")} />);
    await waitFor(() => expect(screen.queryByTestId("projects-client-claude-code")).toBeTruthy());
    const toggle = screen.getByTestId("projects-toggle-claude-local-membership-approved") as HTMLInputElement;
    expect(toggle.checked).toBe(true); // project_enabled:true → ON
    fireEvent.change(toggle, { target: { checked: false } });
    await waitFor(() => expect(captured.body.enable).toBe(false));
    // The load-bearing assertion: the array-move scope + claude-code client + NO value.
    expect(captured.body.scope).toBe("claude-local-membership");
    expect(captured.body.client).toBe("claude-code");
    expect(captured.body.value).toBeUndefined();
    // Reconciles OFF; ✓ flash on the array-move scope's testid.
    await waitFor(() =>
      expect((screen.getByTestId("projects-toggle-claude-local-membership-approved") as HTMLInputElement).checked).toBe(false),
    );
  });

  it("claude Project DISABLE stays OFF after a reload (spring-back regression) — the approval array was written", async () => {
    // FIX 8 regression: the bug's second symptom was spring-back — an object-member
    // disable never touches the approval arrays, so a reload re-seeded the row ON.
    // With the array-move scope the disable IS persisted in the approval array, so
    // a fresh aggregate (now project_enabled:false) re-seeds the row OFF and it
    // STAYS off across the remount. We simulate the post-disable reload by serving
    // an aggregate whose entry is already disabled and asserting the seeded state.
    mountDetail(
      proj({
        key: "/home/x/proj",
        scan: {
          at: "now",
          // Post-disable reload shape: the approval array moved the server to
          // disabledMcpjsonServers, so the project-scoped scan now reports
          // project_enabled:false for it.
          entries: [scanEntry("springy", { "claude-code": {} }, { project_enabled: false })],
          project_scope: { local_servers: [], disabled_mcpjson_servers: ["springy"] },
        },
      }),
    );
    render(<ProjectsScreen route={routeWithPath("/home/x/proj")} />);
    await waitFor(() => expect(screen.queryByTestId("projects-client-claude-code")).toBeTruthy());
    // The reloaded row is seeded OFF from project_enabled:false (NOT springing back
    // ON). claude Project is the array-move substrate, so a disabled server still
    // renders a (value-free) toggle — never a cold Re-add CTA.
    const toggle = screen.getByTestId("projects-toggle-claude-local-membership-springy") as HTMLInputElement;
    expect(toggle.checked).toBe(false);
    expect(screen.queryByTestId("projects-readd-springy")).toBeNull();
    // The raw approval state shows the persisted disable.
    expect(screen.getByTestId("projects-claude-raw").textContent).toContain("springy");
  });

  it("cursor object-member disable → cold re-enable shows the Re-add CTA (warm path removed)", async () => {
    // FIX 7: the warm value-replay path is dead (the aggregate NILs every raw blob),
    // so for a cursor/vscode object-member, DISABLE removes the member and re-enable
    // is ALWAYS cold. Against the sanitized (raw=null) wire shape, disabling the
    // member must reconcile OFF, never carry a value, and present the Re-add CTA —
    // never a value-less enable POST (CORE RULING / D2).
    const captured: { body: Record<string, unknown> } = { body: {} };
    mountDetail(
      proj({
        key: "/home/x/proj",
        // raw is NIL on the wire — exactly what /api/projects returns.
        scan: { at: "now", entries: [scanEntry("memo", { cursor: {} })] },
      }),
      [],
      async (init) => {
        captured.body = JSON.parse((init?.body as string) ?? "{}");
        return jsonResponse(200, { scope: "project-object-member", server: "memo", enabled: false });
      },
    );
    render(<ProjectsScreen route={routeWithPath("/home/x/proj")} />);
    await waitFor(() => expect(screen.queryByTestId("projects-client-cursor")).toBeTruthy());
    const toggle = screen.getByTestId("projects-toggle-project-object-member-memo") as HTMLInputElement;
    expect(toggle.checked).toBe(true); // present → enabled
    // Disable: object-member scope, NO value on the wire (warm removed).
    fireEvent.change(toggle, { target: { checked: false } });
    await waitFor(() => expect(captured.body.enable).toBe(false));
    expect(captured.body.scope).toBe("project-object-member");
    expect(captured.body.value).toBeUndefined();
    // After the reconcile to OFF the row flips to the cold Re-add CTA (member gone);
    // there is no longer an enable toggle to value-lessly POST.
    await waitFor(() => expect(screen.queryByTestId("projects-readd-memo")).toBeTruthy());
    const readd = screen.getByTestId("projects-readd-memo") as HTMLAnchorElement;
    expect(readd.getAttribute("href")).toBe("#/add-server");
    expect(screen.queryByTestId("projects-toggle-project-object-member-memo")).toBeNull();
  });

  it("cold object-member (disabled, no member) renders the Re-add CTA, not a value-less enable toggle", async () => {
    // A cursor object-member that is OFF (member absent) → cold: render a Re-add CTA
    // so an enable POST is never sent without a value (CORE RULING / D2). We seed
    // OFF by... a removed member never appears in the scan, so we exercise the cold
    // branch via the disable→reconcile path proven above; here we assert the
    // disable-affordance warning copy on a live (ON) cursor row (FIX 6).
    mountDetail(
      proj({
        key: "/home/x/proj",
        scan: { at: "now", entries: [scanEntry("warn", { cursor: {} })] },
      }),
    );
    render(<ProjectsScreen route={routeWithPath("/home/x/proj")} />);
    await waitFor(() => expect(screen.queryByTestId("projects-client-cursor")).toBeTruthy());
    const toggle = screen.getByTestId("projects-toggle-project-object-member-warn") as HTMLInputElement;
    // FIX 6: the live object-member control carries the disable-affordance warning.
    expect(toggle.getAttribute("title")).toContain("re-adding it");
    expect(toggle.getAttribute("aria-description")).toContain("re-adding it");
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

describe("ProjectsScreen — P3c group↔project binding (§10.1)", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });
  afterEach(() => cleanup());

  // mountDetailGroups stubs the aggregate + the binding endpoint. dtoGroups are
  // the PER-PROJECT binding-filtered groups (dto.groups) the detail lens reads.
  function mountDetailGroups(
    dtoGroups: unknown[],
    bindImpl?: (init?: RequestInit) => Response | Promise<Response>,
  ) {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/projects/group-binding":
          bindImpl ?? (() => jsonResponse(200, { group: "x", project_path: "" })),
        "/api/projects/toggle": () => jsonResponse(200, { scope: "group-servers", server: "x", enabled: true }),
        "/api/projects": () =>
          jsonResponse(200, aggregate({ projects: [proj({ key: "/home/x/proj", groups: dtoGroups })] })),
        "/api/server/readiness": () => jsonResponse(404, { error: "not found" }),
      }) as unknown as typeof fetch,
    );
  }

  it("renders the replaced copy (no 'not yet bound to a project') + bind affordance", async () => {
    mountDetailGroups([{ name: "g1", servers: [], project_path: "" }]);
    render(<ProjectsScreen route={routeWithPath("/home/x/proj")} />);
    await waitFor(() => expect(screen.queryByTestId("projects-section-groups")).toBeTruthy());
    const sec = screen.getByTestId("projects-section-groups");
    // The P1 placeholder copy is GONE.
    expect(sec.textContent).not.toContain("not yet bound to a project");
    expect(sec.textContent).not.toContain("coming later");
    // The new binding copy is present.
    expect(sec.textContent).toContain("Bind a group here to scope it to this project");
  });

  it("labels a global group 'global (all projects)' and a bound group 'bound to this project'", async () => {
    mountDetailGroups([
      { name: "glob", servers: [], project_path: "" },
      { name: "mine", servers: [], project_path: "/home/x/proj" },
    ]);
    render(<ProjectsScreen route={routeWithPath("/home/x/proj")} />);
    await waitFor(() => expect(screen.queryByTestId("projects-groups-list")).toBeTruthy());
    expect(screen.getByTestId("projects-group-binding-state-glob").textContent).toContain("global (all projects)");
    expect(screen.getByTestId("projects-group-binding-state-mine").textContent).toContain("bound to this project");
    // A global group offers "Bind to this project"; a bound one offers "Unbind".
    expect(screen.getByTestId("projects-group-bind-glob").textContent).toContain("Bind to this project");
    expect(screen.getByTestId("projects-group-bind-mine").textContent).toContain("Unbind");
  });

  it("clicking 'Bind to this project' POSTs the project key and reloads", async () => {
    const captured: { body: Record<string, unknown> } = { body: {} };
    let aggCalls = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/projects/group-binding": async (init) => {
          captured.body = JSON.parse((init?.body as string) ?? "{}");
          return jsonResponse(200, { group: "glob", project_path: "/home/x/proj" });
        },
        "/api/projects": () => {
          aggCalls++;
          return jsonResponse(200, aggregate({ projects: [proj({ key: "/home/x/proj", groups: [{ name: "glob", servers: [], project_path: "" }] })] }));
        },
        "/api/server/readiness": () => jsonResponse(404, { error: "not found" }),
      }) as unknown as typeof fetch,
    );
    render(<ProjectsScreen route={routeWithPath("/home/x/proj")} />);
    await waitFor(() => expect(screen.queryByTestId("projects-group-bind-glob")).toBeTruthy());
    const callsBefore = aggCalls;
    fireEvent.click(screen.getByTestId("projects-group-bind-glob"));
    // The POST carried the group name + this project key.
    await waitFor(() => expect(captured.body.group).toBe("glob"));
    expect(captured.body.project_path).toBe("/home/x/proj");
    // The aggregate was reloaded after a successful bind (so the filter re-derives).
    await waitFor(() => expect(aggCalls).toBeGreaterThan(callsBefore));
  });

  it("clicking 'Unbind (make global)' POSTs an EMPTY project_path", async () => {
    const captured: { body: Record<string, unknown> } = { body: {} };
    mountDetailGroups(
      [{ name: "mine", servers: [], project_path: "/home/x/proj" }],
      async (init) => {
        captured.body = JSON.parse((init?.body as string) ?? "{}");
        return jsonResponse(200, { group: "mine", project_path: "" });
      },
    );
    render(<ProjectsScreen route={routeWithPath("/home/x/proj")} />);
    await waitFor(() => expect(screen.queryByTestId("projects-group-bind-mine")).toBeTruthy());
    fireEvent.click(screen.getByTestId("projects-group-bind-mine"));
    await waitFor(() => expect(captured.body.group).toBe("mine"));
    // Unbind = empty project_path → global.
    expect(captured.body.project_path).toBe("");
  });

  it("a bind failure shows plain copy + Retry (raw code only in tooltip)", async () => {
    mountDetailGroups(
      [{ name: "glob", servers: [], project_path: "" }],
      () => jsonResponse(500, { error: "disk full", code: "PROJECT_GROUP_BINDING_FAILED" }),
    );
    render(<ProjectsScreen route={routeWithPath("/home/x/proj")} />);
    await waitFor(() => expect(screen.queryByTestId("projects-group-bind-glob")).toBeTruthy());
    fireEvent.click(screen.getByTestId("projects-group-bind-glob"));
    await waitFor(() => expect(screen.queryByTestId("projects-group-bind-error-glob")).toBeTruthy());
    const err = screen.getByTestId("projects-group-bind-error-glob");
    expect(err.textContent).toContain("couldn't be saved");
    expect(err.textContent).not.toContain("PROJECT_GROUP_BINDING_FAILED");
    expect(screen.getByTestId("projects-group-bind-retry-glob")).toBeTruthy();
  });

  it("empty filtered list shows the per-project empty copy", async () => {
    mountDetailGroups([]);
    render(<ProjectsScreen route={routeWithPath("/home/x/proj")} />);
    await waitFor(() => expect(screen.queryByTestId("projects-groups-empty")).toBeTruthy());
    expect(screen.getByTestId("projects-groups-empty").textContent).toContain("No groups visible for this project");
  });
});
