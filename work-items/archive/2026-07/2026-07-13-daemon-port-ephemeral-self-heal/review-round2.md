# D commission round 2 — verdict: REVISE (Sol + Terra; fable pending)

Date 2026-07-13. Round-2 fixed round-1's P1/P2; round-2 review finds MORE concurrency P1s in the L1 self-heal.

## CLOSED (Sol + Terra agree)
- P1-2 LSP-brick: registry compensation revert under the flock held across step3+step4+revert. CLOSED. (I proved non-vacuity myself.)
- P2-2 setup remedy: mcphubEffectivePools includes the serena upstream band (ExternalPort+offset); computeEphemeralRangeFix lands above it. CLOSED.
- P2-3 pool-exhausted stop-race: StBackoffWaiting guard added. CLOSED.
- P1-1 (original): blocking Post + disk read removed from the SUCCESS handler (worker reads off-loop, PostCtx). CLOSED on that path.

## STILL-OPEN / NEW (Sol + Terra converge)
### P1 — stale-snapshot lost-update (Terra + Sol)
`supervise_realloc.go:296,369`: the worker reads a FULL intent snapshot off-loop; the loop applies it via handleReapScan WITHOUT a generation/revision check. An operator/reconciler intent update between the worker's read and the loop's apply is OVERWRITTEN. Fix: carry an intent revision/generation + reject stale, OR apply a version-checked TASK-LOCAL delta (not the whole snapshot).

### P1 — the loop CAN STILL BLOCK (Terra + Sol)
`emitBindAccessDeniedTerminalOnce` (called ON-loop) → `emitBindAccessDenied` runs the Windows process-identity resolver + the FIRST uncached `netsh` probe (`ephemeralRangePortContains` sync.Once, setup_ephemeral_range_windows.go:57-72) ON the loop goroutine; `events.Emit` also documented as potentially blocking. The comment "no foreign-holder probe" contradicts the call (it passes `port` as `foreignHolderPort`). So P1-1's loop-block is only PARTIALLY closed — the terminal-event emission path still does blocking I/O on the loop. Fix: warm/probe + resolve off-loop, emit with immutable pre-computed data.

### P1 — successful-persist + intent-read-failure recreates the LSP brick (Sol)
If the two-store write SUCCEEDS but the worker's fresh intent READ (for the handoff snapshot) fails → the loop gets no snapshot → strand/brick variant. Verify + fix.

### P2 — backstop consumes the cap but can't relaunch a wedged worker (Terra + Sol)
If the worker wedges (registry lock / Emit / foreign-holder probe / PostCtx), `reallocInFlight` stays set; backstop restarts consume the reallocation cap before dedupe + CANNOT launch a replacement worker → daemon → crash/quarantine instead of a new port. Fix: worker lease/epoch with timeout REPLACEMENT; consume the realloc budget only for a COMPLETED allocation; post the successful result BEFORE nonessential telemetry.

### P2 — dwell resets too early (Terra)
`startReallocDwell` LoadOrStore preserves healthySince; a leave/re-enter StRunning between ticks is invisible → next tick clears windows without a fresh continuous 60s dwell → forever-flap variant. Fix: reset the dwell clock on EVERY transition OUT of StRunning, not only when a tick observes it.

### P2 — ABA stale-timer hole + legacy canonicalization accepts ambiguous duplicate aliases (Sol)

## Disposition (pending fable)
Findings are TRACTABLE + specific (not an information-theoretic edge-mine). Round-2 CLOSED the worst (LSP-brick, P2-2/3, original P1-1); the closed/held list is growing → convergent trend. LEAN: round-3 fix-all (stale-snapshot version-check/delta; move netsh+resolver off-loop for the terminal event; worker lease/epoch; dwell-reset-on-leave; ABA/alias). FALLBACK if fable finds L1 is a deeper rabbit hole: SPLIT — ship L2 (setup detect, CLOSED) + fix+ship L3 observability, DEFER L1 self-heal as a careful follow-up (the host netsh remedy + L2 warning already prevent collisions without L1's risk). Decide with fable's verdict.
