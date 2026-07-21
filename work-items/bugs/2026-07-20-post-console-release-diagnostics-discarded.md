# Bug: after ReleaseParentConsole, GUI runtime diagnostics with no file sink are silently discarded on terminal-launched runs

- id: 2026-07-20-post-console-release-diagnostics-discarded
- context: 2026-07-20-cli-first-run-ux
- status: open
- severity: medium
- area: internal/cli/gui.go + internal/cli/gui_supervisor_owner.go
- found-by: qa-engineer

Commit `27f42953` calls `process.ReleaseParentConsole()` at
`internal/cli/gui.go:1262-1264`. FreeConsole closes the process's console
handles, so every later `os.Stdout`/`os.Stderr` write on a terminal-launched
GUI (where stdio was rewired to CONOUT$ by `attachParentConsoleIfAvailable`)
is discarded. Redirected runs (`mcphub gui > log 2>&1`) are unaffected (file
handles survive FreeConsole); Explorer/Task-Scheduler launches were already
sink-less. The regression is exactly the operator sitting at the still-open
terminal, who used to see these messages.

Writes after the release point with NO file-sink companion (checked: zero
`LogHubMcpEvent` calls in gui.go; the exit monitor has none either):

- `internal/cli/gui_supervisor_owner.go:574-584` — "warning: supervisor
  exited unexpectedly (PID %d): %v; stderr tail: %s". The captured
  supervisor stderr tail is the ONLY record of a pre-audit-log supervisor
  crash (boundedBuffer capture exists precisely because such deaths were
  otherwise unattributable). Now discarded. The respawn-loop messages
  (respawn-cap, respawn-failed, gui_supervisor_owner.go:440-476) DO
  dual-write to LogHubMcpEvent — only the exit-attribution path is
  file-sink-less.
- `internal/cli/gui.go:1341-1408` — every tray runtime failure
  ("tray: POST /api/stop-all: …", "tray: %v (GUI continues without tray)").
  A tray that dies post-startup now disappears with zero durable trace.
- `internal/cli/gui.go:1269` — browser auto-launch warning (minor).
- Go panic output (stderr) — a GUI panic after the release point leaves no
  trace anywhere on a terminal launch.

Expected: per the repo's own current direction (supervisor stderr sink work,
motivated by 6 unattributable deaths), operator-critical diagnostics must
have a durable sink before the console is discarded. Either add
`LogHubMcpEvent` companions for the file-sink-less writes above (at minimum
the supervisor exit-attribution path, including the stderr tail) or redirect
os.Stderr to a file at the release point.

## Resolution (2026-07-20, pending lead gate)

FIXED by swapping the writers rather than editing each call site.
`internal/cli/gui_diagnostic_sink.go` adds a switchable sink installed as
the gui command's out/err writer, so every existing `cmd.OutOrStderr()`
site — and every future one — is covered without knowing it exists. After
the release it dual-writes: a durable `gui-console-released-diagnostic`
hub-mcp event FIRST, then the original stream, so a redirected run
(`mcphub gui > log 2>&1`) is bit-for-bit unaffected while a console run
still lands somewhere.

The supervisor exit-attribution path needed one extra step: its sink is
captured into each `supervisorOwner` at construction, which happens BEFORE
the release, so `supervisorMonitorStderr` now defaults to the switchable
sink instead of a bare `os.Stderr` — the already-captured reference then
follows the switch.

Measured, faithful `-H windowsgui` reproduction of the loss this fixes:

```
post-release os.Stderr.Write -> n=0 err=write \.\CONOUT$: The handle is invalid
```

The write does not merely go nowhere — it returns an error that every
`fmt.Fprintf` call site discards, which is why the loss was silent.

NOT COVERED, deliberately: Go runtime panic output. The runtime writes it
to file descriptor 2 directly rather than through any `io.Writer`, so no
writer swap can intercept it. Capturing it requires re-pointing the
process's stderr FD at a file, a different mechanism and a separate change.
