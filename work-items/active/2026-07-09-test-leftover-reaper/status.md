# status - test-leftover reaper

Template: design revised / implementation backlog. Orchestrator: `$lead`.
State: DESIGN REVISED - feasibility PASS-with-constraints - implementation not started.
Depends-on: 2026-07-09-lsp-relay-per-client-disable-gui, 2026-07-09-intent-collapse-stop-resurrection

## Active agents / lanes
- None. Parked behind the two in-flight PRs.

## Completed agents / lanes
- Design memo accepted and copied into this work-item as `design.md`.
- Feasibility probe incorporated on 2026-07-09: Windows live env proof is GO only for an amd64 PEB-based reader against amd64/i386 same-user targets; unsupported env proof remains preview/refuse-only.

## Next action
After PR #524 and PR #525 land, route implementation as a security-sensitive
full-delivery item: design -> implement -> QA -> security review. The lane is
operator-invoked only, not on the unattended ticker. The design requires an
explicit test-leftover cleanup lane, amd64 Windows PEB live process-environment
evidence for apply, identity-bound termination, and security review before
publication.
