---
- id: 2026-07-27-supervisor-event-flock-release-single-owner
- status: proposed
- decided-by: $architect (REVISE closure, PR `feat/liveness-headless-gui-recovery`)
- context: seventh recurrence of "cross-process event-log flock release outcome discarded at the call site", the sixth of which was itself inside the fix for the fifth
- supersedes: none
- superseded-by: none
---

# The `supervisor-events.log` flock-release verdict gets ONE process-scoped owner, not 131 call-site returns

## Decision

**A cross-process flock-release failure on `supervisor-events.log` is recorded by a single
process-scoped owner keyed by log path, written from inside `internal/api` at the four points
where the release outcome is already computed, and READ by whoever must report it. It no
longer needs to travel on the return value of individual `Emit` calls.**

Concretely:

1. New owner `internal/api/supervisor_event_lock_health.go`. It answers exactly one
   question, for one lock path: *can this process still be holding the
   `supervisor-events.log` flock?* — `released` / `outstanding` / `stranded`.
2. It is fed from the four sites in `internal/api/supervisor_events.go` that already
   compute a release outcome (the synchronous releaser, the bounded-emit worker handoff,
   the worker's own release, and `TryReplayPending`'s releaser). **No call site changes.**
3. `daemonrecovery.committedAuditFinalizer` STOPS deriving `AuditHandoff` from its own
   per-call error enumeration and instead READS the owner. The optimistic
   `handoff := AuditHandoffDurable` default — the direct cause of finding F1 — is deleted
   along with the enumeration that made it reachable.
4. `AuditHandoff`'s documented meaning widens from "every flock **this recovery** took was
   confirmed released" to "every flock **this process** took on this log was confirmed
   released". The per-recovery scoping was the category error; see Rationale.

### Scope this PR converts

| Surface | Converted? | Why |
|---|---|---|
| `internal/api/supervisor_events.go` — 4 release-outcome sites | **Yes** | The producer. This is where the owner is fed. |
| `internal/daemonrecovery/recovery.go` `finalize()` (F1) | **Yes** | The one reader. Fail-open default deleted. |
| `internal/daemonrecovery/recovery.go:793` `emitRecoverAuditEvent` (F6) + its 15 callers | **Yes, without being touched** | All 15 emit through `SupervisorEventLog.Emit`, whose release failure the owner now records. The `_ =` at the call site becomes correct rather than lossy. |
| The other **114** discarded `.Emit(...)` sites (`internal/cli/supervise.go` ×62, `supervisor_controller.go` ×21, 19 further files) | **Yes, without being touched** | Same mechanism. None of them needs a per-call consumer. |
| `internal/api/gui_event_log.go:164` — `gui-events.log` flock | **No** | Different lock. See below. |
| `internal/api/intent_audit.go:494` — `intent-audit.log` flock | **No** | Different lock. See below. |

### Measured class size (larger than the review that triggered this decision assumed)

Two independent inventories on this tree, both non-test files only:

- **Discarded emit verdicts:** `_ = <x>.Emit(` — **132 sites across 23 files**. Top
  contributors: `internal/cli/supervise.go` (62), `internal/cli/supervisor_controller.go`
  (21), `internal/cli/supervise_respawn.go` (6), `internal/cli/supervise_realloc.go` (6).
  All of these route through `SupervisorEventLog` and are therefore covered by this
  decision without being edited.
- **Discarded flock releases:** `_ = lock.Unlock()` — **24 sites across 14 files**, NOT
  the 2 the triggering review named. Beyond `gui_event_log.go:164` and
  `intent_audit.go:494` the same shape guards `daemon_intent.go` (5),
  `register_supervisor.go` (6), `install_parsed_manifest.go` (3),
  `lsp_trusted_roots.go` (2), `default_workspace_marker.go`, `intent_collapse.go`,
  `membership.go`, `stop_intent_subblock.go`, `supervisor_intent_mutate.go`, and
  `internal/cli/overlay_quarantine.go`.

The second inventory matters and is deliberately NOT resolved here. Those are
**state-file** locks, not log locks: a stranded `daemon-intent.json` or
`supervisor-intent.json` lock blocks reconcile and install rather than an audit
append, so the consequence, the reporting surface, and the correct remedy all differ
from the log case. Treating them as one undifferentiated sweep would be exactly the
kind of over-generalization this decision argues against — the owner is per-lock, and
these are ~20 further locks with their own semantics. Sizing them is a separate
research task; this note exists so the next reader starts from the measured number
rather than rediscovering it.

### Why that split is principled, not arbitrary

**The unit of this invariant is the LOCK, not the file, the caller, or the package.**
One lock → one owner. `supervisor-events.log`, `gui-events.log`, and `intent-audit.log`
are three distinct flocks with three distinct blast radiuses (the supervisor + install CLI;
the GUI event stream; the intent audit trail) and three distinct reporting surfaces. This PR
gives the first lock its owner and establishes the pattern. Extending the same pattern to the
other two in a PR whose admitted scope is *headless GUI recovery* would be precisely the
"while I'm here let me also" expansion the governance forbids, and each carries its own
regression surface. Follow-ups filed as adjacent findings.

## Rationale

### The recurrence is a scope mismatch, not carelessness

The invariant is **process-scoped**. Both existing doc comments already say so, in the code
that keeps failing:

- `internal/daemonrecovery/recovery.go:70-74` — "What it reports is a PROCESS-scoped
  condition: this process may still hold the flock on supervisor-events.log, blocking every
  other emitter — the supervisor, the install CLI — until it exits. The remediation is
  'restart this process', never 'retry this recovery'."
- `internal/api/supervisor_events.go:227-236` — "this process may still hold the flock on
  supervisor-events.log, so every other process that emits here ... is blocked until this
  one exits."

The reporting channel is **call-scoped**: a returned `error`. A process-scoped fact carried
on a call-scoped channel has no per-call consumer, so at essentially every site the local
author correctly concludes there is nothing to do with it and writes `_ =`. Measured on this
tree: **131 discarded `.Emit(` sites across 22 non-test files.** Exactly one of them
(`finalize()`) has a surface that reports it — and that one got the verdict wrong anyway.

So the defect is not 131 instances of an author forgetting. It is one design fact — the
invariant has 131 would-be owners, i.e. **zero owners** — reproducing itself at each new call
site. Per-caller instrumentation has now been attempted six times and the seventh occurrence
appeared *inside the sixth fix*. That is the falsifying evidence against continuing.

### Why NOT "one release-verdict-returning emit surface + thread it through"

`SupervisorEventLog.Emit` **already is** a release-verdict-returning surface:
`joinSupervisorEventReleaseErr` (`supervisor_events.go:558`) folds the release failure into
the returned error, and it is documented at `supervisor_events.go:380-382`. The surface is
not missing. What is missing is any reason for a caller to keep the value. Adding a second,
more emphatic return channel does not change that, and Go has no `#[must_use]` to force the
check — `_ =` defeats even `errcheck` unless `-blank` is enabled, which this repo does not run.
Threading it through would mean editing 131 sites and their signatures to carry a value that
131 of them cannot act on, and would leave site 132 free to drop it again.

### Why a latch is the right shape and not a workaround

The remedy for this condition is "restart this process" — a **composition-root** decision.
Architecture rule D1 (failure is a typed returned value; only the composition root
terminates) says a leaf must report the condition as a typed value and let the root decide.
Because the value cannot survive 131 fire-and-forget returns, the typed condition is instead
published to one owner the root can read. That is D1's shape plus C1 (one owner per
cross-cutting invariant) and D2 (one diagnostic port), not a symptom suppressor: the root
cause named and corrected is *the release verdict has no owner*, and after this change it has
exactly one.

### Why the owner is keyed by path rather than a single global

Keying by log path (a) states the true scope — one lock, one owner — so the other two log
families cannot be silently folded into a verdict that is not about them, and (b) gives tests
natural isolation, since each test's `t.TempDir()` is its own key. Records are pruned when
they return to clean, so the map is empty in the steady state.

### Consequence for `finalize()`: the enumeration is deleted, not extended

Keeping the per-call enumeration *and* reading the owner would be the "no logic duplication /
no fix layering" violation — re-checking one invariant at two heights because the first was
not trusted. The per-call branches are therefore removed from the verdict path. What survives
per-call is the genuinely per-call decision: whether to attempt an opportunistic
`TryReplayPending` (a release failure means do not reacquire).

## Consequences

- **F1 becomes structurally impossible.** There is no optimistic default to inherit, because
  the verdict is a read of an owner rather than a downgrade from an assumed-good start. A
  future unenumerated emit outcome cannot silently mean "confirmed released".
- **F6 and 114 further sites are covered without being edited.** Their `_ =` is now correct.
- **`AuditHandoff` widens to process scope.** A recovery can now report `release_unconfirmed`
  because an *unrelated* emitter in the same process stranded the lock. This is fail-closed
  and the operator remedy is identical ("restart this process"), but the doc comment must and
  does state it, so the field is not read as per-recovery attribution.
- **A new fail-closed false-positive window exists:** an abandoned bounded-emit worker that
  releases cleanly microseconds after `finalize()` reads the owner is reported as
  `outstanding`. Deliberate: a warning channel must fail closed, and the worker in question
  has by construction already blown its caller's deadline.
- **Test-visible behavior change.** `TestQueueIdempotentAuditFallbackOutcomeMatrix` rows that
  injected a synthetic release error and asserted `release_unconfirmed` no longer do so: an
  injected error takes no real flock, so `durable` is the truthful answer. Handoff coverage
  moves to a test that drives real emits through a failing unlock seam. This is the gate's own
  F1 observation applied consistently ("the fake handle means no real lock is held, so the row
  does not model its own scenario").
- `internal/api` gains process-global mutable state. Bounded (pruned to empty when clean),
  guarded by one mutex, with a test-reset seam — the same posture as the existing
  `supervisorEventWriteFn` / `supervisorEventUnlockFn` seams in the same file.

## Alternatives rejected

1. **Continue per-caller instrumentation (instrument `emitRecoverAuditEvent`, leave the rest).**
   Decisive rejection driver: six prior attempts, and the seventh occurrence landed inside the
   sixth fix. Also mechanically awkward — `emitRecoverAudit` is a package-level free function
   taking `stateDir`, called from 15 sites with no threaded per-operation object, so
   propagation means 15 signature changes to reach one consumer.
2. **Add a second, "louder" returning surface (e.g. `EmitChecked`) and migrate callers.**
   Rejected: the existing surface already returns the verdict; the problem is consumption, not
   production. Migration would edit 131 sites and still leave site 132 free to `_ =` it.
3. **Enable `errcheck -blank` in CI to ban `_ =` on these calls.** Rejected as the primary
   fix: it would force 131 sites to invent a consumer for a value they cannot act on, which
   converts a silent discard into 131 noisy fake handlings. Reasonable as a *later* guard once
   the value genuinely has a consumer; not a substitute for giving the invariant an owner.
   `ASSUMPTION (UNVERIFIED)`: that this repo runs no `errcheck` with `-blank`; resolved by
   grepping CI config and lint config for `errcheck` — not performed, because the decision does
   not turn on it (the argument stands either way).
4. **Self-terminate the process on a stranded flock.** Rejected: D1 reserves termination for
   the composition root; a library leaf killing the GUI because an audit row's unlock failed is
   a far larger blast radius than the condition warrants.
5. **Extend the owner to `gui-events.log` and `intent-audit.log` in this PR.** Rejected on
   scope: three locks, three blast radiuses, and this PR's admitted scope is GUI recovery.
   Filed as adjacent findings instead.
