---
title: A retained Phase-I GUI flock degrades the classifier but does not suppress the same tick's GUI relaunch
severity: low
found-by: backend-engineer
found-in-phase: review finding 1 implementation
affected-surface: internal/cli/supervise_ensure_alive.go
context: adjacent-finding
status: open
related-work-item: 2026-07-25-liveness-headless-gui-recovery
---

## Defect

Review finding 1 is fixed at the named caller: `runEnsureAliveGUIRecoveryFree`
now observes its owned-probe lease release via `ReleaseErr` and, when the
release is NOT confirmed, emits the unknown/degraded diagnostic instead of
advice that invites a GUI relaunch (`ensureAliveGUIFreeMessage` /
`ensureAliveGUIOwnerRecoveringMessage`).

That closes the classifier's own fail-open, but it does not reach the
supervisor-liveness body that runs immediately afterwards in the SAME one-shot
process. `runEnsureAliveGUIRecovery` returns void, so `runEnsureAlive`
(`internal/cli/supervise_ensure_alive.go:1259+`) cannot know a lease was
retained, and a `guiOwnerStateConfirmedDead` classification can still authorize
`livenessRelaunchFn()` on this tick — via the supervisor-down owner-death branch
or via `runEnsureAliveHeadlessFleet`. The relaunched `mcphub gui` then fails to
acquire the single-instance flock this process still holds and dies; the fleet
stays headless until the next tick.

## Evidence

- `internal/gui/single_instance.go` `release()` documents the residual
  explicitly: on a PERSISTENT `Unlock` failure gofrs/flock's `Close()` delegates
  to `Unlock()` and fails identically, so the OS lock is held until process
  exit.
- Empirically verified this session on Windows 11 (Go probe, gofrs/flock at the
  repo's pinned version): a second `flock.New(path).TryLock()` against a path
  already locked by the SAME process returns `ok=false, err=<nil>`. A retained
  lease therefore genuinely locks out a relaunched GUI.
- The same probe result means the `guiOwnerStateUnknown` escalation path is
  ALREADY fail-closed by accident: `probeGUIOwnerLockUnheld`
  (`internal/cli/supervise_ensure_alive.go:252-258`) tries to acquire that same
  flock, gets `ErrSingleInstanceBusy`, and reports `unheld=false`. Only the
  `guiOwnerStateConfirmedDead` path — which is pidport-CONTENT based
  (`probeGUIOwnerAlive`) and never touches the flock — bypasses that backstop.

## Why it was not fixed here

The prescribed fix was scoped to `runEnsureAliveGUIRecoveryFree`. Suppressing
the relaunch requires threading a "lease retained" signal out of the
budget-bounded classifier goroutine and gating `runEnsureAlive`'s recovery
decision on it — a behavior change to the primary liveness recovery path, which
is the highest-stakes function on this branch and was not part of the approved
change surface. The classifier's timeout arm (`case <-classifierCtx.Done()`)
also abandons its goroutine without an outcome, so a fail-closed version has to
decide what an UNKNOWN release outcome should do there; that is a design call,
not an implementer call.

## Suggested direction (for the architect, not applied)

The semantically exact form is a downgrade rather than a new branch: when a
lease may still be held, treat the GUI owner as `guiOwnerStateUnknown` at both
`guiOwnerAliveFn()` classification points. `Unknown` already has fully defined,
tested fail-closed behavior at both (suppress the headless relaunch; recover the
supervisor via the GUI-independent standalone spawn, which does not need the GUI
flock), so no new control flow is introduced. Note the escalation path writes a
durable Unknown-confirmation marker, so the interaction with
`resetGUIOwnerUnknownConfirmationMarkerLogged` needs checking.

## Bound on impact

Requires a persistent `UnlockFileEx` failure on a lock this process legitimately
holds — near-impossible per the existing residual note — AND an expired
nonterminal handoff marker AND a confirmed-dead GUI owner in the same tick. The
cost when it does occur is one lost recovery tick (~1 min), self-clearing on the
next tick because process exit frees the lock.
