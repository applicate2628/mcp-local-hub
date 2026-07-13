# D commission round 1 — verdict: REVISE (NOT fleet-safe). Sol + Terra + fable converge; arch PASS missed runtime.

Date 2026-07-13. Reviewers: architecture-reviewer (PASS, structural — MISSED the runtime concurrency), Terra (REVISE, concurrency), Sol (REVISE, P1 lifecycle/persistence/host-mutation), fable (REVISE, NOT fleet-safe — 2 P1 + 3 P2).

## P1 (blockers)
### P1-1 — blocking Post from a loop handler → whole-supervisor self-deadlock (fable)
`supervise_realloc.go:348-352` handleReallocApplied (reallocated branch) → `supervisor_controller.go:1026` refreshSupervisorIntent → `c.eventLoop.Post(...)` = BLOCKING send to the loop's OWN main channel, executed ON the loop goroutine. If the 1024 buffer is full (this feature's storm: many proxies collide at once → EvChildExit bursts + realloc outcomes + IntentWatcher scans + status polls), the loop blocks sending to itself FOREVER → whole fleet unmanaged, IPC wedges, liveness can't help (lock holder alive). Violates the loop's OWN contract (`supervisor_event_loop.go:233-241`: "PostSelf is the ONLY safe way to post from inside a handler"; PR #236 P2). Secondary: `reapIntentReader` disk read runs on the loop under a "NEVER runs blocking I/O" comment (`supervise_realloc.go:331`).
**Fix:** move the fresh read + refresh onto the worker (post the parsed snapshot in the evReallocApplied body), OR PostSelf-based scan-apply. NEVER blocking-Post from a handler.

### P1-2 — partial two-store transaction permanently BRICKS an LSP daemon (fable/Terra/Sol)
`reallocate_dynamic_pool.go:76-120`: step 3 registry Save() then step 4 intent write, NO compensation on step-4 failure. If step 4 fails (DACL/disk/AV, or supervisor dies between — the design's own §E case): registry=newPort, intent argv/Port=oldPort. LSP proxy startup fail-closed check (`daemon_workspace.go:192`: entry.Port != portFlag → exit 1, "run mcphub register") → exit 1 (NOT exitBindRefused) → normal crash path → 10 crashes → quarantine → parole → same exit 1 → re-quarantine FOREVER. Self-heal never re-drives (keys on exit-3 only). GUI LSP router (`lsp_routing/resolver.go:325-333`) routes to newPort where nothing listens. The design's "relaunch old → self-heal again" is FALSE for LSP (only serena survives — its self-validate checks argv vs INTENT, still self-consistent). The crash-consistency test is serena-only + asserts allocator-skip, never daemon startability.
**Fix:** on step-4 failure compensate (revert the registry row under the still-held flock), OR reconcile row↔intent at spawn, OR make the LSP proxy's port-mismatch exit self-healing/exit-3.

## P2
### P2-1 — unbounded flock waits + no-timer hold + dedup → stranded daemons, recovery = full restart only (fable/Terra)
`reallocate_dynamic_pool.go:43` reg.Lock() + `:86` MutateSupervisorIntentIfChanged flock = blocking, NO deadline, on the SINGLE worker. If a co-process wedges holding workspaces.yaml.lock, the worker blocks forever; the daemon is parked StBackoffWaiting with NO timer (holdInBackoffNoTimer + successful dispatch arms nothing); evReallocApplied never arrives; reallocInFlight never clears → tryDispatchRealloc keeps returning true. Nothing restarts a StBackoffWaiting daemon. Recovery = full supervisor restart.
**Fix:** deadline/TryLock the two flocks with a Failed outcome on timeout, OR always arm a long backstop fallback timer even on successful dispatch (the stale-timer re-check at armRespawnBackoffTimer:3825 makes a duplicate harmless).

### P2-2 — `--fix-ephemeral-range` moves the OS range INTO the serena upstream band → new unhealable theft class (fable)
`setup_ephemeral_range_windows.go:117-181`: mcphubEffectivePools = {9121-9149, serena ext ~9150-9205, LSP 9400-9599} → computeEphemeralRangeFix picks newStart=9600. But serena UpstreamPort = ExternalPort + 10000 (NativeHTTPInternalPortOffset) = 19150-19205 — INSIDE the new range (was outside 1024-15000). OS can now steal a serena UPSTREAM port → serena backend child exit 1 → crash/quarantine, NO self-heal (upstream failures unclassified). The remedy the L3 event recommends makes things worse.
**Fix:** include pool+offset bands in the pool set fed to computeEphemeralRangeFix (start ~19300), OR default to 49152.

### P2-3 — pool-exhausted outcome overrides a raced operator stop (Terra/fable)
`supervise_realloc.go:364-389`: the pool-exhausted branch stores StQuarantined UNCONDITIONALLY — lacks the `smStateIs(task, StBackoffWaiting)` guard both sibling branches have. Operator `mcphub stop` → StIdle → worker posts pool-exhausted → loop stamps the STOPPED daemon StQuarantined. Operator stop overwritten.
**Fix:** add the StBackoffWaiting guard.

## P3 (fable)
exit-3 = Windows CRT abort() collision (note by the constant); zero-value outcome = Reallocated (make zero = Failed); listener-theft WSAEADDRINUSE rarely reaches self-heal (F1 gate holds spawn first — coverage narrower than claimed, document); stale serena registry row after crash-3-4 when thief releases (1 leaked port); fixed-global terminal-dedupe never clears on non-quarantine recovery (suppresses next episode's event); legacy bare task-name rows fail the worker's exact `==` match → burns cap; GUI daemon-card degraded reason NOT implemented (spec gap).

## Test blind spots (fable)
1. The reallocated-outcome LOOP path (P1-1 code) is NEVER executed by any test (reapIntentReader nil in tests). 2. No reallocOutcomeFailed test. 3. No LSP crash-consistency+STARTABILITY test (P1-2). 4. No stale-generation exit-3 test. 5. No full-reallocCh/dispatch-drop test. 6. No operator-stop-race test (P2-3). 7. No malformed-outcome-body test. 8. No exit-3-at-non-StRunning test.

## Held up under attack (Sol/Terra/fable agree — do NOT re-touch)
classification boundary (exit-3 + dynamic-pool); cap accounting (no crash-increment within cap); dwell gate (transient StRunning doesn't reset — tested + I proved non-vacuity); never-kill-foreign (read-only PID+basename probe); zero client-config churn; stale-generation guard blocks reallocating a live daemon; cross-daemon double-alloc prevented (single worker + flock + row-skip + OS probe); lock order registry→intent (no ABBA).

## Disposition — round-2
Fix P1-1 (PostSelf/off-loop refresh), P1-2 (compensate the two-store write), P2-1 (deadline flocks or backstop timer), P2-2 (upstream band in the range fix), P2-3 (StBackoffWaiting guard) + the P3s + the 8 test blind spots (esp. the reallocated-loop-path e2e + LSP-brick startability test). Then re-run Sol+Terra+fable.
