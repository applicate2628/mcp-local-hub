# MCP front daemon PR #588 live-finding closure

Admission source: direct operator request on 2026-07-27 to classify and close
the live Codex-bot findings on PR #588, branch `feat/mcp-front-daemon`.

## Admitted outcome

Classify every reported finding as `ALREADY FIXED`, `REAL, open`, or `WRONG`.
For every open defect, correct the root owner and the complete defect class,
prove the regression test fails without the correction, run the required
verification, and commit without pushing. A local unpushed commit is evidence
to inspect, not proof of closure.

## Boundaries

- Stay inside this worktree.
- Any `internal/api` or `internal/cli` test run uses
  `-tags=test_state_path_env` and a fresh `MCPHUB_STATE_DIR_OVERRIDE`.
- Never run unscoped `go test ./...`.
- Never launch the GUI, tray, supervisor, or production scheduler paths.
- Never kill by image name.
- Never checkout, hard-reset, stash, or push.
- Build and vet are required broad checks; package tests remain narrowly
  filtered and state-sandboxed.

## Success signals

- Every finding has evidence-backed classification.
- Every defect-class sweep enumerates all participants.
- Every real fix has a mutation-failure proof and restored green run.
- `go build ./...`, `go vet ./...`, and scoped tagged package tests exit 0.
- One local commit names the findings and closure mechanism; no push occurs.
