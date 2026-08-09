---
title: "internal/api/cleanup.go findWindowsExeExtensionEnd panics with index-out-of-range on some go-test temp binary paths"
severity: medium
found-by: backend-engineer, MCP front-daemon Increment-1 implementation (2026-07-25) — full-suite verification pass
affected-surface: internal/api/cleanup.go (findWindowsExeExtensionEnd, firstTokenBasename, isOurOwnProcess); internal/cli/cleanup_aggressive_test.go (TestCleanupAggressive_IncludeClassFlagOverridesWithWarning)
context: adjacent-finding
status: open
---

## What happened

While verifying the MCP front-daemon Increment-1 diff with a full
`go test ./internal/gui/... ./internal/mcproute/... ./internal/cli/...` pass, one
unrelated test panicked (not a normal `FAIL`, a runtime panic that aborted the whole
`internal/cli` package run):

```
--- FAIL: TestCleanupAggressive_IncludeClassFlagOverridesWithWarning (0.88s)
panic: runtime error: index out of range [85] with length 67 [recovered, repanicked]
...
mcp-local-hub/internal/api.findWindowsExeExtensionEnd({0x...+43 bytes})
	internal/api/cleanup.go:332
mcp-local-hub/internal/api.firstTokenBasename(...)
	internal/api/cleanup.go:229
mcp-local-hub/internal/api.isOurOwnProcess(...)
	internal/api/cleanup.go:50
mcp-local-hub/internal/api.parseAggressiveCandidates(...)
	internal/api/cleanup.go:1474
mcp-local-hub/internal/api.(*API).AggressiveCleanup(...)
	internal/api/cleanup.go:1599
mcp-local-hub/internal/cli.newCleanupAggressiveCmdReal.func1(...)
	internal/cli/cleanup_aggressive.go:96
```

## Verified NOT caused by the front-daemon Increment-1 diff

The Increment-1 diff (branch `feat/mcp-front-daemon`) never touches
`internal/api/cleanup.go`, `internal/cli/cleanup_aggressive.go`, or
`internal/cli/cleanup_aggressive_test.go` — confirmed via `git diff origin/master..HEAD
--stat` (only `internal/cli/root.go`, `internal/cli/route.go`, `internal/gui/csrf.go`,
`internal/gui/route_adapter.go(_test.go)`, `internal/mcproute/*` changed).

Per the indirect-regression discipline (code-untouched is not proof of
non-involvement), this was verified empirically rather than assumed: a temporary git
worktree was added at the merge-base commit (`1889cff6`, before either Increment-1
commit) and the SAME single test was run in isolation there. It panics identically:

```
panic: runtime error: index out of range [85] with length 67
	.../basecheck-1889cff6/internal/api/cleanup.go:332
```

Same index (85), same length (67), same call chain — the bug is pre-existing on
`master` and independent of this branch's changes.

## Likely root cause (not yet root-caused to a fix, hypothesis only)

`isOurOwnProcess` → `firstTokenBasename` → `findWindowsExeExtensionEnd` parses a
process-identity string (almost certainly derived from `os.Args[0]` / the running
test binary's own path or a synthesized candidate command line) to find the `.exe`
extension boundary for basename comparison. The panic computes an index (85) past the
end of a 67-byte string — an off-by-some-offset bug in the boundary search, NOT a
nil/empty-input problem (the string is non-empty, length 67). It reproduces
deterministically for this ONE test (`TestCleanupAggressive_IncludeClassFlagOverridesWithWarning`)
on at least two different absolute checkout paths (`D:/dev/mcphub-front-daemon` and
`R:/Temp/.../basecheck-1889cff6`), so it is plausibly sensitive to the LENGTH or
shape of the `go test`-built temp binary's own path/name (which embeds the package
import path and varies by checkout location) rather than to any specific string
literal in the test itself — but this is an unverified hypothesis; a proper fix needs
someone to add a length guard / bounds check in `findWindowsExeExtensionEnd` and a
regression test using a synthetic overlong/short input, not a live path.

## Why this is filed as adjacent, not fixed here

Per the Backend Engineer role's adjacent-findings protocol: this is outside the
approved Increment-1 change surface (serena+LSP MCP front-daemon extraction) and does
not block Increment-1's own acceptance gate (the failure is isolated to one
pre-existing, unrelated test/command; `internal/gui` and `internal/mcproute` suites are
fully green, including under `-race`). Filed here for the orchestrator to prioritize
rather than fixed opportunistically.

## Suggested next step

Root-cause `findWindowsExeExtensionEnd`'s indexing against a range of synthetic
overlong/short candidate strings (not live `os.Args`), add a bounds check, and add a
regression test with a fixed, deterministic input reproducing the same shape as the
live failure (rather than depending on ambient checkout path length, which makes the
existing test flaky/environment-dependent by construction).
