# Read-only review: Go inventory at the published #658 commit

## Contents

1. [Source and purpose](#source-and-purpose)
2. [Execution boundary](#execution-boundary)
3. [Initialization and environment](#initialization-and-environment)
4. [Prerequisites and commands](#prerequisites-and-commands)
5. [Known limits](#known-limits)

## Source and purpose

The reviewed source is commit `f12932bcca6c288d062f9affd32ea58a7c9eb74f`, tree `79a36a7aca62cd2c08cb6a1a79a1f7d58ad6010c`, in `applicate2628/mcp-local-hub`. The published source is read independently of the older Go budget logs, whose source was `f755d374fab317e1037deecd5dac8ffabbcdde36` plus a change to `internal/cli/gui_force_test.go`.

The purpose is to obtain fresh compiled test inventories for the application programming interface (API) and command-line interface (CLI), with and without the `test_state_path_env` build tag. A ROOT is a top-level test, example, or fuzz-test name; nested subtests are not enumerated by `go test -list`.

The six reviewed files were fetched from the exact commit and their Git blob identifiers recomputed. `source-options/reviewed-source-manifest.json` retains the identifiers and SHA-256 checksums. Primary source: [tools/testshards/main.go](https://github.com/applicate2628/mcp-local-hub/blob/f12932bcca6c288d062f9affd32ea58a7c9eb74f/tools/testshards/main.go).

## Execution boundary

`testshards` accepts only `./internal/api` or `./internal/cli`, requires absolute source/evidence/Go paths and explicit `GOROOT`, `GOCACHE`, and `GOMODCACHE`, and rejects a dirty source tree or an existing evidence directory. The evidence directory must be outside the source checkout.

Its plan path invokes `go test -list . -p=1`, optionally with the requested build tag. This compiles the selected package's test binary, executes package initialization and `TestMain`, and lists compiled ROOT names. It does not execute test, example, fuzz, or benchmark bodies. The tool parses and deduplicates names, requires exactly one `TestMain` declaration, partitions the names into groups, checks source cleanliness again, and writes `inventory.txt` and `plan.json`.

The `-execute=false` option is explicit in the controller. No shard selector or execution command is requested. The execution-only branch uses different commands and is not reached. Plans retain `mandatoryUnshardedGateUnchanged=true` and `equivalentToSingleProcess=false`; listing or partitioning does not qualify the full five-minute test gate.

## Initialization and environment

The existing four test entry-point guards were reviewed: [API](https://github.com/applicate2628/mcp-local-hub/blob/f12932bcca6c288d062f9affd32ea58a7c9eb74f/internal/api/main_test.go), [CLI](https://github.com/applicate2628/mcp-local-hub/blob/f12932bcca6c288d062f9affd32ea58a7c9eb74f/internal/cli/settings_registry_test.go), [daemon](https://github.com/applicate2628/mcp-local-hub/blob/f12932bcca6c288d062f9affd32ea58a7c9eb74f/internal/daemon/main_test.go), and [graphical user interface (GUI)](https://github.com/applicate2628/mcp-local-hub/blob/f12932bcca6c288d062f9affd32ea58a7c9eb74f/internal/gui/main_test.go). Only the API and CLI test packages are selected for inventory.

API and CLI helper-dispatch branches occur before normal test isolation. Therefore inheriting arbitrary `MCPHUB_*` variables is not acceptable. The controller creates an explicit environment allowlist: a small set of Windows platform variables, exact existing tool paths and cache paths, offline Go settings, and only four explicit application suppression/isolation variables. No inherited helper variable or credential is passed through.

Normal API and CLI initialization creates owned temporary state, applies test state overrides and client sandboxes, substitutes process-launch/reconciliation functions, isolates interprocess communication (IPC), and restores and cleans up after `m.Run`. All selected temporary, profile, cache and evidence paths are on drive D. Existing compiler and operating-system files on drive C are read as installed tools.

The original [RemoveTestMainRoot](https://github.com/applicate2628/mcp-local-hub/blob/f12932bcca6c288d062f9affd32ea58a7c9eb74f/internal/api/apitest/testmain_cleanup.go) uses `os.RemoveAll` on the exact temporary root it owns, with bounded retries. This existing initialization/cleanup behavior was explicitly reviewed and accepted before the inventory run. The controller itself preserves source, receipts, streams, and evidence and does not perform cleanup.

## Prerequisites and commands

The controller is `run_inventory.py`, SHA-256 `4b6404c2218aef6f08c8dbcbcf77eba1eea84d453540c984fba648594c1225dd`, 14,574 bytes. It must reside in `D:\dev\ChatGPT\mcp-r23-go-inventory-20261121` and run on Windows with the existing Python installation, using `-I -B` (isolated configuration and no bytecode cache writes).

It checks the existing Git, Go, GNU Compiler Collection (GCC), Go root directory, build cache and module cache. The existing Go and GCC executable checksums are pinned. `prepare` fetches only the specified public commit into a fresh bare Git store and creates a detached worktree; it creates no branch ref and integrates no branch. It records the exact source commit, tree, clean status, all 3,216 file hashes, and their aggregate checksum.

After a successful preparation receipt, `inventory` checks the same source again, requires `go1.27.4 windows/amd64`, and builds only the existing `./tools/testshards` program with `-mod=readonly -p=1 -trimpath`. It runs these four independent plan selections sequentially:

| Selection | Package | Build tag |
|---|---|---|
| `api-normal` | `./internal/api` | none |
| `cli-normal` | `./internal/cli` | none |
| `api-tagged` | `./internal/api` | `test_state_path_env` |
| `cli-tagged` | `./internal/cli` | `test_state_path_env` |

Every selection passes absolute `-repo`, `-evidence`, and `-go` paths, `-max-roots 80`, and `-execute=false`. The evidence path is new for each selection. `GOPROXY=off`, `GOSUMDB=off`, `GOVCS=*:off`, `GOTOOLCHAIN=local`, `GOWORK=off`, `GOENV=off`, and `GOTELEMETRY=off` disable module/toolchain fetching and stored Go configuration. A missing prerequisite is a blocker; no installation or network fallback is allowed.

## Known limits

The tool clears `GOFLAGS` for its `go test -list` child. Thus the explicit `-mod=readonly` option applies to building `testshards`; it is not passed through to the listing child. Network settings still apply. The source commit, tree, clean status and complete tracked-file hashes are checked after all selected commands, including failure paths.

The original tool has no inventory deadline. The controller retains a direct process handle and applies a 300-second deadline to each top-level build or inventory command. It can terminate and settle that direct process, but it does not prove a deadline or settlement for the whole descendant process tree. An unsuccessful phase is preserved and stops subsequent work; it is not retried blindly.

Compiled inventory provides qualified names and counts, not durations of successful tests, nested subtest counts, performance measurements, or full-gate success. The historical full/tagged streams contain no Go JSON event trace, and fresh lists do not reconstruct missing historical timing events.
