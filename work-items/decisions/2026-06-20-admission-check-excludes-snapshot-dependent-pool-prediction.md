---
status: proposed
date: 2026-06-20
slug: admission-check-excludes-snapshot-dependent-pool-prediction
---

# AdmissionCheck holds only scope-independent STRUCTURAL checks; it never re-predicts a lock-held owner's runtime outcome

## Context

PR #382 unified the install-admission decision into one `AdmissionCheck` owner (ADR
`2026-06-19-admission-check-single-gate`). In r2 the client-binding validation was REMOVED
from AdmissionCheck because it is caller/scope-dependent and the scoped planner owns it.
The dynamic-pool FREE-PORT / foreign-bound prediction then repeated the exact same defect:
it tries to PREDICT whether `AllocatePort` (the real per-workspace, registry-lock-held pool
allocator at registration time, `internal/api/port_alloc.go:40`) will succeed — from a
snapshot-free gate that does not carry the operation's inputs. Over r3→r6 the Codex bot
found a new edge every round, all the same class:

- the gate reloads the WHOLE default registry while the install materializes only its
  filtered `opts.Workspaces` snapshot;
- ownership identification was wrong across daemon-name shapes (serena `serena-<key>` vs
  LSP `lsp-<key>-<lang>` vs future backends — an open-ended set);
- the gate elevated a foreign-bound UNALLOCATED spare to blocking, while `AllocatePort`
  simply skips it and only fails on true exhaustion.

Threading the snapshot in (Option C) does not converge — it moves the divergence to the
next un-modeled input. This is the anti-pattern in the lesson
`feedback_readiness_mirror_gate_via_dryrun_not_reimpl`: a predictor that re-derives a real
gate's checks drifts; call the gate, don't re-implement it.

## Decision

`AdmissionCheck` (and its readiness mirror) contains ONLY scope-INDEPENDENT, STRUCTURAL
pool checks — native-http upstream-port overflow (`port+offset > 65535`, a pure function of
the manifest pool bounds) and registry read/resolve failure (register cannot allocate at
all if the registry is unreadable). The runtime free-port / foreign-bound prediction is
REMOVED from both gates and its dead helpers deleted; pool-capacity exhaustion is at most an
ADVISORY readiness note (`Optional`), never a Preflight reject or `Ready=false`. The real
per-workspace port-binding outcome stays owned by `AllocatePort` at registration, which has
the lock, the registry, and the OS probe — a full pool surfaces precisely as
`ErrPortPoolExhausted` at the NEW-workspace registration that actually allocates, not the
server install/reinstall.

General rule (extends the r2 client-scope removal): a unified admission/readiness gate
predicts only what it can decide from scope-independent inputs it actually holds. Anything
whose outcome depends on a caller scope, a filtered snapshot, live OS/registry state, or a
lock-held write path belongs to that real owner — the gate must not re-derive it.

## Consequences

- The pool divergence class the bot mined for 4 rounds is eliminated by construction (both
  gates now use the same two structural predicates; they cannot disagree).
- ~6 helper funcs + a struct are deleted (the predictor surface), net simplification.
- The genuinely structural blockers (overflow, registry-read-failure) stay.
- A wrongly-blocked install now proceeds; if a NEW workspace truly cannot get a port, the
  existing `ErrPortPoolExhausted` at registration names the real operation and moment.

Referenced from PR #382. Promotion to `accepted` is the architecture-reviewer's call.
