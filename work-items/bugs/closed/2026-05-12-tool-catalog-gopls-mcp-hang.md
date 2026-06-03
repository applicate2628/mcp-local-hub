---
title: TestToolCatalog_GoldenAgainstUpstream hangs invoking `gopls mcp` when gopls is on PATH
severity: low
found-by: g4-phase3 r6 verification on PR #157
found-on: 2026-05-12
project: mcp-local-hub
context: pre-existing master flake
status: closed
related-pr: pending Phase 3 (feat/g4-phase3-resolver-sessions-aggregator)
---

# Pre-existing test hang: tool_catalog_test.go probes `gopls mcp` and never times out

## Reproduction

On any dev box that has `gopls` on PATH (every Go developer):

```bash
go test -tags=test_state_path_env -count=1 -timeout 5m -run '^TestToolCatalog_GoldenAgainstUpstream$' ./internal/api/
```

hangs the full 5-minute test deadline. Stack capture at timeout
shows `tool_catalog_test.go:115` (`t.Run(tc.kind, ...)`) blocked
inside `captureToolsList` — the `gopls mcp` subprocess is waiting on
its stdio and the test's read pump never completes.

## Why this is pre-existing

`tool_catalog_test.go` has NOT been touched on
`feat/g4-phase3-resolver-sessions-aggregator` vs `origin/master`:

```bash
$ git diff origin/master..HEAD -- internal/api/tool_catalog_test.go
# (empty)
```

The test was designed to skip when the upstream binary is absent
(`t.Skipf("%s not on PATH; skipping live golden test", tc.bin)`), but
it does NOT skip when the binary IS present but hangs. The skip path
covers CI (no Go toolchain in the test image) but not local dev.

## Why filing as adjacent finding

- Phase 3 r6 deliverable touches only `hub_mcp_aggregator.go`,
  `hub_mcp_request_id.go`, and their tests. Tool catalog code is out
  of scope.
- The 5-minute hang costs the user 5 minutes per pre-push verification.
  Skipping the test locally is the cheap workaround; the proper fix
  needs a wall-clock cap inside `captureToolsList`.

## Suggested fix

Add a context-with-timeout inside `captureToolsList` (e.g., 30s):
on expiry, kill the subprocess and `t.Skipf("upstream probe timed out")`
so the catalog drift check is best-effort, never blocking. Mirrors
the timeout pattern already in `hub_mcp_aggregator.go`'s
`PerDaemonInitTimeout`.

## Related code

- `internal/api/tool_catalog_test.go:97-160` — golden test definition
- `internal/api/tool_catalog_test.go:128` — `captureToolsList` call
  site (the hang point)

## Workaround for the Phase 3 PR

Skip this test on local pre-push verification with
`-skip 'TestToolCatalog_GoldenAgainstUpstream'`. CI runs on a runner
without `gopls` installed, so the test self-skips via the existing
`exec.LookPath` check and the suite passes cleanly there.

## Status

**CLOSED — fixed in commit `d27c10e`** ("feat(api): embedded tool
catalog + synthetic initialize/tools-list"). The suggested fix (a
wall-clock cap inside `captureToolsList`) is implemented: HEAD
`internal/api/tool_catalog_test.go:188` wraps the helper in
`context.WithTimeout(context.Background(), 20*time.Second)` and drives
the subprocess via `exec.CommandContext`, so a wedged `gopls mcp`
child is killed at 20 s instead of hanging the full 5-minute deadline.
A `defer` at `internal/api/tool_catalog_test.go:210-213` calls
`cmd.Process.Kill()` + `cmd.Wait()` on every exit path, and a
background goroutine drains stderr (`:214`) so the child never blocks
on a full pipe. The exact wall-clock-cap remedy this doc requested.

Verified 2026-06-02 (`d27c10e` is an ancestor of HEAD; the
`context.WithTimeout(..., 20*time.Second)` + `Process.Kill()`/`Wait()`
defer + stderr drain are all present at HEAD in `captureToolsList`).
The old "the hang point" line refs in the Related-code section above
(`:97-160`, `:128`) predate the rewrite and no longer line up; the
current cap lives at `tool_catalog_test.go:186-214`.
