---
status: closed
context: backlog
severity: low
---

# Backlog: tools/list live visibility+membership symmetry (proper design)

## Closure (2026-06-29) — SUPERSEDED (implemented + tested)

Verified against live code: the "proper design (Option A — re-derive the
fan-out on membership change)" this doc asked for has SHIPPED, so the
list-says-yes / call-says-no split it tracked no longer exists.

- **tools/list now filters live bindings before fan-out and applies a fresh
  hidden-set.** `AggregateToolsList` reconciles captured init successes
  against the CURRENT resolver snapshot via
  `filterToolsListSuccessesByLiveSnapshot`
  (`internal/api/hub_mcp_aggregator.go:318-341` orchestration; `:344-386`
  the filter), re-reads the live `tools_hidden` set from the freshest
  published snapshot (`hiddenSnapshot` / `buildHiddenToolSet` at `:331-335`),
  and resolves the empty-but-declared-group case
  (`allowEmptyKnown`/`snapshotKnowsScope` at `:318-323`,
  `:546-561`). All three Codex-flagged P2 edge cases (hide-vs-member-removal,
  empty-but-declared group, success-count/route-preservation) are handled by
  the live-snapshot rederivation rather than a per-tool band-aid.
- **tools/call already revalidated the live snapshot** (the PR #374 fence
  this doc built on): `resolveToolsCallRoute` refuses an out-of-scope or
  hidden tool against the live snapshot with -32601
  (`internal/api/hub_mcp_aggregator.go:862-891`).
- **Tests cover the symmetry.** `TestAggregateToolsListFiltersRemovedLiveBinding`
  (`internal/api/hub_mcp_aggregator_test.go:606-665`) asserts BOTH that
  tools/list drops a removed live binding (no leaked tool, no fan-out to the
  removed daemon, empty RouteMap) AND that the matching tools/call returns
  -32601 — the exact list/call symmetry this doc requested.

Closed as superseded; no remaining work.

---

(Original backlog content preserved below for provenance.)

# Backlog: tools/list live visibility+membership symmetry (proper design)

Found + attempted: PR #376 (correctness audit). Reverted on the hot path
after Codex surfaced cascading edge cases; tracked here for a proper design.
Priority: low (cosmetic — the tools/call fence already enforces correctness;
the gap self-heals on reconnect, which CLAUDE.md documents).

## The gap

PR #374 added mid-session revocation at **tools/call**
(`resolveToolsCallRoute` revalidates the live snapshot via `snapshotHidesTool`
+ `daemonStillBound`). **tools/list** still filters off the session's
init-captured snapshot, so after an operator hides a tool OR removes a member
server, a long-lived `/g/<group>` session keeps ADVERTISING that tool on
tools/list while the next tools/call rejects it with -32601. A
list-says-yes / call-says-no split until the client reconnects.

## Why the tools/list-filter band-aid was reverted

Making `assembleToolsListResponse` apply the live hidden filter + a live
`daemonStillBound` membership check looks simple but fights the existing
design (the session fan-out / `InitSuccesses` is FIXED at init), and Codex
found three interacting P2 edge cases across two rounds:

1. **Hide vs member-removal.** Reading live `tools_hidden` closes the
   hide-a-tool case but a member-REMOVAL drops the server's tools_hidden
   entry entirely, so the hidden filter alone re-advertises it → needed a
   separate membership check.
2. **Empty-but-declared group.** `isKnownGroup` allows a declared group with
   zero resolvable members (via `ResolverSnapshot.Groups`), but
   `BuildResolverSnapshotFromManifestsAndGroups` leaves no `Bindings[g:<name>]`
   key. A naive "scope-key absent → skip membership filter" guard wrongly
   DISABLES the fix for the empty-group case (it should return an empty list).
3. **Success-count / route preservation.** A removed-but-still-responding
   daemon (`r.err == nil`) increments `listSuccessCount` even though the
   membership filter drops all its tools. If the remaining BOUND daemon's
   tools/list then transiently fails, the hub takes the success path and
   stores an empty route map instead of the all-failed path that preserves
   last-good routes — a transient outage could ERASE routes.

Plus a `SnapshotAtInit == nil` mid-setup window (a real session always
captures one; tests/proactive-reinit hit nil) that must be guarded.

## Proper design (not a per-tool filter)

The fan-out, not the filter, is the right layer. Options:

- **(A) Re-derive the fan-out on membership change.** When `live != SnapshotAtInit`,
  recompute the session's bindings from the live snapshot's `Bindings[scopeKey]`
  before assembling — tools/list then reflects the current membership by
  construction, and the success-count / route-preservation logic operates on
  the correct (bound) daemon set. Empty group → empty bindings → empty list,
  naturally.
- **(B) Revoke-and-reinit.** When tools/list detects `live != SnapshotAtInit`
  for the scope, return a sentinel / empty list that prompts the client to
  reinitialize (mirroring the tools/call -32601), rather than silently
  filtering. Simpler, but a harder UX break than (A).

Either must preserve: the byte-identical /clients/ fence, the
collision-detection leak guard, the all-bound-failed last-good-route
preservation, and deterministic ordering. Add tests for: hide,
member-removal, empty-group→empty-list, transient-bound-failure→preserve-routes,
SnapshotAtInit-nil.

## Current shipped state (PR #376)

tools/call fence (PR #374) stands; tools/list keeps the documented
reconnect-to-refresh behavior. snapshotHidesTool was made allocation-free
(direct scan) — that perf win shipped and is unrelated to this gap.
