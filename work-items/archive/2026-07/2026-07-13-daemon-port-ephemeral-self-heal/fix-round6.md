# D round-6 — single-timer-owner (respawnArmGen), the one genuine round-5 imperfection

Round-5 commission: fable (arbiter, real-code trace) = round-5 FLEET-SAFE, ship-ready; it REFUTED Sol's P1 severity + both Terra F3 P2s + showed both F2 P2s are consistent-with-the-accepted-pool-exhausted-branch. The ONE genuine imperfection is Sol's redundant coincident retry timer — REAL (the double/triple-arm exists) but P3 (non-accelerating, self-limiting: coincident ~30s timers cluster; the fire-time stale-state re-check drops moved-state timers; portGateInFlight + reallocInFlight dedup throttle to one worker per cycle; reallocFailCount increments once per completed run → escalation at bound+1 in ~4×30s as designed). Fix it to satisfy Sol (mandatory acceptance) AND clean the LATENT double-timer the bind-refused path has carried since round-1.

## The imperfection
`preSpawnPortGateHold` → `holdSpawnInBackoff` arms a 30s timer (supervisor_controller.go:4228) BEFORE the worker runs; then `dispatchDynamicPoolRealloc` arms the backstop (supervise_realloc.go:289); then a quick `reallocOutcomeFailed` re-arms a third (:680). Stacked coincident timers. Non-accelerating (per fable's trace) but redundant.

## FIX (fable's design — generation-guarded single-timer-owner, local + additive, NO per-task timer registry)
Add a per-task monotonic backoff-arm generation `respawnArmGen sync.Map` (canonical taskName -> uint64), mirroring the existing `pid_generation` pattern.
- `armRespawnBackoffTimer`: at arm time, bump `respawnArmGen[task]` and CAPTURE the new value in the timer's closure.
- At fire time, AFTER the existing StBackoffWaiting stale-state re-check (armRespawnBackoffTimer:~3874), drop the EvTimerDue if `respawnArmGen[task]` has advanced past the captured value (a newer arm superseded this one).
- Net: all stacked/coincident timers collapse to the single most-recent arm → exactly one effective EvTimerDue per episode. This also fixes the bind-refused path's latent double-timer (P2-1 always-arm backstop + the respawn arm), a real cleanup.
- Reset/cleanup `respawnArmGen[task]` on the same lifecycle points the other per-task maps clear (Remove/quarantine-leave/dwell) to avoid unbounded growth — OR leave it (a uint64 per task is negligible; but clear on Remove for hygiene).

## Test (non-vacuous)
A test arming the respawn timer TWICE (two arms in the same window), firing both EvTimerDue: assert only ONE effective respawn/dispatch occurs (the second, superseded timer is dropped by the generation guard). Non-vacuity: remove the generation guard → assert BOTH fire (double-dispatch/double-arm observable) → restore.

## Do NOT change (fable REFUTED — leave as-is)
- F3 stale-classification: do NOT add a loop-side owner re-probe (would regress the loop-no-blocking-I/O invariant). The bounded 1-cap-slot waste is acceptable.
- F3 operator-stop: the StBackoffWaiting respawn guard already prevents the respawn; the persisted new port is valid on re-enable. No change.
- F2 on-loop persist + raw errStr: consistent with the accepted pool-exhausted branch; no change (unless you also change pool-exhausted for symmetry — out of scope).

## Verify
build/vet clean; `go test -count=1 ./internal/api/ ./internal/cli/` + `-tags=test_state_path_env` (-p 1); `go test -race -count=3 -run 'Realloc|PreSpawn|PortGate|Backoff|Timer|Dwell'`. Back up live intent; byte-identical after. Sweep only go-build/Temp mcphub.

## After round-6: re-verify → quick fable+Sol confirm (Sol must clear the timer finding) → clean gate → commit → push → re-trigger bot → PASS → merge → deploy.
