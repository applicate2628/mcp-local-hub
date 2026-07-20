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

## Measurement reconciliation (the "~31s per wmic call" record)

`internal/api/main_test.go:82-88` records "~31s per wmic call on Win11 24H2".
This host IS 24H2 (build 26100). Measured here, deliberately including samples
taken WHILE the readiness fan-out was hammering WMI:

| query | idle | under fan-out load |
|---|---|---|
| `wmic process where ProcessId=<live>` (single-PID, filtered) | 2.3-4.6 s | 1.6-10.3 s |
| `wmic process get ...` (FULL table, 1018 processes) | 2.9-8.2 s | — |
| `powershell Get-CimInstance` (single PID) | 10.5 s | — |
| `powershell Get-CimInstance` (full table) | 7.8 s | — |

The obvious hypothesis — that 31s was a full-table enumeration and the newer
numbers were a cheap filtered query — was tested and **does not hold**:
full-table is 2.9-8.2s here, not the slow one. The worst single probe observed
under load was **10.3s**.

So 31s could not be reproduced, and also could not be falsified — it is most
likely a different host or WMI-repository state (a rebuilding or corrupted WMI
repository is exactly this pathology). The probe caps were therefore sized to
ACCOMMODATE it (`probeCommandTimeout` 45s, `probeChainBudget` 60s) rather than
to match today's numbers, because the harm is asymmetric: a cap set too high
costs bounded latency, while a cap set too low manufactures false timeouts and a
report that is mostly UNKNOWN. Full argument in `process_probe_exec.go`.

## Follow-up A — the readiness budget does NOT gate the admission phase

**This is the dominant unbounded-latency path and it is NOT covered by the
budget threading already shipped.** An earlier revision of this file claimed a
"~80s worst case (20s budget + one 60s chain)". That number was wrong by an
order of magnitude and is corrected here.

Two independent reasons:

1. **`AdmissionCheck` is un-budgeted.** `readiness.go:564` calls it with no
   budget, and `admission_check.go:216` / `:227` run the full ownership chain per
   in-use port arm BEFORE any budget-gated `fixedPortStatus` row executes. The
   shipped budget gates only the detail rows layered on top, not the probes that
   actually seed `Ready`.
2. **One ownership call is not one chain.** `portHeldByOurDaemonForPortArm`
   stacks several independently-capped probes, and the ancestry walk mints a
   FRESH `probeChainBudget` per iteration (`internalPortParentWalkDepth = 3`):

   ```
   IPC ~5s + lookupProcess (netstat 45s + wmic 45s) + walk 3x60s
     + lookupProcess again 90s + processIdentityByPID 2x60s + schtasks 15s
   ~= 500s for ONE port arm on a fully wedged host
   x2 arms for a native-http daemon ~= 16 min (more for a multi-daemon manifest)
   ```

   This is a full-wedge CEILING, not a typical figure — a healthy host
   short-circuits at `portAvailable` before any subprocess runs, and the measured
   real-world fleet report is ~24s. But the ceiling is MINUTES.

**Why it was not threaded:** `AdmissionCheck` is shared with the install
Preflight gate (`install.go:1844`), where truncating an ownership probe would be
actively harmful — Preflight must probe properly before mutating. A readiness-only
budget therefore cannot simply be added to its signature; it needs a design that
distinguishes the diagnostic caller from the gating caller.

**Also disclosed:** raising the probe caps (15s/20s -> 45s/60s) to accommodate the
31s record made this wedged corner ~3x slower than before (~180s -> ~500s per port
arm). Deliberate honesty-over-latency trade; see the cap argument in
`process_probe_exec.go`.

## Follow-up B — in-flight probes are not interruptible

Interrupting an already-running probe would require threading a context through
the `lookupProcess` seam, which has **97 test fakes**. Until then a probe that has
started always runs to its own cap.

## Follow-up C — a preflight test is real-WMI-load-bound

`TestPreflight_AllowsSupervisorIntentNativeHTTPInternalPortAndRejectsRowlessPort`
exercises the real probe stack: measured 55.8s here and 14.6s by a reviewer on
another host, i.e. it floats with host WMI load rather than with any deadline. Its
subject is admission logic, not the probe stack, so it should fake the identity
seam (the `fakeProcessNameAndParent` / `fakeProcessNameAndParentTimingOut` pattern
now exists in `install_own_port_test.go`). Left alone here to keep a
documentation-only round free of test-behavior changes.

## Related fix

The unbounded-probe defect on the same path is fixed separately:
`internal/api/process_probe_exec.go` (per-probe + fallback-chain deadlines) and the
`allServerReadinessBudget` fan-out budget in `internal/api/readiness.go`. That fix
guarantees the endpoint is BOUNDED and honest; it does not make it FAST. This bug is
what makes it fast.
