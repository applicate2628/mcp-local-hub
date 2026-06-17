# Supervisor deep-sec P3/LOW residuals deferred from PR #268

- **Status:** RESOLVED 2026-06-16 (3-angle review-loop synthesis: Sonnet code-review + consultant; codex+fable unavailable that session). Conc-F5 fixed; Conc-F4 + Conc-F7 fixed via the converged minimal approach; Reg-F2 deliberately DEFERRED (proposed fix rejected — see below).
- **Date:** 2026-06-09 (residuals) / 2026-06-16 (resolution)
- **Severity:** P3 / LOW (none blocking)

## RESOLUTION 2026-06-16 (review-loop converged)

The original "Action" lines below were the FIRST-PASS designs. A 3-angle review
loop (fix-design `.scratch/reviews/fix-design-2026-06-16-deepsec-f4f7r2.md`)
materially revised two of them and rejected one:

- **Conc-F5 → FIXED (836b1b2).** Atomic copy-on-write on `EventLoop.handlers`
  (RegisterHandler CAS-swaps a fresh slice; dispatch does one atomic load).
  Register-after-`go loop.Run` is now race-free without restructuring
  supervise.go's ctrl-construction order. `-race` regression test added.
- **Conc-F4 → FIXED as an INVARIANT TEST + table comment (a222538), NOT a timer
  restructure.** The real risk is "a future SM edit"; a code change guards only
  the current path, whereas `TestStateMachineInvariant_TimerDueSpawnsOnlyFromBackoffWaiting`
  pins the PROPERTY (EvTimerDue issues a spawn only from StBackoffWaiting) and
  goes RED the moment the dangerous edit lands — pure function, internal/api,
  zero fleet risk. The timer goroutine is left untouched (its smStates re-check
  is a harmless perf early-out + observability emit, not the safety property).
- **Conc-F7 → FIXED as a DEDUP (6410064); staleness left DOCUMENTED, not closed.**
  The inline NormalizeOverlayKey scan was replaced by the single-owner
  `IntentCache.LookupCanonical` (semantically identical — NormalizeOverlayKey is
  purely the backslash toggle, the same equivalence class LookupCanonical
  covers). The review loop established that the cheap swap does NOT close the
  cache-vs-disk staleness (LookupCanonical reads the same snapshot), and the
  fresh-disk-read variant (reapIntentReader) adds per-respawn disk I/O to close
  a sub-second operator-initiated window — not justified. Documented in code.
- **Reg-F2 → DEFERRED (NO code change; the proposed fix is REJECTED).** Both
  reviewers rejected the "skip the liveness sweep for StExiting/StSpawning"
  action: (a) Sonnet — an unconditional `StExiting → continue` removes the ONLY
  retry for a foreign-PID daemon whose `terminate` keeps FAILING
  (executeSideEffect skips synthesizeForeignChildExit on terminate!=nil), so it
  trades a self-correcting one-extra-restart (LOW) for a possible PERMANENT
  wedge — a net-negative fleet-safety regression. (b) Consultant — the fix has
  the sweep goroutine read SM state to self-suppress, which VIOLATES the PR #268
  architectural principle "off-loop posters detect and post; the event loop is
  the only gate, they must not pre-judge SM state." Today's "post unconditionally,
  let the loop coalesce" IS the principled behavior. Combined with self-
  correcting + Conc-F2 having already shrunk the window, Reg-F2 stays
  documented-and-deferred; re-evaluate ONLY on a live repro.

---

### Original first-pass residual notes (superseded by the resolution above)
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

## Resolution (closed 2026-06-17 — repo audit)

Fixed-in: Conc-F5 (836b1b2 atomic COW) + Conc-F4 (a222538 invariant test) + Conc-F7 (6410064 dedup); Reg-F2 deferred by both reviewers
