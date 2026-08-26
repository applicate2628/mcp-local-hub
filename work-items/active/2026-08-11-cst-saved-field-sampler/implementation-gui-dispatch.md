# W8 F1 GUI dispatcher implementation

Date: 2026-08-13

Execution role: `$backend-engineer`

## Immutable inputs and scope

Implementation was bound to candidate commit
`7aa0a2168e09139bf8deeac0fe31b8d0260c017d`, accepted W8 finding artifact
SHA-256 `560E34037EC81D1390E0434FD699D1361C8F88F873850B25660F3ABDB2C2B2BD`,
accepted design SHA-256
`CC0AE477C1ACD4A62D4A39094A4D82A5687A89D4F4EF9C707E22F825C450A3BC`,
and design-review PASS SHA-256
`136CC3C4809C64F545A741F83952C51C384E73A0A0CD7E409310C9043380847D`.
CodeGraph was used before source reads and was fresh after implementation at
2,118 indexed files.

The exact source change surface is:

| Path | SHA-256 after implementation |
|---|---|
| `internal/gui/main_test.go` | `6593690D854F215EABFBC729DF39E6B247B141CA16FF6696276FC45A4C4D0E5E` |
| `internal/gui/audit_lock_terminal_worker_test.go` | `0D213D405C464EC3882150BDCCE741F1A30E6B4827AD6FA7818EC8BE97CBC5F5` |
| `internal/gui/testmain_dispatch_test.go` | `775BBE829F79A414326C36C0F354EF464F332135E99A5EF13FFFA62B2C71191C` |
| `internal/gui/probe_linux_test.go` | `9E7856F414B4EEAE6D6D56F716F29F578C3E830B5ACD99F8CD76D793781AF255` |

No production source, external contract, persisted schema, dependency, live
service, CST, Service Control Manager, App Control, virtual disk, signing,
publication, Git index, or Git history was changed. Existing unrelated dirty
work-item files were not touched by this implementation lane.

## Implementation

- `TestMain` now makes one pre-root switch on an explicit process role:
  normal parent, R6 receiver, audit terminal worker, blocking helper, Linux
  PIDFD helper, or invalid.
- One parser owns all four installed Go `test.run` spellings, including valid
  present-empty values, parse stops, duplicate and malformed detection.
- One eight-key registry owns helper environment identity, validation and
  filtering. Windows identity is case-insensitive; POSIX identity is exact.
- The unmarked exact R6 selector remains an ordinary parent. The marked R6
  child captures and consumes both receiver keys before it starts either
  production-shaped nested terminal worker.
- Blocking, R6, PIDFD and adversarial child spawns all scrub the common
  namespace before adding their exact canonical role frame.
- Terminal-worker acceptance and implementation remain unchanged; valid child
  roles propagate the exact `m.Run` result.

There are no changed HTTP, database, queue, cache, or remote-procedure-call
surfaces; timeout/retry, authorization, pagination and wire before/after
statements are therefore not applicable. The internal compiled-test wire shape
changes from overlapping booleans/value-only environment reads to the accepted
exact role/selector/full-environment truth table; invalid frames now return
exit `3` with the accepted stable reason identifier.

## TDD and root-cause evidence

RED was observed before owner implementation:

- the original exact tagged R6 command exited `1` with
  `internal/gui: invalid test helper dispatch` before `m.Run`;
- the new truth-table test failed to compile solely because the wished-for
  role, failure and filter owner API did not exist.

GREEN was then reached with the smallest owner implementation. A broader
focused run exposed one reason-precedence error for an incomplete blocking
tuple beside terminal argv; tuple completeness was moved ahead of argv
conflict classification, and the exact suite passed twice. Linux cross-compile
then exposed a missing Linux-only `runtime` import; after adding it, the exact
cross-compile passed and its scratch binary was removed.

## Verification

| Probe | Observed result |
|---|---|
| Exact tagged R6 plus classifier/filter/outer reachability | PASS; exact R6 returned intended first uncertain and second durable sequence through its existing assertions. |
| Focused compiled dispatcher, terminal-worker protocols and Linux-named PIDFD suite, `-count=2` | PASS: `ok mcp-local-hub/internal/gui 0.943s`. |
| Final focused dispatcher/R6 rerun after truth-table reconciliation, `-count=2` | PASS: `ok mcp-local-hub/internal/gui 0.592s`. |
| Final full tagged GUI suite | PASS: `ok mcp-local-hub/internal/gui 58.347s`. |
| Windows tagged `go vet` | PASS, exit `0`. |
| Linux tagged compile and `go vet` | PASS, exit `0`; generated scratch binary removed. |
| Explicit saved-field Python preservation set | PASS, all collected `test_cst_saved_field*.py`; one existing Pydantic incomplete-definition warning. |
| Native unsigned verifier | PASS; image SHA-256 `38D87C50F716E334F89628D4F35604534C3A35BDCF35F378F1D939137BAB89E3`. |
| Protected production diff | PASS: zero diff in `audit_lock_state.go`, `audit_lock_terminal_worker.go`, and `internal/process`. |
| Source inventory | PASS: one selector parser, one classifier, one eight-key registry/filter; no `withoutAuditLockTestEnv` definition; all four test re-exec sites reconciled. |
| `git diff --check`, exact four-path diff, Git index | PASS: no whitespace errors; exactly four authorized source paths; index empty. |
| Publication-safety scanner | PASS for each exact changed source file; canonical range receipt v2 clean at immutable tip. Whole-directory path mode also reported pre-existing tracked findings and is not a clean attribution surface. No publication was attempted. |

## Receiving-side echo

| Diff-invisible invariant | Result |
|---|---|
| Normal GUI tests remain isolated from operator state | Verified by full tagged GUI PASS and unchanged normal-parent setup/cleanup body. |
| Children never create/delete the package root | Verified structurally: all four child roles exit from the pre-root switch; only normal parent reaches `MkdirTemp` and cleanup. Compiled child protocols passed. |
| R6 first worker fails only through capped stderr injection and second succeeds durably | Verified by exact tagged R6 PASS twice; existing 409/200, event, lock, redaction and marker-residue assertions all executed. |
| Malformed helper state cannot enter setup or `m.Run` | Verified by pure adversarial matrix and compiled exit-3/stable-reason matrix. |
| Production worker framing and cleanup do not change | Verified by zero protected production diff and passing terminal protocol/containment tests. |
| No stale parallel parser/filter remains | Verified by exact source inventory: one parser/classifier/registry/filter and zero old filter definitions. |

Defect-class audit:

| Participant | Classification | Evidence |
|---|---|---|
| Unmarked outer R6 selector | fixed | Classified normal parent; exact tagged and compiled reachability probes pass. |
| Marked R6 receiver | fixed | Exact receiver role bypasses root; role keys are consumed at test entry. |
| First nested terminal worker | fixed | Inherits only deliberate stderr fault marker; existing capped/redacted uncertain assertions pass. |
| Second nested terminal worker | fixed | Inherits no helper role; durable 200 and settlement assertions pass. |
| Blocking same-binary helper | fixed | Common scrub plus exact `B/L/E` frame; cancellation/reap protocol passes. |
| Plain/fault terminal worker | not affected | Production launch/framing unchanged; both protocols pass. |
| Linux PIDFD same-binary helper | fixed | Explicit Linux role, common scrub, exact `P/T` frame; Linux compile/vet pass. |
| Adversarial compiled probe | fixed | Common scrub makes malformed rows deterministic; exact exit/reason probes pass. |

## Risks and adjacent findings

No adjacent implementation finding was opened. Linux runtime execution was not
available on this Windows host; Linux role syntax/type integration is verified
by cross-compile and vet, while runtime PIDFD behavior remains covered by its
existing Linux-host tests. The broad `internal/gui` path scanner contains old
tracked findings unrelated to this four-file delta; exact changed-file scans
are clean. Publication and live target work remain unauthorized and undone.

## Gate

`PASS`

The accepted four-file, test-only design is implemented and freshly verified.
Route next to independent W8 architecture re-review; any source mutation after
the hashes above invalidates this package.

## Terms and Abbreviations

- CST: CST Studio Suite electromagnetic solver.
- GUI: Graphical user interface.
- PIDFD: Linux process file descriptor.
- R6: real cross-process GUI recovery receiver regression scenario.
- SCM: Service Control Manager.
- TDD: test-driven development.
- W8: independent implementation architecture review phase.
