# Bug: the restarted RestartV3 child GUI has no restart coordinator (self-disarms after first use)

- id: 2026-07-18-restart-v3-child-missing-coordinator
- status: fixed
- severity: high (flagship self-restart flow breaks its own repeatability, ships default-ON)
- area: internal/cli/gui.go (startGuiServerWithStartup / ConfigureRestartCoordinator)
- found-by: security-reviewer (fable, Phase J live-gate-ON-risk review)

## Root cause

`ConfigureRestartCoordinator` has ONE call site — inside the `if startup == nil`
branch of `startGuiServerWithStartup` (cli/gui.go:873-895). The v3 CHILD path
(`startRestartV3GUIChild` → `runRestartV3ChildStartup` → `StartRuntime` with
`startup != nil`) takes the `else` branch (`s = startup.server`), a server built at
cli/gui.go:226-231 with `RestartV3Enabled: true` but `restartCoordinator == nil`.

## Failing scenario (gate-ON production, after the Phase J flip)

Operator clicks Restart GUI → v3 handoff succeeds → the surviving GUI is the CHILD →
operator changes the port / clicks restart AGAIN → `guiRestartV3Handler` hits the
nil-coordinator branch (gui_self_restart.go:115-119) → HTTP 200 with
`spawn_error: "restart v3 parent coordinator is not configured"` on EVERY subsequent
attempt until a manual close-and-relaunch. Fail-safe (no exit, no stranding, fleet
untouched, a plain `mcphub gui` relaunch re-arms it because that path is
`startup == nil`) but the flagship flow disarms after its first successful use.

## Fix

Hoist the gate-ON coordinator composition OUT of the `startup == nil` branch so BOTH
the initial GUI and the activated child server get a coordinator (all inputs —
`ownedLease`, `pidportPath`, `s.Port`, `os.Args[1:]` — are available in both). Add a
child-path test asserting the activated child server has a non-nil coordinator and
can `Start()` a SECOND handoff.

## Resolution (2026-07-18)
FIXED — coordinator composition hoisted into `composeGuiServerRestartV3` (cli/gui.go:785), invoked by BOTH the
initial (startup==nil) and child (startup!=nil, reuses `startup.server` + `server.GUIListenerOwner()`) paths.
Proof: `TestRestartV3_ActivatedChildAcceptsSecondRestart` (cli/gui_self_restart_handoff_test.go) — child
activation then a SECOND restart returns 202, not the nil-coordinator 200 spawn_error. fable re-check PASS
(build/vet + `-race -count=3` clean; the listener-reuse is what makes the hoist correct — the coordinator
captures the compose-time `s.guiListener`). Two non-blocking P3 advisories deferred to
`backlog/2026-07-18-restart-v3-phasej-residuals.md`.

Terminal-at: 2026-08-08T22:58:13Z
Resolution: Pre-V1 terminal status `fixed` is preserved during operator-authorized V1 physical migration.
Evidence: Historical terminal time is unknown; preserved pre-V1 input SHA-256 `dbf02d9ecba72515abe1e50cc3d6e4cab21beeed382cc4b6dd2573ec56909f53`; original terminal status `fixed`; explicit operator-authorized V1 migration.
V1-Migration-Evidence: Historical terminal time is unknown; preserved pre-V1 input SHA-256 `dbf02d9ecba72515abe1e50cc3d6e4cab21beeed382cc4b6dd2573ec56909f53`; original terminal status `fixed`; explicit operator-authorized V1 migration.
