# Bug: three more detached supervisor spawn sites still let the child re-attach to the launching terminal's console

- id: 2026-07-20-detached-supervisor-spawns-still-console-clients
- context: 2026-07-20-cli-first-run-ux
- status: fixed (pending lead gate)
- severity: medium
- area: internal/gui/supervisor_restart_windows.go + internal/cli/install_upgrade.go + internal/cli/install_migration_wiring_windows.go
- found-by: backend-engineer (while fixing 2026-07-20-gui-spawned-supervisor-console-client)

The FIX-1 revision closed this defect at ONE spawn site — the GUI's own
supervisor spawn/respawn, via `configureSupervisorDetach`
(`internal/cli/gui_supervisor_owner_windows.go`), which now applies
`process.SuppressConsoleAttach` alongside the creation flags. Three other
sites spawn the SAME binary with `DETACHED_PROCESS |
CREATE_NEW_PROCESS_GROUP` and no suppression, so the child still calls
`AttachConsole(ATTACH_PARENT_PROCESS)` in its own `main()` and becomes a
client of whatever console the spawning process happened to hold:

- `internal/gui/supervisor_restart_windows.go:34-38` (`configureDetached`)
  — the RestartV3 / IPC supervisor-restart path.
- `internal/cli/install_migration_wiring_windows.go:448-453` — the
  `install --upgrade` explicit supervisor start.
- `internal/cli/install_upgrade.go:424-431` — same flow, documented there.

The premise is stated explicitly and is FALSE. `install_upgrade.go:428-431`
reads: "detached CreateProcess with DETACHED_PROCESS|CREATE_NEW_PROCESS_GROUP
(the new supervisor's stdin/stdout/stderr inherit nothing from this CLI
process, so it survives the upgrade caller's own exit)". Surviving the
caller's EXIT and surviving the caller's CONSOLE are different properties.
`configureDetached`'s own comment makes the same claim ("DETACHED_PROCESS —
child has no console").

Measured (this session, real `-H windowsgui` build, attachment measured
externally by the console-owning parent via `GetConsoleProcessList`):

```
detached child, no suppression marker : ATTACHED (became a console client)
detached child, suppression marker    : NO CONSOLE (never a console client)
```

Impact, in descending order:

- `install --upgrade` typed at a terminal: the freshly started supervisor
  attaches to that terminal. Operator closes it -> CTRL_CLOSE_EVENT ->
  supervisor dies -> KILL_ON_JOB_CLOSE reaps the whole daemon fleet, right
  after an upgrade, which is exactly when an operator closes the window
  believing the work is done.
- RestartV3 / IPC restart: only reachable while the GUI still holds a
  console, i.e. under `--foreground` / `--no-tray`, where the operator has
  asked for terminal-coupled lifetime anyway. Lower severity, but it is a
  latent trap the moment those semantics are revisited.

Not fixed in the FIX-1 revision deliberately: the lead's change surface
named the GUI-spawned supervisor, and these are separate command
lifecycles (`install --upgrade`, the restart handler) whose verification is
not covered by the GUI probe above. Each fix is one line — apply
`process.SuppressConsoleAttach(cmd)` next to the creation flags — but each
needs its own spawn-path verification, and `install --upgrade` in
particular must be re-probed end to end because it is the highest-impact
one.

Fix direction: rather than three independent one-liners that can drift
again, consider making the complete detach a single owner (creation flags
AND suppression marker in one `process.DetachChildFromConsole(cmd)` helper)
and routing all four sites through it. The current state — one site
correct, three carrying a comment that asserts the disproven premise — is
the drift the single owner would prevent.

## Resolution (2026-07-20, pending lead gate)

Sent back by the lead under the all-return-paths discipline and fixed in the
same branch. One correction to the enumeration above:

**`install_upgrade.go:424` is a COMMENT site, not a spawn site.** That file
calls `opts.Deps.StartSupervisor(...)` through an interface; the actual
Windows spawn is `spawnSupervisorDetached` in
`install_migration_wiring_windows.go`. So there were TWO real spawn
configurations to fix, not three, plus stale text in a third file. The stale
text was fixed under rule C6 (it asserted the disproven premise verbatim).

Applied through the single owner `process.SuppressConsoleAttach` — the env
name string still appears exactly once, in `internal/process/console_attach.go`:

- `internal/gui/supervisor_restart.go` `newDetachedSupervisorCmd` (manual
  supervisor restart).
- `internal/cli/install_migration_wiring_windows.go` `newInstallSupervisorCmd`
  (install/upgrade supervisor start).

Each inline `build` closure was extracted to a package-level constructor so
the spawn CONFIGURATION is assertable without starting a supervisor.

Per-site measurement, real `-H windowsgui` binary, attachment measured
externally by the console-owning parent via `GetConsoleProcessList`:

```
site A  flags=0x01000208 (DETACHED|NEW_GROUP|BREAKAWAY)  no marker : ATTACHED
site A  flags=0x01000208                                 marker    : NO CONSOLE
site B  flags=0x00000208 (DETACHED|NEW_GROUP)            no marker : ATTACHED
site B  flags=0x00000208                                 marker    : NO CONSOLE
retry   flags=0x00000000 (flags stripped, minimal spawn) no marker : ATTACHED
retry   flags=0x00000000                                 marker    : NO CONSOLE
```

The third row pair is the important one and was not anticipated when this was
filed. `startSupervisorDetachedBreakaway` /
`startDetachedSupervisorTolerant` degrade on ERROR_ACCESS_DENIED by REBUILDING
with creation flags stripped. At `CreationFlags=0` a child with no detach flags
at all still does not become a console client when the marker is set — so on a
locked-down corp host that refuses the detach flags, **the marker is the only
protection that survives**, and it survives only because every retry rebuilds
through the same constructor.

### One site deliberately NOT changed

`internal/gui/gui_self_restart.go` `newRestartV3GUICmd` shares
`configureDetached` with the supervisor spawn but spawns a replacement **GUI**,
not a supervisor, re-parsing the same argv. Under `--foreground` / `--no-tray`
the operator has explicitly asked for a console-attached GUI, so suppressing
the attach would silently kill their terminal output and Ctrl-C across a
restart; in the default background mode the parent has already released its
console, so the marker would be a no-op verifying nothing. Applying it there
would be a defensive call with no verified precondition. The asymmetry is
pinned by `TestNewRestartV3GUICmdDoesNotSuppressConsoleAttach` so a later
consistency sweep cannot quietly "fix" it.

This is also why the marker was NOT folded into `configureDetached` itself:
that helper is shared by a supervisor spawn and a GUI spawn whose console
requirements are opposite.
