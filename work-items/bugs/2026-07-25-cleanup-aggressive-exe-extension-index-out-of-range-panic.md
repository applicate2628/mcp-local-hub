# Bug: `AggressiveCleanup` can panic while parsing a live process command line

- id: 2026-07-25-cleanup-aggressive-exe-extension-index-out-of-range-panic
- context: adjacent-finding
- status: open
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

## Status and next step

This is a preserved open finding, not a claim that the current hosted master
was re-reproduced by the console-tray candidate. A dedicated investigation
should isolate a deterministic synthetic input, trace every caller's index
contract, and add a regression test before changing production logic.

## Terms and Abbreviations

- **DACL** — discretionary access control list.
