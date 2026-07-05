# Closure — unify daemon port-resolution into a single owner

Closed: 2026-07-05

## Outcome

DELIVERED. The daemon port + identity resolution is now single-owned in
`internal/api/supervisor_port_owner.go`; the F5 startup backfill
(`BackfillIntentDaemonPorts`) and the dead `ResolveManifestDaemonPort` are
deleted. Merged as PR #505 (squash `e306cbd7` on master), deployed to the live
host via staged cross-volume `install --upgrade`, and live-verified: canonical
binary `e306cbd7`, supervisor restarted, 20 daemons Running + job_protection,
hub `√ Connected`, and all 22 MCP servers (hub, every LSP, serena, mui,
codegraph, …) green via `claude mcp list`.

## What shipped

The owner is the sole authority every port-DECISION path resolves through
(liveness sweep, P1b deadline, squatter classifier, `mcphub daemon recover`,
startup running-scan, status display, serena-unregister force-kill), so a Port=0
legacy descriptor no longer structurally disables port protections — the manifest
port resolves lazily instead.

Core symbols:
- `EffectiveDaemonPort` / `EffectiveStartupBindDeadlineSeconds` / `DaemonPortResolver` (memoized).
- `DescriptorServerDaemon` (pair identity, fail-closed on field/argv mismatch) + `DescriptorServerName` (server-only, permissive).
- `IsSerenaProxyDescriptor` / `IsWorkspaceLSPProxyDescriptor` / `DescriptorHasGlobalDaemonArgv` — the proxy-aware argv shape discriminators (single-owned; cli re-exports them).
- `ResolveDescriptorMatchIdentity` → `(server, daemon, IdentitySource)` — the 3-way classifier (FromArgvOrField / CorruptGlobalArgv / TaskNameSafe) every PAIR consumer switches on; fail-closed `default`.

The unifying principle the review process converged on: **scope picks the owner**
— PAIR-scoped match/select/reap decisions resolve strictly through
`ResolveDescriptorMatchIdentity` (a field that contradicts the launch argv → fail
closed); SERVER-scoped decisions resolve permissively through
`DescriptorServerName` (a partial `--server X` still resolves; only a
contradicting `--server` fails closed). No consumer reads a raw `d.Server` /
`d.Daemon` field for a decision on a row that could carry a `daemon` argv.

## Residual risk

- **Deferred (tracked follow-up):** `internal/cli/supervise_status.go` PRIMARY
  display derivation still trusts a populated `Server` field; it feeds status
  display labels + `secrets.go` post-rotation restart bucketing. A lying-cache row
  (field ≠ argv — corrupt/hand-edit only, never mcphub-written) would bucket under
  the stale server, causing a SKIPPED restart of the truly-affected daemon after a
  secret rotation — the FAIL-SAFE direction (no wrong-target kill). Flagged by
  sonnet at lower severity; codex + fable did not flag it. Fix = route the primary
  derivation through `DescriptorServerName` like the sibling `effDesc` synthesis
  already does. Left out of this PR to avoid a late display-derivation change; the
  effDesc PORT path in the same function IS fixed.
- The refactor is fully unit-tested (owner predicate + resolver + all consumer
  fail-closed/neutral cases), commission-verified (fable+sonnet+codex ×3 rounds),
  and bot-passed; the live host had no Port=0 DAEMON rows (only the
  `workspace-weekly-refresh` timer, correctly portless), so the Port=0-daemon sweep
  path is unit-verified rather than live-exercised.

## Retrospective — the 7-round edge-mine

This landed after **7 Codex bot rounds + 3 full commissions + 2 architect
reconsults**. The shape is worth recording (see `docs/lessons.md` /
`work-items/lessons/`):

- **Root class:** deleting F5 removed a WRITE (persisted ports + healed blank
  identity) that MANY readers silently relied on. Each bot round surfaced another
  consumer that keyed on a persisted field / task-name instead of the argv owner.
- **The turning point** was recognising (bot r5→r6, per the mimocode edge-mine
  heuristic: 3+ rounds of the same consistency-edge class = wrong abstraction) that
  the fix was NOT another per-consumer patch but a **single owner predicate** the
  resolve side and the protect side both call. Reconsulting the architect twice
  produced the `DescriptorHasGlobalDaemonArgv` + `ResolveDescriptorMatchIdentity`
  chokepoints and the "drop the blank-field short-circuit" insight (common-path
  neutral — the owners already fail closed; consumers just bypassed them).
- **The commission earned its keep:** the r6b re-commission caught a P1 regression
  the amend itself introduced (status effDesc rebuilt without the fields) BEFORE it
  reached the bot, and all three models converged on the same remaining consumer
  set — a strong convergence signal that the class was finally closed.
- **Lesson:** when a deletion removes a cross-cutting write, enumerate the readers
  up front (grep the field/task-name consumers) and route them ALL through one
  owner in the first pass, rather than discovering them one bot-round at a time.

## Artifacts

- Design: `design.md` (§4b deadline model, F4 serena-quarantine note).
- Plan: `plan.md` (P1–P4 phasing).
- Decision: `work-items/decisions/2026-07-05-daemon-port-resolution-single-owner.md` (accepted).
- Related bug (resolved): `work-items/bugs/2026-07-05-stop-force-supervisor-port-zero-gap.md`.
- PR #505 → squash `e306cbd7`.
