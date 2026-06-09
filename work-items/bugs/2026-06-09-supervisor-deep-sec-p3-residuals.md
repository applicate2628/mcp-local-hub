# Supervisor deep-sec P3/LOW residuals deferred from PR #268

- **Status:** open / deferred (tracked for a future hardening pass)
- **Date:** 2026-06-09
- **Severity:** P3 / LOW (none blocking)
- **Source:** the 3-angle deep-security gate on PR #268 (full diff master..52d390b, post-bot-PASS). The operator chose "fix all P2+"; the P2+ set (Sec-F1 overlay read-gate, Conc-F1 PersistTo field-drop, Conc-F2 sweep/handler persist non-atomicity, Conc-F3 ownSpawned re-registration double-spawn, Conc-F6 overlay torn-read, Reg-F1 stale_pid IPC field) is fixed in #268. The findings below are the explicitly-deferred P3/LOW residuals.

## Conc-F4 (P3) — backoff stale-timer relies on the SM table, not its own re-check
`internal/cli/supervisor_controller.go` (~999-1056). The backoff timer goroutine re-checks `c.smStates.Load(task) == StBackoffWaiting` at fire time and drops otherwise, but the check-then-`loop.Post(EvTimerDue)` is not atomic. The SM table currently absorbs every stale fire (StSpawning/StRunning/StExiting all drop or ignore `EvTimerDue`), so it is safe TODAY — but the safety lives in the SM table, not the goroutine's re-check. **Risk if a future SM edit adds an `EvTimerDue` row to StSpawning/StRunning**, the stale fire becomes a live double-spawn. Action: document the dependency at the re-check site, or make the timer post through the loop so the loop owns the gate. Not fixed now (no live bug).

## Conc-F5 (P3) — RegisterHandler mutates the handlers slice after loop.Run started (latent race)
`internal/api/supervisor_event_loop.go:62-64` + `internal/cli/supervise.go:500,759`. `go loop.Run(loopCtx)` starts at supervise.go:500; `loop.RegisterHandler(ctrl.handleLoopEvent)` runs at 759 with an unsynchronized `l.handlers = append(...)` while Run's goroutine reads the slice. Verified NOT a live race today (no producer Posts to the loop in the 500→759 window; every post-759 producer gets the channel happens-before edge). But it violates the documented "RegisterHandler is start-up-only" invariant and is one early-producer away from a genuine `-race` hit. Action: register both handlers before `go loop.Run`, or guard `handlers` with a mutex/atomic snapshot. Not fixed now (latent).

## Conc-F7 (P3) — IPC handleRespawn captures a possibly-stale intent descriptor
`internal/cli/supervise_respawn.go:136-148`. `handleRespawn` reads `ctrl.intentCache.snap` directly and takes `desc = &snap.intent.Daemons[i]`. `IntentCache.Refresh` allocates a fresh snapshot (never mutates in place), so the captured pointer stays valid — **no use-after-free**. The residual is staleness: a mid-run intent refresh (e.g. a migrate changing a port) leaves the respawn spawning the old descriptor. Action: re-resolve under the same freshness discipline the liveness sweep uses (`ReadSupervisorIntent` fresh), or go through `Lookup`/`TaskNames` (which also applies canonicalization) instead of reaching into `snap` directly. Not fixed now (staleness, not unsafety).

## Reg-F2 (LOW) — liveness redundant-restart if a restart cycle exceeds the 5s ticker
`internal/cli/supervise_liveness.go:185-191` + `supervisor_controller.go:785-816`. During an in-flight terminate-restart of a port-stale daemon the tracker stays `running` with the OLD PID for the whole `StExiting` window (port-stale reasons skip MarkExited). If terminate-grace + spawn exceeds the 5s ticker, the next sweep re-posts `EvManualRestart` on the now-freshly-respawned daemon → one extra clean restart. Self-correcting (the r11 `ownSpawned` discriminator prevents a spurious failure-count, so quarantine is not falsely advanced); practically unreachable given the immediate-sweep + first-tick ordering and the `supervisorPortBindGrace` window. Action (optional hardening): skip the liveness sweep for tasks whose controller SM state is already StExiting/StSpawning. Not fixed now (self-correcting, LOW).

## Note
Conc-F2's full single-writer fix (routing the liveness MarkExited+persist through the event loop) closes most of the window Reg-F2 lives in; after the #268 deep-sec fixes land, re-evaluate whether Reg-F2 is still reachable before spending effort on it.
