# Relay binary probe: rejection of corrupt output

**18 November 2026, Windows/amd64.** This report qualifies a correction to the measurement oracle in draft [#626](https://github.com/applicate2628/mcp-local-hub/pull/626). It does not change the production Relay or establish a performance improvement. The original source is `06d8706a446b456642a13f23cfa58e10c6e3c28f`.

## Contents

1. [Defect and correction](#defect-and-correction)
2. [Native controls and results](#native-controls-and-results)
3. [Source and evidence identities](#source-and-evidence-identities)
4. [Reproduction](#reproduction)
5. [Scope and remaining gates](#scope-and-remaining-gates)

## Defect and correction

The original `TestRXRelayBinaryProbe` consumes one initialize response and sixteen call responses per child. Its stdout reader has a four-element buffered channel. After closing input, the test waits for the reader and child but does not inspect buffered responses left after the last expected call. A duplicate final response can therefore be accepted and a measurement file issued. The initialize and ordinary-response predicates also omit the `jsonrpc` version. Correct IDs, sessions and payloads with `"jsonrpc":"1.0"` are accepted.

The correction requires `JSONRPC == "2.0"` in both existing predicates and checks that the response channel is empty and closed before issuing measurements. JSON-RPC means JavaScript Object Notation Remote Procedure Call; this fixture expects version 2.0. The remaining ID/session/payload checks are retained.

The trailing-response check occurs **after** receiving a successful reader result, `cmd.Wait()`, `joined = true`, and the child-error checks. The ordinary channel receive waits for the reader's immediately following close. A nonblocking receive with a default branch would race that close. Rejecting a tail before marking the child joined would make failure cleanup consume the already-received reader result again. Both conditions were examined in self-review and independent review.

## Native controls and results

The finite [control executable](testdata/main.go) calls the same unchanged repository `HTTPToStdioRelay.Run`. Its wrapper changes only the visible output: it either preserves it, duplicates the final response after Relay returns, changes the initialize version, or changes the first ordinary-response version. It accepts only an explicit mode and a numeric loopback HTTP endpoint. Each invocation has one owned child and a normal workload of sixteen sequential calls. With the correction, the two version controls stop earlier, at initialize or the first ordinary response. No external service, installation or model execution is involved.

**Table 1. Actual native outcomes with the same control executable.** A rejection is a deliberately failing Go test, not a successful test counted as PASS.

| Output | Original probe | Corrected probe | Measurement file with correction |
|---|---|---|---|
| Unchanged | PASS, exit 0 | PASS, exit 0 | Written |
| Duplicate final response | Incorrect PASS, exit 0 | Expected FAIL, exit 1; `unexpected trailing relay response` | Absent |
| Initialize version 1.0 | Incorrect PASS, exit 0 | Expected FAIL, exit 1; `bad init` | Absent |
| Ordinary response version 1.0 | Incorrect PASS, exit 0 | Expected FAIL, exit 1; `wrong binary relay response` | Absent |

All eight invocations used `go test -json -race -count=1 -p=1 -timeout=90s -run=^TestRXRelayBinaryProbe$/^processes_1$ ./internal/daemon`, installed Go **1.27.4**, existing local caches and disabled dependency downloads. The parent test and HTTP fixture were built with the Go data-race detector; the unchanged control executable itself was built with ordinary `go build` and was reused for both phases. No child race-instrumentation claim is made.

Every negative control has exactly the expected failing subtest, parent test and package outcome, exactly one expected diagnostic, no additional output, no skipped test, empty standard error, and no measurement file. A panic, race report or additional cleanup error cannot satisfy this check. The raw Go JSON logs and command checksums are embedded in [receipt.json](receipt.json); their exact bytes were verified against the original SHA-256 checksums after transfer.

The first fixed-phase **log classifier** falsely rejected the long trailing-response diagnostic because `test2json` split one line between two `Output` events. The final [runner](run.py) concatenates output fragments before splitting lines. Both phases' existing logs were reclassified with their original checksums; Go tests were not repeated for this classifier correction. The initial fixed summary and corrected summaries are both retained in the receipt, so the initial classifier failure is visible.

**Table 2. Additional checks on the corrected source.**

| Command / scope | Result |
|---|---|
| `gofmt -l` on the changed probe and control source | Exit 0, empty output |
| `go vet ./internal/daemon` | PASS, exit 0 |
| `go test -json -race -tags=test_state_path_env -count=1 -p=1 -timeout=90s -run=^TestRelayLoad(Contract\|HTTPErrorPreservesID\|OracleRejectsCorruption\|EOFDrainsFinalResponse)$ ./internal/daemon` | PASS, exit 0; four top-level tests, including six subcases of `TestRelayLoadContract`; no skips |
| `git diff --check` | PASS, exit 0 |

The supplementary receipt includes full commands, output and checksums. All six supplementary commands, including status and diff inspection, returned 0 with empty standard error. The focused phase was followed by the complete mandatory commands on the clean published commit, as recorded below.

## Source and evidence identities

**Table 3. SHA-256 identities.** These bind the actual tested bytes; they are not substitutes for native behavior.

| Artifact | SHA-256 |
|---|---|
| Original probe source | `e29a175eb9493ee4af7612d162e270e83ee0830c111464a34bb305eae023a724` |
| Corrected probe source | `6de4d1554dd967402e7fbfb14d2e97a9ebf65f87b6ef31e5676601e1c2a87684` |
| Control source | `640cda49da31b88cbc19547e67d346b8f09e827a360f6e89442853f809601121` |
| Same control executable in both phases | `1a7cf244e089bddc0c2582a1022aadd24bd5e8250e9fe676a48b5f0d398433a6` |

The final runner hash and each raw-log hash are in the receipt. Only the existing probe changes executable Go test behavior: two predicates and one three-line trailing-response check. The additional source, runner and receipt reproduce and document that same question. No dependency or production source changes.

## Reproduction

Use an independent owned checkout of the exact original commit and already installed Python, Go and the C compiler. Keep the work directory strictly under that checkout's `.scratch`, copy `testdata/main.go` to `<work>/fixture/main.go`, and use existing Go module/build caches. All paths are explicit; missing local dependencies are a blocker, not permission to install or download.

```text
python <report-directory>/run.py baseline --source <owned-checkout> --work <owned-checkout>/.scratch/<unique-run> --module-cache <existing-module-cache> --build-cache <existing-build-cache> --goroot <installed-go-root> --go <installed-go-executable> --cc <installed-C-compiler>
```

The baseline invocation builds the owned control once. With all other repository sources unchanged, replace only `internal/daemon/relay_binary_probe_windows_test.go` by this correction, then run the same command with `fixed` in place of `baseline`. Each phase refuses to reuse an existing result directory. `--check-existing` instead verifies retained log hashes and reclassifies them without repeating builds or tests; it still checks the installed Go version and unchanged control identity.

The runner sets `GOTOOLCHAIN=local`, `GOPROXY=off`, `GOSUMDB=off`, `GOWORK=off`, explicit caches, and owned temporary/profile directories. Preserve evidence outside trash. The original machine execution used `D:\dev\ChatGPT\mcp-local-hub-pr626-audit-20261118`; concrete command paths are retained in the receipt. They are provenance, not portable defaults.

## Scope and remaining gates

This proves that the original oracle accepts these three actual corrupt outputs and the correction distinguishes them from the correct output. It is not evidence that a production Relay emitted those corrupt outputs. The control runs are correctness experiments, not replacement latency or memory samples.

Existing #626 working-set sums can count shared pages more than once. They are not unique physical memory or peak usage. Closed-loop samples from sixteen sequential calls per child do not qualify open-loop tail latency or a service-level objective. The optional private-memory probe and full-versus-minimal Go executables remain separate measurement questions; no Rust necessity follows from these controls.

### Complete commands on the published correction

Both mandatory commands were executed on clean `a00cbd013348163526b2ec3cd213f27a2c126c70`, from 18:12:00 to 18:24:27 UTC on 18 November (21:12–21:24 Moscow). The later documentation/evidence commit does not change the tested Go source. The [receipt](receipt.json) records all command times, exact arguments, exit codes, checksums, package results, failed-test names, timeout observations and the runner source. Full raw logs remain in the owned evidence directory named there; no complete-test absence of skips is claimed from non-verbose output.

**Table 4. Full-repository command results on exact `a00cbd0`.**

| Command | Result |
|---|---|
| `go build ./...` | PASS, exit 0; empty stdout/stderr |
| `go vet ./...` | PASS, exit 0; empty stdout/stderr |
| `go test -count=1 -timeout 5m ./...` | FAIL, exit 1; eight failing packages, including API and CLI package timeouts at 300 seconds |
| `go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/` | FAIL, exit 1; both packages reach their 300-second timeout; recorded earlier test failures are retained |

The first required build/vet/test chain therefore fails overall, and the second required command also fails. Both test standard-error files are empty; diagnostics are in standard output. Plain-suite stdout SHA-256 is `a190a8d80e621e22c4d262ebd2515ea0ad7664697db06679db167fd0f1c4491d`; tagged stdout is `2603983a1fbf4f4cbcabcd5b86ea4b0f25110750b3d2866a2cec759959302bb0`.

The recorded context includes `MCPHUB_NO_AUTO_GUI=1`, `MCPHUB_E2E_SCHEDULER=none`, owned profile/temporary roots, existing offline caches and no qualified installed Product/source fixture. Failures include entry-mode expectations under the no-auto-GUI setting, directory access-control refusals, unavailable qualified entry/Product inputs and the total package time budgets. These runs establish failure in that context; they do not establish that the probe correction introduced those production defects. Active test names at a package timeout do not by themselves prove those individual tests deadlocked. Limits, access controls and required native inputs were not weakened to obtain a green result.

The checkout remained clean at the exact tested commit. A post-run process query found no executable or command line associated with this owned checkout other than the query's own command interpreter and PowerShell process, which then exited. This is a scoped resource observation, not a claim about unrelated machine processes.

Both complete mandatory Go commands remain FAIL / unqualified. This correction does not qualify Unix, installed Windows Product, all native failure paths, or the production cancellation work in #629. Saved #624/#628 measurement series were not repeated. The PR remains Draft and no integration or merge is performed.
