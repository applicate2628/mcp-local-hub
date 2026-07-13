# D round-7 — global monotonic arm-epoch (fixes the round-6 ABA both reviewers found)

Round-6 (respawnArmGen) CLOSED Sol's round-5 stacked-timer finding, but its cleanup (`Delete` + per-task reset-to-1 in `nextRespawnArmGen`) INTRODUCED an ABA collision that Sol (P2) + Terra (HIGH-BROKEN) both flagged.

## The ABA (both reviewers, converged fix)
`nextRespawnArmGen` does Load+increment+Store per-task, and `clearRemovedTaskRuntime` Deletes the entry. When a task is REMOVED then RE-REGISTERED under the SAME canonical name, the new first arm gets generation **1 again** (deleted entry → Load-miss → 0 → +1 = 1). A stale pre-removal respawn timer captured at generation 1 then MATCHES the new incarnation's generation-1 arm (`g == armGen`) → it is NOT dropped → emits a redundant EvTimerDue against the NEW incarnation, potentially shortening its backoff. Plus a delete/re-arm window where a map-miss `Load` "falls through as valid" and a stale timer fires. Reintroduces the redundant-respawn class for EVERY daemon sharing this mechanism.

## FIX (both reviewers' exact recommendation: non-reusable epoch, never reset)
Replace the per-task-reset counter with a GLOBAL monotonic atomic epoch that never repeats:
- Add `respawnArmEpoch atomic.Uint64` (a single controller-wide counter, NOT per-task).
- `nextRespawnArmGen(task)`: `gen := c.respawnArmEpoch.Add(1)` (globally-unique, ever-increasing); `c.respawnArmGen.Store(task, gen)`; return gen. (The per-task `respawnArmGen sync.Map` still maps task → its LATEST global epoch, for the fire-time compare.)
- Arm-time: capture `armGen` = the returned global epoch.
- Fire-time guard (after the StBackoffWaiting state re-check): `v, ok := c.respawnArmGen.Load(armKey); if !ok || v.(uint64) != armGen { drop (emit daemon-respawn-timer-superseded); return }`. **DROP on miss** (`!ok`) as well as on mismatch — a missing entry means the task was cleaned up (stale timer) or never armed; either way this timer must not fire. This closes Sol's "map-miss falls through as valid" window.
- Cleanup (`Delete` in `clearRemovedTaskRuntime`): now SAFE — a re-registered task's new arm gets a FRESH high global epoch (never 1-again), so a stale pre-removal timer captured the old epoch sees either a miss (→ drop) or the new higher epoch (→ drop). ABA eliminated.

Net: arm generations are globally unique and monotonic, so a stale timer's captured epoch can NEVER match a different arm's epoch — the compare is exact-identity, not a reused small integer. Normal backoff (arm→fire→re-arm→fire) is unaffected (each arm is the latest global epoch at its own fire time). Stacked coincident arms still collapse to the latest (higher epoch supersedes). No ABA on remove→re-register.

## Tests (non-vacuous — keep the round-6 tests + ADD)
- Keep `TestRespawnArmGen_StackedTimersCollapseToOne` + `TestRespawnArmGen_NormalBackoffFiresEachArm` (still valid with the global epoch).
- ADD `TestRespawnArmGen_RemoveReregisterNoABA` (both reviewers asked for it): arm a timer for task T (global epoch e1); clearRemovedTaskRuntime(T); re-register T (new arm → global epoch e2 > e1, NOT 1); fire the STALE e1 timer → assert it is DROPPED (does NOT fire against the new incarnation). Non-vacuity: revert to the per-task-reset counter → the stale gen-1 timer matches the new gen-1 arm → fires (ABA observable) → restore.

## Do NOT change (fable REFUTED in round-5; unchanged in round-6/7)
F3 stale-classification, F3 operator-stop, F2 on-loop persist/raw errStr. And the held-under-attack list.

## Verify
build/vet clean; `go test -count=1 ./internal/api/ ./internal/cli/` + `-tags=test_state_path_env` (-p 1); `go test -race -count=3 -run 'Realloc|PreSpawn|PortGate|Backoff|Timer|Dwell|LostChild|RespawnArm'`. Back up live intent; byte-identical after. Sweep only go-build/Temp mcphub.

## After round-7: re-verify → Sol + Terra confirm the ABA is CLOSED (they found it) → clean gate → commit round-5+6+7 → push → re-trigger bot → PASS → merge → deploy.
