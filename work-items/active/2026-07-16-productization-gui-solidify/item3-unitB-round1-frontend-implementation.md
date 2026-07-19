# PR #563 round-1 P1 frontend implementation

Outcome: **PASS** for the bounded frontend implementation gate. This artifact covers only P1 readiness-gated
port-change navigation; integration, bundle regeneration, the full frontend suite, Go checks, commit, and push
remain owned by the main integration lane.

## Summary

The restart-progress consumer now treats a matching port-change `reserved` event as permission to begin a
bounded readiness probe, not permission to navigate immediately. The production probe loads a cache-busted
`/favicon.ico` from the target loopback origin. The standby listener rejects that path
(`internal/gui/ping.go:123-130`), while the activated full GUI serves the embedded icon
(`internal/gui/assets.go:31-48`), so only the full GUI can satisfy the image probe. Readiness exhaustion,
image errors, timeouts, aborts, rejected injected probes, and blocked assignment all leave the old page in place.

Owner: `consumeGuiRestartProgressEvent` and its private readiness helpers in
`internal/gui/frontend/src/api.ts:953-1042,1110-1149`. Falsifying probe: the focused Vitest command below must
fail on the pre-fix implementation and pass on this implementation.

## Changed files

- `internal/gui/frontend/src/components/settings/SectionGuiServer.test.tsx:41-53,293-360` — injected readiness
  seam plus pending, success, exhaustion, stream-mismatch, same-port, and committed navigation assertions.
- `internal/gui/frontend/src/api.ts:953-1042,1110-1149` — bounded image readiness polling and asynchronous
  readiness-gated assignment.
- `internal/gui/frontend/src/components/settings/SectionGuiServer.tsx` — not changed; it already consumes the
  single API owner at `:71-88`, and no additional component-lifetime coordinator is required for a finite,
  locally-cleaned poll.

## Red / green evidence

RED command:

`cd internal/gui/frontend && npm test -- src/components/settings/SectionGuiServer.test.tsx`

Observed exit 1: 19 tests ran, 17 passed, and the two new readiness tests failed. Both failures reported
`waitUntilReady` was called 0 times (`SectionGuiServer.test.tsx:310` and `:342`), matching the pre-fix direct
assignment at the former `api.ts:1050-1055` path.

GREEN command:

`cd internal/gui/frontend && npm test -- src/components/settings/SectionGuiServer.test.tsx`

Latest observed exit 0 after the final cleanup review: 1 test file passed; all 19 tests passed in 1.15 seconds.

Type check:

`cd internal/gui/frontend && npm run typecheck`

Observed exit 0 from `tsc --noEmit`.

## Receiving-side echo

Named guard actual expected versus observed:

- Matching `reserved` port change on the current old-port stream: expected `assign` to remain uncalled while
  readiness is pending, remain uncalled on controlled exhaustion, and run exactly once after controlled
  success. RED observed no readiness call; GREEN observed all three assertions pass
  (`SectionGuiServer.test.tsx:293-344`).
- Stream mismatch: expected no second navigation from an event whose `old_port` is not the current port;
  GREEN kept the assignment count at one after the mismatched event (`SectionGuiServer.test.tsx:318-327`).
- Same-port and non-`reserved`: expected no navigation; GREEN kept `assign` uncalled for same-port `reserved`
  and `committed` (`SectionGuiServer.test.tsx:346-360`). The owner-level phase guard at `api.ts:1123-1124`
  also excludes `in-progress` and `interrupted` before any readiness work.

Invariant verification:

- Full-GUI readiness discriminator: verified structurally. The probe is the target origin's cache-busted
  `/favicon.ico` (`api.ts:1022-1029`); standby 404s non-ping paths (`ping.go:123-130`); the activated full GUI
  uniquely registers the icon route (`assets.go:31-48`). A live browser self-restart was intentionally not run
  under the task's no-GUI-spawn constraint.
- Bounded cleanup: verified structurally. There are 20 attempts, each capped at 500 ms, with at most 19
  250 ms retry delays: a worst-case timer budget of 14.75 seconds (`api.ts:960-962,1026-1036`). Every image
  completion clears its timeout, removes its abort listener, nulls both image callbacks, and cancels a failed
  load with a data URL (`api.ts:971-1003`); every retry timer removes its listener or is cleared on abort
  (`api.ts:1006-1019`). The detached async task catches both readiness rejection and assignment failure
  (`api.ts:1138-1147`), so it creates no unhandled promise rejection.

Class inventory — every navigation trigger/read site:

- `api.ts:1115-1149`, matching port-change `reserved` trigger — **fixed**: favicon readiness precedes the sole
  restart-origin `window.location.assign`.
- `api.ts:1123-1135`, invalid/non-`reserved`, missing-port, same-port, old-stream mismatch, and target-equals-old
  guards — **not affected**: all still return before probing or assignment.
- `SectionGuiServer.tsx:71-88`, the sole production `gui-restart-progress` consumer — **fixed through the API
  owner**; no parallel navigation path exists.
- `SectionGuiServer.tsx:95-105`, restart POST `target_port` reader — **not affected**: renders guidance only.
- `SectionGuiServer.tsx:256-275`, progress `new_port`/`target_port` reader — **not affected**: renders coarse
  progress only and does not navigate.
- Frontend hash routing (`app.tsx:222-224`, `hooks/useRouter.ts:9`) and Add Server reload/hash actions
  (`screens/AddServer.tsx:1195-1208`) — **not affected**: intra-origin application navigation, outside the GUI
  restart-origin class.

## Risks and non-goals

- **ASSUMPTION (UNVERIFIED):** the target production browser fires `Image.onload` for the embedded icon and
  `Image.onerror` for the standby 404 exactly as the browser contract normally does. Resolver: the integration
  owner's permitted manual port-change self-restart smoke; this retry was explicitly forbidden from spawning a
  GUI.
- Activation taking longer than the 14.75-second polling budget intentionally exhausts and leaves the old page
  in place; the existing manual new-port guidance remains visible.
- No unsafe module-global mutable coordinator was added; only immutable limits are module-scoped, and all
  mutable probe state is call-local.
- No perf surface: this is a bounded, event-triggered handoff probe, not a render or interaction hot path.
- Accessibility: no rendered element, keyboard behavior, focus behavior, accessible name, or ARIA semantics
  changed. The package has no dedicated automated accessibility script (`package.json:6-10`); no accessibility
  check was applicable to this non-DOM change.
- `go generate`, bundle regeneration, full frontend tests/build, Go checks, commit, and push were not run in
  this lane.

## Terms and Abbreviations

- GUI: Graphical User Interface.
- P1: Priority 1 review finding.
- TDD: Test-Driven Development.
