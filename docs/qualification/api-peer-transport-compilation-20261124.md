# API peer transport test compilation — 24 November 2026

## Scope and correction

At exact #665 head `2872ca12e5355be0b466377f88bf51e838c5ecdf`, `startPeerTransportChild` calls `process.NoConsole(cmd)` without importing `mcp-local-hub/internal/process`. Windows application programming interface (API) tests therefore fail type checking before any selected test can execute. Add the missing import. Preserve the existing console policy, child arguments, original-process ownership and settlement order.

## Execution evidence

The exact test composition starts from issued Source `4fcd57a6d236276bcf362aefaf0baf5233d68555`, tree `fb733ed7c5bf6e5f62d92068b4762f0805626442`, with this import and the independently owned #637 correction of unavailable test interfaces. Its complete inventory contains 3,398 files and 51,467,747 bytes; inventory SHA-256 is `2650f1d0fbefc18a6a80d5eee82365f297277ccc651e63500c6a243ea5ef110f`.

**Table 1. Executed checks on that exact repaired inventory.**

| Check | Actual result |
|---|---|
| `gofmt -l` on both changed test files | Exit 0; empty output |
| `go test -tags=test_state_path_env -c -ldflags=-H=windowsgui -o <isolated-output> ./internal/api` | Exit 0; compile only |
| Same compile-only command for `./internal/cli` | Exit 0; compile only |
| `go vet ./...` | Exit 0; empty standard error |
| Complete source inventory before/after | Identical |

The successful raw command records remain in `source-compile-repair-v2-20261124-57604a3a`. The original undefined-import failure is retained in `current-source-build-v1-20261124-57604a3a`. This evidence belongs to the declared composition; it does not assert standalone acceptance of the new PR head or successful native peer execution.

## Remaining admission

Full standard/tagged tests and native transport scenarios still require their declared environment admission and actual execution. External review must target the new exact commit. No installed state, Task Scheduler operation or execution policy was changed by this repair.
