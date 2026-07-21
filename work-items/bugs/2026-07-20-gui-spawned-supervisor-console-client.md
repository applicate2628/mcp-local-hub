# Bug: GUI-spawned supervisor attaches to the launching terminal's console — terminal close still kills the whole daemon fleet

- id: 2026-07-20-gui-spawned-supervisor-console-client
- context: 2026-07-20-cli-first-run-ux
- status: open
- severity: high
- area: internal/cli/gui.go + internal/cli/gui_supervisor_owner.go + cmd/mcphub/console_windows.go
- found-by: qa-engineer

Commit `27f42953` ("GUI survives its launching terminal") releases the GUI's
console AFTER the supervisor spawn (`internal/cli/gui.go:1200` spawn →
`internal/cli/gui.go:1262-1264` release). The supervisor child is spawned with
`DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP`
(`internal/cli/gui_supervisor_owner_windows.go:26-32`), but it is the SAME
binary and its own `main()` calls
`AttachConsole(ATTACH_PARENT_PROCESS)` (`cmd/mcphub/console_windows.go:34-38`).
At spawn time the GUI still holds the terminal console, so the supervisor
re-attaches to it and becomes a console client — a CTRL_CLOSE_EVENT target.

Reproduction (empirical, this session): probe pair replicating the exact
chain (parent holds console, spawns child with the identical creation flags,
child calls `AttachConsole(-1)` then `GetConsoleProcessList`):

- Scenario A (parent HAS console = supervisor spawn point):
  `before=false attachRet=1 after=true` — the detached child ATTACHES.
- Scenario B (parent freed console first = tray spawn point):
  `attachRet=0 (handle invalid) after=false` — attach fails, child free.

Raw outputs preserved at
`<scratchpad>/consoleprobe/{report,outA,outB}.txt` (session scratchpad).

Expected vs actual: the commit's headline is that closing the launching
terminal no longer kills the app. Actual: the GUI and tray survive (verified
by the commit), but a supervisor that the GUI SPAWNED during that terminal
session (the first-run scenario the commit targets: fresh install, no
autostart supervisor yet, operator types `mcphub`) dies with the terminal —
CTRL_CLOSE_EVENT → supervisor terminated → Job-Object handles close →
KILL_ON_JOB_CLOSE reaps every daemon. The GUI respawn loop then restarts the
supervisor and the fleet cold-restarts (dropped MCP sessions, LSP reindex).
The commit's own empirical verification measured GUI + TRAY attachment only,
never the supervisor child.

Not affected: an ADOPTED supervisor (autostart/Task Scheduler parent — no
console) — `ensureSupervisorRunning` probe-adopt path.

Compounding: the GUI-side death attribution ("supervisor exited unexpectedly
(PID …): …; stderr tail: …", `internal/cli/gui_supervisor_owner.go:574-584`)
is stderr-only, and after the release that stderr is a dead console handle —
see bug 2026-07-20-post-console-release-diagnostics-discarded. The fleet blip
arrives unattributed.

Fix direction (for the implementer, not prescriptive): either release before
the supervisor spawn (conflicts with the "after startup output" ordering —
would need the startup lines re-ordered or dual-sunk), or prevent the
supervisor child from attaching (e.g. an env/flag telling the child's
`attachParentConsoleIfAvailable` to skip, or spawn via an intermediary), or
have `supervise` free its console itself when running as a background owner.

## Resolution (2026-07-20, pending lead gate)

FIXED at the supervisor, not by reordering the release. `main()` now skips
`AttachConsole` entirely when the process carries
`process.SuppressConsoleAttachEnv` (`MCPHUB_NO_CONSOLE_ATTACH=1`), and
`configureSupervisorDetach` sets that marker alongside the creation flags,
so the GUI-spawned supervisor — and every respawn, which rebuilds its cmd
through the same hook — is never a console client at all.

Env var rather than a flag because `main()` attaches before cobra parses
anything; the environment is the only carrier readable at that moment, and
it crosses the CreateProcess boundary without the parent knowing the
child's argv shape. An `mcphub supervise` typed interactively carries no
marker and keeps its console unchanged.

Verified with the reviewer's technique (external measurement of the real
`-H windowsgui` child by the console-owning parent, `GetConsoleProcessList`):

```
BASELINE detached, no env : ATTACHED    (defect reproduced independently)
BASELINE detached, env set : ATTACHED   (env inert without code support)
FIXED    detached, env set : NO CONSOLE (the supervisor's new state)
FIXED    detached, no env  : ATTACHED   (interactive `supervise` preserved)
```

The compounding diagnostics loss noted above is fixed under
`2026-07-20-post-console-release-diagnostics-discarded`.

REMAINING, filed separately as
`2026-07-20-detached-supervisor-spawns-still-console-clients`: three OTHER
detached spawn sites of the same binary (RestartV3, and two `install
--upgrade` paths) still lack the marker.
