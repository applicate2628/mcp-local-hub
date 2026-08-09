# Phase B verification: router-liveness mutation proofs

## Scope and verdict

Phase B independently mutated the two production authorization gates named in
`plan.md:247-346`, observed the required behavior-specific failures, reversed
each mutation with `apply_patch`, and obtained the required green reruns. Tests,
fixtures, and non-target production files were not edited. All test commands
used a fresh state directory, `MCPHUB_STATE_DIR_OVERRIDE`,
`-tags=test_state_path_env`, `-count=1`, `-timeout 10m`, and a narrow regular
expression. No Graphical User Interface (GUI), tray, supervisor, scheduler, or
child application was launched.

**Gate: PASS**

## Acceptance-criterion map

| Criterion | What an inadequate criterion could let pass | Evidence and result |
|---|---|---|
| B-AC1 | A compile, setup, timeout, or warning-only failure could be mistaken for a killed liveness gate. | Mutation 1 removed only the unbound `!liveManaged` early-continue. Both named tests executed and failed on direct-entry disappearance: stopped at `register_test.go:4363`, foreign at `register_test.go:4440`. Exit 1. PASS. |
| B-AC2 | A green rerun could conceal an inexact restoration. | Reverse `apply_patch` restored `internal/api/register.go` to baseline SHA-256 `B1D16A019E6C61AED31E66EC191889D6EE2ACB1C7BBF52CC0DFD425095B70B0E`; the two exact guards then passed with exit 0. PASS. |
| B-AC3 | The bound mutation could fail in the unbound branch or on test setup instead of losing bound cleanup. | Mutation 2 inserted only a bound-only `!liveManaged` early-continue. The dedicated test executed and failed at `register_test.go:4198` because the bound Codex CLI entry survived. Exit 1. PASS. |
| B-AC4 | A green rerun could pass while the source still contained the bound-only mutation. | Reverse `apply_patch` removed the three inserted lines, the target hash matched baseline, and the dedicated bound guard passed with exit 0. PASS. |
| B-AC5 | Evidence could be lost, reused between runs, or accepted from unrelated failure. | Each of the four kill/restore runs has a unique `.scratch` directory containing `go-test.txt`, `exit-code.txt`, and `wall-time.txt`. Kill logs contain the named behavioral assertions; all restores are exit 0. PASS. |
| B-AC6 | Tests or non-target source could change while the target appeared restored. | Final SHA-256 values for all four Phase A files exactly match the pre-mutation baseline; `git diff --check` exits 0; `git diff --name-only -- internal` remains exactly the four accepted Phase A paths; the Mutation 2 marker has zero source hits. PASS. |

## Baseline and restoration hashes

| File | Pre-mutation SHA-256 | Post-restoration SHA-256 | Match |
|---|---|---|---|
| `internal/api/register.go` | `B1D16A019E6C61AED31E66EC191889D6EE2ACB1C7BBF52CC0DFD425095B70B0E` | `B1D16A019E6C61AED31E66EC191889D6EE2ACB1C7BBF52CC0DFD425095B70B0E` | Yes |
| `internal/api/register_test.go` | `011557A393975FD959D123D59A065BF12F2CC4F38F3C9989DF9CC9B61EEF3C02` | `011557A393975FD959D123D59A065BF12F2CC4F38F3C9989DF9CC9B61EEF3C02` | Yes |
| `internal/gui/projects_toggle.go` | `F3E145A933057DAB4B2E682B565055854982A1D43FA65A17574E14630DDFF590` | `F3E145A933057DAB4B2E682B565055854982A1D43FA65A17574E14630DDFF590` | Yes |
| `internal/gui/projects_toggle_test.go` | `E5D8FDDA4796EA42AA04A816891424B233139682E0ED8376E96AF66FBCDCB8C6` | `E5D8FDDA4796EA42AA04A816891424B233139682E0ED8376E96AF66FBCDCB8C6` | Yes |

Raw restoration proof:
`.scratch/pr583-b-restoration-proof-20260727-063039300-a68eedd622f449ffb534649e36000c06/`.

## Mutation 1: invalidate the managed-liveness gate

The only kill edit was the removal of this early-continue from the unbound
branch of `lspCleanupAliasesForClient`:

```go
if !liveManaged {
	continue
}
```

Configured-entry ownership, language, route shape, and port equality remained
unchanged. The mutation therefore reproduced the rejected rule that configured
port equality alone can authorize router-origin cleanup.

Kill result: 0 passed, 2 failed, 0 skipped, 0 expected-failure cases; wall time
5.143 seconds; Go package time 0.038 seconds; exit 1.

```text
register_test.go:4363: stopped configured router authorized removal of direct entry "legacy-go-stopped-router"
register_test.go:4440: foreign listener authorized removal of cursor direct entry "legacy-go-cursor"
```

Raw output:
`.scratch/pr583-b-port-equality-kill-20260727-062743651-20ad07e229274d5fba24c366176bdcc0/{go-test.txt,exit-code.txt,wall-time.txt}`.

Restore result: 2 named top-level tests passed, 0 failed, 0 skipped, 0
expected-failure cases; wall time 1.929 seconds; Go package time 0.061 seconds;
exit 0.

Raw output:
`.scratch/pr583-b-port-equality-restore-20260727-062811290-5334275f9b9d411d9f5b5021f089fa87/{go-test.txt,exit-code.txt,wall-time.txt}`.

## Mutation 2: invalidate the same-registration bound bypass

The only kill edit inserted this bound-only condition immediately before the
unchanged unbound branch:

```go
if bound && !liveManaged {
	continue
}
```

Kill result: 0 passed, 1 failed, 0 skipped, 0 expected-failure cases; wall time
6.715 seconds; Go package time 0.136 seconds; exit 1.

```text
register_test.go:4198: codex-cli's superseded direct LSP entry survived; the bound client's entry IS replaced and must still be cleaned up
```

Raw output:
`.scratch/pr583-b-bound-bypass-kill-20260727-062848370-85e8467e0fbb4ba0a2e564e6bbe5ce55/{go-test.txt,exit-code.txt,wall-time.txt}`.

Restore result: 1 named top-level test passed, 0 failed, 0 skipped, 0
expected-failure cases; wall time 0.969 seconds; Go package time 0.047 seconds;
exit 0.

Raw output:
`.scratch/pr583-b-bound-bypass-restore-20260727-062920938-3de39991561f440b81686c46a0852f9b/{go-test.txt,exit-code.txt,wall-time.txt}`.

## Current-source positive guards

After both reverse patches and hash matches:

- Focused Application Programming Interface (API) suite: 8 named top-level
  tests passed, 0 failed, 0 skipped, 0 expected-failure cases; wall time 1.708
  seconds; Go package time 0.653 seconds; exit 0.
  Raw output:
  `.scratch/pr583-b-current-api-20260727-062957333-29ca3fa196094faa9e6289e85f37e249/{go-test.txt,exit-code.txt,wall-time.txt}`.
- Non-launching GUI identity and ping-wire suite: 2 named top-level tests passed,
  0 failed, 0 skipped, 0 expected-failure cases; wall time 4.833 seconds; Go
  package time 0.038 seconds; exit 0.
  Raw output:
  `.scratch/pr583-b-current-gui-20260727-062957304-8a5f4ba6f7364a5b8f5ae2c652f57b8f/{go-test.txt,exit-code.txt,wall-time.txt}`.

## Executed commands

Mutation 1 kill:

```powershell
$run = Join-Path (Get-Location) ('.scratch\pr583-b-port-equality-kill-' + (Get-Date -Format 'yyyyMMdd-HHmmssfff') + '-' + [guid]::NewGuid().ToString('N')); New-Item -ItemType Directory -Path $run | Out-Null; $env:MCPHUB_STATE_DIR_OVERRIDE = Join-Path $run 'state'; New-Item -ItemType Directory -Path $env:MCPHUB_STATE_DIR_OVERRIDE | Out-Null; $sw=[System.Diagnostics.Stopwatch]::StartNew(); & go test -tags=test_state_path_env -count=1 -timeout 10m -run '^(TestRegister_CleanupKeepsDirectEntryWhenConfiguredRouterIsStopped|TestRegister_CleanupKeepsDirectEntryWhenRouterPortHasForeignListener)$' ./internal/api 2>&1 | Tee-Object -FilePath (Join-Path $run 'go-test.txt'); $code = $LASTEXITCODE; $sw.Stop(); Set-Content -LiteralPath (Join-Path $run 'exit-code.txt') -Value $code; Set-Content -LiteralPath (Join-Path $run 'wall-time.txt') -Value $sw.Elapsed.TotalSeconds.ToString('F3',[Globalization.CultureInfo]::InvariantCulture); if ($code -eq 0) { throw "mutation survived stopped/foreign guards; evidence: $run" }; Write-Output "EXPECTED MUTATION FAILURE; evidence: $run"
```

Mutation 1 restore:

```powershell
$run = Join-Path (Get-Location) ('.scratch\pr583-b-port-equality-restore-' + (Get-Date -Format 'yyyyMMdd-HHmmssfff') + '-' + [guid]::NewGuid().ToString('N')); New-Item -ItemType Directory -Path $run | Out-Null; $env:MCPHUB_STATE_DIR_OVERRIDE = Join-Path $run 'state'; New-Item -ItemType Directory -Path $env:MCPHUB_STATE_DIR_OVERRIDE | Out-Null; $sw=[System.Diagnostics.Stopwatch]::StartNew(); & go test -tags=test_state_path_env -count=1 -timeout 10m -run '^(TestRegister_CleanupKeepsDirectEntryWhenConfiguredRouterIsStopped|TestRegister_CleanupKeepsDirectEntryWhenRouterPortHasForeignListener)$' ./internal/api 2>&1 | Tee-Object -FilePath (Join-Path $run 'go-test.txt'); $code = $LASTEXITCODE; $sw.Stop(); Set-Content -LiteralPath (Join-Path $run 'exit-code.txt') -Value $code; Set-Content -LiteralPath (Join-Path $run 'wall-time.txt') -Value $sw.Elapsed.TotalSeconds.ToString('F3',[Globalization.CultureInfo]::InvariantCulture); if ($code -ne 0) { throw "liveness-gate restore failed; evidence: $run" }
```

Mutation 2 kill:

```powershell
$run = Join-Path (Get-Location) ('.scratch\pr583-b-bound-bypass-kill-' + (Get-Date -Format 'yyyyMMdd-HHmmssfff') + '-' + [guid]::NewGuid().ToString('N')); New-Item -ItemType Directory -Path $run | Out-Null; $env:MCPHUB_STATE_DIR_OVERRIDE = Join-Path $run 'state'; New-Item -ItemType Directory -Path $env:MCPHUB_STATE_DIR_OVERRIDE | Out-Null; $sw=[System.Diagnostics.Stopwatch]::StartNew(); & go test -tags=test_state_path_env -count=1 -timeout 10m -run '^TestRegister_CleanupBoundClientBypassesManagedRouterProof$' ./internal/api 2>&1 | Tee-Object -FilePath (Join-Path $run 'go-test.txt'); $code = $LASTEXITCODE; $sw.Stop(); Set-Content -LiteralPath (Join-Path $run 'exit-code.txt') -Value $code; Set-Content -LiteralPath (Join-Path $run 'wall-time.txt') -Value $sw.Elapsed.TotalSeconds.ToString('F3',[Globalization.CultureInfo]::InvariantCulture); if ($code -eq 0) { throw "mutation survived bound-bypass guard; evidence: $run" }; Write-Output "EXPECTED MUTATION FAILURE; evidence: $run"
```

Mutation 2 restore:

```powershell
$run = Join-Path (Get-Location) ('.scratch\pr583-b-bound-bypass-restore-' + (Get-Date -Format 'yyyyMMdd-HHmmssfff') + '-' + [guid]::NewGuid().ToString('N')); New-Item -ItemType Directory -Path $run | Out-Null; $env:MCPHUB_STATE_DIR_OVERRIDE = Join-Path $run 'state'; New-Item -ItemType Directory -Path $env:MCPHUB_STATE_DIR_OVERRIDE | Out-Null; $sw=[System.Diagnostics.Stopwatch]::StartNew(); & go test -tags=test_state_path_env -count=1 -timeout 10m -run '^TestRegister_CleanupBoundClientBypassesManagedRouterProof$' ./internal/api 2>&1 | Tee-Object -FilePath (Join-Path $run 'go-test.txt'); $code = $LASTEXITCODE; $sw.Stop(); Set-Content -LiteralPath (Join-Path $run 'exit-code.txt') -Value $code; Set-Content -LiteralPath (Join-Path $run 'wall-time.txt') -Value $sw.Elapsed.TotalSeconds.ToString('F3',[Globalization.CultureInfo]::InvariantCulture); if ($code -ne 0) { throw "bound-bypass restore failed; evidence: $run" }
```

Focused current-source API guards:

```powershell
$run = Join-Path (Get-Location) ('.scratch\pr583-b-current-api-' + (Get-Date -Format 'yyyyMMdd-HHmmssfff') + '-' + [guid]::NewGuid().ToString('N')); New-Item -ItemType Directory -Path $run | Out-Null; $env:MCPHUB_STATE_DIR_OVERRIDE = Join-Path $run 'state'; New-Item -ItemType Directory -Path $env:MCPHUB_STATE_DIR_OVERRIDE | Out-Null; $sw=[System.Diagnostics.Stopwatch]::StartNew(); & go test -tags=test_state_path_env -count=1 -timeout 10m -run '^(TestManagedRouterProbe|TestRegister_CleanupKeepsDirectEntryWhenConfiguredRouterIsStopped|TestRegister_CleanupKeepsDirectEntryWhenRouterPortHasForeignListener|TestRegister_CleanupRemovesDirectEntryWithProvenManagedRouter|TestRegister_CleanupBoundClientBypassesManagedRouterProof|TestRegister_CleanupAliasAuthorizationForDirectEntryKinds|TestRegister_CleanupRejectsInvalidRouterEntries|TestRegister_ClientScopeResolvedOnceForTheWholeRegistration)$' ./internal/api 2>&1 | Tee-Object -FilePath (Join-Path $run 'go-test.txt'); $code = $LASTEXITCODE; $sw.Stop(); Set-Content -LiteralPath (Join-Path $run 'exit-code.txt') -Value $code; Set-Content -LiteralPath (Join-Path $run 'wall-time.txt') -Value $sw.Elapsed.TotalSeconds.ToString('F3',[Globalization.CultureInfo]::InvariantCulture); if ($code -ne 0) { throw "focused current-source API tests failed; evidence: $run" }
```

Focused current-source GUI guards:

```powershell
$run = Join-Path (Get-Location) ('.scratch\pr583-b-current-gui-' + (Get-Date -Format 'yyyyMMdd-HHmmssfff') + '-' + [guid]::NewGuid().ToString('N')); New-Item -ItemType Directory -Path $run | Out-Null; $env:MCPHUB_STATE_DIR_OVERRIDE = Join-Path $run 'state'; New-Item -ItemType Directory -Path $env:MCPHUB_STATE_DIR_OVERRIDE | Out-Null; $sw=[System.Diagnostics.Stopwatch]::StartNew(); & go test -tags=test_state_path_env -count=1 -timeout 10m -run '^(TestProjectsToggle_RegisterSuppliesManagedGUIIdentity|TestPing_OrdinaryWireShapeRemainsByteCompatible)$' ./internal/gui 2>&1 | Tee-Object -FilePath (Join-Path $run 'go-test.txt'); $code = $LASTEXITCODE; $sw.Stop(); Set-Content -LiteralPath (Join-Path $run 'exit-code.txt') -Value $code; Set-Content -LiteralPath (Join-Path $run 'wall-time.txt') -Value $sw.Elapsed.TotalSeconds.ToString('F3',[Globalization.CultureInfo]::InvariantCulture); if ($code -ne 0) { throw "focused current-source GUI tests failed; evidence: $run" }
```

Final restoration proof:

```powershell
Get-FileHash -Algorithm SHA256 -LiteralPath 'internal\api\register.go','internal\api\register_test.go','internal\gui\projects_toggle.go','internal\gui\projects_toggle_test.go'
git status --short
git diff --check
git diff --name-only -- internal
Select-String -LiteralPath 'internal\api\register.go' -Pattern 'bound && !liveManaged' -SimpleMatch
```

## Defect-class participant disposition

| Participant | QA disposition |
|---|---|
| Replacement origin | Fixed: both the pre-existing router liveness term and same-registration bound bypass have independent red/green mutation evidence. |
| Listener state | Fixed for the reviewed defect: stopped and foreign listeners kill the unsafe mutation and pass after restoration; managed, timeout, and malformed cases pass in the focused current-source API suite. |
| Router-entry validity | Not affected by QA mutations; missing, disabled, malformed, non-loopback, wrong-language, stale-port, and matching-owned cases remain covered by the focused current-source guard. |
| Direct-entry kind | Not affected by QA mutations; `mcp-language-server` and direct `gopls` rows pass in the focused current-source guard. |
| Client grain | Fixed: the unbound router-origin and bound same-registration branches have separate mutation proofs. |
| Language grain | Not affected by QA mutations; per-language positive and negative cases pass in the focused current-source guard. |
| Caller provenance | Not affected by QA mutations; the GUI server-owned identity guard passes, while absent identity remains part of the bound mutation fixture. |
| Warning surface | Not affected by QA mutations; stopped/foreign guards and focused current-source tests retain warning-class/cardinality assertions. |
| Return/skip paths | Fixed for the two reviewed authorization gates; backup/removal behavior is the direct failing discriminator in both mutation runs. |

## Residual risks

- These are bounded unit-level mutation proofs with in-process loopback fixtures,
  not a live GUI or scheduler smoke. Phase B intentionally did not launch those
  surfaces.
- Managed-router liveness remains a point-in-time proof; the process can stop
  after a successful probe and before later configuration writes.
- The compact Go output reports top-level package success rather than verbose
  subtest names. Counts above are the named top-level tests selected by each
  exact regular expression; no skip marker appeared in raw output.
- Resource-body closure and redirect behavior still require the planned Phase C
  code-path review in addition to the green tests.

## Revision 1 re-verification

### Scope and verdict

This was an independent, source-read-only re-verification after the
architecture-review correction in `implementation.md:112-137`. The revised
`managedRouterProofNeeded` now composes the existing per-language router-entry
gate with the shared workspace-scoped direct-candidate matcher before deciding
whether the cleanup-wide listener probe is needed
(`internal/api/register.go:533-566`, `internal/api/register.go:649-678`).

**Gate: PASS for the Revision 1 no-candidate correction, with the uncovered
high-level `port-unresolved` path retained as a Phase C review risk.**

### Current file hashes

The hashes were identical before and after all re-verification runs:

| File | SHA-256 |
|---|---|
| `internal/api/register.go` | `E8DCA649627AC8A9DDE8250A94CAFF4A5EC470A757E164755989ADE7A3C74509` |
| `internal/api/register_test.go` | `AC649AE8F20EA0FA5C0A90B6485D969770C1E7E530208E9E4360021EFB28E911` |
| `internal/gui/projects_toggle.go` | `F3E145A933057DAB4B2E682B565055854982A1D43FA65A17574E14630DDFF590` |
| `internal/gui/projects_toggle_test.go` | `E5D8FDDA4796EA42AA04A816891424B233139682E0ED8376E96AF66FBCDCB8C6` |

### Required guards

| Check | Counts | Result | Raw evidence |
|---|---:|---|---|
| Two new no-candidate guards | 2 named top-level passed; 0 failed; 0 skipped; 0 expected-failure cases | Exit 0; Go package time 0.035 seconds; wall time 0.960 seconds | `.scratch/pr583-r1-qa-no-candidate-20260727-070945704-3fdf76529dbf4bcd8e6dbffc8867e306/{go-test.txt,exit-code.txt,wall-time.txt}` |
| Full focused API guards | 10 named top-level and 31 nested subtests passed; 0 failed; 0 skipped; 0 expected-failure cases | Exit 0; Go package time 0.650 seconds; wall time 1.592 seconds | `.scratch/pr583-r1-qa-focused-api-20260727-071014673-f1ab50f4db72430488179cb824fc7d03/{go-test.txt,exit-code.txt,wall-time.txt}` |
| GUI identity and ping-wire guards | 2 named top-level passed; 0 failed; 0 skipped; 0 expected-failure cases | Exit 0; Go package time 0.024 seconds; wall time 4.460 seconds | `.scratch/pr583-r1-qa-gui-20260727-071014660-a04ad16013e649be8875627ddc2805c9/{go-test.txt,exit-code.txt,wall-time.txt}` |

The new guards assert the absolute no-candidate properties, not merely a green
return:

- without a registered-language router entry: zero HTTP requests, zero proof
  warnings, direct entry preserved, and zero backups
  (`internal/api/register_test.go:4129-4175`);
- with a valid router entry but only a different-workspace direct entry: zero
  HTTP requests, zero proof warnings, non-candidate preserved, and zero backups
  (`internal/api/register_test.go:4178-4238`).

### Invalid-router request-count inspection

`TestRegister_CleanupRejectsInvalidRouterEntries` leaves `wantRequests` at its
zero value for `missing`, `disabled`, `malformed`, `non-loopback`,
`wrong-language`, and `stale-port`; only `matching-owned` sets
`wantRequests: 1` (`internal/api/register_test.go:4660-4688`). Every table row
compares the observed request counter with that exact expected value
(`internal/api/register_test.go:4746-4747`). All seven rows passed in the fresh
focused API run.

### Uncovered high-level path

`port-unresolved` remains covered only by the pure
`probeManagedRouter` invalid-input table
(`internal/api/register_test.go:3948`). No high-level registration/cleanup test
asserts a `port-unresolved` warning. In current production flow,
`cleanupDirectLanguageServerEntriesAfterRegister` calls
`managedRouterProofNeeded` only when `lspRouterGUIPort` returns no error; a
resolution error bypasses both the proof and warning block
(`internal/api/register.go:697-712`).

This does not invalidate the verified Revision 1 requirement that irrelevant
clients and non-candidates cause zero requests and zero proof warnings. It does
mean this QA pass is not evidence that the accepted high-level
`port-unresolved` diagnostic remains reachable. Phase C should decide whether
that silent resolution-error path conforms to the warning contract.

### Executed commands

Two new no-candidate guards:

```powershell
$run = Join-Path (Get-Location) ('.scratch\pr583-r1-qa-no-candidate-' + (Get-Date -Format 'yyyyMMdd-HHmmssfff') + '-' + [guid]::NewGuid().ToString('N')); New-Item -ItemType Directory -Path $run | Out-Null; $env:MCPHUB_STATE_DIR_OVERRIDE = Join-Path $run 'state'; New-Item -ItemType Directory -Path $env:MCPHUB_STATE_DIR_OVERRIDE | Out-Null; $sw=[System.Diagnostics.Stopwatch]::StartNew(); & go test -v -tags=test_state_path_env -count=1 -timeout 10m -run '^(TestRegister_CleanupSkipsManagedRouterProofWithoutRegisteredLanguageRouterEntry|TestRegister_CleanupSkipsManagedRouterProofWithoutMatchingDirectCandidate)$' ./internal/api 2>&1 | Tee-Object -FilePath (Join-Path $run 'go-test.txt'); $code=$LASTEXITCODE; $sw.Stop(); Set-Content -LiteralPath (Join-Path $run 'exit-code.txt') -Value $code; Set-Content -LiteralPath (Join-Path $run 'wall-time.txt') -Value $sw.Elapsed.TotalSeconds.ToString('F3',[Globalization.CultureInfo]::InvariantCulture); if($code -ne 0){ throw "Revision 1 no-candidate guards failed; evidence: $run" }
```

Full focused API guards:

```powershell
$run = Join-Path (Get-Location) ('.scratch\pr583-r1-qa-focused-api-' + (Get-Date -Format 'yyyyMMdd-HHmmssfff') + '-' + [guid]::NewGuid().ToString('N')); New-Item -ItemType Directory -Path $run | Out-Null; $env:MCPHUB_STATE_DIR_OVERRIDE = Join-Path $run 'state'; New-Item -ItemType Directory -Path $env:MCPHUB_STATE_DIR_OVERRIDE | Out-Null; $sw=[System.Diagnostics.Stopwatch]::StartNew(); & go test -v -tags=test_state_path_env -count=1 -timeout 10m -run '^(TestManagedRouterProbe|TestRegister_CleanupKeepsDirectEntryWhenConfiguredRouterIsStopped|TestRegister_CleanupKeepsDirectEntryWhenRouterPortHasForeignListener|TestRegister_CleanupRemovesDirectEntryWithProvenManagedRouter|TestRegister_CleanupBoundClientBypassesManagedRouterProof|TestRegister_CleanupAliasAuthorizationForDirectEntryKinds|TestRegister_CleanupRejectsInvalidRouterEntries|TestRegister_ClientScopeResolvedOnceForTheWholeRegistration|TestRegister_CleanupSkipsManagedRouterProofWithoutRegisteredLanguageRouterEntry|TestRegister_CleanupSkipsManagedRouterProofWithoutMatchingDirectCandidate)$' ./internal/api 2>&1 | Tee-Object -FilePath (Join-Path $run 'go-test.txt'); $code=$LASTEXITCODE; $sw.Stop(); Set-Content -LiteralPath (Join-Path $run 'exit-code.txt') -Value $code; Set-Content -LiteralPath (Join-Path $run 'wall-time.txt') -Value $sw.Elapsed.TotalSeconds.ToString('F3',[Globalization.CultureInfo]::InvariantCulture); if($code -ne 0){ throw "Revision 1 focused API guards failed; evidence: $run" }
```

GUI identity and ping-wire guards:

```powershell
$run = Join-Path (Get-Location) ('.scratch\pr583-r1-qa-gui-' + (Get-Date -Format 'yyyyMMdd-HHmmssfff') + '-' + [guid]::NewGuid().ToString('N')); New-Item -ItemType Directory -Path $run | Out-Null; $env:MCPHUB_STATE_DIR_OVERRIDE = Join-Path $run 'state'; New-Item -ItemType Directory -Path $env:MCPHUB_STATE_DIR_OVERRIDE | Out-Null; $sw=[System.Diagnostics.Stopwatch]::StartNew(); & go test -v -tags=test_state_path_env -count=1 -timeout 10m -run '^(TestProjectsToggle_RegisterSuppliesManagedGUIIdentity|TestPing_OrdinaryWireShapeRemainsByteCompatible)$' ./internal/gui 2>&1 | Tee-Object -FilePath (Join-Path $run 'go-test.txt'); $code=$LASTEXITCODE; $sw.Stop(); Set-Content -LiteralPath (Join-Path $run 'exit-code.txt') -Value $code; Set-Content -LiteralPath (Join-Path $run 'wall-time.txt') -Value $sw.Elapsed.TotalSeconds.ToString('F3',[Globalization.CultureInfo]::InvariantCulture); if($code -ne 0){ throw "Revision 1 GUI guards failed; evidence: $run" }
```

Diff hygiene and boundary:

```powershell
git diff --check
git diff --name-only -- internal
```

Both exited 0. The internal diff boundary remains exactly:

```text
internal/api/register.go
internal/api/register_test.go
internal/gui/projects_toggle.go
internal/gui/projects_toggle_test.go
```

No source, test, commit, checkout, reset, stash, push, GUI, tray, supervisor,
scheduler, or child application mutation was performed by this re-verification.

## Revision 2 re-verification

### Scope and gate

This was an independent, source-read-only verification of the typed
port-independent cleanup preflight described in `design.md:292-590` and
implemented in `implementation.md:146-197`. Every current
`TestCleanupDirectLSP_` guard and every requested adjacent must-not-break guard
ran against current source with a fresh isolated state directory.

**Gate: PASS**

### Current source hashes

The hashes were identical before and after every test, diagnostic, and static
inspection:

| File | SHA-256 |
|---|---|
| `internal/api/register.go` | `BEC52E1178FB992E1DAB9CF1986B12013E578B33C5080109972069FFC2E23727` |
| `internal/api/register_test.go` | `709F854EF64DC786D6A9D62FAAF17C78ECD076E74E5622067ADD6D0A2F9F1500` |
| `internal/gui/projects_toggle.go` | `F3E145A933057DAB4B2E682B565055854982A1D43FA65A17574E14630DDFF590` |
| `internal/gui/projects_toggle_test.go` | `E5D8FDDA4796EA42AA04A816891424B233139682E0ED8376E96AF66FBCDCB8C6` |

### Fresh execution evidence

| Check | Counts | Result | Raw evidence |
|---|---:|---|---|
| Complete focused API guard set | 18 named top-level tests and 33 nested subtests passed; 0 failed; 0 skipped; 0 expected-failure cases | Exit 0; Go package time 0.642 seconds; wall time 1.605 seconds | `.scratch/pr583-r2-qa-focused-api-20260727-075230005-a7a8c3197e864ea78cc1e4925eab2956/{go-test.txt,exit-code.txt,wall-time.txt}` |
| GUI identity and ping-wire guards | 2 named top-level tests passed; 0 failed; 0 skipped; 0 expected-failure cases | Exit 0; Go package time 0.028 seconds; wall time 4.476 seconds | `.scratch/pr583-r2-qa-gui-20260727-075253372-c18ba55c6ce04679b4bb385e8cad19ff/{go-test.txt,exit-code.txt,wall-time.txt}` |
| Hash, diff-hygiene, boundary, and control-flow snapshot | Four hashes matched; `git diff --check` exit 0; internal boundary exact | Exit 0 | `.scratch/pr583-r2-qa-static-audit-20260727-075337223-c22ca9006ed0413abe4d11406f78ec80/{hashes.txt,git-diff-check.txt,internal-boundary.txt,control-flow.txt,exit-code.txt}` |

The API regular expression included all eight current Revision 2 guards:

1. `TestCleanupDirectLSP_PortResolutionErrorWarnsOnceAndDoesNotMutate`
2. `TestCleanupDirectLSP_GetEntryErrorIsReturnedOnceBeforeAnySideEffect`
3. `TestCleanupDirectLSP_DirectScanErrorIsReturnedOnceBeforeAnySideEffect`
4. `TestCleanupDirectLSP_StaleRouterPortIsNotProbed`
5. `TestCleanupDirectLSP_MatchingOwnedCandidateUsesOneCachedProof`
6. `TestCleanupDirectLSP_NoRouterEntrySkipsResolverProbeAndWarning`
7. `TestCleanupDirectLSP_NoDirectCandidateSkipsResolverProbeAndWarning`
8. `TestCleanupDirectLSP_BoundOnlyPlanNeverResolvesOrProbes`

It also included `TestManagedRouterProbe`, stopped, foreign, managed-positive,
bound-bypass, invalid-router-entry, both-direct-kind, one-binding-snapshot, and
both Revision 1 no-candidate guards.

### Control-flow proof

| Required invariant | Static owner | Falsifying test observation |
|---|---|---|
| A preflight diagnostic aborts before resolver, probe, backup, or removal. | `buildDirectCleanupPreflight` produces explicit `complete`; the worker aggregates diagnostics and returns at `internal/api/register.go:883-896` before resolver/probe at `:898-914` and mutation at `:916-941`. | GetEntry error asserted resolver/probe/matcher `0/0/0`, zero backup/removal, and one diagnostic. Candidate/survivor errors asserted resolver/probe `0/0`, one preflight matcher call, zero backup/removal, and one diagnostic. |
| A relevant router-port resolution error warns once and mutates nothing. | One resolver call is inside `preflight.hasRouterMatches`; error goes through one `addProofWarning("port-unresolved")` and immediately returns at `internal/api/register.go:900-906`. | `TestCleanupDirectLSP_PortResolutionErrorWarnsOnceAndDoesNotMutate` asserted resolver 1, probe 0, zero bound/unbound backup/removal, and warning count 1 in both returned warnings and progress output. |
| A stale structural port is never probed. | `hasRouterMatchesForPort` gates the sole probe call at `internal/api/register.go:907-913`; exact equality is owned by `routerReplacementPortMatches` at `:234-236`. | Stale-port guard asserted resolver/probe/matcher `1/0/1`, no warning, no backup, and preserved direct entry. |
| An exact structural port uses exactly one managed proof. | The worker has one non-loop probe call site at `internal/api/register.go:907-913`. | Matching-owned guard used two eligible clients and asserted resolver 1, probe 1, request 1, one backup/removal per client. |
| No matcher runs after proof. | All matcher calls occur while building the preflight at `internal/api/register.go:620-685`; the post-proof execution loop consumes only cached match slices at `:916-941`. | Matching-owned guard made the injected client fail on a second scan, observed one client scan and two total client-group matcher calls, then successfully removed both cached entries. |
| A bound-only plan performs no router resolution or probe. | Bound aliases populate cached `boundMatches`; `hasRouterMatches` remains false and the execution loop uses the cached bound slice. | Bound-only guard asserted resolver/probe/matcher `0/0/1`, one backup, direct removal, and no proof warning. |
| Warning strings are deduplicated by one owner. | `directCleanupWarningAccumulator` owns a full-string `seen` map; both `addDiagnostic` and `addProofWarning` return for already-seen strings at `internal/api/register.go:813-840`. | Port-resolution, GetEntry, and direct-scan guards asserted exact report counts of one; the proof-warning guard also asserted exactly one progress line. |

### Go diagnostics

Current Go language-server diagnostics were requested for
`internal/api/register.go` and `internal/api/register_test.go`. They returned no
errors or warnings. `register_test.go` had no diagnostics. `register.go` had two
non-blocking style hints:

```text
690:3-690:8: [Hint] Replace m[k]=v loop with maps.Copy
1269:2-1269:17: [Hint] Replace m[k]=v loop with maps.Copy
```

### Executed test commands

Focused API guards:

```powershell
$run = Join-Path (Get-Location) ('.scratch\pr583-r2-qa-focused-api-' + (Get-Date -Format 'yyyyMMdd-HHmmssfff') + '-' + [guid]::NewGuid().ToString('N')); New-Item -ItemType Directory -Path $run | Out-Null; $env:MCPHUB_STATE_DIR_OVERRIDE = Join-Path $run 'state'; New-Item -ItemType Directory -Path $env:MCPHUB_STATE_DIR_OVERRIDE | Out-Null; $sw=[System.Diagnostics.Stopwatch]::StartNew(); & go test -v -tags=test_state_path_env -count=1 -timeout 10m -run '^(TestCleanupDirectLSP_PortResolutionErrorWarnsOnceAndDoesNotMutate|TestCleanupDirectLSP_GetEntryErrorIsReturnedOnceBeforeAnySideEffect|TestCleanupDirectLSP_DirectScanErrorIsReturnedOnceBeforeAnySideEffect|TestCleanupDirectLSP_StaleRouterPortIsNotProbed|TestCleanupDirectLSP_MatchingOwnedCandidateUsesOneCachedProof|TestCleanupDirectLSP_NoRouterEntrySkipsResolverProbeAndWarning|TestCleanupDirectLSP_NoDirectCandidateSkipsResolverProbeAndWarning|TestCleanupDirectLSP_BoundOnlyPlanNeverResolvesOrProbes|TestManagedRouterProbe|TestRegister_CleanupKeepsDirectEntryWhenConfiguredRouterIsStopped|TestRegister_CleanupKeepsDirectEntryWhenRouterPortHasForeignListener|TestRegister_CleanupRemovesDirectEntryWithProvenManagedRouter|TestRegister_CleanupBoundClientBypassesManagedRouterProof|TestRegister_CleanupRejectsInvalidRouterEntries|TestRegister_CleanupAliasAuthorizationForDirectEntryKinds|TestRegister_ClientScopeResolvedOnceForTheWholeRegistration|TestRegister_CleanupSkipsManagedRouterProofWithoutRegisteredLanguageRouterEntry|TestRegister_CleanupSkipsManagedRouterProofWithoutMatchingDirectCandidate)$' ./internal/api 2>&1 | Tee-Object -FilePath (Join-Path $run 'go-test.txt'); $code=$LASTEXITCODE; $sw.Stop(); Set-Content -LiteralPath (Join-Path $run 'exit-code.txt') -Value $code; Set-Content -LiteralPath (Join-Path $run 'wall-time.txt') -Value $sw.Elapsed.TotalSeconds.ToString('F3',[Globalization.CultureInfo]::InvariantCulture); if($code -ne 0){ throw "Revision 2 focused API guards failed; evidence: $run" }
```

GUI identity and ping-wire guards:

```powershell
$run = Join-Path (Get-Location) ('.scratch\pr583-r2-qa-gui-' + (Get-Date -Format 'yyyyMMdd-HHmmssfff') + '-' + [guid]::NewGuid().ToString('N')); New-Item -ItemType Directory -Path $run | Out-Null; $env:MCPHUB_STATE_DIR_OVERRIDE = Join-Path $run 'state'; New-Item -ItemType Directory -Path $env:MCPHUB_STATE_DIR_OVERRIDE | Out-Null; $sw=[System.Diagnostics.Stopwatch]::StartNew(); & go test -v -tags=test_state_path_env -count=1 -timeout 10m -run '^(TestProjectsToggle_RegisterSuppliesManagedGUIIdentity|TestPing_OrdinaryWireShapeRemainsByteCompatible)$' ./internal/gui 2>&1 | Tee-Object -FilePath (Join-Path $run 'go-test.txt'); $code=$LASTEXITCODE; $sw.Stop(); Set-Content -LiteralPath (Join-Path $run 'exit-code.txt') -Value $code; Set-Content -LiteralPath (Join-Path $run 'wall-time.txt') -Value $sw.Elapsed.TotalSeconds.ToString('F3',[Globalization.CultureInfo]::InvariantCulture); if($code -ne 0){ throw "Revision 2 GUI guards failed; evidence: $run" }
```

### Diff boundary and residual risk

`git diff --check` exited 0. `git diff --name-only -- internal` returned exactly:

```text
internal/api/register.go
internal/api/register_test.go
internal/gui/projects_toggle.go
internal/gui/projects_toggle_test.go
```

No blocking uncovered risk was found in the requested Revision 2 scope. One
lower-risk test gap remains: exact-string diagnostic deduplication is directly
visible in the single accumulator and the named guards assert one-count
outcomes, but there is no dedicated test that injects the same diagnostic
through multiple structural-port groups. Phase C can inspect that small
coverage gap without changing the PASS result for the verified orchestration
contracts.

No source, test, commit, checkout, reset, stash, push, GUI, tray, supervisor,
scheduler, or child application mutation was performed by this QA pass.

## Final committed-code mutation proof

After fix commit `50a0e4b0`, the main session changed only the cleanup
authorization assignment from `snapshot.liveManaged` to `true`. This mutation
recreated the original defect class against the final cached-preflight
implementation.

The exact stopped-router and foreign-listener guards both failed:

```text
--- FAIL: TestRegister_CleanupKeepsDirectEntryWhenConfiguredRouterIsStopped
register_test.go:5074: stopped configured router authorized removal of direct entry "legacy-go-stopped-router"
--- FAIL: TestRegister_CleanupKeepsDirectEntryWhenRouterPortHasForeignListener
register_test.go:5151: foreign listener authorized removal of cursor direct entry "legacy-go-cursor"
FAIL mcp-local-hub/internal/api
```

The mutation run exited 1. Evidence:
`.scratch/pr583-final-liveness-mutation-20260727-081142642-e06a10721a9843c5a8f238b2c9c867bd/`.

The reverse patch restored `snapshot.liveManaged`. The same two guards then
passed with exit 0:

```text
ok mcp-local-hub/internal/api 0.038s
```

Restored evidence:
`.scratch/pr583-final-liveness-restored-20260727-081203005-35bb58f2d07143adaba16496f570d9a2/`.
The restored `internal/api/register.go` SHA-256 is
`BEC52E1178FB992E1DAB9CF1986B12013E578B33C5080109972069FFC2E23727`,
exactly matching the committed Revision 2 hash. `git status --short` was empty.

## Terms and Abbreviations

- **API** — Application Programming Interface.
- **CLI** — Command-Line Interface.
- **GUI** — Graphical User Interface.
- **LSP** — Language Server Protocol.
- **PID** — Process identifier.
- **QA** — Quality Assurance.
- **SHA-256** — Secure Hash Algorithm 256-bit file digest.
