# CLI first-run UX: `mcphub` should just work

Template: quick-fix (requiresLead: false) · Lead: main conversation · Opened: 2026-07-20

## Goal (operator, verbatim)
"нужно чтобы user установил, запустил mcphub и все работает!" — install → run `mcphub` → the hub +
GUI are up. Today a bare `mcphub` in a terminal dumps 40 commands instead (and several descriptions are
stale v0.4.x language).

## Evidence (verified on the CURRENT binary a0b2dded, build 2026-07-20T08:41)
1. `cmd/mcphub/main.go` `shouldAutoLaunchGUI()` routes to `gui` ONLY when there are no args AND **no console
   attached** (Explorer double-click). From a terminal (console attached) cobra prints the 40-command help.
2. Stale descriptions (v0.4.x survived the v0.5 supervisor + v0.6 watchdog removal):
   - `setup`: "…install **watchdog** task" — the watchdog was DELETED in v0.6 (`mcphub setup` no longer
     installs `\mcp-local-hub-watchdog`).
   - `status`: "state of all mcp-local-hub **scheduler tasks**" — replaced by the supervisor in v0.5.
   - `restart`: "re-run **scheduler tasks**"; `stop`: "(**tasks** and configs remain)"; `uninstall`:
     "(**scheduler** + client bindings)" — same stale model.
3. 52 registered commands, flat: internals (`daemon`, `relay`, `intent-collapse`, `hub-mcp`,
   `adopt-provenance`, `repair-state-dacl`) sit next to user-facing ones. `Hidden:` is used in only 4 files;
   there are NO cobra groups (`AddGroup`/`GroupID` absent).

## Operator decisions (asked + answered 2026-07-20)
- **Bare-run** → **launch hub+GUI** (same as `mcphub gui`), regardless of console. `--help`/`help` keep
  printing the command list.
- **Command list** → **hide internals + group the rest** (Setup / Servers / Runtime / Secrets / Diagnostics).

## Scope
`cmd/mcphub/main.go` (shouldAutoLaunchGUI), `internal/cli/root.go` (groups), the internal commands' `Hidden`,
and the stale `Short` descriptions. Non-goals: changing what `gui` itself does; renaming commands; touching
the supervisor/daemon runtime.

## Stage log
| Stage | Owner | Status |
|---|---|---|
| Evidence + operator decisions | main conv ($lead) | PASS |
| Implement | $backend-engineer | dispatched |

## Requirement #4 (operator, added 2026-07-20): GUI must survive the launching terminal
Operator, verbatim: "при закрытии терминала, из которого я запускал `mcphub gui`, он тоже закрывается
(пропадает из трея)" + "какого хрена оно вообще привязывается к терминалу, сколько это может повторяться!"

### ROOT CAUSE (verified, `file:line`)
`cmd/mcphub/main.go:26` calls `attachParentConsoleIfAvailable()` (`cmd/mcphub/console_windows.go:34`) as the
FIRST statement of `main()`. mcphub.exe is a **Windows-subsystem** binary (no console by default, so a
double-click does not flash a black window); that helper **attaches it to the parent console** so CLI output
is visible from a terminal. Attaching makes the process a **client of that console** → on console close
Windows delivers CTRL_CLOSE_EVENT to every attached process → the GUI/tray dies.

This is NOT a regression — it is a standing design gap: the GUI path never RELEASES the console. That is why
the operator experiences it as recurring. The repo's `DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP` usages
(`internal/cli/gui_supervisor_owner_windows.go`, `internal/gui/supervisor_restart_*.go`) cover spawning the
SUPERVISOR / RestartV3 child — never the GUI-launched-from-a-terminal case.

### Fix options handed to the implementer
- **(a) `FreeConsole()` on the GUI path — LOW RISK, preferred first.** Detaches from the parent console after
  the GUI path is chosen → no CTRL_CLOSE_EVENT → survives terminal close. Adds NO new process and NO new
  flock/spawn path, so it cannot re-open the single-instance / RestartV3 reservation race (PR #568 area).
  Caveat to verify: the launching shell may still WAIT (prompt does not return until exit).
- **(b) Re-spawn detached + exit** — also returns the prompt, but adds a spawn path that must not
  double-acquire/orphan the single-instance flock, must not look like a RestartV3 structured child, and must
  not disturb the reservation window. Higher risk; if needed, it must be designed, not hacked.

Implementer instructed: if (a) is insufficient for the operator goal and (b) cannot be done safely as a
bounded edit, STOP and report that it needs a design pass rather than hacking it. Empirical proof required:
launch from a real console, CLOSE it, show the GUI still alive; state whether the prompt returns.
