# status - test-leftover reaper

Template: design revised / implementation backlog. Orchestrator: `$lead`.
State: DESIGN REVISED - feasibility PASS-with-constraints - implementation not started.

## Active agents / lanes
- None. Ready to implement; sequenced after the in-flight PRs by lead choice, not
  blocked by them.

## Completed agents / lanes
- Design memo accepted and copied into this work-item as `design.md`.
- Feasibility probe incorporated on 2026-07-09: Windows live env proof is GO only for an amd64 PEB-based reader against amd64/i386 same-user targets; unsupported env proof remains preview/refuse-only.
- Live process census (2026-07-09) validated the revised branch gates against a real
  host: the two live leftovers were `mcphub-reliability-<digits>.exe supervise` (the
  old exact-`mcphub.exe` gate would have missed both), while a naive
  parent-missing/`supervise`/age gate would have killed the live installed supervisor,
  and a broad `mcp*` gate would have killed a live `mcp-server-fetch.exe` daemon child
  and three editor `mcp-language-server.exe` processes.

## Next action
After PR #524 and PR #525 land, route implementation as a security-sensitive
full-delivery item: design -> implement -> QA -> security review. The lane is
operator-invoked only, not on the unattended ticker. The design requires an
explicit test-leftover cleanup lane, amd64 Windows PEB live process-environment
evidence for apply, identity-bound termination, and security review before
publication.
