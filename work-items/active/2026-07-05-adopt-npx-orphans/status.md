# status — adopt npx-stdio orphans into the hub (A2)

Template: full-delivery (security-sensitive). Orchestrator: main conversation.
State: DESIGN ACCEPTED (architect gate PASS 2026-07-05); impl NOT started.

Queued: PR4/PR5 of the 2026-07-05 bug-backlog order — AFTER the A1 lost-child
F-package (PR2 F2+F6, PR3 F1+F3) lands + deploys, per the locked sequence.

## Next actions (before impl)
1. $security-engineer review of design.md (the 3 gated constraints: config-mutation
   consent, secret-vault routing, kill-authority identity gate).
2. Resolve blocker H2 (symlinked codex config.toml write behavior — probe) → blocks PR4.
3. Resolve blocker pipe-peer spike (SystemHandleInformation reliability) → blocks PR5 kill path.
4. Register the 4 proposed decisions (work-items/decisions/).
5. $planner phases design.md into commit-ready PRs (PR4 = P0+P1; PR5 = P2).

## Artifacts
- design.md — architect package (this dir).
- Source bug: work-items/bugs/2026-07-04-npx-stdio-mcp-orphan-accumulation-bypasses-hub.md.
- Adjacent finding (standalone-shippable subset of P2): work-items/bugs/2026-07-05-cleanuporphans-raw-taskkill-no-identity-reverify.md.

## Depends-on
2026-07-05-a1-lost-child-fpackage (F1/F3 kill-authority precedent + primitives; A2-P2 sequenced after).
