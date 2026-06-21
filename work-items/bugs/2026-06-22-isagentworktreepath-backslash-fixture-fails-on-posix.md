---
title: TestIsAgentWorktreePath/windows-backslash case fails on POSIX (ToSlash no-op for `\`)
severity: low
found-by: backend-engineer
found-in-phase: PR #418 r2 dead-worktree mount-guard last-worktree fix — full POSIX (WSL) targeted test sweep
affected-surface: >
  internal/api/workspace_prune_test.go:18 (fixture
  {"windows-backslash agent worktree", `d:\dev\x\.claude\worktrees\agent-abc\sub`, true})
  vs internal/api/workspace_prune.go:66-71 (IsAgentWorktreePath → filepath.ToSlash)
context: adjacent-finding
status: open
---

## Symptom

On Linux/WSL (POSIX), `go test -run TestIsAgentWorktreePath ./internal/api/`
FAILS the subcase `windows-backslash agent worktree`:

```
workspace_prune_test.go:28: IsAgentWorktreePath("d:\\dev\\x\\.claude\\worktrees\\agent-abc\\sub") = false, want true
```

All other `TestIsAgentWorktreePath` subcases pass on POSIX; the whole test
passes on Windows.

## Root cause (verified)

`IsAgentWorktreePath` (workspace_prune.go:70) normalizes separators via
`filepath.ToSlash(canonicalPath)` before the substring match against
`agentWorktreeMarker` ("/.claude/worktrees/agent-"). `filepath.ToSlash`
translates the OS path separator to `/` — but on POSIX the separator is `/`
and backslash `\` is a LEGAL filename byte, so `ToSlash` is a no-op for a
backslash-laden string on Linux. The fixture passes a Windows-style
backslash path (`d:\dev\x\.claude\worktrees\agent-abc\sub`); on POSIX it
stays backslash-delimited, never contains the forward-slash marker, and the
match returns false.

This is a pre-existing test artifact, present verbatim at commit 9c0ca179
(before the dead-worktree mount-guard work): fixture at line 18 and the
`ToSlash` call at line 70 both predate this change.

## Not a production bug

The registry stores the EvalSymlinks-resolved, drive-lowercased CANONICAL
path, which on Windows uses backslashes that `ToSlash` DOES translate, and
on POSIX uses forward slashes natively. A real stored canonical path never
has the cross-OS mismatch the fixture synthesizes, so production
classification is correct on both platforms. The defect is confined to the
test fixture using a Windows-shaped literal that is only meaningful on
Windows.

## Suggested fix (not applied — outside approved change surface)

Gate the `windows-backslash` fixture on `runtime.GOOS == "windows"` (the
same GOOS-gating pattern already used in
`TestParseGitWorktreePointer_ForeignGitdir`), since a backslash-delimited
agent-worktree path is only a realistic canonical form on Windows.
