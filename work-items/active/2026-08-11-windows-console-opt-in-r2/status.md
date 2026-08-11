---
template: full-delivery
orchestration: full-lead
status: active
started: 2026-08-11
updated: 2026-08-11 17:18
---

Reopens: 2026-08-10-windows-console-opt-in
Depends-on: none

## Current state

- **Scope boundary**: current hub candidate remains primary; user-approved PR fixes may proceed only in isolated worktrees with disjoint source/test surfaces and may not mutate or execute against the hub candidate tree.
- **Owner**: main session Lead.
- **Integration owner**: backend-engineer for candidate assembly; platform-engineer for release/install; Lead for final integration.
- **Evidence gate**: deterministic reproduction or immutable-HEAD A/B for every broad failure; real Linux and native macOS execution; independent final QA; publication safety before push; installed-process and visible-window proof after restart.
- **Primary task**: finish and ship the working Windows hub with console creation disabled by default and enabled only by exact leading `--debug-console`
- **Primary task status**: active successor of the parked delivery
- **Admission source**: direct user instruction on 2026-08-11 to continue through deploy/live verification
- **Stage**: Phase G — local commit complete; waiting for explicit push approval
- **Main conv role**: orchestrating Lead
- **Last accepted artifact**: `implementation-integration-final.md` — PASS; exact 105-path stage on current `origin/master`, publication-clean, SHA-256 `D3C40A5EC01F3E4F87A16B5DC9797BB829622E7F2D836D47A3AA7A6F6E93C082`
- **Open obligations before closeout**: user-authorized push; install/restart; live no-visible-console verification
- **Parallel PR follow-up**: #598 exact-head PASS and squash merge `d08ab398955d`; #600 exact-head PASS and squash merge `a95d5c7493d`; #599 exact-head PASS and squash merge `599bbd92fb63`; #597 closed as superseded; GitHub open PR count is 0; hub remains primary.
- **Priority**: high

## Active agents

| Agent | Role | Model/effort | Status | Launched |
| --- | --- | --- | --- | --- |
| none | — | — | integration gate terminal PASS | 2026-08-11 17:46 +03:00 |

## Completed agents

| Agent | Role | Result | Artifact |
| --- | --- | --- | --- |
| predecessor | full delivery chain | PARKED after Windows target 6/6 and 63/63 GREEN plus canonicalize fixture correction PASS | `work-items/archive/2026-08/2026-08-10-windows-console-opt-in/closure.md` |
| console-successor-readiness | knowledge-archivist | PASS — exact reopen path ready; unrelated historical drift does not block | `.reports/2026-08/report(knowledge-archivist)-2026-08-11_windows-console-successor-readiness.md` |
| console-lifecycle-reliability | reliability-engineer | PASS — 5-minute CLI alarm was package-budget misattribution; exact GUI leak owner and deterministic audit-lock guards established | `reliability.md` |
| console-test-lifecycle-impl | backend-engineer | PASS after REVISE 1 — actual eight-site AST owner guard, audit deterministic guards, full GUI/race/vet/format gates green | `implementation-f.md` |
| console-cli-adjacent-reliability | reliability-engineer | PASS — quiesce readiness and supervisor cancel-without-join owners isolated; exact candidate/HEAD A/B preserved broad failures as open | `reliability-cli-adjacent.md` |
| console-cli-lifecycle-arch | architect | REVISE — two-phase owner chosen, but Reconcile, maintenance/transitive workers, IPC settlement, finite bounds, and macOS process-generation contracts are missing | `design-cli-lifecycle.md` |
| console-cli-prerequisites | reliability-engineer | REVISE — finite settlement is not provable; Policy A needs fatal-exit/restart ownership, Policy B parks the baseline for this console release; Windows/Linux design PASS, native macOS narrow blocked | `reliability-cli-prerequisites.md` |
| console-platform-final | platform-engineer | REVISE — Windows/PE/npm/six-package gates PASS; native WSL non-CLI red and native macOS unavailable | `implementation-platform-final.md` |
| console-final-qa-r2 | qa-engineer | BLOCKED — candidate does not introduce Linux reds; native API baseline still fails, process timing remains unengineered, native macOS runner absent | `qa-final-r2.md` |
| console-linux-fix | backend-engineer | PASS — nine test-owner corrections; native WSL normal/race/common/vet/build and proportionate Windows/compile gates green | `implementation-linux-final.md` |
| console-final-qa-r2 reverify | qa-engineer | PASS — causal RED, Linux normal/race/common/vet/build, Windows exact/normal/changed-race/vet, product-slice scanner green; macOS operator-parked | `qa-final-r2.md` |
| console-final-integration | backend-engineer | PASS after one bounded EOF-only correction — remote-base drift overlap 0, exact 105-path stage, diff-check and publication scan green | `implementation-integration-final.md` |

## Operator policy decision

| Field | Value |
| --- | --- |
| **Decision** | Policy B |
| **Recorded** | 2026-08-11 13:44 +03:00 |
| **Authorized scope** | Park the pre-existing broad CLI lifecycle defects for this console-only release; do not add fatal-exit/restart behavior. |
| **Still mandatory** | Focused console/platform checks, available native platform evidence, independent QA, publication safety, commit/push, install/restart, and live no-visible-console verification. |
| **Owner of next action** | Lead routes remaining release gate. |

## Next action

Native macOS is explicitly parked because no target exists. The accepted exact stage is committed locally. Await explicit publication approval for the hub push, then install/restart and live-verify no visible consoles.
