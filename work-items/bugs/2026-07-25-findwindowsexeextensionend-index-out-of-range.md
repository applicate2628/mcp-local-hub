---
title: findWindowsExeExtensionEnd panics with index-out-of-range when strings.ToLower changes the byte length of a process command-line string
severity: medium
found-by: backend-engineer, Increment 1b (supervisor auto-spawn of `mcphub route`) regression sweep
affected-surface: internal/api/cleanup.go (findWindowsExeExtensionEnd, firstTokenBasename, isOurOwnProcess, parseAggressiveCandidates, AggressiveCleanup)
context: adjacent-finding
status: open
---

## What happened

While running the internal/cli regression safety net for an unrelated change
(the supervisor auto-spawning the `mcphub route` front daemon,
work-items/decisions/2026-07-25-supervisor-builtin-singleton-daemon.md),
`TestCleanupAggressive_IncludeClassFlagOverridesWithWarning`
(internal/cli/cleanup_aggressive_test.go:60) panicked:

```
panic: runtime error: index out of range [85] with length 67 [recovered, repanicked]
mcp-local-hub/internal/api.findWindowsExeExtensionEnd(...)
	D:/dev/mcphub-front-daemon/internal/api/cleanup.go:332
mcp-local-hub/internal/api.firstTokenBasename(...)
	D:/dev/mcphub-front-daemon/internal/api/cleanup.go:229
mcp-local-hub/internal/api.isOurOwnProcess(...)
	D:/dev/mcphub-front-daemon/internal/api/cleanup.go:50
mcp-local-hub/internal/api.parseAggressiveCandidates(...)
	D:/dev/mcphub-front-daemon/internal/api/cleanup.go:1474
mcp-local-hub/internal/api.(*API).AggressiveCleanup(...)
	D:/dev/mcphub-front-daemon/internal/api/cleanup.go:1599
```

**Confirmed pre-existing, unrelated to the change in progress:** reproduced
identically with `git stash -u` (every tracked AND untracked change from the
Increment 1b work removed, back to the accepted branch tip `f5f1eb8a`) —
`go test ./internal/cli/... -run TestCleanupAggressive_IncludeClassFlagOverridesWithWarning -count=3`
panics the same way. This is exactly the "full internal/cli sweep is
flaky/crashy on this host" behavior CLAUDE.md's PR workflow section already
warns about.

## Root cause (read, not yet fixed)

`internal/api/cleanup.go:312-342`:

```go
func findWindowsExeExtensionEnd(s string) int {
	lower := strings.ToLower(s)
	...
	idx := strings.Index(lower[searchFrom:], ext)
	...
	abs := searchFrom + idx
	end := abs + len(ext)
	if end == len(s) || s[end] == ' ' || ...   // <-- indexes into s, not lower
```

`end` is an offset computed against `lower` (the lower-cased copy), but the
boundary check indexes into `s` (the ORIGINAL string) at that same offset.
`strings.ToLower` is NOT guaranteed to preserve UTF-8 byte length — some
non-ASCII code points expand or contract when case-folded (e.g. Turkish
dotted/dotless I variants, several Cyrillic/Greek/Georgian pairs). When a
real process's command line (this test calls the REAL, unstubbed
`AggressiveCleanup`, enumerating actual live processes on the host) contains
such a character before the matched extension, `lower` becomes longer than
`s`, `end` can exceed `len(s)`, and `s[end]` panics
(`index out of range [85] with length 67` — `lower` was 18 bytes longer than
`s` here).

This is a real, reachable defect in production code (`AggressiveCleanup` is a
real operator-facing command, `mcphub cleanup aggressive`), not merely a test
artifact — the test happens to trigger it because it runs the unstubbed
function against this host's actual process list, and something in that
process list carries a case-expanding Unicode byte in its command line.

## Suggested fix direction (not implemented — out of scope for the change in progress)

`findWindowsExeExtensionEnd`'s boundary check must index into the SAME string
whose length the offset was computed against. Two candidate fixes:

1. Compute `end` against `lower` throughout, and boundary-check
   `lower[end]` instead of `s[end]` — but then the returned index is only
   valid as an offset into `lower`, not `s`, which may break callers that
   slice `s` with the returned value (needs a careful read of
   `firstTokenBasename` and every other caller before changing the contract).
2. Avoid the byte-length mismatch entirely by doing the extension search
   ASCII-case-insensitively without `strings.ToLower` allocating a
   variable-length copy — e.g. a custom case-insensitive `Index` that compares
   byte-by-byte with ASCII-only case folding (`'A'-'Z'` vs `'a'-'z'`), which
   preserves length invariance since Windows executable extensions and the
   whitespace/EOL boundary set are all ASCII. This seems like the more robust
   fix: it sidesteps the Unicode case-folding class entirely rather than
   reconciling two different-length strings' offsets.

Either fix needs its own verified hypothesis + regression test (a
command-line string containing a case-expanding Unicode character before a
recognized Windows exe extension) per this repo's pre-fix diagnostic gate —
not bundled into an unrelated change.

## Why not fixed here

Out of the approved change surface for Increment 1b (supervisor auto-spawn of
`mcphub route`) — `internal/api/cleanup.go` and the `mcphub cleanup
aggressive` command are an unrelated subsystem. Filed per the adjacent-findings
protocol instead of expanding scope.
