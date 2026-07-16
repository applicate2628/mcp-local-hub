# daemon recovery — deferred follow-ups (from the item-2 review panel)

Filed: 2026-07-16
Source: the Sol (architecture/adversarial) + Terra (narrow audit) + fable (arbiter) panel on
Productization Phase-0 item 2 (`feat/gui-daemon-recovery`). The arbiter PASSED the design and the
concurrency story; 9 defects were fixed in the item itself. These four were explicitly scoped OUT with
the arbiter's agreement — none is a correctness hole.

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

**Arbiter's ruling: acceptable.** Traced in code: the port probe runs BEFORE the `supervisor-state.json`
read, so a just-respawned child is caught by the tracked-child exclusion (the supervisor persists
CurrentPID at spawn, before bind); the held generation pins the PID across
classify→confirm→boundary-recheck→kill; a concurrent sweep kill of the same dead PID is idempotent. The
worst outcome of a race is **one supervisor-owned extra restart of a fresh child — never an unverified
kill**.

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
