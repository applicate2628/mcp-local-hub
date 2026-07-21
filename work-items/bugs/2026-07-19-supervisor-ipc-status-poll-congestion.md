# Supervisor IPC status-poll congestion → recurring dashboard RED

Status: open
Discovered: 2026-07-19 (operator host, deployed master)
Severity: P2 (display/control-plane flakiness; MCP serving unaffected)

## Symptom
GUI Dashboard shows the hub RED intermittently ("hub keeps disconnecting"). `mcphub status` (CLI, new IPC
connection) times out; GUI `/api/status` flaps green↔RED. Not a real MCP outage — `claude mcp list` stays
all-green throughout; the daemon fleet + hub listener serve normally.

## ⛔ Root cause below is FALSIFIED (2026-07-20 log forensics) — do not fix against it

**The congestion hypothesis is refuted by the data.** Corrected analysis follows;
the original text is retained beneath it for provenance only.

**Load does not cause it — load is flat, and throughput COLLAPSES when the errors
fire.** Over the full window the poll rate is constant (~1400 polls/hour). Measured:

```
2026-07-19T10:40..10:59   status=20-26/min   helloERR=0     <- baseline
2026-07-19T11:01          status= 2/min      helloERR=2
2026-07-19T11:02..11:17   status= 2-11/min   helloERR=1-5   <- 80% throughput collapse
2026-07-19T11:22..11:24   status=23-26/min   helloERR=0     <- recovered
```

The poller never sped up; **the supervisor stopped servicing**. That is the
opposite of the causal direction the original diagnosis assumed. Only two long
clusters exist (07-19 07:31→09:11, 63 errors / 99 min; and 11:00→11:57, 68 errors /
57 min) plus 3 lone singletons — and **zero** on 07-18 and 07-20 under identical
poll load.

**"A restart clears it" is also false.** The 11:18:02 restart bought ~23 clean
minutes (11:22–11:45), then the cluster resumed on the **fresh** supervisor
(11:45:22 … 11:57:20). The condition is **host-level, not supervisor-internal
state** — so a supervisor-side IPC fix alone will not close it.

**Fix option 1 is already implemented.** The hello write is NOT on the accept hot
path: `internal/cli/supervise.go:1651` dispatches `go serveIPCConn(conn, listener,
deps)`, and a failure closes only its own connection then `TryEmit`s the row
(`:1669-1698`). The write is bounded by a 10s deadline
(`internal/cli/supervise_ipc_common.go:23,70`) and the error string originates at
`:73-75`. `"The pipe is being closed."` means the **peer** closed first. One
abandoned client therefore cannot stall another — that property already holds.

**Impact, measured:** 57,361 of 57,369 IPC commands in the window were
`cmd:status`. All 8 mutating commands (6 `reconcile`, 1 `quiesce-timers`, 1 `exit`)
landed OUTSIDE both clusters, so no mutating command was lost — but nothing
structurally prevents that. Each lost status poll = one `/api/status` failure = one
Dashboard RED flap. **No daemon death.**

## Corrected fix direction

1. **Find the host-level cause of the servicing collapse.** Both clusters need
   correlating against host state (an EDR/AV scan pass, handle pressure, a system
   sleep/resume, disk stall). This is the actual open question; the supervisor is
   the victim, not the culprit.
2. **Log level.** Do NOT flat-demote. The individual row is benign per-connection,
   but the *cluster* is the only durable evidence of a real 99-minute degradation.
   Per-connection row → `debug`; add a rate-based `warn`/`error` (N failures in M
   minutes) so the signal survives without training the operator to ignore 134
   identical rows.
3. **Persistent GUI IPC connection** (was option 2) remains worthwhile on its own
   merits — fewer handshakes is strictly better — but it is a mitigation of the
   symptom, not the fix.
4. Original option 3 (reduce poll rate) is **rejected**: the rate is not the cause.

Related: `835ee3e4 (#566)` removed the read-only status rows from the audit log,
which cut ~96% of log volume. That addressed audit flood, not this. Note the side
effect tracked in `2026-07-20-supervisor-never-crash-reliability`: the supervisor
now emits nothing for hours, so healthy-and-quiet is indistinguishable from dead —
a heartbeat row is being added there.

<details>
<summary>Original (falsified) root-cause text, retained for provenance</summary>

The GUI Dashboard `/api/status` is sourced through the supervisor IPC status seam (named pipe). The event
log shows ~354 `ipc-command` events over ~2 min (≈3 status polls/sec, from the GUI poll + the restart-watcher
health probe) hammering a single-threaded IPC listener. Under that load the supervisor is too slow to write
the connection "hello" frame; the client's read-hello deadline expires first and closes the pipe, so the
supervisor logs `ipc-hello-write-error: "The pipe is being closed."` and the client sees `i/o timeout` →
`/api/status` returns `STATUS_FAILED` → red. Reproduces on a FRESH supervisor (not just an old degraded one),
independent of serena daemon churn — so it is a chronic IPC-congestion design issue in the current binary,
not a one-off. A full supervisor restart (manual, or via `install --upgrade`) clears it temporarily; it can
recur under the same poll-flood conditions.

</details>

## Notes
Pre-existing in master (not introduced by #563 RestartV3). Not fixed here to keep #563 focused. `mcphub status`
timeout is the cleanest repro; `grep ipc-hello-write-error supervisor-events.log` confirms the driver.
