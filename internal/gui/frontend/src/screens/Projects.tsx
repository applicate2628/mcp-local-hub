// screens/Projects.tsx — the per-project lens (epic
// work-items/epics/2026-06-19-install-and-it-works-ux.md area 6).
//
// "Projects" is the project-centric complement to the global Servers matrix:
// where Servers is a server×client grid, Projects groups every per-project
// mechanism (workspace LSP rows, project-local client config, and groups) under
// ONE card per project path.
//
// PHASE 1 was strictly READ-ONLY and composed two endpoints client-side.
// PHASE 3b (this file) switches the data source to the SINGLE GET /api/projects
// aggregate (design decision 6) and wires the IMMEDIATE per-row toggle
// (work-items/decisions/2026-06-27-per-project-gui-p3b-uxdesign.md):
//   - LIST view (#/projects) is still fed from the same projects[] (now derived
//     from the aggregate, not /api/workspaces).
//   - DETAIL lens (#/projects?path=<key>) renders the 4 mechanism sections with
//     live toggles: [A] Workspace tools (scope workspace-lsp), [B] Project MCP
//     config (claude both-scopes §10.2 / cursor / vscode), [C] Group lens (scope
//     group-servers).
//
// SINGLE-OWNER DISPATCH (acceptance criterion 2): the frontend NEVER branches on
// a client name to pick a write owner. The ONLY scope logic is
// lib/project-toggle.ts `scopeForToggle`; the backend clients.ProjectToggleOwner
// turns (client, scope) into the actual writer.
//
// PER-ROW IMMEDIATE TOGGLE (decision 8 — the INVERSE of Servers.tsx; NO
// dirty-map/Apply): optimistic flip + per-row spinner → on 200 reconcile to
// response.enabled (NOT intent) + ✓ flash + quiet warnings → on non-2xx REVERT +
// row-scoped inline error (§3.1 code→copy map) + Retry. mountedRef-guards every
// post-await setState; double-click is debounced by disabling the in-flight
// control.
//
// Reuse map (cite file:line of each reused pattern):
//   - list-of-cards + empty-state + error+Retry → screens/Groups.tsx:300-408
//   - tools_hidden NOT-a-security-fence note (verbatim) → Groups.tsx:700-705
//   - stateShape() + state-ok/state-down/state-shape classes → Servers.tsx:1213-1221
//   - lsp-chip lsp-chip-via-hub/-direct/-legacy classes → Servers.tsx:1700-1715
//   - mountedRef async-guard → Servers.tsx:256-262
//   - readiness consent gate (ReadinessPanel + readinessBlockerCount + AddSecretModal)
//     → screens/Catalog.tsx:536-693 (the CatalogInstallGate single-owner gate)
//   - route?.query → URLSearchParams param parse → screens/Secrets.tsx:23-31

import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import type { RouterState } from "../hooks/useRouter";
import {
  getProjectsAggregate,
  getServerReadiness,
  toggleProjectServer,
  ProjectToggleError,
  type GroupDTO,
  type ProjectAggregateDTO,
  type ProjectToggleRequest,
  type ReadinessReport,
  type WorkspaceEntryDTO,
} from "../api";
import type { ProjectScopeInfo, ScanEntry } from "../types";
import { stateShape } from "../lib/status";
import {
  scopeForToggle,
  toggleErrorCopy,
} from "../lib/project-toggle";
import { ReadinessPanel, readinessBlockerCount } from "../components/ReadinessPanel";
import { AddSecretModal } from "../components/AddSecretModal";

// canonicalProjectKey is the SINGLE owner of the project join key (this file).
// A workspace_path is normalized into a canonical key used both to (a)
// deduplicate workspace entries into one card per project and (b) match the
// #/projects?path=<key> detail-lens param back to its project. The /api/projects
// aggregate already keys each project by clients.CanonicalProjectKey, so in P3b
// the detail lens matches on the backend `key` directly; this helper stays for
// the per-entry grouping fallback and is exported for unit tests.
//
// Normalization: filepath-clean (collapse `.`/`..`/duplicate separators) +
// forward-slash normalize (backslashes → `/`, strip a trailing slash). It never
// touches disk. CASE IS PRESERVED (a POSIX hub opened from a Windows browser
// would otherwise wrongly fold case-sensitive paths).
export function canonicalProjectKey(rawPath: string): string {
  let p = (rawPath ?? "").trim();
  if (p === "") return "";
  p = p.replace(/\\/g, "/");
  const isUNC = p.startsWith("//");
  const hasLeadingSlash = p.startsWith("/");
  const segments = p.split("/");
  const out: string[] = [];
  for (const seg of segments) {
    if (seg === "" || seg === ".") continue;
    if (seg === "..") {
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
  if (cleaned.length > 1 && cleaned.endsWith("/")) cleaned = cleaned.slice(0, -1);
  return cleaned;
}

// routingChipForBackend maps a workspace registry `backend` to one of the reused
// lsp-chip variants. Router-backed backends route through the hub LSP router →
// "via-hub"; a bare legacy direct-stdio entry is "direct"; the deprecated
// legacy-lsp backend is "legacy" (red). Unknown/blank → null (render "—"); an
// unknown non-empty backend must NOT be fabricated into a green via-hub chip.
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
// Servers glyph + color classes. A non-empty last_error always reads as Failed.
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
      return "Stopped";
  }
}

// Project is one row in the LIST view: a canonical key + the project's aggregate
// DTO (entries + scan + groups context). workspacePath is the operator's own
// casing (the registry's first-seen path).
interface Project {
  key: string;
  workspacePath: string;
  dto: ProjectAggregateDTO;
}

type LoadKind = "loading" | "ready" | "error";

interface LoadState {
  kind: LoadKind;
  projects: Project[];
  groups: GroupDTO[];
  groupsError: string | null;
  // error is the hard-fail message (the whole aggregate fetch threw).
  error: string | null;
}

const LOADING_STATE: LoadState = {
  kind: "loading",
  projects: [],
  groups: [],
  groupsError: null,
  error: null,
};

function asError(e: unknown): string {
  if (e instanceof Error) return e.message;
  return String(e);
}

export interface ProjectsScreenProps {
  // route carries ?path=<canonicalKey> for the detail lens. Read-only routing;
  // no onDirtyChange — P3b is per-row immediate, there is no dirty/Apply model.
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
    try {
      const agg = await getProjectsAggregate();
      if (!mountedRef.current) return;
      const projects: Project[] = agg.projects.map((dto) => ({
        key: dto.key,
        workspacePath: dto.workspace_path || dto.key,
        dto,
      }));
      setState({
        kind: "ready",
        projects,
        groups: agg.groups,
        groupsError: agg.groups_error ?? null,
        error: null,
      });
    } catch (e) {
      if (!mountedRef.current) return;
      setState({
        kind: "error",
        projects: [],
        groups: [],
        groupsError: null,
        error: asError(e),
      });
    }
  }

  useEffect(() => {
    void load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // selectedPath is the detail-lens param (mirrors Secrets.tsx:23-31).
  const selectedPath = useMemo(() => {
    const raw = new URLSearchParams(route?.query ?? "").get("path")?.trim();
    return raw && raw.length > 0 ? raw : null;
  }, [route?.query]);

  if (state.kind === "loading") {
    return (
      <section class="projects-screen" data-testid="projects-loading">
        <h1>Projects</h1>
        <p>Loading…</p>
      </section>
    );
  }

  if (state.kind === "error") {
    return (
      <section class="projects-screen" data-testid="projects-error">
        <h1>Projects</h1>
        <p class="settings-error" data-testid="projects-load-error" role="alert">
          Could not load projects: {state.error}
        </p>
        <button type="button" class="btn" onClick={() => void load()}>
          Retry
        </button>
      </section>
    );
  }

  if (selectedPath !== null) {
    const project = state.projects.find((p) => p.key === selectedPath) ?? null;
    return (
      // KEY by the selected project path (FIX 5): switching projects remounts the
      // detail tree so every per-row useToggleRow re-seeds from the new project's
      // initialEnabled — no stale toggle state leaks across same-named servers.
      <ProjectDetail
        key={selectedPath}
        projectKey={selectedPath}
        project={project}
        groups={state.groups}
        groupsError={state.groupsError}
        onReload={() => void load()}
      />
    );
  }

  return (
    <ProjectList
      projects={state.projects}
      groups={state.groups}
      groupsError={state.groupsError}
      onRetry={() => void load()}
    />
  );
}

// ProjectList renders the LIST view (#/projects): one card per project + an
// empty-state. Mirrors the Groups list-of-cards layout (Groups.tsx:356-408).
function ProjectList({
  projects,
  groups,
  groupsError,
  onRetry,
}: {
  projects: Project[];
  groups: GroupDTO[];
  groupsError: string | null;
  onRetry: () => void;
}): preact.JSX.Element {
  return (
    <section class="projects-screen" data-testid="projects-loaded">
      <h1>Projects</h1>
      <p class="m-0 mb-4 text-sm text-app-muted">
        Each card is one project (a registered workspace path). It composes the
        per-project mechanisms — workspace LSP tools, project-local client config,
        and groups — into one lens, so you manage a project&rsquo;s MCP servers
        per-project instead of hunting across the global matrix.
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

      {projects.length === 0 ? (
        <p class="empty-state" data-testid="projects-empty">
          No projects yet. Register a project to manage its MCP servers
          per-project.
        </p>
      ) : (
        <ul class="projects-list m-0 list-none p-0" data-testid="projects-list">
          {projects.map((p) => {
            const wsCount = p.dto.entries.length;
            // Groups are not yet path-bound (P3c), so every project lists ALL
            // groups; when the groups fetch FAILED render "? groups (load failed)"
            // rather than a misleading "0 groups".
            const grSummary =
              groupsError !== null
                ? "? groups (load failed)"
                : `${groups.length} group${groups.length === 1 ? "" : "s"}`;
            // The project scan may have failed for one project (root deleted)
            // while the rest render — surface a per-card hint.
            const cfgSummary = p.dto.scan_error
              ? "config unavailable"
              : `${p.dto.scan?.entries?.length ?? 0} config server${
                  (p.dto.scan?.entries?.length ?? 0) === 1 ? "" : "s"
                }`;
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
                  {wsCount} workspace tool{wsCount === 1 ? "" : "s"} ·{" "}
                  {cfgSummary} · {grSummary}
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
// mechanism badge for a detail section (the anti-confusion rule). NEUTRAL
// .projects-mechanism-badge class (theme-token gray) so it never misreads as a
// routing-chip value.
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

// ───────────────────────────────────────────────────────────────────
// The per-row toggle state machine (decision 8). Each row owns one RowToggleState
// and a controller (useToggleRow) that runs the optimistic-flip → POST →
// reconcile / revert cycle. mountedRef-guarded; double-click is debounced by the
// `busy` flag (the control is disabled while busy).
// ───────────────────────────────────────────────────────────────────

interface RowToggleState {
  // enabled is the displayed state — optimistic during a toggle, reconciled to
  // response.enabled after a 200.
  enabled: boolean;
  busy: boolean;
  // ✓ flash after a successful reconcile (cleared on the next interaction).
  flash: boolean;
  // error is the §3.1 plain-copy + behavior after a failed toggle; null = clean.
  error: { copy: ReturnType<typeof toggleErrorCopy>; code: string } | null;
  // warnings surfaced quietly after a successful toggle (e.g. a group republish
  // stale notice). Cleared on the next interaction.
  warnings: string[];
}

function initialRowState(enabled: boolean): RowToggleState {
  return { enabled, busy: false, flash: false, error: null, warnings: [] };
}

// useToggleRow owns ONE row's toggle lifecycle. `buildRequest(enable)` produces
// the POST body (the caller closes over root/client/scope/server/group/value).
// `onReloadRequest` is invoked when the §3.1 copy asks for a full reload
// (PROJECT_ROOT_INVALID) or a section reload (GROUP_NOT_FOUND). The hook is the
// single owner of the optimistic-flip → reconcile / revert discipline; no caller
// re-implements it.
function useToggleRow(
  initialEnabled: boolean,
  mountedRef: { current: boolean },
): {
  st: RowToggleState;
  toggle: (next: boolean, buildRequest: (enable: boolean) => ProjectToggleRequest) => Promise<void>;
} {
  // On a fresh aggregate reload the whole detail tree remounts (each row is keyed
  // by server name), so the hook is re-seeded from the new initialEnabled — there
  // is no external-reconcile path to clobber an in-flight toggle.
  const [st, setSt] = useState<RowToggleState>(() => initialRowState(initialEnabled));

  async function toggle(
    next: boolean,
    buildRequest: (enable: boolean) => ProjectToggleRequest,
  ): Promise<void> {
    // Debounce double-click: ignore while a toggle is already in flight.
    let skip = false;
    setSt((s) => {
      if (s.busy) {
        skip = true;
        return s;
      }
      // Optimistic flip + spinner; clear any prior feedback.
      return { enabled: next, busy: true, flash: false, error: null, warnings: [] };
    });
    if (skip) return;

    const prev = !next; // the state we revert to on failure
    try {
      const res = await toggleProjectServer(buildRequest(next));
      if (!mountedRef.current) return;
      setSt({
        // Reconcile to the PERSISTED state, not the requested intent (idempotent/
        // clamp self-corrects).
        enabled: res.enabled,
        busy: false,
        flash: true,
        error: null,
        warnings: res.warnings ?? [],
      });
    } catch (e) {
      if (!mountedRef.current) return;
      const code = e instanceof ProjectToggleError ? e.code : "PROJECT_TOGGLE_FAILED";
      setSt({
        enabled: prev, // REVERT the optimistic flip
        busy: false,
        flash: false,
        error: { copy: toggleErrorCopy(code), code },
        warnings: [],
      });
    }
  }

  return { st, toggle };
}

// ToggleControl renders the immediate per-row toggle + its feedback (spinner, ✓
// flash, row-scoped inline error + Retry, quiet warnings). data-testid is keyed
// by scope + server per the spec (projects-toggle-<scope>-<server>).
function ToggleControl({
  testId,
  scope,
  server,
  st,
  label,
  disabled,
  title,
  onToggle,
  onRetry,
}: {
  testId: string;
  scope: string;
  server: string;
  st: RowToggleState;
  label?: string;
  disabled?: boolean;
  // title is a hover/aria-description surfaced on the checkbox itself — used for
  // the object-member disable-affordance warning (FIX 6: "Disabling removes this
  // entry; re-enabling will require re-adding it.").
  title?: string;
  onToggle: (next: boolean) => void;
  onRetry: () => void;
}): preact.JSX.Element {
  // PROJECT_TOGGLE_UNSUPPORTED hides the control (managed in the client).
  if (st.error?.copy.hideToggle) {
    return (
      <div class="projects-toggle-cell" data-testid={`projects-toggle-unsupported-${scope}-${server}`}>
        <span class="settings-error text-xs" role="alert" title={st.error.code}>
          {st.error.copy.message}
        </span>
      </div>
    );
  }
  return (
    <div class="projects-toggle-cell">
      <label class="projects-toggle-label" title={title}>
        <input
          type="checkbox"
          data-testid={testId}
          checked={st.enabled}
          disabled={disabled || st.busy}
          aria-busy={st.busy}
          title={title}
          aria-description={title}
          onChange={(e) => onToggle((e.target as HTMLInputElement).checked)}
        />
        {label ? <span class="text-xs">{label}</span> : null}
        {st.busy && (
          <span class="projects-toggle-spinner" data-testid={`projects-toggle-spinner-${scope}-${server}`} aria-hidden="true">
            ⏳
          </span>
        )}
        {st.flash && !st.busy && (
          <span class="projects-toggle-flash" data-testid={`projects-toggle-ok-${scope}-${server}`} aria-hidden="true">
            ✓
          </span>
        )}
      </label>
      {st.warnings.length > 0 && (
        <ul class="projects-toggle-warnings m-0 list-none p-0" data-testid={`projects-toggle-warn-${scope}-${server}`}>
          {st.warnings.map((wn, i) => (
            <li key={i} class="text-xs text-app-muted">
              ⚠ {wn}
            </li>
          ))}
        </ul>
      )}
      {st.error && !st.error.copy.hideToggle && (
        <div class="projects-toggle-error" data-testid={`projects-toggle-error-${scope}-${server}`} role="alert">
          {/* Raw code lives ONLY in the tooltip, never on the visible row. */}
          <span class="settings-error text-xs" title={st.error.code}>
            {st.error.copy.message}
          </span>
          {st.error.copy.retry && (
            <button
              type="button"
              class="btn text-xs"
              data-testid={`projects-toggle-retry-${scope}-${server}`}
              onClick={onRetry}
            >
              Retry
            </button>
          )}
        </div>
      )}
    </div>
  );
}

// ───────────────────────────────────────────────────────────────────
// Consent-on-enable gate (decision 7 — reuse, no fork). On an enable for an
// object-member / claude-local / group server, GET /api/server/readiness; if a
// report comes back WITH blockers, show the gate (Confirm disabled while
// readinessBlockerCount > 0 — the SAME predicate Catalog uses). O-1: if NO report
// is obtainable (404 / fetch error — a bare project-only object-member with no
// global manifest), SKIP the gate and proceed (the backend toggle is
// authoritative). Disable never gates.
// ───────────────────────────────────────────────────────────────────

type ConsentResolution =
  | { kind: "proceed" } // readiness ready, or no report (O-1), or fetch error (advisory)
  | { kind: "gate"; report: ReadinessReport }; // a report WITH blockers → gate

// ProjectConsentGate renders the readiness panel for ONE server's enable. It
// mirrors Catalog's CatalogInstallGate single-owner predicate. On Confirm it
// calls onConfirm (the actual toggle); on Cancel it closes without toggling.
function ProjectConsentGate({
  server,
  report,
  onConfirm,
  onCancel,
}: {
  server: string;
  report: ReadinessReport;
  onConfirm: () => void;
  onCancel: () => void;
}): preact.JSX.Element {
  const [reloadToken, setReloadToken] = useState(0);
  const [liveReport, setLiveReport] = useState<ReadinessReport>(report);
  const [loading, setLoading] = useState(false);
  const [secretModalKey, setSecretModalKey] = useState<string | null>(null);
  const mountedRef = useRef(true);
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  // Re-fetch readiness after an inline secret is set so a just-set secret flips
  // the row advisory→satisfied and clears any blocker.
  useEffect(() => {
    if (reloadToken === 0) return; // first render uses the report we were handed
    let cancelled = false;
    setLoading(true);
    getServerReadiness(server)
      .then((rep) => {
        if (cancelled || !mountedRef.current) return;
        setLiveReport(rep);
      })
      .catch(() => {
        // A re-fetch error is advisory (the initial gate already had a report);
        // keep the last report so the operator can still Confirm.
      })
      .finally(() => {
        if (!cancelled && mountedRef.current) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [server, reloadToken]);

  const blockers = readinessBlockerCount(liveReport);
  const confirmDisabled = loading || blockers > 0;

  return (
    <div class="projects-consent-gate" data-testid={`projects-consent-gate-${server}`}>
      <p class="m-0 mb-1 text-xs text-app-muted">
        Enabling <code>{server}</code> for this project — check readiness first:
      </p>
      <ReadinessPanel
        report={liveReport}
        loading={loading}
        error={null}
        inlineSecrets={{}}
        onInlineSecretChange={() => {}}
        renderSecretAction={(key) => (
          <div class="readiness-secret-actions" data-testid={`projects-secret-actions-${key}`}>
            <button
              type="button"
              class="btn btn-secondary"
              data-testid={`projects-secret-set-${key}`}
              onClick={() => setSecretModalKey(key)}
            >
              Set {key}
            </button>
            <a
              class="readiness-secret-deeplink"
              href={`#/secrets?key=${encodeURIComponent(key)}`}
              data-testid={`projects-secret-open-secrets-${key}`}
            >
              Open Secrets
            </a>
          </div>
        )}
      />
      <div class="projects-consent-actions">
        <button
          type="button"
          class="btn btn-secondary"
          data-testid={`projects-consent-confirm-${server}`}
          disabled={confirmDisabled}
          onClick={onConfirm}
        >
          {blockers > 0
            ? `Fix ${blockers} blocker${blockers === 1 ? "" : "s"} to enable`
            : "Enable"}
        </button>
        <button
          type="button"
          class="btn btn-secondary"
          data-testid={`projects-consent-cancel-${server}`}
          onClick={onCancel}
        >
          Cancel
        </button>
      </div>

      <AddSecretModal
        key={secretModalKey ?? "closed"}
        open={secretModalKey !== null}
        prefillName={secretModalKey ?? undefined}
        onClose={() => setSecretModalKey(null)}
        onSaved={() => {
          if (mountedRef.current) setReloadToken((n) => n + 1);
        }}
      />
    </div>
  );
}

// resolveConsentOnEnable runs the O-1 readiness decision for an enable. It GETs
// readiness; a report with blockers → gate; a ready report → proceed; a 404 / any
// fetch error (no obtainable report — a bare project-only object-member with no
// global manifest, decision O-1) → proceed (backend authoritative). It NEVER
// gates a disable (the caller only calls this on enable).
async function resolveConsentOnEnable(server: string): Promise<ConsentResolution> {
  try {
    const report = await getServerReadiness(server);
    if (readinessBlockerCount(report) > 0) {
      return { kind: "gate", report };
    }
    return { kind: "proceed" };
  } catch {
    // O-1 / advisory: no report obtainable (404 no-manifest) or a transport
    // error → do NOT block; the backend toggle stays authoritative.
    return { kind: "proceed" };
  }
}

// ───────────────────────────────────────────────────────────────────
// Toggleable-server-row — the shared building block for the object-member,
// claude-local-array, and group sections. Owns its toggle state machine, the
// cold re-enable affordance, and the consent-on-enable gate.
//
// RE-ENABLE IS ALWAYS COLD FOR AN OBJECT-MEMBER SUBSTRATE (P3b r2, FIX 2). The
// warm value-replay path was REMOVED: the only legitimate warm source would have
// been the just-disabled member value, but the /api/projects aggregate NILs every
// ClientPresence.raw (sanitizeScanResult / stripClientEntryRaw — the strip-Raw
// security posture that keeps secret-bearing config off the wire). So the held
// value was ALWAYS undefined and the warm path was dead code. For an object-member
// (cursor / vscode, scope project-object-member) DISABLE removes the member and
// re-enable ALWAYS renders the cold "Re-add…" CTA → #/add-server (never a
// value-less enable POST, never a backend-echoed value). The claude Project row
// is NOT an object-member here — it uses the value-free APPROVAL ARRAY-MOVE
// (scope claude-local-membership), whose .mcp.json definition stays put, so its
// re-enable is trivial (no value, no cold CTA): coldReenable=false.
// ───────────────────────────────────────────────────────────────────

interface ServerRowProps {
  server: string;
  scope: ReturnType<typeof scopeForToggle>;
  initialEnabled: boolean;
  // buildRequest closes over the substrate identity (root/client/group). No value
  // is ever carried — value-free substrates (workspace / claude-local array /
  // group) need none, and object-member ENABLE is cold (Re-add CTA), never a
  // value-bearing enable POST.
  buildRequest: (enable: boolean) => ProjectToggleRequest;
  // coldReenable: object-member rows (cursor / vscode, scope project-object-member)
  // DELETE the member on disable, so a disabled row cannot be toggled back on —
  // it renders the cold "Re-add…" CTA instead. Value-free substrates (workspace /
  // claude-local array / group) set this false: their disable is reversible by a
  // plain enable toggle.
  coldReenable: boolean;
  // gateOnEnable: object-member / claude-local / group rows run the readiness
  // consent gate on enable; workspace rows do not (LSP register has its own flow).
  gateOnEnable: boolean;
  onReload: () => void;
  mountedRef: { current: boolean };
  testIdPrefix?: string;
}

function ToggleableServerRow(props: ServerRowProps): preact.JSX.Element {
  const {
    server,
    scope,
    initialEnabled,
    buildRequest,
    coldReenable: coldReenableSubstrate,
    gateOnEnable,
    onReload,
    mountedRef,
  } = props;
  const { st, toggle } = useToggleRow(initialEnabled, mountedRef);
  // pendingConsent holds the readiness report while the gate is open (null = no
  // gate). lastIntentRef remembers the toggle target so Retry replays it.
  const [pendingConsent, setPendingConsent] = useState<ReadinessReport | null>(null);
  const lastIntentRef = useRef<boolean>(initialEnabled);
  const toggleTestId = `projects-toggle-${scope}-${server}`;

  // doToggle is the single entry into the state machine. An enable first resolves
  // consent (O-1); a disable just runs (never gates). No value is ever carried.
  async function doToggle(next: boolean): Promise<void> {
    lastIntentRef.current = next;
    if (!next) {
      // Disable never gates.
      await toggle(false, (en) => buildRequest(en));
      return;
    }
    // ENABLE.
    if (gateOnEnable) {
      const resolution = await resolveConsentOnEnable(server);
      if (!mountedRef.current) return;
      if (resolution.kind === "gate") {
        setPendingConsent(resolution.report);
        return; // wait for Confirm/Cancel
      }
    }
    await toggle(true, (en) => buildRequest(en));
  }

  function onConfirmConsent(): void {
    setPendingConsent(null);
    void toggle(true, (en) => buildRequest(en));
  }

  function onCancelConsent(): void {
    setPendingConsent(null);
  }

  function onRetry(): void {
    // Replay the last intent through the full path (re-runs consent for an enable).
    void doToggle(lastIntentRef.current);
  }

  // Cold re-enable CTA: an object-member substrate whose member was removed on
  // disable cannot be toggled back on — render a Re-add… CTA routing to the
  // Add-server / Catalog flow instead (never a value-less enable POST; CORE
  // RULING / D2). Value-free substrates never hit this.
  //
  // GATE ON !busy: while a disable is IN FLIGHT (optimistic OFF + spinner), keep
  // the toggle visible so the operator sees the spinner and an error-revert can
  // bring the control straight back. Swap to the Re-add CTA only once the disable
  // has RECONCILED (busy cleared, st.enabled settled false) — i.e. the member is
  // confirmed gone, not merely optimistically flipped.
  const coldReenable = coldReenableSubstrate && !st.enabled && !st.busy;

  return (
    <li class="projects-server-row py-1.5" data-testid={`projects-server-${scope}-${server}`} data-server={server}>
      <span class="projects-server-name text-sm font-medium text-app-text">{server}</span>
      {coldReenable ? (
        <a
          class="btn text-xs"
          href={`#/add-server`}
          data-testid={`projects-readd-${server}`}
          title="Disabling removed this entry; re-add this server from the Add-server / Catalog flow to enable it again."
        >
          Re-add…
        </a>
      ) : (
        <ToggleControl
          testId={toggleTestId}
          scope={scope}
          server={server}
          st={st}
          disabled={pendingConsent !== null}
          // The disable-affordance warning (P3b r2, FIX 6): an object-member
          // disable removes the entry, and re-enable is cold (Re-add). Surface
          // that on the live (ON) control so the operator knows the cost BEFORE
          // clicking. Value-free substrates pass no title.
          title={
            coldReenableSubstrate
              ? "Disabling removes this entry; re-enabling will require re-adding it."
              : undefined
          }
          onToggle={(next) => void doToggle(next)}
          onRetry={onRetry}
        />
      )}
      {pendingConsent !== null && (
        <ProjectConsentGate
          server={server}
          report={pendingConsent}
          onConfirm={onConfirmConsent}
          onCancel={onCancelConsent}
        />
      )}
      {st.error?.copy.reload && (
        <button
          type="button"
          class="btn text-xs"
          data-testid={`projects-toggle-reload-${scope}-${server}`}
          onClick={onReload}
        >
          Reload
        </button>
      )}
    </li>
  );
}

// ───────────────────────────────────────────────────────────────────
// ProjectDetail — the DETAIL lens (#/projects?path=<key>). 4 mechanism sections,
// all fed from the SINGLE aggregate DTO.
//
// RE-SEED ON PROJECT CHANGE (P3b r2, FIX 5): ProjectDetail is KEYED by projectKey
// at the ProjectsScreen call site, so navigating between two projects remounts the
// whole detail tree. Each per-row useToggleRow then re-seeds from the new
// initialEnabled, so a same-named server with a different enabled state in the
// next project never shows the previous project's stale toggle state.
// ───────────────────────────────────────────────────────────────────

function ProjectDetail({
  projectKey,
  project,
  groups,
  groupsError,
  onReload,
}: {
  projectKey: string;
  project: Project | null;
  groups: GroupDTO[];
  groupsError: string | null;
  onReload: () => void;
}): preact.JSX.Element {
  const displayPath = project?.workspacePath ?? projectKey;
  const dto = project?.dto ?? null;
  const scan = dto?.scan ?? null;
  const scanError = dto?.scan_error ?? null;
  const root = project?.workspacePath ?? projectKey;

  const mountedRef = useRef(true);
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  return (
    <section class="projects-screen" data-testid="projects-detail">
      <div class="mb-1 flex items-center justify-between gap-3">
        <h1 class="m-0">{displayPath}</h1>
        <a class="btn text-xs" href="#/projects" data-testid="projects-back">
          ← All projects
        </a>
      </div>
      <p class="m-0 mb-4 text-xs text-app-muted">
        <a href="#/servers" data-testid="projects-servers-crosslink">
          Open the global Servers matrix →
        </a>
      </p>

      <SectionWorkspace
        entries={dto?.entries ?? []}
        root={root}
        onReload={onReload}
        mountedRef={mountedRef}
      />

      <SectionProjectConfig
        scan={scan}
        scanError={scanError}
        root={root}
        onReload={onReload}
        mountedRef={mountedRef}
      />

      <SectionGroups
        groups={groups}
        groupsError={groupsError}
        onReload={onReload}
        mountedRef={mountedRef}
      />
    </section>
  );
}

// ───────────────────────────────────────────────────────────────────
// [A] Workspace tools — backed by workspaces.yaml. The Enabled toggle column
// registers / unregisters the workspace LSP daemon (scope workspace-lsp). Each
// row toggles ALL of its languages (the registry groups by language; a per-row
// toggle drives api.Register/Unregister for that language).
// ───────────────────────────────────────────────────────────────────

function SectionWorkspace({
  entries,
  root,
  onReload,
  mountedRef,
}: {
  entries: WorkspaceEntryDTO[];
  root: string;
  onReload: () => void;
  mountedRef: { current: boolean };
}): preact.JSX.Element {
  return (
    <div class="card mb-3" data-testid="projects-section-workspace">
      <div class="card-title flex items-center justify-between gap-3">
        <span>Workspace tools</span>
        <MechanismBadge label="workspace" backedBy="workspaces.yaml" testId="projects-badge-workspace" />
      </div>
      {entries.length === 0 ? (
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
              <th>Enabled</th>
            </tr>
          </thead>
          <tbody>
            {entries.map((entry) => (
              <WorkspaceRow
                key={`${entry.workspace_key}-${entry.language}`}
                entry={entry}
                root={root}
                onReload={onReload}
                mountedRef={mountedRef}
              />
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

function WorkspaceRow({
  entry,
  root,
  onReload,
  mountedRef,
}: {
  entry: WorkspaceEntryDTO;
  root: string;
  onReload: () => void;
  mountedRef: { current: boolean };
}): preact.JSX.Element {
  const st = lifecycleState(entry);
  const chip = routingChipForBackend(entry.backend);
  const clientList = Object.keys(entry.client_entries ?? {}).sort();
  // EVERY rendered workspace row is a REGISTERED registry entry — it only appears
  // here because the registry holds it — so it is "enabled" for the toggle's
  // purpose: the first click DISABLES (Unregister/cleanup). That includes a
  // Failed/missing entry (FIX 3): a broken LSP registration is still registered
  // and the operator must be able to clean it up. Seeding such a row OFF would
  // make the first click an enable/Register (a no-op churn) instead of the
  // Unregister the operator needs. So initialEnabled is true for every state.
  const initialEnabled = true;
  const scope = scopeForToggle("workspace");

  function buildRequest(enable: boolean): ProjectToggleRequest {
    return {
      root,
      scope,
      server: entry.language, // Model A keys off languages, not a server name
      enable,
      languages: [entry.language],
    };
  }

  return (
    <tr data-testid={`projects-workspace-row-${entry.language}`}>
      <td>
        <code>{entry.language}</code>
      </td>
      <td>{entry.backend || "—"}</td>
      <td class="projects-routing-cell">
        {chip === null ? (
          <span class="lsp-cell-empty">—</span>
        ) : clientList.length === 0 ? (
          <span class={`lsp-chip lsp-chip-${chip}`} data-testid={`projects-chip-${entry.language}`}>
            {chip}
          </span>
        ) : (
          clientList.map((client) => (
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
        title={(entry.last_error ?? "").trim() || undefined}
      >
        <span class="state-shape" aria-hidden="true">
          {stateShape(st)}
        </span>{" "}
        {st}
      </td>
      <td>
        <WorkspaceToggleCell
          language={entry.language}
          scope={scope}
          initialEnabled={initialEnabled}
          buildRequest={buildRequest}
          onReload={onReload}
          mountedRef={mountedRef}
        />
      </td>
    </tr>
  );
}

function WorkspaceToggleCell({
  language,
  scope,
  initialEnabled,
  buildRequest,
  onReload,
  mountedRef,
}: {
  language: string;
  scope: ReturnType<typeof scopeForToggle>;
  initialEnabled: boolean;
  buildRequest: (enable: boolean) => ProjectToggleRequest;
  onReload: () => void;
  mountedRef: { current: boolean };
}): preact.JSX.Element {
  const { st, toggle } = useToggleRow(initialEnabled, mountedRef);
  const lastIntentRef = useRef<boolean>(initialEnabled);
  function doToggle(next: boolean): void {
    lastIntentRef.current = next;
    void toggle(next, (en) => buildRequest(en));
  }
  return (
    <>
      <ToggleControl
        testId={`projects-toggle-${scope}-${language}`}
        scope={scope}
        server={language}
        st={st}
        onToggle={doToggle}
        onRetry={() => doToggle(lastIntentRef.current)}
      />
      {st.error?.copy.reload && (
        <button
          type="button"
          class="btn text-xs"
          data-testid={`projects-toggle-reload-${scope}-${language}`}
          onClick={onReload}
        >
          Reload
        </button>
      )}
    </>
  );
}

// ───────────────────────────────────────────────────────────────────
// [B] Project MCP config — per-client sub-cards. claude-code is the dual-scope
// card (§10.2); cursor + vscode are flat live-toggle lists. Each is fed from the
// project scan's per-entry ClientPresence + the ProjectScope (claude Local).
// ───────────────────────────────────────────────────────────────────

// claudeEntriesOf returns the claude-code .mcp.json (Project-scope) entries from
// the scan — every entry that has a "claude-code" presence. shadowed entries are
// flagged; non-shadowed entries carry ProjectEnabled.
function entriesForClient(scan: ScanEntry[] | null | undefined, client: string): ScanEntry[] {
  return (scan ?? []).filter((e) => e.client_presence && client in e.client_presence);
}

function SectionProjectConfig({
  scan,
  scanError,
  root,
  onReload,
  mountedRef,
}: {
  scan: import("../types").ScanResult | null;
  scanError: string | null;
  root: string;
  onReload: () => void;
  mountedRef: { current: boolean };
}): preact.JSX.Element {
  const entries = scan?.entries ?? [];
  const claudeEntries = entriesForClient(entries, "claude-code");
  const cursorEntries = entriesForClient(entries, "cursor");
  const vscodeEntries = entriesForClient(entries, "vscode");
  // A claude Local-only project (FIX 4): the backend sets ProjectScope
  // (local_servers / approval arrays) INDEPENDENTLY of the scan entries, so a
  // project whose ONLY project config is the claude Local scope has zero scan
  // entries but a non-empty project_scope. Render the claude both-scopes card for
  // it (its Local subsection is the whole content) rather than the "no config"
  // empty-state.
  const projectScope = scan?.project_scope ?? null;
  const hasClaudeLocalScope =
    (projectScope?.local_servers?.length ?? 0) > 0 ||
    (projectScope?.enabled_mcpjson_servers?.length ?? 0) > 0 ||
    (projectScope?.disabled_mcpjson_servers?.length ?? 0) > 0 ||
    projectScope?.enable_all_project_mcp_servers === true;
  const noConfig =
    !scanError &&
    claudeEntries.length === 0 &&
    cursorEntries.length === 0 &&
    vscodeEntries.length === 0 &&
    !hasClaudeLocalScope;

  return (
    <div class="card mb-3" data-testid="projects-section-config">
      <div class="card-title flex items-center justify-between gap-3">
        <span>Project MCP config</span>
        <MechanismBadge label="project-config" backedBy=".mcp.json" testId="projects-badge-config" />
      </div>

      {scanError !== null ? (
        // Section-scoped scan error — the project root couldn't be read/scanned.
        <p class="settings-error" data-testid="projects-section-config-error" role="alert">
          {scanError === "PROJECT_ROOT_INVALID"
            ? "This project's root could not be read — it may have moved or been deleted."
            : "The project config could not be scanned."}{" "}
          <button type="button" class="btn text-xs" onClick={onReload}>
            Reload
          </button>
        </p>
      ) : noConfig ? (
        <p class="m-0 text-sm text-app-muted" data-testid="projects-config-empty">
          No project-local MCP config found for this project (cursor / vscode /
          claude-code). Add one in your client or via Add server.
        </p>
      ) : (
        <>
          <ClaudeBothScopesCard
            entries={claudeEntries}
            projectScope={projectScope}
            root={root}
            onReload={onReload}
            mountedRef={mountedRef}
          />
          <FlatClientCard
            client="cursor"
            label="Cursor"
            backedBy=".cursor/mcp.json"
            entries={cursorEntries}
            root={root}
            onReload={onReload}
            mountedRef={mountedRef}
          />
          <FlatClientCard
            client="vscode"
            label="VS Code"
            backedBy=".vscode/mcp.json"
            entries={vscodeEntries}
            root={root}
            onReload={onReload}
            mountedRef={mountedRef}
          />
        </>
      )}
    </div>
  );
}

// ClaudeBothScopesCard renders the §10.2 dual-substrate claude card: a Project
// subsection (live toggles, scope claude-local-membership — the APPROVAL
// ARRAY-MOVE, NOT a .mcp.json member delete) + a Local READ-ONLY subsection (no
// write owner in P3b v1 — D1). A shadowed entry is rendered ONCE Local-owned: the
// Project subsection shows a muted ⊘ cross-reference anchor, the Local subsection
// shows the authoritative row.
//
// SCOPE (P3b r2, FIX 1): the claude checked-in .mcp.json Project toggle uses
// mechanism "claude-local" → scope claude-local-membership, which MOVES the
// server name between ~/.claude.json projects.<key>.{enabled,disabled}McpjsonServers
// and NEVER deletes the .mcp.json mcpServers definition (decision 5). It is NOT
// the object-member member-delete substrate (scope project-object-member) — that
// would (a) data-loss the shared checked-in def on disable and (b) spring back ON
// on reload because the approval arrays were never touched. Because the def stays
// put, re-enable is trivial (a value-free array flip), so coldReenable=false: NO
// Re-add CTA and NO value on the wire. Only cursor / vscode are object-member.
function ClaudeBothScopesCard({
  entries,
  projectScope,
  root,
  onReload,
  mountedRef,
}: {
  entries: ScanEntry[];
  projectScope: ProjectScopeInfo | null;
  root: string;
  onReload: () => void;
  mountedRef: { current: boolean };
}): preact.JSX.Element {
  const localServers = projectScope?.local_servers ?? [];
  const projectEntries = entries; // claude-code .mcp.json (Project scope)
  // The claude Project toggle is the APPROVAL ARRAY-MOVE (claude-local-membership),
  // a value-free substrate — never the project-object-member member delete.
  const scope = scopeForToggle("claude-local");

  return (
    <div class="projects-client-card" data-testid="projects-client-claude-code">
      <div class="projects-client-card-head flex items-center justify-between gap-2">
        <span class="text-sm font-medium text-app-text">Claude Code</span>
        <span class="text-xs text-app-muted">two scopes</span>
      </div>

      {/* ── Project subsection (.mcp.json — checked-in, shared) ── */}
      <div class="projects-claude-subsection" data-testid="projects-claude-project">
        <p class="m-0 text-xs text-app-muted">
          <strong>Project</strong> — <code>.mcp.json</code> (checked-in, shared).
          Toggles approve/disapprove the server for this project (the{" "}
          <code>.mcp.json</code> definition stays put — only your approval moves).
        </p>
        {projectEntries.length === 0 ? (
          <p class="m-0 text-sm text-app-muted" data-testid="projects-claude-project-empty">
            No <code>.mcp.json</code> servers.
          </p>
        ) : (
          <ul class="projects-server-list m-0 list-none p-0">
            {projectEntries.map((entry) => {
              const shadowed = entry.project_shadowed_by_local === true;
              if (shadowed) {
                // Rendered ONCE Local-owned — the Project subsection shows a muted
                // ⊘ cross-reference, never a competing toggle.
                return (
                  <li
                    key={entry.name}
                    class="projects-server-row projects-shadow-muted py-1.5"
                    data-testid={`projects-shadow-${entry.name}`}
                    data-server={entry.name}
                  >
                    <span class="text-sm text-app-muted" aria-hidden="true">⊘</span>{" "}
                    <span class="text-sm text-app-muted">{entry.name}</span>
                    <span class="ml-2 text-xs text-app-muted">
                      shadows the <code>.mcp.json</code> entry → see Local below
                    </span>
                  </li>
                );
              }
              // ProjectEnabled==null = anomaly → disabled toggle + "state unknown",
              // never guess ON.
              if (entry.project_enabled === undefined) {
                return (
                  <li
                    key={entry.name}
                    class="projects-server-row py-1.5"
                    data-testid={`projects-claude-unknown-${entry.name}`}
                    data-server={entry.name}
                  >
                    <span class="text-sm text-app-text">{entry.name}</span>
                    <span class="ml-2 text-xs text-app-muted" title="approval state unknown">
                      (state unknown)
                    </span>
                  </li>
                );
              }
              return (
                <ToggleableServerRow
                  key={entry.name}
                  server={entry.name}
                  scope={scope}
                  initialEnabled={entry.project_enabled === true}
                  // Array-move substrate: value-free, reversible by a plain toggle.
                  // The .mcp.json def is never deleted (decision 5), so a disabled
                  // server re-enables by flipping the approval array — no Re-add CTA.
                  coldReenable={false}
                  gateOnEnable={true}
                  onReload={onReload}
                  mountedRef={mountedRef}
                  buildRequest={(enable) => ({
                    root,
                    client: "claude-code",
                    scope,
                    server: entry.name,
                    enable,
                  })}
                />
              );
            })}
          </ul>
        )}
      </div>

      {/* ── Local subsection (~/.claude.json — private, READ-ONLY in P3b v1) ── */}
      <div class="projects-claude-subsection" data-testid="projects-claude-local">
        <p class="m-0 text-xs text-app-muted">
          <strong>Local</strong> — <code>~/.claude.json</code> (private to you).
          Read-only here — Local takes precedence over Project (Claude loads the
          Local definition). Manage it in Claude.
        </p>
        {localServers.length === 0 ? (
          <p class="m-0 text-sm text-app-muted" data-testid="projects-claude-local-empty">
            No Local-scope servers for this project.
          </p>
        ) : (
          <ul class="projects-server-list m-0 list-none p-0" data-testid="projects-claude-local-list">
            {localServers.map((name) => {
              const isShadow = projectEntries.some(
                (e) => e.name === name && e.project_shadowed_by_local === true,
              );
              return (
                <li
                  key={name}
                  class="projects-server-row py-1.5"
                  data-testid={`projects-claude-local-${name}`}
                  data-server={name}
                >
                  <span class="text-sm text-app-text">{name}</span>
                  <span class="ml-2 text-xs text-app-muted">read-only</span>
                  {isShadow && (
                    <span
                      class="ml-2 text-xs text-app-muted"
                      data-testid={`projects-shadow-authoritative-${name}`}
                    >
                      shadows the Project <code>.mcp.json</code> &lsquo;{name}&rsquo; entry
                    </span>
                  )}
                </li>
              );
            })}
          </ul>
        )}
        <ClaudeRawApprovalState projectScope={projectScope} />
      </div>
    </div>
  );
}

// ClaudeRawApprovalState is the collapsible "Advanced: raw approval state" that
// shows the verbatim enabled/disabled toggle arrays + the enable-all flag (§10.2).
function ClaudeRawApprovalState({
  projectScope,
}: {
  projectScope: ProjectScopeInfo | null;
}): preact.JSX.Element | null {
  if (!projectScope) return null;
  const disabled = projectScope.disabled_mcpjson_servers ?? [];
  const enabled = projectScope.enabled_mcpjson_servers ?? [];
  const enableAll = projectScope.enable_all_project_mcp_servers === true;
  if (disabled.length === 0 && enabled.length === 0 && !enableAll) return null;
  return (
    <details class="projects-claude-raw" data-testid="projects-claude-raw">
      <summary class="text-xs text-app-muted">Advanced: raw approval state</summary>
      <ul class="m-0 list-none p-0 text-xs text-app-muted">
        <li>enableAllProjectMcpServers: {enableAll ? "true" : "false"}</li>
        <li>enabledMcpjsonServers: {enabled.length ? enabled.join(", ") : "—"}</li>
        <li>disabledMcpjsonServers: {disabled.length ? disabled.join(", ") : "—"}</li>
      </ul>
    </details>
  );
}

// FlatClientCard renders a single-substrate client (cursor / vscode) as a flat
// live-toggle list (scope project-object-member). Each row is an object-member:
// DISABLE removes the member from the project config, so re-enable is ALWAYS COLD
// (the Re-add… CTA — P3b r2, FIX 2). No value is ever held or sent (the aggregate
// is NAMES-only — sanitizeScanResult NILs every raw blob).
function FlatClientCard({
  client,
  label,
  backedBy,
  entries,
  root,
  onReload,
  mountedRef,
}: {
  client: string;
  label: string;
  backedBy: string;
  entries: ScanEntry[];
  root: string;
  onReload: () => void;
  mountedRef: { current: boolean };
}): preact.JSX.Element | null {
  if (entries.length === 0) return null;
  const scope = scopeForToggle("object-member");
  return (
    <div class="projects-client-card" data-testid={`projects-client-${client}`}>
      <div class="projects-client-card-head flex items-center justify-between gap-2">
        <span class="text-sm font-medium text-app-text">{label}</span>
        <span class="text-xs text-app-muted">
          backed by <code>{backedBy}</code>
        </span>
      </div>
      <ul class="projects-server-list m-0 list-none p-0">
        {entries.map((entry) => (
          // For a single-substrate object-member, presence in the scan means the
          // member exists (enabled). A removed member won't appear in entries at
          // all, so initialEnabled is true for every rendered row. coldReenable is
          // true: disabling removes the member, so a re-enable routes to the
          // Re-add CTA rather than a value-less enable POST (CORE RULING / D2).
          <ToggleableServerRow
            key={entry.name}
            server={entry.name}
            scope={scope}
            initialEnabled={true}
            coldReenable={true}
            gateOnEnable={true}
            onReload={onReload}
            mountedRef={mountedRef}
            buildRequest={(enable) => ({
              root,
              client,
              scope,
              server: entry.name,
              enable,
            })}
          />
        ))}
      </ul>
    </div>
  );
}

// ───────────────────────────────────────────────────────────────────
// [C] Group lens — backed by groups.yaml. Per-server toggle (scope group-servers)
// adds/removes a server from the group's servers list. Keeps the P1 "tools_hidden
// not a security fence" + "not yet project-bound" copy until P3c.
// ───────────────────────────────────────────────────────────────────

function SectionGroups({
  groups,
  groupsError,
  onReload,
  mountedRef,
}: {
  groups: GroupDTO[];
  groupsError: string | null;
  onReload: () => void;
  mountedRef: { current: boolean };
}): preact.JSX.Element {
  return (
    <div class="card mb-3" data-testid="projects-section-groups">
      <div class="card-title flex items-center justify-between gap-3">
        <span>Group lens</span>
        <MechanismBadge label="group" backedBy="groups.yaml" testId="projects-badge-groups" />
      </div>
      <p class="m-0 mb-2 text-xs text-app-muted">
        Groups are not yet bound to a project path (coming later), so this lists
        every group.{" "}
        <a href="#/groups" data-testid="projects-manage-groups">
          manage in Groups →
        </a>
      </p>
      {groupsError !== null ? (
        <p class="settings-error" data-testid="projects-section-groups-error" role="alert">
          Could not load groups: {groupsError}{" "}
          <button type="button" class="btn text-xs" onClick={onReload}>
            Retry
          </button>
        </p>
      ) : groups.length === 0 ? (
        <p class="m-0 text-sm text-app-muted" data-testid="projects-groups-empty">
          No groups defined.
        </p>
      ) : (
        <ul class="projects-groups-list m-0 list-none p-0" data-testid="projects-groups-list">
          {groups.map((g) => (
            <GroupCard key={g.name} group={g} onReload={onReload} mountedRef={mountedRef} />
          ))}
        </ul>
      )}
    </div>
  );
}

function GroupCard({
  group,
  onReload,
  mountedRef,
}: {
  group: GroupDTO;
  onReload: () => void;
  mountedRef: { current: boolean };
}): preact.JSX.Element {
  const hasHidden = group.tools_hidden && Object.keys(group.tools_hidden).length > 0;
  const scope = scopeForToggle("group");
  return (
    <li class="projects-group-card py-1.5" data-testid={`projects-group-${group.name}`} data-group={group.name}>
      <div class="flex items-center gap-2">
        <span class="text-sm font-medium text-app-text">{group.name}</span>
        <span class="text-xs text-app-muted">
          {group.servers.length === 0 ? "No servers" : `${group.servers.length} member${group.servers.length === 1 ? "" : "s"}`}
        </span>
      </div>
      {group.servers.length > 0 && (
        <ul class="projects-server-list m-0 list-none p-0">
          {group.servers.map((server) => (
            <ToggleableServerRow
              key={server}
              server={server}
              scope={scope}
              initialEnabled={true}
              // Group membership is a value-free array (group-servers): a removed
              // member re-joins by a plain enable toggle, no Re-add CTA.
              coldReenable={false}
              gateOnEnable={true}
              onReload={onReload}
              mountedRef={mountedRef}
              buildRequest={(enable) => ({
                scope,
                group: group.name,
                server,
                enable,
              })}
            />
          ))}
        </ul>
      )}
      {hasHidden && (
        <p
          class="m-0 mt-1 text-xs text-app-muted"
          data-testid={`projects-group-hidden-note-${group.name}`}
        >
          <strong>Note:</strong> hiding tools reduces the surface exposed at the
          hub; it is <strong>not</strong> an access-control boundary — daemon
          ports stay directly reachable, and at gate-OFF the hub filter is not in
          the path. Filter changes apply to new client sessions (reconnect to
          apply).
        </p>
      )}
    </li>
  );
}
