---
severity: medium
found-by: $architect (adjacent finding during port-resolution design, 2026-07-05)
resolved-by: PR #505 (port-resolution owner) — the Codex bot escalated it from
  deferred-backlog to a live REGRESSION (deleting F5 activated the gap), so it was
  fixed in-PR rather than deferred: forceKillOneSupervisorTarget now resolves
  api.EffectiveDaemonPort(d) at entry, so a legacy Port=0 row engages the port
  wait/kill path on its resolved manifest port.
depends-on: 2026-07-05-unify-port-resolution-owner
---

- **status:** fixed
- **fixed-by:** PR #505 (`e306cbd7`) - force-stop resolves `api.EffectiveDaemonPort`.
- **HEAD reconciliation (2026-07-09):** Verified against master `63b6a008`; see `TRIAGE-2026-07-09.md` for code/test evidence.

# force-stop shares the Port=0 kill-by-port gap

## Symptom / risk

`internal/api/stop_force_supervisor.go` kills each target "by its descriptor
Port via the shared port-kill primitive" (file header :62). A legacy descriptor
with `Port == 0` (the exact class F5 / the port-resolution refactor targets)
cannot be killed by port — there is no port. When the trusted PID kill is
unavailable or untrusted AND `d.Port == 0`, the port classifier has nothing to
fall back on, so a lost-child squatter of a port-less legacy daemon is not
force-stoppable by port. Same structural gap as the liveness / recover / squatter
Port=0 disablement the F5 backfill (and its successor, the single
port-resolution owner) address — this consumer was NOT in F5's activation set.

Confirmed sites (verified 2026-07-05, file:line):
- `stop_force_supervisor.go:156` — `if d.Port == 0 { return RestartResult{...} }`
  short-circuits to success after a PID kill with no port-release verification
  (correct when the PID kill already proved death, but the Port=0 branch is the
  same latent assumption).
- header `:60-64` — the by-port kill path is the documented mechanism, so a
  Port=0 descriptor has no port-based force-stop at all.

## Correct fix (deferred to the port-resolution owner)

This should be fixed by the SAME single lazy port-resolution owner designed in
`work-items/active/2026-07-05-unify-port-resolution-owner/design.md`: once
force-stop resolves the effective port through `api.EffectiveDaemonPort`
(descriptor Port when >0, else manifest), the Port=0 gap closes here for free —
no separate special-case. `Depends-on` that work-item; do NOT patch a standalone
Port=0 branch here (that would be exactly the layered-patch pattern the refactor
removes).

## Non-goal
Not part of PR #504 (serena-guard). Filed so the port-resolution owner's
reader/writer inventory includes this consumer.
