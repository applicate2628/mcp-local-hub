# Brief — Windows console opt-in delivery R2

## Goal

Finish the archived parent delivery from `work-items/archive/2026-08/2026-08-10-windows-console-opt-in/` and ship a working Windows hub that never opens visible consoles in ordinary operation.

## Accepted baseline

- Design, architecture review, plan, implementation, runner, security, integration, and QA artifacts remain canonical in the archived parent.
- Windows target console contract: 6/6 top-level probes and 63/63 subtests GREEN.
- Canonicalize PE fixture correction: focused, adjacent, race, vet, formatting, and diff gates GREEN.
- Do not rerun the full Windows target suite unless a relevant source byte changes.

## Current scope

- Broad CLI reconcile timeout and GUI broadcaster/audit-lock lifecycle failures.
- Real Linux and native macOS verification with durable outputs.
- Independent final QA, publication safety, commit/push, install/restart, fleet recovery, and live no-console verification.

## Out of scope until hub delivery

- New pull-request and merged-without-PASS review wave. It is queued and begins only after the hub is functional and live-verified.

## Owners and risks

- Reliability owner: broad lifecycle failures and deterministic reproduction.
- Backend integration owner: coherent candidate after any accepted correction.
- QA owner: independent release gate.
- Platform owner: native CI, packaging, installation, rollback, and fleet restoration.
- Publication approver: knowledge-archivist; security-reviewer only for any exception.

## Open obligations

1. Broad CLI/GUI gates.
2. Real Linux/native macOS gates.
3. Independent QA.
4. Publication safety and human push approval.
5. Install/restart/live no-console proof.
