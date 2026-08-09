# Supervisor-liveness headless-fleet recovery + GUI exit-reason attribution

Template: quick-fix (requiresLead: false) · Lead: main conversation · Opened: 2026-07-25
Worktree: `<worktree>`, branch `feat/liveness-headless-gui-recovery` (off `origin/master@1889cff6`)

## Goal
`mcphub supervise --ensure-alive`'s `if running` branch was a bare no-op: a live supervisor
was always treated as healthy without checking whether its GUI owner (serena/LSP router,
hub aggregate) was still alive. Live incident: GUI dead ~4h after a reboot until manual
relaunch, while the supervisor kept the fleet looking "up". Companion ask: attribute WHICH
trigger began a `mcphub gui` process's exit (signal / tray Quit / tray Quit-and-stop-all /
restart-v3 self-restart) in `supervisor-events.log`.

A reliability design pass validated the original spec against the code and produced 5
corrections (stale line numbers in Part A; a hardcoded-port smell in Part B's serving
probe; a signature-threading dead end; a synchronous-before-os.Exit requirement; a
test-isolation gap) — all applied below.

## Delivered (2 commits on this branch)

- **`091ece37`** — fix(supervisor): recover a headless fleet (supervisor alive, GUI dead) — Part B.
- **`18700445`** — feat(gui): attribute which trigger began a GUI process exit — Part A.
- **`6a32c027`** — docs(bugs): adjacent-finding log for two pre-existing test flakes surfaced during gating (not caused by this work; not fixed here).

## Part B — headless-fleet recovery

`internal/cli/supervise_ensure_alive.go`:
- `if running {}` (was :685-689) now calls the widened `guiOwnerAliveFn()` seam; GUI dead
  routes into `runEnsureAliveHeadlessFleet`, which applies two fail-closed suppressors
  (an unexpired restart-v3 handoff marker; a supervisor boot-grace window, 45s) before
  relaunching the GUI owner via the SAME seam (`livenessRelaunchFn` /
  `relaunchSupervisorOwner`) genuine owner-death already uses — re-firing the autostart
  task ADOPTS the live supervisor (`gui_supervisor_owner.go`, spawned:false), zero daemon
  churn.
- `guiOwnerAliveFn` / `probeGUIOwnerAlive` widened from `(bool, int)` to `(bool, int, int)`
  (alive, pid, port) so the post-relaunch serving attestation dials the GUI's actual
  configured port instead of a hardcoded 9125 (zero external callers, grep-confirmed).
- New events: `gui-headless-fleet-detected`, `gui-headless-fleet-relaunch-suppressed`
  (machine-filterable `reason`: `"live-handoff"` | `"boot-grace"`),
  `liveness-relaunched-gui-headless-fleet` (+ non-gating `serving_probe_ok`),
  `liveness-gui-headless-relaunch-failed`.
- Every read (handoff marker, supervisor lock-owner sidecar) fails closed by suppressing
  the tick, never by guessing.

Tests: 3 new falsification tests (`TestEnsureAlive_HeadlessFleet_RelaunchesGUI`,
`_BootGraceSuppresses`, `_LiveHandoffSuppresses`), each with a manually-executed
mutation proof (temporarily deleted the guarded code, confirmed the test fails, restored,
confirmed identical to the pre-mutation diff via `diff`). Fixed 8 pre-existing tests that
hold the real `supervisor.lock` and would otherwise fall through to the REAL
`probeGUIOwnerAlive` (touching the developer's actual `%LOCALAPPDATA%` pidport) once this
branch started consulting it.

## Part A — GUI exit-reason attribution

New `internal/gui/gui_exit_reason.go`: `EmitExitReasonEvent` — one bounded
(`EmitWithTimeout`, 2s) best-effort emitter shared by `internal/cli` and `internal/gui`,
writing a single `gui-exit-reason` event with a machine-filterable `reason` field.

Instrumented sites (re-located by symbol name — the original spec's line numbers were
stale):
- `internal/cli/gui.go`'s `signal.NotifyContext` (RunE, ~:473): an ADDITIONAL
  `signal.Notify` observer channel (Go fans out one signal to every registered channel —
  does not steal/delay delivery; a second Ctrl-C still kills the process) attributes
  sigint vs sigterm, which `ctx.Done()` alone cannot distinguish.
- `internal/cli/gui.go`'s tray `Quit` and `QuitAndStopAll` closures (~:1403-1413).
- `internal/gui/gui_self_restart.go`'s `RequestSelfRestartExit` (wraps `selfRestartExitFn`
  = `os.Exit(0)`, ~:171-176): emitted SYNCHRONOUSLY before `selfRestartExitFn()` — `os.Exit`
  runs no deferred functions, so a deferred emit would never fire.

Tests: 2 new tests in `internal/gui/gui_exit_reason_test.go` (row shape + reason-field
round trip; distinct-values sweep across the 5 `GUIExitReason` constants). No existing
test calls `RequestSelfRestartExit` or the tray closures directly (tests inject their own
stub `Exit`/`Quit` functions), so this instrumentation is inert under `go test`.

## Gate

- `go build ./...` — clean.
- `go vet ./...` — clean.
- `go test ./internal/cli/... -run 'TestEnsureAlive|TestRestartV3'` — PASS (25 tests).
- `go test ./internal/gui/... -run 'TestEmitExitReasonEvent'` — PASS (2 tests).
- `go test ./internal/cli/... -run 'TestGui|TestRestartV3|TestSelfRestart'` — PASS.
- Full `internal/cli` and `internal/gui` sweeps were NOT used as the gate signal — both
  are independently flaky on this host (confirmed pre-existing, reproduces on the base
  commit; see the adjacent-finding bug doc) and CLAUDE.md already documents the
  `internal/cli` sweep as "flaky/crashy on this host". Targeted `-run` filters covering
  every changed symbol were used instead, per the task's own gate instruction.
- No test-spawned `mcphub.exe` processes existed at any point (verified via
  `Get-CimInstance Win32_Process`; the only running `mcphub.exe` processes were the
  operator's live fleet at the installed `~/.local/bin` path — left untouched).

## Review round 2 — independent cross-family reviewer, 5 findings, all fixed

An independent cross-family reviewer returned REVISE against the round-1 delivery above.
All 5 findings were reproduced with a failing test first, fixed, and mutation-proven
(guard temporarily removed, target test confirmed to fail, guard restored) before
committing. 5 new commits on this branch:

- **`90fcf526`** (P1-1) — `guiOwnerAliveFn`/`probeGUIOwnerAlive` widened from a bare bool
  to a `guiOwnerProbeState` tri-state (`guiOwnerStateAlive` /
  `guiOwnerStateConfirmedDead` / `guiOwnerStateUnknown`) keyed on `Verdict.Class`. The
  round-1 bool collapsed a malformed/unresolvable probe into the same value a
  confirmed-dead owner produces, which AUTHORIZED the headless-fleet relaunch (a
  `schtasks /Run` against a `StopExisting` task) on a merely-ambiguous read — the worst
  finding. Only `guiOwnerStateConfirmedDead` (`VerdictDeadPID`) authorizes that relaunch
  now; `guiOwnerStateUnknown` suppresses (headless-fleet branch) or routes to the
  GUI-independent standalone relaunch (supervisor-down branch) instead. Side effect: also
  fixes a pre-existing macOS gap (a ping-matching Healthy verdict used to read
  `PIDAlive=false` there).
- **`9f278d5e`** (P1-2) — `ensureAliveHeadlessFleetLiveHandoffSuppressed` no longer gates
  its handoff-marker READ on this process's own `gui.RestartV3Enabled()`. That resolver
  legitimately gates whether THIS process may INITIATE a v3 restart; it must not disable
  RECOGNITION of another process's already-in-flight handoff marker. Added
  `gui.ResetRestartV3ResolvedForTest` (single owner, replacing a private test-only copy)
  so a cross-package test can force the gate's value despite its process-wide
  `sync.Once`.
- **`fad399df`** (P1-3) — added the `guiServingProbeFn` injectable seam so
  `TestEnsureAlive_HeadlessFleet_RelaunchesGUI` no longer issues a real HTTP GET to
  `127.0.0.1:9125` (the well-known default GUI port) when it "fakes" a relaunch — the
  post-relaunch serving attestation was calling the real probe unconditionally.
- **`91d7da93`** (P1-4) — `SupervisorEventLog.emit`'s `emitTimeout` mode now bounds the
  ENTIRE write (rotation/open/write/close), not just mutex+flock acquisition. Fixed in
  the shared `internal/api/supervisor_events.go` (not any one caller) since 6 call sites
  across 3 packages rely on the same documented "never hang forever" contract, most
  notably `RequestSelfRestartExit` emitting synchronously right before `os.Exit(0)`.
- **`cde73948`** (P2-5) — `gui.EmitExitReasonEvent` now dedups process-wide
  (`exitReasonOnce`, first-trigger-wins) so a signal racing a tray Quit can't write two
  conflicting reasons. Extracted `internal/cli/gui_exit_signal.go`'s
  `awaitGUIExitSignalReason` so RunE's signal observer selects on `ctx.Done()` as well as
  the signal channel — making it safe to JOIN via a `sync.WaitGroup` before RunE returns
  (round-1's version was fire-and-forget, so a fast shutdown could exit before its event
  landed).

Gate for this round: `go build ./...` clean, `go vet ./...` clean, scoped `-run`
tests green on `internal/api` (`SupervisorEvent`), `internal/cli`
(`EnsureAlive|ExitSignal|AwaitGUIExitSignalReason|ProbeGUIOwnerAlive`), and
`internal/gui` (`TestEmitExitReasonEvent|RestartV3`) — no unscoped sweep, no real
`mcphub` process started or killed.

## Status: delivered, NOT pushed

Bot review is down until Tuesday per instruction — no `git push` / PR opened. Branch is
ready for the next session to push + open the PR once the bot is back.

## Next action
Push `feat/liveness-headless-gui-recovery` and open the PR once Codex Cloud bot review is
back (Tuesday). Then run the full PR review workflow (CLAUDE.md "PR review + merge
workflow"): pre-push local verification, bot PASS loop, deep-security commission.

## Review round 3 — re-verification complete, REVISE

- **Primary task**: independently re-verify closure of the five round-2 findings.
- **Primary task status**: review complete; delivery remains active because the gate is `REVISE`.
- **Stage**: bounded correction required for findings 1, 4, and 5.
- **Main conversation role**: `$lead` orchestration.
- **Accepted evidence artifacts**:
  - `.reports/2026-07/report(analyst)-2026-07-26_01-49_liveness-fix-round-map.md`
    (`PASS` for factual completeness; not a closure verdict).
  - `.reports/2026-07/report(qa-engineer)-2026-07-26_02-15_liveness-fix-round.md`
    (`REVISE`; safe exact-name tests plus nine isolated mutation scenarios).
  - `.reports/2026-07/report(architecture-reviewer)-2026-07-26_20-20_liveness-fix-round.md`
    (`REVISE` verification gate; no anti-layering pile-up found).
- **Lead reconciliation**: findings 2 and 3 are `CLOSED`; findings 1, 4, and 5
  are `PARTIALLY CLOSED`. The architecture report's broader closure claims for
  findings 1 and 4 were not accepted because they conflict with the verified
  producer/error paths and daemon-recovery timeout/fallback path recorded by
  the analyst and QA artifacts.
- **Mutation disposition**: eight mutations were caught by their named tests;
  removing the RunE `WaitGroup` wiring left all six selected relevant tests
  green, so that claimed guard is not mutation-proven.
- **Next concrete action**: correct the three blocking residuals recorded in
  `work-items/bugs/2026-07-26-gui-owner-probe-ambiguous-error-and-stale-metadata.md`,
  `work-items/bugs/2026-07-26-daemon-recovery-late-audit-after-emit-timeout.md`,
  and
  `work-items/bugs/2026-07-26-gui-exit-reason-causal-arbitration-and-join.md`,
  then repeat the same safe re-verification gate.

## Completed agents

| Agent | Role | Result | Artifact |
| --- | --- | --- | --- |
| `analyst_fix_round` | `$analyst` | PASS | `.reports/2026-07/report(analyst)-2026-07-26_01-49_liveness-fix-round-map.md` |
| `qa_fix_round` | `$qa-engineer` | REVISE | `.reports/2026-07/report(qa-engineer)-2026-07-26_02-15_liveness-fix-round.md` |
| `architecture_fix_round` | `$architecture-reviewer` | REVISE | `.reports/2026-07/report(architecture-reviewer)-2026-07-26_20-20_liveness-fix-round.md` |

## Failed / rerouted agents

| Agent | Role | Result | Evidence |
| --- | --- | --- | --- |
| external architecture gate | `$external-reviewer` replacing `$architecture-reviewer` | UNVERIFIED; rerouted | Codex CLI `gpt-5.6-sol` xhigh emitted no final artifact; resume failed because the run was ephemeral. Evidence: `.scratch/external-architecture-20260726/`. |

## Review round 4 — PR #589 live Codex-bot findings

- **Primary task**: classify and close all seven supplied PR #589 Codex-bot
  findings on the current local branch.
- **Primary task status**: active.
- **Interruption marker**: none.
- **Stage**: reliability design re-verification.
- **Main conversation role**: `$lead` orchestration.
- **Last accepted artifact**: recovery/index reconciliation from
  `$knowledge-archivist` on 2026-07-27 (`PASS`).
- **Admission source**: direct user request on 2026-07-27, superseding the
  prior read-only five-finding round.
- **Classification**: breaking-or-cross-cutting behavioral correction; seven
  concurrency, resource-lifetime, audit-durability, and recovery-progress
  findings across `internal/cli` and `internal/daemonrecovery`.
- **Integration owner**: `$backend-engineer`.
- **Open obligations before closeout**: classify all seven findings; sweep each
  real defect class; implement root-cause fixes and tests; mutation-prove every
  real test; run tagged scoped tests plus tagged build/vet; independent
  architecture review; commit locally; do not push.

### Active agents

| Agent | Role | Model/effort | Status | Launched |
| --- | --- | --- | --- | --- |
| `workitem_recovery_audit` | `$knowledge-archivist` | inherited `gpt-5.6-sol` xhigh; multi-item recovery consistency required | completed `REVISE` | 2026-07-27 |
| `workitem_index_reconcile` | `$knowledge-archivist` | inherited `gpt-5.6-sol` xhigh; multi-item index/board reconciliation required | completed `PASS`; all five active folders indexed once, derived board created, no active/archive duplicate or stranded terminal marker | 2026-07-27 |
| `pr589_round4_classification` | `$analyst` | inherited `gpt-5.6-sol` xhigh; seven concurrency/history findings require class-complete factual mapping | completed `PASS`; F1-F4 already fixed by `f150be61`, F5-F7 real/open | 2026-07-27 |

Recovery/index gate artifact:
`.reports/2026-07/report(knowledge-archivist)-2026-07-27_04-54_pr589-index-board-reconcile.md`.

### Next concrete action

Re-verify the corrected `design.md` with the same
`$reliability-engineer`; F1-F4 require no source change.

### Design gate

- Two sealed external `$architect` candidate lanes completed with exit `0`.
- Main synthesis artifact: `design.md`.
- Gate: `PASS`.
- Selected contracts after reliability revision: typed atomic GUI-lease
  lifecycle; event-owner durable pending-event handoff; action-before-log
  ordering at the two recovery-preaction sites.
- Next stage: same-angle reliability re-verification.

| Agent | Role | Model/effort | Status | Launched |
| --- | --- | --- | --- | --- |
| `pr589_owner_seam_design` | external `$architect` candidate | Codex `gpt-5.6-sol` xhigh | completed `RETURN(lead)` | 2026-07-27 |
| `pr589_failure_injection_design` | external `$architect` candidate | Codex `gpt-5.6-sol` xhigh | completed `RETURN(lead)` | 2026-07-27 |
| main synthesis | `$architect` design-panel synthesis held by `$lead` | inherited runtime | completed `PASS` | 2026-07-27 |

### Reliability gate

- Artifact: `review-reliability-pr589-round4.md`.
- Initial gate: `REVISE`.
- Required corrections: typed race-free GUI lease lifecycle; action-before-log
  ordering because `TryEmit` leaves write I/O unbounded; explicit
  post-respawn audit-durability failure; all-mode replay and retain-on-error
  bounds.
- Design revision applied in `design.md`.
- Same-angle re-verification: `PASS`.
- Accepted guarantees: typed atomic GUI-lease lifecycle, event-owner
  process-exit-safe handoff with retained-history exactly-once, and
  action-before-observability ordering.
- Next action: `$planner` produces the exact implementation and mutation plan.

## Review round 4 — final reconciliation

- **State**: CLOSED.
- **Outcome**: PASS; all seven supplied PR #589 findings were classified and
  reconciled.
- **F1-F4**: ALREADY FIXED by local commit `f150be61`; the closing change did
  not alter those implementations, and their named guards passed.
- **F5**: fixed by one atomic GUI-owner lease lifecycle shared by probe,
  timeout, every release path, and all three GUI-owner relaunch decisions.
- **F6**: fixed by normalize-once event bytes, a process-exit-safe pending
  carrier, all-mode replay under the event-log locks, exact active-plus-`.1`
  deduplication, post-respawn durability finalization, and CLI exit 7.
- **F7**: fixed at both class participants by ordering the recovery or
  suppression decision before the blocking detection diagnostic.
- **Mutation evidence**: F5, F6, F7 headless, F7 Unknown-escalation, and the
  architecture large-integer correction each failed at their intended
  behavioral assertion; every inverse patch restored the exact pre-mutation
  SHA-256 and the guard then passed.
- **Verification**: initial named matrix 42/42; architecture-correction matrix
  34/34; full GUI package 1065 pass and full daemon-recovery package 46 pass;
  tagged repository build and vet exited 0 before and after the architecture
  correction.
- **Review**: reliability design re-verification PASS; architecture review
  found one lossy prepared-event validation regression, returned REVISE, and
  the same reviewer closed it PASS after a byte-preserving boundary fix and
  QA mutation proof.
- **Known external fixture gaps**: the full tagged API package retains five
  state-path tests incompatible with the mandatory process override; the full
  tagged CLI package retains eleven tests requiring absent `C:\mcphub.exe`.
  Exact controls reproduced both pre-existing classes; neither owning fixture
  file is in this change.
- **Publication**: local commit only; no push.
- **Rollback units**: F5 lifecycle and F7 ordering are independently
  reversible; the F6 event owner plus recovery/CLI consumer is one atomic
  rollback unit.

Closed: 2026-07-27
