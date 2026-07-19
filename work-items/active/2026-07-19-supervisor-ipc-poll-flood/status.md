# Supervisor IPC status-poll control-plane: stop flooding it

Template: full-delivery (requiresLead: true) · Lead: main conversation · Opened: 2026-07-19
Epic: 2026-07-16-productization-gui-solidify (Phase-0, admitted ahead of items 4-6 per $product-manager brief)

## Goal
Stop the read-only status-poll stream (GUI 5s StatusPoller + restart-watcher, ≈3/sec) from flooding the
single-threaded supervisor IPC control plane. Two symptoms, one root cause:
- **Fix A (bug 2026-07-16, P1 audit-flood):** every IPC request emits an unconditional `ipc-command`
  audit row (`supervise.go:~1744-1756`) → 100% of `supervisor-events.log` is poll noise → real lifecycle
  events evicted before an operator can read them.
- **Fix B (bug 2026-07-19, congestion → dashboard RED):** the poll flood starves the IPC connection
  hello-write on the single-threaded listener → client i/o timeout → `/api/status` STATUS_FAILED → the
  Dashboard hub indicator recurs RED (operator's twice-repeated "хаб постоянно отваливается"). `mcphub
  status` (new connection) times out.

## Invariant to preserve
The MCP-serving path stays green throughout both symptoms (`claude mcp list` all-green). Do NOT touch or
risk it. This is a control-plane fix only.

## Bounded scope
Read-only status-poll control-plane: poll rate/connection lifetime (GUI + restart-watcher), IPC hello/accept
concurrency, read-vs-mutating audit-row emission. Non-goals: redesign the IPC protocol or event loop beyond
the poll-flood; the deep-sec RestartV3 residuals; productization items 4-6.

## Stage log
| Stage | Owner | Status |
|---|---|---|
| Prioritization | $product-manager | PASS — admitted HIGH, ahead of items 4-6; runner-up item-4 (not live-felt) |
| Analysis (map IPC path + confirm single-threaded-starvation via live repro) | $analyst | dispatched |

## Kill/re-intake trigger ($product-manager)
If, after reducing/persisting the poll connection AND servicing hello concurrently, the dashboard still
recurs RED → single-threaded-listener starvation was NOT the true driver → stop, re-intake for a deeper
IPC-architecture pass.

## Delivery cautions (memory)
Any test on `internal/cli/supervise.go` (serves ALL `mcphub status` + GUI traffic) must NOT wipe/kill the
live supervisor-intent or real fleet — back up state / narrowed test paths
([[feedback_kosyak_subagent_test_wiped_live_supervisor_intent]], [[feedback_kosyak_full_test_sweep_affects_real_scheduler]]).

## Bugs
- work-items/bugs/2026-07-19-supervisor-ipc-status-poll-congestion.md (Fix B)
- work-items/bugs/2026-07-16-supervisor-audit-log-flooded-by-status-polls.md (Fix A, P1)

## Analysis — $analyst PASS (runtime-verified, 2026-07-19) — HYPOTHESIS CORRECTED

The unified "single-threaded starvation" root cause is **REFUTED**. Two independent drivers:
- **Fix A (audit-flood): CONFIRMED.** Unconditional per-request `ipc-command` audit emit at
  `internal/cli/supervise.go:1746-1756`; dispatch already discriminates read-only `status` (`:1804`) from
  mutating verbs (`:1839`). Fix = guard the emit to skip `status` (read-only) only; keep mutating rows. + a
  regression test (does not yet exist).
- **Fix B (dashboard RED): single-threaded-starvation REFUTED.** The IPC listener is ALREADY concurrent
  per-connection — `supervise.go:1651` `go serveIPCConn`, hello-write off the accept loop (`:1669`), status
  never touches the FIFO loop (`:1804-1826`). This was fixed by **PR #530 `157c6661` (2026-07-11)** — do NOT
  re-apply. Real driver = **restart/handoff-window transients**, amplified by RestartV3 (#563) new
  200ms readiness burst-probe (`gui_supervisor_owner.go:167-210`, `supervisorReadyPollInterval=200ms`).
  Steady-state runtime-verified fine (0.35 IPC/sec, `mcphub status` 12-60ms, 0 timeouts, hello-errors
  cluster ONLY at deploy/restart transitions).
- Client dial: single 5s deadline for the whole round-trip (`supervisor_ipc_status_client.go:56,96`).
- Coalescer `statusPortOwnersTTL=1s`/probe 3s (`supervise_status.go:41,48`) — secondary transient latency
  under fleet-generation churn.

**Retargeted Fix B** = restart/handoff-window RESILIENCE, NOT concurrency: debounce transient
STATUS_FAILED in the GUI RED indicator (bound to the readiness/handoff window, must NOT mask a genuine
prolonged outage); throttle/short-circuit the 200ms burst-probe; optional persistent GUI IPC connection
(helps steady-churn, not the handoff window). Falsification: 1-3 `install --upgrade`/RestartV3 restarts →
hello-errors + RED confined to the handoff window vs steady-state.

Stage: Analysis PASS → $architect (fix design against corrected picture).

## Design — $architect PASS (2026-07-19)
Design is implement-ready (serves as the plan). Two independent phases, share no file, independently revertible:
- **Phase 1 Fix A (backend):** add `api.IPCCommandIsReadOnly` (allowlist `{"status"}`, single owner) in
  `internal/api/supervisor_ipc.go`; guard the `ipc-command` emit at `internal/cli/supervise.go:1746` with
  `!api.IPCCommandIsReadOnly(req.Cmd)` (skip read-only, keep mutating); regression test in
  `internal/cli/supervise_accept_loop_test.go` via `makeTestDeps` (temp log, NO live fleet) — 3 assertions
  (status→no row; quiesce-timers→row; taxonomy table pin). Decision:
  `work-items/decisions/2026-07-19-ipc-audit-readonly-allowlist.md` (proposed).
- **Phase 2 Fix B (frontend):** DO-NOW option-(a) only — bounded RED-banner debounce in
  `internal/gui/frontend/src/screens/Dashboard.tsx` via a single `degradedSince: number|null` reducer
  (source-agnostic: HTTP-poll catch `:118` + SSE poller-error `:238`); `persistentlyDegraded` = degraded ≥
  RESTART_GRACE_MS (≈20s, covers RestartV3 handoff envelope, below operator-action horizon); calm
  reconnecting cue within grace, RED banner + RecoveryActions past grace (fail-loud PRESERVED); deadline
  setTimeout so the banner appears at the bound. Test in `Dashboard.test.tsx` (fake timers) + keep existing
  startup/persistent-down tests green. REJECTED (b) burst-probe throttle (fresh RestartV3 timing) + (c)
  persistent conn (doesn't help handoff window).

Protected/must-not-touch: IPC listener concurrency (PR #530 serveIPCConn/acceptIPCConnections), RestartV3
readiness timing (gui_supervisor_owner.go 200ms burst-probe + gui.go:1171 waitFor=15s), /api/status
fail-loud server contract (health.go:407-443), mutating-command audit rows, MCP-serving path. No contract
or persisted-state change.

Stage: Design PASS → Implement (Fix A backend ‖ Fix B frontend, parallel disjoint) → architecture-reviewer + QA.

## Implement — Fix A + Fix B PASS ($lead-verified 2026-07-19)
- **Fix A (backend):** `api.IPCCommandIsReadOnly` allowlist + guard at supervise.go:1750 + regression test
  `TestHandleIPCConnAuditSkipsReadOnlyStatusKeepsMutating` (temp-log). Diff = exactly the design (one guard
  condition + predicate + comment). build/vet + new test GREEN.
- **Fix B (frontend):** `degradedSince` reducer + RESTART_GRACE_MS=20s + 3-way render gate + deadline timer,
  fail-loud preserved. Vitest 69 files/1124 tests GREEN, typecheck clean, bundle regenerated (app.js +631B),
  embed smoke GREEN.
- **e2e ($lead-fixed):** dashboard-fail-loud.spec.ts + dashboard.spec.ts timeouts 15s→32s (RED now appears
  after the ~5s poll + 20s grace for a persistent outage) + stale "grace bypassed" comments refreshed.
- **Full internal/cli suite FAIL was a PRE-EXISTING load-flake, NOT Fix A:** the only failing test was
  `TestQuiesceHandler_MixedDrainedAndStillRunning` (supervise_quiesce_test.go:309), which spawns
  `powershell Start-Sleep 200ms` and expects it to drain within a 2s window. Under the session's heavy
  system load (240+ leaked node from the client-MCP sprawl), powershell spawn+exit exceeded the 2s window
  → drained=0. VERIFIED not-Fix-A: clean master (Fix A stashed) ALSO fails it 5/5 under the same load; the
  test never touches Fix A's code (direct `NewQuiesceHandler().Drain()`, no IPC/audit). Pre-existing
  load-sensitive test-design flake (2s window too tight under saturation); CI on a clean machine is unaffected.
  NOTE for a separate follow-up: widen the drain window or gate the test on a lighter spawn than powershell.

Stage: Implement PASS → architecture-reviewer + QA → commission → PR → bot → merge → deploy.

## Review + Commission — PASS ($lead-integrated 2026-07-19)
- architecture-reviewer: Fix A+B core SOUND; 2 e2e defects (timeout not bumped in sibling spec + stale STARTUP_GRACE_POLLS comments) — FIXED.
- Terra: Fix A+B+e2e SOUND; minor test cite supervise.go:1746→:1750 — FIXED.
- fable (hidden-bug): F1 (grace timer single-shot, wall-clock step-back → RED delayed; fixed: graceTick self-re-arm + performance.now monotonic), F2 (e2e route served 200 forever → banner oscillation, 32s not bound-proof; fixed: route serves 500 after first 200, timeout→60s), F3 (never-loaded grace shrank 60-90s→20s; fixed: STARTUP_GRACE_MS=45s for !hasEverLoaded vs RESTART_GRACE_MS=20s loaded, via graceThresholdMs). Also fixed persistentlyDegraded render-gate (was Date.now vs performance.now degradedSince = clock-mix + RESTART_GRACE_MS not graceThresholdMs).
- Verify: typecheck clean, Vitest 69 files/1124 tests pass, bundle regenerated, embed smoke OK.
- F6 commit hygiene: stage ONLY the fix files; exclude pre-existing adopt_test/install_migration + the auto-reaper work-item files.

Stage: Review+Commission PASS → PR → bot → merge → deploy.
