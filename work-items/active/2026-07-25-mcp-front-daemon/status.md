# MCP front daemon — Increment 1

Template: full-delivery (reliability/architecture-critical). Orchestration weight: requiresLead.
Branch: feat/mcp-front-daemon (worktree d:/dev/mcphub-front-daemon, off origin/master 1889cff6).
Started: 2026-07-25. Operator go-ahead: yes.

## Goal
Extract the serena+LSP router data plane out of the GUI process into a
supervisor-managed `mcphub route` front daemon on a secondary port, so serena+LSP
MCP survive GUI death. Contract-neutral (no client-config change, secondary port).
Full design: work-items/decisions/2026-07-25-mcp-data-plane-off-gui-onto-supervised-front-daemon.md

## Constraint
Codex bot unavailable until Tuesday (operator). No merge until bot PASS. Build +
internal ultracode gate now; hold merge.

## Pipeline
- [x] Research/design: architect (PASS) + reliability (REVISE→resolved on the
      safety-net side) — both lenses in.
- [x] Decision record filed (proposed).
- [x] Implement Increment 1 (sonnet), SCOPED per a verified conflict (see
      below): internal/mcproute (port-bound origin guard only) + thin GUI
      adapter (all gui tests green, incl. -race) + `mcphub route` subcommand
      (reuses gui.Server directly — the full handler was NOT relocated) +
      real empirical probe (PASS — real windowsgui binary, temp state,
      GUI killed, route daemon proved to survive). Supervisor daemon
      descriptor wiring is DEFERRED (reported as a distinct remaining
      sub-step, not attempted).
      Commits: 1fa828eb (Phase 1a), a71e861e (Phase 1b), 74104237 (adjacent
      finding). Full report: see the implementer's final message in this
      session (not yet copied into a separate report.md).
      CONFLICT found + reported (not fixed unilaterally): a literal move of
      serena_router.go/lsp_router.go's HANDLER + the stateful session stores
      (serena_router_session.go/serena_router_handshake.go) is blocked by (1)
      an undeclared hard dependency on serena_router_lifecycle.go (JSON-RPC/
      idle-wake/activity logic, not in the Increment-1 file list) and (2)
      ~9+ existing gui test files directly touching the session stores'
      UNEXPORTED fields/methods — Go visibility rules make byte-identical
      preservation across a package move impossible; only a mechanical,
      compiler-verified rename would work, which is a scope decision for the
      architect, not an implementer judgment call.
- [ ] Architect/orchestrator decision needed: accept the scoped Phase-1a/1b
      delivery as Increment 1, or approve the wider rename-and-move scope for
      a follow-up Phase 1a-continued.
- [ ] Cross-family adversarial verify (codex Sol @xhigh + Opus architecture-
      reviewer + reliability) on the architect's 8 claims + diff-invisible
      invariants.
- [ ] Synthesis + acceptance (Opus/Fable).
- [ ] HOLD merge for Tuesday's bot.

## Next action
Orchestrator reviews the implementer's report (conflict + scoped delivery),
decides whether to accept the scoped Increment 1 or approve the wider
handler/session-store relocation scope, then route to cross-family verify.
