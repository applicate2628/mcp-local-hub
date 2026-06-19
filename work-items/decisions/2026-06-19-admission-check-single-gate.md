---
status: accepted
date: 2026-06-19
slug: admission-check-single-gate
---

# Unify the install-admission decision into one `AdmissionCheck` owner

## Context

PR #377 ("install-and-it-works" readiness) added `CheckServerReadiness`
(`internal/api/readiness.go`) — an all-requirements, GUI-renderable report that
predicts whether installing/spawning an MCP-server daemon will succeed — alongside
the existing fail-fast `Preflight` (`internal/api/install.go`).

The PR went through ~7 Codex-bot review rounds (r12–r18), each surfacing 1–6
P1/P2 edge cases. An `$architect` review (2026-06-19, read-only) identified the
**root cause**: the authoritative "will this install be admitted" decision has
**no single owner**. It is an implicit SEQUENCE —

- global/legacy path: `Preflight(m, filter)` → `BuildPlanWithOpts(...)` (`install.go` `(*API).Install`);
- workspace/serena path: `validateDynamicPoolManifest(m)` → `Preflight(m, "")` → plan/admission (`install_parsed_manifest.go` `(*API).InstallParsedManifest`);

— re-spelled at each install site, and `CheckServerReadiness` is a **fourth**
approximation of that same sequence (it calls the shared predicates + `BuildPlanWithOpts`
dry-run + `validateDynamicPoolManifest`, but NOT `Preflight`). Four spellings of
one decision is the divergence engine the bot keeps mining.

Extracting shared leaf predicates (`binaryAvailable`, `manifestNeedsGit`,
`entryScriptCheckTargets`/`entryScriptStatus`, `fixedPortStatus`, `normalizeLauncher`,
`runtimeBehindLauncher`) — done in #377 — **narrows** the class but cannot **end**
it: the two callers can still call different *subsets* of the predicates, which is
exactly what r12–r18 found.

Straight delegation (readiness → `Preflight(m,"")`) is WRONG: `Preflight` is only
*half* the gate — it omits the `BuildPlanWithOpts` binding/url/remote-secret check,
the dynamic-pool PortPool readiness, and the operator client-scope
(`DefaultInstallClientNamesEffectiveIn`) that readiness validates. Delegating would
re-open divergence in the opposite direction.

## Decision

Extract the full admission sequence into ONE pure, side-effect-free function:

```go
type AdmissionFinding struct{ ID, Name, Reason, Fix string; Optional bool }
func AdmissionCheck(m *config.ServerManifest, scope AdmissionScope) []AdmissionFinding
```

Body = the union of every check currently in `Preflight` + the `BuildPlanWithOpts`
dry-run + `validateDynamicPoolManifest` + the pool-port check — each appending a
finding instead of returning early.

- `Preflight(m, filter) error` becomes a thin fail-fast adapter:
  `for _, f := range AdmissionCheck(m, scope{filter}) { if !f.Optional { return errors.New(f.Reason) } }; return nil`
  — identical observable first-error behavior, existing Preflight tests unchanged.
- `CheckServerReadiness`'s `Ready` flag becomes `AdmissionCheck(...)` having zero
  non-optional findings. The rich per-key `ReadinessRequirement` rows stay as the
  advisory GUI rendering layered on top (inline secret-prompt UX preserved).

Enforce with a corpus test: `Preflight(m,"") == nil  ⟺  CheckServerReadiness(m).Ready`
over a manifest corpus. This makes the two gates non-divergent **by construction**;
shared predicates alone cannot guarantee it.

## In-scope criterion (the convergence boundary)

A prerequisite belongs in the gate iff ALL of: (a) **pre-spawn** (evaluable without
running the daemon), (b) **state-committing** (failure = a false install that commits
client+supervisor state for a guaranteed-dead daemon, not a runtime hiccup),
(c) **mcphub-fixable** (an actionable Fix exists). Anything downstream of `cmd.Start`
(git+ remote down, npm registry outage, wheel build failure, the server's own
auth/runtime) is OUT OF SCOPE — the daemon reports its own error. This criterion is
now documented in the `CheckServerReadiness` package doc so review rounds adjudicate
against a stated rule.

Known-tolerated non-convergences (advisory, not bugs): relative `base_args[0]` with a
non-absolute daemon cwd (launch cwd unknowable); the readiness bind-probe vs Preflight
dial+intent port check (Preflight's is the richer authoritative superset).

## Status / sequencing

ACCEPTED as the structural end-state. NOT implemented in #377 — bolting a refactor of
the critical install gate (incl. the supervisor-intent port collision logic) onto a
converging PR at r18 risks the gate and re-opens its whole review. To be a dedicated
follow-up PR after #377 merges, carrying the corpus test. #377 ships the bounded
shared-predicate form, which the architect confirms is correct as the leaf layer.

## Consequences

- One owner of "install admitted"; the bot's divergence-finding surface collapses.
- Slightly more indirection (the adapter layer) in exchange for a CI-enforceable
  equivalence invariant.
- The port-probe fork (`fixedPortStatus` bind vs `preflightPortInUse` dial) is collapsed
  to one canonical check (bind-probe + `portHeldByOurDaemonForPortArm`) inside
  `AdmissionCheck` during this follow-up; the Preflight collision tests that mock the dial
  probe are migrated to the bind probe at that time.
