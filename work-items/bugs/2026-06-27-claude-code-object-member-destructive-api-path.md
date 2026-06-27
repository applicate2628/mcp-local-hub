---
status: open
severity: medium
context: closeout-bot-finding (#436); latent backend gap surfaced by the per-project-GUI closeout review
---

# /api/projects/toggle still accepts the destructive `project-object-member` scope for claude-code (member-DELETEs the checked-in `.mcp.json` definition)

## Finding

The P3b r2 wrong-substrate fix (`work-items/bugs/closed/2026-06-27-p3b-claude-project-toggle-wrong-substrate.md`,
shipped #434) was **FRONTEND-ONLY**: it changed the GUI call-site to send scope
`claude-local-membership` (the non-destructive approval array-move) for the claude
Project row. The BACKEND classifier was not narrowed, so a direct API caller — or a
future frontend regression — can still trigger the destructive path.

## Verified mechanism (file:line, on-disk at 8c359a1c)

- `internal/clients/project_toggle_owner.go:89-108` — `ProjectToggleOwner(client, scope)`
  dispatches on **scope**, not client. For `ScopeProjectObjectMember` it returns
  `OwnerProjectObjectMember` whenever `projectScopeForClient(client) != nil`
  (`:93-97`).
- `internal/clients/project_scope.go:69` — claude-code IS in `projectScopeRegistry`
  (`{Client: "claude-code", RelFile: ".mcp.json", SectionKey: "mcpServers", Supported: true}`),
  so `projectScopeForClient("claude-code")` is non-nil.
- Therefore `ProjectToggleOwner("claude-code", "project-object-member")` →
  `OwnerProjectObjectMember` → dispatched at `internal/gui/projects_toggle.go:130-131`
  to `toggleProjectObjectMember` →
  `clients.ToggleProjectObjectMember("claude-code", "<root>/.mcp.json", server, value, enable)`
  (`project_toggle_owner.go:166-185`) → `mutateJSONObjectMemberPath(..., del=!enable)`.
- On `enable=false` this emits an RFC-6902 `remove` of `/mcpServers/<server>` from the
  checked-in, shared `<root>/.mcp.json` — the exact data-loss the P3 design line 22
  rejects ("data-loss — loses the server definition") and decision 5 forbids.

A caller sending `{client:"claude-code", scope:"project-object-member", enable:false}`
to `/api/projects/toggle` bypasses the frontend's `claude-local-membership` choice and
deletes the definition. The request is same-origin-gated
(`requireSameOrigin`, `projects_toggle.go:103`) but that does not stop a future
frontend regression or any in-page caller.

## Fix (follow-up PR — NOT this docs-only closeout)

Two acceptable directions; pick one in the follow-up:

1. **Reject (preferred):** `ProjectToggleOwner` returns `OwnerUnsupported` (or a typed
   refusal) for `client == claude-code && scope == ScopeProjectObjectMember`, forcing
   the non-destructive `claude-local-membership` array-move. This makes the destructive
   path unreachable for claude-code at the single-owner classifier — no consumer-side
   conditional needed.
2. **Document as intentional power-user path:** if a deliberate member-delete for
   claude-code is ever wanted, it must be an explicit, separately-named scope, not the
   default `project-object-member` the cursor/vscode rows share.

Add a regression test (`project_toggle_owner_test.go`) asserting claude-code +
`project-object-member` does NOT route to `OwnerProjectObjectMember`.

Found by the closeout bot on #436 (round 3).
