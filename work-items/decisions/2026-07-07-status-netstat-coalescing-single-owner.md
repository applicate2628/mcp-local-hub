---
status: proposed
date: 2026-07-07
---

OS-probe (netstat) frequency on the status path is owned by the supervisor-side coalescer (`internal/cli/supervise_status.go`); client-side status caches (`health.go` DaemonStatusSnapshot) own only IPC round-trip + concurrent-HTTP fan-in amortization and may never re-own OS-probe amortization (anti-layering: one owner per concern); probe-freshness for restart/kill decisions stays with the liveness sweep / `daemon recover` paths and never routes through the status cache. The netstat is ctx-bounded (3s) so a wedged system network stack degrades to port_owner_unverified rather than a hung status IPC.
