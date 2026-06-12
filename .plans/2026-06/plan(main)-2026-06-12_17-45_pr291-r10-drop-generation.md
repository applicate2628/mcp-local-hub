## Plan Snapshot

1. Ground finding in code and existing tests - completed.
2. Write failing race regression test - completed.
3. Implement drop-generation guard - completed.
4. Run required verification commands - completed.
5. Write session report and commit one change - in progress at snapshot time.

## Acceptance Criteria

- Add `serenaBackendDropGen map[string]uint64` to `Server`, guarded by `serenaBackendPIDMu`.
- Increment the generation in the single locked baseline drop owner.
- Capture candidate path generations with the prior reconcile baseline snapshot.
- At persist time, skip stale reconcile writes for paths whose generation advanced, preserving newer dropped or re-established state.
- Keep the existing `withWorkspaceCount` filter.
- Add a falsifying regression test for drop/rebind during an in-flight reconcile.
- Run the requested Go build, vet, test, race-test, and formatting checks.
- Create one local commit and do not push.

## Terms and Abbreviations

- GUI: graphical user interface.
- IPC: inter-process communication.
- PID: process identifier.
- PR: pull request.
