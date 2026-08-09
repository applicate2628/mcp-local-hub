# Closure — fetch MCP dependency compatibility

Closed: 2026-08-08T22:50:35Z
Outcome: DELIVERED and DEPLOYED in PR #594, squash commit `3d87718393b09b7770662df367653a5b09309a29`.
Evidence: focused normal/race tests, vet, Windows build, live MCP 200/200/204 probes, current-head Codex review, and 12-file publication scan all passed.
Residual risk: the deployed recovery build should be replaced by an ordinary `master` deployment after its other integrated PRs merge; no fetch blocker remains.

## Outcome

DELIVERED and DEPLOYED. PR #594 was squash-merged to `master` as commit `3d87718393b09b7770662df367653a5b09309a29`.

The canonical fetch manifest now pins `mcp==1.28.1` with `mcp-server-fetch@2026.7.10`. Installation has an explicit `--no-client-config` contract that materializes daemon and supervisor intent without reading or writing client configurations, and the Windows upgrade path stages the candidate binary beside the canonical target before rename-aside replacement.

## Functional verification

- Focused `internal/api` and `internal/cli` normal tests passed.
- The same focused set passed under the Go race detector.
- `go vet ./internal/api ./internal/cli` passed.
- The Windows production build completed with commit `2d617efd` embedded before merge.
- `fetch/default`, `vcpkg/default`, and `route/front` were Running in the canonical supervisor fleet.
- Fresh fetch and vcpkg MCP initialize/tools-list/DELETE lifecycles returned 200/200/204 and exposed tools.
- Route readiness returned 405 with `Allow: POST,DELETE`.
- Hosted Codex reviewed exact PR head `2d617efd2450a7b0fd135c323447b33dc07f1fd5` and reported no major issues with zero inline threads.

## Publication verification

All 12 PR files passed the publication-safety scanner immediately before merge. The local and hosted PR head were byte-identical and the worktree was clean.

## Residual risk

No known fetch functional blocker remains. The deployed binary was built from the verified integration commit retained at `refs/recovery/mcphub-prod-20260809`; later ordinary deployment from `master` should replace that recovery build after the other integrated PRs merge.

## Archive location

`work-items/archive/2026-08/2026-08-01-fetch-mcp-dependency-compat/`

## Retrospective

- Runtime validation was load-bearing because the manifest is embedded in the binary; editing the YAML alone could not repair the running daemon.
- The GitHub API publication fallback briefly produced a partial branch commit after truncated manifest processing. The branch was completed without rewriting published history, and squash merge kept that transport artifact out of `master` history.
