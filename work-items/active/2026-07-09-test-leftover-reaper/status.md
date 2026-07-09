# status - test-leftover reaper

Template: design accepted / implementation backlog. Orchestrator: `$lead`.
State: DESIGN ACCEPTED - implementation not started.
Depends-on: 2026-07-09-lsp-relay-per-client-disable-gui, 2026-07-09-intent-collapse-stop-resurrection

## Active agents / lanes
- None. Parked behind the two in-flight PRs.

## Completed agents / lanes
- Design memo accepted and copied into this work-item as `design.md`.

## Next action
After PR #524 and PR #525 land, route implementation as a security-sensitive
full-delivery item. The design requires an explicit operator-invoked
test-leftover cleanup lane, live process-environment evidence, identity-bound
termination, and security review before publication.
