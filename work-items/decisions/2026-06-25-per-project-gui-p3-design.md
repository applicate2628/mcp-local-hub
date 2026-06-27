# Decision: per-project-GUI P3 design (the WRITE phase)

status: accepted
date: 2026-06-25
owners: architect (a5d44dad, PASS) + operator sign-offs (§10.1, §10.2 decided 2026-06-25)
parent: work-items/decisions/2026-06-24-per-project-gui-design.md
depends-on: P2b (PR #432 — supplied the `IsMcpjsonServerEnabled` + `canonicalClaudeProjectKey` symbols). RESOLVED: #432 merged; the symbols are in the tree.
Implemented: P3a (PR #433) + P3b (PR #434) + P3c (PR #435) — all shipped; live 8c359a1c. This decision is fully implemented; the planning-tense dependency/blocker notes below are HISTORICAL (kept for design provenance, not live gates).

## Operator sign-offs (DECIDED 2026-06-25)
- **§10.1 = ADD `project_path` to groups.yaml (binding).** Groups bind to a project path; the project view shows "groups for THIS project". Additive `yaml:"project_path,omitempty"`; existing groups (no field) = global/unbound; version stays 1.
- **§10.2 = show BOTH claude scopes distinctly**, labeled, each toggled in its correct substrate: Project (`.mcp.json`, checked-in) + Local (`~/.claude.json` projects.<key>.{enabled,disabled}McpjsonServers, private).

## Per-model toggle WRITE owner (ONE classifier dispatches; GUI never re-derives — decision 5)
| Model | Toggle = | Write owner (single) | Pipeline reused | Path |
|---|---|---|---|---|
| A workspace LSP | enable=register / disable=unregister daemon | `api.Register`/`api.Unregister` (register.go:109,:979) | supervisor-intent + workspace_registry | workspace_path |
| B cursor/vscode | add/remove object member | `clients.mutateJSONObjectMemberPath` (jsonc.go:197) → SecureWriteClientConfig | hujson + handle-relative atomic | `<root>/.cursor/mcp.json`, `<root>/.vscode/mcp.json` |
| B-claude Project (`.mcp.json` checked-in, shared) | DISPLAYS the `.mcp.json` member; ON/OFF GATED by the approval array-move — moves the name between enabled/disabledMcpjsonServers (scope `claude-local-membership`). **NEVER deletes the `.mcp.json` mcpServers definition.** | `OwnerClaudeLocalMembership` array-move (same mechanism as B-claude Local; the toggle gate writes `~/.claude.json`, the definition it gates lives in `<root>/.mcp.json`) | hujson + handle-relative atomic | gate write → `~/.claude.json` projects.<canonicalClaudeProjectKey>.{enabled,disabled}McpjsonServers; definition stays in `<root>/.mcp.json` |
| B-claude Local (`~/.claude.json` private) | MOVE name between enabled/disabledMcpjsonServers — **NEVER delete from mcpServers** | `clients.toggleClaudeMcpjsonMembership` (array-membership owner); enabled-predicate = P2b `IsMcpjsonServerEnabled` | hujson + handle-relative atomic | `~/.claude.json` → projects.<canonicalClaudeProjectKey>.{enabled,disabled}McpjsonServers (both the definition AND the gate live here) |
| C groups | add/remove from group.servers | `api.ReadModifyWriteGroups` (hub_mcp_groups.go:348) | atomic under hub-mcp.lock | groups.yaml |

Rejected: (1) universal ProjectConfigWriter (collapses 4 owners → method-branch-in-engine anti-pattern); (2) claude-local delete-from-mcpServers (data-loss — loses the server definition); (3) matrix dirty/Apply across 4 files (4 owners/locks, no atomic cross-file txn — decision 8).

> **The two claude rows are DISTINCT substrates, not duplicates — same toggle MECHANISM.**
> Both the `B-claude Project` and `B-claude Local` rows toggle via the approval
> ARRAY-MOVE (the `enabled/disabledMcpjsonServers` arrays in `~/.claude.json`,
> the `project_enabled` gate), because that one approval gate is what governs
> visibility of BOTH scopes. They differ in WHICH server-definition the gate
> covers: **Project** gates a definition that lives in the checked-in, shared
> `<root>/.mcp.json`; **Local** gates a definition that lives in the private
> `~/.claude.json projects.<key>.mcpServers`. Neither toggle ever deletes a
> definition — it only flips the approval array entry. (In P3b v1 the Local
> definition is also READ-ONLY, deferral D1; the Project row is live-toggleable.)
>
> **Table correction (shipped #434 r2):** the `B-claude Project` row originally
> mapped to "same as B" (the `project-object-member` member-delete via
> `mutateJSONObjectMemberPath`). That was the P3b wrong-substrate bug
> (`work-items/bugs/closed/2026-06-27-p3b-claude-project-toggle-wrong-substrate.md`):
> it would have DELETED the checked-in `.mcp.json` definition, the exact data-loss
> outcome rejected-item (2) above forbids. The row now reflects the SHIPPED reality —
> the claude Project toggle uses the approval array-move, leaving the `.mcp.json`
> definition intact.

## Security threat model (security-reviewer MANDATORY — write phase)
- **T1 project-path write TOCTOU:** resolve write path ONLY via `clients.ProjectScanConfigPaths` (no 4th path-logic copy); write via `SecureWriteClientConfig` (handle-relative, O_NOFOLLOW, atomic rename, DACL-before-bytes — TOCTOU-safe by construction).
- **T2 ~/.claude.json corruption (catastrophic):** comment+unknown-key preserving via the `mutateJSONObjectMemberPath` hujson family; atomic temp+rename; nested `projects.<key>` write must not clobber sibling projects; key normalized via the SAME `canonicalClaudeProjectKey` (no 4th normalizer — arch P2b note P3-1).
- **T3 groups.yaml:** via ReadModifyWriteGroups (atomic, lost-update-safe); `project_path` is data-only (not the scope key `g:<name>` nor route segment).
- **T4 CSRF:** all write handlers behind `requireSameOrigin` (csrf.go:81).
- **T5 leak:** `writeAPIErrorRedacted` (stable code, fixed body, reason server-side only).
- **T6 inertness:** disabled = inert in its substrate (A unregister, B remove member, B-claude disabled array, C remove from group.servers) — substrate IS the gate, no consumer-side conditional.

## §10.1 groups.yaml project_path migration
Additive omitempty field, version stays 1. Existing groups (no field) → ProjectPath=="" → global/unbound (visible in every project lens). KnownFields(true) means an OLDER binary hard-fails on a NEWer file carrying project_path — accepted (groups.yaml is local state, fail-closed is documented/safe). When set, normalize via CanonicalProjectKey; clean+abs validation (do NOT require the path to exist — a group can be authored pre-registration). Read binding: project lens filters `project_path == thisKey || project_path == ""`; the predicate owner is backend-side in /api/projects, not the frontend.

## Deps-consent (decision 7): GET /api/server/readiness → ReadinessPanel.tsx + AddSecretModal.tsx (DETECT → command + Copy + Re-check → user confirms). No silent install. CheckServerReadiness is the single owner — call, never re-derive.

## /api/projects aggregate (decision 6): composes A (workspaces) + B (project ScanFrom incl. both claude scopes) + C (groups filtered by binding), one DTO per canonical key with per-client substrate labels + per-server enabled/disabled. Uses the PROJECT resolver (never DefaultScanConfigPaths) → global scan byte-identical.

## Phasing (XL → sub-split, dependency-ordered) — ALL SHIPPED
The original dependency order (P3a → P3b/P3c) was honored and every phase merged:
- **P3a — writes-backend + /api/projects aggregate (SHIPPED #433).** Classifier + 4 leaf writers + CanonicalProjectKey owner + 2 write handlers + aggregate. Gated by security-reviewer (MANDATORY) + arch + qa (golden global-scan-unchanged + ~/.claude.json round-trip-preserves-keys). No frontend.
- **P3b — frontend toggle + both-scopes (§10.2) + consent (SHIPPED #434, incl. the r2 wrong-substrate fix).** Detail-lens immediate per-row toggles + 2 labeled claude scopes + ReadinessPanel/AddSecretModal. Gated by ux-reviewer + arch + qa (Playwright E2E).
- **P3c — group↔project binding (§10.1) (SHIPPED #435).** api.Group += ProjectPath + binding write/UI + filter predicate. Gated by arch (schema decision) + security (groups.yaml) + ux + qa.

## Protected (all P3): scan.go (0-diff, golden-pinned) · Servers.tsx (kept) · Client interface + 46 adapters (no new method, no per-adapter edit) · each adapter's GLOBAL write path (P3 writes PROJECT paths + the claude-local arrays ONLY).

## Claims (14, for the reviewers) — see the architect's full package (task a5d44dad); each is {guarantee, single-owner, enforcement-probe}. Key ones: scan.go 0-diff (golden); SecureWriteClientConfig for every project write (no os.WriteFile); claude-local moves arrays never deletes mcpServers (mcpServers[name] still present after disable); ~/.claude.json round-trip preserves all keys+comments+sibling projects; one classifier dispatches (GUI doesn't branch on client name); deps-consent reuses CheckServerReadiness (no re-derive); project_path additive/v1-stays; same-origin gated.

## BLOCKER — RESOLVED (historical)
At design time P3a was `BLOCKED:prerequisite` on #432 until `IsMcpjsonServerEnabled`
+ `canonicalClaudeProjectKey` existed in the tree, and `CanonicalProjectKey`
(decision 4, the A+B+C join owner) was named-but-unimplemented. **All resolved:**
#432 merged and supplied the predicate/key symbols; P3a (#433) landed
`CanonicalProjectKey` and the rest of the write backbone. No live blocker remains —
P3a/P3b/P3c are all shipped (live 8c359a1c). This section is retained for provenance.
