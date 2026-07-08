---
status: closed (fixed by PR #510, merged 2026-07-07 23:35Z as 1aeec052; hotfix-deployed 2026-07-07 23:10 local, final master redeployed 2026-07-08 04:31 local)
severity: medium
filed: 2026-07-07
context: GUI hub health flap; root-caused by Opus against live logs + HEAD
---

# Hub color flaps RED↔GREEN — supervisor status IPC does a fresh netstat per call (no coalescing) → restart-watcher timeout

## Symptom (operator-reported, live)
The GUI/tray hub health color "constantly changes" (red↔green). Servers screen: all
checkboxes ON but the toggles are inactive (a pending/degraded state). Pre-existing —
first `restart-watcher-status-degraded` in `hub-mcp.log` is **2026-07-03T10:15Z** (NOT
introduced by the 2026-07-07 A1 v0.4.23 deploy; the fleet itself is healthy: 20 Running,
0 Quarantined, 0 lost-child symptoms).

## Root cause (data-pinned)
- The GUI restart-watcher (`internal/api/daemon_restart_watcher.go`, 2s interval, degrades
  after 3 consecutive status-fetch errors → logs `restart-watcher-status-degraded`) and
  `/api/status` both source from `a.DaemonStatusSnapshot` (`internal/api/health.go:291`),
  a TTL+singleflight cache with `daemonsTTLMs = 2000` (health.go:201).
- On a cache miss the snapshot calls the supervisor status IPC (`DialSupervisorIPCStatus`,
  5s timeout, supervisor_ipc_status_client.go:56) → the supervisor's `"status"` handler
  (`supervise.go:1705` → `supervisorStatusDaemons`, supervise_status.go:35) does a **fresh
  `netstat -ano` snapshot (`loopbackPortOwnersSnapshotFn`, supervise_status.go:59) + a
  21-daemon resolution on EVERY call** — there is NO supervisor-side coalescing/cache.
- The client TTL (2s) == the restart-watcher interval (2s), so every restart-watcher tick
  MISSES the cache → hits the raw supervisor IPC → netstat (~1-2s measured; `mcphub status`
  timed at 0.9/1.6/2.6s). Under contention (restart-watcher + `/api/status` dashboard poll
  + auto-cleanup-tick + any manual `mcphub status`) the serialized netstat-backed IPC
  round-trip occasionally exceeds the 5s status timeout → `i/o timeout` → 3 consecutive →
  degraded → the hub color flaps. 360+ degraded events logged over ~4 days.

## Fix (single-owner, anti-layering-compliant)
The netstat is a SUPERVISOR-side operation, so the supervisor must OWN "how often to
netstat for status." Add a short-TTL (~1s) + singleflight coalescing cache to the
supervisor's `"status"` IPC handler / `supervisorStatusDaemons`, so concurrent AND rapid
status IPC calls share ONE netstat snapshot + resolution → every status IPC returns fast →
no >5s spikes → no restart-watcher timeout → hub stays green.

**Anti-layering (AGENTS.md "No logic duplication / no fix layering"):** the client
`daemonsTTLMs=2000` cache's stated purpose is *"amortizing the wmic/netstat cost"* — the
SAME concern the supervisor cache would now own. Two independent TTLs both bounding
netstat-frequency = layering. Resolve to ONE owner: the supervisor owns netstat-coalescing;
the client cache is re-scoped to its DISTINCT concern (collapsing a burst of concurrent
HTTP `/api/status` requests into one IPC — a fan-in singleflight, not a netstat-freshness
TTL) or thinned, so the two do not duplicate the amortization decision. Fable (arbiter) to
finalize the exact split so no cache layer re-owns the other's concern.

## Secondary (verify after the flap fix)
- Servers-toggles-inactive: likely a downstream effect of the flapping/degraded status
  (the dashboard can't get a stable snapshot). Re-check once the flap is gone; if it
  persists, root-cause the toggle-disable predicate separately.

## Not-a-cause (ruled out)
- The 2026-07-07 A1 deploy (flap predates it by 4 days). The fleet + lost-child class
  (healthy, 0 symptoms). Duplicate supervisor/GUI (exactly one each).


## Closure (2026-07-08)

Fix shipped as PR #510 (`1aeec052`): supervisor-side `statusPortOwnersCoalescer`
(TTL 1s, mutex-singleflight, fleet-generation invalidation sampled pre-probe) +
ctx-bounded netstat/proc walks (3s probe < 5s IPC timeout) across win/linux.
Deploy-proven twice: the pre-merge hotfix (f0a08773) took status IPC from
0.9-4.7s (with timeouts) to ~0.5s stable and held ZERO degraded events for
hours under load; the merged-master redeploy showed one transition-window
degraded (74s after supervisor start, recovered in 0.76s) and none after
settle. The dominant HOST netstat term was the @mui/mcp orphan pile (125
procs) — swept + adopted into the hub (`mui-mcp`@9301 via the new `mcphub
adopt`, PR #513), so both the amplifier and the hub-side defect are gone.

Secondary (Servers-toggles-inactive) was root-caused separately as the
LSP-router per-client-disable gap — fixed in PR #512, bug
2026-07-07-lsp-router-relay-entries-ignore-per-client-disable.md.
