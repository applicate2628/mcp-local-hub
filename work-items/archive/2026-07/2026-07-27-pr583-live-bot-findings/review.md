# Architecture review: managed router proof

## Gate

**REVISE**

## Blocking finding

| Severity | Location | Finding | Required correction |
|---|---|---|---|
| P2 | `internal/api/register.go:533-539` | `managedRouterProofNeeded` returns true for every existing unbound client. It checks neither whether that client has a relevant pre-existing router entry for a registered language nor whether a direct cleanup candidate exists. As a result, an ordinary command-line registration with no supplied identity (`internal/cli/register.go:115-120`) can emit `identity-not-supplied` even though no router-origin cleanup authorization could be used; an in-process GUI registration can make an unnecessary loopback probe. This contradicts the accepted “only when router-origin authorization is needed” rule in `plan.md:104-106`, A-AC2/A-AC7 in `plan.md:150-168`, and the no-warning-when-no-proof-is-needed rule in `design.md:159-162`. | Make the need decision use the actual registered-language router-entry and direct-cleanup-candidate class. Add guards for an existing unbound client with no router entry and for a router entry with no matching direct cleanup candidate; both must make zero requests and emit no router-proof warning. Keep one cleanup-wide proof and the existing bound bypass. |

## Eight-claim disposition

| Claim | Disposition at this gate |
|---:|---|
| 1. Pre-existing router cleanup needs one bounded managed proof. | REVISE: the proof gates destructive cleanup correctly, but the decision to perform it is broader than the actual cleanup candidate class. |
| 2. Bound clients remain eligible without GUI proof. | Evidence present in `internal/api/register.go:754-787`, the named bound guard, and the QA mutation; final acceptance deferred until re-review. |
| 3. Router-origin authorization remains per client/language. | Evidence present in the alias owner and managed Go/sibling Python guard; final acceptance deferred until re-review. |
| 4. Both direct-entry kinds use one alias authorization. | Evidence present in the shared alias result and table guard; final acceptance deferred until re-review. |
| 5. Proof failure/timeout is fail-closed and visible once. | Cleanup permission is fail-closed, but visibility is currently triggered when no proof is needed; REVISE. |
| 6. One registration retains one binding snapshot. | Evidence present in the sole pre-loop binding resolution and existing named guard; final acceptance deferred until re-review. |
| 7. Public HTTP, CLI, configuration, and persisted-state contracts are unchanged. | No protected-file diff observed; final acceptance deferred until re-review. |
| 8. Only four approved internal files changed. | PASS at review time: `git diff --name-only -- internal` contains the four approved paths. |

## Anti-layering and resource review

- Liveness enforcement is centralized in the cleanup-wide snapshot and is not
  duplicated in either direct-entry matcher or the backup/removal loop.
- The bound bypass and unbound managed-proof formula remain in
  `lspCleanupAliasesForClient`.
- The response path uses one request, a 500 ms context/client ceiling, refuses
  redirects, caps the body at 4 KiB, rejects a second JSON value, and defers body
  closure.
- The GUI caller supplies port, PID, and version from its owning `Server`.
- The one open issue is the candidate-need boundary above; no second liveness
  layer should be added to correct it.

## Re-verification required

The same `$architecture-reviewer` angle must review the corrected candidate
selection and re-run the eight-claim gate. The existing mutation proof remains
valid for the two authorization branches, but QA must at least verify the new
no-candidate guards and current-source hashes after the revision.

## Revision 1 re-review

### Gate

**REVISE**

The original P2 finding at former `internal/api/register.go:533-539` is
**fixed**. The revised candidate pass now requires both a valid
registered-language router entry and a workspace-matching direct cleanup
candidate before it requests the cleanup-wide managed-listener proof
(`internal/api/register.go:533-565`). The two new guards cover the no-router and
wrong-workspace/no-direct-candidate cases with zero requests, zero proof
warnings, zero backups, and preserved direct entries
(`internal/api/register_test.go:4129-4238`). Revision 1 nevertheless remains
`REVISE` because two sibling failure paths in the revised candidate pass are
silent.

### Reviewed surfaces

- Revision 1 artifacts: `implementation.md:112-137` and
  `verification.md:190-257`.
- Candidate/probe/cleanup control flow:
  `internal/api/register.go:533-756`.
- New no-candidate guards:
  `internal/api/register_test.go:4117-4238`.
- GUI identity source and guard:
  `internal/gui/projects_toggle.go:170-185` and
  `internal/gui/projects_toggle_test.go:61-107`.
- Unchanged ping producer and byte guard:
  `internal/gui/ping.go:17-29` and `internal/gui/ping_test.go:13-42`.
- Accepted failure and warning contract:
  `plan.md:104-140`, `plan.md:150-165`, and `design.md:189-218`.
- Current internal/protected diff boundary. No tests, build, vet, GUI, tray,
  supervisor, scheduler, or child application were launched in this re-review;
  the preserved Revision 1 QA evidence was inspected rather than re-executed.

### Blocking findings

#### R1-F1 — router-port resolution failure bypasses the accepted warning

- **Severity:** P2
- **Defect class:** incomplete failure-path propagation / silent
  `port-unresolved` outcome
- **Location:** `internal/api/register.go:697-712`
- **Single owner:** `cleanupDirectLanguageServerEntriesAfterRegister`
- **fix-class:** `design-decision`
- **WHAT:** The cleanup calls `managedRouterProofNeeded` and enters the
  snapshot/warning block only when `routerPortErr == nil`
  (`internal/api/register.go:697-705`). When router-port resolution fails,
  `probeManagedRouter` is not called and no snapshot receives the
  `port-unresolved` class, even though the pure probe supports that class
  (`internal/api/register.go:568-573`). The zero snapshot then keeps every
  unbound direct entry, but no report warning or progress warning is emitted.
  This contradicts the exact failure-class contract in `plan.md:122-128`,
  A-AC2 in `plan.md:150-153`, and the explicit failure-mode row requiring the
  resolution warning to be consolidated as `port-unresolved` in
  `design.md:198-202`.
- **Failure scenario:** An unbound client has a registered-language router
  entry and a matching workspace direct candidate, while `GUIPort` is zero and
  `gui_server.port` cannot be resolved. Cleanup is safely suppressed, but the
  operator receives no reason.
- **Falsifying probe:** A high-level registration guard with that candidate
  setup and an induced `lspRouterGUIPort` resolution error must observe zero
  HTTP requests, zero backup/removal, and exactly one `port-unresolved`
  router-proof warning in both the returned report and progress output. The
  current suite has only the pure-probe row
  (`internal/api/register_test.go:3948`), as QA also recorded at
  `verification.md:243-257`.
- **ADVISORY HOW (non-binding):** Preserve one cleanup owner by deriving a
  port-independent “router-origin cleanup candidate may exist” result before
  the port-dependent authorization check, or by returning a typed candidate
  result that can distinguish `not-needed`, `needed`, and
  `indeterminate-with-diagnostic`. When need is established and port resolution
  fails, emit the existing `port-unresolved` snapshot warning once and do not
  request or clean. Avoid a second router-shape owner; the architect should
  choose how the existing configured-entry gate exposes the pre-port candidate
  state.

#### R1-F2 — candidate-pass diagnostics are discarded

- **Severity:** P2
- **Defect class:** failure-transparency loss / inconsistent failure idiom
- **Location:** `internal/api/register.go:548-560`
- **Single owner:** `managedRouterProofNeeded` candidate-pass result
- **fix-class:** `design-decision`
- **WHAT:** The candidate pass discards both warning channels:
  `aliases, _ := lspCleanupAliasesForClient(...)` at
  `internal/api/register.go:548-556` and
  `matches, _ := directLanguageServerCleanupMatches(...)` at
  `internal/api/register.go:560`. Those helpers deliberately create
  per-entry and direct-scan warnings (`internal/api/register.go:649-678`).
  If either read fails, the candidate pass returns false; the later cleanup
  receives `liveManaged == false`, produces no unbound aliases, and skips the
  match composer at `internal/api/register.go:721-740`. There is therefore no
  second opportunity to surface the discarded diagnostic. Cleanup stays
  fail-closed, but a real or indeterminate router-origin candidate is preserved
  silently. This conflicts with the accepted per-entry warning behavior in
  `design.md:202` and the return/skip-path requirement in
  `plan.md:64-74`.
- **Failure scenario:** An unbound client has a matching direct candidate, but
  its router-entry read fails; or it has a valid router entry but direct
  candidate/survivor discovery fails. The operation performs no destructive
  cleanup and returns no warning explaining why.
- **Falsifying probe:** Add one high-level guard for an unbound candidate whose
  `GetEntry` fails and one for a direct candidate/survivor scan failure. Each
  must observe zero request/backup/removal and exactly one existing
  per-entry/scan diagnostic in `RegisterReport.Warnings`; neither error may be
  converted into proof need or cleanup permission.
- **ADVISORY HOW (non-binding):** Return a structured candidate-pass result
  carrying both `needed` and diagnostics, and aggregate those diagnostics once
  at the cleanup owner even when `needed` is false. A reusable candidate
  inventory may avoid repeating scans after a successful proof, but it must not
  move identity/liveness policy into either matcher.

### Eight-claim Claim-Verify map

| Claim | Owner and source/test evidence | Falsifying probe | Verdict |
|---:|---|---|---|
| 1. Pre-existing router cleanup needs one bounded managed proof. | The revised need pass is one cleanup-owned precondition at `internal/api/register.go:533-565`; the proof is still one snapshot at `internal/api/register.go:697-712`. The new no-router and no-direct-candidate guards are `internal/api/register_test.go:4129-4238`. | Existing unbound but irrelevant clients must produce zero requests/warnings; a matching router plus direct candidate must produce at most one request and may authorize only after exact identity. | **verified / PASS** |
| 2. Bound clients remain eligible without GUI proof. | The bound branch remains in the single alias owner at `internal/api/register.go:791`; the named guard starts at `internal/api/register_test.go:4257`, and the prior bound-only mutation failed as required. | Require `liveManaged` for `bound == true`; the bound guard must fail because the replaced bound entry survives. | **verified / PASS** |
| 3. Router-origin authorization remains per client/language. | `lspCleanupAliasesForClient` remains the per-client/per-language owner at `internal/api/register.go:791`; the managed Go/sibling Python guard starts at `internal/api/register_test.go:4577`. | Give one client a valid Go router entry but no Python entry; any Python deletion falsifies the claim. | **verified / PASS** |
| 4. Both direct-entry kinds use one alias authorization. | The shared match composer is `internal/api/register.go:649-678`, and the removal path consumes its result once at `internal/api/register.go:737-740`; the two-kind guard starts at `internal/api/register_test.go:4753`. | Bypass the shared aliases for either `mcp-language-server` or direct `gopls`; the table must show unequal outcomes for identical evidence. | **verified / PASS** |
| 5. Proof failure/timeout is fail-closed and visible once. | Network/status/body/timeout failures remain bounded and fail-closed at `internal/api/register.go:568-640`, but `routerPortErr` bypasses warning creation at `internal/api/register.go:697-712`, and candidate-pass diagnostics are discarded at `internal/api/register.go:548-560`. | A genuine candidate plus router-port resolution error must produce one `port-unresolved` warning; alias and match read failures must produce their existing diagnostics while keeping zero request/backup/removal. | **failed / REVISE** |
| 6. One registration retains one binding snapshot. | The sole production `effectiveClientBindings` resolution remains at `internal/api/register.go:472`; the named timing guard starts at `internal/api/register_test.go:5063`. | Add a second production resolution or re-resolve after a deterministic mid-loop settings change; the named guard must expose split client scope. | **verified / PASS** |
| 7. Public HTTP, CLI, configuration, and persisted-state contracts are unchanged. | GUI identity remains internal at `internal/gui/projects_toggle.go:170-185`; `/api/ping` remains `internal/gui/ping.go:17-29` with byte guard `internal/gui/ping_test.go:34-42`; the protected diff is empty. | Any ping byte change, protected-file diff, CLI/schema path change, or non-internal identity field falsifies the claim. | **verified / PASS** |
| 8. Only four approved internal files changed and protected files have empty diff. | Current `git diff --name-only -- internal` lists only `internal/api/register.go`, `internal/api/register_test.go`, `internal/gui/projects_toggle.go`, and `internal/gui/projects_toggle_test.go`; the protected-file diff exits zero. | Add any fifth internal path or any hunk in `internal/cli/register.go`, `internal/gui/ping.go`, `internal/gui/ping_test.go`, `internal/api/register_supervisor.go`, or `internal/api/legacy_migrate.go`. | **verified / PASS** |

### Anti-layering verdict

- **Authorization/liveness class:** `CLEAN-SINGLE-OWNER`. Revision 1 did not
  add liveness checks to either matcher. The cleanup owns one probe/snapshot,
  `lspCleanupAliasesForClient` remains the alias authorization owner, and
  `directLanguageServerCleanupMatches` is one shared candidate/matching owner.
- **Failure idiom:** **REVISE**. The removal pass returns alias/match warnings,
  while the candidate pass silently drops the same warning types. This is not a
  `PILED` liveness fix, but it is an inconsistent same-layer failure path and is
  gate-bearing under failure transparency.
- **Resource and wire checks:** response bodies still close through
  `defer resp.Body.Close()` (`internal/api/register.go:600-605`); redirects are
  refused at `internal/api/register.go:594-599`; one 500 ms context/client
  ceiling and one `Do` call remain at `internal/api/register.go:582-603`; the
  4 KiB bound and trailing-value rejection remain at
  `internal/api/register.go:616-630`; GUI identity still comes from the owning
  `Server`; the ordinary ping wire is unchanged.

### Residual risk

- The managed identity proof remains point-in-time; the listener may stop after
  proof and before cleanup.
- The mutable `projectsToggleRegister` test seam is restored by `t.Cleanup` and
  the reviewed guard is not parallel. Future parallel mutation of that
  package-global seam would require synchronization or server-owned injection.
- Preserved QA evidence is current for the Revision 1 hashes and candidate
  guards, but this re-review deliberately did not execute tests or builds.

### Required routing

Return R1-F1 and R1-F2 to `$architect` because the corrections must preserve the
single router-entry/alias/candidate owners across a destructive cleanup path.
After implementation, re-run the same architecture angle and add high-level
guards for `port-unresolved`, router-entry read error, and candidate/survivor
scan error.

## Terms and Abbreviations

- **API** — Application Programming Interface.
- **CLI** — Command-Line Interface.
- **GUI** — Graphical User Interface.
- **LSP** — Language Server Protocol.
- **PID** — Process identifier.
- **P2** — blocking priority-two finding.
- **QA** — Quality Assurance.

## Revision 2 final re-review

### Gate

**PASS**

All eight original plan claims and all eight Revision 2 claims are verified:
**16 verified, 0 failed, 0 not-verifiable**. The current source hashes match the
fresh Revision 2 QA hashes in `verification.md:311-329`; this architecture pass
therefore used the preserved current-source execution evidence and did not
re-run tests or builds.

### Reviewed surfaces

- Revision 2 design and claims: `design.md:292-590`; original claims and named
  invariants: `design.md:229-288`.
- Backend echo and implementation evidence: `implementation.md:146-197`.
- Independent current-hash QA evidence and residual:
  `verification.md:299-404`.
- Typed structural inspection, preflight, probe, warning accumulator, cached
  execution, and alias/matcher owners:
  `internal/api/register.go:184-250`, `internal/api/register.go:559-943`, and
  `internal/api/register.go:946-1188`.
- Revision 2 guards: `internal/api/register_test.go:3958-4714`; adjacent
  stopped, foreign, managed, bound, stale-port, invalid-shape, two-kind, and
  binding-snapshot guards: `internal/api/register_test.go:4844-5830`.
- GUI identity provenance:
  `internal/gui/projects_toggle.go:160-193` and
  `internal/gui/projects_toggle_test.go:61-107`.
- Unchanged ping producer and byte guard:
  `internal/gui/ping.go:17-30` and `internal/gui/ping_test.go:13-42`.
- Current four-file internal diff, protected-file empty diff, scoped
  `git diff --check`, and the four current Secure Hash Algorithm 256-bit hashes.

### Prior-finding dispositions

#### R1-F1 — `fixed`

The cleanup first establishes a port-independent real router-origin direct
match, then resolves the router port once. Resolution failure now calls the
single warning owner with `port-unresolved` and returns before probe, backup, or
removal (`internal/api/register.go:883-906`). The high-level guard includes
both bound and unbound candidates and asserts resolver/probe counts `1/0`, zero
backups/removals, and exactly one warning in both the report contribution and
progress output (`internal/api/register_test.go:4186-4255`).

#### R1-F2 — `fixed`

Configured-entry inspection and direct matching now return explicit typed
`diagnostics` and `complete` fields
(`internal/api/register.go:192-231`, `internal/api/register.go:559-570`, and
`internal/api/register.go:769-810`). `buildDirectCleanupPreflight` collects
those diagnostics and marks the whole plan incomplete
(`internal/api/register.go:620-685`); the cleanup owner deduplicates and returns
them before resolver, proof, backup, or removal
(`internal/api/register.go:883-896`). The `GetEntry` guard observes
resolver/probe/matcher `0/0/0`, while both direct scan rows observe `0/0/1`;
all three assert one returned diagnostic and zero backup/removal
(`internal/api/register_test.go:4257-4399`).

### Original eight-claim Claim-Verify map

| Claim | Owner and current evidence | Falsifying probe | Verdict |
|---:|---|---|---|
| O1. A pre-existing router entry authorizes deletion only after one bounded cleanup-wide managed-identity proof. | `cleanupDirectLanguageServerEntriesAfterRegisterWithDeps` resolves once, selects an exact cached port group, calls the sole probe site, and gates unbound cached matches on `snapshot.liveManaged` at `internal/api/register.go:898-920`. Probe behavior is bounded at `internal/api/register.go:688-759`. | Stopped, foreign, malformed, timeout, and exact-managed cases plus request count one; the focused QA set passed those guards on the current hash (`verification.md:323-344`). | **verified** |
| O2. Same-registration bound clients remain eligible without GUI proof. | Bound aliases are composed independently at `internal/api/register.go:977-985`; cached bound matches execute without resolver/probe in the bound-only path at `internal/api/register.go:916-923`. | `TestCleanupDirectLSP_BoundOnlyPlanNeverResolvesOrProbes` observes resolver/probe/matcher `0/0/1`, one backup, and removal; the mixed identity-absent bound guard also removes the bound entry (`internal/api/register_test.go:4661-4714`, `:4844-4916`). | **verified** |
| O3. Router-origin authorization remains per client and language. | Each client receives its own language-derived alias plan and structural port groups at `internal/api/register.go:635-683` and `:965-1003`; execution consumes only that client's exact-port cached matches. | Managed Go must remove Go while sibling Python without a router replacement remains; the guard asserts that split at `internal/api/register_test.go:5164-5240`. | **verified** |
| O4. Both direct entry kinds consume the same alias authorization. | `lspCleanupAliasesForClient` remains the sole alias composer, and every group is passed through the single `directLanguageServerCleanupMatches` owner at `internal/api/register.go:640-680` and `:769-810`. | The two-kind table guard must give equivalent authorization outcomes to direct `mcp-language-server` and `gopls` candidates (`internal/api/register_test.go:5340`). | **verified** |
| O5. Proof failure and timeout are fail-closed and operator-visible once. | The probe returns typed failure classes at `internal/api/register.go:688-759`; one accumulator owns report and progress emission at `:813-840`; the worker preserves unbound matches when proof is false at `:907-920`. | The foreign multi-client/multi-language guard observes one request, zero backup/removal, and one warning (`internal/api/register_test.go:5082-5162`); the timeout guard observes one cancelled request within the bounded window (`:4064-4105`). | **verified** |
| O6. One registration retains one effective binding snapshot. | The sole production invocation remains `effectiveClientBindings(m)` at `internal/api/register.go:498`; cleanup receives the derived bound set rather than resolving again. | `TestRegister_ClientScopeResolvedOnceForTheWholeRegistration` plus a production call-count search; both were included in the current focused QA/static evidence (`verification.md:331-344`). | **verified** |
| O7. Public HTTP, command-line, configuration, and persisted-state contracts remain unchanged. | GUI identity is internal caller evidence from one `Server` at `internal/gui/projects_toggle.go:170-185`; `/api/ping` remains byte-compatible at `internal/gui/ping.go:17-30` and `internal/gui/ping_test.go:34-42`; protected-file diff is empty. | Any ping-byte change, protected-file hunk, CLI/config/schema change, or identity source outside the running `Server` falsifies the claim. | **verified** |
| O8. The probe is one 500 ms no-retry request and every obtained response body closes. | One context and client both use `managedRouterProbeTimeout`, there is one `client.Do`, redirects are refused, and `defer resp.Body.Close()` dominates every post-response return at `internal/api/register.go:702-759`. | Request-count, timeout cancellation, redirect refusal, and early-status body-close rows are `internal/api/register_test.go:3958-4126`. | **verified** |

### Revision 2 eight-claim Claim-Verify map

| Claim | Owner and current evidence | Falsifying probe | Verdict |
|---:|---|---|---|
| R2-1. No router-origin direct match means no port resolution, proof, or proof warning. | `directCleanupPreflight.hasRouterMatches` is the only resolution gate at `internal/api/register.go:594-602` and `:898-900`. | No-router and no-direct-candidate guards observe resolver/probe `0/0`, no proof warning, and no mutation (`internal/api/register_test.go:4558-4659`). | **verified** |
| R2-2. Structural route validity is port-independent; exact ownership is selected only after resolution. | `inspectClientLSPRouterReplacement` produces a structural candidate and observed port without the resolved GUI port at `internal/api/register.go:198-231`; exact selection is the pure `routerReplacementPortMatches` owner at `:234-236`, consumed by cached port selection at `:579-611`. | The stale-port guard observes resolver/probe/matcher `1/0/1`, no warning, no backup, and preserved direct entry (`internal/api/register_test.go:4401-4460`). | **verified** |
| R2-3. Relevant port-resolution failure makes no request or removal and warns exactly once in report and writer. | The single error branch is `internal/api/register.go:900-906`; `directCleanupWarningAccumulator.addProofWarning` owns dedupe and writer emission at `:834-840`. | The high-level port-resolution guard asserts one resolver, zero probe, zero bound/unbound backup/removal, and warning cardinality `1/1` (`internal/api/register_test.go:4186-4255`). | **verified** |
| R2-4. `GetEntry` and direct scan errors surface once before every external or destructive side effect. | Typed incompleteness is built at `internal/api/register.go:620-685`; diagnostics are aggregated and returned at `:891-896`, before resolver/probe at `:898-913` and backup/removal at `:916-941`. | Entry-read and candidate/survivor scan guards assert their exact resolver/probe/matcher counters, one diagnostic, and zero backup/removal (`internal/api/register_test.go:4257-4399`). | **verified** |
| R2-5. One cleanup invocation performs at most one managed-listener proof. | There is one non-loop `deps.probeRouter` call site at `internal/api/register.go:907-913`. | The cached-proof guard uses two eligible clients and observes resolver/probe/request `1/1/1`, one backup/removal per client (`internal/api/register_test.go:4462-4556`). | **verified** |
| R2-6. Candidate, route-shape, alias, matcher, and liveness logic is not copied into removal. | Structural inspection is owned by `inspectClientLSPRouterReplacement`; exact port equality by `routerReplacementPortMatches`; aliases by `lspCleanupAliasesForClient`; direct matching by `directLanguageServerCleanupMatches`; liveness by `probeManagedRouter`. The removal loop at `internal/api/register.go:916-941` consumes cached entries only. | A source-owner/call-site audit or a post-proof matcher sentinel must expose any copied predicate or rescan; current call sites and `TestCleanupDirectLSP_MatchingOwnedCandidateUsesOneCachedProof` show neither. | **verified** |
| R2-7. Bound clients remain independent of GUI proof. | `boundAliases` is a separate cached lane at `internal/api/register.go:559-576`, `:657-664`, and `:916-920`. | Bound-only guard must remove with zero resolver/probe calls; mixed identity-failure guard must still remove the bound entry. Both current guards pass in the preserved QA evidence. | **verified** |
| R2-8. Public HTTP, command-line, configuration, and persisted-state contracts remain unchanged. | The current internal diff contains exactly the approved four files; protected-file diff is empty; GUI identity comes from `s.Port()`, `s.cfg.PID`, and `s.cfg.Version`; ordinary ping producer/bytes are unchanged. | Any fifth internal path, protected hunk, ping-byte drift, or public/config/persisted schema edit falsifies the claim. | **verified** |

### Anti-layering verdict

- **Structural route and exact-port class:** `CLEAN-SINGLE-OWNER`.
  `lspRouterURLLanguagePort` remains the route parser;
  `inspectClientLSPRouterReplacement` owns candidate inspection; and
  `routerReplacementPortMatches` owns the only exact-port comparison.
- **Alias and direct-match class:** `CLEAN-SINGLE-OWNER`.
  Preflight groups and caches the existing owners' results; it does not
  reimplement either predicate.
- **Liveness and authorization class:** `CLEAN-SINGLE-OWNER`.
  One cleanup worker owns one immutable authorization decision, and
  `probeManagedRouter` remains the sole liveness engine.
- **Diagnostic/failure class:** `CLEAN-SINGLE-OWNER`.
  Typed `complete` results replace the two prior silent Boolean/error-discard
  paths; one accumulator owns dedupe and proof progress emission. The removal
  phase has no rescan or alternate failure idiom.
- **Resource ownership:** `PASS`. The preflight owns no external resource; the
  probe owns its context, client, and obtained response body across every
  return path.

### QA residual disposition

The lack of a dedicated repeated-diagnostic-across-multiple-structural-ports
test is **not material to this gate**. Multiple port groups may append the same
diagnostic while building the preflight (`internal/api/register.go:667-680`),
but all of them pass through one exact-string `seen` map before return
(`internal/api/register.go:813-840`, `:891-895`). Existing entry/direct-scan
guards already assert exact count one and side-effect counters
(`internal/api/register_test.go:4257-4399`), while the cached multi-client guard
asserts the preflight/proof call counts (`:4462-4556`). A dedicated multi-port
row would strengthen mutation-resistant coverage, but the missing row does not
create an unverified owner or an uncovered execution branch; retain it as
non-blocking test debt as QA recorded at `verification.md:395-401`.

### Residual risk

- Managed identity remains a point-in-time proof; the listener can stop after
  proof and before cached removal. This is the accepted original residual, not
  Revision 2 drift.
- The GUI test still swaps the package-level `projectsToggleRegister` seam and
  restores it with `t.Cleanup`; it is non-parallel today. Future parallel tests
  should move this seam to synchronized or server-owned injection.
- This final architecture pass did not execute tests or builds. It verified
  that the four current hashes equal the fresh QA hashes and inspected the
  preserved guard results.

## Terms and Abbreviations

- **API** — Application Programming Interface.
- **CLI** — Command-Line Interface.
- **GUI** — Graphical User Interface.
- **LSP** — Language Server Protocol.
- **PID** — Process identifier.
- **QA** — Quality Assurance.
