# Bug: `AggressiveCleanup` can panic while parsing a live process command line

- id: 2026-07-25-cleanup-aggressive-exe-extension-index-out-of-range-panic
- context: adjacent-finding
- status: fixed
- severity: high
- area: `internal/api/cleanup.go` (`findWindowsExeExtensionEnd`, `firstTokenBasename`, `isOurOwnProcess`, `parseAggressiveCandidates`)
- found-by: platform-engineer during console/tray verification

## Reproduction

The preserved report observed this host-scoped failure while running:

```text
go test -tags=test_state_path_env -count=1 -run TestCleanupAggressive_IncludeClassFlagOverridesWithWarning -v ./internal/cli/
```

The test binary panicked with `runtime error: index out of range` from the
`findWindowsExeExtensionEnd` call path. The failing path consumes live process
command-line data, so the report did not retain the triggering process command
line or any machine-specific file paths.

## Impact

The failure aborts the `internal/cli` package test binary. The same parser is
reachable from the operator-facing `mcphub cleanup aggressive` command, so an
input that triggers the faulty index calculation can terminate that command.

## Resolution

Commit `0357794596674c3f224d7b083ec1de9341b8e7cb` fixed the byte-offset-space
defect by searching the original bytes rather than a transformed string. The
regression oracles are `internal/api/cleanup_test.go:568-616`. PR #592 needs
no further cleanup production-code change.

## Terms and Abbreviations

- **DACL** — discretionary access control list.
