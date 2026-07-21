---
status: accepted
date: 2026-07-20
owner: Lead (main conversation)
---

# Defer the serena cold-start concurrency cap; ship jitter + a derived 90s budget and measure

## Context

The serena readiness-timeout inversion (inner `HealthTimeout` 30s vs outer
`StartupBindDeadlineSeconds` 120s) is fixed by deriving the inner budget from the
outer one — 3/4, so serena gets 90s. The implementer escalated a second question
rather than deciding it silently: should the six-way simultaneous serena cold
start also be capped by a `ColdStartConcurrency`-style limiter, as the LSP lazy
proxy already does?

The implementer shipped **jitter only** and stated plainly that jitter is the
weaker option: six ~46s CPU-bound cold indexes are not decongested by spreading
start times a few seconds. Jitter breaks lockstep *re-collision on retry waves*;
it does not reduce concurrent load.

## Decision

**Ship jitter + the derived 90s budget. Do NOT build the concurrency cap yet.
Measure first.**

## Rationale

1. **The inversion alone is sufficient to produce the reported bug.** 46s > 30s
   fails with *zero* contention. The herd explains the six-at-once waves and the
   dilated `attempts=92` probe counter, not the root cause. Fixing the root cause
   is not blocked on fixing the amplifier.

2. **90s is very likely sufficient, and the arithmetic is checkable.** The measured
   cold index is 46s (`api/supervisor_intent.go:83`). The observed contention
   dilation is `attempts=150` unloaded (200ms cadence) versus `attempts=92`
   (≈326ms per probe) loaded — a ~1.6× slowdown. 46s × 1.6 ≈ 74s, inside a 90s
   budget with ~16s of headroom. This is an estimate, not a measurement, and is
   labelled as such below.

3. **The cap is not an implementation detail.** In `supervisorController` a
   deferred spawn must answer what happens to `queued_action`, quarantine
   accounting, the arm-generation guard, and reap/stop races — across a
   ~4400-line state machine. Under a "must not crash at all" mandate, adding
   lifecycle semantics to the restart path to fix an *amplifier* is the wrong
   risk trade when the root cause is already addressed.

4. **The falsifying measurement arrives for free.** Every supervisor restart
   produces exactly the six-way contended serena cold start in question. The next
   deploy IS the experiment.

## Correction after adversarial review (2026-07-20, same day)

The reviewer sharpened one point that materially narrows what jitter buys, and it
is recorded here rather than left in the review transcript:

**Jitter lands only in `armRespawnBackoffTimer` (`supervisor_controller.go:3912`)
— i.e. on RESPAWN arms.** The initial reconcile still spawns all six serena
proxies simultaneously. So on the case that actually matters — a supervisor
restart, which is by far the most common way six serena cold starts happen at
once — the first-wave contention is absorbed **solely by the 90s budget**, with no
spreading at all. Jitter only prevents the retry waves from re-colliding in
lockstep after that first wave.

This does not change the decision (the inversion is still the root cause, and the
arithmetic still supports 90s), but it removes any comfort that jitter partially
covers the contended case. It makes the measurement below the **only** protection
for the first wave, so the falsification is not optional follow-up — it is the
verification this decision rests on.

Reviewer also quantified the margin more precisely than I did: ~16s (18%)
headroom, break-even dilation **≈1.96×**, with the caveat that probe-loop dilation
(the 200ms→326ms figure) is a *weak proxy* for CPU-bound index dilation — the
index may dilate more than a mostly-sleeping probe loop. Treat 1.96× as an
optimistic break-even.

## Falsification criterion

After deploying this fix, across three consecutive supervisor restarts:

- **PASS** — zero `upstream not ready after 90s` lines in any
  `%LOCALAPPDATA%\mcp-local-hub\logs\serena-*.log`, and no serena entry in
  `daemon-respawn-scheduled` attributable to a readiness timeout.
- **FAIL** — any such line. Then the cap is warranted and routes to `$architect`
  for the lifecycle-semantics design, with the observed elapsed-to-ready
  distribution as its input.

`ASSUMPTION (UNVERIFIED)`: that 90s survives six-way contention on this host. It
could not be measured without loading the operator's live machine. The probe above
settles it.

## Consequences

- The `ColdStartConcurrency` cap stays an open P2 item, escalated not dropped.
- If the measurement fails, we will additionally have the elapsed-to-ready
  distribution, which the cap design needs anyway — so the deferred path loses
  nothing.
- Jitter is upward-only by construction, so it can never retry sooner than
  today's ladder; it cannot accelerate a daemon toward the 10-in-30min quarantine
  threshold.

## Related

- `work-items/active/2026-07-20-supervisor-never-crash-reliability/plan.md` (P0.2)
- Precedent for the capped shape: `internal/daemon/lazy_proxy.go`
  (`ColdStartConcurrency`, default 2) + CLAUDE.md "LSP router — cold-start
  contract (P2c)".
