# W10 Backend Correction

Date: 2026-08-13

Execution role: `$backend-engineer`

Bound candidate under correction: `bab886092ae0a4148c05f1e057eeedd73731eedf`

Receiving W10 artifact SHA-256: `961DD6339AC30C5076352071083591D3F28D2F741972ED687B6C048E47B9FD6B`

Bug record SHA-256: `47FB775099E12B684E55EEDD3B410300A4F9153F7B78DC3FE307AC8A1A9A8A7C`

## Diagnostic gate

| Blocker | Verified root cause | Falsifying probe |
|---|---|---|
| Exact-candidate vet | `internal/api/netsh_no_console_windows_test.go` calls `newExcludedPortNetshCommand`; its coherent production owner is the pre-existing live-worktree change in `internal/api/port_alloc_excluded_windows.go`, originating from `b87dc8dd`. The 66-path candidate omitted that owner. | Candidate archive vet reports undefined symbol; current worktree with the owner runs `go vet ./...` successfully. |
| Supervise TempDir race | `runSupervise` cancelled the event loop but returned without joining it. The initial reconcile only enqueues work; the event-loop goroutine owns dispatch of the injected blocking spawn. | Deterministic RED held the dispatch in `SpawnFunc` and observed `runSupervise` return early. Joining only the initial reconcile goroutine remained RED, disproving that narrower hypothesis. |
| Package roots | CLI/GUI `TestMain` discarded `os.RemoveAll` errors. More importantly, re-exec children created a package root and then called `os.Exit`, panicked, or were killed, so the normal post-`m.Run` cleanup was unreachable. CLI production child argv `supervise` also caused the generated test main to recursively run the package. | Fresh full GUI created one root before correction. Unified CLI helper RED created one root for each of exit-code, stale-release, terminate, PID-match and `supervise` protocols. Full CLI initially created six roots, then two until the `supervise` argv owner was included. |

## Correction

- `runSupervise` now cancels and joins its owned event-loop goroutine before returning.
- CLI and GUI `TestMain` use one shared bounded exact-root remover that verifies absence and surfaces terminal failure instead of discarding it.
- Re-exec-only helper protocols are dispatched before package-root creation. CLI production `route` and `supervise` child argv cannot recursively execute the test suite.
- Deterministic tests cover the supervise dispatch window, all CLI re-exec protocols, GUI audit-lock child lifetime, and the cleanup helper's success/failure contract.
- The admitted netsh production owner is included unchanged from its verified live-worktree implementation; it was not recreated or copied into a second owner.

## Fresh verification

| Check | Result |
|---|---|
| `go vet ./...` | PASS, exit 0. |
| Supervise dispatch + original two cleanup tests, `-count=20` | PASS, 80 executions. |
| Shared cleanup helper, `-count=20` | PASS, 60 executions. |
| Unified five-protocol CLI helper guard, `-count=10` | PASS, 50 executions. |
| Three force scenarios, `-count=10` | PASS, 30 executions; zero fresh CLI roots. |
| Full `go test ./internal/gui -count=1` | PASS in 57.027 s; earlier instrumented fresh run also produced zero GUI roots. |
| Final full `go test ./internal/cli -count=1 -json` | Expected FAIL in 345.484 s: exactly the seven accepted Windows upgrade/review/staging guards; zero fresh CLI roots. |
| Final full `go test ./... -count=1 -json` | Expected FAIL in 352.1 s: exactly nine accepted failures (two routing capture-before-release guards plus the seven CLI guards); zero fresh CLI/GUI roots. |
| Python `pytest -q` | PASS, 635/635. |
| Native verifier `verify.ps1 -Unsigned` | PASS; image SHA-256 `38D87C50F716E334F89628D4F35604534C3A35BDCF35F378F1D939137BAB89E3`. |
| Go CST direct frontend route | PASS. |
| `git diff --check` | PASS. |
| Git index | Empty. |

Pre-existing roots were retained as diagnostic evidence and were not manually deleted or counted as the fix oracle. No live service, CST, App Control, virtual disk, fleet, Git index, commit, push, publication or deployment state was mutated.

## Reassembly allowlist

| Path | SHA-256 |
|---|---|
| `internal/api/port_alloc_excluded_windows.go` | `6E0E1BDA63061B99ECD0D5C503908BD2F6FCB20ADF6D290CDB53A30E823ECEEB` |
| `internal/api/apitest/testmain_cleanup.go` | `EAD170C2F4BB3E92940406B78162D97ED2306B99DE191DA40B874528AFADBF81` |
| `internal/api/apitest/testmain_cleanup_test.go` | `A99A13A3A902BF2EDCD43B939139424D89C0561F19823116B2EFA71C14DF3F67` |
| `internal/cli/settings_registry_test.go` | `F360FBA9D7B0623A2C7EC279E1293DFB0939A5D6D8C230CE9C03194D72A1202C` |
| `internal/cli/supervise.go` | `87861C8DD0E557C423F37035D7C5AB18666110CD0B1481C45FFFA4BEAC799150` |
| `internal/cli/supervise_reconcile_wiring_test.go` | `8C76E30F081E26BAFC2D51A6F18EEE497C0D7561D9964276FF189004C3901D4E` |
| `internal/cli/testmain_helpers_test.go` | `DCCBFC4585AE7CEDF335A8584EB40ABA5B6137D8C80A78229E1B777DB67D1B2D` |
| `internal/gui/audit_lock_terminal_worker_test.go` | `DAC206C6C49E76BCAD8F78C705210CC43EA6B5CB664D547D2743FB27C5E10282` |
| `internal/gui/main_test.go` | `3DF69DCB47C6333F1FA961768057657B4F1B206ED268818793F134905709D5A8` |

## Gate

`PASS`

## Terms and Abbreviations

- CLI: command-line interface.
- CST: Computer Simulation Technology electromagnetic solver.
- GUI: graphical user interface.
- RED/GREEN: failing-then-passing test-driven-development evidence.
- W10: independent local working-result acceptance phase.
