# W8 F1 GUI compiled-test process dispatch design

Date: 2026-08-13

Execution role: `$architect`

Scope: bounded correction of W8 finding F1. This package designs test-only
process framing; it contains no implementation and authorizes no source, Git
index/history, live service, CST, Service Control Manager, App Control, virtual
disk, CiTool, signing, hardware security module, publication, or deployment
mutation.

## Accepted evidence and immutable binding

The source authority is immutable commit
`7aa0a2168e09139bf8deeac0fe31b8d0260c017d`, parent
`bab886092ae0a4148c05f1e057eeedd73731eedf`, tree
`b3fe2b5f95ceaa6c51204ac38d29b4563bac81d4`. The accepted W8 REVISE artifact is
`implementation-architecture-review.md`, SHA-256
`560E34037EC81D1390E0434FD699D1361C8F88F873850B25660F3ABDB2C2B2BD`.
The supplied earlier hash
`D898AB4CBCC19E85DA9ECC0A197844063E75A9EF61738285C03451BD93F8AFC8`
was not present among the current active-item files or the three reachable
item commits, so it is unavailable input, not evidence used by this design.

Fresh CodeGraph inspection returned the current GUI `TestMain`, classifier,
terminal-worker launch, strict runner, and their call paths. Its only pending
files were unrelated to `internal/gui`. Git-object reads then bound these facts
to the immutable candidate:

- `TestMain` classifies before any package root is created, but represents roles
  with overlapping booleans and positional re-checks (`internal/gui/main_test.go:186-207,
  278-320`).
- the classifier rejects the unmarked exact R6 selector and admits the marked
  R6 tuple only by falling through to normal package setup
  (`internal/gui/main_test.go:310-320`);
- the same test is both outer parent and receiver child; the parent adds the R6
  marker and absolute state root (`internal/gui/audit_lock_terminal_worker_test.go:496-518`);
- the receiver invokes the ordinary production-shaped terminal worker twice,
  first with only the stderr fault marker and then without it
  (`internal/gui/audit_lock_terminal_worker_test.go:533-582`);
- production `runAuditLockTerminalWorker` launches the current executable with
  exact argv `audit-lock-terminal-worker` and inherits ambient environment
  (`internal/gui/audit_lock_state.go:1547-1561`);
- blocking-helper launch currently inherits ambient helper state and appends its
  own three fields (`internal/gui/audit_lock_terminal_worker_test.go:48-80`);
- two overlapping environment filters exist
  (`internal/gui/audit_lock_terminal_worker_test.go:725-744` and
  `internal/gui/testmain_dispatch_test.go:37-60`).

## Chosen approach

Replace the boolean/positional mixture with one test-only, package-local
process-role classifier, one canonical Go-test-selector parser, and one
authoritative helper-environment registry. The classifier returns exactly one
role plus a stable failure reason:

`normal-parent | r6-receiver-child | audit-terminal-worker-child |
blocking-helper-child | pidfd-linux-child | invalid`.

`TestMain` switches once on that role. Only `normal-parent` enters existing
package-root creation, sandbox installation, `m.Run`, explicit cleanup, and
root removal. R6, blocking, and Linux PIDFD children execute only their exact
selected test through `m.Run` and exit with its returned code, without creating
a package root. The terminal worker executes only `RunAuditLockTerminalWorker`
and exits with its existing result semantics. Invalid framing exits `3` before
`m.Run` or any package setup.

The selector parser is the pre-root argv owner because `TestMain` runs before
Go parses flags. Installed Go 1.26.5 evidence binds the contract: `testing.Init`
registers string flag `test.run` (`$GOROOT/src/testing/testing.go:438-465`), and
`M.Run` invokes `flag.Parse` if needed (`testing.go:2351-2353`). The accepted
independent review additionally bound the installed `flag` grammar: one or two
dashes and attached or next-token string values. The parser therefore accepts
exactly `-test.run=value`, `--test.run=value`, `-test.run value`, and
`--test.run value` before the first positional token or `--`; it normalizes the
one value before classification. An explicitly present empty value is valid and
normalizes to the ordinary empty selector for `-test.run=`, `--test.run=`, and
the split forms when the following token is the empty string; it can never
authorize a helper role. The parser rejects only a split form with no following
token, every second occurrence across attached/split or mixed-dash forms, and any flag-region token
that begins `-test.run` or `--test.run` but is not one of those four forms.
After `--` or the first positional token, no selector spelling is interpreted
as a flag; helper roles requiring a selector are then invalid, and terminal
worker argv must still be its exact sole positional token.

The R6 receiver test consumes its role fields exactly once: capture the already
validated absolute state root, unset both R6 framing keys with a checked,
test-cleanup-backed restore, assert they are absent, then enter
`runAuditLockR6ReceiverScenario`. `MCPHUB_STATE_DIR_OVERRIDE` is independently
set to the captured root by the scenario and is not scrubbed. Consequently its
nested production-shaped worker inherits no receiver role while retaining the
intended first-run stderr fault marker. No production launcher or worker
acceptance rule changes.

All test-binary spawns start from `withoutGUITestHelperEnvironment(os.Environ(),
runtime.GOOS)` and add only their role's exact fields. The registry includes the
six audit keys and both PIDFD keys. On Windows, logical key identity is the same
case-insensitive identity used by the installed Go 1.26.5 `os/exec` environment
deduplicator (`$GOROOT/src/os/exec/exec.go:1251-1293`): normalize the key with
the same lower-case rule, reject duplicate case variants before dispatch,
scrub every case variant, and emit only the canonical uppercase spelling once.
On POSIX, identity and scrub are byte-for-byte case-sensitive; differently
cased names are unrelated ambient variables, while the canonical helper keys
alone convey authority. Classifier production input is the full environment
slice plus target GOOS, not a case-sensitive fake getter. The registry, key
identity, selector parser, and filter live beside the classifier in
`main_test.go`; the two existing duplicate filters are removed. This is the
single framing owner and prevents fix layering.

## Exact process-role truth table

Selector values below are normalized from all four supported Go spellings.
“Absent” means the logical key is not present, not an empty assignment. `B`,
`L`, `E`, `R`, `S`, `F`, `P`, and `T` denote respectively the blocking marker,
lock path, entered path, R6 marker, R6 state root, stderr fault marker, PIDFD
child marker, and PIDFD stall marker.

| Argv / selector | Recognized helper environment | Role / action |
|---|---|---|
| Ordinary Go test flags; selector absent or any non-reserved selector | `B,L,E,R,S,F,P,T` absent | `normal-parent`; create one package root, run, clean, propagate `m.Run` code. |
| Exact R6 selector in any supported spelling | All eight fields absent | `normal-parent`; valid outer test that creates the receiver child. |
| Exact R6 selector in any supported spelling | `R=1`, `S=<absolute clean path>`; other six absent | `r6-receiver-child`; no package root, run only selected test, propagate code. |
| Exact blocking selector in any supported spelling | `B=1`, `L=<absolute clean path>`, `E=<absolute clean path>`; other five absent | `blocking-helper-child`; no package root, run only selected test, propagate code. |
| On Linux, exact `^TestRetainedPIDFDAlive_LinuxChildHelper$` selector in any supported spelling | `P=1`; `T` absent or `T=1`; all six audit fields absent; no positional argv | `pidfd-linux-child`; no package root, run only selected test, propagate code; `T=1` selects its existing stall branch. |
| Same PIDFD selector/frame on non-Linux, or `T` without `P=1` | Any | Invalid platform/partial frame; exit `3`. |
| Sole positional `audit-lock-terminal-worker` before any selector | `B,L,E,R,S,P,T` absent; `F` absent | `audit-terminal-worker-child`; run bounded worker and retain current 0/1 result. |
| Sole positional `audit-lock-terminal-worker` before any selector | `B,L,E,R,S,P,T` absent; `F=1` | Same worker; after bounded worker call emit existing capped test marker and exit `3`. |
| Exact blocking selector | `B,L,E` all absent | Invalid reserved selector; exit `3` before root or `m.Run`. |
| Exact R6 selector | `S` present without `R=1`, or `R=1` without `S` | Invalid partial R6 frame; exit `3`. |
| Any selector/argv | `L` or `E` present without the complete blocking tuple | Invalid orphan blocking field; exit `3`. |
| Any selector/argv | Any recognized marker value other than exact `1` | Invalid unknown marker value; exit `3`. |
| R6 tuple | Non-absolute, empty, or non-clean `S` | Invalid R6 state-root framing; exit `3`. |
| Blocking tuple | Non-absolute, empty, or non-clean `L` or `E` | Invalid blocking path framing; exit `3`. |
| Any selector/argv | Fields from two or more role families | Invalid conflict; exit `3`. |
| Terminal positional token plus any extra positional token or test selector | Any | Invalid argv conflict; exit `3`. |
| Flag region | Duplicate selectors across any attached/split or one-/two-dash forms | Invalid duplicate selector; exit `3`. |
| Flag region | Explicit attached empty value, or split form whose next token is the empty string | Valid normalized empty selector; `normal-parent` only when no helper frame is present. |
| Flag region | Split form with no following token, or reserved-looking unsupported selector syntax | Invalid selector grammar; exit `3`. |
| Windows environment | Two entries with the same logical helper key under case-insensitive identity | Invalid duplicate/conflicting environment frame; exit `3`. |
| Non-terminal argv | `F` present | Invalid terminal-only fault field; exit `3`. |

Unknown ordinary Go test flags and unrelated environment variables remain Go's
and the test's concern. `--` and the first positional token end the parser's
flag region exactly as Go does. The dispatcher rejects malformed reserved
selector spellings and unknown values or partial tuples only within its
eight-key framing namespace. This avoids broadening a test-only security
boundary into a generic argv policy.

## Components, owners, and caller changes

| Owner | Required design change | Protected behavior |
|---|---|---|
| `internal/gui/main_test.go` | Own the role enum, stable invalid-reason enum, canonical four-form selector parser, eight-key platform-aware registry/filter, exact classifier, and one `TestMain` role switch. | Normal-parent sandbox setup/cleanup remains byte-for-behavior equivalent; terminal protocol and output bounds do not change. |
| `internal/gui/audit_lock_terminal_worker_test.go` | Blocking and R6 spawns call the common filter before adding exact fields. Receiver captures then consumes/unsets `R` and `S` before scenario work. Delete `withoutAuditLockTestEnv`. | HTTP sequence, settlement assertions, stderr redaction, state-root isolation, strict containment, and timeout logic remain unchanged. |
| `internal/gui/testmain_dispatch_test.go` | Use the common filter and extend the compiled-binary adversarial/positive matrix. Delete its local key list/filter. | Tests continue to observe real process exit, stderr identifier, and package-root absence. |
| `internal/gui/probe_linux_test.go` | Its same-binary spawn calls the common filter, adds canonical `P=1` and optional `T=1`, and is classified as `pidfd-linux-child`. | Existing fd 3/4 barrier, context timeout, wait delay, cancellation and cleanup remain unchanged. |
| `internal/gui/audit_lock_state.go` and `audit_lock_terminal_worker.go` | No change. | Production-shaped argv, bounded JSON, containment, failure IDs, and ambient production behavior are frozen. |

Dependency direction stays `TestMain/test spawner -> test-only framing owner ->
existing production contract`. Production source must not import or depend on
test support.

## Marker inheritance and transitions

| Spawn/transition | Scrub first | Add/retain | Resulting child authority |
|---|---|---|---|
| Normal parent -> R6 receiver | Scrub `B,L,E,R,S,F` from copied env. | Add only `R=1`, `S=<absolute clean state root>`; inherit ordinary sandbox env. | R6 receiver only. |
| R6 receiver entry -> scenario | Capture `S`; checked-unset `R,S`; restore through test cleanup. | Scenario sets independent `MCPHUB_STATE_DIR_OVERRIDE=<captured S>`. | No helper role remains in ambient env. |
| R6 scenario -> first terminal worker | R6 role already consumed; production launcher unchanged. | Add/retain only `F=1` for deliberate stderr-failure run. | Terminal worker with fault injection. |
| R6 scenario -> second terminal worker | R6 role absent; checked-unset `F` and verify absence. | No helper fields. | Plain terminal worker. |
| Normal parent -> blocking helper | Scrub `B,L,E,R,S,F,P,T` from copied env. | Add only `B=1`, absolute `L`, absolute `E`. | Blocking helper only. |
| Linux normal parent -> PIDFD child | Scrub all eight canonical logical keys using Linux case-sensitive identity. | Add only `P=1`, and `T=1` iff stalled; retain existing `GORACE` and fd 3/4 contract. | PIDFD Linux child only. |
| Adversarial direct probe | Scrub all eight logical keys using target-platform identity. | Add only the row's deliberate malformed fields, including case variants on Windows. | Deterministic classifier falsifier. |

## Stable contracts and observability

There is no external contract or persisted-state change. The internal test
contract is exact argv + exact selector + exact environment tuple. Invalid
classification emits no paths or environment values; it emits
`internal/gui: invalid test helper dispatch: <reason-id>` and exits `3`.

| Failure mode | Stable discriminator |
|---|---|
| Duplicate selector | `GUI_TEST_HELPER_DUPLICATE_SELECTOR`, exit `3`. |
| Missing split token or unsupported reserved selector form | `GUI_TEST_HELPER_INVALID_SELECTOR_GRAMMAR`, exit `3`. |
| Reserved selector lacks its marker | `GUI_TEST_HELPER_SELECTOR_ONLY`, exit `3`. |
| Partial/orphan tuple | `GUI_TEST_HELPER_PARTIAL_FRAME`, exit `3`. |
| Unknown recognized marker value | `GUI_TEST_HELPER_UNKNOWN_VALUE`, exit `3`. |
| Two role families active | `GUI_TEST_HELPER_CONFLICT`, exit `3`. |
| Wrong positional argv | `GUI_TEST_HELPER_WRONG_ARGV`, exit `3`. |
| Invalid helper path | `GUI_TEST_HELPER_INVALID_PATH`, exit `3`. |
| Duplicate Windows case variant | `GUI_TEST_HELPER_DUPLICATE_ENV_KEY`, exit `3`. |
| PIDFD frame on non-Linux or incomplete PIDFD tuple | `GUI_TEST_HELPER_PIDFD_FRAME_INVALID`, exit `3`. |
| Receiver role not consumed before nested spawn | Positive nested-worker test sees execution failure instead of expected first `409` then second `200`; an explicit child-env probe reports the retained key name only. |
| Package root created by a child | Child-root sentinel/count probe is nonzero; expected zero. |
| Test body fails in a valid child | Exact nonzero `m.Run` code is propagated, not converted to success. |

## Resource lifetime and all-return cleanup

- `normal-parent` remains sole owner of its temp package root and all existing
  explicit restore/remove operations on success and failure.
- helper children create no package root. Their `testing.T` cleanup runs before
  `m.Run` returns; `TestMain` propagates that code after cleanup.
- R6 role consumption saves prior presence/value, checked-unsets both fields,
  and registers restoration before any server, event subscription, request, or
  nested worker can start. Failure to unset is fatal before scenario mutation.
- existing R6 `defer`/`t.Cleanup` ownership for server events, audit lock,
  context, and state override is unchanged.
- the blocking helper remains owned by `RunStrictlyContained`; cancel/timeout
  reaps the process tree. Its parent environment slice is immutable after
  `cmd.Start`.
- the Linux PIDFD parent remains sole owner of both pipe pairs, context cancel,
  child wait/release state, and cleanup. The PIDFD child only borrows fd 3/4 and
  exits through its selected test; bypassing package setup adds no resource.
- terminal-worker handle/pipe/job cleanup stays exclusively in the strict
  runner. This correction adds no handle, goroutine, file, listener, or global
  production state.

## Diff-invisible invariants

1. **Normal GUI tests remain isolated from operator state.** Named regression
   guard: full `go test ./internal/gui`; root escape and cleanup checks remain
   clean.
2. **Children never create or delete a package root.** Named regression guard:
   compiled child probes instrument the root owner and observe creation count
   `0` for R6, blocking, terminal, and Linux PIDFD roles; normal parent observes
   exactly `1` and removes it.
3. **The first R6 worker fails only through capped stderr injection, and the
   second succeeds durably.** Named regression guard: exact tagged R6 test with
   `-count=2` returns `409/DAEMON_RECOVERY_OUTCOME_UNCERTAIN` then `200`, two
   recoverer calls, settled events, unlocked store, one redacted failure row,
   and no raw marker residue.
4. **Malformed helper state cannot enter package setup or `m.Run`.** Named
   regression guard: compiled adversarial matrix expects exit `3`, a stable
   reason ID, and zero root/body sentinel for every negative row.
5. **Production worker framing and cleanup do not change.** Named regression
   guard: `git diff -- internal/gui/audit_lock_state.go
   internal/gui/audit_lock_terminal_worker.go internal/process` is empty and
   existing terminal-worker protocol/containment tests pass.
6. **No stale parallel parser/filter remains.** Named regression guard: source
   search finds one selector parser and one eight-key registry/filter owner,
   reconciles every `os.Args[0]` re-exec caller, and finds no
   `withoutAuditLockTestEnv` definition or second literal key list.

## Security-by-design requirements

- Fail closed before root creation or test execution for every conflicting,
  partial, unknown-value, duplicate-selector, or wrong-argv frame.
- Parse selector presence from argv with Go-compatible flag-region and
  four-spelling semantics; do not infer it with prefix search or `flag.Parse`.
- Parse environment from its full entry slice with target-platform logical key
  identity and presence, never a value-only or accidentally case-sensitive map.
- Never log helper paths, environment values, stdin, or the raw stderr marker.
- Receiver role data is authority only for reaching its exact test body; it is
  consumed before any production-shaped child is spawned.
- The terminal worker does not learn to tolerate R6 or blocking markers. This
  preserves the security finding closure instead of weakening the receiver.
- The common filter copies an input slice, removes every Windows case variant
  or exact POSIX canonical key as applicable, and callers append each authorized
  logical role field once in canonical uppercase spelling.
- No fallback role, permissive default, production feature flag, dependency,
  or persisted alias is introduced.

## Test strategy

Implementation follows test-driven development: extend the classifier and
compiled-binary tables first and observe the current candidate fail the valid
outer R6 and inherited-marker cases.

Positive probes:

1. pure selector/parser/classifier table covers all four Go selector spellings,
   explicitly empty attached/split values, genuinely missing split tokens,
   parser terminators/positionals, Windows/POSIX environment identity, and every
   valid role row above;
2. exact unmarked tagged R6 outer test passes twice;
3. direct marked R6 receiver creates no package root and propagates `m.Run`;
4. first and second nested terminal workers observe respectively only `F=1`
   and no helper key, while retaining current bounded protocol results;
5. exact blocking helper reaches its barrier without a package root;
6. plain and stderr-injected terminal workers retain current exit/output rules;
7. Linux PIDFD helper reaches its fd 3/4 barrier in normal and stalled modes,
   creates no package root, and remains reaped by existing cleanup;
8. normal parent still creates exactly one root and cleans it on passing and
   deliberately failing selected tests.

Negative probes enumerate: attached/split and one-/two-dash mixed duplicates;
genuinely missing split tokens; reserved-looking unsupported spellings; `--` and
first-positional parse stops; selector-only blocking; every single orphan
`L/E/S/T`; missing field from each complete tuple; marker values `0`, `2`,
empty-present, and arbitrary text; relative/non-clean paths; every pairwise role
conflict; terminal token with selector/extra positional arg; `F` on non-terminal
argv; PIDFD frame on non-Linux; and valid child whose test body deliberately
returns nonzero. Windows rows seed canonical, lower-, and mixed-case forms of
all eight keys, including duplicate logical variants; POSIX rows prove distinct
case is unrelated and canonical framing remains exact. Each malformed row
expects exit `3` plus its stable reason ID and zero package-root or test-body
sentinel, except the deliberate body failure which expects the exact propagated
`m.Run` code.

Positive empty-selector probes cover `-test.run=`, `--test.run=`, and both split
forms with an explicit empty-string next argv element. Each normalizes to one
present empty selector, remains `normal-parent`, creates/cleans the normal root,
and runs no helper body. Pairing any empty selector with `B`, `R`, `P`, `F`, or
their dependent fields is a helper-role conflict and exits `3` before setup.

Focused verification is the exact tagged R6 command, focused classifier and
terminal-worker tests, full `internal/gui`, scoped `go vet`, immutable-source
diff, CST preservation tests, native verifier, and the canonical publication
scanner. The latter is diagnostic only and cannot authorize publication.

## Alternatives

1. **Chosen: explicit role enum plus consume-on-entry R6 framing.** Smallest
   coherent surface; keeps production launch frozen, gives children no package
   root, and makes inheritance intentional.
2. **Dedicated R6 receiver executable/harness.** Strong separation, but adds a
   new binary/protocol/packaging owner for one test and expands maintenance and
   security review without evidence of a second consumer. Rejected by the
   smallest-durable-surface constraint.
3. **Teach the terminal worker to ignore R6 markers.** Superficially small but
   weakens production-shaped framing and leaves ambient role leakage intact.
   Rejected because W8 explicitly requires preservation of terminal-worker
   security closure.
4. **Only allow the unmarked R6 selector.** Fixes the first symptom but not the
   nested inherited-marker failure; rejected as fix layering.

## Change-Surface Contract

`{ intended change surface: internal/gui/main_test.go,
internal/gui/audit_lock_terminal_worker_test.go,
internal/gui/testmain_dispatch_test.go, and internal/gui/probe_linux_test.go
only; approved extension seams: the package TestMain pre-root selector/parser
classifier, the shared test-helper environment identity/registry/filter, and
the three test-owned child spawn sites; protected /
must-not-touch surfaces: all non-_test.go production code including
audit_lock_state.go, audit_lock_terminal_worker.go, internal/process, CST
frontend/daemon/broker/native/protocol/policy/manifest code, external
contracts, persisted schemas, and live system state; declared blast radius:
compiled internal/gui test-binary startup, four helper child roles, their argv
and environment framing, exit/status diagnostics, and test-only regression
matrices }`

## Claims

1. `{ guarantee: exactly one Go-compatible selector parser and classifier
   select exactly one GUI test-process role before package setup;
   single-owner: internal/gui TestMain framing owner; enforcement-probe: pure
   truth-table test covers all four selector forms including explicitly empty
   values, genuinely missing split tokens, parse stops, and every valid and
   invalid row }`.
2. `{ guarantee: only the normal parent creates and removes the package test
   root; single-owner: normal-parent TestMain branch; enforcement-probe:
   compiled role probes report parent count 1 and R6, blocking, terminal, and
   Linux PIDFD child counts 0 }`.
3. `{ guarantee: an unmarked exact R6 selector is a valid normal parent while
   the marked exact tuple is only an R6 receiver; single-owner: GUI role
   classifier; enforcement-probe: outer and direct-child positive tests both
   reach their expected bodies }`.
4. `{ guarantee: R6 framing is absent before either nested terminal worker is
   spawned; single-owner: R6 receiver test entry transition;
   enforcement-probe: nested-child environment observation finds neither R nor
   S and exact R6 -count=2 returns first 409 then second 200 }`.
5. `{ guarantee: terminal-worker acceptance is not weakened and production
   source is unchanged; single-owner: existing terminal-worker composition;
   enforcement-probe: zero diff in protected production paths plus existing
   protocol/containment tests }`.
6. `{ guarantee: blocking, R6, and Linux PIDFD spawns inherit no stale GUI
   helper role under Windows or POSIX key identity; single-owner: shared
   eight-key platform-aware environment filter; enforcement-probe: spawn tests
   inspect exact logical key sets across canonical/lower/mixed-case inputs }`.
7. `{ guarantee: genuinely missing split values and malformed selector names,
   but not explicitly present empty selector values, duplicate logical environment
   keys, partial, unknown-value, conflicting, duplicate-selector, platform, and
   wrong-argv frames fail nonzero before root creation or m.Run; single-owner:
   GUI role classifier; enforcement-probe: compiled adversarial matrix observes
   exit 3, stable reason ID, and zero side-effect sentinels }`.
8. `{ guarantee: valid child test failures are never converted to success;
   single-owner: TestMain child-role switch; enforcement-probe: deliberate
   child-body failure returns the exact m.Run code }`.
9. `{ guarantee: the live tree contains one selector parser, one helper-key
   identity/registry/filter, and a declared role for every GUI same-binary
   child; single-owner: main_test.go test-support owner; enforcement-probe:
   source/caller inventory reconciles every os.Args[0] launch, finds one
   registry, and zero withoutAuditLockTestEnv definitions }`.
10. `{ guarantee: no external contract, persisted schema, production behavior,
    dependency, or live state changes; single-owner: declared four-file change
    surface; enforcement-probe: candidate diff is a subset of those four
    _test.go files and dependency manifests plus protected paths have zero diff }`.

## Rollback

The correction is one four-file test-only change. Before publication, rollback
means remove that successor change and return to immutable commit `7aa0a216...`;
then the exact W8 F1 reproduction must fail again in both documented ways,
proving the rollback boundary. No data migration, service action, state repair,
or compatibility window exists. Any implementation that touches a protected
surface, retains both filters, adds a second classifier, or cannot meet the
truth table returns to architecture review rather than widening this design.

## Adjacent findings

None. The Linux PIDFD same-binary child is now an explicit platform-scoped
participant of the shared process-role owner rather than an excluded sibling.

## Gate

`PASS`

The design closes both halves of W8 F1 through one fail-closed, test-only role
state machine, explicit marker consumption, and one environment-framing owner,
without changing production behavior. Route next to an independent
`$architecture-reviewer`; only an accepted review may authorize bounded backend
implementation.

## Terms and Abbreviations

- App Control: Windows application-control policy enforcement.
- CST: CST Studio Suite electromagnetic solver.
- GUI: Graphical user interface.
- HSM: Hardware security module.
- R6: real cross-process GUI recovery receiver regression scenario.
- SCM: Windows Service Control Manager.
- VHDX: Hyper-V virtual hard disk format.
- W8: independent implementation architecture review phase.
