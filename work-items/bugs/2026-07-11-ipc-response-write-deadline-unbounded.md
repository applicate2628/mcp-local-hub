# Bug: supervisor IPC response writes are deadline-unbounded

Status: open
Filed: 2026-07-11
Severity: P3 (same-user surface, bounded; pre-existing)
Source: fable-5 deep review of PR #530 (finding P3-1)

## Symptom

`writeIPCFrame` (`internal/cli/supervise.go`, the response-side writer used by
`handleIPCConn`) does a bare `conn.Write` with NO write deadline. `handleIPCConn`'s
`ipcConnIdleTimeout` (60s) bounds only READS, not writes.

Scenario: a same-user IPC client sends a command (e.g. `status`), never drains the
response, and the response exceeds the pipe/socket output buffer (~4 KiB — plausible
with a large daemon fleet). The connection's `handleIPCConn` goroutine then parks in
`Write` indefinitely; the idle reaper never fires because the goroutine is not in
`Read`. Repeated such clients accumulate parked goroutines + open connections.

## Relationship to PR #530

PR #530 fixed exactly this defect class for the HELLO write (moved it off the accept
loop into `serveIPCConn` and bounded it with `ipcHelloWriteTimeout` + a cleared
deadline). The response-write path (`writeIPCFrame`) has the same unbounded-write
shape and was NOT touched by #530 (out of scope — #530 was the accept-loop decouple).

## Fix direction

Set a write deadline around `writeIPCFrame`, mirroring `WriteHello`'s pattern
(`SetWriteDeadline(now + bound)` before the write, cleared or connection-closed after).
A per-response bound (or a per-connection write deadline refreshed alongside the read
deadline in `handleIPCConn`'s loop) bounds the goroutine. Same owner-only trust
boundary as #530, so this is hardening, not a security-critical fix.

## Scope note

Bounded, same-user-only (owner-only pipe SDDL / 0600 socket), pre-existing. Not a
merge blocker for #530. File a small follow-up PR mirroring the #530 deadline pattern.
