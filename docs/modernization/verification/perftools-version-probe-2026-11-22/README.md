# Bounded perftools version commands — 22 November 2026

## Contents

1. [Problem and implementation](#1-problem-and-implementation)
2. [Exact inputs](#2-exact-inputs)
3. [Observed checks](#3-observed-checks)
4. [Review and limitations](#4-review-and-limitations)
5. [Evidence and references](#5-evidence-and-references)

## 1. Problem and implementation

`DetectTools` calls four version probes before callers can decide whether an
installed integration is available. At the parent revision, `probe` used
`exec.Command(...).CombinedOutput()` with no command deadline or output limit.
A stalled or noisy `--version` command could therefore stall discovery or retain
unbounded output. This is separate from the stdout fixture isolation already
published in this PR (pull request), which this increment preserves.

The change keeps the public `ToolInfo`, `Catalog`, `DetectTools` and first-line
extraction contracts. After successful `exec.LookPath`, `probe` starts a
five-second context and executes the resolved path. `runVersionProbe` uses the
existing `cappedBuffer` with a **shared 64 KiB limit** for standard output and
standard error (65,536 bytes total), and sets `Cmd.WaitDelay` to one second.
Both streams use the same writer pointer, retaining the serialization behavior
of `CombinedOutput` [1]. Overflow is an error and no partial version is accepted;
command failures retain their execution error and context cause. Successful
output still passes through the existing version extractor.

The private helper does not invoke the oneAPI handler environment or change
global discovery state. Reusing the handler executor would change error and
stream semantics: that executor keeps streams separate and treats a nonzero
analyzer exit differently. No generic execution layer or new dependency is added.

The five seconds apply to one command after path lookup. This is not a hard
six-second bound for all of `DetectTools`: path lookup, operating-system
scheduling and four serial probes remain separate. `WaitDelay` addresses
command/pipe waits; it does not establish that an entire descendant process tree
has exited [1]. The cap bounds retained combined output, not total process memory.

## 2. Exact inputs

The Windows qualification used an unchanged detached source at
`99dd76ccd453ccd56d9adbcd6f4d4d06ce5cb57c`, tree
`c1366014f49465da47e8244bbfd262ec86259094`, with exactly the two Go overlays in
Table 1. These are the bytes included in the source change, not a blanket claim
about a different master or a future composition.

**Table 1. Candidate file identities (SHA-256).**

| Path | SHA-256 |
| --- | --- |
| `internal/perftools/discovery.go` | `3c7cc0b981348cdd0184d57270f8765a400fc7519719e7e543f033cfbb7dc78c` |
| `internal/perftools/discovery_test.go` | `d413cbe57566e7f56cfc166b3084c908ece07cc0438fffaab2440ed87a0a3b29` |

The existing Windows tools were identified before and after the commands:
Go `go1.27.4 windows/amd64`, plus exact hashes of Go, gofmt, GCC (GNU Compiler
Collection) and Git. The receipts contain their hashes and paths. Module and
toolchain downloading were disabled (`GOPROXY=off`, `GOSUMDB=off`,
`GOTOOLCHAIN=local`, `GOFLAGS=-mod=readonly`). Existing shared caches were used
without relocation or removal. Task profiles, temporary files and telemetry
paths stayed under the task directory on `D:\dev\ChatGPT`.

The selected tests launch only their own current test executable with an exact
helper selector. They do not start installed perftools programs, a shell,
oneAPI discovery or descendants. A ready file proves that cancellation after
start reached the helper. Its finite fallback exits with code 5 and is explicitly
rejected by the cancellation oracle. The tests also reject returned output on
failure and measure return time relative to actual cancellation/deadline.

## 3. Observed checks

All rows in Table 2 refer to the pinned source plus explicit overlays. No row
represents the unrestricted default or tagged test suite.

**Table 2. Observed results; PASS/FAIL counts distinguish test and package events.**

| Check | Actual result | Scope |
| --- | --- | --- |
| gofmt on both candidate files | Exit 0 | Formatted bytes match Table 1 |
| Finite unbounded-capture control, `go test -race -json -count=1 -timeout=90s ./internal/perftools -run '^TestVersionProbeOutput$'` | Exit **1**, expected failure; 4 subtest PASS, 3 overflow subtest FAIL, root FAIL, package FAIL; 0 SKIP | Combined-output cap distinguishes the candidate from unbounded capture |
| Candidate, `go test -race -json -count=1 -timeout=90s ./internal/perftools -run '^(TestVersionProbe(Output\|Cancellation)\|TestProbeMissingPath)$'` | Exit 0; **13 test/subtest PASS + 1 package PASS**; 0 FAIL/SKIP | Ordering, exact limit, both streams and combined overflow, exit error, cancellation before/after start, deadline, missing path |
| `go build ./...` | Exit 0; 31.923 s | Full build for this source/overlay composition |
| `go vet ./...` | Exit 0; 30.065 s | Full static analysis for this source/overlay composition |

The actual commands include `-overlay <phase>/overlay.json`; the raw argument
arrays are in each receipt. The candidate test phase completed at
11:46:16 Moscow time (08:46:16 UTC); build and vet completed by 11:48:06 Moscow
time on 22 November 2026. No command timed out. Source HEAD and clean status,
candidate hashes and tool hashes were checked before/after as recorded.

The negative control replaces only the capped writer with `CombinedOutput`
inside the new private helper. It retains the context, wait delay and error
handling, and runs only finite output modes. It is **not execution of the entire
untouched parent revision**, and its expected result does not turn the raw FAIL
events into PASS. Its three `overflow accepted:` diagnostics are preserved.

## 4. Review and limitations

Own review covered requirements, execution/error behavior, shared-writer safety,
compatibility, scope and test oracles. Independent internal review found two
cancellation-oracle gaps: a finite natural exit could satisfy a context-only
assertion, and wrapping an error could hide unexpectedly returned output. Both
were corrected before the recorded test phase. It also prompted preserving the
failure receipt if timeout termination itself fails. The initial preparation
runner is retained separately; all subsequent phases use the corrected runner.

No remaining mandatory defect was identified in that reviewed change. This is
internal review, not the required external review of the exact published head.
Keep the PR Draft until that and the remaining acceptance criteria are satisfied.

The following remain **unqualified**:

- Full default/tagged Go suites and architecture acceptance on their eventual
  combined head; unsafe external inputs are still a separate gate.
- Actual Hyperfine, CMake, oneAPI, Serena and installed-product behavior.
- Descendant termination, credentials, Task Scheduler or installed Hub changes.
- Inherited-pipe behavior with a deliberately detached descendant: the recorded
  test helper creates none. `WaitDelay` is an implementation control, not a claim
  that such a native scenario was exercised.

## 5. Evidence and references

The [manifest](probe-evidence-manifest.json) indexes all **209** archived files:
raw standard output/error, per-command start/launch/final records, six phase
receipts, overlays, both candidate/control files and runner versions. The
[archive](probe-evidence.tar.xz) is 15,852 bytes; SHA-256:
`4e4ac9ed77107ca156405e09238a3b2b48ced4f6af1359719488e09430b8c3a8`.
The [read-only verifier](verify_evidence.py) checks every member, actual exit and
stream hash, candidate identity, runner identity and the recorded test oracles.
It does not execute Go or certify the behavior of code it has not run.

1. Go standard library, [`os/exec.Cmd`](https://pkg.go.dev/os/exec#Cmd): comparable
   shared writers, `CommandContext`, and `WaitDelay` semantics. Observed execution
   used the pinned tool above; this reference explains the API contract.
2. [Existing stdout fixture qualification](../perftools-stdout-fixture-2026-11-21/README.md).
3. [Canonical integration plan](../../../../.plans/2026-11/open-pr-integration-20261117.md).
