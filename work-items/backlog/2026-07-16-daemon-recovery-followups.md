# daemon recovery — deferred follow-ups (from the item-2 review panel)

Filed: 2026-07-16
Source: the Sol (architecture/adversarial) + Terra (narrow audit) + fable (arbiter) panel on
Productization Phase-0 item 2 (`feat/gui-daemon-recovery`). The arbiter PASSED the design and the
concurrency story; 9 defects were fixed in the item itself. The first four were explicitly scoped OUT
with the arbiter's agreement — none is a correctness hole.

## 1. A third copy of the task-name canonicalization predicate

`internal/daemonrecovery.CanonicalTaskName` now sits beside `daemon_env_overlay.NormalizeOverlayKey` and
`cli.canonicalSupervisorTaskName` — three implementations of the same backslash-canonicalization rule.
The 12-second identity-lookup deadline constant is likewise duplicated.

**Why deferred:** all three agree today and the item's own tests pin the behavior; consolidating touches
three packages and their callers, which is its own change. **Why it must not linger:** three copies of a
predicate that decides WHICH TASK a destructive operation targets is exactly the no-logic-duplication
rule this repo enforces — a future edit to one copy is a silent divergence on a kill path.

**Right shape:** one owner (likely the `daemon_env_overlay` normalizer, since it already spans surfaces),
the other two delegating; one shared constant for the lookup deadline.

## 2. No per-task recovery lease

Nothing serializes two concurrent recoveries of the same task (two browser tabs, or CLI + GUI).

**Arbiter's ruling: acceptable.** The identity gate constrains the only possible victim to a process
running our binary with our exact argv naming the recovered task. The bound is therefore never a
stranger, and no loss beyond what hard-restarting the target daemon itself entails: in-flight requests
and writes of that daemon can be severed. The held generation pins that process across
classify→confirm→boundary-recheck→kill, while
the destructive-boundary `supervisor-state.json` re-read reduces the stale tracked-child verdict from
the operator-confirmation interval to the controller event loop's measured approximately 1.5-second
persistence lag. It does not assume that `CurrentPID` is persisted before the child binds.

**Right shape (follow-up):** a per-task lease around Execute, same pattern as the adopt/de-adopt
per-manifest flock.

## 3. The audit cannot distinguish a GUI click from a CLI recover

Both emit `source: "recover"` with `actor` = the OS user, so an incident review cannot tell which surface
authorized a reap.

**Right shape:** a body/option discriminator threaded from each adapter into the audit payload
(`surface: "gui" | "cli"`). Cheap, but it changes the audit schema — its own change.

## 4. The `daemon-recovery-*` CSS classes are unstyled

The Recover affordance renders with the existing card vocabulary; the dedicated classes carry no rules
yet. Cosmetic; no behavior impact.

## 5. Automatic recovery cannot audit committed-but-unconfirmed termination distinctly

The automatic recovery paths at `internal/cli/supervise_squatter.go:417` and
`internal/cli/supervise_squatter.go:546` fold a termination that committed but whose exit wait was
unconfirmed into `daemon-port-squatter-reap-failed` ("the port is still held"). The cause is that
`process.TerminatePIDWithIdentity` discards the shared held-generation primitive's `committed` flag at
`internal/process/pid_identity_windows.go:167` before returning to those callers.

The current direction is fail-safe: it never claims an unconfirmed reap, and the supervisor loop
self-heals on the next tick. The right shape is to surface committed-ness from the shared primitive and
emit the distinct unconfirmed event on the automatic paths too.

**Why deferred:** changing the shared primitive's signature affects three other call sites, verified via
the language server rather than assumed — `internal/api/cleanup.go:1684` (`orphanTerminateFn`),
`internal/cli/supervise.go:195` (`productionTerminatePIDWithIdentityFn`), and
`internal/cli/supervise_squatter.go:70` (`squatterTerminatePIDFn`, which is the automatic sweep path this
follow-up exists to fix). It needs its own reviewed change rather than widening GUI recovery round 8.

## 6. A future wrapper-based Windows port probe must set `Cmd.WaitDelay`

`waitForPortFree` is bounded today only because the Windows port probe uses
`exec.CommandContext(ctx, "netstat", "-ano")` directly
(`internal/api/serena_port_owner_windows.go`) and that command spawns no grandchild inheriting its
stdout pipe. No `Cmd.WaitDelay` is set anywhere on this path. If the probe is ever replaced by a
wrapper that forks, `cmd.Output()` can stop returning after cancellation while the inherited pipe
remains open; the post-commit window would silently stop being bounded and create the next
kill-without-restart gap at exactly that point.

**Right shape:** set `Cmd.WaitDelay` on the probe command whenever this path is changed or wrapped.

## 7. The production-default respawn-reservation refusal surface is unreachable

`FailureRespawnBudgetInsufficient` (CLI exit 6, HTTP 503
`RECOVER_RESPAWN_BUDGET_INSUFFICIENT`, TypeScript union member, and Dashboard message) is correct
defence-in-depth for a custom `Dependencies` caller. At production defaults, however, the supervisor
probe is self-capped at 5 seconds, so at least 25 seconds of the 30-second post-kill budget remains —
always more than the 20-second respawn reserve. No default-configured operator can reach this failure
surface today; retain that reachability fact when maintaining its exit code, wire code, union entry,
and UI copy.
