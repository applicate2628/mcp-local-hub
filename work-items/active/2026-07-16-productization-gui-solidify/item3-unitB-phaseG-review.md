# Item-3 Unit B Phase G — parent restart coordinator — review record

Phase G (parent `RestartCoordinator` + 202/2xx endpoint + cli-layer lease/spawn/argv/exit wiring +
server-side hub barrier), default-OFF behind `RestartV3Enabled()`. Branch `feat/gui-restart-unitb-gated`.
codex Sol implemented; three commission rounds converged.

## Implementation
codex Sol drafted the full phase (RestartCoordinator: Begin → spawn retained-handle child with owner-only
nonce → confirm authenticated standby → GRACE(P)/same-port-close → Reserve under flock → close parent hub →
release flock → post-release no-op; pre-release rollback with proved-clear vs terminal-interrupt) + the
202/2xx endpoint + the AC-G1..G8 tests. $lead authorized adding `internal/cli/gui.go` to the surface (plan
Phase E already deferred the acquire-caller wiring to "F/G/I"; plan amended `beadf474`). All 8 AC tests +
the invariants ($lead-verified: rollback-gate = `parentLeaseReleased` concrete fact, hub-close-before-release,
post-release no-op, lease release-once, self-restart exit skips `manager.Stop`, spawn-failure 2xx) held.

## Commission convergence (3 rounds)
- **R1 (Sol deep):** REVISE, 3 P1 + 4 P2 — hub-producer-race, single-shot-confirm false-rollback,
  same-port-rollback listener-state; 202-flush, nonce-generation-binding, concurrent-restart-2xx,
  post-Begin-cleanup-error. codex fixed all 7 (`fix`).
- **R2 (full: Terra + fable, fable ran -race + a Windows errno probe):** REVISE. **Decisive fable P1:**
  the same-port bind-retry was INERT on Windows — `errors.Is(err, syscall.EADDRINUSE)` is FALSE for a real
  WSAEADDRINUSE (10048; `syscall.EADDRINUSE` on Windows = 536870914), masked by a `syscall.EADDRINUSE`-
  injecting test. Fix: `api.IsPortBindRefusedErr` + a `windows.WSAEADDRINUSE`-injecting test (memory
  `feedback_windows_eaddrinuse_wrong_predicate`). Plus Terra physical-close-vs-Serve-wait-timeout,
  unbounded producers.Wait, cleanup-wedge-when-marker-cleared, unauthenticated-child-not-terminated,
  .lock/nonce residue, conditional-flush-ack. codex fixed all 7 (`fix2`); codex ran the full `-race` suite
  (gui + cli ok) + `-race -count=2 TestRestartV3_` (ok).
- **R3 (convergence: Terra + fable):** **fable VERDICT PASS** — all 7 R2 fixes correct, no defect above P3;
  fable re-ran build/vet/gofmt + `-race`. Two non-blocking P3s (deferred, see backlog): the post-Reserve
  rollback arm attempts an unreachable proved-clear then escalates to terminal Interrupt (benign, process
  was exiting); child same-port bind budget (2s) < parent quiesce ceiling (5s) — a slow drain starves the
  child bind → safe proved rollback. **Terra REVISE (1 P1: the pre-Begin nonce/.lock sweep is unsafe) —
  RESOLVED as a FALSE POSITIVE ($lead + fable):** the sweep runs inside `Start()` AFTER the mutex-serialized
  `c.run=true` guard and BEFORE this generation's `WriteNonce`, while the parent holds the single-instance
  flock (only one GUI serves /gui/restart), and new generations use new hashed leaves so nothing re-locks a
  swept path; a live-handle sharing violation just fails the sweep → `resetBeforeSpawn` → clean retryable
  error. Terra did not account for the single-instance-flock serialization + generation-bound leaf naming
  (same over-cautious-reviewer pattern as bot #562-R5). No fix.

## Verdict
Phase G is commit-safe (gated, default-OFF): fable PASS, the one Terra P1 refuted 2-way, 2 non-blocking P3s
deferred. Authoritative $lead full tagged `./internal/gui/ ./internal/cli/` run green (not trusting the
subagent -race runs). Deferred P3s: `backlog/2026-07-18-phaseg-coordinator-followups.md`.
