---
status: open
context: adjacent-finding
---

# P2b claude .mcp.json approval reader is `~/.claude.json`-only

Filed during PR #432 (per-project-GUI P2b) while implementing the architect's
opt-IN reconciliation ruling. Architect adjacent-finding (deferred, not part of
the per-server rule): the approval reader is correct TODAY but reads only ONE of
Claude Code's potential approval surfaces.

## Summary

`clients.ReadClaudeLocalScope` reads the `.mcp.json` (Project-scope) approval
state — `enableAllProjectMcpServers`, `enabledMcpjsonServers`,
`disabledMcpjsonServers` — EXCLUSIVELY from `~/.claude.json`
(`projects.<canonicalKey(root)>`). It does NOT consult the project's
`.claude/settings.json` or `.claude/settings.local.json`, which Claude Code's
settings docs also document as carriers of these same keys.

## Why it is correct now (not a bug today)

Claude Code issue #24657 states that `~/.claude.json` is the AUTHORITATIVE
location where Claude Code currently persists the per-project `.mcp.json`
approval/trust state (the "approve these project MCP servers?" prompt writes
`enabledMcpjsonServers` / `disabledMcpjsonServers` / `enableAllProjectMcpServers`
into `~/.claude.json` → `projects.<key>`). So reading only `~/.claude.json` gives
the same answer the live Claude Code session would compute. The settings.json /
settings.local.json keys are a documented schema surface but are not where the
trust-prompt result lands today.

## The risk to flag

A FUTURE Claude Code version could start honoring (or migrating the
trust-prompt result into) `.claude/settings.json` /
`.claude/settings.local.json` for these approval keys. If that happens, mcphub's
P2b `ProjectEnabled` reconciliation would silently diverge from what Claude Code
actually loads — a server approved via `.claude/settings.local.json` would show
as NOT-approved in the Projects GUI (and vice versa for a deny).

## What a proper fix would do (deferred)

- Add a settings.json / settings.local.json reader for the three approval keys.
- Define the PRECEDENCE across `~/.claude.json` vs settings.json vs
  settings.local.json (verify against the then-current Claude Code docs — do NOT
  guess the layering; the precedence is the load-bearing decision).
- Fold the merged result into `ClaudeLocalScope` so
  `IsMcpjsonServerEnabled` stays the single owner of the predicate.

## Pointers

- `internal/clients/claude_local_scope.go` — `ReadClaudeLocalScope` (the
  `~/.claude.json`-only reader) + `IsMcpjsonServerEnabled` (the single-owner
  opt-IN predicate).
- `internal/api/project_claude_local.go` — `EnrichProjectClaudeLocalScope`
  (the P2b enrichment caller).

NOT in scope for PR #432 — wiring `hasTrustDialogAccepted` into the per-server
predicate is a SEPARATE architect adjacent-finding (the 3 settings keys are
sufficient for the per-server rule).
