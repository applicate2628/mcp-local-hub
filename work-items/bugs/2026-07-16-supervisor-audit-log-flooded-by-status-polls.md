# BUG: the supervisor audit log is 100% flooded by read-only status polls — real events are evicted

Status: open
Filed: 2026-07-16
Severity: P1 (observability destroyed — the audit trail is unusable exactly when an incident needs it)

## Evidence (live host, measured 2026-07-16)

```
last 2000 rows of supervisor-events.log: 2000 = "event":"ipc-command"      # 100%
non-ipc events in the last 800 rows:      0                                # fleet stable, no flapping
supervisor-events.log    6_624_789 b   (current, still filling)
supervisor-events.log.1 10_485_902 b   (already rotated at the 10 MB cap — full)
```

The log has ALREADY rotated once and is 6.6 MB into the next file, containing nothing but read-only
poll rows. Every real lifecycle event — `daemon-exit`, restart-policy transitions, `daemon-quarantined`,
`per-spawn-job-create-failed`, adopt provenance, `hub-listener-*` — is evicted by the single `.log.1`
backfile long before an operator could read it.

## Root cause

- `internal/cli/gui.go:588` starts the GUI's backend `StatusPoller` on a **5-second** interval:
  `gui.NewStatusPoller(s.StatusProvider(), s.Broadcaster(), 5*time.Second)`.
- Each poll calls `api.Status()`, which goes through the supervisor IPC.
- `internal/cli/supervise.go:~1744-1756` emits an audit row for **every** IPC request, unconditionally:
  ```go
  _ = deps.events.TryEmit(api.SupervisorEvent{
      Severity: "info", Source: "ipc", Event: "ipc-command",
      Body: map[string]any{"cmd": req.Cmd, "id": req.ID},
  })
  ```
  The comment above it even states the intent: "Audit: each request gets one `ipc-command` audit row".

So a permanently-running GUI generates ≥12 audit rows/minute (~17k/day) of pure read noise against a
10 MB × 2-file budget. The `StatusPoller` itself correctly publishes only DELTAS to the SSE bus
(`internal/gui/poller.go` compares State/PID/Port/OrphanPID/StalePID/LastResult/JobProtection against
`p.last`), so the SSE side is quiet on a stable fleet — but the IPC audit row is written regardless of
whether anything changed.

## Why it matters

The `supervisor-events.log` is the documented v0.6 audit/event surface (it replaced the v0.4.x
`intent-audit.log`; `mcphub supervise --help` prints its path for exactly this purpose). CLAUDE.md's
Job-Protection runbook tells operators to attach its `severity: warn` entries when escalating to an
endpoint-policy owner. Today those entries are statistically certain to be gone.

## Proposed fix

Do not spend the audit budget on read-only reads. Options, cheapest first:

1. **Skip the audit row for read-only commands** (`status`, and any other pure query) — keep it for
   mutating commands (`restart`, `stop`, `exit`, `quiesce-timers`, force-respawn, strict-mode …). The
   read path is already observable via the GUI's own `hub-mcp.log`/`gui-events.log`.
2. If reads must stay auditable, emit them at `debug` severity and make the writer drop `debug` unless
   an explicit verbose flag is set.
3. Optionally rate-limit/aggregate identical consecutive `ipc-command` rows (e.g. one row per N
   occurrences with a count), mirroring the drop-reporter pattern already in
   `internal/gui/events.go`.

Recommend option 1 + a regression test asserting a `status` IPC command writes NO audit row while a
mutating command still does.

## Notes

Found from an operator observation ("что-то постоянно обновляется/рефрешится") while investigating the
GUI's refresh behavior. The refresh itself is NOT daemon churn — the fleet was verified stable (zero
non-IPC events across 800 rows) and the poller dedups. The audit flood is the real defect the
observation surfaced.
