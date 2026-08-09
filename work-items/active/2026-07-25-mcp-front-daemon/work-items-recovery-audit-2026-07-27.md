# Work-items recovery audit — 2026-07-27

## Verdict

**PASS for the PR #588 finding-closure stage.** The current item is structurally
recoverable: its `brief.md`, `roadmap.md`, and `status.md` are present and
nonempty; the status is explicitly active, names the current research stage,
and lists the remaining gates
(`work-items/active/2026-07-25-mcp-front-daemon/status.md:8-21`). Its admitted
outcome still matches the operator request
(`work-items/active/2026-07-25-mcp-front-daemon/roadmap.md:6-12`).

The wider recovery tree has bounded drift recorded below. This audit makes no
lifecycle decision and repairs no unrelated item.

## Audit controls

The following read-only controls were run at branch HEAD
`3872ee1609afba53649ec1b00be09200015c0268`.

| Control | Result |
|---|---|
| Active tree slugs versus Active-index links | 5 tree slugs; 5 index rows; 5 unique rows; set difference 0 |
| Active index links | 5/5 targets exist |
| Required active artifacts | 5 `status.md`; 2 `brief.md`; 2 `roadmap.md` |
| Active orphan predicate | 0 `closure.md`; 0 `status/state/stage/outcome = closed/done/complete/completed/archived` matches |
| Active/archive slug intersection | 0 |
| Archive tree slugs versus Archived-index links | 16 tree slugs; 14 index rows; 14 unique rows; 2 tree-only slugs |
| Archive link and closure controls | 14/14 index targets exist; all 14 archived directories contain `closure.md` |
| Epic tree slugs versus Epics-index links | 3 epic files; 2 index rows; 2 unique rows; 1 tree-only slug |
| Epic index links | 2/2 targets exist |
| Dependency declaration scan | Three matching lines: current item `Epic: none`, current item `Depends-on: none`, and one archived dependency record |
| Status-board control | `Test-Path work-items/README.md` = `False`; `git ls-files --error-unmatch work-items/README.md` exited 1 |

Falsifying probes: adding/removing an active folder or Active-index link changes
the first set difference; adding a closure/done marker changes the orphan
count; duplicating a slug under archive changes the intersection; adding either
missing archive row or the missing epic row reduces its corresponding
tree-only set.

## Active-item reconciliation

| Active item | Required artifacts | Classification | Evidence |
|---|---|---|---|
| `2026-07-14-cbuild-mcp` | `status.md` present; `brief.md` missing; `roadmap.md` missing | **MISSING + internally stale** | The first live state says “DESIGN ACCEPTED → IMPLEMENTING” while the later owner record says PR #541 is parked (`work-items/active/2026-07-14-cbuild-mcp/status.md:6-7`, `work-items/active/2026-07-14-cbuild-mcp/status.md:38-56`). The index already discloses this conflict (`work-items/index.md:14`). |
| `2026-07-16-productization-gui-solidify` | All three present | **STALE brief/roadmap/index summary** | The brief still scopes the PR #563 round-1 correction and says “Current stage: corrective implementation” (`work-items/active/2026-07-16-productization-gui-solidify/brief.md:3-8`, `work-items/active/2026-07-16-productization-gui-solidify/brief.md:39-50`). |
|  |  |  | The status explicitly supersedes that round-1 resume point and records merge/deploy plus the next Phase-0 items (`work-items/active/2026-07-16-productization-gui-solidify/status.md:285-308`). |
|  |  |  | The roadmap still labels the already-delivered item-1 milestone “in progress” (`work-items/active/2026-07-16-productization-gui-solidify/roadmap.md:30-50`), while the status records items 1 and 2 delivered (`work-items/active/2026-07-16-productization-gui-solidify/status.md:30-48`). |
|  |  |  | The index says the round-1 changes are uncommitted and undeployed (`work-items/index.md:15`), contradicted by the later merge/deploy record above. |
| `2026-07-20-cli-first-run-ux` | `status.md` present; `brief.md` missing; `roadmap.md` missing | **MISSING** | The status remains at revision delivered / awaiting Lead gate (`work-items/active/2026-07-20-cli-first-run-ux/status.md:40-48`), matching the index summary (`work-items/index.md:16`). |
| `2026-07-20-supervisor-never-crash-reliability` | `status.md` present; `brief.md` missing; `roadmap.md` missing | **MISSING** | The status says investigation complete and design lanes dispatched (`work-items/active/2026-07-20-supervisor-never-crash-reliability/status.md:3-6`, `work-items/active/2026-07-20-supervisor-never-crash-reliability/status.md:191-201`), matching the index summary (`work-items/index.md:17`). |
| `2026-07-25-mcp-front-daemon` | All three present | **CONSISTENT current item; stale index summary only** | Status updated 2026-07-27 04:45 and explicitly active (`work-items/active/2026-07-25-mcp-front-daemon/status.md:1-19`). |
|  |  |  | The current brief scopes fourteen reported rows as ten deduplicated classes (`work-items/active/2026-07-25-mcp-front-daemon/brief.md:1-10`), but the index still describes the previous seven-finding round (`work-items/index.md:18`). |

The Active index path set itself is exact: all five folders have exactly one row
at `work-items/index.md:14-18`, and every linked `status.md` exists.

## Orphan and dual-state controls

No active item meets the hook-aligned orphan predicate: the recursive
`closure.md` count is zero, and the anchored lifecycle-marker search returned
`NO_MATCHES`. This is a structural result, not a semantic close decision.

No active slug is also present among the 16 logical archive slugs. The exact
active/archive intersection count is zero.

`2026-07-20-cli-first-run-ux` remains a possible **semantic** close/continue
decision because its latest stage is “delivered — awaiting lead gate”
(`work-items/active/2026-07-20-cli-first-run-ux/status.md:43-48`), but it is not
a structural orphan and this archivist pass does not decide its lifecycle.

## Archive/index reconciliation

| Archived item | Form | Closure/state evidence | Archived index |
|---|---|---|---|
| `2026-06-11-pr288-cumulative-review` | directory | `closure.md` present | present |
| `2026-06-15-dynamic-mcp-discovery` | directory | `closure.md` present | present |
| `2026-06-15-settings-redesign` | directory | `closure.md` present | present |
| `2026-06-15-supervisor-lifecycle-permanent-fix` | directory | Closed 2026-06-15; delivered (`work-items/archive/2026-06/2026-06-15-supervisor-lifecycle-permanent-fix/closure.md:1-6`) | **MISSING** |
| `2026-06-15-workspace-daemon-prune` | directory | `closure.md` present | present |
| `2026-06-17-phase1-audit-findings` | flat legacy file | Header reconciliation records 6/6 resolved or exempt (`work-items/archive/2026-07/2026-06-17-phase1-audit-findings.md:5-30`) | present |
| `2026-06-18-vendor-init-uninstalled-clients` | flat legacy file | Status resolved (`work-items/archive/2026-07/2026-06-18-vendor-init-uninstalled-clients.md:1-9`) | present |
| `2026-07-05-adopt-npx-orphans` | directory | `closure.md` present | present |
| `2026-07-05-unify-port-resolution-owner` | directory | `closure.md` present | present |
| `2026-07-09-adopt-side-durable-pre-adopt-provenance` | directory | `closure.md` present | present |
| `2026-07-09-deadopt-hub-to-native` | directory | `closure.md` present | present |
| `2026-07-09-intent-collapse-stop-resurrection` | directory | `closure.md` present | present |
| `2026-07-09-lsp-relay-per-client-disable-gui` | directory | `closure.md` present | present |
| `2026-07-09-test-leftover-reaper` | directory | `closure.md` present | present |
| `2026-07-12-adopt-abort-preserve-provenance` | directory | Closed 2026-07-13; delivered (`work-items/archive/2026-07/2026-07-12-adopt-abort-preserve-provenance/closure.md:1-11`) | **MISSING** |
| `2026-07-13-daemon-port-ephemeral-self-heal` | directory | `closure.md` present | present |

The control search for both missing slugs in `work-items/index.md` returned
`NO_INDEX_REFERENCES`. All 14 rows that do exist are at
`work-items/index.md:60-76`, and every target resolves.

## Epic and dependency reconciliation

| Epic | File status | Index | Child-rollup form |
|---|---|---|---|
| `2026-06-17-gui-quality-initiative` | closed; 19/19 rollup and closure (`work-items/epics/2026-06-17-gui-quality-initiative.md:20-49`) | **MISSING** | Legacy labels such as `gui-g1-*`, not repository work-item slugs |
| `2026-06-19-install-and-it-works-ux` | closed with closure (`work-items/epics/2026-06-19-install-and-it-works-ux.md:87-116`) | present (`work-items/index.md:45`) | Legacy numbered areas, not repository work-item slugs |
| `2026-06-23-desktop-app-mcp-catalog` | closed with closure (`work-items/epics/2026-06-23-desktop-app-mcp-catalog.md:87-99`) | present (`work-items/index.md:44`) | Legacy tier checklist, not repository work-item slugs |

All three closed epics predate the current mechanically resolvable child-slug
shape. Their own closure text is internally explicit, but live derivation from
child folders is unavailable. This is bounded legacy drift; no epic is active.

The only current-item dependency declaration is `Epic: none` /
`Depends-on: none`
(`work-items/active/2026-07-25-mcp-front-daemon/status.md:20-21`). The only
non-none dependency declaration found in the approved archive surface is the
deadopt record
(`work-items/archive/2026-07/2026-07-09-deadopt-hub-to-native/status.md:9`).
Its work-item dependency
`2026-07-09-adopt-side-durable-pre-adopt-provenance` resolves exactly once in
the archive. Its two `bug:` dependencies are **ASSUMPTION (UNVERIFIED)** in
this pass because `work-items/bugs/**` was outside the approved input surface;
resolve by checking those two bug registry paths and statuses.

## Status-board mismatch

`work-items/README.md` is absent from both the working tree and Git's tracked
path set. The repository therefore has no derived status board at the location
named by the recovery-governance contract. This is unrelated to the current PR
item and does not prevent its next research/implementation gate.

## Bounded follow-ups

No repair is authorized in this pass. A later stewardship pass can:

1. add or refresh the missing/stale brief and roadmap artifacts for the three
   unrelated active items and refresh productization's stale pair;
2. refresh the two stale Active-index summaries;
3. add the two omitted archive rows and the omitted closed-epic row;
4. decide whether to create the missing derived `work-items/README.md` board;
5. decide whether closed legacy epics remain grandfathered or receive
   resolvable child-slug metadata.

## Terms and Abbreviations

- CAS: Compare-And-Swap.
- CLI: Command-Line Interface.
- GUI: Graphical User Interface.
- PR: Pull Request.

