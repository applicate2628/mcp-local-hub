# Bug: timed-out ownership probe degrades to OWNED (Ready=true) on the internal-port arm

- id: 2026-07-20-readiness-timeout-downgrades-to-owned-internal-port
- context: fix/readiness-handler-unbounded-probe (7e4c4955, QA review)
- status: open
- severity: high
- area: internal/api/install.go:2025-2033, internal/api/install.go:2044-2073, internal/api/processes.go:699-726
- found-by: qa-engineer

## Reproduction

Unit-level, against production code at 7e4c4955 (test file used during QA review,
not committed — reconstruct from the snippet below; it FAILS, i.e. the lie fires):

- Seed `supervisor-intent.json` with a native-http row (Server=demo, Daemon=alpha,
  Port=externalPort).
- Fake `supervisorIPCStatusFn` → live wrapper PID for the task.
- Fake `lookupProcess(internalPort)` → a listener PID != wrapper PID (netstat ANSWERED).
- Fake `processNameAndParentByPID` → `("", 0, false)` — exactly what
  `procNameAndParent` returns when both wmic and PowerShell hit the new
  `probeChainBudget` (`ErrProbeTimeout` is erased to a bare `ok=false`).
- Call `portHeldBySupervisorIntentDaemon(internalPort, "demo", "alpha")`.

Expected: `false` (ownership NOT established — probe timed out; readiness then
reports the honest "in use ... ownership could not be verified" not-ready).
Actual: `true` — `internalPortListenerChainsToWrapperPID` returns
`resolved=false` on the `!ok` walk step (install.go:2060-2062), and the caller's
downgrade (install.go:2030-2033, "No usable port-owner or ancestry proof")
returns OWNED. AdmissionCheck then emits no port finding → `ReadinessReport.Ready=true`.

## Why this violates the fix's own contract

Commit 7e4c4955 states: "A timed-out probe degrades to an honest not-ready +
reason. It must NEVER degrade to ready." `ErrProbeTimeout`
(process_probe_exec.go:17) documents "Callers MUST treat it as unknown, never as
a negative answer" — but the sentinel has ZERO production consumers: it is
erased at every boolean seam (`procNameAndParent`, `lookupProcess`,
`processIdentityByPID` return bare `ok`). The pre-existing internal-port
downgrade was designed for "surface unavailable" (nil seam on probe-less hosts —
pinned by TestPortHeldBySupervisorIntentDaemonInternalPortRequiresWrapperAncestry's
`lookupProcess = nil` case); the fix routes the NEW "probe ran and timed out"
state into that same branch. Pre-fix a wedged-WMI host HUNG here (visible);
post-fix it silently claims ownership. By the commit's own standard ("a hang is
visible, a lie is not"), this arm regressed.

Reachable surfaces: native-http internal ports (`Port+NativeHTTPInternalPortOffset`)
and `runtime_spec.UpstreamPort` rows (serena dynamic pool) — via
admission_check.go:227 and readiness.go:395 → the live GUI `/api/server/readiness`.
Trigger condition is exactly the one the fix targets (WMI slow/wedged); note
internal/api/main_test.go:82-88 records ~31s per wmic call on Win11 24H2, which
exceeds the 15s per-probe cap — on such hosts the chain times out routinely,
making the internal-port arm systematically Ready-on-unverified.

## Fix direction (for the implementer)

Distinguish "probe ran and timed out / answered" from "probe surface
unavailable" at the seams (tri-state or error return), and make the timeout
case yield `owned=false` from the ownership gate so readiness degrades to the
already-implemented honest "ownership could not be verified" not-ready. Add a
regression test pinning the timeout arm (the existing ancestry test pins only
resolvable-foreign and nil-surface).
