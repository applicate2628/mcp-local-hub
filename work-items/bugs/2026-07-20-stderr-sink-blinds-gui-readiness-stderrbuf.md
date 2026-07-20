# Bug: stderr sink silently blinds the GUI supervisor-owner's readiness-timeout stderr diagnostic (PR #212 r5 finding 2 regression)

- id: 2026-07-20-stderr-sink-blinds-gui-readiness-stderrbuf
- context: feat/supervisor-death-forensics (branch, pre-merge)
- status: open
- severity: medium
- area: internal/cli/gui_supervisor_owner.go:133-148 vs internal/cli/supervisor_stderr_sink.go
- found-by: qa-engineer

## Reproduction / scenario

`internal/cli/gui_supervisor_owner.go:139-148` spawns the supervisor with
`c.Stderr = stderrBuf` (bounded 4 KiB buffer) explicitly so that "a startup
crash (corrupt supervisor-intent.json, state-path sanity rejection, internal
panic before any audit log row lands) is visible in the readiness-timeout
error rather than dropped silently. PR #212 r5 finding 2."

With the death-forensics branch, the spawned supervisor sees stderr = a pipe,
`stderrIsInteractiveConsole()` is false, and the redirect fires immediately
after the singleton lock. Everything written after that point — including the
exact startup-crash text the stderrBuf exists to carry — goes to
`supervisor-stderr.log` instead of the pipe. The GUI's readiness-timeout
error regresses to an empty/truncated stderr tail for every post-lock
failure. Only pre-lock failures (state-dir resolve, lock acquire) still reach
the buffer, because cobra prints the returned error after
`stderrSink.release()` has restored the pipe binding.

## Expected vs actual

- Expected: the GUI readiness-timeout error keeps carrying the supervisor's
  startup-crash text (existing diagnosability contract), or is explicitly
  re-pointed at the new durable location.
- Actual: the tail is silently empty for post-lock startup failures; the
  operator-facing error loses its diagnostic payload with no pointer to where
  it went.

## Suggested direction

Either exempt nothing and instead make the readiness-timeout error message
name `supervisor-stderr.log` as the place to look (and include its tail), or
have the sink write-through/tee the pre-`reconcile-ready` window. At minimum
the readiness-timeout error text should reference the sink path.
