---
context: adjacent-finding
status: open
---

# `findWindowsExeExtensionEnd` panics: index out of range when `strings.ToLower` changes byte length

Discovered as an adjacent finding while running the full gate
(`go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/`)
for the sub-increment 2a F1/F2 adversarial-gate fix
(`work-items/active/2026-07-25-mcp-front-daemon/`). Unrelated to that
work item's change surface (`internal/api/scan.go`,
`internal/api/serena_client_reconcile.go`,
`internal/api/lsp_client_router.go`,
`internal/cli/install_reconcile_mcp_front.go`) — this bug lives entirely
in `internal/api/cleanup.go`, which sub-increment 2a does not touch.

## Symptom

`go test -tags=test_state_path_env -run TestCleanupAggressive_IncludeClassFlagOverridesWithWarning -v ./internal/cli/`
panics (not a normal test failure — a real Go runtime panic that aborts
the whole `internal/cli` package test binary):

```text
panic: runtime error: index out of range [85] with length 67 [recovered, repanicked]
...
mcp-local-hub/internal/api.findWindowsExeExtensionEnd({0x..., 0x43})
	D:/dev/mcphub-front-daemon/internal/api/cleanup.go:332 +0x1a7
mcp-local-hub/internal/api.firstTokenBasename(...)
	D:/dev/mcphub-front-daemon/internal/api/cleanup.go:229 +0x145
mcp-local-hub/internal/api.isOurOwnProcess(...)
	D:/dev/mcphub-front-daemon/internal/api/cleanup.go:50 +0x1c
mcp-local-hub/internal/api.parseAggressiveCandidates(...)
	D:/dev/mcphub-front-daemon/internal/api/cleanup.go:1474 +0x145
mcp-local-hub/internal/api.(*API).AggressiveCleanup(...)
	D:/dev/mcphub-front-daemon/internal/api/cleanup.go:1599 +0x19b
mcp-local-hub/internal/cli.newCleanupAggressiveCmdReal.func1(...)
	D:/dev/mcphub-front-daemon/internal/cli/cleanup_aggressive.go:96 +0x2aa
```

Length `67` is `len(s)` (the ORIGINAL command-line string); index `85`
is `end` — an offset computed against `lower := strings.ToLower(s)`
that no longer lines up with `s` once `lower` is used to index back
into `s`.

## Root cause (read, not yet independently instrumented beyond the two
reproductions below)

`findWindowsExeExtensionEnd` (`internal/api/cleanup.go:312-342`):

```go
func findWindowsExeExtensionEnd(s string) int {
	lower := strings.ToLower(s)
	...
	idx := strings.Index(lower[searchFrom:], ext)
	...
	abs := searchFrom + idx
	end := abs + len(ext)
	if end == len(s) || s[end] == ' ' || ... {
```

`abs`/`idx`/`end` are all computed against byte offsets in `lower`,
then `end` is used to index directly into `s` (`s[end]`) and compared
against `len(s)`. `strings.ToLower` on a UTF-8 string can change the
BYTE length of the result relative to the input whenever a rune's
lowercase mapping has a different UTF-8 encoding length than the
original (this is a real, documented Unicode case-folding property,
not exotic — e.g. certain Cyrillic/Greek/Turkish code points expand or
contract by 1-2 bytes under case folding). Whatever process command
line this test's `AggressiveCleanup`/`parseAggressiveCandidates` walk
encountered on this host contains at least one such character, making
`len(lower) != len(s)` and producing an `end` that is a valid offset
into `lower` but out of range for `s`.

## Reproduction (verified twice, once as originally hit and once
independently confirmed against the accepted branch tip)

```bash
go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/
```

fails with the panic above in `internal/cli`. Re-verified in isolation:

```bash
go test -tags=test_state_path_env -count=1 -run 'TestCleanupAggressive_IncludeClassFlagOverridesWithWarning' -v ./internal/cli/
```

**Confirmed pre-existing and independent of the sub-increment 2a
F1/F2 changes**: reproduced identically (`index out of range [85] with
length 67`, byte-for-byte same panic signature) after `git stash -u`
back to the accepted branch tip commit `16c34eba` (before any F1/F2
edits), then reproduced again after `git stash pop` restored the F1/F2
working tree. Neither state touches `internal/api/cleanup.go` or
`internal/cli/cleanup_aggressive.go`/`cleanup_aggressive_test.go`.

The test itself does not appear to construct a deliberately
Unicode-cased fixture (`cleanup_aggressive_test.go:66` calls into
`AggressiveCleanup`, which enumerates real live processes on this host
via `parseAggressiveCandidates`) — the panic depends on the ACTUAL
process list / command lines present on the machine the test runs on,
which is why this reads as host-environment-dependent rather than a
fixed, deterministic test failure. This matches the project's own
"flake = real bug, root-cause, don't dismiss" standing lesson — it is
not being dismissed here, just filed as out of scope for this work
item per the adjacent-findings protocol.

## Suggested fix direction (not applied — out of scope for this item)

`findWindowsExeExtensionEnd` needs to search `s` directly with a
case-insensitive comparison (e.g. `strings.EqualFold` per-candidate-window,
or use `strings.ToLower` on individual small substrings/runes rather than
the whole string, or track a rune-index mapping) instead of computing
offsets against a separately-lowercased copy and indexing back into the
original. Any fix should be verified with a targeted unit test feeding a
command-line string containing a rune whose lowercase mapping changes
UTF-8 byte length, plus the existing `windowsExeExtensions` boundary
cases already covered in this file's history (PR #143 rounds 1-4).

## Impact

`isOurOwnProcess` / `firstTokenBasename` / `parseAggressiveCandidates`
are on the `mcphub cleanup --aggressive` path (process-list classification
during aggressive cleanup). A panic here means `mcphub cleanup --aggressive`
can crash outright on a host where some enumerated process's command line
contains a Unicode character that expands/contracts under case-folding,
rather than gracefully classifying or skipping that entry.
