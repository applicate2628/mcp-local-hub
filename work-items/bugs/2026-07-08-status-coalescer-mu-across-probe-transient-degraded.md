---
severity: low
filed: 2026-07-08
context: post-deploy tail of the hub-flap fix (PR #510); predicted by fable acceptance INFO-1
---

- **status:** fixed
- **fixed-by:** PR #514 (`c2171b44`) - status coalescer single-flight plus serve-stale.
- **HEAD reconciliation (2026-07-09):** Verified against master `63b6a008`; see `TRIAGE-2026-07-09.md` for code/test evidence.

# Rare transient degraded blips: coalescer holds `mu` across the 3s probe → stacked status callers can exceed the 5s IPC timeout

## Symptom (live, post-PR #510 deploy)
Two single degraded→recovered blips in the first ~25 min after the merged-master redeploy
(2026-07-08 01:32:48 recovered in 0.76s; 01:35:53 recovered in 1.47s), on a host still loaded
by CI-scale test sweeps. Each is 3 consecutive `supervisor IPC status: read status response:
i/o timeout` then immediate recovery; agents do NOT hang; status latency otherwise ~0.3-0.6s.
Contrast pre-fix: 409 degraded events over 4 days with continuous flapping + agent hangs.

## Mechanism (fable acceptance INFO-1, confirmed by the event shape)
`statusPortOwnersCoalescer.Get()` (internal/cli/supervise_status.go) holds `mu` ACROSS the
ctx-bounded netstat probe (up to 3s). Worst case under a slow-netstat window: caller A runs a
full 3s probe; caller B waits 3s for the lock; if the fleet generation bumped mid-probe (or
TTL expired), B then runs its OWN 3s probe → ~6s > the 5s status-IPC client timeout → the
restart-watcher counts errors → one transient degraded, self-healed on the next tick (the
result IS cached; TTL 1s). Supervisor events show only the normal 2s ipc-command cadence
around the blips — no daemon churn — pointing at netstat slowness + caller stacking.

## Fix direction (small, single-owner)
Don't make waiters re-probe behind a held lock: serve-stale-while-refreshing — `Get()` returns
the cached snapshot immediately when a probe is already in flight (even if TTL/generation says
stale), and the in-flight prober refreshes the cache for the NEXT call. A status row built from
a ≤1-TTL-stale owner map is the documented, self-correcting degraded mode (fable INFO-1 calls
it acceptable); a >5s IPC stall is not. Alternative: bound the lock wait (TryLock + stale-serve).
Keep: error caching, generation stamping semantics (pre-probe sample), nil-coalescer path.

## Acceptance
Under a deliberately slowed netstat (injectable snapshotFn sleeping 3s) + N concurrent status
callers: every call returns < 2s; no caller ever runs a second serial probe behind a completed
one; degraded events under synthetic load = 0. Live falsifier: zero degraded blips over 24h of
normal operation (measure before/after).


## Closure (2026-07-08)

Three mechanisms shipped in PR #514, each pinned by a RED-proven test:
1. **Single-flight + serve-stale** — one probe ever in flight; warm TTL-stale
   cache served immediately with background refresh; cold waiters join the
   shared probe (no serial re-probes → no stacking past the IPC deadline).
2. **Generation-join, capped** — a fleet-generation change joins the probe for
   a FRESH result (read-your-writes after respawn), capped at ONE probe wait
   (3s < 5s IPC); a mid-probe fleet race returns the freshest completed result
   and kicks a background re-probe.
3. **TryEmit for the ipc-command audit row** — the ACTUAL root of the
   minutes-long windows (lock-trace verdict H3): handleIPCConn emitted the
   per-command audit with a blocking Emit, and the supervisor-events.log
   flock (contended with the GUI) stalled every IPC response behind an audit
   write. The 2s-cadence observability row is now best-effort; kill/reap
   audit rows keep blocking semantics.

Live proof: differential probes during a degraded window showed CLI status
fast (0.97-1.75s) while the watcher timed out — client-sparse vs
continuous-sampling tail behavior that H3 explains; after the flock fix the
decisive watch held ZERO degraded events for 45 minutes across ~9 auto-cleanup
ticks (the exact trigger of the prior 4-5.5-minute RED windows).
