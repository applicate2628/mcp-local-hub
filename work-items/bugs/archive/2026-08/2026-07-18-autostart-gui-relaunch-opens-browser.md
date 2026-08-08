# Bug: the Windows autostart GUI relaunch opens a browser on every fleet churn

- id: 2026-07-18-autostart-gui-relaunch-opens-browser
- status: fixed
- severity: medium (operator-facing annoyance; browser-tab flood)
- area: internal/autostart/windows.go (superviseArgs)
- found-by: operator ("браузер открылся 6 раз" during a dev session)
- platform: Windows only

## Symptom

The operator saw a browser window/tab open ~6-10 times during a development
session. Not from the Go/frontend test suites directly (all real GUI spawns in
tests pass `--no-browser`; the main spawn test `TestGuiCmd_SecondInstanceActivates`
is Windows-skipped; vitest uses happy-dom).

## Root cause

The Windows autostart Task Scheduler entry `\mcp-local-hub-supervisor` (which
under the "GUI owns supervisor lifecycle" model since PR #212 launches
`mcphub gui`, and which `mcphub supervise --ensure-alive` re-fires whenever the
GUI/supervisor owner dies) was installed with args **`gui`** — WITHOUT
`--no-browser`. `internal/autostart/windows.go superviseArgs` returned
`[]string{"gui"}` / `[]string{"gui", "--strict-mode"}`.

`mcphub gui` without `--no-browser` auto-launches a browser on startup
(`startGuiServer` → `if !noBrowser { LaunchBrowser }`, cli/gui.go:1164). So
EVERY relaunch of the owner — a user logon OR a liveness recovery after the fleet
is killed — popped a browser window.

The trigger for the repeated opens this session: repeated **sweeps of
`mcphub.exe`** (the "Sweep mcphub.exe after tests" step in the PR workflow +
codex dispatch specs, plus manual `Stop-Process mcphub`) killed the live GUI; the
`\mcp-local-hub-supervisor` autostart task then relaunched `mcphub gui` (no
`--no-browser`) each time → a browser per relaunch. ~6-10 sweeps → ~6-10 browsers.
(darwin/linux autostart launch `mcphub supervise`, not `gui`, so they never had
this — Windows-only.)

## Fix

1. **Source** (`internal/autostart/windows.go superviseArgs`): now returns
   `[]string{"gui", "--no-browser"}` / `[]string{"gui", "--no-browser",
   "--strict-mode"}`. The autostart/logon/recovery GUI is a headless server +
   tray indicator (memory `feedback_gui_always_tray`: tray on, browser off); the
   operator opens the dashboard from the tray. windows_test.go assertions updated;
   the drift check (`superviseArgs(opts)` at windows.go:340) now expects
   `--no-browser`, so a stale task installed without it is flagged drifted and
   re-installed by `mcphub setup` / `mcphub autostart enable`.
2. **Installed task** (immediate): the live `\mcp-local-hub-supervisor` task
   action was changed in place to `gui --no-browser` (Set-ScheduledTask), so
   future relaunches on this host are already browser-free.
3. **Behavioral:** stop blind-sweeping `mcphub.exe` (it kills the live fleet →
   triggers the autostart relaunch); the "sweep mcphub.exe after tests" step must
   be targeted at test-port processes, never the live fleet. Related memories:
   `feedback_kosyak_mcphub_sweep_kills_running_daemons`,
   `feedback_kosyak_full_test_sweep_affects_real_scheduler`.
4. **Backstop** (`internal/gui/browser.go`): a `MCPHUB_SUPPRESS_BROWSER_LAUNCH`
   env kill-switch in `LaunchBrowser`, set by the gui + cli TestMains, so no Go
   test (in-process or a spawned child that inherits the env) can flash a browser
   even if a future test path reaches `LaunchBrowser` without `--no-browser`.

## Residual

The CURRENTLY-running GUI on this host was relaunched as `mcphub gui` (no
`--no-browser`) before the task was fixed, so it already opened its browser; it
serves fine. Its NEXT relaunch uses the corrected task action.

Terminal-at: 2026-08-08T22:58:13Z
Resolution: Pre-V1 terminal status `fixed` is preserved during operator-authorized V1 physical migration.
Evidence: Historical terminal time is unknown; preserved pre-V1 input SHA-256 `b94472f5cac4c1969ccf91b3519f631d1214424bc0e6499f410335d679408e51`; original terminal status `fixed`; explicit operator-authorized V1 migration.
V1-Migration-Evidence: Historical terminal time is unknown; preserved pre-V1 input SHA-256 `b94472f5cac4c1969ccf91b3519f631d1214424bc0e6499f410335d679408e51`; original terminal status `fixed`; explicit operator-authorized V1 migration.
