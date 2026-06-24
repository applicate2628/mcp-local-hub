// screens/Projects.tsx — the per-project lens (epic
// work-items/epics/2026-06-19-install-and-it-works-ux.md area 6, Phase 1).
//
// "Projects" is a NEW read-only nav screen that composes the TWO existing
// read endpoints — GET /api/workspaces and GET /api/groups — into a
// project-centric view. It is the Model-A complement to the global Servers
// matrix: where Servers is a server×client grid, Projects groups every
// per-project mechanism (workspace LSP rows, project-local .mcp.json config,
// and groups) under ONE card per project path.
//
// Phase 1 scope is strictly READ-ONLY + ADDITIVE:
//   - NO backend change — it composes the two existing endpoints client-side.
//   - NO write/toggle/editor — the per-mechanism Enable/Disable (P2), the
//     project-config write (Model B, P2/P3), and the group↔project binding
//     (P3) are deliberately NOT here. P1 shows the sections EXIST and where
//     each one is backed, with an anti-confusion "backed by …" provenance
//     label so the operator never mistakes which mechanism owns what.
//   - The global Servers matrix stays byte-untouched.
//
// Structure mirrors the existing screens' conventions:
//   - LIST view (#/projects): one card per unique project (= unique canonical
//     workspace path) with a one-line per-mechanism summary, plus an
//     empty-state card on a clean install (mirrors Groups' empty-state).
//   - DETAIL lens (#/projects?path=<canonicalKey>): 3 labelled sections, each
//     with a right-aligned "backed by <file>" provenance label + a mechanism
//     badge. Section [A] reuses the Servers chip + state classes; [B] is a
//     not-yet-wired placeholder; [C] is a read-only list of ALL groups.
//
// Reuse map (cite file:line of each reused pattern):
//   - list-of-cards + empty-state + error+Retry → screens/Groups.tsx:300-408
//   - tools_hidden NOT-a-security-fence note (verbatim) → Groups.tsx:700-705
//   - stateShape() + state-ok/state-down/state-shape classes → Servers.tsx:1213-1221
//   - lsp-chip lsp-chip-via-hub/-direct/-legacy classes → Servers.tsx:1700-1715
//   - mountedRef async-guard → Servers.tsx:256-262
//   - per-endpoint catch-isolation (one failing fetch ≠ whole screen) → Servers.tsx:299-303
//   - route?.query → URLSearchParams param parse → screens/Secrets.tsx:23-31
//
// DATA-FIDELITY NOTE (honest composition, no faked data): the /api/workspaces
// `entries` DTO carries `client_entries` (client→entry-name) + `backend`
// ("mcp-language-server" | "gopls-mcp" | "serena") + `lifecycle`/`last_error`,
// but it does NOT carry the per-client `transport` presence the live Servers
// SCAN does. So the section [A] routing chip is derived from what the registry
// actually knows: a router-backed backend routes through the hub LSP router
// (via-hub); the legacy direct-stdio backend is direct. The chip is rendered
// per client that has a `client_entries` entry — that is the registry's own
// record of which clients this workspace LSP is registered for. We do not
// fabricate a transport field the endpoint never sends.

import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import type { RouterState } from "../hooks/useRouter";
import {
  listWorkspaces,
  getGroups,
  type WorkspaceEntryDTO,
  type GroupDTO,
} from "../api";
import { stateShape } from "../lib/status";

// canonicalProjectKey is the SINGLE owner of the project join key (this file).
// A workspace_path (Model A) is normalized into a canonical key used both to
// (a) deduplicate workspace entries into one card per project and (b) match
// the #/projects?path=<key> detail-lens param back to its entries.
//
// Normalization: filepath-clean (collapse `.`/`..`/duplicate separators) +
// forward-slash normalize (backslashes → `/`, strip a trailing slash). This is
// a display/join key, not a filesystem operation — it never touches disk.
//
// CASE IS PRESERVED. The P1 join is workspace-path ↔ workspace-path: both sides
// come from the SAME backend source (/api/workspaces) with the SAME casing, so
// a case-sensitive compare is correct and never collapses two distinct entries.
// We deliberately do NOT case-fold on Windows here: navigator.platform reflects
// the BROWSER's OS, not the hub's — a POSIX hub opened from a Windows browser
// (WSL/remote) would otherwise wrongly fold case-sensitive POSIX paths
// (`/home/A/Repo` vs `/home/a/repo`) and merge 2 distinct projects into one
// card. The OS-aware fold is only needed for the P2 Model-B claude-key↔path
// join, which can take a BACKEND-provided OS hint at that point.
export function canonicalProjectKey(rawPath: string): string {
  let p = (rawPath ?? "").trim();
  if (p === "") return "";
  // Normalize separators to forward slashes first so the clean pass is
  // separator-agnostic across Windows backslashes and POSIX slashes.
  p = p.replace(/\\/g, "/");
  // filepath-clean equivalent: collapse runs of slashes, resolve `.` and `..`
  // segments. Preserve a leading slash (POSIX root) and a leading drive
  // (`C:`) verbatim. A leading `//` is a UNC root (`\\server\share` →
  // `//server/share`) and MUST keep BOTH slashes — collapsing it to a single
  // `/server/share` would silently rewrite the host into a path segment.
  const isUNC = p.startsWith("//");
  const hasLeadingSlash = p.startsWith("/");
  const segments = p.split("/");
  const out: string[] = [];
  for (const seg of segments) {
    if (seg === "" || seg === ".") continue;
    if (seg === "..") {
      // Pop the last real segment unless we'd escape past a root/drive.
      if (out.length > 0 && out[out.length - 1] !== "..") {
        out.pop();
      } else if (!hasLeadingSlash) {
        out.push("..");
      }
      continue;
    }
    out.push(seg);
  }
  const prefix = isUNC ? "//" : hasLeadingSlash ? "/" : "";
  let cleaned = prefix + out.join("/");
  if (cleaned === "") cleaned = hasLeadingSlash ? "/" : ".";
  // Strip a trailing slash (except the bare root "/").
  if (cleaned.length > 1 && cleaned.endsWith("/")) cleaned = cleaned.slice(0, -1);
  return cleaned;
}

// routingChipForBackend maps a workspace registry `backend` to one of the
// reused lsp-chip variants. Router-backed backends (the per-language
// mcp-language-server router, the go-specific gopls-mcp router, and serena's
// dynamic-pool proxy) route through the hub LSP router → "via-hub". A bare
// legacy direct-stdio entry is "direct". The deprecated `legacy-lsp` backend
// (the old direct-LSP path the router replaced — see admission_check_test.go's
// Backend:"legacy-lsp") is "legacy", rendered with the red lsp-chip-legacy
// class so the operator sees it is the OLD path, not a healthy via-hub route.
// Unknown/blank → null (render "—"). An unknown NON-empty backend must NOT be
// fabricated into a green via-hub chip — we honestly render no chip rather than
// guess routing the registry never told us.
//
// This is the honest mapping from what /api/workspaces actually carries; it is
// NOT the live-scan per-client transport probe the Servers matrix runs.
function routingChipForBackend(backend: string): "via-hub" | "direct" | "legacy" | null {
  switch ((backend || "").trim()) {
    case "mcp-language-server":
    case "gopls-mcp":
    case "serena":
      return "via-hub";
    case "stdio":
    case "direct":
      return "direct";
    case "legacy-lsp":
      return "legacy";
    default:
      return null;
  }
}

// lifecycleState maps a workspace entry's lifecycle/last_error into the same
// vocabulary stateShape() understands so the State column reuses the exact
// Servers glyph + color classes. The registry's authoritative lifecycle words
// are the 5 in internal/api/workspace_registry.go:20-26 — "configured",
// "starting", "active", "missing", "failed" — folded here to the capitalized
// health vocabulary stateShape switches on:
//   - "active" (materialized + healthy) → Running (state-ok ●)
//   - "missing"/"failed" → Failed: a DOWN/ERROR state (state-down ✕), NOT a
//     benign idle. "missing" = the LSP binary is not on PATH; "failed" = a
//     materialization error. Both are real failures the operator must see, so
//     they must never read as the neutral open circle.
//   - "starting" → Starting (transient ◓)
//   - "configured"/""/"idle" → Stopped (benign idle ○): registry entry exists,
//     backend not spawned — deliberately not running.
// A non-empty last_error always reads as Failed regardless of lifecycle (it is
// surfaced as the cell tooltip so the operator can read the cause).
function lifecycleState(entry: WorkspaceEntryDTO): string {
  if ((entry.last_error ?? "").trim() !== "") return "Failed";
  switch ((entry.lifecycle ?? "").trim().toLowerCase()) {
    case "active":
    case "materialized":
    case "running":
      return "Running";
    case "missing":
    case "failed":
      return "Failed";
    case "starting":
    case "spawning":
      return "Starting";
    case "":
    case "configured":
    case "idle":
    case "registered":
      return "Stopped";
    default:
      // Unknown lifecycle word reads as benign idle (open circle), never the
      // error cross — matches stateShape's default.
      return "Stopped";
  }
}

// Project is one row in the LIST view: a unique canonical project path plus
// the entries/groups that belong to it. workspacePath is the first raw
// (un-lowercased) path seen for the key so the card shows the operator's own
// casing rather than the lowercased join key.
interface Project {
  key: string;
  workspacePath: string;
  entries: WorkspaceEntryDTO[];
}

// LoadState mirrors Groups' three-state load with PER-ENDPOINT isolation: a
// failed /api/workspaces does not blank the groups section and vice versa.
// Each endpoint result is either its data or an error string; the screen only
// hard-fails (full error+Retry) when BOTH fail, otherwise it renders what it
// has and shows a section-scoped error inside the failed mechanism's section.
type EndpointResult<T> = { ok: true; data: T } | { ok: false; error: string };

interface LoadState {
  kind: "loading" | "ready";
  workspaces: EndpointResult<WorkspaceEntryDTO[]>;
  groups: EndpointResult<GroupDTO[]>;
}

function asError(e: unknown): string {
  if (e instanceof Error) return e.message;
  return String(e);
}

const LOADING_STATE: LoadState = {
  kind: "loading",
  workspaces: { ok: true, data: [] },
  groups: { ok: true, data: [] },
};

export interface ProjectsScreenProps {
  // route carries ?path=<canonicalKey> for the detail lens. Read-only; no
  // onDirtyChange because P1 has no editor (mirrors Secrets' route?: prop).
  route?: RouterState;
}

export function ProjectsScreen({ route }: ProjectsScreenProps): preact.JSX.Element {
  const [state, setState] = useState<LoadState>(LOADING_STATE);

  // mountedRef guards the post-await setState against a navigate-away race
  // (mirrors Servers.tsx:256-262).
  const mountedRef = useRef<boolean>(true);
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  async function load(): Promise<void> {
    setState((s) => ({ ...s, kind: "loading" }));
    // Per-endpoint catch-isolation (mirrors Servers.tsx:299-303): one failing
    // fetch must not blank the other mechanism's section. Settle both, map
    // each independently.
    const [wsRes, grRes] = await Promise.allSettled([listWorkspaces(), getGroups()]);
    if (!mountedRef.current) return;
    const workspaces: EndpointResult<WorkspaceEntryDTO[]> =
      wsRes.status === "fulfilled"
        ? { ok: true, data: wsRes.value.entries }
        : { ok: false, error: asError(wsRes.reason) };
    const groups: EndpointResult<GroupDTO[]> =
      grRes.status === "fulfilled"
        ? { ok: true, data: grRes.value.groups }
        : { ok: false, error: asError(grRes.reason) };
    setState({ kind: "ready", workspaces, groups });
  }

  useEffect(() => {
    void load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // selectedPath is the detail-lens param (mirrors Secrets.tsx:23-31). Empty =
  // LIST view; a non-empty value selects the detail lens for that project key.
  const selectedPath = useMemo(() => {
    const raw = new URLSearchParams(route?.query ?? "").get("path")?.trim();
    return raw && raw.length > 0 ? raw : null;
  }, [route?.query]);

  // projects collapses the workspace entries into one Project per canonical
  // path. canonicalProjectKey is the single join owner.
  const projects = useMemo<Project[]>(() => {
    if (!state.workspaces.ok) return [];
    const byKey = new Map<string, Project>();
    for (const entry of state.workspaces.data) {
      const key = canonicalProjectKey(entry.workspace_path);
      if (key === "") continue;
      const existing = byKey.get(key);
      if (existing) {
        existing.entries.push(entry);
      } else {
        byKey.set(key, { key, workspacePath: entry.workspace_path, entries: [entry] });
      }
    }
    return Array.from(byKey.values()).sort((a, b) => a.key.localeCompare(b.key));
  }, [state.workspaces]);

  const groups: GroupDTO[] = state.groups.ok ? state.groups.data : [];

  // Hard-fail only when BOTH endpoints fail — otherwise render what we have
  // with a section-scoped error inside the failed mechanism.
  const bothFailed = !state.workspaces.ok && !state.groups.ok;

  if (state.kind === "loading") {
    return (
      <section class="projects-screen" data-testid="projects-loading">
        <h1>Projects</h1>
        <p>Loading…</p>
      </section>
    );
  }

  if (bothFailed) {
    // Both endpoints failed — surface BOTH errors (dropping the groups error
    // here would hide half the diagnosis).
    const wsErr = state.workspaces.ok ? "" : state.workspaces.error;
    const grErr = state.groups.ok ? "" : state.groups.error;
    return (
      <section class="projects-screen" data-testid="projects-error">
        <h1>Projects</h1>
        <p class="settings-error" data-testid="projects-load-error">
          Could not load projects.
        </p>
        <p class="settings-error" data-testid="projects-load-error-workspaces">
          Workspace tools: {wsErr}
        </p>
        <p class="settings-error" data-testid="projects-load-error-groups">
          Groups: {grErr}
        </p>
        <button type="button" class="btn" onClick={() => void load()}>
          Retry
        </button>
      </section>
    );
  }

  if (selectedPath !== null) {
    const project = projects.find((p) => p.key === selectedPath) ?? null;
    return (
      <ProjectDetail
        projectKey={selectedPath}
        project={project}
        workspacesError={state.workspaces.ok ? null : state.workspaces.error}
        groups={groups}
        groupsError={state.groups.ok ? null : state.groups.error}
        onRetry={() => void load()}
      />
    );
  }

  return (
    <ProjectList
      projects={projects}
      groups={groups}
      workspacesError={state.workspaces.ok ? null : state.workspaces.error}
      groupsError={state.groups.ok ? null : state.groups.error}
      onRetry={() => void load()}
    />
  );
}

// ProjectList renders the LIST view (#/projects): one card per project + an
// empty-state. Mirrors the Groups list-of-cards layout (Groups.tsx:356-408).
function ProjectList({
  projects,
  groups,
  workspacesError,
  groupsError,
  onRetry,
}: {
  projects: Project[];
  groups: GroupDTO[];
  workspacesError: string | null;
  groupsError: string | null;
  onRetry: () => void;
}): preact.JSX.Element {
  return (
    <section class="projects-screen" data-testid="projects-loaded">
      <h1>Projects</h1>
      <p class="m-0 mb-4 text-sm text-app-muted">
        Each card is one project (a registered workspace path). It composes the
        per-project mechanisms — workspace LSP tools, project-local{" "}
        <code>.mcp.json</code> config, and groups — into one lens, so you manage
        a project&rsquo;s MCP servers per-project instead of hunting across the
        global matrix.
      </p>

      {groupsError !== null && (
        <p class="settings-error" data-testid="projects-groups-error" role="alert">
          Could not load groups: {groupsError} — the per-card group count below
          reads &ldquo;? groups&rdquo; (load failed), not 0.{" "}
          <button type="button" class="btn text-xs" onClick={onRetry}>
            Retry
          </button>
        </p>
      )}

      {workspacesError !== null ? (
        // The workspace registry failed to load, so `projects` is [] for a
        // reason that has nothing to do with a clean install. Render an explicit
        // UNAVAILABLE list state — NEVER the "No projects yet" empty-state, which
        // would falsely tell the operator the registry is empty and send them to
        // re-register projects that may already exist. The empty-state below is
        // reachable ONLY when the workspace registry loaded OK and is genuinely
        // empty.
        <p class="settings-error" data-testid="projects-workspaces-error" role="alert">
          Projects unavailable — failed to load the workspace registry:{" "}
          {workspacesError}{" "}
          <button type="button" class="btn text-xs" onClick={onRetry}>
            Retry
          </button>
        </p>
      ) : projects.length === 0 ? (
        <p class="empty-state" data-testid="projects-empty">
          No projects yet. Register a project to manage its MCP servers
          per-project.
        </p>
      ) : (
        <ul class="projects-list m-0 list-none p-0" data-testid="projects-list">
          {projects.map((p) => {
            const wsCount = p.entries.length;
            // Groups are not yet path-bound (P3), so every project lists ALL
            // groups in the detail lens; the per-card summary reflects the
            // global group count so the operator sees the mechanism exists.
            // When the groups fetch FAILED, render "? groups (load failed)"
            // rather than a misleading "0 groups" — a real empty groups.yaml
            // and a failed load must be visually distinguishable.
            const grSummary =
              groupsError !== null
                ? "? groups (load failed)"
                : `${groups.length} group${groups.length === 1 ? "" : "s"}`;
            return (
              <li
                key={p.key}
                class="card mb-3"
                data-testid={`projects-row-${p.key}`}
                data-project={p.key}
              >
                <a
                  class="card-title"
                  href={`#/projects?path=${encodeURIComponent(p.key)}`}
                  data-testid={`projects-open-${p.key}`}
                >
                  {p.workspacePath}
                </a>
                <p
                  class="m-0 text-xs text-app-muted"
                  data-testid={`projects-summary-${p.key}`}
                >
                  {/* entries are the per-(language) workspace tool registrations
                      for this ONE project, NOT separate workspaces — labelling
                      them "workspaces" disagreed with the one-card-per-project
                      model. They are "workspace tools". */}
                  {wsCount} workspace tool{wsCount === 1 ? "" : "s"} ·{" "}
                  {/* Project MCP config (Model B) is not wired in P1 → always — */}
                  &mdash; project-config · {grSummary}
                </p>
              </li>
            );
          })}
        </ul>
      )}
    </section>
  );
}

// MechanismBadge is the right-aligned "backed by <file>" provenance label +
// mechanism badge for a detail section (the anti-confusion rule). It tells the
// operator exactly which on-disk mechanism owns this section so the three are
// never confused for one another. The badge is a LABEL ("backed by …"), NOT a
// routing-chip value — so it uses a NEUTRAL .projects-mechanism-badge class
// (theme-token gray) rather than the green lsp-chip-via-hub, which would both
// misread as a routing signal and wash out on the dark theme.
function MechanismBadge({
  label,
  backedBy,
  testId,
}: {
  label: string;
  backedBy: string;
  testId: string;
}): preact.JSX.Element {
  return (
    <span class="flex items-center gap-2 text-xs text-app-muted" data-testid={testId}>
      <span class="projects-mechanism-badge">{label}</span>
      <span>
        backed by <code>{backedBy}</code>
      </span>
    </span>
  );
}

// ProjectDetail renders the DETAIL lens (#/projects?path=<key>): the 3 labelled
// sections. Read-only in P1 — no Enable/Disable, no editor.
function ProjectDetail({
  projectKey,
  project,
  workspacesError,
  groups,
  groupsError,
  onRetry,
}: {
  projectKey: string;
  project: Project | null;
  workspacesError: string | null;
  groups: GroupDTO[];
  groupsError: string | null;
  onRetry: () => void;
}): preact.JSX.Element {
  const displayPath = project?.workspacePath ?? projectKey;
  return (
    <section class="projects-screen" data-testid="projects-detail">
      <div class="mb-1 flex items-center justify-between gap-3">
        <h1 class="m-0">{displayPath}</h1>
        <a class="btn text-xs" href="#/projects" data-testid="projects-back">
          ← All projects
        </a>
      </div>
      {/* Quiet cross-link into the global Servers matrix. ServersScreen inits to
          the all-workspaces sentinel (ALL_WORKSPACES_KEY), so #/servers opens
          the GLOBAL matrix — NOT a view scoped to this project's path. The copy
          says exactly that so it does not promise a scoped lens it can't deliver
          (the route-scoped deep-link is a P4 nicety; faking it here by reading a
          route query would have to touch the protected Servers.tsx). */}
      <p class="m-0 mb-4 text-xs text-app-muted">
        <a href="#/servers" data-testid="projects-servers-crosslink">
          Open the global Servers matrix →
        </a>
      </p>

      {/* ───────── [A] Workspace tools (backed by workspaces.yaml) ───────── */}
      <div class="card mb-3" data-testid="projects-section-workspace">
        <div class="card-title flex items-center justify-between gap-3">
          <span>Workspace tools</span>
          <MechanismBadge
            label="workspace"
            backedBy="workspaces.yaml"
            testId="projects-badge-workspace"
          />
        </div>
        {workspacesError !== null ? (
          <p class="settings-error" data-testid="projects-section-workspace-error" role="alert">
            Could not load workspace tools: {workspacesError}{" "}
            <button type="button" class="btn text-xs" onClick={onRetry}>
              Retry
            </button>
          </p>
        ) : !project || project.entries.length === 0 ? (
          <p class="m-0 text-sm text-app-muted" data-testid="projects-workspace-empty">
            No workspace tools registered for this project.
          </p>
        ) : (
          <table class="projects-workspace-table" data-testid="projects-workspace-table">
            <thead>
              <tr>
                <th>Language</th>
                <th>Backend</th>
                <th>Routing</th>
                <th>State</th>
              </tr>
            </thead>
            <tbody>
              {project.entries.map((entry) => {
                const st = lifecycleState(entry);
                const chip = routingChipForBackend(entry.backend);
                const clients = Object.keys(entry.client_entries ?? {}).sort();
                return (
                  <tr
                    key={`${entry.workspace_key}-${entry.language}`}
                    data-testid={`projects-workspace-row-${entry.language}`}
                  >
                    <td>
                      <code>{entry.language}</code>
                    </td>
                    <td>{entry.backend || "—"}</td>
                    <td class="projects-routing-cell">
                      {chip === null ? (
                        <span class="lsp-cell-empty">—</span>
                      ) : clients.length === 0 ? (
                        <span
                          class={`lsp-chip lsp-chip-${chip}`}
                          data-testid={`projects-chip-${entry.language}`}
                        >
                          {chip}
                        </span>
                      ) : (
                        clients.map((client) => (
                          <span
                            key={client}
                            class={`lsp-chip lsp-chip-${chip}`}
                            title={`Registered for ${client}`}
                            data-testid={`projects-chip-${entry.language}-${client}`}
                          >
                            {client}: {chip}
                          </span>
                        ))
                      )}
                    </td>
                    <td
                      class={`state-cell ${st === "Running" ? "state-ok" : st === "Failed" ? "state-down" : ""}`}
                      data-testid={`projects-workspace-state-${entry.language}`}
                      // Surface the materialization error (if any) as the cell
                      // tooltip so a Failed/missing row tells the operator WHY.
                      title={(entry.last_error ?? "").trim() || undefined}
                    >
                      <span class="state-shape" aria-hidden="true">
                        {stateShape(st)}
                      </span>{" "}
                      {st}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>

      {/* ───────── [B] Project MCP config (backed by .mcp.json) ───────── */}
      <div class="card mb-3" data-testid="projects-section-config">
        <div class="card-title flex items-center justify-between gap-3">
          <span>Project MCP config</span>
          <MechanismBadge
            label="project-config"
            backedBy=".mcp.json"
            testId="projects-badge-config"
          />
        </div>
        <p class="m-0 text-sm text-app-muted" data-testid="projects-config-placeholder">
          Not managed here yet — project-local client config support is coming
          (manage in your client for now).
        </p>
      </div>

      {/* ───────── [C] Group lens (backed by groups.yaml) ───────── */}
      <div class="card mb-3" data-testid="projects-section-groups">
        <div class="card-title flex items-center justify-between gap-3">
          <span>Group lens</span>
          <MechanismBadge
            label="group"
            backedBy="groups.yaml"
            testId="projects-badge-groups"
          />
        </div>
        <p class="m-0 mb-2 text-xs text-app-muted">
          Groups are not yet bound to a project path (coming later), so this
          lists every group.{" "}
          <a href="#/groups" data-testid="projects-manage-groups">
            manage in Groups →
          </a>
        </p>
        {groupsError !== null ? (
          <p class="settings-error" data-testid="projects-section-groups-error" role="alert">
            Could not load groups: {groupsError}{" "}
            <button type="button" class="btn text-xs" onClick={onRetry}>
              Retry
            </button>
          </p>
        ) : groups.length === 0 ? (
          <p class="m-0 text-sm text-app-muted" data-testid="projects-groups-empty">
            No groups defined.
          </p>
        ) : (
          <ul class="projects-groups-list m-0 list-none p-0" data-testid="projects-groups-list">
            {groups.map((g) => {
              const hasHidden = g.tools_hidden && Object.keys(g.tools_hidden).length > 0;
              return (
                <li
                  key={g.name}
                  class="py-1.5"
                  data-testid={`projects-group-${g.name}`}
                  data-group={g.name}
                >
                  <span class="text-sm font-medium text-app-text">{g.name}</span>
                  <span class="ml-2 text-xs text-app-muted">
                    {g.servers.length === 0
                      ? "No servers"
                      : `Servers: ${g.servers.join(", ")}`}
                  </span>
                  {hasHidden && (
                    <p
                      class="m-0 mt-1 text-xs text-app-muted"
                      data-testid={`projects-group-hidden-note-${g.name}`}
                    >
                      <strong>Note:</strong> hiding tools reduces the surface
                      exposed at the hub; it is <strong>not</strong> an
                      access-control boundary — daemon ports stay directly
                      reachable, and at gate-OFF the hub filter is not in the
                      path. Filter changes apply to new client sessions
                      (reconnect to apply).
                    </p>
                  )}
                </li>
              );
            })}
          </ul>
        )}
      </div>
    </section>
  );
}
