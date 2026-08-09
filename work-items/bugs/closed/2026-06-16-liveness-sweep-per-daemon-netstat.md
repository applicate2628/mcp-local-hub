---
severity: high
status: closed
found-by: performance-engineer
context: standalone
---

## Description

The supervisor liveness sweep (`sweepSupervisorLivenessOnce`) resolves
port-ownership PER running daemon and each call shells one `netstat -ano`
(Windows) or reads the full `/proc/net/tcp` table + walks `/proc` (Linux).
With 13-15 running daemons the sweep fires 13-15 full-table OS queries
every 5 seconds, indefinitely — the same N×full-scan anti-pattern that
`/api/status` fixed in commit a699713. The status fix was deliberately
scoped to `supervisorStatusDaemons` only; its commit message states the
sweep "keeps the global probe, zero behavior change". The probe-injectable
seam needed to fix the sweep (`supervisorDaemonEntryLiveWithProbe` +
`loopbackPortOwnersSnapshotFn`) already exists and is unused by the sweep.

## Metric

- **Metric**: full process/socket-table OS queries per liveness sweep
- **Budget**: 1 per sweep (one shared snapshot, like the status path)
- **Actual**: N per sweep (one `netstat -ano` / `/proc` walk per running daemon)
- **Baseline**: status path measured at ~730ms cold for 15 daemons (15
  netstat spawns) before a699713; the sweep retains that exact cost,
  amortized continuously at one sweep / 5s

## Files involved

- internal/cli/supervise_liveness.go:158-167 (sweep loop → supervisorDaemonEntryLive)
- internal/cli/supervise_liveness.go:308-310 (global-probe wrapper)
- internal/cli/supervise_liveness.go:82-84 (supervisorPortOwnerPID → per-port netstat)
- internal/cli/supervise_status.go:33,57-78 (the existing snapshot pattern to mirror)
- internal/api/serena_port_owner_windows.go:41-57 (per-port loopbackPortOwnerPID)

## Resolution (closed 2026-06-17 — repo audit)

Fixed-in: de1bde6 "perf(supervisor): one port-owner snapshot per liveness sweep instead of netstat-per-daemon" — the sweep now takes ONE loopbackPortOwnersSnapshotFn() per sweep (supervise_liveness.go:204, outside the per-daemon loop) and resolves every daemon against the shared map, mirroring the /api/status fix (a699713). Finding was filed before de1bde6 landed.
