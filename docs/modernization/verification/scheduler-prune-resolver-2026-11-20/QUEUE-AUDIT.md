# R17 continuation: read-only queue and overlap audit

**Snapshot:** 20 November 2026, 17:55:47 Moscow time (14:55:47 UTC). Repository: `applicate2628/mcp-local-hub`. Scope: existing open pull requests (PRs), exact file ownership and remaining scheduler consumer work. This note records a read-only audit; it is not a readiness, native qualification or integration result.

## Contents

1. [Current heads](#1-current-heads)
2. [Overlap and ownership](#2-overlap-and-ownership)
3. [Chosen next slice](#3-chosen-next-slice)
4. [Remaining gates](#4-remaining-gates)
5. [Evidence and limits](#5-evidence-and-limits)

## 1. Current heads

The open-PR collection returned **31 PRs, all Draft**. All 31 paginated changed-filename queries succeeded. The supplied checkpoint is unchanged:

**Table 1 — Pinned source references**

| Responsibility | Ref | Current commit |
|---|---|---|
| Accepted default branch | master | `a7d92d2852c4f6eb8afb0569ef76a545e38aacf7` |
| Scheduler backend | #660 | `b7831aabdb7162b3a39c78e73c3ab67558236d1d` |
| Modernization board/evidence | #623 | `b70dd87183c01bf22eaacd9f606ea1e8813e5f68` |
| Go isolation/recovery and quality plan | #658 | `cbefb0dd140e046732a7ee9ab2a92937eb483840` |
| R16 port observations | #659 | `a3568dfe3426afaca4176d57a2310097e5214e77` |
| R16 status | #628 | `453e856347cabcebb289ec1c9af70f12af3376c0` |
| Rust owner | #625 | `287a7772a4be75ed9fb2f9ba1feb68cf035ed907` |
| Rust identity | #644 | `1cf5754b81c850a3569be269642b49605895eeb2` |
| Foreign-function interface | #631 | `ebfa4ef32b57cfa022acdcfcb4a2745f32b815c6` |

The PR response's historical `base.sha` is not evidence that current master has moved backward. The default branch was read independently. No new published Rust wait/close checkpoint was observed. This does **not** establish absence of the user's parallel local work; `mh_wait1`/`WaitForExit` and `mh_close1`/`CloseExited` remain outside this work.

## 2. Overlap and ownership

**Table 2 — Relevant changed files, from actual file lists**

| Area | Open PR ownership | Implication |
|---|---|---|
| `internal/scheduler/scheduler.go`, `scheduler_com_windows.go`, `scheduler_windows.go` and related hidden/Create/LastResult/owned-run tests | #660 only | Existing backend responsibility; no second scheduler PR was found. |
| `internal/api/install.go` | #659 only | File-level overlap. Its actual diff changes port/PID lookup and kill observations around base lines 2850, 3132 and 4898–4970. |
| Install scheduler replacement at base lines 3815–3840 | No open PR changes these lines | The current Export/Delete suppression and late rollback registration remain present in pinned #660 source. |
| `pruneObsoleteServerTasks` snapshot/Delete/rollback at base lines 4266–4289 | No open PR changes these lines | The selected prune fix has no textual overlap with #659. The #659 branch or patch must not be integrated. |
| `internal/api/scheduler_mgmt.go`, `scheduler_swap.go`, `weekly_refresh.go` and corresponding tests | No open PR | No duplicate consumer transaction work was identified. |
| `internal/autostart/*`, `internal/cli/scheduler_cmd.go`, `internal/cli/weekly_refresh.go` | No open PR | The related existing consumer findings remain unimplemented in the current queue. |
| `internal/scheduler/entrypoint_migration_windows.go`, `enumerate_all_windows.go`, `xml_encoding.go` | No open PR | No independent export migration was found. |
| General isolation/recovery tests and `tools/testshards/*` | #658 | Already implemented work; preserve its history and do not duplicate or transfer it. |
| Roadmap and verification publications | #623 | Documentation/evidence only; current PR has no production Go/Rust changes. |

A direct read of #659's `install.go` patch confirms that its 34 changed lines (21 additions, 13 deletions) concern `lookupPortPID`, ownership observation and unknown release outcomes. They do not alter scheduler snapshot, replacement or prune transactions.

All 50 paths from the pinned R17 source manifest were intersected with all open changed-file lists. Their complete ownership map, together with all 31 heads and file lists, is saved in [snapshot.json](QUEUE-SNAPSHOT.json). The original 50-path manifest does not include `install.go`; that file was checked separately against its exact source and #659 patch.

## 3. Chosen next slice

Root selected the smaller first consumer change: **within `pruneObsoleteServerTasks`, an export error or zero-byte definition must prevent Delete for that task, while unrelated obsolete tasks continue through the existing best-effort loop.**

The pinned source currently performs this sequence:

1. Ignore an ExportXML error and leave `priorXML` empty.
2. Delete the selected obsolete task.
3. Register a rollback that can only report inability to recreate the task when the snapshot is empty.

This can remove an existing definition without any recoverable copy when export is denied or otherwise fails. That is a source-level defect, not a claimed native failure. Export failure and empty success must be distinguished in diagnostics, but both are insufficient inputs for the destructive step.

This slice is a continuation of the existing scheduler safety work. No new PR, branch integration, task mutation or COM replacement is necessary to make it reviewable. The helper/tests and one ownership fixture adjustment are being authored separately by the assigned implementation agent. This audit did not edit code.

**Outside this slice:** general install replacement still ignores Export/Delete failures and installs its rollback only after Create succeeds. Simply moving a rollback before Create is not sufficient evidence of safety against an external author: Create conflict can identify a different current definition that must not be deleted or overwritten. These remain next consumer transaction work, with precise ownership/observed-definition semantics required.

## 4. Remaining gates

The latest plan's section 22.4 is the current remainder; lower historical RX checklists are not fresh implementation requests.

- **Export replacement:** direct schtasks stdout is rejected in the observed environment. CIM InputObject produced a structurally different document; its semantic equivalence is not established. Export by the canonical name matched one observed COM definition but still needs the remaining state, visibility, absence/error and consumer UTF-8 cases. Keep the present COM route until that boundary is qualified.
- **Create conflict:** removal of `/F` and distinguishing command fixtures are already implemented. Native duplicate-registration behavior and preservation of the original definition remain open.
- **Consumer transactions:** replacement/install/upgrade/register/weekly recovery, independent primary/rollback errors, external replacement, schedule preservation and Disabled/unknown states remain separate obligations. A prune snapshot guard does not close them.
- **Native behavior:** no task creation, modification, run, stop or deletion is authorized in this audit. Completed visible/hidden reads and LastResult observations must not be repeated without a new question.
- **Go acceptance:** broad #660 commands remain NOT_RUN for the separately recorded isolation and input reasons. Historical full/tagged failures remain failures; selected mock tests cannot replace them. #658 recovery/isolation is already implemented and must not be recreated.
- **Parallel Rust:** wait/close local work is protected. Published HEADs alone cannot clear it for editing.
- **Review and integration:** no external quota review or CI dispatch was performed. No PR was closed or marked ready, and no merge, rebase, cherry-pick or other integration was performed or justified by this audit.

## 5. Evidence and limits

**Sources**

1. [Open PR collection](https://api.github.com/repos/applicate2628/mcp-local-hub/pulls?state=open&per_page=100&sort=updated&direction=desc), read once; all 31 corresponding paginated changed-filename responses were checked.
2. [Master branch](https://api.github.com/repos/applicate2628/mcp-local-hub/branches/master), independently read.
3. [#659 actual file patches](https://api.github.com/repos/applicate2628/mcp-local-hub/pulls/659/files?per_page=100), especially `internal/api/install.go`.
4. [Pinned #660 install source](https://github.com/applicate2628/mcp-local-hub/blob/b7831aabdb7162b3a39c78e73c3ab67558236d1d/internal/api/install.go).
5. [Pinned R17 source manifest](https://github.com/applicate2628/mcp-local-hub/blob/b70dd87183c01bf22eaacd9f606ea1e8813e5f68/docs/modernization/plans/scheduler-contracts-2026-11-19.sources.json).
6. Local supplied `r17-continuation/plan.published.md`, especially sections 20.2, 22.2 and 22.4, and `board.updated.md`, especially sections 0.1–0.3 and RX-05b.

This is a point-in-time read-only audit. It verifies the public queue and exact inspected source ranges. It does not certify unpublished local state, a native operating-system transaction, test results or the eventual candidate patch.

