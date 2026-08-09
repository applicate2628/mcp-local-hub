# Implementation plan: managed LSP-router proof for destructive cleanup

## Accepted basis and delivery boundary

- Accepted scope: findings 4 and 5 are one open defect class: configured port
  equality currently authorizes backup/removal without proving a live managed
  router (`research.md:10-18`, `research.md:71-85`).
- Accepted architecture: one bounded cleanup-wide `/api/ping` identity probe,
  one immutable snapshot, and the existing per-client/per-language alias owner
  jointly authorize pre-existing-router cleanup (`design.md:3-23`,
  `design.md:69-132`).
- Impact classification: **behavioral, bounded**. A stopped or foreign listener
  no longer authorizes destructive cleanup. Same-registration bound
  replacements retain their existing cleanup eligibility.
- Integration owner: **main-session `$lead`**. The lead records the pre-change
  dirty-worktree baseline, hands the accepted design to each phase owner,
  reconciles all evidence, runs the pre-commit checkpoint, and authors the one
  focused local commit. The lead does not push.
- Execution order is strictly **Phase A -> Phase B -> Phase C**. There are no
  parallel implementation phases because Phase B mutates Phase A's sole
  authorization gates and Phase C reviews the restored Phase A result.
- Stable acceptance-criterion identifiers are append-only within their phase.
  A removed criterion is retired and its identifier is never reused.

### Exact change surface

| Path | Permitted use |
|---|---|
| `internal/api/register.go` | Add the typed expected managed-GUI identity input; implement the one cleanup-wide probe/snapshot/warning; pass the immutable liveness result into the existing alias owner. |
| `internal/gui/projects_toggle.go` | Supply port, process identifier, and version from the same running `Server` instance that owns the request. |
| `internal/api/register_test.go` | Add/strengthen the pure probe, cleanup authorization, direct-entry-kind, warning-cardinality, binding-snapshot, and mutation guards. |
| `internal/gui/projects_toggle_test.go` | Verify the project-toggle caller supplies the exact server-owned identity tuple without launching GUI, tray, supervisor, or another process. |

`internal/gui/ping_test.go` is verification-only: retain and run
`TestPing_OrdinaryWireShapeRemainsByteCompatible`; do not edit it. No other
source or test file is in the approved write set. In particular, do not change
`internal/gui/ping.go`, `internal/cli/register.go`,
`internal/api/register_supervisor.go`, `internal/api/legacy_migrate.go`, router
entry writers, persisted client schemas, scheduler/supervisor/tray code, or the
already accepted `c826a48d` closures.

### Copied authorization invariant

For each client and registered language, cleanup eligibility is exactly:

`same-registration bound replacement OR (managedRouterSnapshot.liveManaged AND existing per-language router-entry gate passes)`.

The first term does not consult the network proof. The second term requires the
one process-wide proof and then preserves the incumbent configured-entry checks:
entry present and enabled, loopback route grammar, client ownership, language,
and port equality. No whole-client shortcut is permitted. Both direct
`mcp-language-server` and direct `gopls` Model Context Protocol (MCP)
candidates consume aliases from this same owner.

The Graphical User Interface (GUI) caller's expected tuple is evidence from the
same running `Server`: port, positive process identifier (PID), and non-empty
version. Command-Line Interface (CLI) registration and legacy migration leave
the tuple absent; absence is the safe path and cannot authorize cleanup based
only on a pre-existing router entry. `GUIPort` remains the existing route-port
provenance input, and expected-identity port mismatch fails closed.

### Defect-class participant table

| Participant | Required cases | Owning phase and observable disposition |
|---|---|---|
| Replacement origin | Same-registration binding; pre-existing router entry | A: bound origin bypasses the network proof; pre-existing origin requires the immutable managed snapshot plus the existing entry gate. B mutates both branches independently. |
| Listener state | Stopped; timeout; managed; foreign | A: stopped/timeout/foreign preserve direct entries with one classified warning; only exact managed identity authorizes. B proves stopped/foreign guards kill the port-equality mutation. |
| Router-entry validity | Missing; disabled; malformed; non-loopback; wrong language; stale port; matching owned | A: positive managed snapshot still delegates every case to the existing per-language gate; only matching owned is eligible. |
| Direct-entry kind | `mcp-language-server`; direct `gopls` MCP | A: one table drives both kinds through the same alias owner and removal path with identical positive/negative outcomes. |
| Client grain | Bound default; opt-in with router evidence; absent client | A: bound is same-registration proof, opt-in needs both proof layers, absent/nil remains skipped. |
| Language grain | One language; multiple languages | A: proof is evaluated at most once per cleanup while configured-entry authorization remains per language. |
| Caller provenance | GUI; CLI; legacy migration | A: GUI supplies expected tuple; unchanged CLI/migration callers supply none and therefore cannot authorize router-only cleanup. |
| Warning surface | Progress writer; `RegisterReport.Warnings`; GUI response; CLI stderr | A: existing report propagation remains owner; one cleanup-wide warning value is written once to progress and appended once to the report. |
| Return/skip path | Port resolution; identity validation/probe; client absence; empty aliases; scan; backup; removal | A/C: no proof failure becomes cleanup permission; existing backup/remove failure handling remains unchanged. |

## Phase A — `$backend-engineer`: implement the accepted design

### Dependency and handoff

The main-session `$lead` first runs `git status --short` and records the
pre-existing dirty paths. The backend owner receives only `brief.md`,
`research.md`, `design.md`, and this plan, then echoes before editing:

1. the GUI server-owned expected identity and why CLI/legacy absence is safe;
2. the exact ping wire and 500 ms/no-retry/body-close contract;
3. the single cleanup snapshot and single warning owner;
4. the bound bypass and per-language unbound formula;
5. stopped/foreign/managed guards and both direct-entry kinds.

Any need to change ping bytes, add a persisted nonce/schema, probe inside the
client/language loop, require GUI proof for bound clients, or touch a path
outside the exact change surface is `REVISE` to `$architecture-reviewer` before
editing.

### Implementation steps and exact seams

1. In `internal/api/register.go`, extend the internal Go-only `RegisterOpts`
   contract with one typed expected managed-GUI identity containing port, PID,
   and version. Zero/absent/invalid values are fail-closed evidence, not a
   fallback.
2. In `internal/gui/projects_toggle.go`, populate that tuple from the same
   running `Server` instance that already supplies the port. Do not discover
   identity from ambient state and do not change route behavior.
3. In `cleanupDirectLanguageServerEntriesAfterRegister`, after the router port
   is resolved once and only when router-origin authorization is needed, create
   one immutable cleanup snapshot with resolved port, `liveManaged`, and one
   bounded failure class.
4. The production probe contract is exact:
   - without sending a request, reject invalid/unresolved port, missing or
     invalid expected identity, and expected/resolved port mismatch;
   - issue exactly one `GET http://127.0.0.1:<port>/api/ping`;
   - use one 500 ms request context and an HTTP client with the same 500 ms
     total ceiling; never retry;
   - refuse redirects away from the exact loopback target;
   - accept only HTTP 200 and `application/json`;
   - read at most 4 KiB and accept exactly one JSON object with no
     non-whitespace trailing payload;
   - require `ok == true`, `pid > 0`, non-empty `version`, and exact PID/version
     equality with the caller evidence;
   - close every response body on every response path and retain no handle in
     the snapshot.
5. Classify proof failure as exactly one of
   `identity-not-supplied`, `port-unresolved`, `port-mismatch`,
   `stopped-or-timeout`, `http-status`, `content-type`,
   `malformed-response`, or `identity-mismatch`. Append once to
   `RegisterReport.Warnings` and write once to the progress writer:

   `LSP router managed-identity proof unavailable (<failure class>); keeping direct LSP entries whose only replacement is a pre-existing shared-router entry`

   Do not expose raw response bodies or machine paths. Do not emit this warning
   solely because bound replacements do not require the proof.
6. Pass only the snapshot's immutable `liveManaged` value into
   `lspCleanupAliasesForClient`. Keep that function as the single
   per-client/per-language alias owner:
   - bound: add aliases without consulting `liveManaged`;
   - unbound and not live-managed: add no aliases and do not inspect the
     configured router entry for authorization;
   - unbound and live-managed: call the existing configured-entry gate.
7. Keep both direct-entry matchers downstream of that one alias result. Do not
   duplicate liveness or identity logic in either matcher.
8. Add focused tests only in the two approved test files. Test helper listeners
   are in-process and loopback-only; no GUI, tray, supervisor, scheduler, or
   child-process operation is permitted.

### Acceptance criteria

- **A-AC1:** GUI project-toggle registration supplies one tuple whose port, PID,
  and version exactly equal the owning `Server` values; CLI and legacy callers
  remain unchanged and therefore supply no tuple.
- **A-AC2:** Invalid/unresolved port, missing/invalid identity, or port mismatch
  makes zero HTTP requests, authorizes zero router-origin backups/removals, and
  emits exactly one corresponding cleanup-wide warning when router-origin proof
  is needed.
- **A-AC3:** A needed proof performs exactly one loopback GET, has one 500 ms
  total deadline, performs zero retries, and a blocking handler observes
  cancellation within the ceiling plus scheduler tolerance.
- **A-AC4:** Only HTTP 200, `application/json`, a body of at most 4 KiB, one
  trailing-payload-free JSON object, `ok:true`, positive PID, non-empty version,
  and exact expected PID/version equality set `liveManaged=true`.
- **A-AC5:** Every proof failure is fail-closed; stopped, timeout, foreign,
  malformed, and identity-mismatch cases perform zero backup and zero removal
  attributable to router-only replacement evidence.
- **A-AC6:** One proof failure across multiple clients and languages yields
  exactly one request and exactly one router-proof value in
  `RegisterReport.Warnings`, with the same value written once to progress.
- **A-AC7:** With identity absent, a same-registration bound client's replaced
  direct entry is still backed up/removed, while an unbound router-only sibling
  remains and no proof is required merely for the bound branch.
- **A-AC8:** With a valid managed snapshot, an owned valid `go` router entry may
  authorize only `go`; sibling `python` without its own valid entry remains.
- **A-AC9:** Missing, disabled, malformed, non-loopback, wrong-language, and
  stale-port configured entries remain ineligible even when the managed
  identity proof succeeds.
- **A-AC10:** Equivalent `mcp-language-server` and direct `gopls` candidates
  receive identical cleanup authorization outcomes from the same alias owner.
- **A-AC11:** `TestRegister_ClientScopeResolvedOnceForTheWholeRegistration`
  remains green and the cleanup path performs no second binding resolution.
- **A-AC12:** `/api/ping` bytes, HTTP/CLI/configuration/persisted-state
  contracts, existing workspace/backup/removal safeguards, and all protected
  `c826a48d` closures are unchanged.
- **A-AC13:** Every Phase A command below produces fresh output with exit code
  zero and no operation launches GUI, tray, supervisor, scheduler, or a child
  process.

### Diff-invisible invariants and named regression guards

| Copied invariant | Named guard and required observation |
|---|---|
| A stopped listener never authorizes cleanup. | `TestRegister_CleanupKeepsDirectEntryWhenConfiguredRouterIsStopped`: matching entry and expected identity, no listener, one bounded attempt, direct entry remains, backup/remove counts zero, one `stopped-or-timeout` warning. |
| A foreign HTTP listener never authorizes cleanup. | `TestRegister_CleanupKeepsDirectEntryWhenRouterPortHasForeignListener`: mismatched/non-managed ping, request count one, direct entry remains, backup/remove counts zero, one classified warning. |
| A managed listener authorizes only current per-language candidates. | `TestRegister_CleanupRemovesDirectEntryWithProvenManagedRouter`: exact tuple, valid `go` replacement removes direct `go`, sibling `python` remains, request count one. |
| Same-registration bound clients remain cleanup-eligible without GUI proof. | `TestRegister_CleanupBoundClientBypassesManagedRouterProof`: identity absent, bound direct entry removed, unbound router-only entry preserved. |
| Stale-port, disabled, malformed, wrong-language, and non-loopback entries remain ineligible. | `TestRegister_CleanupRejectsInvalidRouterEntries`: positive managed snapshot plus both result directions; every invalid shape observes no deletion. |
| One registration uses one binding snapshot. | Retain `TestRegister_ClientScopeResolvedOnceForTheWholeRegistration`; no second production resolution. |
| Both direct-entry kinds share one alias owner. | `TestRegister_CleanupAliasAuthorizationForDirectEntryKinds`: table-driven positive/negative `mcp-language-server` and `gopls` rows observe identical eligibility. |
| Probe failure is visible once, not multiplied by loop cardinality. | Multi-client/multi-language stopped or foreign rows assert one HTTP request and one router-proof warning. |
| Probe resources and latency are bounded. | `TestManagedRouterProbe` blocking-handler subtest asserts the 500 ms ceiling plus scheduler tolerance, request cancellation, no retry; review confirms body closure on every response path. |
| Ordinary ping wire is unchanged. | Retain and run `TestPing_OrdinaryWireShapeRemainsByteCompatible` unchanged. |

### Exact Phase A commands and expected observations

Run formatting only on the approved write set:

```powershell
gofmt -w internal/api/register.go internal/api/register_test.go internal/gui/projects_toggle.go internal/gui/projects_toggle_test.go
```

Expected: exit code 0; only the four approved files may change.

Run the scoped Application Programming Interface (API) guards with a fresh
state directory and preserve raw output:

```powershell
$run = Join-Path (Get-Location) ('.scratch\pr583-a-api-' + (Get-Date -Format 'yyyyMMdd-HHmmssfff') + '-' + [guid]::NewGuid().ToString('N')); New-Item -ItemType Directory -Path $run | Out-Null; $env:MCPHUB_STATE_DIR_OVERRIDE = Join-Path $run 'state'; New-Item -ItemType Directory -Path $env:MCPHUB_STATE_DIR_OVERRIDE | Out-Null; & go test -tags=test_state_path_env -count=1 -timeout 10m -run '^(TestManagedRouterProbe|TestRegister_CleanupKeepsDirectEntryWhenConfiguredRouterIsStopped|TestRegister_CleanupKeepsDirectEntryWhenRouterPortHasForeignListener|TestRegister_CleanupRemovesDirectEntryWithProvenManagedRouter|TestRegister_CleanupBoundClientBypassesManagedRouterProof|TestRegister_CleanupAliasAuthorizationForDirectEntryKinds|TestRegister_CleanupRejectsInvalidRouterEntries|TestRegister_ClientScopeResolvedOnceForTheWholeRegistration)$' ./internal/api 2>&1 | Tee-Object -FilePath (Join-Path $run 'go-test.txt'); $code = $LASTEXITCODE; Set-Content -LiteralPath (Join-Path $run 'exit-code.txt') -Value $code; if ($code -ne 0) { throw "scoped API tests failed; evidence: $run" }
```

Expected: exit code 0; all named tests report PASS; the run directory contains
`go-test.txt`, `exit-code.txt`, and an otherwise isolated state subtree.

Run only the non-launching GUI caller/wire guards:

```powershell
$run = Join-Path (Get-Location) ('.scratch\pr583-a-gui-' + (Get-Date -Format 'yyyyMMdd-HHmmssfff') + '-' + [guid]::NewGuid().ToString('N')); New-Item -ItemType Directory -Path $run | Out-Null; & go test -count=1 -timeout 10m -run '^(TestProjectsToggle_RegisterSuppliesManagedGUIIdentity|TestPing_OrdinaryWireShapeRemainsByteCompatible)$' ./internal/gui 2>&1 | Tee-Object -FilePath (Join-Path $run 'go-test.txt'); $code = $LASTEXITCODE; Set-Content -LiteralPath (Join-Path $run 'exit-code.txt') -Value $code; if ($code -ne 0) { throw "scoped GUI tests failed; evidence: $run" }
```

Expected: exit code 0; the tuple-propagation test and unchanged byte-wire test
pass; no GUI, tray, supervisor, scheduler, or child process is launched.

Run the local diff hygiene check:

```powershell
git diff --check
```

Expected: exit code 0 and no output.

### Phase A rollback

The four Phase A files are one **atomic revert group** because the API option,
its GUI producer, and their tests form one contract. Before any commit, reverse
only the task hunks with `apply_patch`; never use checkout, reset, or stash and
never touch unrelated dirty work. After a local commit, the main lead may
remove that one unpushed commit only after rechecking the worktree boundary.
There is no persisted-state or migration output to roll back. Do not revert the
pre-existing `c826a48d` closures.

## Phase B — `$qa-engineer`: independent mutation proof

### Dependency, scope, and method

Phase B starts only after every Phase A criterion passes. The QA owner may
temporarily modify **only** `internal/api/register.go`, and only with the
`apply_patch` tool. Tests are immutable during mutation proof. Every mutation
must compile far enough to execute the named guard; a compile failure, unrelated
failure, timeout, empty output, or failure in a different assertion is not
accepted evidence. Every run writes raw stdout/stderr and its exit code under a
fresh unique `.scratch` directory.

### Mutation 1 — invalidate the single liveness gate

1. Use `apply_patch` on `internal/api/register.go` only to make configured router
   port equality authorize the unbound/router-origin branch without the managed
   ping verdict. Do not change route validation, backup/removal code, or tests.
2. Run exactly:

```powershell
$run = Join-Path (Get-Location) ('.scratch\pr583-b-port-equality-kill-' + (Get-Date -Format 'yyyyMMdd-HHmmssfff') + '-' + [guid]::NewGuid().ToString('N')); New-Item -ItemType Directory -Path $run | Out-Null; $env:MCPHUB_STATE_DIR_OVERRIDE = Join-Path $run 'state'; New-Item -ItemType Directory -Path $env:MCPHUB_STATE_DIR_OVERRIDE | Out-Null; & go test -tags=test_state_path_env -count=1 -timeout 10m -run '^(TestRegister_CleanupKeepsDirectEntryWhenConfiguredRouterIsStopped|TestRegister_CleanupKeepsDirectEntryWhenRouterPortHasForeignListener)$' ./internal/api 2>&1 | Tee-Object -FilePath (Join-Path $run 'go-test.txt'); $code = $LASTEXITCODE; Set-Content -LiteralPath (Join-Path $run 'exit-code.txt') -Value $code; if ($code -eq 0) { throw "mutation survived stopped/foreign guards; evidence: $run" }; Write-Output "EXPECTED MUTATION FAILURE; evidence: $run"
```

Expected: non-zero test exit; at least one named stopped/foreign guard fails
because backup/removal occurred (non-zero backup/remove count or the direct
entry disappeared). A failure caused only by compilation, setup, or warning
text is `REVISE`, not proof.

3. Restore the exact liveness verdict flow with a reverse `apply_patch` only.
   Do not use checkout, reset, or stash.
4. Re-run exactly:

```powershell
$run = Join-Path (Get-Location) ('.scratch\pr583-b-port-equality-restore-' + (Get-Date -Format 'yyyyMMdd-HHmmssfff') + '-' + [guid]::NewGuid().ToString('N')); New-Item -ItemType Directory -Path $run | Out-Null; $env:MCPHUB_STATE_DIR_OVERRIDE = Join-Path $run 'state'; New-Item -ItemType Directory -Path $env:MCPHUB_STATE_DIR_OVERRIDE | Out-Null; & go test -tags=test_state_path_env -count=1 -timeout 10m -run '^(TestRegister_CleanupKeepsDirectEntryWhenConfiguredRouterIsStopped|TestRegister_CleanupKeepsDirectEntryWhenRouterPortHasForeignListener)$' ./internal/api 2>&1 | Tee-Object -FilePath (Join-Path $run 'go-test.txt'); $code = $LASTEXITCODE; Set-Content -LiteralPath (Join-Path $run 'exit-code.txt') -Value $code; if ($code -ne 0) { throw "liveness-gate restore failed; evidence: $run" }
```

Expected: exit code 0; both guards pass with zero backup/removal.

### Mutation 2 — invalidate the bound bypass

1. Use `apply_patch` on `internal/api/register.go` only to require
   `liveManaged=true` in the same-registration bound branch. Do not alter the
   test, identity input, or unbound gate.
2. Run exactly:

```powershell
$run = Join-Path (Get-Location) ('.scratch\pr583-b-bound-bypass-kill-' + (Get-Date -Format 'yyyyMMdd-HHmmssfff') + '-' + [guid]::NewGuid().ToString('N')); New-Item -ItemType Directory -Path $run | Out-Null; $env:MCPHUB_STATE_DIR_OVERRIDE = Join-Path $run 'state'; New-Item -ItemType Directory -Path $env:MCPHUB_STATE_DIR_OVERRIDE | Out-Null; & go test -tags=test_state_path_env -count=1 -timeout 10m -run '^TestRegister_CleanupBoundClientBypassesManagedRouterProof$' ./internal/api 2>&1 | Tee-Object -FilePath (Join-Path $run 'go-test.txt'); $code = $LASTEXITCODE; Set-Content -LiteralPath (Join-Path $run 'exit-code.txt') -Value $code; if ($code -eq 0) { throw "mutation survived bound-bypass guard; evidence: $run" }; Write-Output "EXPECTED MUTATION FAILURE; evidence: $run"
```

Expected: non-zero test exit for the named guard because the identity-absent
bound direct entry remains or the expected backup/removal count is zero. A
compile/setup failure is not proof.

3. Restore the unconditional bound branch with a reverse `apply_patch` only.
4. Re-run exactly:

```powershell
$run = Join-Path (Get-Location) ('.scratch\pr583-b-bound-bypass-restore-' + (Get-Date -Format 'yyyyMMdd-HHmmssfff') + '-' + [guid]::NewGuid().ToString('N')); New-Item -ItemType Directory -Path $run | Out-Null; $env:MCPHUB_STATE_DIR_OVERRIDE = Join-Path $run 'state'; New-Item -ItemType Directory -Path $env:MCPHUB_STATE_DIR_OVERRIDE | Out-Null; & go test -tags=test_state_path_env -count=1 -timeout 10m -run '^TestRegister_CleanupBoundClientBypassesManagedRouterProof$' ./internal/api 2>&1 | Tee-Object -FilePath (Join-Path $run 'go-test.txt'); $code = $LASTEXITCODE; Set-Content -LiteralPath (Join-Path $run 'exit-code.txt') -Value $code; if ($code -ne 0) { throw "bound-bypass restore failed; evidence: $run" }
```

Expected: exit code 0; identity-absent bound cleanup is restored.

### Acceptance criteria

- **B-AC1:** The port-equality mutation makes at least one stopped/foreign guard
  fail for the required destructive observation: backup/removal occurs or the
  direct entry disappears.
- **B-AC2:** After reverse `apply_patch`, both stopped/foreign guards pass with
  zero backup/removal and preserved direct entries.
- **B-AC3:** The bound-bypass mutation makes the dedicated bound guard fail
  because the bound direct entry is not backed up/removed.
- **B-AC4:** After reverse `apply_patch`, the bound guard passes with the bound
  direct entry removed and the unbound router-only sibling preserved.
- **B-AC5:** Every kill and restore run has its own raw `go-test.txt` and
  `exit-code.txt`; no accepted kill result is a compile, setup, timeout, or
  unrelated-test failure.
- **B-AC6:** Final inspection shows no QA-only mutation residue in
  `internal/api/register.go`; tests and every non-target source file are
  byte-untouched by QA.

### Diff-invisible invariants and named regression guards

| Copied invariant | Mutation evidence |
|---|---|
| A stopped listener never authorizes cleanup. | Port-equality kill must make `TestRegister_CleanupKeepsDirectEntryWhenConfiguredRouterIsStopped` fail on backup/removal; restore must pass. |
| A foreign HTTP listener never authorizes cleanup. | The same kill must make the foreign-listener guard fail if it reaches the port-equality authorization; restore must pass. |
| Same-registration bound clients remain cleanup-eligible without GUI proof. | Bound-bypass kill must make `TestRegister_CleanupBoundClientBypassesManagedRouterProof` fail; restore must pass. |

The other Phase A invariants are not independently mutated in this bounded QA
phase because the task requires these two kill points. They remain mandatory
positive guards in Phases A and C.

### Phase B rollback

Mutation hunks are disposable and must never be committed. Each kill/restore
pair is an atomic QA exercise. Restore only with reverse `apply_patch`, then
obtain the required green rerun before advancing. If restoration cannot be
proved, stop with `BLOCKED`; do not use checkout, reset, stash, or a broad file
replacement.

## Phase C — `$architecture-reviewer` Claim-Verify, then main-session `$lead`

### Dependency and gate order

Phase C begins only after Phase B has both accepted red/green mutation proofs
and no mutation residue. The `$architecture-reviewer` performs Claim-Verify
read-only review first. `REVISE` returns to Phase A and repeats every affected
Phase B proof. Only reviewer `PASS` allows the main-session `$lead` to run final
verification and create the local commit.

### Eight-claim Claim-Verify map

| Claim | Single owner | Required falsifying probe/evidence | PASS observation |
|---:|---|---|---|
| 1. Pre-existing router cleanup needs one bounded managed proof. | `cleanupDirectLanguageServerEntriesAfterRegister` snapshot | Stopped, foreign, malformed, timeout, and managed controlled cases; request count. | Failures preserve with zero backup/removal; managed exact tuple may authorize; needed probe count is one. |
| 2. Bound clients remain eligible without GUI proof. | Bound branch in `lspCleanupAliasesForClient` | Bound guard plus Phase B bound kill. | Identity-absent bound entry is removed; kill fails; restore passes. |
| 3. Router-origin authorization remains per client/language. | `lspCleanupAliasesForClient` | Managed `go` versus sibling `python`. | Only the language with its own valid entry is eligible. |
| 4. Both direct-entry kinds use one alias authorization. | Aliases returned by `lspCleanupAliasesForClient` | Two-kind positive/negative table. | `mcp-language-server` and `gopls` outcomes are identical for identical evidence. |
| 5. Proof failure/timeout is fail-closed and visible once. | Cleanup warning aggregation | Multi-client/multi-language failure case. | Zero backup/removal; one request when attempted; one report warning and one progress write. |
| 6. One registration retains one binding snapshot. | Sole `effectiveClientBindings` invocation in `registerWithManifest` | Existing named snapshot guard and reviewer call-site search. | Test passes; no second production resolution appears. |
| 7. Public HTTP, CLI, configuration, and persisted-state contracts are unchanged. | Existing ping wire and `RegisterReport` contract | Ping byte guard and internal diff boundary. | Ping test passes; only four approved files changed; protected files have empty diff. |
| 8. Network lifetime is one 500 ms no-retry request and bodies close. | Managed GUI ping probe | Blocking-handler cancellation, request count, and all-response-path code review. | Bounded cancellation; request count <= 1; every response path closes body; no redirect escape. |

### Anti-layering audit

The reviewer returns `REVISE` if any of these observations fails:

- one typed expected-identity input, constructed by the GUI caller from the
  owning server; no ambient lookup and no second identity format;
- one probe call site and one immutable snapshot owner in
  `cleanupDirectLanguageServerEntriesAfterRegister`;
- no HTTP call, retry loop, identity comparison, or warning emission inside a
  client/language loop;
- `clientHasActiveLSPRouterReplacement` remains the configured-entry gate and
  does not become a second process-liveness owner;
- `lspCleanupAliasesForClient` remains the only alias authorization owner, with
  the bound bypass and the exact unbound formula;
- direct `mcp-language-server` and direct `gopls` matching do not duplicate
  identity/liveness policy;
- exactly one cleanup-wide warning owner; existing per-entry and backup/removal
  warnings remain their own existing concerns;
- no ping-wire, CLI help, persisted schema, router writer, supervisor,
  scheduler, tray, launch, workspace-free cleanup, or unregister change.

Run this read-only inventory:

```powershell
rg -n "ManagedGUIIdentity|managedRouterSnapshot|liveManaged|/api/ping|lspCleanupAliasesForClient|clientHasActiveLSPRouterReplacement" internal/api/register.go internal/gui/projects_toggle.go
```

Expected: one GUI identity-construction path, one cleanup-owned probe/snapshot
path, one immutable liveness propagation path, and the existing alias and
configured-entry owners. Any per-client/per-language probe or duplicate
identity/warning policy is `REVISE`.

Check the protected diff:

```powershell
git diff --exit-code -- internal/cli/register.go internal/gui/ping.go internal/gui/ping_test.go internal/api/register_supervisor.go internal/api/legacy_migrate.go
```

Expected: exit code 0 and no output.

Check the exact internal change set:

```powershell
git diff --name-only -- internal
```

Expected lines, and no others:

```text
internal/api/register.go
internal/api/register_test.go
internal/gui/projects_toggle.go
internal/gui/projects_toggle_test.go
```

### Safe final verification

Re-run the full scoped API guard set in a fresh state directory:

```powershell
$run = Join-Path (Get-Location) ('.scratch\pr583-c-api-' + (Get-Date -Format 'yyyyMMdd-HHmmssfff') + '-' + [guid]::NewGuid().ToString('N')); New-Item -ItemType Directory -Path $run | Out-Null; $env:MCPHUB_STATE_DIR_OVERRIDE = Join-Path $run 'state'; New-Item -ItemType Directory -Path $env:MCPHUB_STATE_DIR_OVERRIDE | Out-Null; & go test -tags=test_state_path_env -count=1 -timeout 10m -run '^(TestManagedRouterProbe|TestRegister_CleanupKeepsDirectEntryWhenConfiguredRouterIsStopped|TestRegister_CleanupKeepsDirectEntryWhenRouterPortHasForeignListener|TestRegister_CleanupRemovesDirectEntryWithProvenManagedRouter|TestRegister_CleanupBoundClientBypassesManagedRouterProof|TestRegister_CleanupAliasAuthorizationForDirectEntryKinds|TestRegister_CleanupRejectsInvalidRouterEntries|TestRegister_ClientScopeResolvedOnceForTheWholeRegistration)$' ./internal/api 2>&1 | Tee-Object -FilePath (Join-Path $run 'go-test.txt'); $code = $LASTEXITCODE; Set-Content -LiteralPath (Join-Path $run 'exit-code.txt') -Value $code; if ($code -ne 0) { throw "final scoped API tests failed; evidence: $run" }
```

Expected: exit code 0 and all named guards PASS.

Re-run only the non-launching GUI caller/wire guards:

```powershell
$run = Join-Path (Get-Location) ('.scratch\pr583-c-gui-' + (Get-Date -Format 'yyyyMMdd-HHmmssfff') + '-' + [guid]::NewGuid().ToString('N')); New-Item -ItemType Directory -Path $run | Out-Null; & go test -count=1 -timeout 10m -run '^(TestProjectsToggle_RegisterSuppliesManagedGUIIdentity|TestPing_OrdinaryWireShapeRemainsByteCompatible)$' ./internal/gui 2>&1 | Tee-Object -FilePath (Join-Path $run 'go-test.txt'); $code = $LASTEXITCODE; Set-Content -LiteralPath (Join-Path $run 'exit-code.txt') -Value $code; if ($code -ne 0) { throw "final scoped GUI tests failed; evidence: $run" }
```

Expected: exit code 0; caller propagation and byte compatibility PASS; no GUI,
tray, supervisor, scheduler, or child process is launched.

Run repository-wide compile and static analysis without executing applications:

```powershell
go build ./...
```

Expected: exit code 0.

```powershell
go vet ./...
```

Expected: exit code 0 and no diagnostics.

Run final diff hygiene:

```powershell
git diff --check
```

Expected: exit code 0 and no output.

### Acceptance criteria

- **C-AC1:** The architecture reviewer records an explicit PASS/REVISE verdict
  for each of the eight claims, citing its single owner and falsifying probe.
- **C-AC2:** The anti-layering audit finds one identity input, one probe owner,
  one immutable snapshot, one warning owner, and one alias owner; it finds no
  nested reprobe or duplicated cleanup policy.
- **C-AC3:** The protected-file diff is empty and the internal changed-file list
  is exactly the four approved paths.
- **C-AC4:** Final API and GUI scoped runs produce fresh exit-code-zero evidence
  under unique `.scratch` directories with all named guards PASS.
- **C-AC5:** `go build ./...`, `go vet ./...`, and `git diff --check` each exit
  zero; no GUI, tray, supervisor, scheduler, or process operation occurs.
- **C-AC6:** The main lead reconciles Phase A criteria, Phase B red/green proof,
  Phase C claim review, original brief scope, protected surfaces, and residual
  risks before staging.
- **C-AC7:** The staged set is exactly the four approved source/test files and
  contains no secret, token, raw external transcript, machine-local absolute
  path, or QA mutation residue.
- **C-AC8:** One focused local commit is created only after all prior criteria
  pass; no push, merge, release, checkout, reset, or stash occurs.

### Diff-invisible invariants and named regression guards

All ten Phase A invariants and named guards are copied into Phase C through the
eight-claim map and final scoped commands. None is `none`: every invariant is
endangered by this behavioral cleanup gate or is a must-not-break contract
explicitly named by the accepted design.

### Commit checkpoint and exact local commit commands

Before staging, the main-session `$lead` records:

- diagnostic fact: port/configuration equality previously authorized
  destructive cleanup without listener/managed-identity proof;
- verified hypothesis chain: the GUI caller owns a separately known
  port/PID/version tuple; the unchanged ping producer exposes that tuple; one
  cleanup-wide proof plus the existing per-language gate is sufficient; bound
  same-registration replacement is independent proof;
- proportional scope: four approved files, no public or persisted contract;
- no-kostyl result: the authorization invariant is corrected rather than an
  error being swallowed or a fallback masking malformed evidence;
- recovery: one unpushed atomic commit, no persisted-state migration, and the
  pre-change dirty baseline preserved.

Then run:

```powershell
git add -- internal/api/register.go internal/api/register_test.go internal/gui/projects_toggle.go internal/gui/projects_toggle_test.go
```

Expected: exit code 0.

```powershell
git diff --cached --check
```

Expected: exit code 0 and no output.

```powershell
git diff --cached --name-only
```

Expected: exactly the four approved paths and no work-item, report, `.scratch`,
protected, or unrelated dirty file.

```powershell
git commit -m "fix(register): require managed router proof before cleanup" -m "Root cause: configured router port equality could authorize backup and removal without proving a live managed listener. Cleanup now consumes one bounded managed-identity snapshot, while same-registration bound replacements retain their existing bypass and per-language alias authorization."
```

Expected: exit code 0 and one new local commit. Do not push.

### Phase C rollback and residual risks

The implementation, caller propagation, and tests remain one atomic revert
group. There is no data migration, schema change, or external state to undo.
Before commit, reverse task hunks only with `apply_patch`. After the local
unpushed commit, the main lead may remove only that commit after reconciling the
dirty baseline; never discard unrelated work. No push is authorized.

Residual risks to state in the final handoff:

- scheduler jitter can widen the observed timeout slightly, so the test uses a
  documented tolerance while the production request/client ceilings remain
  exactly 500 ms;
- PID/version equality is only accepted with separately owned GUI caller
  evidence and loopback transport; missing/mismatched evidence remains false;
- warning wording/cardinality is downstream-observable and must stay one
  cleanup-wide value;
- future callers that want router-origin cleanup must own and explicitly supply
  live identity; they must not infer it from port configuration;
- request/body resources and redirect behavior require code-path review in
  addition to green unit tests.

## Terms and Abbreviations

- **API** — Application Programming Interface.
- **CLI** — Command-Line Interface.
- **GUI** — Graphical User Interface.
- **LSP** — Language Server Protocol.
- **MCP** — Model Context Protocol.
- **PID** — Process identifier.
- **Claim-Verify** — review each design guarantee against its named owner and a
  probe capable of falsifying it.
- **Mutation proof** — deliberately break one production gate, observe the
  named test fail for the intended behavior, restore it, and observe PASS.

PASS
