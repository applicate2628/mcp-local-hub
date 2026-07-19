# Item-3 Unit B Phase J — gate flip ON + v1 removal — review record

The release-cutting commit: `restartV3DefaultEnabled` OFF→ON, honest 503 at gate-OFF, v1 `spawnSelfRestartGUI`
body deleted (v3 seams + os.Exit-skips-manager.Stop preserved), AC-J1 inert-matrix, Phase-I ensure-alive
regression fix (absent state-dir returns before the record-lock MkdirAll), docs pass (verification runbook +
CLAUDE.md B1 correction + C6 supersession note).

## Commission
- **$lead + Terra:** gate-flip mechanics clean (Terra PASS: gate-flip completeness, v1-deletion safety,
  ensure-alive regression fix all clean). Full CLAUDE Step-1 gate green (build/vet + go test ./... + tagged
  api/cli/gui).
- **fable (live-gate-ON-risk review) — REVISE, 1 decisive P1:** the restarted CHILD GUI had NO restart
  coordinator (`ConfigureRestartCoordinator` was called only in the `startup == nil` branch), so the flagship
  self-restart flow DISARMED after its first gate-ON use (nil-coordinator → 200 spawn_error forever until a
  manual relaunch). Ships default-ON, zero coverage. Terra + $lead both missed it (checked the flip mechanics,
  not the child-path wiring); fable's specifically-requested live-gate-ON focus caught it. Memory
  `feedback_gate_flip_verify_activated_path`. FIXED: hoisted the composition into `composeGuiServerRestartV3`
  invoked by both paths (child reuses `startup.server` + `server.GUIListenerOwner()`) + proof test
  `TestRestartV3_ActivatedChildAcceptsSecondRestart` (second restart → 202). fable re-check PASS (build/vet +
  `-race -count=3` clean; the listener-reuse is what makes the hoist correct).
- Four fable non-blocking findings handled: P3 stale-v1-comments + P3 rollback-env-skew doc (setx) fixed
  in-PR; P2 reservation-aware CLI acquire + P3 same-port bind budget + P3 owner-mismatch guard + P3 test
  coverage boundary deferred to `backlog/2026-07-18-restart-v3-phaseJ-residuals.md` (all bounded/fail-safe).

## Verdict
Phase J is commit-safe: gate-flip verified sound (rollback full + fail-closed, v1 deletion complete, inert
matrix true, ensure-alive fix correct); the one live-gate-ON blocker (child coordinator) fixed + re-checked
PASS. Unit B (D-J) ships as one PR from here.
