---
title: readiness fan-out issues 2 serial WMI identity probes per in-use port (~6.7s each) instead of one batched process snapshot
severity: medium
found-by: backend-engineer (adjacent finding while bounding the readiness probe path)
affected-surface: internal/api/processes.go:503 (processIdentityByPID) + internal/api/readiness.go (AllServerReadiness fan-out)
context: adjacent-finding
status: open
---

## Symptom

`GET /api/server/readiness` (no `?server=` → fleet-wide) took **338 seconds** on a
loaded reference host (Windows 11 26100, 38 live mcphub daemons, 13 of 14 manifest
ports in use). Reproduced by `TestReadinessHandler_AllServers`
(`internal/gui/readiness_test.go:467`), measured at `--- PASS ... (338.14s)` under a
15-minute budget.

The unbounded-probe half of this is FIXED (see "Related fix" below): the endpoint is
now bounded and returns in ~37s. This bug tracks the remaining defect — the endpoint
is bounded but still far slower than it needs to be, and the bound now truncates most
of the fleet report on a busy host, so the operator sees mostly "UNKNOWN" rows.

## Root cause

The fan-out is serial and issues a **separate WMI query per PID, twice per port**.

`internal/api/install.go:1945` `portHeldByOurDaemonForPortArm` calls the
`processIdentityByPID` seam, whose production implementation
(`internal/api/processes.go:503`) is:

```go
image, parentPID, ok := procNameAndParent(pid)     // WMI query #1
...
parentImage, _, _ = procNameAndParent(parentPID)   // WMI query #2
```

Measured cost per probe on the reference host:

| probe | measured |
|---|---|
| `netstat -ano` | 110 ms |
| `schtasks /Query /TN <task> /V /FO LIST` | 177 ms |
| `wmic process where ProcessId=<live pid>` | **4.1 – 6.7 s** |
| `powershell Get-CimInstance` (single PID) | **10.5 s** |

So one in-use port costs roughly `netstat + wmic(lookup) + 2×wmic(identity) + schtasks`
≈ 18s, and 13 in-use ports ≈ 234s — consistent with the observed 338s once the
PowerShell fallback fires on PIDs whose wmic row does not parse.

`internal/api/main_test.go:82-88` already documents the same class independently:
"lookupProcess + lookupProcessBatch are wired at processes.go init() to real
`netstat -ano` + `wmic` shell-outs on Windows (~31s per wmic call on Win11 24H2)".

## Two independent wins available

1. **Skip the parent lookup when it cannot change the answer.** The only consumer
   (`install.go:1949`) is
   `if !isMcphubProcessImage(image) && !isMcphubProcessImage(parentImage)` — OR
   semantics. When `image` is already `mcphub.exe` the second ~6.7s query is dead
   work. On a host where our own daemons hold the ports (the common case) this alone
   halves the cost. Caveat: `processIdentityByPID` is a shared seam; returning an
   empty `parentImage` in that branch changes its documented contract, so the change
   belongs at the seam definition with its callers re-checked, not at one call site.

2. **Batch the identity lookups.** `internal/api/processes.go:523`
   `lookupProcessBatch` already exists and is documented as "Batch variant: one
   netstat + one wmic for N ports" — but the readiness path does not use it, and
   there is no batched equivalent for the *identity* (Name/ParentProcessId) query.
   One `wmic process get Name,ParentProcessId,ProcessId /format:csv` snapshot (the
   shape `runProcessSnapshot` already produces) would replace ~26 per-PID queries
   with one, turning ~234s into ~7s.

## Why it was not fixed in the bounding change

Deliberately out of scope. The admitted change surface was "bound the unbounded
probe so the handler cannot hang"; batching and seam-contract changes are a
performance redesign touching `processIdentityByPID`'s contract and its
install/preflight callers. Filed rather than absorbed, per the adjacent-findings
protocol.

## Related fix

The unbounded-probe defect on the same path is fixed separately:
`internal/api/process_probe_exec.go` (per-probe + fallback-chain deadlines) and the
`allServerReadinessBudget` fan-out budget in `internal/api/readiness.go`. That fix
guarantees the endpoint is BOUNDED and honest; it does not make it FAST. This bug is
what makes it fast.
