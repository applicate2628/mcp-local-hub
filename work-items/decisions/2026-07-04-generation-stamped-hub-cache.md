---
status: accepted
date: 2026-07-04
deciders: $architect (design), main-conversation (impl), Codex #500 bot (3 rounds of edge-mining)
---

# Cache freshness = a monotone generation stamp co-located under the same lock as the qualified value

## Context

Fixing the gate-ON hub store FREEZE required releasing the per-port `state.mu`
across the blocking reinit handshake. That removed atomic observe-and-act and
opened a SERIES of observe-then-act windows the Codex #500 bot flagged three
times (fable Defect 1 lost-restart; redundant-reinit-leak; window #3 cache-
written/stale-not-cleared). Each prior round added another cross-lock re-check.

## Decision

Freshness of a cached daemon MCP session id is a **monotone generation stamp
co-located under the same lock as the sid it qualifies**, NOT a separate `stale
bool` under a different lock. A caller reuses the cached sid iff
`cachedGen >= currentRestartGeneration`; otherwise it reinits. Cross-lock reads
are safe because the comparison is conservative in the monotone direction (read
the cache first, then the generation; a restart landing between bumps the
generation → the compare fails → reinit).

Concretely: `stalePortState` keeps `mu`+`generation` (drop `stale bool`);
`hubSession` gains `InitSuccessGen map[canonicalDaemonRef]uint64` written ONLY in
`cacheReinitResult` (single writer of fresh reinit sids) under the same `s.mu`
as `InitSuccesses`, stamped with the FLIGHT's start generation (`flightGen`, read
once in the DoChan fn before dialing — the durable form of the fable Defect 1
fix). `refreshStalePortBeforeDispatch` collapses the fast-path re-check + the
pre-reinit re-check + window #3 into one compare.

This DISSOLVES the s.mu/state.mu/stale-bool hazard class (the two locks are never
held simultaneously after the clear is deleted) rather than adding a fourth
re-check. It deletes more than it adds: staleClearToken, staleClearTokenForPort,
clearCachedReinitStaleFlag, liveCallerClears, daemonInitState.staleGen, both
re-checks, the stale bool, and the test seams.

## Residual (accepted)

Window W: `[singleflight forgets the key] → [cacheReinitResult writes the fresh
entry]`. A straggler there reads the old entry → one redundant flight + one
leaked daemon session (GC-reclaimed). NOT a regression (present pre-change);
strictly smaller than the pre-decision windows. Closable by moving the cache
write INSIDE the DoChan fn (pre-scoped follow-up, needs its own review of the
deleteStarted / detached-drain / DELETE-cleanup interaction).

## Pre-scoped follow-ups (not this change)

- Move the authoritative cache write inside the DoChan fn (closes window W).
- Make `generation` an `atomic.Uint64`, drop the per-port `mu` (makes the freeze
  structurally impossible); guard the transient generation==0 publication window.
- Extend gen-awareness to the moved-port cache (cachedDaemonInitState).
