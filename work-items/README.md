# Work items status board

Snapshot: 2026-07-27.

This is a derived navigation board. The thin active/archive registry is
[`index.md`](index.md); each linked `status.md` owns its item's current detail.
The commit anchor identifies the source baseline, while the summaries below
also reflect admitted, uncommitted task-memory updates present in this
worktree at snapshot time.

## Portfolio state

Four work items are active. The largest remaining bodies are productization
items 4-6 and the four supervisor-reliability design lanes. Other active work
is parked or awaiting bounded implementation and empirical verification.

| Active item | Current position | Canonical detail |
| --- | --- | --- |
| C/C++ build MCP | Parked at PR #541 pending the CMake-delegated preset-listing redesign | [status](active/2026-07-14-cbuild-mcp/status.md) |
| Productization and GUI solidification | Item 3 is delivered and deployed; phase-0 items 4-6 remain | [status](active/2026-07-16-productization-gui-solidify/status.md) |
| CLI first-run experience | Terminal-lifetime root cause and options are recorded; empirical console-close proof remains | [status](active/2026-07-20-cli-first-run-ux/status.md) |
| Supervisor reliability | Investigation is complete and four design lanes are dispatched | [status](active/2026-07-20-supervisor-never-crash-reliability/status.md) |

## Recovery navigation

- Use [`index.md`](index.md) to enumerate active and archived work.
- Resume an item from its linked `status.md`; do not infer current stage from
  directory names or this board.
- The most recent closure is
  [PR #589 headless GUI recovery](archive/2026-07/2026-07-25-liveness-headless-gui-recovery/closure.md).

## Terms and Abbreviations

- MCP: Model Context Protocol.
- PR: pull request.
- QA: quality assurance.
