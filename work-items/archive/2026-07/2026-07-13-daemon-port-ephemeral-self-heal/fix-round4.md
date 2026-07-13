# D round-4 fix spec — Sol D3b (mandatory acceptance) + Terra D3 REVISE; fable framed bounded but 2/3 say fix

Round-3 verdict: fleet-killer CLOSED (all agree). fable (arbiter) PASS-bounded; **Sol (mandatory acceptance) + Terra both REVISE with concrete code-cited findings**. I verified the FIX-3 persist+read-fail residual is REAL (bounded ≤60s, not permanent — but real). Rule: no merge before the mandatory reviewer clears → round-4 closes the real findings, most cheap.

## FIX-3b (Sol+Terra, verified real) — persist-success + intent-read-failure → stale-cache respawn → exit-1 for ≤60s
**Root:** on a successful realloc, `handleReallocReq` (`supervise_realloc.go:327-331`) reads the WHOLE fresh intent via `reapIntentReader()`; if that read FAILS, `freshIntent=nil` → the body omits the intent → `handleReallocApplied` (`:415`) skips the cache refresh → the respawn uses the STALE cache (argv=old) while disk intent+registry=new → `classifyLSPPortMismatch` (intent≠argv) → exit-1 (indistinguishable from a genuine misregistration) until the ≤60s IntentWatcher cache-swap rescues it. A few crash counts accrue; bounded but real, and relies on the 60s watcher.
**Fix (robust — Terra's suggestion; removes the read dependency entirely):** the worker ALREADY knows `newPort` (the `reallocFn` return value). Carry `newPort` (+ the task key) in the event body ALWAYS — not only when the whole-intent read succeeds. In `handleReallocApplied`, when the full fresh-intent snapshot is absent (read miss), patch JUST this descriptor's cache entry — Port field + `--port` argv (+ serena RuntimeSpec) — to `newPort` directly (a targeted, loop-owned cache mutation), so the respawn carries argv=new even when the disk read failed. Keep the whole-intent apply (via handleReapScan) as the path when the snapshot IS present. Net: the respawn is always on newPort; the ≤60s watcher becomes a backstop, not the primary rescue. Add a test: successful realloc + forced reapIntentReader failure → assert the respawn argv == newPort (not old). Non-vacuity: neuter the port-patch → assert exit-1/old-port → restore.

## FIX-4b (Sol+Terra) — reallocSnapshotIsStale timestamp-only is not a strict ordering
`reallocSnapshotIsStale` (`:498-507`) rejects only strictly-After parseable timestamps; EQUAL `UpdatedAt` applies and parse-fail applies fail-open. Flock serialization orders writes but does not make wall-clock timestamps unique/monotonic. fable framed the fail-open as "= round-2 baseline, bounded"; Sol+Terra want a strict ordering.
**Fix:** add a monotonic revision/generation to `SupervisorIntentFile` (a counter bumped on every intent write — the intent owner already does a flock-serialized RMW, so bump-under-lock is trivial + strictly increasing) and compare THAT in reallocSnapshotIsStale (reject `incoming.rev <= current.rev`). If adding a persisted field is too invasive this round, at minimum treat EQUAL-timestamp as stale (reject `<=`, not just `<`) AND on parse-fail re-arm a refresh instead of applying (Terra), and document the residual. Prefer the generation. Test both the equal-timestamp and parse-fail cases.

## FIX-5b (Sol+Terra) — dwell continuity: reset on every StRunning departure, not only on a tick observation
`startReallocDwell` (`:513-517`) LoadOrStore preserves healthySince; `runReallocDwellTick` (`:525,:543-550`) resets only when a periodic tick happens to sample a non-Running state. A leave-and-reenter StRunning between ticks accumulates non-continuous dwell → can clear the crash/realloc windows without a genuine continuous 60s healthy → weakens the quarantine gate for a flapping daemon.
**Fix:** reset/clear the dwell entry (healthySince) on EVERY transition OUT of StRunning (in the SM transition, not the sampling tick), so only genuinely continuous StRunning accrues dwell. Test: leave+reenter between ticks → assert the window is NOT cleared early.

## FIX-6 (Sol+Terra) — wedged worker consumes cap without a replacement allocation
`maybeHandleBindRefusedExit` records a reallocation (cap slot) BEFORE dispatch; `tryDispatchRealloc` (`:242-249`) treats an existing `reallocInFlight` marker as success without launching a replacement; a dropped channel dispatch likewise consumes an already-recorded slot. A wedged single worker → each backstop old-port retry burns a cap slot → crash/quarantine without any replacement allocation.
**Fix (prefer the cheap variant):** record the reallocation cap slot only on a COMPLETED allocation (move the record to after the worker posts a non-Failed outcome), OR add a worker lease/epoch with timeout replacement. If the lease is too invasive this round, do the "count only completed allocation" + ensure a dropped/failed dispatch does NOT burn a slot. Test: dropped dispatch + failed outcome do not consume cap.

## FIX-7 (Sol NEW) — pool-exhausted terminal event can claim a quarantine that never happened
`handleReallocReq` emits action `quarantined-pool-exhausted` BEFORE posting the result; `handleReallocApplied` may then NOT quarantine (operator stop moved the daemon out of StBackoffWaiting) → the emitted terminal event lies about a quarantine. Fix: emit the terminal action AFTER the loop decides the real outcome (or emit the actual applied outcome), so the event never claims a quarantine that the StBackoffWaiting guard skipped.

## FIX-8 (Sol+fable) — stale/contradictory comments
- `tryDispatchRealloc` comment says a delivered/in-flight request means the caller need not arm a fallback — but the caller ALWAYS arms one (P2-1 always-arm). Fix the comment.
- `ReallocateDynamicPoolPort` compensation comment still says the uncompensated LSP mismatch "exits 1 forever" — contradicts FIX-3's exit-3 classification. Fix.
- fable P3-3 (on-loop warm-range-check + flock events.Emit — pre-existing pattern; note only).

## FIX-9 (fable P3 test pins)
1. Pin FIX-1: a panicking `reallocForeignHolderFn` in the cap-exhausted/fixed-global terminal-emit tests so a regression re-passing `port>0` is caught.
2. Strengthen `TestRealloc_MalformedOutcomeBody_DecodesFailed` so a zero=Reallocated inversion would fail it (assert the timer-delay/observable difference).
3. (fable P3-4 LSP MCPHUB_SUPERVISOR_INTENT_PATH channel symmetry — nice-to-have follow-up, note only, Windows GA env-immune.)

## Verify
build/vet clean; `go test -count=1 ./internal/api/ ./internal/cli/` (+ `-tags=test_state_path_env`); `go test -race -count=5 -run 'Realloc|ClassifyLSP|Bind|Ephemeral|Dwell'`. Back up live supervisor-intent.json before subagent tests; sweep only go-build/Temp mcphub.exe.

## After round-4: final re-commission (Sol + Terra + fable). Ship to bot only when Sol (mandatory) + Terra clear + fable holds PASS.
