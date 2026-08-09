# PR #583 live Codex-bot finding classification

## Finding classification

| # | Finding | Classification | Evidence |
|---:|---|---|---|
| 1 | `internal/api/default_client_scope_test.go:119` — isolate the canonical binary in the bulk-install test | **ALREADY FIXED** by local commit `c826a48d06cf13a9a727acaaf776ee8f358b64b2` | The test now installs a temporary canonical-path stub at `internal/api/default_client_scope_test.go:128-134`; the preflight seam consumes that path at `internal/api/install.go:55-74`. The named test passed in the tagged, isolated run captured below. |
| 2 | `internal/api/register.go:405` — snapshot client bindings once per registration | **ALREADY FIXED** by local commit `c826a48d06cf13a9a727acaaf776ee8f358b64b2` | `registerWithManifest` resolves once at `internal/api/register.go:486-495`, passes the same snapshot to every language at `internal/api/register.go:496-502`, and derives cleanup names from that snapshot at `internal/api/register.go:504-512`. |
| 3 | `internal/api/register.go:77` — preserve selected relay clients during registration | **ALREADY FIXED** by local commit `c826a48d06cf13a9a727acaaf776ee8f358b64b2` | The accepted fail-visible path returns filtered relay names at `internal/api/register.go:71-85`, constructs mixed and zero-binding warnings at `internal/api/register.go:116-127`, and emits the warning to both the writer and report at `internal/api/register.go:491-495`. |
| 4 | `internal/api/register.go:181` — verify the router is live before deleting direct entries | **REAL, open** | `clientHasActiveLSPRouterReplacement` checks entry presence, enabled state, route grammar, port equality, and ownership at `internal/api/register.go:241-258`, but performs no listener or managed-identity probe. Cleanup then backs up and calls `RemoveEntry` at `internal/api/register.go:648-660`. |
| 5 | `internal/api/register.go:182` — prove router liveness before removing direct entries | **REAL, open** — duplicate report of finding 4 | The same authorization boundary at `internal/api/register.go:241-258` treats configured port equality as sufficient replacement proof. The tests at `internal/api/register_test.go:4113-4161` and `internal/api/register_test.go:4405-4465` start no router listener yet require direct-entry deletion. |
| 6 | `internal/api/default_client_scope_test.go:119` — stub the canonical binary in the new dry-run test | **ALREADY FIXED** — duplicate report of finding 1 | The temporary canonical binary is created at `internal/api/default_client_scope_test.go:128-134`, before the preflight reached by `installUsingEmbedFirst` at `internal/api/install.go:393-421`. |
| 7 | `internal/api/register.go:405` — snapshot effective bindings for the whole registration | **ALREADY FIXED** — duplicate report of finding 2 | There is one production resolution at `internal/api/register.go:491`; its result is threaded through the language loop at `internal/api/register.go:496-502` and cleanup at `internal/api/register.go:504-512`. |
| 8 | `internal/cli/register.go:40` — exclude relay-only clients from the register promise | **ALREADY FIXED** by local commit `c826a48d06cf13a9a727acaaf776ee8f358b64b2` | Help text derives the relay client names from the registry at `internal/cli/register.go:26-42` and explicitly states the exclusion, warning behavior, and zero-bind case at `internal/cli/register.go:65-72`. |

No supplied finding is **WRONG**. Six are **ALREADY FIXED** by the local commit
ahead of the remote head; findings 4 and 5 are duplicate reports of one
**REAL, open** defect class.

## Files & symbols

| File or symbol | Role in the findings | Evidence |
|---|---|---|
| `internal/api/default_client_scope_test.go` | Hermetic persisted-default-client install regression | Temporary canonical binary at `internal/api/default_client_scope_test.go:128-134`; target test at `internal/api/default_client_scope_test.go:66-145`. |
| `internal/api/install.go` | Canonical binary preflight owner and install entry path | Preflight path seam at `internal/api/install.go:55-74`; `installUsingEmbedFirst` reaches preflight at `internal/api/install.go:393-421`. |
| `internal/api/register.go` | Binding resolution, relay filtering/warnings, language writes, and destructive cleanup | Relay filter at `internal/api/register.go:71-85`; warning owner at `internal/api/register.go:116-127`; snapshot at `internal/api/register.go:486-512`; cleanup authorization and deletion at `internal/api/register.go:576-660`. |
| `clientHasActiveLSPRouterReplacement` | Defective replacement-proof boundary | It performs configuration/URL/port/ownership checks only at `internal/api/register.go:241-258`. |
| `lspCleanupAliasesForClient` | Per-client/per-language cleanup alias decision | `internal/api/register.go:698-727`. |
| `matchingDirectLanguageServerEntries` | Direct Language Server Protocol entry candidate path | `internal/api/register.go:778-814`. |
| `matchingDirectGoplsMCPEntries` | Direct `gopls` Model Context Protocol entry candidate path | `internal/api/register.go:817-864`. |
| `internal/api/register_supervisor.go` | Supervised registration consumer of the same binding snapshot | Receives the snapshot at `internal/api/register_supervisor.go:188-199`; uses it in registry/write loops at `internal/api/register_supervisor.go:269-283` and `internal/api/register_supervisor.go:344-355`. |
| `internal/api/lsp_client_router.go` | Router entry construction and configured-entry ownership parsing | Relay-shaped router entry support at `internal/api/lsp_client_router.go:675-698`; ownership parsing at `internal/api/lsp_client_router.go:757-781`; route grammar at `internal/api/lsp_client_router.go:845-869`. |
| `internal/clients/clients.go` | Canonical supported-client and relay classification registry | Six relay rows at `internal/clients/clients.go:865-937`; registry-derived `IsRelayStdio` and `RelayStdioClientNames` at `internal/clients/clients.go:1046-1074`. |
| `internal/cli/register.go` | Register user-facing promise and relay exclusion disclosure | Registry-derived names at `internal/cli/register.go:26-42`; help contract at `internal/cli/register.go:65-72`. |

## Flows

### Persisted default-client install test

1. `TestInstallUsingEmbedFirst_HonorsPersistedDefaultClientsOverride` prepares
   isolated state and a temporary canonical binary at
   `internal/api/default_client_scope_test.go:66-145`.
2. `installUsingEmbedFirst` enters preflight at
   `internal/api/install.go:393-421`.
3. Preflight resolves the injected canonical path through the seam at
   `internal/api/install.go:55-74`, so the test no longer depends on a
   machine-global `mcphub setup`.

### Registration binding snapshot

1. `registerWithManifest` resolves effective bindings once at
   `internal/api/register.go:486-495`.
2. The same `bindings` value is passed to each normal-language registration at
   `internal/api/register.go:496-502`, and to the supervised lane at
   `internal/api/register_supervisor.go:188-199`.
3. Cleanup derives bound client names from that same snapshot at
   `internal/api/register.go:504-512`; there is no second settings resolution.

### Relay-only selection

1. Effective client selection is classified through the canonical registry at
   `internal/clients/clients.go:1046-1074`.
2. Register filters relay-stdio bindings while returning the dropped names at
   `internal/api/register.go:71-85`.
3. The owner converts that result into mixed-set or zero-bind warnings at
   `internal/api/register.go:116-127`, then emits them at
   `internal/api/register.go:491-495`.
4. Command-Line Interface help names the same registry-derived exclusion at
   `internal/cli/register.go:26-42` and `internal/cli/register.go:65-72`.

### Direct-entry cleanup

1. Cleanup intentionally rejects a general liveness-probe requirement at
   `internal/api/register.go:576-596`.
2. Direct Language Server Protocol and direct `gopls` Model Context Protocol
   candidates converge on the same alias gate at
   `internal/api/register.go:621-644`.
3. `lspCleanupAliasesForClient` makes the decision per client and language at
   `internal/api/register.go:698-727`.
4. `clientHasActiveLSPRouterReplacement` authorizes replacement from configured
   entry shape and caller port at `internal/api/register.go:241-258`.
5. Once authorized, cleanup backs up and removes the direct entry at
   `internal/api/register.go:648-660`.
6. Because step 4 performs no live-listener or managed-identity proof, a stopped
   router or foreign listener can authorize the destructive step.

## Contracts

| Contract or invariant | Status | Evidence |
|---|---|---|
| One effective client-binding snapshot governs one registration, including every language, supervised writes, and cleanup. | **VERIFIED** | One production resolution at `internal/api/register.go:491`; propagation at `internal/api/register.go:496-512` and `internal/api/register_supervisor.go:188-199`. |
| Every selected supported client is either registered or receives an explicit register warning explaining why it is not. | **VERIFIED for the relay-stdio class** | Filter/warning path at `internal/api/register.go:71-127`; emitted at `internal/api/register.go:491-495`; all relay clients are registry-derived at `internal/clients/clients.go:1046-1074`. |
| The persisted-default-client install regression is hermetic with respect to the canonical binary. | **VERIFIED** | Test-local stub at `internal/api/default_client_scope_test.go:128-134`; tagged named test passed. |
| No direct entry is deleted unless the same client and language have a live managed router replacement. | **CONTRADICTED / open** | Authorization checks no listener identity at `internal/api/register.go:241-258`, then deletion occurs at `internal/api/register.go:648-660`. |
| Configured port equality proves router liveness or ownership of the listening process. | **FALSE** | The parser only extracts loopback route/name/port at `internal/api/lsp_client_router.go:757-781` and `internal/api/lsp_client_router.go:845-869`; current cleanup tests require deletion without starting a listener at `internal/api/register_test.go:4113-4161` and `internal/api/register_test.go:4405-4465`. |

## Defect-class sweep

| Defect class | Sites swept | Result |
|---|---|---|
| Tests reaching install preflight must isolate the canonical binary. | Shared helper owner at `internal/api/install_test.go:264-279`. | Protected by the existing helper. |
|  | Helper call sites in `admission_check_test.go`, `availability_admission_paths_test.go`, `install_intent_test.go`, `install_own_port_test.go`, `install_parsed_manifest_lane_b_test.go`, `install_parsed_manifest_test.go`, `install_test.go`, `lane_a_internal_review_test.go`, `phase_f_lifecycle_test.go`, and `required_secret_gate_test.go`. | Existing preflight-reaching tests use the shared preparation path. |
|  | Direct `installUsingEmbedFirst` tests in `internal/api/default_client_scope_test.go` and `internal/api/embed_manifest_precedence_test.go`. | The finding target now stubs the binary. The precedence tests intentionally assert a warning before preflight and tolerate admission errors at `internal/api/embed_manifest_precedence_test.go:187-265`; they do not rely on preflight success. |
| One registration must resolve client bindings exactly once. | Production binding-resolution references. | Sole production call is at `internal/api/register.go:491`; test references were retained as the search control. |
|  | Normal registration lane at `internal/api/register.go:941-960`, `internal/api/register.go:1130-1137`, and `internal/api/register.go:1261-1293`. | Receives and uses the caller snapshot. |
|  | Supervised registration lane at `internal/api/register_supervisor.go:188-199`, `internal/api/register_supervisor.go:269-283`, and `internal/api/register_supervisor.go:344-355`. | Receives and uses the same caller snapshot. |
|  | Cleanup at `internal/api/register.go:504-512`. | Derives client names from the same snapshot; no fresh resolution. |
| Relay-stdio selections must not disappear silently. | Canonical registry at `internal/clients/clients.go:865-937`. | Exactly six supported relay clients are declared: Antigravity, Zed, Aider, Pi, Pochi, and Zencoder. |
|  | Concrete relay classifiers at `internal/clients/aider.go:86`, `internal/clients/antigravity.go:62`, `internal/clients/pochi.go:79`, `internal/clients/pi.go:78`, `internal/clients/zencoder.go:80`, and `internal/clients/zed.go:100`. | Every declared relay client reports relay-stdio capability. |
|  | Register filter/warning at `internal/api/register.go:71-127` and writer/report emission at `internal/api/register.go:491-495`. | Mixed and relay-only selections are explicit, including the zero-write outcome. |
|  | Command-Line Interface help at `internal/cli/register.go:26-42` and `internal/cli/register.go:65-72`. | Promise now describes the same exclusion and warning behavior. |
| Destructive cleanup requires a live, managed, same-client/same-language replacement. | Replacement authorization at `internal/api/register.go:241-258`. | **Open:** configuration shape, port, and entry ownership are checked; listener state and listener identity are not. |
|  | Direct Language Server Protocol candidates at `internal/api/register.go:778-814`. | **Open:** uses the deficient alias authorization. |
|  | Direct `gopls` Model Context Protocol candidates at `internal/api/register.go:817-864`. | **Open:** uses the same deficient alias authorization. |
|  | Register-client interface boundary at `internal/api/register.go:1792-1802` and real adapter at `internal/api/register.go:1930-1939`. | Both cleanup candidate kinds reach the same removal surface. |
|  | Graphical User Interface caller at `internal/gui/projects_toggle.go:169-177`. | Supplies `s.Port()`, which identifies the intended port but does not prove a live managed listener at cleanup time. |
|  | Command-Line Interface caller at `internal/cli/register.go:115-120`. | Leaves `GUIPort` zero; no independent liveness/identity evidence is supplied. |
|  | Legacy migration caller at `internal/api/legacy_migrate.go:168-172`. | Leaves `GUIPort` zero; no independent liveness/identity evidence is supplied. |

### Search controls

| Negative claim | Control showing the search could find the mechanism |
|---|---|
| Cleanup contains no router listener/identity request. | The same network-call search found the separate workspace readiness request at `internal/api/register.go:1627-1633`, while no network call occurs in the cleanup authorization at `internal/api/register.go:241-258` or `internal/api/register.go:576-660`. |
| Production registration contains no second binding resolution. | The resolution search found the sole production call at `internal/api/register.go:491` and also found test references, demonstrating that the symbol pattern was active. |
| The relay class contains six supported clients, not only those named by one finding. | The registry sweep found six relay declarations at `internal/clients/clients.go:865-937`, and the all-supported-client test at `internal/clients/relay_stdio_test.go:14-56` pins the classifier result. |

## Object-axis records

| Object | Axes swept | Result |
|---|---|---|
| Canonical-binary preflight isolation | Test entry path × preflight reached/not reached × helper/stub owner | Finding target fixed; shared helper sites protected; precedence-only tests intentionally do not require preflight success. |
| Registration binding snapshot | Registration mode × language count × settings mutation timing × write/cleanup consumer | Normal, supervised, and cleanup consumers share one snapshot. The deterministic multi-language mutation guard covers the reported race. |
| Relay client handling | Every `SupportedClientNames` row × `IsRelayStdio` × Application Programming Interface warning × Command-Line Interface promise × mixed/relay-only set | All six relay clients are registry-derived and explicitly excluded with a warning; representative runtime coverage plus an all-client classifier guard exists. |
| Router replacement cleanup | Client × language × replacement-entry origin × listener state × listener identity × direct-entry kind | Port-mismatch and configured-entry axes are covered. Stopped-listener and foreign-listener axes remain open. Both direct Language Server Protocol and direct `gopls` Model Context Protocol entry kinds share the deficient gate. |

## Tests & coverage

### Safe Application Programming Interface run

The following command used a fresh state directory and the mandatory test build
tag:

```text
MCPHUB_STATE_DIR_OVERRIDE=.scratch/pr583-classification-v-9b796610de6044ffb71f5cf29b0fb91e/state
go test -v -tags=test_state_path_env -count=1 -timeout 10m -run '^(TestInstallUsingEmbedFirst_HonorsPersistedDefaultClientsOverride|TestRegister_ClientScopeResolvedOnceForTheWholeRegistration|TestRegister_RelayStdioOnlyOverrideWarnsInsteadOfSilentZeroWrite|TestRegister_CleanupRejectsRouterEntryOnStalePort|TestRegister_CleanupCoversOptInClientRoutedThroughSharedLSPRouter|TestRegister_CleanupJudgesRouterEntriesAgainstCallerLiveGUIPort)$' ./internal/api/
```

Captured output:

```text
PASS
ok  	github.com/mcp-local-hub/mcp-local-hub/internal/api	1.803s
```

Evidence file:
`.scratch/pr583-classification-v-9b796610de6044ffb71f5cf29b0fb91e/api-classification-verbose.log`.

### Relay classification run

```text
go test -v -count=1 -timeout 10m -run '^TestIsRelayStdioClassifiesEverySupportedClient$' ./internal/clients/
```

Captured output:

```text
PASS
ok  	github.com/mcp-local-hub/mcp-local-hub/internal/clients	0.023s
```

Evidence file:
`.scratch/pr583-relay-class-d8b192ede56e420db4f4126b8a772ae9/relay-classification.log`.

### Coverage interpretation

| Guard | What it proves | Gap |
|---|---|---|
| `TestInstallUsingEmbedFirst_HonorsPersistedDefaultClientsOverride` at `internal/api/default_client_scope_test.go:66-145` | The target test reaches its client-scope assertion without a machine-global canonical binary. | None for the reported hermeticity defect. |
| `TestRegister_ClientScopeResolvedOnceForTheWholeRegistration` at `internal/api/register_test.go:4287-4378` | A deterministic settings change during a multi-language registration does not change the snapshot used by later writes or cleanup. | No separate supervised-lane runtime guard; static propagation is present. |
| `TestRegister_RelayStdioOnlyOverrideWarnsInsteadOfSilentZeroWrite` at `internal/api/register_test.go:4188-4263` | Relay-only selection is fail-visible rather than silent. | The Application Programming Interface test is representative; all names are pinned separately by the classifier test. |
| `TestIsRelayStdioClassifiesEverySupportedClient` at `internal/clients/relay_stdio_test.go:14-56` | Every supported client stays synchronized with its relay classification. | Does not assert Command-Line Interface help text. |
| `TestRegister_CleanupRejectsRouterEntryOnStalePort` at `internal/api/register_test.go:4030-4087` | A configured entry on a different port cannot authorize cleanup. | Port equality does not prove listener state or identity. |
| `TestRegister_CleanupCoversOptInClientRoutedThroughSharedLSPRouter` at `internal/api/register_test.go:4113-4161` | Current behavior deletes the direct Go entry from configured router metadata. | The test starts no router listener, so its pass is evidence of the open defect rather than liveness proof. |
| `TestRegister_CleanupJudgesRouterEntriesAgainstCallerLiveGUIPort` at `internal/api/register_test.go:4405-4465` | Current behavior uses caller port 9200 for the cleanup decision. | It labels a numeric port “live” without starting or identifying a listener and requires Python direct-entry deletion. |

Mutation proof, implementation-scoped tests, `go build ./...`, and
`go vet ./...` belong to the downstream fix and verification stages; they were
not performed by this read-only research stage.

## Similar implementations

| Implementation | Evidence | Relevance |
|---|---|---|
| Workspace proxy readiness probe | `internal/api/register.go:1603-1652` sends Model Context Protocol `initialize`, waits for HTTP 200, and can inspect `serverInfo`. | Demonstrates a protocol-level readiness pattern already exists in the owning package. It is not currently connected to router cleanup. |
| Graphical User Interface ping endpoint | `internal/gui/ping.go:17-29` returns `ok`, process identifier, and version. | Provides a managed-identity-bearing response shape; availability of the route alone is not identity proof. |
| Incumbent process handshake | `internal/gui/handshake.go:51-125` retries malformed/foreign listeners and cross-checks ping process identifier against pid-port state. | Demonstrates existing fail-closed handling for a foreign process on an expected port. |
| Graphical User Interface listener health watcher | `internal/api/hub_listener_health_watcher.go:151-168`, launched at `internal/gui/hub_listener.go:951-967`. | A Transmission Control Protocol dial is used while the Graphical User Interface owns the listener lifecycle. It cannot prove liveness later for independent Command-Line Interface or migration cleanup. |
| Relay router entry writer | `lspRouterMCPEntryForClient` at `internal/api/lsp_client_router.go:675-698`. | Confirms relay-shaped router entries are representable. The current branch deliberately chose the bot-accepted explicit-warning path for register rather than silently discarding selections. |

## Constraints

- Any `go test` covering `./internal/api` or `./internal/cli` must use
  `-tags=test_state_path_env` and a fresh `MCPHUB_STATE_DIR_OVERRIDE`.
- An unscoped `go test ./...` is prohibited.
- The Graphical User Interface, tray, and supervisor must not be launched.
- Processes must not be killed by image name.
- Production scheduler/state paths and all other worktrees are protected.
- No checkout, hard reset, stash, push, or publication is authorized.
- This analyst stage is evidence-only; production and test edits are owned by
  the downstream implementation stage.

## Change risks

| Risk | Why it matters | Required falsifier |
|---|---|---|
| Replacing a port check with a bare Transmission Control Protocol connect still accepts a foreign listener. | The defect is managed replacement identity, not only socket availability. | Occupy the configured port with a controlled foreign listener and prove the direct entry survives. |
| A positive liveness probe can become a second source of route or ownership policy. | Duplicated decision logic would drift from the router entry owner. | Keep route/client/language ownership in the existing parser/authorization boundary and test one canonical proof path. |
| A global router check can authorize cleanup for the wrong client or language. | Cleanup is explicitly per client and language at `internal/api/register.go:698-727`. | Prove one client/language replacement cannot delete a sibling language’s direct entry. |
| Probe failure handling can turn registration into an unrelated hard failure. | The required invariant is fail-closed destructive cleanup; registration write success is a separate result. | Simulate stopped and foreign listeners and require “keep direct entry” with visible diagnostics, not catch-and-swallow behavior. |
| Existing tests encode the defective rule. | Two green tests currently require deletion without a live listener. | Revise them so stopped/foreign cases preserve entries and a controlled managed listener is the sole positive deletion case. |
| The register surface has accumulated successive local fixes. | Blame/history shows the same boundary changed in `bff6b6bf`, `fbcbd661`, `2e74a0b4`, `04f8ee8f`, and `c826a48d`. | Implement the replacement-proof invariant once at its authorization owner and re-run the full class sweep. |

## Unresolved questions

1. Which existing managed identity contract should router cleanup consume:
   the Graphical User Interface `/api/ping` process-identifier/version response,
   a language-router endpoint, or a new narrow owner-level probe?
2. How should Command-Line Interface and legacy-migration callers obtain the
   expected managed identity when they currently pass `GUIPort == 0` at
   `internal/cli/register.go:115-120` and
   `internal/api/legacy_migrate.go:168-172`?
3. Should failed replacement proof be reported only as a cleanup warning, or
   also represented in the registration report contract? This needs inspection
   during design; research does not choose a new external contract.

Required implementation falsifiers:

- `TestRegister_CleanupKeepsDirectEntryWhenConfiguredRouterIsStopped`: matching
  configured entry and port, no listener, no backup/remove.
- `TestRegister_CleanupKeepsDirectEntryWhenRouterPortHasForeignListener`:
  foreign listener on the expected port, no backup/remove.
- `TestRegister_CleanupRemovesDirectEntryWithProvenManagedRouter`: controlled
  managed identity proven, matching direct entry removed, sibling language
  without proof preserved.

## Research admission gates

| Gate | Verdict | Evidence |
|---|---|---|
| Every supplied finding classified with a current-code basis | **PASS** | Eight-row classification table above; six already fixed, two duplicate real/open, zero wrong. |
| Local-versus-remote state distinguished | **PASS** | Branch `fix/cursor-not-default-install` was observed at local `c826a48d06cf13a9a727acaaf776ee8f358b64b2`, ahead of remote `04f8ee8f37de0056572cd524dbfa3b0034443313`. |
| Defect class swept beyond named instances | **PASS** | Preflight tests, normal/supervised snapshot consumers, all six relay clients, both direct-entry candidate kinds, and all register callers are enumerated above. |
| Negative claims carry controls | **PASS** | Network-call, resolution-call, and relay-inventory controls are recorded under Search controls. |
| Safe runtime evidence captured | **PASS** | Tagged isolated Application Programming Interface run and all-client relay classifier run both passed; logs are preserved under `.scratch/`. |
| Open defect has falsifying tests, not an inferred fix | **PASS** | Stopped, foreign-listener, positive-managed-listener, and sibling-language cases are specified above. |
| Research is sufficient for downstream design/implementation | **PASS** | Root owner, affected flows, deficient contract, adjacent mechanisms, risks, and verification obligations are identified. |

## Adjacent findings

| Adjacent item | Status | Disposition |
|---|---|---|
| Command-Line Interface register help has no direct regression test for the relay carve-out. | The help is correct and registry-derived at `internal/cli/register.go:26-72`; no direct help assertion was found. | Add a narrow help guard only if the downstream implementation/review scope admits it; it does not reopen finding 8. |
| Supervised registration has no separate runtime snapshot-race test. | Static propagation uses the same `bindings` parameter at `internal/api/register_supervisor.go:188-199`. | Residual coverage gap, not evidence that finding 2 remains open. |
| Two cleanup tests call an unproven numeric port “live.” | `internal/api/register_test.go:4113-4161` and `internal/api/register_test.go:4405-4465` start no listener but require deletion. | Must be revised as part of the real liveness class fix because they currently preserve the wrong invariant. |
| Graphical User Interface health watching is lifecycle-local. | The watcher at `internal/api/hub_listener_health_watcher.go:151-168` stops with the Graphical User Interface lifecycle launched at `internal/gui/hub_listener.go:951-967`. | Do not treat prior watcher success as proof for a later Command-Line Interface or migration cleanup. |
| Workspace-free language-server cleanup uses separate conservative readers. | `internal/api/register.go:1804-1813`. | Excluded from this finding because it does not pass through the reported registration cleanup authorization. |
| Unregister behavior is a separate destructive path. | It is not invoked by the reported registration flow. | Excluded from this research scope; no classification claim is made about it. |

## Terms and Abbreviations

- **API**: Application Programming Interface.
- **CLI**: Command-Line Interface.
- **GUI**: Graphical User Interface.
- **LSP**: Language Server Protocol.
- **MCP**: Model Context Protocol.
- **PASS**: Evidence is sufficient for the next role.
- **REAL, open**: The reported defect is present in current local code and is not
  closed by the local commit.
- **ALREADY FIXED**: The remote-head finding is closed by a current local commit;
  no further fix is justified for that finding.

**Research verdict: PASS.**
