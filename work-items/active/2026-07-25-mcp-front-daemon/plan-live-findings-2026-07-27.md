# PR #588 live-finding execution plan

Date: 2026-07-27  
Owner: `$backend-engineer` for implementation and integration; `$qa-engineer`
for independent verification  
Decision input: `design.md` — PASS  
Reliability input: `reliability-live-findings-2026-07-27.md` — R2 PASS  
Execution gate: **PASS — RETURN(lead); planner-eligible**

## R3 correction amendment — active execution contract

Accepted inputs:

- R3 architecture decision at SHA-256
  `6DC6DC4478F05044DBD5E58E08F7DF0E4D4A87AA4161BE20E02B46B93330028F`;
- R3 design at SHA-256
  `A42A6CDF0BDEEF1502640269E305D1F6D3F09E05F72789810A125FA6CD6C106B`;
- repeat reliability PASS at SHA-256
  `7237D8CFE8CE6A9C535BAC0EAA885E015EAD52F9941207874C8705EA6F890B34`;
- `architecture-r2-live-findings-2026-07-27.md`.

This amendment supersedes R2 and every earlier plan statement where they
conflict. The older amendments/phases remain evidence history only.

### R3-0 — foreign-overlap admission gate

Owner: `$lead` with the foreign change owner or user

Source writes: none

Gate: **BLOCKED before implementation while overlap remains**

Current-session evidence shows foreign changes in
`internal/api/lsp_client_router.go`,
`internal/api/serena_client_reconcile.go`, and
`internal/api/serena_client_reconcile_test.go`, including a `TEMPDEBUG` stderr
write. All three overlap the R3 surface. `.codegraph` and
`.codegraph-init.log` are also foreign/user-owned and excluded.

Before `$backend-engineer` starts:

1. The Lead records the exact foreign path/hash/diff owner and asks that owner
   or the user to finish, remove, or explicitly admit those changes.
2. No R3 actor edits, stages, cleans, deletes, moves, formats, or includes
   `TEMPDEBUG` or `.codegraph*`.
3. If any unresolved foreign hunk remains in an allowed R3 path,
   implementation stays `BLOCKED`; file-level allowlisting does not authorize
   overwriting an overlapping hunk.
4. R3 is a new correction atop commit `31b9ca94`. No amend, rebase, reset,
   squash, checkout, stash, force operation, or history rewrite is authorized.

- **R30-AC1:** Before the first Go/code edit, an owner-approved overlap record
  names every dirty allowed path and disposition; unresolved overlap count is
  exactly zero.
- **R30-AC2:** `git status --short` still lists `.codegraph*` as foreign or the
  foreign owner has removed it; no R3 diff/stage contains `.codegraph*`.
- **R30-AC3:** `git rev-parse HEAD` equals `31b9ca94...` at implementation
  admission or names a newer user-owned descendant explicitly accepted by the
  user; R3 never rewrites that history.

### Exact R3 change surface

| Kind | Exact path | Sole R3 concern |
| --- | --- | --- |
| Production | `internal/clients/config_lock.go` | Wrapper-only one-target, multi-entry dependency authorization |
|  | `internal/api/lsp_client_router.go` | Frozen forward plan and canonical dependency predicates for each legacy removal |
|  | `internal/api/lsp_client_router_snapshot.go` | Full rollback groups, legacy dependency predicates, exact inverses |
|  | `internal/api/serena_client_reconcile.go` | Serena inverse consumes retained `BaselineBytes`, never reopens a pin path |
|  | `internal/cli/install_reconcile_mcp_front.go` | Strict row schema, classifier, forward/rollback policies, pin orchestration, independent progress, retirement |
|  | `internal/api/pin_windows.go` | Root-handle-relative Windows no-reparse bounded reader |
|  | `internal/api/pin_posix.go` | Root-FD-relative POSIX no-follow bounded reader |
| Test | `internal/clients/config_lock_wrapped_test.go` | Wrapper group-dependency and capability/fail-closed tests |
|  | `internal/api/lsp_client_router_plan_test.go` | Forward dependency boundary and frozen-plan tests |
|  | `internal/api/lsp_client_router_snapshot_review_test.go` | Rollback dependency boundary/group retry tests |
|  | `internal/api/serena_client_reconcile_test.go` | Exact retained-byte inverse and path-not-reopened tests |
|  | `internal/cli/install_reconcile_mcp_front_v3_test.go` | F2, conflict, pin, policy, independent progress, schema, retirement matrices |
|  | `internal/cli/install_reconcile_mcp_front_pr588_r2_test.go` | Command-owner v1/v2 forward/rollback byte/no-write refusal and protected CLI guards |
|  | `internal/api/pin_windows_test.go` | Windows root/intermediate/final reparse, swap, size, cleanup matrix |
|  | `internal/api/pin_posix_test.go` | POSIX root/intermediate/final symlink, no-follow, swap, size, cleanup matrix |

No other production or test file is admitted.
`internal/api/state_read_caps.go` remains the unchanged owner of
`maxStateFileBytes = 1 << 20`; `internal/cli/install.go`,
`internal/cli/route.go`, state-path resolution, scheduler, GUI, tray,
supervisor, daemon, and `.codegraph*` remain excluded. Widening is
`REVISE(architect)`.

All R3 production/tests are atomic revert group `RG-PR588-V3-R3`. Before commit,
recovery uses only inverse `apply_patch` plus exact SHA-256 equality. A
commit-level rollback requires user direction because checkout, reset, and
stash are forbidden.

### Finding and reliability map

| ID | Implementation phase | Mandatory executable falsifier |
| --- | --- | --- |
| AR2-01 | R3-A wrapper group authorization | Forward canonical-dependency edit and rollback legacy-dependency edit each invoke zero unsafe target mutations |
| AR2-02 | R3-B one classifier/two policies | Full F2 table; `prepared` and `post-write-conflict` both block changed-plan forward replacement |
| AR2-03 | R3-C secure pin readers/orchestration | Windows/POSIX no-follow matrices, pin swap after load, full malformed-pin matrix |
| AR2-04 | R3-B durable Serena conflict | First-generation pinless conflict persists; prior receipt/pin survives later no-write conflict |
| AR2-05 | R3-B rollback policy | Mixed uncertain/applied Serena rows and LSP groups complete all independent safe inverses |
| AR2-06 | R3-D integration | Full F2/conflict/pin/version/dependency/retirement matrices execute through real owners |
| RR3-01 | R3-C platform readers | Root/intermediate/final/swap/size/cleanup target-platform tests consume one bounded final handle/FD |
| RR3-02 | R3-B conflict/admission lifecycle | Prior receipt remains sole inverse authority; any disposition blocks same/changed forward replay |

## Phase R3-A — same-lock multi-entry dependency authorization

Owner: `$backend-engineer`

Depends on: R3-0 clear

Revert: `RG-PR588-V3-R3`

1. Replace the target-only conditional capability with wrapper-only
   `ConditionalEntryGroupMutation`: one target, ordered same-config dependency
   predicates, optional backup, durable callback, one target add/remove, and
   complete target/dependency readback inside one `withConfigLock`.
2. Refuse different-config dependencies or missing group capability before
   backup/prepare/mutation. Concrete adapters and callers cannot emulate the
   capability.
3. Forward canonical add is target-only; every legacy remove requires the exact
   canonical intended front-route dependency under the same lock.
4. Rollback canonical add/remove requires every exact routable legacy baseline
   under the same lock. In-memory readiness may order/report but cannot
   authorize.
5. Preserve one adapter invocation maximum per admitted row and lock order:
   operation → one config lock → short journal lock → target mutation.

Endangered invariants/guards:

| ADR item | Exact guard and expected observation |
| --- | --- |
| I3 frozen population | Existing C10 guard; no re-enumeration and zero calls for a newly appearing client |
| I5 durable prepare | Real group seam prepare failure produces `Invoked=false` and zero target calls |
| I8 route preservation | `TestMCPFrontR3_GroupMutationRejectsChangedForwardCanonical` and `TestMCPFrontR3_GroupMutationRejectsChangedRollbackLegacy` |
| R3-1 | Change/remove canonical at the forward dependency boundary; legacy remove count `0`, at least one route remains |
| R3-2 | Delete/disable/replace legacy at rollback authorization; canonical inverse count `0`, canonical remains |

- **R3A-AC1:** Group observation includes target plus every ordered dependency
  from the same config owner, and exactly one target operation can be invoked.
- **R3A-AC2:** Forward dependency edit subcases `changed`, `removed`, and
  `disabled` each preserve the legacy entry and invoke zero legacy removals.
- **R3A-AC3:** Rollback dependency edit subcases `deleted`, `disabled`, and
  `replaced` each preserve canonical and invoke zero canonical inverses.
- **R3A-AC4:** A different-config dependency and a test adapter without the
  wrapper group capability both fail closed with zero target mutations.

## Phase R3-B — classifier, conflict lifecycle, and independent rollback

Owner: `$backend-engineer`

Depends on: R3-A observation shape fixed

Revert: `RG-PR588-V3-R3`

1. Implement one pure classifier covering no attempt, `prepared`,
   `post-write-conflict`, no-invocation `precondition-conflict`,
   `confirmed-no-write`, valid `applied`, and invalid/unknown.
2. Forward first refuses any pending or terminal rollback disposition, for
   byte-identical and changed plans. Otherwise every uncertain/invalid row
   blocks generation, plan, pin, row, and client changes.
3. Rollback durably marks uncertainty without early return: one Serena row or
   one LSP group blocks only itself; process every independent safe row/group in
   stable order, then aggregate remaining identities.
4. Persist first-generation Serena precondition conflict as the exact pinless,
   authority-free terminal row. With prior ownership, retain exact `Applied`
   receipt and pin, write a pending no-write conflict, and permit inverse only
   against that prior receipt.
5. Only explicit rollback advances dispositions. Forward never clears them.
   Retirement still requires durable all-terminal re-read.

Endangered invariants/guards:

| ADR item | Exact guard and expected observation |
| --- | --- |
| I1 immutable baseline | Existing C1/C2 guards compare first baseline bytes after retries |
| I2 row-only authority | Strict row/projection tests; report port/projection cannot authorize |
| I4 causation | `TestMCPFrontR3_F2CausationMatrix`, exactly 24 surface/state subcases |
| I6 exact receipt | Same-call observed intended state creates the only current receipt |
| I7 CAS inverse | Prior receipt conflict matrix permits inverse only while exact live match remains |
| I10 monotonic recovery | `TestMCPFrontR3_PriorReceiptConflictAndForwardAdmissionMatrix` |
| I11 independent progress | `TestMCPFrontR3_RollbackContinuesIndependentSafeWork` |
| R3-3 | `prepared` and `post-write-conflict` preserve generation/plan/pins and invoke zero adapters on changed-plan retry |
| R3-4 | One uncertain Serena row/LSP group stays pending while an independent owned row/group restores |
| R3-7 | First-generation conflict is durable, pinless, no-write, inverse-free |
| R3-8 | Full F2 table never promotes re-entry equality |
| R3-10 | Pending/failed/uncertain durable row prevents retirement |
| R3-11 | No-invocation conflict creates no ownership, retains prior ownership, and cannot be cleared by forward |

- **R3B-AC1:** F2 executes eight durable-state rows for each of Serena add, LSP
  add, and LSP remove—exactly 24 subcases—with the receipt/inverse counts in the
  ADR table.
- **R3B-AC2:** Conflict/policy matrix covers `prepared`,
  `post-write-conflict`, unknown, applied-without-receipt, pending disposition,
  terminal disposition, prior-receipt conflict with matching live state, and
  prior-receipt conflict with diverged live state.
- **R3B-AC3:** First-generation conflict persists with zero pin/receipt/inverse
  calls; prior-owned conflict preserves receipt and pin byte-for-byte.
- **R3B-AC4:** Mixed rollback runs restore and verify the independent Serena row
  and independent LSP group before returning aggregate pending identities.
- **R3B-AC5:** Repeated entry contains no retry/backoff loop and invokes each
  admitted adapter at most once.

## Phase R3-C — one secure Serena pin read and exact-byte inverse

Owner: `$backend-engineer`

Depends on: R3-B exact pin-requiring row classifier

Revert: `RG-PR588-V3-R3`

1. Add `internal/api/pin_windows.go`: open the root with
   `NtCreateFile`, `FILE_OPEN_REPARSE_POINT`, `OBJ_DONT_REPARSE`, and
   directory-only options; open every child relative to the validated parent
   handle; reject every reparse handle; require directory intermediates and a
   regular final object.
2. Add `internal/api/pin_posix.go`: open no-follow root directory, walk with
   parent-relative `openat(..., O_NOFOLLOW|O_CLOEXEC)`, require
   `O_DIRECTORY` intermediates and regular final `fstat`.
3. Both readers use unchanged `maxStateFileBytes` through
   `io.LimitReader(..., cap+1)`, hash/read the final handle exactly once, and
   close root/intermediate/final resources on success, error, cancellation,
   oversize, and checksum refusal.
4. CLI validates row/pin agreement and exact declared set, securely loads all
   required pins before the first inverse, retains an immutable row-key →
   verified-bytes map, and passes bytes—not paths—to Serena inverse.
5. Serena inverse accepts `BaselineBytes`; the v3 rollback path never reopens
   `BackupPath`. Lexical/final-path checks are diagnostics/defense only, never
   authorization.

Endangered invariants/guards:

| ADR item | Exact guard and expected observation |
| --- | --- |
| I9 exact verified bytes | `TestMCPFrontR3_PinSwapAfterLoadUsesRetainedBytes`; replacement path is never read |
| R3-5 | Retained bytes passed to inverse equal the bytes hashed from the single secure final read |
| R3-6 / RR3-01 | Target-platform root/intermediate/final link/reparse, component swap, size, and cleanup matrices |

- **R3C-AC1:** Windows target run executes root, intermediate, final reparse,
  component-swap, non-regular, >1 MiB, cancellation, and cleanup cases; every
  refusal occurs before client mutation and handle-leak count is zero.
- **R3C-AC2:** POSIX target run executes root, intermediate, final symlink,
  explicit no-follow, component-swap, non-regular, >1 MiB, cancellation, and
  cleanup cases; every refusal occurs before mutation and FD-leak count is zero.
- **R3C-AC3:** The complete 17-row Serena pin matrix covers missing, extra,
  LSP-attached, duplicate, lexical escape, root/intermediate/final link,
  component swap, unreadable/non-regular, oversize, checksum, metadata, pin-set
  disagreement, post-load swap, cleanup, and exact pinless conflict.
- **R3C-AC4:** Post-load path replacement does not affect the inverse:
  consumed bytes equal the retained verified bytes; replacement read count is
  zero.
- **R3C-AC5:** `state_read_caps.go` is diff-free and no duplicate size cap
  exists in CLI or either platform reader.

Target-platform test execution is mandatory. Windows success does not substitute
for POSIX execution or vice versa; absence of a target runner is `BLOCKED`, not
PASS or skip.

## Phase R3-D — command-owner integration and complete acceptance matrices

Owner and integration owner: `$backend-engineer`

Depends on: R3-A through R3-C green on available target; POSIX/Windows target
matrix may complete at QA but may not be waived

Revert: `RG-PR588-V3-R3`

1. Integrate strict version-3 decode, row validation, classifier, forward
   admission, pin load, independent rollback, durable dispositions, verification,
   aggregate return, and retirement in the accepted order.
2. Command-owner forward and rollback for v1/v2 decode only the version,
   preserve exact report bytes, perform zero client/state mutations, and return
   `legacy-ownership-unproven`.
3. Strict v3 rejects unknown top-level projections, trailing values, incomplete
   rows, malformed attempt/receipt pairs, illegal pins, and interim R2 shapes.
4. Preserve C1/C2/C4/C8/C9/C10 and protected C3/C5/C6/C7 without changing their
   oracles. `install.go`, `route.go`, operation-lock scope, and total preflight
   remain unchanged.
5. Remove every R3 actor's temporary diagnostic; foreign `TEMPDEBUG` remains
   excluded and must have been resolved by R3-0, not silently incorporated.

Invariant/guard completion:

| ADR invariant | Implementation step and mandatory guard |
| --- | --- |
| I1 | Row merge; C1/C2 first-baseline guard |
| I2 | Strict v3 row validator; projection/report-port mutation guard |
| I3 | Frozen LSP plan; C10 appearing-client guard |
| I4 | Classifier; 24-case F2 table |
| I5 | Group/Serena durable callback; real-seam prepare failure guard |
| I6 | Same-call finish transition; exact receipt tests |
| I7 | Serena/LSP expected-live inverse; C4 and prior-receipt matrices |
| I8 | Wrapper dependencies; forward/rollback boundary race tests |
| I9 | Secure readers plus retained-byte inverse; platform/pin-swap tests |
| I10 | Forward admission/disposition/retirement; conflict-policy matrix |
| I11 | Stable independent rollback loops; two mixed-progress tests |
| I12 | Command owners; v1/v2 × forward/rollback exact-byte/no-write table |

- **R3D-AC1:** `TestMCPFrontR3_CommandOwnersRefuseV1V2ByteIdentical` runs four
  command cases—v1/v2 × forward/rollback—with exact byte equality and zero
  adapter/report writes.
- **R3D-AC2:** Strict-v3 malformed matrix rejects every ADR shape before the
  first client mutation.
- **R3D-AC3:** Every R3-1 through R3-11 guard reaches its real owner; helper-only
  state tests do not count.
- **R3D-AC4:** Existing original C1-C10 and protected C3/C5/C6/C7 test sets
  remain green.
- **R3D-AC5:** `git diff --check` exits 0; exactly seven production and eight
  test paths are in the R3 diff; excluded/foreign path count is zero.

Exact restored Windows/local commands:

```powershell
Invoke-PR588Go -GoArgs @(
    'test', '-v', '-tags=test_state_path_env', '-count=1', '-timeout', '10m',
    '-run',
    '^(TestMCPFrontR3_.*|TestMCPFrontV3_.*|TestMCPFrontR2_(CheckWithReconcileMutatesNothing|SecondInvocationRefusesWhileTheTransactionLockIsHeld|ForwardRefusesWhenOnlyTheSerenaRouteIsLive)|TestRouteDaemon_Session(StoresAreReachableForExpiry|ExpiryActuallyReclaimsBoundSessions|ExpiryStopsWithContext))$',
    './internal/cli/'
)

Invoke-PR588Go -GoArgs @(
    'test', '-v', '-tags=test_state_path_env', '-count=1', '-timeout', '10m',
    '-run',
    '^(TestMCPFrontR3_.*|TestMCPFrontV3_.*|TestReadSerenaPinBeneathRootNoFollow_.*|TestSnapshotRestore_.*|TestSnapshotLSPRouterClientEntries_CapturesLegacyPerWorkspaceEntries|TestSerena(ClientReconcile_.*|Reconcile_.*))$',
    './internal/api/'
)

Invoke-PR588Go -GoArgs @(
    'test', '-v', '-tags=test_state_path_env', '-count=1', '-timeout', '10m',
    '-run',
    '^(Test(AllClientsAreLockWrapped|ConditionalEntryGroupMutation_.*|CAS.*))$',
    './internal/clients/'
)
```

The CLI regex is mandatory; never run the whole CLI package without it.

Exact POSIX target command uses a new temp directory per invocation and the same
tag:

```sh
state_dir="$(mktemp -d)"
MCPHUB_STATE_DIR_OVERRIDE="$state_dir" \
  go test -v -tags=test_state_path_env -count=1 -timeout 10m \
  -run '^(TestReadSerenaPinBeneathRootNoFollow_POSIX.*|TestMCPFrontR3_Pin.*)$' \
  ./internal/api/
```

The target runner must verify `state_dir` is its created temp child before
cleanup. No `go test ./...` is allowed.

## Phase R3-E — independent QA and controlled defect mutations

Owner: `$qa-engineer`

Depends on: R3-D handoff hashes and both target-platform reader runs available

Production corrections return to `$backend-engineer`

Revert: immediate inverse `apply_patch` and exact pre-mutation SHA-256

For every row below: hash the production owner, apply only the named defect,
compile and fail the exact named real-owner assertion, inverse-patch
immediately, require exact hash equality, and rerun green.

| Claim | Controlled production mutation | Guard |
| --- | --- | --- |
| R3-1 | Drop canonical dependency from legacy remove | Forward group-boundary guard |
| R3-2 | Drop legacy dependencies from canonical inverse | Rollback group-boundary guard |
| R3-3 | Treat `post-write-conflict` as settled on forward admission | Uncertain-plan replacement matrix |
| R3-4 | Return globally on first uncertain row/group | Independent-progress matrix |
| R3-5 | Reopen pin path in Serena inverse | Pin-swap retained-bytes guard |
| R3-6 | Replace handle/FD-relative component open with pathname check/open | Target-platform root/intermediate/final/swap guard |
| R3-7 | Ignore missing first-generation Serena conflict row | Pinless durable-conflict guard |
| R3-8 | Promote prepared state from re-entry equality | 24-case F2 matrix |
| R3-9 | Permit a v1/v2 command owner to rewrite/retire artifact | Four-case command-owner byte/no-write guard |
| R3-10 | Retire from in-memory or nonterminal durable state | Durable all-terminal retirement guard |
| R3-11 | Clear prior receipt/pin or allow forward replay after conflict | Prior-receipt/admission matrix |

QA also preserves the original C1/C2/C4/C8/C9/C10 mutation artifacts, reruns
their current-source guards green, and reruns C3/C5/C6/C7 protected guards.

- **R3E-AC1:** All 11 mutations fail their named assertion; compile-only,
  missing-test, no-op, unrelated panic, timeout, or skip does not count.
- **R3E-AC2:** Every mutated source returns to the exact handoff SHA-256 and its
  guard reruns green.
- **R3E-AC3:** All three scoped local sets plus Windows and POSIX platform
  matrices pass with exact test/subtest counts and zero skips.
- **R3E-AC4:** With a distinct fresh override directory per command,
  `go build -tags=test_state_path_env ./...` and
  `go vet -tags=test_state_path_env ./...` exit 0. Never run
  `go test ./...`.
- **R3E-AC5:** QA proves zero unauthorized mutation, zero route-loss race,
  exact retained pin bytes, 100% independent progress, and durable all-terminal
  retirement.

Absolute safety: every API/CLI command has
`-tags=test_state_path_env` and a fresh
`MCPHUB_STATE_DIR_OVERRIDE`; no whole CLI package test, GUI, tray, supervisor,
scheduler, daemon, process kill by image name, checkout, reset, stash,
worktree creation, commit, or push.

## Phase R3-F — completion-verified external review and correction commit

Owner: `$lead`

Depends on: R3-E PASS and publication-ready target-platform evidence

1. Run distinct claim-verification and adversarial architecture lanes with
   file-based prompts against the R3 ADR/design, exact diff, QA artifact,
   original classifications, AR2-01..06, RR3-01/02, I1-I12, and R3-1..11.
2. Windows launcher owner must resolve and execute native `codex.exe` directly
   from PowerShell, wait for process completion, capture the process object's
   `ExitCode`, and write stdout/stderr/exit files. A PowerShell owner using
   `Start-Process -Wait -PassThru` with explicit redirections is acceptable.
   A `.cmd`/`.bat` parent exit file is forbidden: the two prior `.cmd` parents
   returned before the Node/Codex child and are not completion oracles.
3. Each counted lane requires native exit `0`, nonempty output, requested
   provenance/header, final `PASS`, and no auth/quota/truncation/timeout marker.
   If native `codex.exe` cannot be actively resolved, the gate is `BLOCKED`;
   do not substitute the failed `.cmd` oracle.
4. Any `REVISE` returns to its owner and repeats affected mutations, complete QA,
   and both external gates.
5. After two PASS verdicts: promote ADR status to `accepted`; reconcile all 14
   findings, C1-C10, AR2/RR3, invariants, mutations, target-platform evidence,
   foreign-overlap disposition, and safety incident; run publication/leak scan;
   stage only accepted task-owned files; create one new local correction commit
   atop `31b9ca94` or its explicitly accepted descendant. Do not amend or push.

- **R3F-AC1:** Both external lanes have native exit `0`, valid output, and
  `PASS`; neither uses `.cmd` as its parent oracle.
- **R3F-AC2:** Publication scan finds zero secret, token, raw backup, environment
  value, machine-local path, `TEMPDEBUG`, `.codegraph*`, or unrelated/foreign
  hunk in staged content.
- **R3F-AC3:** New local correction commit has parent `31b9ca94...` or an
  explicitly accepted descendant and names original findings plus AR2-01..06
  and RR3-01/02. History is not rewritten.
- **R3F-AC4:** Final report includes classifications, class/invariant maps,
  11 red/green mutation proofs, exact hashes, target-platform tests, build/vet,
  both review oracles, safety incident, commit hash, and `not pushed`.
- **R3F-AC5:** The PR work-item stays active for human review/publication.

### R3 recommended sequence and gate

`$lead` clears R3-0 foreign overlap → `$backend-engineer` implements R3-A
through R3-D as integration owner → `$qa-engineer` runs R3-E → independent
claim and adversarial `$architecture-reviewer` lanes → `$lead` publication scan
and new local correction commit without push.

**Plan gate: PASS; implementation BLOCKED at R3-0 until the current foreign
overlapping API/debug changes are resolved by their owner or explicitly admitted
by the user.** After that clearance, the plan is backend-eligible.

## R2 correction amendment — historical execution contract

This amendment is the active plan for the post-QA architecture corrections
F1-F4 and A-01-A-03. It consumes
`work-items/decisions/2026-07-27-mcp-front-reconcile-v3-row-journal.md`
at SHA-256
`42A3FBE24E4E87EA7EF1D5A2E59BEC895DF05C310C740222492E8A8AC3776B62`
and `architecture-r1-live-findings-2026-07-27.md`. The original Phases A-G
below are retained as the completed R1 implementation/QA record. Where they
conflict with this amendment or the decision record, they are superseded and
must not drive a new edit, test oracle, review verdict, or commit.

In particular, original A-AC3, D-AC1, D-AC2, and the original
prepared-attempt settlement wording are retired: a durable `prepared` or
no-write `precondition-conflict` state is never promoted from later value
equality. The R2 row transition contract below replaces those criteria without
renumbering them.

### R2 allowed change surface

| Kind | Exact path | R2 owner |
| --- | --- | --- |
| Production | `internal/clients/config_lock.go` | Generic lock-scoped `ConditionalEntryMutator`; exact read/compare, optional backup, durable prepare callback, one typed add/remove, and readback under one `withConfigLock` call |
|  | `internal/clients/cas_mutator.go` | Retain the existing nine-adapter Serena rollback restore/remove allowlist; no generic adapter admission |
|  | `internal/api/serena_client_reconcile.go` | Serena forward through the conditional mutation seam; present-baseline restore and absent-baseline guarded-remove inverse |
|  | `internal/api/lsp_client_router.go` | LSP forward canonical add and legacy remove through the conditional mutation seam |
|  | `internal/api/lsp_client_router_snapshot.go` | LSP rollback through the conditional seam; full persisted dependency-group reconstruction and live route-readiness owner |
|  | `internal/cli/install_reconcile_mcp_front.go` | Sole version-3 row journal, row-owned pin validation, same-call attempt transition, complete rollback input, durable disposition and retirement gate; remove stale projections/helpers |
| Test | `internal/clients/config_lock_wrapped_test.go` | Conditional capability/factory matrix, one-lock mutation order, missing-capability fail-closed behavior |
|  | `internal/clients/cas_mutator_test.go` | Exact unchanged CAS allowlist and Serena present/absent inverse polarity |
|  | `internal/api/serena_client_reconcile_test.go` | Serena conditional-forward and absent-baseline owned-remove real seams |
|  | `internal/api/lsp_client_router_plan_test.go` | Conditional LSP add/remove real seams and durable-prepare ordering |
|  | `internal/api/lsp_client_router_snapshot_review_test.go` | Full dependency group, retry barrier, baseline-only live readiness, and C9 unreachable ownership |
|  | `internal/cli/install_reconcile_mcp_front_v3_test.go` | F2, A-01, row schema, pin authority, and all original version-3 guards |
|  | `internal/cli/install_reconcile_mcp_front_review_test.go` | Caller-level durable re-read/retirement guard and existing review fixtures |
|  | `internal/cli/install_reconcile_mcp_front_pr588_test.go` | Remove or rewrite only tests that consume superseded top-level projections or `mergeMCPFrontReconcileReport` |
|  | `internal/cli/install_reconcile_mcp_front_pr588_r2_test.go` | Remove or rewrite only the stale `newMCPFrontReconcileJournal` helper test; retain C3/C5/C6 guards |

No other production or test file is admitted. A required extra file is
`REVISE` to the architect, not an implicit expansion.

Protected behavior and files:

- `internal/cli/install.go` and `internal/cli/route.go` remain diff-free;
- the operation-level reconcile lock and total Serena/LSP preflight inside
  `internal/cli/install_reconcile_mcp_front.go` remain byte-behaviorally
  unchanged;
- exact first baseline, per-row receipt port, frozen LSP population,
  canonical-before-legacy forward ordering, version-1/version-2 no-write
  refusal, C1/C2/C4/C8/C9/C10 closures, and all C3/C5/C6/C7 guards remain
  binding;
- no public flag, client-config format, dependency, Graphical User Interface
  (GUI), tray, supervisor, scheduler, route lifecycle, registry, or second
  recovery artifact is added;
- no new adapter is admitted to `CASEntryMutator`.

All R2 production and test changes form atomic revert group
`RG-PR588-V3-R2`. The row schema, wrapper mutation boundary, API callbacks,
rollback group semantics, and their tests ship or revert together.

## Phase R2-A — wrapper-owned conditional mutation and real-seam ordering

Owner: `$backend-engineer`  
Depends on: accepted-ready R2 decision and this amended plan  
Revert: `RG-PR588-V3-R2`

Work:

1. Add `clients.ConditionalEntryMutator` and its request/observation value types
   in `internal/clients/config_lock.go`; implement it only on `*lockingClient`.
2. Under one `withConfigLock` call, perform exact live read, matcher check,
   optional `BackupKeep`, `BeforeMutation`, exactly one add/remove, and
   post-state read. Return `Invoked=false` for precondition, capability, backup,
   or durable-prepare failure.
3. Keep lock order exactly
   `reconcile operation lock -> client config lock -> recovery state-file lock`.
   The callback may persist the row but may not mutate the client or retain the
   unwrapped adapter.
4. Route Serena forward plus every LSP forward/rollback add/remove through this
   capability. Capability absence is durable pending/structural failure and
   zero client writes.
5. Keep Serena rollback restore/remove on the existing `CASEntryMutator`
   capability; prove the generic conditional capability covers every production
   factory while the CAS allowlist remains exactly its current nine adapters.

Diff-invisible invariants and named guards copied from the decision:

- no concurrent hub edit is overwritten outside one adapter-owned critical
  section:
  `TestMCPFrontV3_ConditionalMutationRejectsInterveningEdit`;
- no real adapter call occurs before durable prepare:
  `TestMCPFrontV3_RealMutationSeamsRequireDurablePrepare`;
- every production factory is lock-wrapped and conditionally mutable while CAS
  membership is unchanged:
  `TestAllClientsAreLockWrapped` plus the existing `TestCAS*` allowlist set.

Acceptance:

- **R2A-AC1:** The F1 table executes Serena add, LSP canonical add, LSP legacy
  remove, and LSP rollback add/remove through the real API mutation owners; an
  injected intervening edit remains byte-identical and the hub mutation count
  is exactly zero.
- **R2A-AC2:** A real Serena or LSP prepare-publication failure returns
  `Invoked=false`, yields exactly zero adapter add/remove calls, and leaves a
  durable pending/error result.
- **R2A-AC3:** Every production client factory returns an object satisfying
  `ConditionalEntryMutator`; the exact existing nine satisfy
  `CASEntryMutator`, every previously excluded adapter remains excluded, and no
  concrete adapter implements the conditional seam directly.
- **R2A-AC4:** No code path from the admitted forward/rollback operations calls
  ordinary `AddEntry` or `RemoveEntry` after a separate point-in-time
  authorization check.

Narrow verification:

```powershell
Invoke-PR588Go -GoArgs @(
    'test', '-v', '-tags=test_state_path_env', '-count=1', '-timeout', '10m',
    '-run',
    '^(Test(AllClientsAreLockWrapped|CAS.*|MCPFrontV3_Conditional.*))$',
    './internal/clients/'
)

Invoke-PR588Go -GoArgs @(
    'test', '-v', '-tags=test_state_path_env', '-count=1', '-timeout', '10m',
    '-run',
    '^(TestMCPFrontV3_(ConditionalMutationRejectsInterveningEdit|RealMutationSeamsRequireDurablePrepare)|TestSerena(Reconcile_.*|ClientReconcile_.*))$',
    './internal/api/'
)
```

Expected fresh evidence: each named top-level test is enumerated, both commands
exit 0, and the API command uses its own fresh state directory.

## Phase R2-B — sole row authority, causal receipts, and stale-owner removal

Owner: `$backend-engineer`  
Depends on: Phase R2-A observation contract fixed  
Revert: `RG-PR588-V3-R2`

Work:

1. Make version-3 `Rows` the only persisted mutation authority. Move the exact
   Serena baseline, pin path/origin/SHA-256, latest attempt, applied receipt,
   and rollback disposition onto its authoritative row.
2. Decode a minimal version envelope first. Versions 1 and 2 remain
   byte-identical no-write refusals; only the strict version-3 row shape may
   drive mutation.
3. Remove top-level version-3 `Serena`, `LSP`, `Pins`, `Applied`, and `Port`
   decision fields. Keep `MigrateReport` display/result-only.
4. Before the first rollback write, require every Serena row to resolve to one
   nonempty, unique pin path/checksum under the report pin directory and verify
   the bytes. Missing, extra, duplicate, escaped, unreadable, changed, or
   disagreeing pins reject the complete rollback with zero writes.
5. Replace equality-based settlement with a single same-call transition owner:
   only `EntryMutationObserved{Invoked=true}` plus exact intended readback and
   durable publication creates an applied receipt. A no-invocation
   precondition conflict never promotes. A durable prepared state surviving
   re-entry remains `pending-ownership-unknown`.
6. Remove `newMCPFrontReconcileJournal`, stale journal `commit`/fingerprint
   projections, `verifyMCPFrontSerenaNotEdited`,
   `mergeMCPFrontReconcileReport`, and tests whose only purpose is those
   superseded owners.

Diff-invisible invariants and named guards copied from the decision:

- durable intent or no-write conflict is not mutation causation:
  `TestMCPFrontV3_NoInvocationStateEqualityNeverCreatesReceipt`;
- every Serena row owns one verified pin before any write:
  `TestMCPFrontV3_RowsExclusivelyOwnSerenaPins`;
- v1/v2 remain exact no-write refusals:
  `TestMCPFrontV3_V1AndV2ArtifactsRefuseBeforeAnyWrite`;
- C1 exact identity, C2 per-row receipt, C8 post-success promotion, immutable
  first baseline, and frozen population retain their existing named guards.

Acceptance:

- **R2B-AC1:** Serena add, LSP add, and LSP remove cases with zero mutation
  invocations and externally intended live values produce no applied receipt,
  perform zero rollback writes, and remain pending/unowned on re-entry.
- **R2B-AC2:** Every malformed-pin case named by A-01 rejects before the first
  client write, retains the active artifact, and reports no backup bytes.
- **R2B-AC3:** The version-3 persisted type has exactly one row authority and
  zero compatibility projection fields; a reference sweep finds zero live
  production/test references to the four superseded helper families.
- **R2B-AC4:** Original C1/C2/C8 and v1/v2 guards remain green without changing
  their behavior oracle to accommodate R2.

## Phase R2-C — Serena exact inverse and durable LSP dependency groups

Owner: `$backend-engineer`  
Depends on: R2-A mutation owner and R2-B authoritative row type  
Revert: `RG-PR588-V3-R2`

Work:

1. Branch Serena rollback on immutable baseline presence: present uses
   `CASRestoreEntryFromBytesForRollback`; absent uses
   `CASGuardedRemoveEntry`; both match the effective applied fingerprint and
   verify the exact baseline after success.
2. Reconstruct each LSP `(client, language)` rollback group from all persisted
   rows on every call, including terminal conflict, restored, baseline-only,
   pending, failed, and unreachable rows. The CLI must not filter terminal rows
   before the API group owner.
3. Implement one `legacyRouteReady` predicate from exact live state and adapter
   route semantics. Restored and baseline-only legacy rows count ready only
   while their live entry still equals the immutable routable baseline.
4. Preserve canonical when any legacy row is unreadable, missing, disabled,
   non-routable, pending, failed, or conflicted. Retryable rows keep canonical
   pending; terminal conflicts produce durable
   `skipped-dependency-conflict` and stable non-success.
5. Make the old snapshot restore helper conservatively keep an applied or
   uncertain absent row pending while unreachable; no legacy helper may retire
   a record by treating omitted/unreachable ownership as satisfied.

Diff-invisible invariants and named guards copied from the decision:

- every legal Serena transition has an ownership-checked inverse:
  `TestMCPFrontV3_SerenaAbsentBaselineUsesOwnedRemove`;
- every retry reconstructs legacy readiness from every row:
  `TestMCPFrontV3_LSPDependencyBarrierSurvivesRetry`;
- C4 changed-entry preservation and C9 absent/unreachable ownership retain
  `TestMCPFrontV3_SerenaCASRestoresLegacyHubBackupAndRefusesConcurrentEdit`
  and
  `TestSnapshotRestore_AppliedOrUncertainAbsentBaselineUnreachableIsPending`.

Acceptance:

- **R2C-AC1:** An absent-to-owned Serena add rolls back to verified absence; a
  changed replacement is byte-identical after rollback and yields conflict.
- **R2C-AC2:** A terminal legacy conflict blocks canonical inversion on rollback
  call one and remains a blocker on call two; canonical receives exactly zero
  inverse mutations across both calls.
- **R2C-AC3:** Missing, unreachable, disabled, non-routable, and exact-routable
  baseline-only legacy cases each produce the decision-prescribed pending,
  dependency-conflict, or ready result; omission never counts ready.
- **R2C-AC4:** A terminal dependency conflict may retire only after every row is
  terminal, but command output remains non-success and names every skipped row
  without raw values.

## Phase R2-D — CLI integration, durable retirement, and backend handoff

Owner and integration owner: `$backend-engineer`  
Depends on: R2-A through R2-C green  
Revert: `RG-PR588-V3-R2`

Work:

1. Wire forward and rollback through the one conditional mutation owner and one
   row transition owner; no parallel settlement implementation remains.
2. Validate all row-owned Serena pins before the first inverse, classify
   unresolved prepared rows pending, pass all LSP rows to group rollback, and
   persist every observed disposition before a dependent inverse.
3. Re-read through `ReadStateFileInodeAnchored`, compute retirement only from
   that durable object, atomically retire only a fully terminal artifact, and
   return non-success for preserved terminal conflicts.
4. Keep operation serialization, preflight, first baselines, receipt ports,
   frozen population, dependency ordering, and the original ten-class closure
   guards unchanged.
5. Run `gofmt` only on the admitted Go files, run `git diff --check`, inventory
   every changed path against the R2 allowlist, and hand exact SHA-256 hashes to
   independent QA.

Diff-invisible invariants and named guards:

- retirement requires the caller's durable re-read:
  `TestMCPFrontV3_RollbackCallerRereadsDurableStateBeforeRetirement`;
- real mutations require durable prepare:
  `TestMCPFrontV3_RealMutationSeamsRequireDurablePrepare`;
- C3/C5/C6/C7 retain
  `TestMCPFrontR2_CheckWithReconcileMutatesNothing`,
  `TestMCPFrontR2_SecondInvocationRefusesWhileTheTransactionLockIsHeld`,
  `TestMCPFrontR2_ForwardRefusesWhenOnlyTheSerenaRouteIsLive`, and the three
  `TestRouteDaemon_Session*` guards.

Acceptance:

- **R2D-AC1:** Durable publication failure or a durable pending row leaves the
  active path present and retirement count exactly zero even when stale
  in-memory state is terminal.
- **R2D-AC2:** Every forward/rollback return path maps to one explicit durable
  row state and nonzero/success result; no mutation or publication error is
  swallowed.
- **R2D-AC3:** `git diff --check` exits 0; every changed `internal/**` path is in
  the R2 allowlist; `internal/cli/install.go` and `internal/cli/route.go` are
  diff-free and no hunk changes the reconcile lock or preflight owners.
- **R2D-AC4:** The implementation handoff records exact file hashes and the
  green scoped command output; no commit or push occurs before QA and both
  architecture gates.

Restored integration commands:

```powershell
Invoke-PR588Go -GoArgs @(
    'test', '-v', '-tags=test_state_path_env', '-count=1', '-timeout', '10m',
    '-run',
    '^(TestMCPFrontV3_.*|TestMCPFrontR2_(CheckWithReconcileMutatesNothing|SecondInvocationRefusesWhileTheTransactionLockIsHeld|ForwardRefusesWhenOnlyTheSerenaRouteIsLive)|TestMCPFrontReview_(ClientIsNotMutatedWhenItsRecoveryRowCannotBeDurable|RollbackKeepsTheRecordWhileAnyRowIsPending|RollbackFailsWhenTheRecordCannotBeRetired|RetirementClearsTheActiveNamespace)|TestRouteDaemon_Session(StoresAreReachableForExpiry|ExpiryActuallyReclaimsBoundSessions|ExpiryStopsWithContext))$',
    './internal/cli/'
)

Invoke-PR588Go -GoArgs @(
    'test', '-v', '-tags=test_state_path_env', '-count=1', '-timeout', '10m',
    '-run',
    '^(TestMCPFrontV3_.*|TestSnapshotRestore_.*|TestSnapshotLSPRouterClientEntries_CapturesLegacyPerWorkspaceEntries|TestMCPFrontLegacyLSP_ForwardThenRollbackRestoresTheLegacyEntry|TestSerenaClientReconcile_.*|TestSerenaReconcile_.*)$',
    './internal/api/'
)

Invoke-PR588Go -GoArgs @(
    'test', '-v', '-tags=test_state_path_env', '-count=1', '-timeout', '10m',
    '-run',
    '^(Test(CAS.*|AllClientsAreLockWrapped|MCPFrontV3_Conditional.*))$',
    './internal/clients/'
)
```

No command may run the whole CLI package without this exact narrow `-run`
selection. Each command gets a distinct fresh state directory.

## Phase R2-E — independent QA and controlled correction mutations

Owner: `$qa-engineer`  
Depends on: R2-D implementation handoff and exact source hashes  
Writes: inverse production patches only for controlled proof; any real source
correction returns to `$backend-engineer`  
Revert: mutation restoration is immediate inverse `apply_patch` plus exact
SHA-256 equality; delivery remains `RG-PR588-V3-R2`

QA must retain the original six C1/C2/C4/C8/C9/C10 mutation artifacts as
accepted historical evidence, rerun all their restored guards, and add the
following current-source real-seam proofs:

| Class | Controlled production defect | Exact guard that must fail |
| --- | --- | --- |
| F1 | Replace a conditional API mutation with the former split live-read plus ordinary add/remove path | `TestMCPFrontV3_ConditionalMutationRejectsInterveningEdit` |
| F2 | Promote durable prepared or no-write precondition conflict when external live state equals intended | `TestMCPFrontV3_NoInvocationStateEqualityNeverCreatesReceipt` |
| F3 | Send absent Serena baseline through restore-from-bytes or ordinary remove | `TestMCPFrontV3_SerenaAbsentBaselineUsesOwnedRemove` |
| F4 | Filter terminal legacy rows before group reconstruction or treat omitted/baseline-only rows as ready | `TestMCPFrontV3_LSPDependencyBarrierSurvivesRetry` |
| A-01 | Validate a top-level compatibility pin projection instead of every authoritative row | `TestMCPFrontV3_RowsExclusivelyOwnSerenaPins` |
| A-02 mutation order | Move a real Serena or LSP adapter call before `BeforeMutation` succeeds | `TestMCPFrontV3_RealMutationSeamsRequireDurablePrepare` |
| A-02 retirement | Retire from the caller's in-memory result instead of the durable re-read | `TestMCPFrontV3_RollbackCallerRereadsDurableStateBeforeRetirement` |

For each proof, QA records pre-mutation production SHA-256, applies only the
production defect, requires the exact test to compile and fail at its named
assertion, immediately applies the inverse patch, requires exact hash equality,
and reruns the exact test green. Test source, expected values, regex, tag,
timeout, and runner may not be mutated.

Acceptance:

- **R2E-AC1:** All seven correction mutations fail their exact real-seam guard;
  no compile-only, no-op, missing-test, unrelated panic, or timeout counts.
- **R2E-AC2:** Every mutated production file returns to its exact R2 handoff
  SHA-256 and every guard reruns green.
- **R2E-AC3:** The complete restored R2-D CLI/API/clients sets pass, and the
  original C1/C2/C4/C8/C9/C10 guards remain green.
- **R2E-AC4:** With a distinct fresh state directory for each command,
  `go build -tags=test_state_path_env ./...` and
  `go vet -tags=test_state_path_env ./...` both exit 0; no
  `go test ./...` is run.
- **R2E-AC5:** QA records exact PASS/FAIL counts, raw output paths, hash
  restoration, changed-file allowlist, and maps evidence to R2A-AC1 through
  R2D-AC4.

Absolute QA safety: never launch GUI, tray, supervisor, scheduler, or daemon;
never kill by image name; never run a whole CLI package test; never checkout,
hard-reset, stash, create another worktree, commit, push, or clean outside the
resolved fresh `.scratch` state path.

## Phase R2-F — two external architecture gates and local commit

Owner: `$lead`  
Depends on: independent R2 QA PASS  
Source writes: none unless a review returns `REVISE`

1. Run a claim-verification `$architecture-reviewer` lane against all ten R2
   decision claims, the original ten-class closure, exact diff, and fresh QA.
2. Run a distinct adversarial `$architecture-reviewer` lane against F1-F4,
   A-01-A-03, crash/re-entry causation, pin authority, Serena absence,
   dependency retry, stale-helper removal, retirement, and protected surfaces.
3. Both direct external runs require file-based prompts, distinct outputs,
   explicit exit-code completion records, no auth/quota/truncation marker, and
   final `PASS`. An errored, unrecorded, silent, or `REVISE` lane is not a gate.
4. On any `REVISE`, return to the owning phase, repeat the affected mutation,
   restored scoped gates, independent QA, and both external architecture lanes.
5. After two PASS verdicts, promote the decision record from `proposed` to
   `accepted`, reconcile the 14 finding rows and both class-sweep tables, run
   `git diff --check`, exact status/diff/leak checks, stage only admitted
   task-owned paths, and create one local commit naming every original finding
   and the R2 F1-F4/A-01-A-03 closures. Do not push.

Acceptance:

- **R2F-AC1:** Both external lanes have exit 0, valid completion oracles, and
  `PASS` with no unresolved correctness or protected-surface finding.
- **R2F-AC2:** Publication-safety/leak inspection finds zero secret, raw backup,
  token, environment value, or machine-local path in staged content.
- **R2F-AC3:** The final report contains all 14 classifications, original and
  R2 class sweeps, original six plus new seven mutation failures/restorations,
  exact scoped/build/vet output, changed files, safety incident disclosure,
  commit hash, and explicit `not pushed`.
- **R2F-AC4:** `git status --short --branch` shows the local commit and no
  task-owned unstaged source/test change; the overarching PR work-item remains
  active for human review/publication rather than being falsely archived.

### R2 role sequence and rollback

`$backend-engineer` R2-A through R2-D and integration handoff →
`$qa-engineer` R2-E → independent claim-verification
`$architecture-reviewer` → independent adversarial
`$architecture-reviewer` → `$lead` reconciliation, decision acceptance, leak
scan, and local commit without push.

Before commit, restore a disproved edit only through inverse `apply_patch` and
exact file-hash equality. The user forbids checkout, hard reset, and stash.
After a local commit, any commit-level rollback requires operator direction.
An on-disk version-3 artifact is never downgraded: preserve it byte-for-byte
and use a version-3-aware build or operator-guided manual recovery.

R2 gate: **PASS — RETURN(lead); planner-eligible.** The correction stays inside
the accepted Change-Surface Contract, assigns one integration owner, gives
every F1-F4/A-01/A-02 correction a real-seam falsifier, preserves A-03 as a
durable decision owner, and retains every original ten-class and protected
surface gate.

## Outcome and scope

Implement the accepted version-3, row-owned front-reconcile recovery journal
and close the six open defect classes C1, C2, C4, C8, C9, and C10. Preserve
the already-closed C3, C5, C6, and C7 behavior. The admitted outcome requires
all 14 bot rows to be classified, every real class to have a production
mutation proof, scoped state-sandboxed tests, tagged broad build and vet,
independent review, and a local commit without push
(`roadmap.md:6-31`).

The accepted design assigns the complete production change surface to five
files plus tests (`design.md:94-103`). Reliability R2 confirms that all eight
required design revisions and all eleven reliability claims have named owners
and falsifiers (`reliability-live-findings-2026-07-27.md:334-378`). The plan
does not widen that contract.

## Finding classification carried into delivery

No implementation phase may reclassify a row without returning to `$analyst`
and `$lead`. The accepted classification is grounded at
`research-live-findings-2026-07-27.md:31-56`.

| Bot rows | Class | Delivery state | Closure owner |
| --- | --- | --- | --- |
| 1, 13 | C1 — legacy LSP entries | REAL, open | Exact version-3 row identity and immutable baseline merge |
| 2, 10 | C2 — latest port across retries | REAL, open | Per-row effective applied receipt and port |
| 3, 9 | C3 — `--check` dispatch | ALREADY FIXED by `3872ee16` | Preserve top-of-command gate |
| 4 | C4 — Serena rollback overwrite | REAL, open | Rollback-bypass lock-scoped compare-and-set |
| 5, 11 | C5 — whole transaction serialization | ALREADY FIXED by `3872ee16` | Preserve one operation lock |
| 6 | C6 — LSP route readiness | ALREADY FIXED by `3872ee16` | Preserve total preflight |
| 7 | C7 — route-daemon session expiry | ALREADY FIXED by `3872ee16` | Preserve route-owned cleanup |
| 8 | C8 — Serena applied-before-write | REAL, open | Prepared attempt plus post-attempt promotion |
| 12 | C9 — absent/unreachable LSP row | REAL, open | Ownership-aware pending classifier |
| 14 | C10 — snapshot/apply population race | REAL, open | Frozen LSP generation plan |

No row is `WRONG` (`research-live-findings-2026-07-27.md:56`).

## Change-Surface Contract

Allowed production files and owners:

| File | Exact owners allowed to change |
| --- | --- |
| `internal/cli/install_reconcile_mcp_front.go` | `mcpFrontReconcileReport`, journal row/plan/attempt/receipt types, report validation/read/merge, `mcpFrontReconcileJournal`, forward and rollback integration, settlement, disposition persistence, retirement eligibility, stable reason diagnostics |
| `internal/api/lsp_client_router.go` | One plan builder/applicator seam over a single captured client population, exact planned operations, dependency groups, and per-row mutation observations |
| `internal/api/lsp_client_router_snapshot.go` | Exact canonical/legacy row identity, applied-or-uncertain pending classification, row-aware rollback planning, legacy-first group ordering, and inverse readback |
| `internal/api/serena_client_reconcile.go` | Total pre/post attempt callbacks and one shared restore core with a front-reconcile ownership/CAS mode; the existing synchronous migrate compensation wrapper remains behavior-compatible |
| `internal/clients/cas_mutator.go` | One rollback-bypass CAS restore method, the existing nine concrete implementations, and the existing `lockingClient` forwarder; the allowlist must not grow |

Allowed test files:

| File | Planned coverage |
| --- | --- |
| `internal/cli/install_reconcile_mcp_front_v3_test.go` | Version-3 schema, C1/C2/C8 integration, prepared settlement, immutable baseline, compatibility refusal, durable retirement |
| `internal/cli/install_reconcile_mcp_front_pr588_r2_test.go` | Replace the now-stale version-1 acceptance expectation; retain C3/C5/C6 guards |
| `internal/cli/install_reconcile_mcp_front_review_test.go` | Update only the persisted-record fixtures in `TestMCPFrontReview_RollbackKeepsTheRecordWhileAnyRowIsPending` and `TestMCPFrontReview_RollbackFailsWhenTheRecordCannotBeRetired` to valid version-3 generation/row/plan state; preserve both tests' assertions and behavioral targets |
| `internal/api/lsp_client_router_plan_test.go` | Frozen population/pre-state, canonical-before-legacy forward ordering, legacy-before-canonical rollback ordering, exact observations |
| `internal/api/lsp_client_router_snapshot_review_test.go` | Replace the unsafe absent-row expectation with applied/uncertain pending cases |
| `internal/api/serena_client_reconcile_test.go` | Total post-attempt callback and CAS conflict/result mapping |
| `internal/clients/cas_mutator_test.go` | Rollback-bypass polarity, nine-adapter admission, and one-lock forwarder behavior |

Existing test files may be updated only where their current assertion conflicts
with version 3. In particular,
`TestMCPFrontR2_LegacyV1RecordWithVerifiedInputsCanStillRollBack` and its two
version-1 siblings currently encode version-2 compatibility behavior
(`internal/cli/install_reconcile_mcp_front_pr588_r2_test.go:432-512`);
version 3 must replace that family with the table-driven read-only refusal
guard required by `design.md:383-387`.

### Explicit no-go surfaces

- Do not edit `internal/cli/install.go`; the read-only `--check` gate is already
  correct at `internal/cli/install.go:107-121`.
- Do not add or move the front-reconcile operation lock at
  `internal/cli/install_reconcile_mcp_front.go:352-369`.
- Do not weaken or reorder the readiness gate at
  `internal/cli/install_reconcile_mcp_front.go:395-412`.
- Do not edit route expiry, GUI, tray, supervisor, scheduler, launcher,
  registry, demotion, or setup owners. The excluded sibling callers are
  documented at `research-live-findings-2026-07-27.md:97-125`.
- Do not modify `internal/cli/migrate_serena.go`. Preserve its synchronous
  compensation semantics through an API wrapper over the same private Serena
  restore core.
- Do not promote rollback CAS onto `clients.Client`, `jsonMCPClient`, Windsurf,
  or the 38 LSP-eligible adapters outside the deliberate capability allowlist.
  The current nine-adapter gate is
  `internal/clients/cas_mutator.go:112-162`; the exhaustive 47-client boundary
  is `research-adapter-cas-seam-2026-07-27.md:125-197`.
- Do not add a dependency, public CLI flag, public settings key, service,
  archive format, GUI surface, or second recovery artifact. The only persisted
  contract change is the private report version (`design.md:105-117`).
- Never launch GUI, tray, supervisor, or scheduler, kill a process by image
  name, run unscoped `go test ./...`, checkout, hard-reset, stash, or push
  (`roadmap.md:14-24`).

## Class-sweep matrix

This is the implementation and final-report sweep. “Already correct” entries
must be retained and tested, not reimplemented.

| Class | General rule | Violating or load-bearing participants | Already-correct or excluded participants | Mandatory guard |
| --- | --- | --- | --- | --- |
| C1 | Every canonical and legacy LSP entry is one distinct immutable row keyed by `(surface, client, language, entry_name)` | CLI `mergeMCPFrontReconcileReport` and `lspSnapshotKey` collapse rows at `internal/cli/install_reconcile_mcp_front.go:1170-1249` | API capture already emits the canonical row and every legacy candidate at `internal/api/lsp_client_router_snapshot.go:137-178` | `TestMCPFrontV3_LSPArtifactRoundTripPreservesCanonicalAndMultipleLegacyRows` |
| C2 | Ownership evidence is the latest successful post-state for each row, never one report-level port | Global `Port`/`Applied` model at `internal/cli/install_reconcile_mcp_front.go:237-277`; forward publishes one port before fallible per-client writes at `internal/cli/install_reconcile_mcp_front.go:477-496`; rollback supplies it to every LSP row at `internal/cli/install_reconcile_mcp_front.go:573-584` | Per-client API report records Applied only after Add succeeds at `internal/api/lsp_client_router.go:1033-1055` | `TestMCPFrontV3_PartialCrossPortRetryKeepsPerRowAppliedPorts` |
| C3 | `--check` never reaches a mutating dispatch | None open | Top gate at `internal/cli/install.go:107-121`, protected by `TestMCPFrontR2_CheckWithReconcileMutatesNothing` | Existing protected guard |
| C4 | Serena rollback mutates only while the live entry still equals the recorded applied state, under the same config lock, and can restore a legacy hub backup | CLI currently permits unjudgeable `Recorded=false` rows and API restores unconditionally at `internal/cli/install_reconcile_mcp_front.go:998-1013`, `internal/cli/install_reconcile_mcp_front.go:1100-1137`, and `internal/api/serena_client_reconcile.go:561-584` | `CASEntryMutator` already owns lock-scoped compare/mutate at `internal/clients/cas_mutator.go:33-110`, but generic restore uses the wrong `allowHubEntry=false` polarity at `internal/clients/cas_mutator.go:250-258` | `TestMCPFrontV3_SerenaCASRestoresLegacyHubBackupAndRefusesConcurrentEdit` |
| C5 | One front-reconcile operation owns report and client state from read through retirement | None open | One wrapper lock encloses forward and rollback at `internal/cli/install_reconcile_mcp_front.go:352-369` | Existing `TestMCPFrontR2_SecondInvocationRefusesWhileTheTransactionLockIsHeld` |
| C6 | Serena and at least one configured LSP lifecycle are live before recovery state or client writes | None open | Total preflight is before snapshot and mutation at `internal/cli/install_reconcile_mcp_front.go:395-445` | Existing `TestMCPFrontR2_ForwardRefusesWhenOnlyTheSerenaRouteIsLive` |
| C7 | The standalone route process cleans its own Serena and LSP session maps | None open | Route store handoff/cleanup is at `internal/cli/route.go:183-301` | Three existing `TestRouteDaemon_SessionExpiry*` guards |
| C8 | A backup callback durably prepares intent; only a total post-attempt observation may promote Applied ownership | CLI writes a row under `Serena.Applied` before the adapter call at `internal/cli/install_reconcile_mcp_front.go:880-925`; API has only a pre-write hook before `AddEntry` at `internal/api/serena_client_reconcile.go:463-487` | API `MigrateReport.Applied` itself is appended only after success at `internal/api/serena_client_reconcile.go:525-527` | `TestMCPFrontV3_SerenaAddFailureDoesNotPromoteApplied` |
| C9 | An unreachable absent-baseline row is Pending whenever an applied or uncertain attempt may own a created entry | `restorable()` excludes absent rows at `internal/api/lsp_client_router_snapshot.go:72-84`; unreachable classification consults only that predicate at `internal/api/lsp_client_router_snapshot.go:236-253` | Caller already keeps the report for Pending/Failed at `internal/cli/install_reconcile_mcp_front.go:595-612` | Table-driven `TestSnapshotRestore_AppliedOrUncertainAbsentBaselineUnreachableIsPending` |
| C10 | A forward generation mutates only rows in one fail-closed, durable population captured before any write | Snapshot and mutation independently construct clients and recheck `Exists()` at `internal/api/lsp_client_router_snapshot.go:127-136`, `internal/api/lsp_client_router.go:194-202`; CLI passes no shared plan at `internal/cli/install_reconcile_mcp_front.go:439-450`, `internal/cli/install_reconcile_mcp_front.go:486-489` | Snapshot's canonical/legacy enumeration already uses the one legacy-candidate owner at `internal/api/lsp_client_router_snapshot.go:102-178` | `TestMCPFrontV3_ClientAppearingBetweenCaptureAndApplyIsNotMutated` |
| Compatibility | A version-1 or version-2 row set lacks version-3 ownership evidence and authorizes zero automatic writes | Current version-2 validator conditionally admits version 1 at `internal/cli/install_reconcile_mcp_front.go:664-741` | Unknown versions already fail closed at `internal/cli/install_reconcile_mcp_front.go:671-673` | Table-driven `TestMCPFrontV3_V1AndV2ArtifactsRefuseBeforeAnyWrite` |

## Shared command safety wrapper

Every Go command whose package graph can include `internal/api` or
`internal/cli` must carry `-tags=test_state_path_env` and a unique state
directory under this worktree's `.scratch/`. Define this function in the active
PowerShell session before running any command in this plan:

```powershell
function Invoke-PR588Go {
    param(
        [Parameter(Mandatory = $true)]
        [string[]] $GoArgs,
        [switch] $ExpectFailure
    )

    $scratchRoot = (Resolve-Path -LiteralPath '.scratch').Path
    $stateDir = Join-Path $scratchRoot ('pr588-state-' + [guid]::NewGuid().ToString('N'))
    [void](New-Item -ItemType Directory -Path $stateDir)
    try {
        $env:MCPHUB_STATE_DIR_OVERRIDE = $stateDir
        & go @GoArgs
        $code = $LASTEXITCODE
    }
    finally {
        Remove-Item Env:MCPHUB_STATE_DIR_OVERRIDE -ErrorAction SilentlyContinue
        $resolvedState = (Resolve-Path -LiteralPath $stateDir).Path
        $requiredPrefix = $scratchRoot + [IO.Path]::DirectorySeparatorChar
        if (-not $resolvedState.StartsWith($requiredPrefix, [StringComparison]::OrdinalIgnoreCase)) {
            throw "refusing cleanup outside .scratch: $resolvedState"
        }
        Remove-Item -LiteralPath $resolvedState -Recurse -Force
    }

    if ($ExpectFailure) {
        if ($code -eq 0) {
            throw "expected the controlled defect mutation to fail, but go exited 0"
        }
    }
    elseif ($code -ne 0) {
        throw "go exited $code"
    }
}
```

The fresh directory is created before each invocation and removed only after
its resolved path is proven to be below `.scratch`. A failure expected during
mutation proof must be an assertion failure in the named test, not a compile
error, timeout, panic unrelated to the invariant, or missing-test/no-op result.

## Phase A — version-3 journal and compatibility gate

Owner: `$backend-engineer`  
Depends on: accepted design and reliability R2 only  
Revert: atomic revert group `RG-PR588-V3` with Phases B-D

### Work

1. In `internal/cli/install_reconcile_mcp_front.go`, replace the version-2
   global recovery model with the accepted version-3 row map, one active plan,
   monotonic generation, exact attempt, effective applied receipt, and rollback
   disposition model (`design.md:119-169`). Keep
   `mcpFrontReconcileReport` as the artifact owner and add the private symbols
   `mcpFrontReconcileRow`, `mcpFrontReconcilePlan`,
   `mcpFrontReconcileAttempt`, `mcpFrontAppliedReceipt`, and
   `mcpFrontRollbackDisposition`.
2. Make row identity exactly `(surface, client, language, entry_name)`. Serena
   is `surface=serena`, empty language, entry `serena`; LSP rows retain the full
   `LSPRouterEntrySnapshot`.
3. Implement one transition owner and one effective-receipt resolver:
   `prepared`, `confirmed-no-write`, `applied`, and `conflict` follow the
   accepted precedence. An unresolved `prepared` or `conflict` attempt keeps
   its plan immutable and blocks a new generation. Name the single owners
   `settleMCPFrontReconcileAttempts` and
   `effectiveMCPFrontAppliedReceipt`.
4. Validate generation, plan reference, operation, exact pre-state, and intended
   post-state before any mutation. Structural absence is a hard refusal.
5. Make forward read and rollback validation reject version 1 and version 2
   before any adapter call, leave the artifact byte-identical, and report
   `legacy-ownership-unproven` (`design.md:219-228`).
   `mcpFrontReconcileRowKey` replaces the current incomplete
   `lspSnapshotKey`; `canRetireMCPFrontReconcileReport` owns durable
   disposition-based retirement.
6. Replace the stale v1-acceptance test family with the version-1/version-2
   table guard. Do not retain tests whose only assertion is the superseded v2
   acceptance rule.

### Preserved and changed behavior

- Preserve the first captured Serena backup/pin and every exact LSP baseline
  forever within the active artifact.
- Preserve write-ahead durability: no client mutation occurs until its exact
  planned pre/intended state is durable.
- Change only the private artifact from version 2 to version 3.
- Change v1/v2 handling from conditional v1 rollback admission to read-only
  refusal; both forward and rollback perform zero client writes.
- Preserve current artifact path, operation lock, pin hashing, and atomic
  state-file publication.

### Diff-invisible invariants and named regression guards

- Distinct exact LSP rows survive capture, persistence, retry, and restore:
  `TestMCPFrontV3_LSPArtifactRoundTripPreservesCanonicalAndMultipleLegacyRows`.
- Partial A-to-B retries retain B ownership only for B-success rows and A for
  proven B-no-write rows:
  `TestMCPFrontV3_PartialCrossPortRetryKeepsPerRowAppliedPorts`.
- First baselines are immutable: both prior guards compare first-generation
  bytes after retry.
- Every mutation has durable prepare:
  `TestMCPFrontV3_EveryMutationRequiresDurablePrepared`.
- Prepared rows settle before plan replacement or rollback:
  `TestMCPFrontV3_ReentrySettlesWriteReceiptCrashWindows`.
- A referenced uncertain plan cannot be replaced:
  `TestMCPFrontV3_UncertainAttemptBlocksPlanReplacement`.
- Confirmed no-write preserves the older effective applied receipt:
  `TestMCPFrontV3_ConfirmedNoWriteKeepsEarlierPortOwnership`.
- Unknown post-write evidence authorizes neither retry nor rollback:
  `TestMCPFrontV3_PostWriteEvidenceFailureStaysPending`.
- Retirement is computed from re-read durable dispositions:
  `TestMCPFrontV3_PersistenceFailureOrPendingGroupPreventsRetirement`.
- V1 and v2 are both byte-identical, zero-write refusals:
  `TestMCPFrontV3_V1AndV2ArtifactsRefuseBeforeAnyWrite`.

### Acceptance

- **A-AC1:** Serializing and re-reading one canonical row plus two same-language
  legacy rows yields exactly three distinct row identities and byte-equivalent
  immutable baselines.
- **A-AC2:** After two rows are applied at A, then one applies at B while the
  second proves no-write, effective receipt ports are exactly `B, A` in row
  order and rollback uses those two values.
- **A-AC3:** A durable `prepared` attempt whose live state equals intended
  becomes Applied on re-entry; one equal to pre-state becomes
  confirmed-no-write; a third state authorizes zero mutation and blocks plan
  replacement.
- **A-AC4:** A later confirmed-no-write attempt leaves the previous effective
  applied receipt unchanged.
- **A-AC5:** Version-1 and version-2 complete-looking artifacts each yield a
  nonzero command result, zero adapter calls, and byte-identical artifact
  contents.
- **A-AC6:** A pending group or injected disposition-write failure leaves the
  active artifact present; retirement occurs only after a durable re-read
  shows every row terminal.

### Narrow verification

```powershell
Invoke-PR588Go -GoArgs @(
    'test', '-tags=test_state_path_env', '-count=1', '-timeout', '10m',
    '-run',
    '^(TestMCPFrontV3_(LSPArtifactRoundTripPreservesCanonicalAndMultipleLegacyRows|PartialCrossPortRetryKeepsPerRowAppliedPorts|EveryMutationRequiresDurablePrepared|ReentrySettlesWriteReceiptCrashWindows|UncertainAttemptBlocksPlanReplacement|ConfirmedNoWriteKeepsEarlierPortOwnership|PostWriteEvidenceFailureStaysPending|PersistenceFailureOrPendingGroupPreventsRetirement|V1AndV2ArtifactsRefuseBeforeAnyWrite))$',
    './internal/cli/'
)
```

Expected evidence: every named test prints `PASS`; no package outside
`./internal/cli/` is selected; the command exits 0.

## Phase B — lock-scoped Serena ownership and total promotion

Owner: `$backend-engineer`  
Depends on: Phase A row/attempt/receipt types fixed  
Revert: `RG-PR588-V3`

### Work

1. Extend `clients.CASEntryMutator` with one rollback-specific restore method
   that re-reads and checks the expected live Serena entry under the existing
   config lock and composes the existing restore core with
   `allowHubEntry=true`. Add it to the same nine concrete implementations and
   `lockingClient` forwarder; keep `AsCASEntryMutator` as the capability gate.
   Name the new interface method `CASRestoreEntryFromBytesForRollback`.
2. Add one private Serena restore core in
   `internal/api/serena_client_reconcile.go`. Keep
   `RestoreSerenaReconcileApplied` as the existing synchronous-compensation
   wrapper for the migrate caller; expose a front-reconcile ownership mode or
   sibling wrapper named `RestoreSerenaReconcileAppliedOwned` that supplies
   the expected applied fingerprint and maps
   `ErrCASConflict` to a conflict result. Both wrappers must use the one core.
3. Extend `SerenaReconcileOpts` with a total post-attempt hook carrying client,
   backup, intended post-state, adapter return, and observed post-state. It must
   run after `AddEntry` on both success and error, before another client is
   considered. Name the field `OnAttemptFinished`; use one
   `SerenaReconcileAttemptResult` value rather than parallel callback
   parameters.
4. Make callback or observation failure stop further writes. The pre-write
   callback prepares only; it never creates effective Applied ownership.
5. Readback equal to intended state promotes Applied, readback equal to pre-state
   confirms no-write, and unreadable/third state remains uncertain/conflict.

### Preserved and changed behavior

- Preserve the seven-client Serena surface at
  `internal/api/serena_client_reconcile.go:416-448`.
- Preserve backup-before-write, route liveness, managed-entry marker
  best-effort behavior, and legacy-removal-after-success ordering at
  `internal/api/serena_client_reconcile.go:451-527`.
- Preserve the old in-process migrate compensation caller through its existing
  wrapper; change only front-reconcile rollback to the ownership/CAS mode.
- Change a Serena `AddEntry` error from preemptively Applied to
  confirmed-no-write or uncertain, based on post-attempt observation.
- Change operator-diverged state to terminal `skipped-conflict`, zero write,
  and non-success rollback diagnostics.

### Diff-invisible invariants and named regression guards

- Serena rollback can restore the normal legacy hub backup while refusing a
  concurrent/operator edit under the same repository-owned lock:
  `TestMCPFrontV3_SerenaCASRestoresLegacyHubBackupAndRefusesConcurrentEdit`.
- Serena ownership is promoted only after the post-attempt result is observed
  and durable:
  `TestMCPFrontV3_SerenaAddFailureDoesNotPromoteApplied`.
- The post-attempt writer/event pair is total over success and error:
  `TestSerenaReconcile_PostAttemptHookRunsForSuccessAndFailure`.
- The CAS allowlist remains exactly the current nine adapters and Windsurf
  remains excluded: existing `TestCASAllowlist*`, `TestCASPerAdapterRestore`,
  and `TestCASLockingClientForwarderNoDeadlock`.

### Acceptance

- **B-AC1:** Each of the existing nine admitted concrete adapters and
  `lockingClient` satisfies the extended capability; Windsurf and every
  previously excluded adapter still fail admission.
- **B-AC2:** With live Serena equal to the receipt, rollback restores a
  pinned legacy hub entry under one lock; with a deterministic edit between
  outer validation and the lock-scoped compare, rollback returns conflict and
  performs zero write.
- **B-AC3:** An injected `AddEntry` failure whose readback equals pre-state
  produces no Applied receipt; a later operator value remains byte-unchanged
  by rollback.
- **B-AC4:** A write that lands despite an adapter error is classified from
  readback rather than from the error alone; failure to persist that observed
  state stops the run with durable prepared ownership.
- **B-AC5:** The existing non-front migrate compensation test behavior remains
  green through the compatibility wrapper.

### Narrow verification

```powershell
Invoke-PR588Go -GoArgs @(
    'test', '-tags=test_state_path_env', '-count=1', '-timeout', '10m',
    '-run',
    '^(TestCAS(PerAdapterRestore|LockingClientForwarderNoDeadlock|AllowlistExcludesWindsurf|AllowlistAdmitsAdoptReachable|RollbackBypassRestoreGateBranches))$',
    './internal/clients/'
)

Invoke-PR588Go -GoArgs @(
    'test', '-tags=test_state_path_env', '-count=1', '-timeout', '10m',
    '-run',
    '^(TestSerena(Reconcile_PostAttemptHookRunsForSuccessAndFailure|ClientReconcile_.*)|TestMCPFrontV3_SerenaCASRestoresLegacyHubBackupAndRefusesConcurrentEdit)$',
    './internal/api/'
)
```

Expected evidence: both commands exit 0; CAS output proves the fixed allowlist
and lock forwarder; API output proves total callbacks and conflict/no-write.

## Phase C — frozen LSP plan, exact ownership, and route-safe rollback

Owner: `$backend-engineer`  
Depends on: Phase A row identity and attempt contract  
Revert: `RG-PR588-V3`

### Work

1. In `internal/api/lsp_client_router.go`, add one operation-scoped plan builder
   that loads languages, registry, enablement, disabled set, and client map
   once, then records exact canonical add/rewrite and legacy remove rows.
   The CLI-facing symbols are `LSPRouterClientPlan`,
   `PlanLSPRouterClientEntries`, and `ApplyLSPRouterClientPlan`; the existing
   `EnsureLSPRouterClientEntries` remains a compatibility wrapper over that
   one owner for non-front callers.
2. Persist and apply only planned row identities. The applicator must not call
   `clients.AllClients()`, re-enumerate `Exists()`, or admit a row absent from
   the plan. A missing planned adapter is a reportable unavailable result.
3. Immediately before the durable prepared callback, read the exact planned
   entry and require equality with the captured pre-state. A mismatch produces
   conflict-before-write and zero adapter mutation.
4. Return one exact outcome per planned row: client, language, entry name,
   operation, captured pre-state, intended post-state, observed post-state,
   applied port, and error/result class.
5. Enforce forward dependency groups keyed by client/language: canonical
   success and durable observation precede every legacy removal.
6. In snapshot rollback, classify unreachable absent baselines from the
   version-3 row's effective applied/uncertain evidence, not `restorable()`
   alone.
7. Roll back each group legacy-first. Restore and verify every required legacy
   row before canonical inverse; a legacy pending/failure leaves the canonical
   route in place and the group pending.
8. Verify every inverse by exact baseline readback before reporting restored.

### Preserved and changed behavior

- Preserve the existing eligibility, registry, and legacy-candidate predicates
  at `internal/api/lsp_client_router.go:125-258`; move their output into one
  frozen plan rather than re-deriving them.
- Preserve exact operator ownership checks already present at
  `internal/api/lsp_client_router_snapshot.go:264-309`.
- Preserve one reachable route on every ordered failure: forward keeps legacy
  until canonical is durable; rollback keeps canonical until all legacy rows
  are verified.
- Do not claim kernel-enforced CAS for LSP. The accepted residual remains the
  advisory-lock limitation at `design.md:392-400`.

### Diff-invisible invariants and named regression guards

- A client absent at capture and present at apply receives exactly zero
  Add/Remove calls until a later generation:
  `TestMCPFrontV3_ClientAppearingBetweenCaptureAndApplyIsNotMutated`.
- Exact planned pre-state gates every adapter write:
  `TestMCPFrontV3_PlanPopulationAndPrestateAreFrozen`.
- Canonical failure preserves every legacy route:
  `TestMCPFrontV3_CanonicalFailurePreservesAllLegacyRoutes`.
- A failure restoring any required legacy row preserves the canonical route and
  keeps the group pending:
  `TestMCPFrontV3_LegacyRestoreFailureKeepsCanonicalRoute`.
- Applied-created and uncertain-created absent rows are both pending while
  unreachable:
  table-driven
  `TestSnapshotRestore_AppliedOrUncertainAbsentBaselineUnreachableIsPending`.

### Acceptance

- **C-AC1:** One plan-build call performs exactly one client-population
  acquisition; plan application performs zero client-registry enumeration.
- **C-AC2:** A client whose `Exists()` changes false-to-true after capture
  receives zero Add/Remove calls in that generation.
- **C-AC3:** A planned row changed after capture receives zero mutation calls
  and a durable conflict-before-write disposition; its changed bytes remain
  intact.
- **C-AC4:** A canonical add failure yields zero legacy removals for that
  client/language group.
- **C-AC5:** Failure of the second of two legacy restores leaves the canonical
  front route intact and the group nonterminal; a retry can continue without
  losing either restored legacy state.
- **C-AC6:** Applied and uncertain absent-baseline rows each create one Pending
  result when the client is unreachable; a baseline-only absent row creates
  zero Pending rows.
- **C-AC7:** Every row reported restored has a successful exact baseline
  readback; a readback mismatch is Pending/Failed and never terminal success.

### Narrow verification

```powershell
Invoke-PR588Go -GoArgs @(
    'test', '-tags=test_state_path_env', '-count=1', '-timeout', '10m',
    '-run',
    '^(TestMCPFrontV3_(ClientAppearingBetweenCaptureAndApplyIsNotMutated|PlanPopulationAndPrestateAreFrozen|CanonicalFailurePreservesAllLegacyRoutes|LegacyRestoreFailureKeepsCanonicalRoute)|TestSnapshotRestore_AppliedOrUncertainAbsentBaselineUnreachableIsPending|TestSnapshotLSPRouterClientEntries_CapturesLegacyPerWorkspaceEntries|TestMCPFrontLegacyLSP_ForwardThenRollbackRestoresTheLegacyEntry)$',
    './internal/api/'
)
```

Expected evidence: the command exits 0; output names all seven probes; no
unscoped package pattern is used.

## Phase D — CLI integration and durable end-to-end recovery

Owner and integration owner: `$backend-engineer`  
Depends on: Phases A-C all green  
Revert: `RG-PR588-V3`

### Work

1. Keep the existing operation lock and preflight in place. Immediately after
   reading/validating the report, invoke the shared attempt-settlement owner in
   both forward and rollback before a new plan or inverse.
2. Forward: build the frozen LSP plan before client writes, merge only new
   immutable baselines, persist the complete active plan, and route every
   Serena/LSP mutation through prepare and post-attempt journal callbacks.
3. Stop further writes on prepare persistence failure, unreadable/third
   post-state, or receipt persistence failure. Leave durable prepared evidence
   active.
4. Retry: preserve the previous effective applied receipt on proven no-write;
   update only the successful row's latest receipt/port.
5. Rollback: validate pins, checksums, version-3 references, and settle attempts
   before mutation; use Serena CAS receipts and LSP per-row applied post-state
   as ownership evidence.
6. Persist each row/group disposition before a dependent inverse. Compute
   retirement from a fresh report read and retire only terminal
   `baseline-only`, `restored`, or `skipped-conflict` rows.
7. A terminal operator conflict may be retired but must return stable
   non-success naming only client/language/entry/generation/reason. Never emit
   backup bytes, raw entries, headers, tokens, or environment values
   (`design.md:265-287`).
8. Update exactly two stale persisted-record fixtures in
   `internal/cli/install_reconcile_mcp_front_review_test.go`:
   `TestMCPFrontReview_RollbackKeepsTheRecordWhileAnyRowIsPending` at
   `internal/cli/install_reconcile_mcp_front_review_test.go:514-550` must seed
   one valid version-3 LSP row with an effective applied receipt and matching
   active-plan generation so the unreachable-client path, not schema
   validation, owns its result; and
   `TestMCPFrontReview_RollbackFailsWhenTheRecordCannotBeRetired` at
   `internal/cli/install_reconcile_mcp_front_review_test.go:563-594` must seed
   a valid empty version-3 row map and matching empty active plan so the
   injected retirement failure remains the first failing operation. Do not
   alter their assertions, expected diagnostics, seams, or production code for
   these fixture corrections. The validator correctly requires generation,
   row map, and active plan at
   `internal/cli/install_reconcile_mcp_front.go:983-1001`.

### Preserved and changed behavior

- Preserve existing operation serialization, preflight, report path, timeout,
  pin verification, atomic retirement namespace, and no automatic
  compensation across unrelated groups.
- Preserve output success shape; new failure diagnostics are stable reason
  codes and row identities only.
- Change skipped operator divergence from apparent rollback success to explicit
  partial non-success.
- Change report retirement from in-memory report counts to re-read durable
  terminal dispositions.

### Diff-invisible invariants and named regression guards

Every Phase A-C invariant is endangered at this integration seam and every
named guard from those phases is binding. In addition:

- C3, C5, C6, and C7 retain their current owners and behavior:
  `TestMCPFrontR2_CheckWithReconcileMutatesNothing`,
  `TestMCPFrontR2_SecondInvocationRefusesWhileTheTransactionLockIsHeld`,
  `TestMCPFrontR2_ForwardRefusesWhenOnlyTheSerenaRouteIsLive`, and the three
  `TestRouteDaemon_SessionExpiry*` tests.
- A disposition persistence failure or any pending group keeps the active
  journal:
  `TestMCPFrontV3_PersistenceFailureOrPendingGroupPreventsRetirement`.

### Acceptance

- **D-AC1:** Forward and rollback each invoke the same settlement owner before
  plan replacement or inverse; there is no second settlement implementation.
- **D-AC2:** A crash-window fixture with durable prepare and landed write is
  settled from the persisted pre/intended tuple on re-entry without replacing
  its plan.
- **D-AC3:** A receipt-persistence failure returns nonzero, stops later client
  mutations, and leaves the active artifact with the prepared row.
- **D-AC4:** A partial A-to-B retry restores each row using its own latest
  effective receipt and immutable first baseline.
- **D-AC5:** A legacy restoration failure prevents canonical removal; a later
  successful retry completes and only then allows retirement.
- **D-AC6:** A terminal operator conflict produces zero write for that row,
  stable non-success output naming the row, and no sensitive backup/raw value.
- **D-AC7:** The protected C3/C5/C6/C7 tests remain byte/behaviorally
  unchanged; no protected production file outside this phase's allowlist is
  in the diff.
- **D-AC8:** Both named review tests reach their original behavioral boundary:
  the pending fixture reports the recorded `claude-code/go` row unreachable
  and keeps the report, while the retirement fixture reaches the injected
  rename error and reports the record still active. Their assertion bodies are
  byte-unchanged; only valid version-3 fixture fields are added.

### Integration verification

```powershell
Invoke-PR588Go -GoArgs @(
    'test', '-tags=test_state_path_env', '-count=1', '-timeout', '10m',
    '-run',
    '^(TestMCPFrontV3_.*|TestMCPFrontR2_(CheckWithReconcileMutatesNothing|SecondInvocationRefusesWhileTheTransactionLockIsHeld|ForwardRefusesWhenOnlyTheSerenaRouteIsLive)|TestMCPFrontReview_(ClientIsNotMutatedWhenItsRecoveryRowCannotBeDurable|RollbackKeepsTheRecordWhileAnyRowIsPending|RollbackFailsWhenTheRecordCannotBeRetired|RetirementClearsTheActiveNamespace)|TestRouteDaemon_Session(StoresAreReachableForExpiry|ExpiryActuallyReclaimsBoundSessions|ExpiryStopsWithContext))$',
    './internal/cli/'
)

Invoke-PR588Go -GoArgs @(
    'test', '-tags=test_state_path_env', '-count=1', '-timeout', '10m',
    '-run',
    '^(TestMCPFrontV3_.*|TestSnapshotRestore_.*|TestSnapshotLSPRouterClientEntries_CapturesLegacyPerWorkspaceEntries|TestMCPFrontLegacyLSP_ForwardThenRollbackRestoresTheLegacyEntry|TestSerenaClientReconcile_.*|TestSerenaReconcile_PostAttemptHookRunsForSuccessAndFailure)$',
    './internal/api/'
)

Invoke-PR588Go -GoArgs @(
    'test', '-tags=test_state_path_env', '-count=1', '-timeout', '10m',
    '-run',
    '^(TestCAS.*|TestAllClientsAreLockWrapped)$',
    './internal/clients/'
)
```

Expected evidence: all three commands exit 0 and print `PASS`; the CLI/API
commands each use a fresh state directory and the required build tag.

## Phase E — backend self-review and implementation handoff

Owner: `$backend-engineer`  
Depends on: Phase D integration green  
Revert: no independent source change; `RG-PR588-V3` remains the only revert unit

### Work and acceptance

1. Run `gofmt` only on the allowed changed Go files.
2. Run `git diff --check`.
3. Produce a change-surface table matching every changed file to the allowed
   owner above. Any source file outside the contract is `REVISE` to architect,
   not an automatic expansion.
4. Trace all return paths for forward and rollback: validation refusal,
   settlement refusal, plan build, prepare failure, adapter success/error,
   observation failure, promotion failure, pending/conflict, inverse failure,
   disposition failure, retirement failure, and terminal conflict.
5. Hand off exact modified-file hashes and the green Phase D command output to
   `$qa-engineer`.

- **E-AC1:** `git diff --check` exits 0.
- **E-AC2:** Every production diff path is one of the five allowed files.
- **E-AC3:** Every named forward/rollback return path has one explicit durable
  state and command result; no error is swallowed or converted to success.
- **E-AC4:** No new dependency, public flag, config key, GUI/service surface,
  CAS adapter admission, or sensitive diagnostic is present.

Diff-invisible invariants: all 18 architecture claims remain endangered until
QA and architecture review. Named guards: the complete Phase D regex set.

## Phase F — independent QA, controlled defect mutations, and broad gates

Owner: `$qa-engineer`  
Depends on: Phase E handoff  
Writes: test corrections only if a test defect is found; any production change
returns to `$backend-engineer` for a bounded correction cycle  
Revert: no independent delivery commit; source remains `RG-PR588-V3`

### Controlled mutation protocol

For each row below:

1. Record SHA-256 of every production file the mutation will touch using
   `Get-FileHash -Algorithm SHA256 -LiteralPath <path>`.
2. Apply exactly the named production defect with `apply_patch`. Never mutate
   the test, fixture expectation, helper assertion, build tag, timeout, or test
   runner.
3. Run only the named guard through `Invoke-PR588Go -ExpectFailure`. Accept the
   proof only when that named test reaches its class assertion and fails for
   the expected invariant.
4. Immediately restore the production source with the inverse `apply_patch`.
   If the test command is interrupted or errors, restoration is the only next
   action.
5. Recompute SHA-256 and require exact equality with the pre-mutation hash.
6. Re-run the same named guard without `-ExpectFailure` and require exit 0.
7. Record the mutation, failing assertion, nonzero exit, restored hash, and
   green rerun in the QA artifact.

No `git checkout --`, `git reset`, `git stash`, worktree creation, or test-source
mutation is permitted as restoration.

| Class | Production mutation | Guard that must fail |
| --- | --- | --- |
| C1 | In the version-3 row-key owner, omit `entry_name` so canonical and legacy rows collide | `TestMCPFrontV3_LSPArtifactRoundTripPreservesCanonicalAndMultipleLegacyRows` in `./internal/cli/` |
| C2 | In the effective LSP ownership resolver, use the active plan/report port for every row instead of the row's applied receipt | `TestMCPFrontV3_PartialCrossPortRetryKeepsPerRowAppliedPorts` in `./internal/cli/` |
| C4 | In Serena front rollback, replace the rollback-bypass CAS call with the ordinary restore path or bypass the expected-live matcher | `TestMCPFrontV3_SerenaCASRestoresLegacyHubBackupAndRefusesConcurrentEdit` in `./internal/api/` |
| C8 | Promote effective Serena Applied ownership in the pre-write backup callback before `AddEntry` result observation | `TestMCPFrontV3_SerenaAddFailureDoesNotPromoteApplied` in `./internal/cli/` |
| C9 | Revert unreachable-row pending selection to `row.restorable()`/present-baseline only | `TestSnapshotRestore_AppliedOrUncertainAbsentBaselineUnreachableIsPending` in `./internal/api/` |
| C10 | Make the LSP applicator re-enumerate live clients or admit an unplanned newly-present client | `TestMCPFrontV3_ClientAppearingBetweenCaptureAndApplyIsNotMutated` in `./internal/api/` |

Each mutation run uses this exact shape, substituting only the guard regex and
package from the table:

```powershell
Invoke-PR588Go -ExpectFailure -GoArgs @(
    'test', '-tags=test_state_path_env', '-count=1', '-timeout', '10m',
    '-run', '^(EXACT_GUARD_NAME)$',
    './internal/cli/'
)
```

Use `./internal/api/` for the API guards. A build failure is not a mutation
proof; the mutated source must compile and the named assertion must fail.

### Reliability guards after restoration

After all six file hashes are restored, run the full Phase D integration
commands again. This is the green evidence for:

- durable prepare before every write;
- settlement before plan replacement/inverse;
- effective receipt preservation after no-write;
- canonical-before-legacy forward dependency;
- legacy-before-canonical rollback dependency;
- exact frozen plan and pre-state;
- applied/uncertain absent pending classification;
- durable re-read retirement;
- v1/v2 zero-write refusal;
- C3/C5/C6/C7 protected behavior.

### Broad build and vet

The broad package pattern is required, but the absolute tag/state rule still
applies because `./...` includes `internal/api` and `internal/cli`:

```powershell
Invoke-PR588Go -GoArgs @(
    'build', '-tags=test_state_path_env', './...'
)

Invoke-PR588Go -GoArgs @(
    'vet', '-tags=test_state_path_env', './...'
)
```

Expected evidence: both commands exit 0 with no diagnostic output. Do not run
`go test ./...`.

### QA acceptance

- **F-AC1:** All six controlled production mutations fail their exact named
  guard at the intended assertion and none fails only by compilation/no-op.
- **F-AC2:** Every mutated file's post-restoration SHA-256 exactly equals its
  pre-mutation SHA-256.
- **F-AC3:** All restored Phase D CLI/API/clients command sets exit 0.
- **F-AC4:** Tagged `go build ./...` and tagged `go vet ./...`, each with a
  fresh state directory, exit 0.
- **F-AC5:** QA maps evidence explicitly to A-AC1 through E-AC4 and reports no
  missing named guard.

## Phase G — independent architecture gates and local commit

Owner: `$lead`  
Depends on: QA PASS  
Source writes: none unless a reviewer returns REVISE

### Review gates

1. `$architecture-reviewer` claim-verification lane checks all 18 final
   architecture claims at `design.md:318-390` against the diff and fresh QA
   evidence. Every claim requires its one owner and named falsifier.
2. A separate adversarial architecture lane checks crash windows, cross-port
   partial retries, Serena operator edits, LSP dependency ordering, stale
   compatibility artifacts, retirement, and protected-surface leakage.
3. A `REVISE` finding returns to the owning implementation phase and repeats
   the affected mutation, scoped tests, QA, and both review gates. An errored
   review is UNVERIFIED, not PASS.
4. `$lead` reconciles all 14 finding rows, the class-sweep table, six mutation
   proofs, scoped outputs, build/vet, review verdicts, and changed files before
   commit.

### Commit gate

Before committing:

```powershell
git diff --check
git status --short
git diff --stat
git diff -- internal/cli/install_reconcile_mcp_front.go internal/api/lsp_client_router.go internal/api/lsp_client_router_snapshot.go internal/api/serena_client_reconcile.go internal/clients/cas_mutator.go
git diff -- internal/cli/install_reconcile_mcp_front_v3_test.go internal/cli/install_reconcile_mcp_front_pr588_r2_test.go internal/cli/install_reconcile_mcp_front_review_test.go internal/api/lsp_client_router_plan_test.go internal/api/lsp_client_router_snapshot_review_test.go internal/api/serena_client_reconcile_test.go internal/clients/cas_mutator_test.go
```

The lead must verify that no unrelated/user-owned change is staged. Then stage
only the accepted source, tests, and canonical work-item/report artifacts and
create one local commit. Recommended message:

```text
fix(mcp-front): close PR #588 recovery findings

PR #588 findings:
- rows 1/13 (C1): preserve canonical and every legacy LSP row by exact identity
- rows 2/10 (C2): retain latest applied ownership and port per row
- rows 3/9 (C3): preserve the read-only --check dispatch gate from 3872ee16
- row 4 (C4): restore Serena only through lock-scoped expected-live CAS
- rows 5/11 (C5): preserve the whole-operation reconcile lock from 3872ee16
- row 6 (C6): preserve the LSP lifecycle preflight from 3872ee16
- row 7 (C7): preserve route-owned session cleanup from 3872ee16
- row 8 (C8): promote Serena ownership only after observed successful write
- row 12 (C9): keep applied or uncertain absent LSP rows pending when unreachable
- row 14 (C10): apply only the frozen snapshotted client/entry population

Recovery schema v3 refuses v1/v2 automatic writes, settles durable prepared
attempts before retry/rollback, orders LSP dependencies route-safely, and
retires only from re-read durable terminal state.
```

Do not push.

### Final acceptance

- **G-AC1:** Claim-verification architecture review is PASS for all 18 claims.
- **G-AC2:** Adversarial architecture review is PASS with no unresolved
  data-loss, crash-window, or compatibility finding.
- **G-AC3:** The final report contains the 14-row classification table, the
  class-sweep table, six mutation failures plus green restorations, exact
  scoped test/build/vet outputs, changed-file list, commit hash, and explicit
  `not pushed`.
- **G-AC4:** One local commit names every finding row/class and its closure;
  `git status --short --branch` confirms the branch is ahead locally and no
  task-owned unstaged source/test change remains.

## Rollback and recovery

All production/test implementation phases form atomic revert group
`RG-PR588-V3`. Version-3 journal code, API plan/applicator callbacks, CAS
capability, and their tests must ship or revert together. Reverting only one
phase would leave a persisted schema consumer without its writer/adapter
contract.

Before push, if a load-bearing hypothesis is disproved, remove the local
hypothesis-bearing commit through the lead's approved recovery workflow; do
not improvise around the user's explicit ban on checkout, hard reset, and
stash. Because the operator forbids those commands in this task, any
commit-level rollback requires direct operator direction. Before commit,
recover only through the inverse `apply_patch` plus exact file-hash equality
used by the mutation protocol.

An on-disk version-3 artifact must never be automatically downgraded. If a
release rollback is later required after deployment, the safe fallback is:
stop automatic forward/rollback use of the artifact, preserve it byte-for-byte,
and require a build that understands version 3 or an explicit operator-guided
manual restore. Version 1/2 and structurally incomplete artifacts remain
read-only refusals.

## Recommended role sequence

`$backend-engineer` Phases A-D and integration ownership → `$backend-engineer`
Phase E self-review → `$qa-engineer` Phase F mutation and verification gate →
independent `$architecture-reviewer` claim-verification lane → independent
adversarial `$architecture-reviewer` lane → `$lead` reconciliation and local
commit without push.

## Gate

**PASS — RETURN(lead); planner-eligible.** The plan stays inside the accepted
Change-Surface Contract, has one integration owner, distributes every
diff-invisible invariant and named guard, defines six production mutation
proofs with safe restoration, and provides exact state-sandboxed verification
commands plus atomic recovery rules.

## Terms and Abbreviations

- **Applied receipt**: durable exact evidence of one row's latest successful
  forward mutation.
- **CAS**: compare-and-set; compare expected live state and mutate under the
  same repository-owned client-config lock.
- **CLI**: command-line interface.
- **LSP**: Language Server Protocol.
- **Pending**: nonterminal recovery work that keeps the active journal.
- **RG**: atomic revert group.
- **Serena**: the Serena Model Context Protocol client entry reconciled to the
  front route.
