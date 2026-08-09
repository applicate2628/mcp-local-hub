# Bug: host-scoped `internal/cli` package run cascades into supervisor failures

- id: 2026-07-25-internal-cli-full-package-run-cascading-failures-this-host
- context: adjacent-finding
- status: open
- severity: medium
- area: `internal/cli` supervisor, reallocation, and maintenance tests that spawn a real `supervise` subprocess
- found-by: platform-engineer during console/tray verification

## Reproduction

The preserved report used this full-package command after skipping the separate
open `AggressiveCleanup` panic:

```text
go test -tags=test_state_path_env -count=1 -timeout 8m -skip TestCleanupAggressive_IncludeClassFlagOverridesWithWarning ./internal/cli/
```

The test-spawned supervisor resolved its managed executable to a missing
absolute path, emitted the `missing-binary` reason, and caused unrelated
lost-child and reallocation assertions to observe no spawn or respawn. The
private machine path and raw supervisor log are intentionally not retained.

The fresh console-tray candidate's unfiltered `go test -count=1 -timeout 5m
./internal/cli` run also failed in the same missing-binary class, alongside
environmental DACL gate failures. That check did not establish a root cause.

## Impact

The package gate cannot provide a clean signal for unrelated changes on this
host while the supervisor test harness resolves an invalid executable path.

## Status and next step

This remains an open, host-scoped test-infrastructure finding; it is not fixed
or attributed to the console-tray test. A dedicated investigation should trace
the test fixture that supplies the supervised-binary path and reproduce the
failure with sanitized fixture data before changing the harness or production
supervisor logic.

## Terms and Abbreviations

- **DACL** — discretionary access control list.
