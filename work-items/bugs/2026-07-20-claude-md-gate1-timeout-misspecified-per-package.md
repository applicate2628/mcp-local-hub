---
title: CLAUDE.md Gate 1 `go test -timeout 5m ./...` is mis-specified — -timeout is PER PACKAGE and two packages exceed it, so the mandatory pre-push gate cannot pass as written
severity: medium
found-by: backend-engineer (while settling a misattributed "readiness test hangs" report)
affected-surface: CLAUDE.md "Step 1 — Pre-push local verification" + internal/gui + internal/api test wall-time
status: open
---

## Symptom

CLAUDE.md specifies as a hard, mandatory pre-push gate:

```bash
go build ./... && go vet ./... && go test -count=1 -timeout 5m ./...
```

This gate **cannot pass on this repository as written**, and has not been able to for
some time. Every contributor running it sees a `panic: test timed out after 5m0s`, and
because the panic names whichever test happened to be on the clock, the blame lands on
a rotating cast of innocent tests. That is exactly what happened here: a report titled
"internal/gui readiness test hangs blocking full gate" attributed the failure to
`TestReadinessHandler_AllServers`, which turned out to be only partly responsible.

An always-red mandatory gate trains people to stop reading it. Multiple lanes in a
single session independently mis-diagnosed the same symptom because of it.

## Root cause

`-timeout` is applied **per test binary**, not to the run as a whole. From
`go help testflag`:

> `-timeout d` — If a test **binary** runs longer than duration d, panic.

`go test ./...` compiles and runs one test binary per package, so `-timeout 5m ./...`
grants each package its own 5 minutes. The gate therefore fails as soon as any SINGLE
package needs more than 5 minutes — regardless of how fast the rest of the tree is.

Measured on the reference host (Windows 11 26100):

| package | wall time | vs 5m per-package budget |
|---|---|---|
| `internal/gui` | see "Measurements" below | OVER |
| `internal/api` | > 300s (timed out at 5m) | OVER |
| `internal/scheduler` | 0.055s | fine |
| `internal/api/binary_discovery` | 0.030s | fine |
| `internal/api/daemon_env_overlay` | 0.550s | fine |
| `internal/api/lsp_routing` | 0.101s | fine |
| `internal/api/serena_routing` | 0.139s | fine |

The tree is bimodal: almost everything is milliseconds, and two packages are minutes.

## Recommended correction (argue the budget, do not just raise it to today's number)

Raising `5m` to whatever the slowest package measured today would re-break the moment
anything gets slower, and it would also hide the fact that a multi-minute unit-test
package is itself a defect. Recommend instead:

1. **Split the gate into a fast lane and a slow lane**, so the fast lane stays a
   genuine seconds-level pre-push check that people actually run:

   ```bash
   go build ./... && go vet ./...
   go test -count=1 -timeout 2m $(go list ./... | grep -vE '/internal/(gui|api)$')
   go test -count=1 -timeout 20m ./internal/api/ ./internal/gui/
   ```

2. **Budget the slow lane at ~2x the measured worst package, rounded up.** Headroom
   reasoning: these two packages spawn real subprocesses (`wmic`, `netstat`,
   `schtasks`, real `mcphub` binaries) whose cost scales with how loaded the developer's
   machine is; the same `wmic` probe measured 4.1s idle and 6.7s under load here, and
   `internal/api/main_test.go:82-88` records "~31s per wmic call on Win11 24H2" on
   another host. A 2x multiplier absorbs that host-to-host and load-to-load variance
   without being so loose that a genuine new hang goes unnoticed for an hour.

3. **State the per-package semantic explicitly in CLAUDE.md**, since the current
   wording invites everyone to read `5m` as a total. One sentence prevents the next
   misattribution.

4. Track the underlying slowness as its own defect rather than absorbing it into a
   bigger number — see "Related" below. The correct end state is a fast gate, not a
   patient one.

## Measurements

- `internal/gui` full suite, pristine HEAD `d8ab4777`, reported by a sibling lane:
  **935.216s (15.6 min)**, PASS under a 20-minute budget.
- `TestReadinessHandler_AllServers` **in isolation** (`-run` filtered, so no other test
  consumed the budget), pristine HEAD: **338.14s** PASS under a 15-minute budget. This
  single test alone exceeds the 5m per-package gate.
- `TestDeAdoptPlanRouteNeverSerializesExecutionState`, pristine HEAD, reported by a
  sibling lane: **121.30s** — slow but finite.
- `TestExecuteAdoptPersistsProvenanceAcrossFreshAPI` in isolation: **85.14s**.
- `internal/gui` full suite AFTER the readiness probe-bounding fix: recorded below.

## Related

- `work-items/bugs/2026-07-20-readiness-fanout-serial-wmi-identity-storm.md` — why the
  readiness endpoint costs 338s (serial per-PID WMI identity probes). Bounding it cut
  `TestReadinessHandler_AllServers` from 338.14s to 37.06s.
- The `internal/gui` and `internal/api` packages spawning real subprocesses and real
  `mcphub` binaries in unit tests is the structural reason both are minutes rather than
  seconds. Worth its own item: a 15-minute unit-test package is a defect even when
  nothing is hanging.
