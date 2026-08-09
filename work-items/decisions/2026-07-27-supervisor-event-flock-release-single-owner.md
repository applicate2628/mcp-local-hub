---
id: 2026-07-27-supervisor-event-flock-release-single-owner
status: proposed
decided-by: $architect (PR #589 D1/D2 architecture revision; independent gate pending)
context: one process-scoped release owner plus downstream settlement and authoritative repair for PR `feat/liveness-headless-gui-recovery`
supersedes: none
superseded-by: none
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
   `outstanding` is a transient physical state while a bounded writer still owns the
   release attempt and means **wait for settlement**. `stranded` is the permanent
   same-process state after a confirmed release failure and alone carries the remedy
   **restart this process, never retry recovery**.
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
5. The same owner exposes an atomic `{state, revision}` snapshot and transition
   subscription. The graphical user interface (GUI) adapter coalesces one subscription only
   while a recovery has observed `release_pending`, publishes its first later
   `Released|Stranded` settlement over the existing Server-Sent Events (SSE) bus, and
   unsubscribes. A read-only GET reads the owner directly and repairs event loss. SSE is a
   hint; the snapshot is authority. No GUI component writes or re-derives physical state.
6. The Dashboard owns the only observation-order reducer. POST, SSE, and GET observations
   are linearized by one client authority sequence; an accepted newer EventSource transport
   generation outranks all revisions from an older generation because backend revisions are
   process-local. No timer, second EventSource, second state owner, recovery retry, or
   destructive refresh is introduced.
7. The graphical user interface (GUI) audit-lock adapter owns a bounded exact-attempt receipt
   registry beside, but distinct from, the physical owner. The current Dashboard obtains one
   read-only baseline, sends an opaque attempt id plus the baseline's server-instance token,
   and the adapter reserves that id before `Recover` may run. Every admitted handler path
   terminalizes exactly that receipt as committed success, committed error, not committed, or
   uncertain before response
   encoding. The Dashboard reducer alone turns an exact committed non-clean receipt into one
   permission to project independently accepted current physical truth. A different request,
   process epoch, SSE event, or timing observation can never authorize it.
8. Receipt memory is bounded without silent eviction. At most 64 unresolved receipts are
   admitted; a full registry, duplicate id, or stale server instance is rejected before
   recovery mutation. Terminal receipts remain until an idempotent browser acknowledgement or
   process teardown. No timer, background cleanup, retrying recovery POST, second EventSource,
   or second physical-state owner is introduced.

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

- **Discarded emit verdicts:** `_ = <x>.Emit(` — **131 sites across 22 files**. Top
  contributors: `internal/cli/supervise.go` (62), `internal/cli/supervisor_controller.go`
  (21), `internal/cli/supervise_respawn.go` (6), `internal/cli/supervise_realloc.go` (6).
  All of these route through `SupervisorEventLog` and are therefore covered by this
  decision without being edited.

  The raw grep returns **132 across 23** and an earlier revision of this section quoted
  that number. One of those matches is not code: `internal/api/supervisor_event_lock_health.go:12`
  is the owner's own doc comment, which quotes the literal string `` `_ = logger.Emit(...)` ``
  while describing the class. The grep counts the file that describes the grep. The
  code count is 131 across 22, which is what the Rationale below and the owner's doc
  comment both state.
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

The remedy for a confirmed `Stranded` condition is "restart this process" — a
**composition-root** decision. `Outstanding` is transient; its correct action is to await
owner settlement.
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
- **`Outstanding` is a transient wait state.** A bounded writer can occupy it during healthy
  operation. If a recovery response samples it, the Dashboard may
  show pending only until the same owner reports `Released` or `Stranded` through settlement
  SSE or authoritative GET. Only `Stranded` carries restart guidance.
- **Test-visible behavior change.** `TestQueueIdempotentAuditFallbackOutcomeMatrix` rows that
  injected a synthetic release error and asserted `release_unconfirmed` no longer do so: an
  injected error takes no real flock, so `durable` is the truthful answer. Handoff coverage
  moves to a test that drives real emits through a failing unlock seam. This is the gate's own
  F1 observation applied consistently ("the fake handle means no real lock is held, so the row
  does not model its own scenario").
- `internal/api` gains process-global mutable state. Bounded (pruned to empty when clean),
  guarded by one mutex, with a test-reset seam — the same posture as the existing
  `supervisorEventWriteFn` / `supervisorEventUnlockFn` seams in the same file.

## Downstream observation and settlement contract

| Boundary | Single owner | Current contract |
| --- | --- | --- |
| Physical state and revision | `internal/api/supervisor_event_lock_health.go` under its mutex | Atomic `Released\|Outstanding\|Stranded` snapshot; revision changes only with effective state. |
| Transition observation | The same API owner | Atomic subscribe plus initial snapshot; callbacks occur after owner mutation and outside the mutex; unsubscribe is idempotent. |
| Process-to-GUI settlement | One coalesced GUI adapter | Armed only by tracked `release_pending`; publishes first later `Released\|Stranded`, then unsubscribes; ordinary bounded activity is silent. |
| Exact recovery witness | One bounded registry in the GUI audit-lock adapter | Opaque attempt id is reserved before `Recover`, terminalized before every response encode, never cross-authorizes or silently evicts, and is removed only by idempotent acknowledgement/process teardown. |
| Loss/reconnect repair | Read-only GET of the physical owner plus optional exact receipt | Authoritative current snapshot/receipt on EventSource open, visibility, and the existing 60-second cadence while a warning or unresolved attempt is active. |
| Dashboard physical ordering | One audit-lock observation reducer | Every accepted physical POST, SSE, or GET increments one authority sequence. A GET or SSE accepted in a newer transport generation invalidates the physical snapshot of delayed older-generation responses even when its backend revision is lower. |
|  |  | Backend revisions compare only inside one generation. |
| Recovery-warning authorization | The same Dashboard reducer | Only a complete matching response or exact same-instance receipt with `lock_authorization=current_truth` grants one permission independently of physical admission. |
|  |  | Current accepted owner truth consumes it once; `none/not_committed` stays silent and `uncertain` produces only a do-not-rerun ambiguity alert. |

The exact delayed-response falsifier is mandatory: hold generation-1 POST A; accept a
generation-2 GET returning lower-revision `released`; then resolve A with higher-revision
`outstanding`. A's physical snapshot must not apply; its authorization is consumed by current
`released`, the warning must remain clear without flicker, and no recovery retry may occur.
The inverse arrival order is also required: if A applies while the generation-2 GET is
in flight, that newer-generation GET must still replace A when it resolves. Same-generation
higher-revision settlement and ordinary B-before-A response order must continue to apply.

The lost-authorization falsifiers are equally mandatory. First, hold generation-1 A, accept a
generation-2 GET returning current `outstanding`, then resolve A and transition the owner to
`Stranded`: A's physical snapshot never applies, but its authorization reconciles cached
current truth and the later stranded settlement becomes visible. Repeat with generation-2
`released` and require a continuously clear warning. Second, in one generation apply later
B=`not_required` with current `outstanding` before held A=`release_pending`; B remains the
latest applied POST, A grants only authorization, and later `Stranded` becomes visible. A clean
settlement produces no stale-pending flicker. Completely unarmed healthy
`Released -> Outstanding -> Released` remains silent: zero banner, zero autonomous GET, and
zero GUI settlement projection.

The lost-response and exact-attribution falsifiers are mandatory too. Admit A, commit its
non-clean recovery, drop both its response body and every settlement event, and require exact
receipt GET to expose current `Outstanding -> Stranded` without applying A's stale snapshot or
issuing another recovery; repeat with current `Released` and require continuous silence. Then
preflight A and B from the same physical revision, fail A before commit, commit B, drop A's
error body, and query A: A must remain `not_committed` and cannot borrow B's commit. Filling the
64 receipt slots must reject attempt 65 before `Recover`; no receipt may be silently evicted.

## Decision status and co-variation gate

This file is the only decision-registry owner for the process-lock and downstream settlement
pipeline. The PR #589 settlement design is its bounded realization; the two canonical files
must be reviewed as one package because changing only one recreates contradictory live truth.
This decision remains `status: proposed`. A fresh independent architecture `PASS` over both
exact hashes authorizes `$knowledge-archivist` to perform only the mechanical promotion to
`accepted` and synchronize the decision index; implementation does not self-promote it.

## Owner claims and falsifying probes

1. `{ guarantee: Outstanding is transient and means wait for owner settlement; single-owner: supervisor-event lock-health state contract; enforcement-probe: healthy bounded-write transition test plus scoped live-tree current-semantics search }`.
2. `{ guarantee: Stranded is permanent for the process and carries the process-restart remedy; single-owner: supervisor-event lock-health state contract; enforcement-probe: release-failure precedence test plus scoped live-tree current-semantics search }`.
3. `{ guarantee: physical state has one writer and GUI settlement is an observation rather than a second state owner; single-owner: supervisor-event lock-health owner; enforcement-probe: owner subscription race tests and absence of a GUI setter }`.
4. `{ guarantee: every armed pending condition settles through SSE or authoritative GET without another recovery; single-owner: coalesced GUI settlement adapter; enforcement-probe: release-without-second-POST and dropped-event GET-repair tests }`.
5. `{ guarantee: a delayed, lost, or out-of-order recovery response cannot overwrite current physical truth or lose/cross-attribute its exact one-shot permission; single-owner: Dashboard reducer consuming the GUI audit-lock exact-receipt contract; enforcement-probe: deterministic old-generation outcomes, same-generation B-before-A, lost-body released/outstanding/stranded, A-fails-while-B-commits, clean no-flicker, and unarmed healthy silence tests }`.
6. `{ guarantee: unresolved receipt evidence is bounded and never silently discarded; single-owner: GUI audit-lock receipt registry; enforcement-probe: 64-slot admission, duplicate-id, idempotent-ack, abandoned-client, and process-teardown tests }`.

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
6. **Use one process-global recovery epoch instead of request receipts.** Rejected because two
   clients may share baseline epoch E: B can commit and advance the scalar while A fails, then
   A's later GET falsely attributes B's commit to A. Serialization alone does not close the
   window where B commits after A terminates but before A reads.
7. **Silently evict old terminal receipts.** Rejected because response loss makes the receipt
   the only commit witness. Eviction would recreate the hidden-commit defect; fail-closed
   admission plus explicit idempotent acknowledgement is the bounded alternative.

## Terms and Abbreviations

- **GET/POST** — read-only and mutating Hypertext Transfer Protocol request methods.
- **GUI** — graphical user interface.
- **PR** — pull request.
- **SSE** — Server-Sent Events, the existing browser event stream.
