# Work-items recovery audit — PR #583 bootstrap

Audit time: 2026-07-27 04:53 +03:00
Role: `$knowledge-archivist`
Scope: read-only reconciliation of `work-items/active/`, the Active section of
`work-items/index.md`, and active/archive slug and lifecycle predicates.

```text
REPOSITORY ORIENTATION: scope=work-items/active/2026-07-27-pr583-live-bot-findings; status=live; workflow=work-items/index.md plus the item's roadmap.md, brief.md, status.md, and plan.md; protected=all existing work-item lifecycle states and archived cursor-opt-in closure; evidence=work-items/index.md:10-15,work-items/active/2026-07-27-pr583-live-bot-findings/roadmap.md:3-18,work-items/active/2026-07-27-pr583-live-bot-findings/status.md:8-20
```

No lifecycle state, existing work-item content, index row, or archive location
was changed by this audit.

## Named regression guard

The read-only guard combined:

- working-tree active directories from `Get-ChildItem work-items/active -Directory`;
- working-tree archive directory and Markdown basenames;
- `HEAD` paths from `git ls-tree -r --name-only HEAD -- work-items/active work-items/archive`;
- Active-section links parsed only between `work-items/index.md:10` and the next
  level-two heading;
- active `closure.md` existence and keyed lifecycle values matching
  `status|state|stage|outcome: closed|done|complete|completed|archived`.

| Check | Captured result | Control or evidence |
|---|---:|---|
| Working-tree active folders | 5 | `HEAD` contains 4; the union contains 5, proving the working-tree-only PR #583 item was included. |
| Active index rows | 2 | The two rows are at `work-items/index.md:14-15`. |
| Active folders missing an index row | 3 | The captured scan named `2026-07-14-cbuild-mcp`, `2026-07-20-cli-first-run-ux`, and `2026-07-20-supervisor-never-crash-reliability`. |
| Index rows without an active folder | 0 | Active-section parser result: `orphan_index_row_count=0`. |
| Active/archive duplicate slugs | 0 | Working-tree plus `HEAD` union result: `dual_state_count=0`. |
| Active `closure.md` files | 0 | The identical working-tree/`HEAD` check found 15 archive `closure.md` files, so the negative control was capable of finding closure files. |
| Done-keyed active statuses | 0 | The identical lifecycle-key regex found `Stage: archived` at `work-items/archive/2026-07/2026-07-26-cursor-opt-in-review-fixes/status.md:13`. |

Named guard outcome: zero dual-state slugs and zero done-marked or
closure-bearing active folders, but only 2 of 5 active folders have an Active
index row.

## Active-folder reconciliation

The required recovery-file inventory is `roadmap.md`, `brief.md`, `status.md`,
and `plan.md`. Missing-file claims below come from the same five-folder
filesystem inventory used by the named guard.

| Active folder | Index | Required-file inventory | Lifecycle evidence | Gap and remediation owner |
|---|---|---|---|---|
| `2026-07-14-cbuild-mcp` | Missing | Present: `status.md`; missing: `roadmap.md`, `brief.md`, `plan.md` | `work-items/active/2026-07-14-cbuild-mcp/status.md:3-7` says full delivery and implementing; the same file at line 55 also says PR #541 is parked. Neither value matches the done predicate. | `$lead`: reconcile the current semantic stage and supply the missing accepted artifacts. |
|  |  |  |  | `$knowledge-archivist`: add the Active index row after the accepted status is unambiguous. |
| `2026-07-16-productization-gui-solidify` | Present at `work-items/index.md:15` | Present: `roadmap.md`, `brief.md`, `status.md`; missing: `plan.md` | `work-items/active/2026-07-16-productization-gui-solidify/status.md:3-7` records an admitted full-delivery item. | `$lead`: supply or accept the canonical `plan.md`; no index action is needed. |
| `2026-07-20-cli-first-run-ux` | Missing | Present: `status.md`; missing: `roadmap.md`, `brief.md`, `plan.md` | `work-items/active/2026-07-20-cli-first-run-ux/status.md:1-3` records an opened quick-fix item. | `$lead`: supply the missing accepted artifacts and confirm its current semantic stage. |
|  |  |  |  | `$knowledge-archivist`: add the Active index row from the accepted status. |
| `2026-07-20-supervisor-never-crash-reliability` | Missing | Present: `status.md`, `plan.md`; missing: `roadmap.md`, `brief.md` | `work-items/active/2026-07-20-supervisor-never-crash-reliability/status.md:3-6` records full delivery with design lanes dispatched. | `$lead`: supply the missing roadmap and brief and confirm the current semantic stage. |
|  |  |  | `work-items/active/2026-07-20-supervisor-never-crash-reliability/plan.md:1-6` is the accepted delivery-plan surface. |  |
|  |  |  |  | `$knowledge-archivist`: add the Active index row from the accepted status. |
| `2026-07-27-pr583-live-bot-findings` | Present at `work-items/index.md:14` | Present: all four required files | `work-items/active/2026-07-27-pr583-live-bot-findings/status.md:10-17` records an active primary task, research stage, and open obligations. | None. This item is recovery-complete for the current stage. |

All five active folders contain `status.md`; none contains `closure.md`.

## Current PR #583 recovery gate

The current item has:

- admitted outcome and success signals in
  `work-items/active/2026-07-27-pr583-live-bot-findings/roadmap.md:3-18`;
- scoped acceptance, required roles, and change boundary in
  `work-items/active/2026-07-27-pr583-live-bot-findings/brief.md:3-61`;
- an active primary task and explicit next obligations in
  `work-items/active/2026-07-27-pr583-live-bot-findings/status.md:8-20`;
- the full seven-step chain through local commit and archive in
  `work-items/active/2026-07-27-pr583-live-bot-findings/plan.md:1-13`;
- an Active registry row at `work-items/index.md:14`.

The PR #583 task item is therefore recovery-complete for its current research and
classification stage. This conclusion does not claim that the delivery work is
complete;
`work-items/active/2026-07-27-pr583-live-bot-findings/status.md:16-17`
lists the remaining gates.

## Diff-invisible invariant status

| Invariant | Status | Exact follow-up |
|---|---|---|
| One location per active/archive slug | Satisfied | No action; preserve the union scan at the next lifecycle change. |
| Every active folder has an Active index row | Not satisfied | `$lead` resolves semantic status where needed; `$knowledge-archivist` then adds the three missing rows. |
| No active folder is closure-bearing or done-keyed | Satisfied | No action; rerun after any close/archive mechanics. |
| PR #583 item has roadmap, brief, status, plan, and index entry | Satisfied | Main-session `$lead` may continue to implementation routing. |

## Stewardship disposition

The audit artifact is complete and the current PR #583 item is recovery-complete.
Repository-wide index and artifact completeness still contains the explicitly
itemized pre-existing drift above. No remediation patch was authorized in this
lane.

## Terms and Abbreviations

- **Active index row** — a row in the Active section of `work-items/index.md`.
- **Done predicate** — a keyed lifecycle value beginning with closed, done,
  complete, completed, or archived.
- **HEAD** — the current Git commit, used as a second input to the union scan.
- **PR** — pull request.
