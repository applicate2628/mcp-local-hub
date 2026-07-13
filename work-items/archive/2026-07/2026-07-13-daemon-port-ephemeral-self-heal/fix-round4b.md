# D round-4b — ALL THREE reviewers (fable + Sol + Terra) converge on ONE P1 (FIX-4b regressed the COMMON path)

Round-4 verdict: fable REVISE (empirically proven, probe: respawn --port=9401 want 9470) + Sol + Terra INDEPENDENTLY found the same equal-timestamp lost-update. FIX-5b/6/7/8/9 CLOSED + well-pinned; CloneIntentWithReallocatedPort correct.

## Reviewer convergence
- **fable:** empirical probe proves the common-path regression (below).
- **Terra:** "TestRealloc_EqualTimestampSnapshot_TreatedStale only covers the OPPOSITE ordering (cache already has newer port 9999); does NOT exercise this lost-update ordering. A monotonic persisted revision/generation is needed."
- **Sol P2:** "TestRealloc_EqualTimestampSnapshot_TreatedStale sets curDesc.Port=9999 without updating --port argv or RuntimeSpec, checks only dd.Port → a skipped snapshot can appear correct while a real respawn uses STALE ARGV. Use CloneIntentWithReallocatedPort or assert the spawned descriptor's Port + --port + RuntimeSpec."

Option (b) below RESOLVES the stated defect (Sol's "stale argv") because patchCachedReallocatedPort → CloneIntentWithReallocatedPort patches port + argv + serena RuntimeSpec consistently. The monotonic revision is the reviewers' IDEAL ordering signal → follow-up (option b resolves the actual defect).

## The FLAKE is PRE-EXISTING (NOT a round-4b blocker) — proven
Load-disambiguation (bnkpmtzi6): under artificial load, `TestVerifyProxyReady*` (SSE-held-open) failed WITHOUT my tests present (WITHOUT run3), while WITH my tests had 0 non-mine fails. My earlier "-skip passed" was luck. The `-tags=test_state_path_env` full-suite flake in `TestVerifyProxyReady*` / `TestSupervise_IPC_QuiesceTimersTwoFrames` is PRE-EXISTING load-induced timing brittleness (tight SSE/IPC real-time reads that starve under CPU contention), NOT introduced by this feature and NOT on any round-4 code path. → follow-up work-item (make those timing tests load-robust); does NOT block D. The clean tagged gate before commit will run WITHOUT concurrent load.

## The P1 (fable, verified end-to-end + probe test)
`ReallocateDynamicPoolPort`'s step-4 mutate (`internal/api/reallocate_dynamic_pool.go:107-134`) NEVER sets `f.UpdatedAt`; `MutateSupervisorIntentIfChanged`/`writeSupervisorIntentLockHeld` don't stamp it either (UNLIKE every register write, which stamps RFC3339Nano). So after a SUCCESSFUL realloc + SUCCESSFUL intent read (production-normal), the worker's carried snapshot has the SAME `UpdatedAt` as the cache. `reallocSnapshotOrder` (`supervise_realloc.go:571-574`) classifies EQUAL as `reallocSnapshotIncomingStale`; the stale branch (`:441-452`) SKIPS the apply AND does NOT patch (its comment falsely assumes "the cache already carries the reallocated port" — false, nothing refreshed it yet) → the respawn goes out on the OLD stolen port.

Round-3 applied the equal-timestamp snapshot (fail-open) and worked here; FIX-4b (equal→stale) flipped it → a round-4 regression of round-4's OWN headline deliverable, now on the COMMON path (was the rare read-fail path). Impact bounded ≤60s (mtime-keyed IntentWatcher rescues — step-4 temp+rename bumps mtime) + burns the full cap=3 as genuine completed moves + emits a misleading `quarantined-realloc-cap-exhausted` event. Not a brick, not fleet corruption.

Tests missed it: `TestRealloc_LoopPath_RespawnsOnNewPort` fabricates a strictly-newer UpdatedAt production never produces; `TestRealloc_EqualTimestampSnapshot_TreatedStale` models only the watcher-raced case where the cache already holds newPort.

## FIX (option b — safe, no UpdatedAt/strict-mode semantics change)
In `handleReallocApplied`'s reallocated branch (`supervise_realloc.go:436-455`): the `reallocSnapshotIncomingStale` case must ALSO call `c.patchCachedReallocatedPort(task, newPort)` (KEEP the info event for the genuinely-stale operator-raced case). Merge the intent so that ONLY `reallocSnapshotIncomingNewer` does the whole `handleReapScan` apply; EVERY other order (Stale/Equal/Unorderable/absent) does the targeted `patchCachedReallocatedPort(task, newPort)` — the port MUST always land on newPort (the realloc's authoritative move). The patch is idempotent (a no-op when the cache already holds newPort — the watcher-raced case the old comment assumed) and touches ONLY this descriptor's port+argv(+serena RuntimeSpec), never other descriptors or other fields, so it cannot clobber a genuinely-newer operator update to a DIFFERENT descriptor. Update the stale-branch comment to state the port is patched (not "the cache already carries it").

### Regression test (fable's probe shape — the blind spot)
`TestRealloc_EqualTimestamp_CommonPath_PatchesToNewPort` (or similar): cache intent = descriptor@oldPort with `UpdatedAt=T`; carried snapshot = descriptor@newPort with `UpdatedAt=T` (IDENTICAL — exactly what the worker reads post-step-4 since step-4 doesn't stamp UpdatedAt); drive through the running loop+worker; assert the respawn `--port == newPort`. Non-vacuity: revert the fix (stale branch skips the patch) → assert the test FAILS (respawn=oldPort) → restore.

## Deferred to follow-up (NOT this round — has interactions)
- Option (a): stamp `f.UpdatedAt` in the step-4 mutate (matches register precedent → carried snapshot strictly-newer → the intended WHOLE-apply, picking up other descriptor changes too). RISK: the strict-mode CLI seed keys on `intent.updated_at` vs binary mtime (CLAUDE.md) — a realloc bumping updated_at past a freshly-installed binary's mtime could suppress a pending --strict-mode seed on the next cold start. Needs careful design → follow-up work-item.
- Sol/Terra's monotonic revision/generation counter on SupervisorIntentFile (the trap-free ordering) → follow-up.
- fable P3s: no direct unit test for CloneIntentWithReallocatedPort (sibling-untouched/source-unmutated/serena-RuntimeSpec); FIX-6 permanently-wedged-worker silent ~30s flap (worker lease/epoch is the real cure) — both follow-up.

## fable FINAL confirmation (first-hand): P1 CLOSED, PASS, fleet-safe, ready for commit → bot → merge → deploy
fable re-proved the non-vacuity itself (neuter :459 → respawn 9401 want 9470 = its exact round-4 probe → restore → green). New-hole audit: NO P1/P2. Two narrow P3 residuals (non-blocking, tracked here):
- **P3-A (early-confirm of an unrelated pending-reap):** the synthetic cache-derived snapshot fed to handleReapScan on the patch path can early-confirm an UNRELATED still-absent-and-live replace-in-place `pendingReap` candidate, collapsing its verification window. Pre-existing in kind on the Unorderable branch; 4b extends reachability to the common path. Needs a replace-in-place removal racing a realloc; worst case an unnecessary terminate+respawn BOUNCE of the re-added daemon — NEVER a wrong port. Follow-up.
- **P3-B (operator re-ports THIS descriptor inside the realloc window):** if a genuinely-newer operator intent deliberately re-ports THIS exact crashing descriptor within the seconds-wide realloc window, the patch overrides the cache with the worker's newPort and the mtime-driven IntentWatcher won't re-fire without a new disk write → operator's port deferred until the next intent write/restart. Correct-by-design ("realloc is the authoritative move"), observable via `realloc-stale-snapshot-skipped`, extremely narrow (the alternative — skip — was the proven P1). Follow-up.

## Commission COMPLETE. Next: MY clean pre-push gate (quiet) → commit → bot → merge → deploy.
