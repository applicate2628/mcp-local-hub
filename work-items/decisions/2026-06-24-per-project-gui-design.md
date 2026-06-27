# Decision: per-project-GUI design (epic area 6)

status: accepted
date: 2026-06-24
owners: architect + ux-designer (convergent) + operator sign-off (new-screen shape)
epic: work-items/epics/2026-06-19-install-and-it-works-ux.md (area 6 per-project-gui)

## Context

A GUI surface to SEE which MCPs are active **per project** and TOGGLE them, across 3 project
models, keeping the global Servers matrix intact. Deps-policy (operator): DETECT + explicit
guided consent, never silent auto-install. Architect + UX designs ran in parallel and
**converged**; operator approved the core shape.

## The 3 project models (verified, file:line)

- **A — workspace registry** (serena + per-language LSP): `workspaces.yaml` /
  `WorkspaceEntry` (`internal/api/workspace_registry.go:56-78`), `GET /api/workspaces`
  (`internal/gui/workspaces.go:72`), `POST /api/lsp/register`. EXISTS; partial per-project
  (the Servers `WorkspaceSelector` scopes LSP rows only).
- **B — client-config project scope** = THE FROM-ZERO GAP. All adapters are user-global only
  (`claude_code.go:18,28`; `json_mcp.go:30`); scan reads ONE global path/client
  (`scan.go:40-72,708`). No project-local read/write/scan exists.
- **C — groups** (`/g/` namespaces): `groups.yaml` / `api.Group` (`internal/gui/groups.go:206`),
  full Groups CRUD screen. EXISTS but NOT path-bound to a project.

## VERIFIED client project-scope formats (resolved the flagged assumption)

| Client | Project-scope file | Top-level key | Note |
|---|---|---|---|
| claude-code | `.mcp.json` (repo root) **Project scope** + `~/.claude.json → projects."<fwd-slash-abs>".mcpServers` **Local scope** | `mcpServers` | dual substrate; local-scope keyed by forward-slash abs path (verified on host); per-project `disabled/enabledMcpjsonServers` toggle arrays |
| cursor | `.cursor/mcp.json` (repo root) | `mcpServers` | pure path-reparam |
| vscode | `.vscode/mcp.json` (repo root) | **`servers`** (NOT mcpServers) | pure path-reparam, different key |

Cursor/VS Code = pure config-path reparameterization (same JSON, relocated file). claude-code =
two distinct substrates (the hard part).

## Decisions (load-bearing)

1. **NEW "Projects" nav screen** (NOT a scope-toggle on Servers). The matrix is server×client; a
   project is server×mechanism — different axes. Global Servers matrix KEPT byte-untouched.
   **Operator-approved 2026-06-24.**
2. **Model B substrate = a `ProjectScope` registry** (`internal/clients/project_scope.go`, one row
   per client: `{RelFile, SectionKey, Supported}`), NOT a `Client`-interface `ScopedConfigPath()`
   method (that would force editing all 46 adapters — wrong abstraction level). The registry feeds
   the EXISTING path-parameterized scan/write.
3. **Scan isolation (the key invariant):** a project scan is a SEPARATE call
   `ScanFrom(ScanOpts{ConfigPaths: ProjectScanConfigPaths(root)})`. `DefaultScanConfigPaths`
   (`scan.go:1971`), `ScanFrom` (`:708`), `probeClientConfigPresence` (`:172`) are UNTOUCHED →
   the global matrix's scan output is byte-identical (golden test). Two disjoint resolvers →
   leakage structurally impossible.
4. **Project = a canonical filesystem path** (`CanonicalProjectKey` single owner: clean + abs +
   case-fold-on-Windows + forward-slash). The join key for A (workspace_path) + B (project root) +
   C (group binding). MUST match claude's `projects.<key>` form.
5. **Toggle primitive DIVERGES per model** (A = register/unregister daemon; B = mutate the project
   config file; B-claude = move name between enabled/disabledMcpjsonServers; C = add/remove from
   group.servers). ONE classifier dispatches; the GUI never re-derives ownership (mimocode lesson:
   one predicate shared by reader+writer).
6. **Contract:** Phase 1 composes the EXISTING `/api/workspaces` + `/api/groups` client-side (zero
   backend); a `/api/projects` aggregate is added only in P3.
7. **Deps-consent reuses** `CheckServerReadiness` + `/api/server/readiness` + `ReadinessPanel.tsx` +
   `AddSecretModal.tsx` — DETECT → show exact command + Copy + Re-check → user confirms. No new
   deps mechanism, no silent auto-install.
8. **Immediate per-row toggle** (NOT matrix dirty/Apply — one Apply across 3 backing files
   [workspaces.yaml + .mcp.json + groups.yaml] is a blast-radius/ownership smell).

## Phasing (XL / multi-PR, each independently shippable)

- **P1 — read-only VIEW (A+C)**: new Projects screen composing existing endpoints. Frontend-only,
  additive, global matrix untouched. **No backend, no sign-off.** Reviewers: architecture-reviewer
  + ux-reviewer (no security-reviewer — no writes).
- **P2a — Model B path-reparam clients** (cursor/vscode/claude-`.mcp.json`): `ProjectScope` registry
  + `ProjectScanConfigPaths` + `GET /api/projects/scan?root=`. READ-ONLY. **security-reviewer
  MANDATORY** (project-path traversal on `?root=`; the scan-isolation invariant) + arch + qa
  (golden global-scan-unchanged test).
- **P2b — claude-code nested local scope** (`projects.<key>.mcpServers` reader): isolated to
  claude. security + arch + qa.
- **P3 — toggle/write + deps-consent + group-binding + `/api/projects` aggregate**: mutating writes
  through the reused hardened pipeline at project paths. **security-reviewer MANDATORY** (project-path
  TOCTOU, claude-json corruption risk) + arch + ux + qa.

## Operator sign-offs needed before P3 (NOT P1)

- **§10.1 group↔project binding** = a `groups.yaml` schema change (`api.Group +project_path`). Defer
  the decision (add the field vs keep groups path-agnostic-read-only) to P3.
- **§10.2 claude-code dual-scope** (Local `projects.<abs>` vs Project `.mcp.json`): surface both
  (different sharing semantics — Local=private, Project=checked-in) or one. UX-owned.

## Protected surfaces (all phases)

`scan.go` ScanFrom/probeClientConfigPresence/DefaultScanConfigPaths (ZERO edits) · Servers.tsx
global matrix (KEPT) · the `Client` interface (no new method) · the 46 adapters (no per-adapter
edit) · each adapter's global top-level write path.

## Provenance

Research memo (analyst a19062bf, PASS), architect design (a6cbec97, PASS, 10 claims +
Change-Surface Contract), UX design (ac1b1b58, PASS, wireframes + reuse map). All read-only,
evidence-backed, file:line-cited. Test discipline: `test_state_path_env` + `t.Setenv` sandbox —
never touch the live `~/.claude.json` / `workspaces.yaml` (lessons:
feedback_kosyak_subagent_test_wiped_live_supervisor_intent, feedback_state_override_misses_registry_path).
