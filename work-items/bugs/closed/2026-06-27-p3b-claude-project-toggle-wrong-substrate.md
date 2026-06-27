# Bug: P3b claude Project toggle writes the wrong substrate (.mcp.json member delete) instead of the approval array-move

- id: 2026-06-27-p3b-claude-project-toggle-wrong-substrate
- context: PR #434 (per-project-GUI P3b frontend) / work-items/decisions/2026-06-27-per-project-gui-p3b-uxdesign.md
- status: fixed
- severity: high
- area: internal/gui/frontend/src/screens/Projects.tsx (ClaudeBothScopesCard) + lib/project-toggle.ts
- found-by: qa-engineer
- fixed-by: P3b r2 frontend fix (branch feat/projects-p3b-frontend) — see "## Resolution" below

## Reproduction

QA gate, acceptance criterion #3: "the claude Project row sends scope `claude-local-membership`
(NOT project-object-member) — asserted."

The implementation sends `project-object-member` for the claude checked-in `.mcp.json`
Project subsection:
- `internal/gui/frontend/src/screens/Projects.tsx:1256` — `const scope = scopeForToggle("object-member");`
  inside `ClaudeBothScopesCard` (the Project subsection).
- `Projects.tsx:1320` reads ON/OFF from `entry.project_enabled` (the approval-array substrate).
- `Projects.tsx:1332-1339` POSTs `{client:"claude-code", scope:"project-object-member", enable}`.

Expected (accepted P3b UX design line 19/21, §10.2 line 26 + merged P3a backend contract,
commit 12990df1): the claude checked-in `.mcp.json` Project toggle MUST use mechanism
`"claude-local"` → scope `claude-local-membership`, which MOVES the server name between
`~/.claude.json projects.<key>.{enabled,disabled}McpjsonServers` and NEVER deletes the
`.mcp.json` mcpServers definition (decision 5).

Actual: scope `project-object-member` routes to `OwnerProjectObjectMember`
(`internal/clients/project_toggle_owner.go:93-97`; claude-code HAS a projectScopeRegistry
row so the classifier accepts it, `project_scope.go:69`) →
`ToggleProjectObjectMember` → `mutateJSONObjectMemberPath(..., del=!enable)`
(`project_toggle_owner.go:166-184`) → on disable emits an RFC-6902 `remove` of
`/mcpServers/<server>` from `<root>/.mcp.json` (`internal/clients/jsonc.go` delete path).

## Read/write substrate mismatch (the load-bearing defect)

- READ ON/OFF: `entry.project_enabled` ← `IsMcpjsonServerEnabled` (approval arrays in
  ~/.claude.json) — `internal/api/project_claude_local.go:102-103`,
  `internal/clients/claude_local_scope.go:84-99`.
- WRITE: `.mcp.json` member add/remove (a DIFFERENT file/substrate).
- IMMEDIATE read-back: `ProjectObjectMemberPresent` (`.mcp.json` member presence) —
  `internal/gui/projects_toggle.go:226`. This MASKS the bug: the immediate per-row
  reconcile flips OFF correctly because the just-deleted member is absent.

## Concrete consequences

1. Data loss: disabling a claude Project row deletes the server definition from the
   checked-in, shared `<root>/.mcp.json` — the exact outcome the parent P3 design line 22
   rejects ("data-loss — loses the server definition") and decision 5 / `claude_local_toggle.go:16`
   forbid ("NEVER delete from mcpServers").
2. Spring-back contradiction on reload: the write never touches the approval arrays, so a
   fresh aggregate reload re-seeds `initialEnabled={entry.project_enabled===true}`
   (`Projects.tsx:1320`; rows remount per `useToggleRow` comment lines 435-437). If the
   server is in `enabledMcpjsonServers` or `enableAllProjectMcpServers:true`, the row
   reloads ON — contradicting the disable the user just performed.

## Tests codify the bug (criterion #2 both-scopes is wrong, not just incomplete)

- Component: `internal/gui/frontend/src/screens/Projects.test.tsx:283` asserts
  `projects-toggle-project-object-member-approved` for the claude Project row.
- E2E: `internal/gui/e2e/tests/projects-toggle.spec.ts:179-181` asserts
  `toggleBody.scope === "project-object-member"` + `client === "claude-code"`.
- Pure-fn: `lib/project-toggle.test.ts` never asserts that the claude PROJECT subsection
  selects `claude-local-membership` (it tests `scopeForToggle` in isolation, which is
  correct; the wiring at the call site is the bug).

So the all-green suite does not catch this — the tests pin the wrong contract.

## Fix direction (for the implementer; QA does NOT fix)

`ClaudeBothScopesCard` Project subsection must use mechanism `"claude-local"` →
`scopeForToggle("claude-local")` → `claude-local-membership`. The helper already supports
this (`lib/project-toggle.ts:47-48`). The claude Project toggle then becomes value-free
(the array-move needs no member value, the definition stays in `.mcp.json`), which also
removes the warm/cold value-replay + Re-add CTA from the claude Project subsection
(those remain correct for cursor/vscode object-member rows). Component + E2E both-scopes
tests must be updated by the implementer to assert `claude-local-membership` for the
claude Project row. Re-verify decision 5's no-delete invariant (mcpServers[name] present
after disable) end-to-end.

## Resolution (FIXED — P3b r2, branch feat/projects-p3b-frontend)

Frontend-only fix (PROTECTED backend was already correct — the
`OwnerClaudeLocalMembership` array-move handler at `internal/gui/projects_toggle.go:244`
needs no value and reads back via `IsMcpjsonServerEnabled`).

- **FIX 1 (the bug):** `ClaudeBothScopesCard`'s Project subsection now selects
  `scopeForToggle("claude-local")` → scope `claude-local-membership` (the approval
  ARRAY-MOVE) instead of `scopeForToggle("object-member")` → `project-object-member`
  (the `.mcp.json` member delete). The toggle is value-free and the `.mcp.json`
  definition is never deleted (decision 5). cursor/vscode stay `project-object-member`.
  The mis-rationalizing comment in `lib/project-toggle.ts` (lines 27-40) was corrected.
- **FIX 2:** the dead warm value-replay machinery (`heldValuesRef` capture-on-disable +
  replay-on-enable, `rawObjectValue`/`heldKey`) was removed — the aggregate NILs every
  `raw` (`internal/gui/scan.go:83` `stripClientEntryRaw`) so the warm path was always a
  no-op. Object-member (cursor/vscode) re-enable is now ALWAYS COLD (Re-add CTA →
  `#/add-server`), never a value-less or value-bearing enable POST.
- **FIX 3:** every rendered workspace row seeds `initialEnabled=true` (a failed/missing
  registry entry is still registered, so the first click DISABLES/Unregisters for cleanup).
- **FIX 4:** `SectionProjectConfig` renders `ClaudeBothScopesCard` when `project_scope`
  carries claude Local content even with zero scan entries (local-only project).
- **FIX 5:** `ProjectDetail` is keyed by `projectKey` at the `ProjectsScreen` call site,
  so navigating between projects remounts the tree and per-row toggles re-seed.
- **FIX 6:** cursor/vscode object-member rows carry a `title`/`aria-description`
  disable-affordance warning ("Disabling removes this entry; re-enabling will require
  re-adding it.").
- **FIX 7 + 8:** component (`Projects.test.tsx`) + E2E (`projects-toggle.spec.ts`) tests
  updated to assert `claude-local-membership` for the claude Project row + the cold
  object-member path against the sanitized (raw=null) wire shape; NEW regression tests
  added for the decision-5 invariant (claude Project disable → array-move scope, never the
  member-delete) and the reload spring-back (a disabled claude Project server stays OFF
  after a remount/reload). The 20 spec'd `projects-*` CSS classes were added to
  `internal/gui/frontend/src/styles/style.css` (theme-token driven), and the embedded
  bundle was regenerated via `go generate ./internal/gui/...`.

Verification: `npm run typecheck` clean; `npm run test` 852/852 green (incl. the new
claude-local-membership / cold / decision-5 / spring-back tests); `go build ./...` clean;
`go test ./internal/gui/ -run 'Embed|Projects|Scan|Toggle'` green. PROTECTED: scan.go +
gui/scan.go 0-diff, Servers.tsx byte-unchanged, zero Go source changes.
