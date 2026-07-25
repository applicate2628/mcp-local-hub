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
- [ ] Implement Increment 1 (sonnet): extract internal/mcproute + thin GUI
      adapter (all gui tests green) + `mcphub route` subcommand + supervisor
      daemon descriptor + north-star probe.
- [ ] Cross-family adversarial verify (codex Sol @xhigh + Opus architecture-
      reviewer + reliability) on the architect's 8 claims + diff-invisible
      invariants.
- [ ] Synthesis + acceptance (Opus/Fable).
- [ ] HOLD merge for Tuesday's bot.

## Next action
Dispatch the Increment-1 implementer (sonnet) with the architect's spec.
