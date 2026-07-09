# status - phase-2 de-adopt hub to native

Template: design accepted / implementation backlog. Orchestrator: `$lead`.
State: DESIGN ACCEPTED - implementation not started.
Depends-on: 2026-07-09-lsp-relay-per-client-disable-gui, 2026-07-09-intent-collapse-stop-resurrection

## Active agents / lanes
- None. Parked behind the two in-flight PRs.

## Completed agents / lanes
- Design memo accepted and copied into this work-item as `design.md`.

## Next action
After PR #524 and PR #525 land, route implementation as a full-delivery item.
The design requires durable adopt provenance before any user-facing de-adopt
flow, then backend plan/execute, gate-ON aggregate convergence, CLI/GUI surfaces,
and round-trip verification.
