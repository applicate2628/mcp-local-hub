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
