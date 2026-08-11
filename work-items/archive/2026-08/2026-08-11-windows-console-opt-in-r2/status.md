---
template: full-delivery
orchestration: full-lead
status: completed
started: 2026-08-11
updated: 2026-08-11 18:34
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
- **Stage**: terminal PASS — published, installed, restarted, and live-verified
- **Main conv role**: orchestrating Lead
- **Last accepted artifact**: `deployment-live.md` — PASS; installed commit `b87dc8dd`, 21/21 live samples over 646.906 seconds with zero visible windows, SHA-256 `8686302E0660FFA9D49D4B16807EAEC4AD0C68FC2108984A02B328AD49FAE249`
- **Open obligations before closeout**: physical archive move and derived README reconciliation only
- **Parallel PR follow-up**: #598 exact-head PASS and squash merge `d08ab398955d`; #600 exact-head PASS and squash merge `a95d5c7493d`; #599 exact-head PASS and squash merge `599bbd92fb63`; #597 closed as superseded; GitHub open PR count is 0; hub remains primary.
- **Priority**: high

## Active agents

| Agent | Role | Model/effort | Status | Launched |
| --- | --- | --- | --- | --- |
| none | — | — | delivery terminal PASS | 2026-08-11 18:34 +03:00 |

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
| console-live-deploy | platform-engineer + windows-gui-manual-testing | PASS — installed candidate-bound PE2 binary, recovered hub and MCP fleet, 21/21 live samples with zero visible windows | `deployment-live.md` |

## Operator policy decision

| Field | Value |
| --- | --- |
| **Decision** | Policy B |
| **Recorded** | 2026-08-11 13:44 +03:00 |
| **Authorized scope** | Park the pre-existing broad CLI lifecycle defects for this console-only release; do not add fatal-exit/restart behavior. |
| **Still mandatory** | Focused console/platform checks, available native platform evidence, independent QA, publication safety, commit/push, install/restart, and live no-visible-console verification. |
| **Owner of next action** | Lead routes remaining release gate. |

## Next action

Lead has decided terminal delivery PASS. Lifecycle owner moves this completed item to `work-items/archive/2026-08/2026-08-11-windows-console-opt-in-r2/` and reconciles the derived README without disturbing unrelated active items.
Lifecycle-schema: work-items-physical-v1
