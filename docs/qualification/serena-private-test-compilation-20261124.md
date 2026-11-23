# Private Serena test compilation — 24 November 2026

## Scope and correction

The exact #637 head `1f241a872a23cc529195fcf060a5c66e985b5d07` introduced references in `internal/cli/private_serena_migration_case_windows_test.go` to three unavailable interfaces: `api.PreparedSerenaFailureDiagnostic`, `api.PreparedSerenaFailureDiagnostics`, and `buildinfo.CurrentRustOwnerSHA256` with `ErrRustOwnerBuildUnmarked`. The branch's complete `internal/buildinfo` package contains only `buildinfo.go`; it provides the publisher Source observation, without the referenced Rust marker interface. These dangling references prevent Windows command-line interface (CLI) test compilation.

Remove the unavailable diagnostic projection and the two unavailable Rust-marker checks. The original failure cause and original custody remain retained in `privateSerenaMigrationRetainedCustody`. The abort frame retains its schema, stage, bounded error message and custody indication. No unjoined output sink is read or closed by this correction. Existing checks of the actual Source epoch, executable identity and Windows artifact role remain, including refusal to admit a private test publisher as Product. No production API or empty replacement interface is introduced.

## Execution evidence

The reproducible test composition starts from issued Source `4fcd57a6d236276bcf362aefaf0baf5233d68555`, tree `fb733ed7c5bf6e5f62d92068b4762f0805626442`. It applies this correction and the independently owned #665 missing `process` import. The final complete inventory contains 3,398 files and 51,467,747 bytes, with inventory SHA-256 `2650f1d0fbefc18a6a80d5eee82365f297277ccc651e63500c6a243ea5ef110f`. This is composition evidence, not standalone acceptance of this PR head.

**Table 1. Executed checks on the exact repaired inventory.**

| Check | Actual result |
|---|---|
| `gofmt -l` on the two changed test files | Exit 0; empty output |
| `go test -tags=test_state_path_env -c -ldflags=-H=windowsgui -o <isolated-output> ./internal/api` | Exit 0; no test execution |
| Same compile-only command for `./internal/cli` | Exit 0; no test execution |
| `go vet ./...` | Exit 0; empty standard error |
| Complete source inventory before/after | Identical |

Raw command records remain in phase `source-compile-repair-v2-20261124-57604a3a`. Earlier failures are retained: `current-source-build-v1-20261124-57604a3a` exposed the missing diagnostic API and missing #665 import; `source-compile-repair-v1-20261124-57604a3a` then exposed the missing Rust marker interface. Neither failure is reclassified as a pass.

## Remaining admission

Full standard/tagged test execution, private Serena migration, installed Product lifecycle, a real Go/Rust consumer boundary, and external review of the new exact commit remain unqualified. Native namespace admission is separately blocked at the installed PowerShell ancestor. This correction grants no new runtime permission and does not close those gates.
