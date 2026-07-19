# Supervisor IPC status-poll congestion → recurring dashboard RED

Status: open
Discovered: 2026-07-19 (operator host, deployed master)
Severity: P2 (display/control-plane flakiness; MCP serving unaffected)

## Symptom
GUI Dashboard shows the hub RED intermittently ("hub keeps disconnecting"). `mcphub status` (CLI, new IPC
connection) times out; GUI `/api/status` flaps green↔RED. Not a real MCP outage — `claude mcp list` stays
all-green throughout; the daemon fleet + hub listener serve normally.

## Root cause (diagnosed, not yet fixed)
The GUI Dashboard `/api/status` is sourced through the supervisor IPC status seam (named pipe). The event
log shows ~354 `ipc-command` events over ~2 min (≈3 status polls/sec, from the GUI poll + the restart-watcher
health probe) hammering a single-threaded IPC listener. Under that load the supervisor is too slow to write
the connection "hello" frame; the client's read-hello deadline expires first and closes the pipe, so the
supervisor logs `ipc-hello-write-error: "The pipe is being closed."` and the client sees `i/o timeout` →
`/api/status` returns `STATUS_FAILED` → red. Reproduces on a FRESH supervisor (not just an old degraded one),
independent of serena daemon churn — so it is a chronic IPC-congestion design issue in the current binary,
not a one-off. A full supervisor restart (manual, or via `install --upgrade`) clears it temporarily; it can
recur under the same poll-flood conditions.

## Fix options (follow-up)
1. Make the IPC listener service new connections concurrently (write hello off the FIFO event-loop critical
   path), so the status-poll stream cannot starve the accept/hello handshake.
2. Have the GUI hold a persistent IPC connection instead of dialing a fresh pipe per `/api/status` poll.
3. Reduce/coalesce the status-poll rate (GUI + restart-watcher).

## Notes
Pre-existing in master (not introduced by #563 RestartV3). Not fixed here to keep #563 focused. `mcphub status`
timeout is the cleanest repro; `grep ipc-hello-write-error supervisor-events.log` confirms the driver.
