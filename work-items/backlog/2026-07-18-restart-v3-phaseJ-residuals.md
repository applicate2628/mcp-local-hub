# Backlog: RestartV3 Phase-J accepted residuals (fable release-review, non-blocking)

Filed 2026-07-18 by $lead from the fable Phase-J (gate-flip) release review. The blocking P1
(child-missing-coordinator) is FIXED in-PR; these two are bounded, fail-safe, and accepted for the gate-ON
release. Address before the reservation-race matters at scale.

## P2 — normal CLI single-instance acquire is not reservation-aware (release-gap flock theft)
`internal/gui/single_instance.go:277-281`. Phase E intentionally left the CLI + force-kill acquire callers
un-wired for the reservation-aware path ("Phase F will pass one enabled option for a restart child"); Phase J
made the restart flow LIVE. Scenario: mid-handoff (parent released the flock at gui_restart_protocol.go:410,
the designated child is in its 100ms retry loop), a concurrent plain `mcphub gui` / autostart-task fire does a
legacy single-shot TryLock (cli/gui.go:585, no options) and WINS the flock without the designated-child nonce;
the child busy-retries until `Proof` (10s) expiry and exits; the reserved marker expires nonterminal;
ensure-alive then classifies Held and advises `mcphub gui --force --kill` against a healthy (thief) GUI.
Bounded: sub-second window, converges to a live GUI, nothing auto-killed — degraded ADVICE, not damage.
Fix: wire a nonce-less `SingleInstanceAcquireOptions` reservation check into the normal CLI acquire (return an
`ErrHandoffReserved`-aware busy during the reservation window), and update the stale "Phase E" comment.

## P3 — same-port confirm/bind budget tight for a cold/AV-scanned child boot
`internal/gui/gui_restart_record.go:52-57` (Bind=2s). If a cold or AV-scanned child cannot bind + answer the
readiness challenge within the parent's 2s confirm window, the same-port handoff rolls back (verified clean:
terminate child first → BindForRecovery re-binds → restore full handler → clear marker; a restore failure fails
loud via Interrupt+exit with the ensure-alive `mcphub gui` guidance). The 202-then-interrupted outcome is
honest async protocol; this is operational tuning only. Fix option: give the child same-port bind budget
`Bind >= Quiesce` (also raised as a Phase-G follow-up in
`backlog/2026-07-18-phaseG-coordinator-followups.md`) — consolidate.

## P3 (fable P1-fix re-check) — owner-mismatch guard + child-path integration coverage
- `internal/gui/server.go:1138` `ContinueWithGUIListener` reassigns `s.guiListener = owner` AFTER
  `composeGuiServerRestartV3` captured the compose-time `s.guiListener` for the coordinator's `deps.Listener`.
  Today provably the same object (sole caller passes `server.GUIListenerOwner()`, test-asserted), but no
  production invariant enforces `startup.listenerOwner == startup.server.GUIListenerOwner()` — a future
  foreign-owner caller would leave the coordinator draining a stale listener SILENTLY. Fix (advisory
  hardening): error out when a coordinator is configured and `owner != s.guiListener`.
- `TestRestartV3_ActivatedChildAcceptsSecondRestart` drives `composeGuiServerRestartV3` from its own
  StartRuntime, not through production `startGuiServerWithStartup` — a coverage boundary (a regression that
  re-branched the gui.go:906 compose call would be caught only by code review). An integration test through
  `startRestartV3GUIChild` would close it.

## Deep-security commission (2026-07-19) — $architect adjudication: GATE-ON RELEASE ACCEPTABLE, all 10 findings BACKLOG, zero blockers

Three independent reviewers (Sol=auth/trust, Terra=concurrency, fable=error/regression) + `$architect`
release-posture adjudication. Verified fail-safe substrate: exclusive strictly-hand-off-ordered GUI flock
(no split-brain in ANY traced path) + `os.Exit` fleet-preservation (a failed GUI restart never reaps the
separate-process supervisor/daemons) + expiry-gated activation. Every worst case lands at "no GUI serves
until manual relaunch; daemons/supervisor unaffected." F3 (duplicate browser on every self-restart) was the
only inline fix this round (`shouldAutoLaunchBrowser` gates on `startup==nil`, +unit test). F4/T2b resolved
as CORRECT-AS-IS (retain-guard is the right fail-safe; `Interrupt` shares the failing lock/write path, so a
reset would retry against uncertain durable state — worse than "feature inert until relaunch").

Top-2 hardening for the next iteration:

- **S1 (nonce transfer) — security hardening.** The authenticated-readiness nonce is a named owner-only file
  in StateDir (`gui_restart_protocol.go:218-219,813`; `ping.go:178-196`). A same-UID process watching StateDir
  can copy it in the write→consume window and forge readiness to win the reservation-aware flock →
  same-principal GUI-listener impersonation/DoS. SAME CLASS as the already-accepted same-UID co-resident
  residual (a same-UID actor can already inject a daemon descriptor = arbitrary code), strictly weaker, and
  operator-triggered not attacker-pumpable — so not a gate. Fix: transfer the nonce via a child-exclusive
  inherited pipe/OS handle (not a named file), or verify peer process identity against the retained child handle.
- **T1a / F2 (deadline timing) — tuning.** `DefaultRestartDeadlines` sets `Proof == Reservation == 10s`
  (`gui_restart_record.go:53-54`) and the child Proof budget starts at `Run` entry
  (`gui_restart_protocol.go:1014-1015`), so a child can expire while the parent has closed its listener but is
  still flushing/closing the hub → both-miss-port → no GUI (fail-safe). Fix: derive the child budget from the
  parent's physical-close commitment and enforce strict `Bind < Quiesce < Proof < Reservation` (also raised as
  Phase-G P3-2 and the same-port P3 above — consolidate all three).

Remaining findings (all BACKLOG, bounded + fail-safe):

- **T1b — post-acquire parent-death race.** After a successful `Acquire`, a parent death in the µs-window
  before `stopWatchingParent` makes the child release its just-acquired lease and exit
  (`gui_restart_protocol.go:1042-1055`). Worst case = flock-free, no GUI, SINGLE owner throughout (child
  releases before exit → no split-brain). Recoverable via ensure-alive Free-interrupt + `mcphub gui`.
- **T2a — Begin marker replace without expected-state CAS** (`gui_restart_record.go:196-234`). Unreachable in
  production (in-memory `run` guard serializes one coordinator/process + single-instance model = one live GUI);
  the loser would fail `ErrHandoffMarkerCASMismatch` and roll back cleanly. Optional CAS for defense-in-depth.
- **T2b — retain `run=true` on marker-clear failure** (`gui_restart_protocol.go:282-317`). CORRECT fail-safe
  (see above). Optional refinement: reset the guard ONLY when the subsequent `Interrupt` returned
  `terminalErr == nil` (marker proved terminal), preserving the retain path when the terminal write is unproved.
- **T2c — Commit checks no expiry/sequence** (`gui_restart_record.go:259-268`). Activation is ALREADY
  expiry-gated at acquire (`matchesReservedMarker` requires `now.Before(ReservationExpiresAt)`,
  `gui_restart_protocol.go:1086,1207-1218`); only post-activation bookkeeping can cross expiry while the child
  is the sole flock holder → no split-brain/stuck-port. Adding a Commit-side expiry+sequence check is cleanliness.
- **T3a — unbounded wait for `responseFlushed` after Reserve** (`gui_restart_protocol.go:403-409`). Only blocks
  if the API handler panics between WriteHeader and the ack (`gui_self_restart.go:159-164` closes it immediately
  otherwise). Worst case (same-port) = listener closed + parent wedged holding flock + child expired →
  recovery is `mcphub gui --force --kill` (ensure-alive prints the exact command). The one worst-case that
  needs `--force --kill` rather than a plain relaunch. Fix: bound the flush ack by the reservation budget and
  roll back before release.
- **T3b — CloseHub has no acknowledgement before Release** (`gui_restart_protocol.go:413-416`). Impact bounded
  to the hub-aggregate listener, which owns its own restart/health driver (CLAUDE.md B1); GUI flock authority
  + daemons unaffected. Fix: require confirmed hub shutdown before lease release.
- **T5 — failAfterBegin detaches (not terminates) a partial child** (`gui_restart_protocol.go:284-286` vs the
  `rollbackBeforeRelease` `TerminateBeforeRelease` at `:447`). Unreachable in production (`SpawnRestartV3GUI`
  returns `nil,err` on every error path, `gui_self_restart.go:257,261,279`); a hypothetical orphan self-limits
  within Proof (can't steal the parent-held flock). Tighten to Terminate for symmetry.
- **F1 — false `--force --kill` advisory on a healthy holder.** A stale nonterminal marker + a healthy but
  unrelated flock holder makes ensure-alive emit the per-tick "wedged, run `mcphub gui --force --kill`" warn
  against a HEALTHY GUI (`supervise_ensure_alive.go:511-520,615-624`) — precisely the crash-then-manual-restart
  sequence. Nuisance only (nothing auto-kills; obeying loses no data; log churn bounded by rotation). Fix:
  compare the holder's generation/PID to the marker and interrupt an orphan instead of advising force-kill.
